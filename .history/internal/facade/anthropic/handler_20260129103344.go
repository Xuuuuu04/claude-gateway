package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"claude-gateway/internal/canonical"
	"claude-gateway/internal/convert"
	"claude-gateway/internal/logbus"
	"claude-gateway/internal/metrics"
	anthropicproto "claude-gateway/internal/proto/anthropic"
	"claude-gateway/internal/providers/anthropic"
	geminiProvider "claude-gateway/internal/providers/gemini"
	openaiProvider "claude-gateway/internal/providers/openai"
	"claude-gateway/internal/router"
	"claude-gateway/internal/streamconv"
)

type Handler struct {
	rtr *router.Router
	m   *metrics.Metrics
	bus *logbus.Bus
}

func NewHandler(rtr *router.Router, m *metrics.Metrics, bus *logbus.Bus) *Handler {
	return &Handler{rtr: rtr, m: m, bus: bus}
}

func (h *Handler) Register(r chi.Router) {
	r.Post("/messages", h.createMessage)
	r.Get("/models", h.listModels)
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

func (h *Handler) createMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := strings.TrimSpace(r.Header.Get("x-request-id"))
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-Id", requestID)

	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var req anthropicproto.MessageCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid json")
		return
	}
	if strings.TrimSpace(req.Model) == "" || req.MaxTokens <= 0 {
		w.Header().Set("X-Request-Id", requestID)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model and max_tokens are required")
		return
	}

	clientKey, _ := ctx.Value(canonical.ContextKeyClientKey).(string)
	srcIP := clientIP(r)
	userAgent := strings.TrimSpace(r.UserAgent())
	isTest := isTestRequest(r)
	requestBytes := len(body)

	publish := func(up router.RoutedUpstream, status int, latency time.Duration, errMsg string, inputTokens, outputTokens int64, responseBytes int) {
		if h.bus == nil {
			return
		}
		h.bus.Publish(logbus.Event{
			TS:            time.Now(),
			RequestID:     requestID,
			Facade:        string(canonical.FacadeAnthropic),
			RequestModel:  req.Model,
			UpstreamModel: up.Model,
			ProviderType:  up.ProviderType,
			PoolID:        up.PoolID,
			ProviderID:    up.ProviderID,
			CredentialID:  up.CredentialID,
			ClientKey:     clientKey,
			SrcIP:         srcIP,
			UserAgent:     userAgent,
			IsTest:        isTest,
			Stream:        req.Stream,
			RequestBytes:  requestBytes,
			ResponseBytes: responseBytes,
			InputTokens:   inputTokens,
			OutputTokens:  outputTokens,
			Status:        status,
			LatencyMs:     latency.Milliseconds(),
			Error:         errMsg,
		})
	}

	maxAttempts := 1
	if !req.Stream {
		maxAttempts = 2
	}
	exclude := map[uint64]bool{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var up router.RoutedUpstream
		var err error
		if attempt == 0 {
			up, err = h.rtr.PickUpstream(ctx, clientKey, string(canonical.FacadeAnthropic), req.Model)
		} else {
			up, err = h.rtr.PickUpstreamExclude(ctx, clientKey, string(canonical.FacadeAnthropic), req.Model, exclude)
		}
		if err != nil {
			if errors.Is(err, router.ErrNotConfigured) {
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusServiceUnavailable, "overloaded_error", "gateway not configured")
				return
			}
			w.Header().Set("X-Request-Id", requestID)
			writeError(w, http.StatusBadGateway, "api_error", "routing failed")
			return
		}

		start := time.Now()
		status := 0
		ok := false

		switch up.ProviderType {
		case "anthropic":
			targetBody := body
			if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
				req.Model = up.Model
				b, err := json.Marshal(req)
				if err != nil {
					h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
					writeError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
					return
				}
				targetBody = b
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)

			resp, err := anthropic.DoMessages(uctx, anthropic.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
				APIVer:  firstNonEmpty(r.Header.Get("anthropic-version"), "2023-06-01"),
				Timeout: timeout,
			}, targetBody)
			cancel()
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
				return
			}
			defer resp.Body.Close()

			status = resp.StatusCode
			ok = status < 500 && status != http.StatusTooManyRequests
			if !ok {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
			}

			copyHeader(w.Header(), resp.Header)
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(resp.StatusCode)

			if req.Stream {
				var inTok, outTok int64
				var respBytes int
				respBytes, inTok, outTok, err = copyAnthropicSSEWithUsage(w, resp.Body)
				h.rtr.EndRequest(up.CredentialID, err == nil && ok, status, time.Since(start))
				publish(up, status, time.Since(start), errString(err), inTok, outTok, respBytes)
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			_, _ = w.Write(raw)
			h.rtr.EndRequest(up.CredentialID, ok, status, time.Since(start))
			inTok, outTok := extractAnthropicUsage(raw)
			publish(up, status, time.Since(start), "", inTok, outTok, len(raw))
			h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
			return

		case "openai":
			oreq, err := convert.AnthropicToOpenAIChatRequest(req)
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			oreq.Model = up.Model
			oreq.Stream = req.Stream
			b, err := json.Marshal(oreq)
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				writeError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
				return
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)
			resp, err := openaiProvider.DoChatCompletions(uctx, openaiProvider.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
			}, b)
			cancel()
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error", 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
				exclude[up.CredentialID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, mapStatusToAnthropic(status), mapTypeToAnthropic(status), "upstream error")
				return
			}

			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Request-Id", requestID)
				w.WriteHeader(http.StatusOK)
				err := streamconv.OpenAIToAnthropic(w, resp.Body, up.Model)
				h.rtr.EndRequest(up.CredentialID, err == nil && ok, status, time.Since(start))
				publish(up, status, time.Since(start), errString(err), 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
				return
			}

			raw, _ := io.ReadAll(resp.Body)
			var oresp convert.OpenAIChatCompletionResponse
			if err := json.Unmarshal(raw, &oresp); err != nil {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
				return
			}
			aresp := convert.OpenAIResponseToAnthropic(oresp, up.Model)
			outRaw, _ := json.Marshal(aresp)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(outRaw)
			h.rtr.EndRequest(up.CredentialID, true, status, time.Since(start))
			inTok, outTok := extractOpenAIUsage(raw)
			publish(up, status, time.Since(start), "", inTok, outTok, len(outRaw))
			h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
			return

		case "gemini":
			if req.Stream {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				writeError(w, http.StatusNotImplemented, "api_error", "provider conversion not implemented yet (streaming requires conversion)")
				return
			}
			greq, model := convert.AnthropicToGeminiRequest(req)
			model = up.Model
			b, err := json.Marshal(greq)
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				writeError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
				return
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)
			resp, err := geminiProvider.DoGenerateContent(uctx, geminiProvider.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
			}, model, b)
			cancel()
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error", 0, 0, len(raw))
				h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
				exclude[up.CredentialID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, mapStatusToAnthropic(status), mapTypeToAnthropic(status), "upstream error")
				return
			}
			var gres convert.GeminiGenerateContentResponse
			if err := json.Unmarshal(raw, &gres); err != nil {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
				return
			}
			text, usage := convert.GeminiResponseText(gres)
			aresp := convert.GeminiTextToAnthropic(text, model, usage)
			outRaw, _ := json.Marshal(aresp)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(outRaw)
			h.rtr.EndRequest(up.CredentialID, true, status, time.Since(start))
			var inTok, outTok int64
			if usage != nil {
				inTok = int64(usage.PromptTokenCount)
				outTok = int64(usage.CandidatesTokenCount)
			}
			publish(up, status, time.Since(start), "", inTok, outTok, len(outRaw))
			h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, status, time.Since(start))
			return

		default:
			h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
			w.Header().Set("X-Request-Id", requestID)
			writeError(w, http.StatusNotImplemented, "api_error", "unknown provider")
			return
		}
	}
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clientKey, _ := ctx.Value(canonical.ContextKeyClientKey).(string)
	up, err := h.rtr.PickUpstream(ctx, clientKey, string(canonical.FacadeAnthropic), "")
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "routing failed")
		return
	}
	if up.ProviderType != "openai" {
		writeError(w, http.StatusNotImplemented, "api_error", "models listing not supported for this upstream")
		return
	}
	resp, err := openaiProvider.DoModels(ctx, openaiProvider.Upstream{
		BaseURL: up.BaseURL,
		APIKey:  string(up.APIKey),
		Headers: up.Headers,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	h.m.ObserveRequest(string(canonical.FacadeAnthropic), up.ProviderType, resp.StatusCode, 0*time.Millisecond)
}

func firstNonEmpty(v, def string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	return v
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mapStatusToAnthropic(upstreamStatus int) int {
	if upstreamStatus == http.StatusTooManyRequests {
		return http.StatusTooManyRequests
	}
	if upstreamStatus == http.StatusUnauthorized || upstreamStatus == http.StatusForbidden {
		return http.StatusUnauthorized
	}
	if upstreamStatus >= 400 && upstreamStatus < 500 {
		return http.StatusBadRequest
	}
	if upstreamStatus >= 500 {
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

func mapTypeToAnthropic(upstreamStatus int) string {
	switch upstreamStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if upstreamStatus >= 400 && upstreamStatus < 500 {
			return "invalid_request_error"
		}
		if upstreamStatus >= 500 {
			return "api_error"
		}
		return "api_error"
	}
}
