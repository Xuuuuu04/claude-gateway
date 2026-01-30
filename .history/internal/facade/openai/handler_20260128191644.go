package openai

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
	openaiproto "claude-gateway/internal/proto/openai"
	anthropicProvider "claude-gateway/internal/providers/anthropic"
	geminiProvider "claude-gateway/internal/providers/gemini"
	"claude-gateway/internal/providers/openai"
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
	r.Post("/chat/completions", h.chatCompletions)
	r.Post("/responses", h.responses)
	r.Get("/models", h.listModels)
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := strings.TrimSpace(r.Header.Get("x-request-id"))
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-Id", requestID)

	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "failed to read request body")
		return
	}

	var req openaiproto.ChatCompletionsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid json")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		w.Header().Set("X-Request-Id", requestID)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model is required")
		return
	}

	publish := func(up router.RoutedUpstream, status int, latency time.Duration, errMsg string) {
		if h.bus == nil {
			return
		}
		h.bus.Publish(logbus.Event{
			TS:           time.Now(),
			RequestID:    requestID,
			Facade:       string(canonical.FacadeOpenAI),
			Model:        req.Model,
			ProviderType: up.ProviderType,
			PoolID:       up.PoolID,
			ChannelID:    up.ChannelID,
			Status:       status,
			LatencyMs:    latency.Milliseconds(),
			Error:        errMsg,
		})
	}

	maxAttempts := 1
	if !req.Stream {
		maxAttempts = 2
	}
	exclude := map[uint64]bool{}

	clientKey, _ := ctx.Value(canonical.ContextKeyClientKey).(string)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var up router.RoutedUpstream
		var err error
		if attempt == 0 {
			up, err = h.rtr.PickUpstream(ctx, clientKey, string(canonical.FacadeOpenAI), req.Model)
		} else {
			up, err = h.rtr.PickUpstreamExclude(ctx, clientKey, string(canonical.FacadeOpenAI), req.Model, exclude)
		}
		if err != nil {
			if errors.Is(err, router.ErrNotConfigured) {
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusServiceUnavailable, "server_error", "not_configured", "gateway not configured")
				return
			}
			w.Header().Set("X-Request-Id", requestID)
			writeError(w, http.StatusBadGateway, "server_error", "routing_failed", "routing failed")
			return
		}

		start := time.Now()
		status := 0
		ok := false

		switch up.ProviderType {
		case "openai":
			targetBody := body
			if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
				req.Model = up.Model
				b, err := json.Marshal(req)
				if err != nil {
					h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
					writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
					return
				}
				targetBody = b
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)
			resp, err := openai.DoChatCompletions(uctx, openai.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
			}, targetBody)
			cancel()
			if err != nil {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed")
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}
			defer resp.Body.Close()

			status = resp.StatusCode
			ok = status < 500 && status != http.StatusTooManyRequests
			if !ok {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_unavailable")
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					_ = resp.Body.Close()
					continue
				}
			}

			copyHeader(w.Header(), resp.Header)
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(resp.StatusCode)

			if req.Stream {
				err = openai.CopySSE(w, resp.Body)
				h.rtr.EndRequest(up.ChannelID, err == nil && ok, status, time.Since(start))
				publish(up, status, time.Since(start), errString(err))
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				return
			}
			_, _ = io.Copy(w, resp.Body)
			h.rtr.EndRequest(up.ChannelID, ok, status, time.Since(start))
			publish(up, status, time.Since(start), "")
			h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
			return

		case "anthropic":
			areq := convert.OpenAIToAnthropicMessageRequest(req)
			areq.Model = up.Model
			areq.Stream = req.Stream
			b, err := json.Marshal(areq)
			if err != nil {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
				return
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)
			resp, err := anthropicProvider.DoMessages(uctx, anthropicProvider.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
				APIVer:  "2023-06-01",
				Timeout: timeout,
			}, b)
			cancel()
			if err != nil {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed")
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error")
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				exclude[up.ChannelID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, mapStatusToOpenAI(status), mapTypeToOpenAI(status), mapCodeToOpenAI(status), "upstream error")
				return
			}
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Request-Id", requestID)
				w.WriteHeader(http.StatusOK)
				err := streamconv.AnthropicToOpenAI(w, resp.Body, up.Model)
				h.rtr.EndRequest(up.ChannelID, err == nil && ok, status, time.Since(start))
				publish(up, status, time.Since(start), errString(err))
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				return
			}

			raw, _ := io.ReadAll(resp.Body)
			var aresp convert.AnthropicMessageResponse
			if err := json.Unmarshal(raw, &aresp); err != nil {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "server_error", "bad_upstream", "invalid upstream response")
				return
			}
			oresp := convert.AnthropicResponseToOpenAI(aresp)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oresp)
			h.rtr.EndRequest(up.ChannelID, true, status, time.Since(start))
			publish(up, status, time.Since(start), "")
			h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
			return

		case "gemini":
			if req.Stream {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "provider conversion not implemented yet (streaming requires conversion)")
				return
			}
			greq, model := convert.OpenAIToGeminiRequest(req)
			model = up.Model
			b, err := json.Marshal(greq)
			if err != nil {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
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
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed")
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error")
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				exclude[up.ChannelID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, mapStatusToOpenAI(status), mapTypeToOpenAI(status), mapCodeToOpenAI(status), "upstream error")
				return
			}
			var gres convert.GeminiGenerateContentResponse
			if err := json.Unmarshal(raw, &gres); err != nil {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "server_error", "bad_upstream", "invalid upstream response")
				return
			}
			text, usage := convert.GeminiResponseText(gres)
			oresp := convert.GeminiTextToOpenAI(text, model, usage)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oresp)
			h.rtr.EndRequest(up.ChannelID, true, status, time.Since(start))
			publish(up, status, time.Since(start), "")
			h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
			return

		default:
			h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
			w.Header().Set("X-Request-Id", requestID)
			writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "unknown provider")
			return
		}
	}
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "failed to read request body")
		return
	}

	var req openaiproto.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid json")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model is required")
		return
	}

	up, err := h.rtr.PickUpstream(ctx, string(canonical.FacadeOpenAI), req.Model)
	if err != nil {
		if errors.Is(err, router.ErrNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, "server_error", "not_configured", "gateway not configured")
			return
		}
		writeError(w, http.StatusBadGateway, "server_error", "routing_failed", "routing failed")
		return
	}

	if up.ProviderType != "openai" {
		writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "provider conversion not implemented yet for responses")
		return
	}

	start := time.Now()
	targetBody := body
	if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
		req.Model = up.Model
		b, err := json.Marshal(req)
		if err != nil {
			h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
			writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
			return
		}
		targetBody = b
	}

	timeout := up.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	uctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := openai.DoResponses(uctx, openai.Upstream{
		BaseURL: up.BaseURL,
		APIKey:  string(up.APIKey),
		Headers: up.Headers,
	}, targetBody)
	if err != nil {
		h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
		writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		err = openai.CopySSE(w, resp.Body)
		h.rtr.EndRequest(up.ChannelID, err == nil && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests, resp.StatusCode, time.Since(start))
		return
	}
	_, _ = io.Copy(w, resp.Body)
	h.rtr.EndRequest(up.ChannelID, resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests, resp.StatusCode, time.Since(start))
}

func (h *Handler) listModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	h.m.ObserveRequest(string(canonical.FacadeOpenAI), "gateway", http.StatusOK, 0*time.Millisecond)
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

func mapStatusToOpenAI(upstreamStatus int) int {
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

func mapTypeToOpenAI(upstreamStatus int) string {
	switch upstreamStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if upstreamStatus >= 400 && upstreamStatus < 500 {
			return "invalid_request_error"
		}
		return "server_error"
	}
}

func mapCodeToOpenAI(upstreamStatus int) string {
	switch upstreamStatus {
	case http.StatusTooManyRequests:
		return "rate_limit"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "unauthorized"
	default:
		if upstreamStatus >= 500 {
			return "upstream_error"
		}
		return "bad_request"
	}
}
