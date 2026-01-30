package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
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
	origModel := req.Model

	clientKey, _ := ctx.Value(canonical.ContextKeyClientKey).(string)
	srcIP := clientIP(r)
	userAgent := strings.TrimSpace(r.UserAgent())
	isTest := isTestRequest(r)
	requestBytes := len(body)

	publish := func(up router.RoutedUpstream, status int, latency time.Duration, errMsg string, inputTokens, outputTokens int64, responseBytes int, ttft int64, tps float64) {
		if h.bus == nil {
			return
		}
		h.bus.Publish(logbus.Event{
			TS:            time.Now(),
			RequestID:     requestID,
			Facade:        string(canonical.FacadeOpenAI),
			RequestModel:  origModel,
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
			TTFTMs:        ttft,
			TPS:           tps,
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
					h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
					writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
					return
				}
				targetBody = b
			}
			if req.Stream {
				targetBody = ensureOpenAIStreamIncludeUsage(targetBody)
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
			if err != nil {
				cancel()
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}

			status = resp.StatusCode
			ok = status < 500 && status != http.StatusTooManyRequests
			if !ok {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_unavailable", 0, 0, 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					_ = resp.Body.Close()
					cancel()
					continue
				}
			}

			copyHeader(w.Header(), resp.Header)
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(resp.StatusCode)

			if req.Stream {
				var inTok, outTok int64
				var respBytes int
				var ttft int64
				var tps float64
				respBytes, inTok, outTok, ttft, tps, err = copyOpenAISSEWithUsage(w, resp.Body, start)
				_ = resp.Body.Close()
				cancel()
				okFinal := err == nil && ok
				h.rtr.EndRequest(up.CredentialID, okFinal, status, time.Since(start))
				h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, okFinal, status)
				publish(up, status, time.Since(start), errString(err), inTok, outTok, respBytes, ttft, tps)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			cancel()
			_, _ = w.Write(raw)
			h.rtr.EndRequest(up.CredentialID, ok, status, time.Since(start))
			h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, ok, status)
			inTok, outTok := extractOpenAIUsage(raw)
			dur := time.Since(start)
			var tps float64
			if outTok > 0 && dur.Seconds() > 0 {
				tps = float64(outTok) / dur.Seconds()
			}
			publish(up, status, dur, "", inTok, outTok, len(raw), dur.Milliseconds(), tps)
			h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
			return

		case "anthropic":
			areq, err := convert.OpenAIToAnthropicMessageRequest(req)
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_request", err.Error())
				return
			}
			areq.Model = up.Model
			areq.Stream = req.Stream
			b, err := json.Marshal(areq)
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
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
			if err != nil {
				cancel()
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}
			status = resp.StatusCode
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				_ = resp.Body.Close()
				cancel()
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error", 0, 0, 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				exclude[up.CredentialID] = true
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
				_ = resp.Body.Close()
				cancel()
				okFinal := err == nil && ok
				h.rtr.EndRequest(up.CredentialID, okFinal, status, time.Since(start))
				h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, okFinal, status)
				publish(up, status, time.Since(start), errString(err), 0, 0, 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				return
			}

			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			cancel()
			var aresp convert.AnthropicMessageResponse
			if err := json.Unmarshal(raw, &aresp); err != nil {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "server_error", "bad_upstream", "invalid upstream response")
				return
			}
			oresp := convert.AnthropicResponseToOpenAI(aresp)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Request-Id", requestID)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oresp)
			h.rtr.EndRequest(up.CredentialID, true, status, time.Since(start))
			h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, true, status)
			inTok, outTok := extractAnthropicUsage(raw)
			dur := time.Since(start)
			var tps float64
			if outTok > 0 && dur.Seconds() > 0 {
				tps = float64(outTok) / dur.Seconds()
			}
			publish(up, status, dur, "", inTok, outTok, len(raw), dur.Milliseconds(), tps)
			h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
			return

		case "gemini":
			if req.Stream {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "provider conversion not implemented yet (streaming requires conversion)")
				return
			}
			greq, model := convert.OpenAIToGeminiRequest(req)
			model = up.Model
			b, err := json.Marshal(greq)
			if err != nil {
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
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
			if err != nil {
				cancel()
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0, 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, http.StatusBadGateway, time.Since(start))
				exclude[up.CredentialID] = true
				if attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}
			status = resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			cancel()
			ok = status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error", 0, 0, len(raw), 0, 0)
				h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
				exclude[up.CredentialID] = true
				if !ok && attempt+1 < maxAttempts {
					continue
				}
				w.Header().Set("X-Request-Id", requestID)
				writeError(w, mapStatusToOpenAI(status), mapTypeToOpenAI(status), mapCodeToOpenAI(status), "upstream error")
				return
			}
			var gres convert.GeminiGenerateContentResponse
			if err := json.Unmarshal(raw, &gres); err != nil {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				writeError(w, http.StatusBadGateway, "server_error", "bad_upstream", "invalid upstream response")
				return
			}
			text, usage := convert.GeminiResponseText(gres)
			oresp := convert.GeminiTextToOpenAI(text, model, usage)
			outRaw, _ := json.Marshal(oresp)
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
			h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, true, status)
			dur := time.Since(start)
			var tps float64
			if outTok > 0 && dur.Seconds() > 0 {
				tps = float64(outTok) / dur.Seconds()
			}
			publish(up, status, dur, "", inTok, outTok, len(outRaw), dur.Milliseconds(), tps)
			h.m.ObserveRequest(string(canonical.FacadeOpenAI), up.ProviderType, status, time.Since(start))
			return

		default:
			h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
			w.Header().Set("X-Request-Id", requestID)
			writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "unknown provider")
			return
		}
	}
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
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

	type responsesCreateRequest struct {
		Model           string          `json:"model"`
		Input           any             `json:"input"`
		Stream          bool            `json:"stream,omitempty"`
		Instructions    string          `json:"instructions,omitempty"`
		MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
		Temperature     *float64        `json:"temperature,omitempty"`
		TopP            *float64        `json:"top_p,omitempty"`
		Tools           json.RawMessage `json:"tools,omitempty"`
		ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	}

	var req responsesCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid json")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model is required")
		return
	}
	origModel := req.Model

	clientKey, _ := ctx.Value(canonical.ContextKeyClientKey).(string)
	srcIP := clientIP(r)
	userAgent := strings.TrimSpace(r.UserAgent())
	isTest := isTestRequest(r)
	requestBytes := len(body)

	publish := func(up router.RoutedUpstream, status int, latency time.Duration, errMsg string, inputTokens, outputTokens int64, responseBytes int, ttft int64, tps float64) {
		if h.bus == nil {
			return
		}
		h.bus.Publish(logbus.Event{
			TS:            time.Now(),
			RequestID:     requestID,
			Facade:        string(canonical.FacadeOpenAI),
			RequestModel:  origModel,
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
			TTFTMs:        ttft,
			TPS:           tps,
			Error:         errMsg,
		})
	}
	up, err := h.rtr.PickUpstream(ctx, clientKey, string(canonical.FacadeOpenAI), req.Model)
	if err != nil {
		if errors.Is(err, router.ErrNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, "server_error", "not_configured", "gateway not configured")
			return
		}
		writeError(w, http.StatusBadGateway, "server_error", "routing_failed", "routing failed")
		return
	}

	if up.ProviderType != "openai" {
		if up.ProviderType == "anthropic" {
			if req.Stream {
				writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "responses streaming conversion not implemented yet")
				return
			}

			msgs, err := responsesInputToChatMessages(req.Input, req.Instructions)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", err.Error())
				return
			}

			chatReq := openaiproto.ChatCompletionsRequest{
				Model:       req.Model,
				Messages:    msgs,
				MaxTokens:   req.MaxOutputTokens,
				Temperature: req.Temperature,
				TopP:        req.TopP,
				Stream:      false,
				Tools:       req.Tools,
				ToolChoice:  req.ToolChoice,
			}

			areq, err := convert.OpenAIToAnthropicMessageRequest(chatReq)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_request", err.Error())
				return
			}
			areq.Model = up.Model
			areq.Stream = false

			start := time.Now()
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
			}, mustJSON(areq))
			if err != nil {
				cancel()
				h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
				publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0, 0, 0)
				writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
				return
			}
			status := resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			cancel()
			ok := status < 500 && status != http.StatusTooManyRequests
			if status < 200 || status >= 300 {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "upstream_error", 0, 0, len(raw), 0, 0)
				writeError(w, mapStatusToOpenAI(status), mapTypeToOpenAI(status), mapCodeToOpenAI(status), "upstream error")
				return
			}

			var aresp convert.AnthropicMessageResponse
			if err := json.Unmarshal(raw, &aresp); err != nil {
				h.rtr.EndRequest(up.CredentialID, false, status, time.Since(start))
				publish(up, status, time.Since(start), "bad_upstream", 0, 0, len(raw), 0, 0)
				writeError(w, http.StatusBadGateway, "server_error", "bad_upstream", "invalid upstream response")
				return
			}

			oresp := convert.AnthropicResponseToOpenAIResponses(aresp, req.Model)
			outRaw, _ := json.Marshal(oresp)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(outRaw)
			h.rtr.EndRequest(up.CredentialID, ok, status, time.Since(start))
			inTok, outTok := extractAnthropicUsage(raw)
			h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, ok, status)
			dur := time.Since(start)
			var tps float64
			if outTok > 0 && dur.Seconds() > 0 {
				tps = float64(outTok) / dur.Seconds()
			}
			publish(up, status, dur, "", inTok, outTok, len(outRaw), dur.Milliseconds(), tps)
			return
		}

		writeError(w, http.StatusNotImplemented, "server_error", "not_implemented", "provider conversion not implemented yet for responses")
		return
	}

	start := time.Now()
	targetBody := body
	if strings.TrimSpace(up.Model) != "" && up.Model != req.Model {
		req.Model = up.Model
		b, err := json.Marshal(req)
		if err != nil {
			h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
			publish(up, 0, time.Since(start), "encode_failed", 0, 0, 0, 0, 0)
			writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "failed to build upstream request")
			return
		}
		targetBody = b
	}
	if req.Stream {
		targetBody = ensureOpenAIStreamIncludeUsage(targetBody)
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
		h.rtr.EndRequest(up.CredentialID, false, 0, time.Since(start))
		h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, false, 0)
		publish(up, 0, time.Since(start), "upstream_failed", 0, 0, 0, 0, 0)
		writeError(w, http.StatusBadGateway, "server_error", "upstream_failed", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		var respBytes int
		var inTok, outTok int64
		var ttft int64
		var tps float64
		respBytes, inTok, outTok, ttft, tps, err = copyOpenAISSEWithUsage(w, resp.Body, start)
		okFinal := err == nil && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
		h.rtr.EndRequest(up.CredentialID, okFinal, resp.StatusCode, time.Since(start))
		h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, okFinal, resp.StatusCode)
		publish(up, resp.StatusCode, time.Since(start), errString(err), inTok, outTok, respBytes, ttft, tps)
		return
	}
	raw, _ := io.ReadAll(resp.Body)
	_, _ = w.Write(raw)
	okFinal := resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
	h.rtr.EndRequest(up.CredentialID, okFinal, resp.StatusCode, time.Since(start))
	inTok, outTok := extractOpenAIUsage(raw)
	h.rtr.RecordRouteResult(up.PoolID, string(canonical.FacadeOpenAI), origModel, up.CredentialID, okFinal, resp.StatusCode)
	dur := time.Since(start)
	var tps float64
	if outTok > 0 && dur.Seconds() > 0 {
		tps = float64(outTok) / dur.Seconds()
	}
	publish(up, resp.StatusCode, dur, "", inTok, outTok, len(raw), dur.Milliseconds(), tps)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func responsesInputToChatMessages(input any, instructions string) ([]any, error) {
	msgs := make([]any, 0, 8)
	if strings.TrimSpace(instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": strings.TrimSpace(instructions)})
	}

	switch v := input.(type) {
	case nil:
		return nil, errors.New("input is required")
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
		return msgs, nil
	case []any:
		for _, it := range v {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, errors.New("input items must be objects")
			}
			itemType, _ := m["type"].(string)
			itemType = strings.TrimSpace(itemType)
			switch itemType {
			case "", "message":
				role, _ := m["role"].(string)
				role = strings.TrimSpace(role)
				if role == "developer" {
					role = "system"
				}
				if role == "" {
					return nil, errors.New("message items must include role")
				}
				msg := map[string]any{"role": role}

				content := m["content"]
				switch c := content.(type) {
				case nil, string:
					msg["content"] = c
				case []any:
					parts := make([]any, 0, len(c))
					for _, pi := range c {
						pm, ok := pi.(map[string]any)
						if !ok {
							return nil, errors.New("content parts must be objects")
						}
						pt, _ := pm["type"].(string)
						pt = strings.TrimSpace(pt)
						switch pt {
						case "input_text", "output_text":
							t, _ := pm["text"].(string)
							parts = append(parts, map[string]any{"type": "text", "text": t})
						case "refusal":
							t, _ := pm["refusal"].(string)
							parts = append(parts, map[string]any{"type": "text", "text": t})
						case "input_image":
							u, _ := pm["image_url"].(string)
							u = strings.TrimSpace(u)
							if u == "" {
								return nil, errors.New("input_image.image_url is required")
							}
							if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "data:image/")) {
								return nil, errors.New("input_image.image_url must be https:// or data:image/*")
							}
							parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
						case "text":
							t, _ := pm["text"].(string)
							parts = append(parts, map[string]any{"type": "text", "text": t})
						case "image_url":
							img, _ := pm["image_url"].(map[string]any)
							if img == nil {
								return nil, errors.New("image_url.image_url is required")
							}
							u, _ := img["url"].(string)
							u = strings.TrimSpace(u)
							if u == "" {
								return nil, errors.New("image_url.image_url.url is required")
							}
							if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "data:image/")) {
								return nil, errors.New("image_url must be https:// or data:image/*")
							}
							parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
						default:
							return nil, errors.New("unsupported content part type")
						}
					}
					msg["content"] = parts
				default:
					return nil, errors.New("unsupported message content type")
				}

				if tc := m["tool_call_id"]; tc != nil {
					msg["tool_call_id"] = tc
				}
				if tcs := m["tool_calls"]; tcs != nil {
					msg["tool_calls"] = tcs
				}
				msgs = append(msgs, msg)
			case "function_call":
				callID, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				callID = strings.TrimSpace(callID)
				name = strings.TrimSpace(name)
				if callID == "" || name == "" {
					return nil, errors.New("function_call requires call_id and name")
				}
				msgs = append(msgs, map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				callID = strings.TrimSpace(callID)
				if callID == "" {
					return nil, errors.New("function_call_output requires call_id")
				}
				output := m["output"]
				outStr := ""
				switch o := output.(type) {
				case nil:
					outStr = ""
				case string:
					outStr = o
				default:
					b, _ := json.Marshal(o)
					outStr = string(b)
				}
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      outStr,
				})
			default:
				return nil, errors.New("unsupported input item type")
			}
		}
		return msgs, nil
	default:
		return nil, errors.New("input must be string or array")
	}
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clientKey, _ := ctx.Value(canonical.ContextKeyClientKey).(string)
	if clientKey == "" {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthorized", "invalid client key")
		return
	}

	models, err := h.rtr.GetPoolModels(ctx, clientKey)
	if err != nil {
		suffix := clientKey
		if len(suffix) > 6 {
			suffix = suffix[len(suffix)-6:]
		}
		log.Printf("[OpenAI] ListModels failed for key ...%s: %v", suffix, err)
		if errors.Is(err, router.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthorized", "invalid client key")
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
		return
	}

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	type modelList struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	res := modelList{
		Object: "list",
		Data:   make([]modelEntry, 0, len(models)),
	}
	now := time.Now().Unix()
	for _, m := range models {
		res.Data = append(res.Data, modelEntry{
			ID:      m,
			Object:  "model",
			Created: now,
			OwnedBy: "gateway",
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
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

func clientIP(r *http.Request) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && strings.TrimSpace(host) != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func isTestRequest(r *http.Request) bool {
	v := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Gateway-Test")))
	return v == "1" || v == "true" || v == "yes"
}

func parseInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case json.Number:
		i, _ := t.Int64()
		return i
	case string:
		n := strings.TrimSpace(t)
		if n == "" {
			return 0
		}
		i, _ := json.Number(n).Int64()
		return i
	default:
		return 0
	}
}

func extractOpenAIUsage(raw []byte) (int64, int64) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return 0, 0
	}
	u, _ := root["usage"].(map[string]any)
	if u == nil {
		return 0, 0
	}
	in := parseInt64(u["prompt_tokens"])
	if in == 0 {
		in = parseInt64(u["input_tokens"])
	}
	out := parseInt64(u["completion_tokens"])
	if out == 0 {
		out = parseInt64(u["output_tokens"])
	}
	return in, out
}

func extractAnthropicUsage(raw []byte) (int64, int64) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return 0, 0
	}
	u, _ := root["usage"].(map[string]any)
	if u == nil {
		return 0, 0
	}
	in := parseInt64(u["input_tokens"])
	out := parseInt64(u["output_tokens"])
	return in, out
}

func ensureOpenAIStreamIncludeUsage(body []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	so, _ := root["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	if _, ok := so["include_usage"]; !ok {
		so["include_usage"] = true
	}
	root["stream_options"] = so
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func copyOpenAISSEWithUsage(w http.ResponseWriter, r io.Reader, startTime time.Time) (int, int64, int64, int64, float64, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		n, err := io.Copy(w, r)
		return int(n), 0, 0, 0, 0, err
	}

	br := bufio.NewReader(r)
	var (
		respBytes  int
		inTok      int64
		outTok     int64
		ttft       int64
		tps        float64
		chunkCount int
		firstToken time.Time
		lastToken  time.Time
	)
	for {
		block, err := readSSEBlock(br)
		if block != "" {
			now := time.Now()
			if ttft == 0 {
				ttft = now.Sub(startTime).Milliseconds()
				firstToken = now
			}
			lastToken = now
			chunkCount++

			b := []byte(block)
			n, werr := w.Write(b)
			respBytes += n
			if werr != nil {
				return respBytes, inTok, outTok, ttft, tps, werr
			}
			n2, werr2 := w.Write([]byte("\n"))
			respBytes += n2
			if werr2 != nil {
				return respBytes, inTok, outTok, ttft, tps, werr2
			}
			flusher.Flush()

			data := extractSSEData(block)
			if data == "[DONE]" {
				if chunkCount > 1 && !lastToken.IsZero() && !firstToken.IsZero() {
					dur := lastToken.Sub(firstToken).Seconds()
					if dur > 0 {
						tps = float64(chunkCount-1) / dur
					}
				}
				return respBytes, inTok, outTok, ttft, tps, nil
			}
			var chunk map[string]any
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if u, _ := chunk["usage"].(map[string]any); u != nil {
					inTok = parseInt64(u["prompt_tokens"])
					if inTok == 0 {
						inTok = parseInt64(u["input_tokens"])
					}
					outTok = parseInt64(u["completion_tokens"])
					if outTok == 0 {
						outTok = parseInt64(u["output_tokens"])
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if chunkCount > 1 && !lastToken.IsZero() && !firstToken.IsZero() {
					dur := lastToken.Sub(firstToken).Seconds()
					if dur > 0 {
						tps = float64(chunkCount-1) / dur
					}
				}
				return respBytes, inTok, outTok, ttft, tps, nil
			}
			return respBytes, inTok, outTok, ttft, tps, err
		}
	}
}

func readSSEBlock(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && b.Len() > 0 {
				return b.String(), io.EOF
			}
			return "", err
		}
		if line == "\n" || line == "\r\n" {
			return b.String(), nil
		}
		b.WriteString(line)
	}
}

func extractSSEData(block string) string {
	lines := strings.Split(block, "\n")
	var dataLines []string
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
		}
	}
	return strings.TrimSpace(strings.Join(dataLines, "\n"))
}
