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

	"claude-gateway/internal/canonical"
	"claude-gateway/internal/convert"
	"claude-gateway/internal/metrics"
	anthropicProvider "claude-gateway/internal/providers/anthropic"
	"claude-gateway/internal/providers/openai"
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
	r.Post("/chat/completions", h.chatCompletions)
	r.Post("/responses", h.responses)
	r.Get("/models", h.listModels)
	return r
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "failed to read request body")
		return
	}

	var req ChatCompletionsRequest
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
		if up.ProviderType == "anthropic" && !req.Stream {
			areq := convert.OpenAIToAnthropicMessageRequest(req)
			areq.Stream = false
			b, err := json.Marshal(areq)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
				return
			}

			timeout := up.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			uctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			resp, err := anthropicProvider.DoMessages(uctx, anthropicProvider.Upstream{
				BaseURL: up.BaseURL,
				APIKey:  string(up.APIKey),
				Headers: up.Headers,
				APIVer:  "2023-06-01",
				Timeout: timeout,
			}, b)
			if err != nil {
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
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
			var aresp convert.AnthropicMessageResponse
			if err := json.Unmarshal(raw, &aresp); err != nil {
				writeError(w, http.StatusBadGateway, "server_error", "bad_upstream", "invalid upstream response")
				return
			}
			oresp := convert.AnthropicResponseToOpenAI(aresp)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oresp)
			return
		}
		writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "provider conversion not implemented yet (streaming requires conversion)")
		return
	}

	targetBody := body
	if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
		req.Model = up.Model
		b, err := json.Marshal(req)
		if err != nil {
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

	resp, err := openai.DoChatCompletions(uctx, openai.Upstream{
		BaseURL: up.BaseURL,
		APIKey:  string(up.APIKey),
		Headers: up.Headers,
	}, targetBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		_ = openai.CopySSE(w, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "failed to read request body")
		return
	}

	var req ResponsesRequest
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

	targetBody := body
	if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
		req.Model = up.Model
		b, err := json.Marshal(req)
		if err != nil {
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
		writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		_ = openai.CopySSE(w, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) listModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
