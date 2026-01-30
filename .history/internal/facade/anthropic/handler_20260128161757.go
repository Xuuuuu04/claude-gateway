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

	"claude-gateway/internal/canonical"
	"claude-gateway/internal/convert"
	"claude-gateway/internal/metrics"
	anthropicproto "claude-gateway/internal/proto/anthropic"
	"claude-gateway/internal/providers/anthropic"
	openaiProvider "claude-gateway/internal/providers/openai"
	"claude-gateway/internal/router"
)

type Handler struct {
	rtr *router.Router
	m   *metrics.Metrics
}

func NewHandler(rtr *router.Router, m *metrics.Metrics) *Handler {
	return &Handler{rtr: rtr, m: m}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/messages", h.createMessage)
	r.Get("/models", h.listModels)
	return r
}

func (h *Handler) createMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	up, err := h.rtr.PickUpstream(ctx, string(canonical.FacadeAnthropic), req.Model)
	if err != nil {
		if errors.Is(err, router.ErrNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, "overloaded_error", "gateway not configured")
			return
		}
		writeError(w, http.StatusBadGateway, "api_error", "routing failed")
		return
	}

	if up.ProviderType != "anthropic" {
		if up.ProviderType == "openai" && !req.Stream {
			oreq := convert.AnthropicToOpenAIChatRequest(req)
			oreq.Stream = false
			b, err := json.Marshal(oreq)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
				return
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			resp, err := openaiProvider.DoChatCompletions(uctx, openaiProvider.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
			}, b)
			if err != nil {
				writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(raw)
				return
			}
			var oresp convert.OpenAIChatCompletionResponse
			if err := json.Unmarshal(raw, &oresp); err != nil {
				writeError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
				return
			}
			model := req.Model
			if strings.TrimSpace(up.Model) != "" {
				model = up.Model
			}
			aresp := convert.OpenAIResponseToAnthropic(oresp, model)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(aresp)
			return
		}
		writeError(w, http.StatusNotImplemented, "api_error", "provider conversion not implemented yet (streaming requires conversion)")
		return
	}

	targetBody := body
	if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
		req.Model = up.Model
		b, err := json.Marshal(req)
		if err != nil {
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
	defer cancel()

	resp, err := anthropic.DoMessages(uctx, anthropic.Upstream{
		BaseURL: up.BaseURL,
		APIKey:  string(up.APIKey),
		Headers: up.Headers,
		APIVer:  firstNonEmpty(r.Header.Get("anthropic-version"), "2023-06-01"),
		Timeout: timeout,
	}, targetBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		_ = anthropic.CopySSE(w, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
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
