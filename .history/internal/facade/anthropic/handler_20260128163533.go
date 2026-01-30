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
)

type Handler struct {
	rtr *router.Router
	m   *metrics.Metrics
	bus *logbus.Bus
}

func NewHandler(rtr *router.Router, m *metrics.Metrics, bus *logbus.Bus) *Handler {
	return &Handler{rtr: rtr, m: m, bus: bus}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/messages", h.createMessage)
	r.Get("/models", h.listModels)
	return r
}

func (h *Handler) createMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := strings.TrimSpace(r.Header.Get("x-request-id"))
	if requestID == "" {
		requestID = uuid.NewString()
	}

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
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model and max_tokens are required")
		return
	}

	publish := func(up router.RoutedUpstream, status int, latency time.Duration, errMsg string) {
		if h.bus == nil {
			return
		}
		h.bus.Publish(logbus.Event{
			TS:           time.Now(),
			RequestID:    requestID,
			Facade:       string(canonical.FacadeAnthropic),
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

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var up router.RoutedUpstream
		var err error
		if attempt == 0 {
			up, err = h.rtr.PickUpstream(ctx, string(canonical.FacadeAnthropic), req.Model)
		} else {
			up, err = h.rtr.PickUpstreamExclude(ctx, string(canonical.FacadeAnthropic), req.Model, exclude)
		}
		if err != nil {
			if errors.Is(err, router.ErrNotConfigured) {
				writeError(w, http.StatusServiceUnavailable, "overloaded_error", "gateway not configured")
				return
			}
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
					h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
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
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed")
				exclude[up.ChannelID] = true
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
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					continue
				}
			}

			copyHeader(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)

			if req.Stream {
				err = anthropic.CopySSE(w, resp.Body)
				h.rtr.EndRequest(up.ChannelID, err == nil && ok, status, time.Since(start))
				publish(up, status, time.Since(start), errString(err))
				return
			}
			_, _ = io.Copy(w, resp.Body)
			h.rtr.EndRequest(up.ChannelID, ok, status, time.Since(start))
			publish(up, status, time.Since(start), "")
			return

		case "openai":
			if req.Stream {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				writeError(w, http.StatusNotImplemented, "api_error", "provider conversion not implemented yet (streaming requires conversion)")
				return
			}
			oreq := convert.AnthropicToOpenAIChatRequest(req)
			oreq.Model = up.Model
			oreq.Stream = false
			b, err := json.Marshal(oreq)
			if err != nil {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
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
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				exclude[up.ChannelID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				writeError(w, http.StatusBadGateway, "api_error", "upstream error")
				return
			}
			var oresp convert.OpenAIChatCompletionResponse
			if err := json.Unmarshal(raw, &oresp); err != nil {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
				return
			}
			aresp := convert.OpenAIResponseToAnthropic(oresp, up.Model)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(aresp)
			h.rtr.EndRequest(up.ChannelID, true, status, time.Since(start))
			return

		case "gemini":
			if req.Stream {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				writeError(w, http.StatusNotImplemented, "api_error", "provider conversion not implemented yet (streaming requires conversion)")
				return
			}
			greq, model := convert.AnthropicToGeminiRequest(req)
			model = up.Model
			b, err := json.Marshal(greq)
			if err != nil {
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
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
				h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
				exclude[up.ChannelID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			status = resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				exclude[up.ChannelID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				writeError(w, http.StatusBadGateway, "api_error", "upstream error")
				return
			}
			var gres convert.GeminiGenerateContentResponse
			if err := json.Unmarshal(raw, &gres); err != nil {
				h.rtr.EndRequest(up.ChannelID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
				return
			}
			text, usage := convert.GeminiResponseText(gres)
			aresp := convert.GeminiTextToAnthropic(text, model, usage)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(aresp)
			h.rtr.EndRequest(up.ChannelID, true, status, time.Since(start))
			return

		default:
			h.rtr.EndRequest(up.ChannelID, false, 0, time.Since(start))
			writeError(w, http.StatusNotImplemented, "api_error", "unknown provider")
			return
		}
	}
}

func (h *Handler) listModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"data":[],"object":"list"}`))
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
