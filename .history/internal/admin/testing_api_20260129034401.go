package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	anthropicProvider "claude-gateway/internal/providers/anthropic"
	openaiProvider "claude-gateway/internal/providers/openai"
)

type providerModelsResponse struct {
	ProviderID        uint64          `json:"provider_id"`
	ModelsJSON        json.RawMessage `json:"models_json"`
	ModelsRefreshedAt string          `json:"models_refreshed_at,omitempty"`
}

func (h *Handler) getProviderModels(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}

	var (
		modelsJSON  []byte
		refreshedAt sql.NullTime
	)
	if err := h.db.QueryRowContext(r.Context(), `SELECT models_json, models_refreshed_at FROM providers WHERE id=?`, id).Scan(&modelsJSON, &refreshedAt); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}
	out := providerModelsResponse{
		ProviderID: id,
		ModelsJSON: modelsJSON,
	}
	if out.ModelsJSON == nil {
		out.ModelsJSON = []byte("null")
	}
	if refreshedAt.Valid {
		out.ModelsRefreshedAt = refreshedAt.Time.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, out)
}

type refreshModelsRequest struct {
	CredentialID uint64 `json:"credential_id,omitempty"`
	TimeoutMs    int    `json:"timeout_ms,omitempty"`
}

func (h *Handler) refreshProviderModels(w http.ResponseWriter, r *http.Request) {
	providerID, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in refreshModelsRequest
	_ = readJSON(r, &in)
	timeout := 10 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}

	prov, err := h.loadProvider(r.Context(), providerID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}
	credID := in.CredentialID
	if credID == 0 {
		if err := h.db.QueryRowContext(r.Context(), `SELECT id FROM credentials WHERE provider_id=? AND enabled=1 ORDER BY id DESC LIMIT 1`, providerID).Scan(&credID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no enabled credential for provider"})
			return
		}
	}
	key, err := h.decryptCredentialKey(r.Context(), credID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "credential not found"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	modelIDs, raw, status, err := h.fetchProviderModels(ctx, prov, key)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "status": status})
		return
	}

	modelsJSON := raw
	if modelsJSON == nil {
		b, _ := json.Marshal(modelIDs)
		modelsJSON = b
	}

	_, _ = h.db.ExecContext(r.Context(), `UPDATE providers SET models_json=?, models_refreshed_at=NOW() WHERE id=?`, modelsJSON, providerID)
	writeJSON(w, http.StatusOK, map[string]any{"provider_id": providerID, "count": len(modelIDs)})
}

type credentialTestRequest struct {
	Model     string `json:"model,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

type credentialTestResponse struct {
	CredentialID uint64 `json:"credential_id"`
	ProviderID   uint64 `json:"provider_id"`
	ProviderType string `json:"provider_type"`
	OK           bool   `json:"ok"`
	Status       int    `json:"status"`
	LatencyMs    int64  `json:"latency_ms"`
	Model        string `json:"model,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (h *Handler) testCredential(w http.ResponseWriter, r *http.Request) {
	credID, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in credentialTestRequest
	_ = readJSON(r, &in)
	timeout := 10 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}

	provID, prov, key, err := h.loadCredentialAndProvider(r.Context(), credID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "credential not found"})
		return
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = strings.TrimSpace(h.pickProviderTestModel(r.Context(), provID))
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	ok, status, errMsg := h.probeUpstream(ctx, prov, key, model)
	lat := time.Since(start).Milliseconds()
	resp := credentialTestResponse{
		CredentialID: credID,
		ProviderID:   provID,
		ProviderType: prov.Type,
		OK:           ok,
		Status:       status,
		LatencyMs:    lat,
		Model:        model,
		Error:        errMsg,
	}

	_ = h.persistCredentialTestResult(r.Context(), credID, ok, status, lat, errMsg, model)
	writeJSON(w, http.StatusOK, resp)
}

type providerBatchTestRequest struct {
	TimeoutMs        int `json:"timeout_ms,omitempty"`
	ConcurrencyLimit int `json:"concurrency_limit,omitempty"`
}

func (h *Handler) testProviderCredentials(w http.ResponseWriter, r *http.Request) {
	providerID, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in providerBatchTestRequest
	_ = readJSON(r, &in)
	timeout := 10 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}
	limit := 5
	if in.ConcurrencyLimit > 0 && in.ConcurrencyLimit <= 20 {
		limit = in.ConcurrencyLimit
	}

	rows, err := h.db.QueryContext(r.Context(), `SELECT id FROM credentials WHERE provider_id=? AND enabled=1 ORDER BY id DESC`, providerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"provider_id": providerID, "count": 0, "ok": 0, "fail": 0})
		return
	}

	sem := make(chan struct{}, limit)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCnt   int
		failCnt int
	)
	for _, id := range ids {
		wg.Add(1)
		go func(credID uint64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			provID, prov, key, err := h.loadCredentialAndProvider(r.Context(), credID)
			if err != nil || provID == 0 {
				mu.Lock()
				failCnt++
				mu.Unlock()
				return
			}
			model := strings.TrimSpace(h.pickProviderTestModel(r.Context(), provID))

			start := time.Now()
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			ok, status, errMsg := h.probeUpstream(ctx, prov, key, model)
			cancel()
			lat := time.Since(start).Milliseconds()
			_ = h.persistCredentialTestResult(r.Context(), credID, ok, status, lat, errMsg, model)

			mu.Lock()
			if ok {
				okCnt++
			} else {
				failCnt++
			}
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{"provider_id": providerID, "count": len(ids), "ok": okCnt, "fail": failCnt})
}

type poolTestRequest struct {
	Facade    string `json:"facade,omitempty"`
	Model     string `json:"model,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

func (h *Handler) testPool(w http.ResponseWriter, r *http.Request) {
	poolID, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in poolTestRequest
	_ = readJSON(r, &in)
	timeout := 10 * time.Second
	if in.TimeoutMs > 0 {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}
	facade := strings.ToLower(strings.TrimSpace(in.Facade))
	if facade == "" {
		facade = "openai"
	}

	var (
		clientKey string
	)
	if err := h.db.QueryRowContext(r.Context(), `SELECT client_key FROM pools WHERE id=?`, poolID).Scan(&clientKey); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "pool not found"})
		return
	}

	scheme := "http"
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf != "" {
		parts := strings.Split(xf, ",")
		scheme = strings.TrimSpace(parts[0])
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing host"})
		return
	}

	var (
		path string
		body any
	)
	model := strings.TrimSpace(in.Model)
	switch facade {
	case "anthropic":
		path = "/v1/messages"
		body = map[string]any{"model": model, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "ping"}}}
	default:
		path = "/v1/chat/completions"
		body = map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "ping"}}, "max_tokens": 1, "stream": false}
	}

	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, scheme+"://"+host+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(clientKey))

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	lat := time.Since(start).Milliseconds()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	writeJSON(w, http.StatusOK, map[string]any{
		"pool_id":    poolID,
		"facade":     facade,
		"model":      model,
		"ok":         ok,
		"status":     resp.StatusCode,
		"latency_ms": lat,
		"request_id": resp.Header.Get("X-Request-Id"),
	})
}

type loadedProvider struct {
	ID      uint64
	Type    string
	BaseURL string
	Headers map[string]string
}

func (h *Handler) loadProvider(ctx context.Context, providerID uint64) (loadedProvider, error) {
	var (
		typ      string
		baseURL  string
		hdrsJSON []byte
	)
	if err := h.db.QueryRowContext(ctx, `SELECT type, base_url, default_headers_json FROM providers WHERE id=?`, providerID).Scan(&typ, &baseURL, &hdrsJSON); err != nil {
		return loadedProvider{}, err
	}
	hdrs := map[string]string{}
	_ = json.Unmarshal(hdrsJSON, &hdrs)
	return loadedProvider{ID: providerID, Type: typ, BaseURL: baseURL, Headers: hdrs}, nil
}

func (h *Handler) decryptCredentialKey(ctx context.Context, credentialID uint64) (string, error) {
	var blob []byte
	if err := h.db.QueryRowContext(ctx, `SELECT api_key_ciphertext FROM credentials WHERE id=?`, credentialID).Scan(&blob); err != nil {
		return "", err
	}
	plain, err := h.cipher.Decrypt(blob)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (h *Handler) loadCredentialAndProvider(ctx context.Context, credentialID uint64) (uint64, loadedProvider, string, error) {
	var (
		providerID uint64
		blob       []byte
		typ        string
		baseURL    string
		hdrsJSON   []byte
	)
	err := h.db.QueryRowContext(ctx, `SELECT c.provider_id, c.api_key_ciphertext, p.type, p.base_url, p.default_headers_json FROM credentials c JOIN providers p ON p.id=c.provider_id WHERE c.id=?`, credentialID).
		Scan(&providerID, &blob, &typ, &baseURL, &hdrsJSON)
	if err != nil {
		return 0, loadedProvider{}, "", err
	}
	plain, err := h.cipher.Decrypt(blob)
	if err != nil {
		return 0, loadedProvider{}, "", err
	}
	hdrs := map[string]string{}
	_ = json.Unmarshal(hdrsJSON, &hdrs)
	return providerID, loadedProvider{ID: providerID, Type: typ, BaseURL: baseURL, Headers: hdrs}, string(plain), nil
}

func (h *Handler) persistCredentialTestResult(ctx context.Context, credentialID uint64, ok bool, status int, latencyMs int64, errMsg string, model string) error {
	if len(errMsg) > 900 {
		errMsg = errMsg[:900]
	}
	var okv any = nil
	okv = ok
	if status == 0 {
		okv = false
	}
	_, err := h.db.ExecContext(ctx,
		`UPDATE credentials SET last_test_at=NOW(), last_test_ok=?, last_test_status=?, last_test_latency_ms=?, last_test_error=?, last_test_model=? WHERE id=?`,
		okv, nullInt(status), nullInt64(latencyMs), nullStr(errMsg), nullStr(model), credentialID)
	return err
}

func (h *Handler) pickProviderTestModel(ctx context.Context, providerID uint64) string {
	var modelsJSON []byte
	_ = h.db.QueryRowContext(ctx, `SELECT models_json FROM providers WHERE id=?`, providerID).Scan(&modelsJSON)
	if len(modelsJSON) == 0 {
		return ""
	}
	var ids []string
	if err := json.Unmarshal(modelsJSON, &ids); err != nil {
		return ""
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (h *Handler) fetchProviderModels(ctx context.Context, prov loadedProvider, apiKey string) ([]string, []byte, int, error) {
	switch strings.ToLower(strings.TrimSpace(prov.Type)) {
	case "openai":
		resp, err := openaiProvider.DoModels(ctx, openaiProvider.Upstream{BaseURL: prov.BaseURL, APIKey: apiKey, Headers: prov.Headers})
		if err != nil {
			return nil, nil, 0, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, nil, resp.StatusCode, errors.New("upstream models request failed")
		}
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &parsed)
		var out []string
		for _, it := range parsed.Data {
			if strings.TrimSpace(it.ID) != "" {
				out = append(out, it.ID)
			}
		}
		return out, raw, resp.StatusCode, nil
	case "anthropic":
		resp, err := anthropicProvider.DoModels(ctx, anthropicProvider.Upstream{BaseURL: prov.BaseURL, APIKey: apiKey, Headers: prov.Headers, APIVer: "2023-06-01"})
		if err != nil {
			return nil, nil, 0, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, nil, resp.StatusCode, errors.New("upstream models request failed")
		}
		ids := parseModelIDsFromDataList(raw)
		return ids, raw, resp.StatusCode, nil
	default:
		return nil, nil, 0, errors.New("unsupported provider type")
	}
}

func parseModelIDsFromDataList(raw []byte) []string {
	var v struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var out []string
	for _, it := range v.Data {
		id, _ := it["id"].(string)
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func (h *Handler) probeUpstream(ctx context.Context, prov loadedProvider, apiKey, model string) (bool, int, string) {
	typ := strings.ToLower(strings.TrimSpace(prov.Type))
	if typ == "openai" {
		resp, err := openaiProvider.DoModels(ctx, openaiProvider.Upstream{BaseURL: prov.BaseURL, APIKey: apiKey, Headers: prov.Headers})
		if err != nil {
			ok2, st2, msg2 := h.probeOpenAIChat(ctx, prov, apiKey, model)
			if st2 != 0 {
				return ok2, st2, msg2
			}
			return false, 0, err.Error()
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		ok := resp.StatusCode >= 200 && resp.StatusCode < 300
		errMsg := ""
		if !ok {
			errMsg = "upstream models request failed"
		}
		return ok, resp.StatusCode, errMsg
	}
	if typ == "anthropic" {
		if strings.TrimSpace(model) == "" {
			return false, 0, "model is required"
		}
		payload := map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
			"stream":     false,
		}
		b, _ := json.Marshal(payload)
		resp, err := anthropicProvider.DoMessages(ctx, anthropicProvider.Upstream{
			BaseURL: prov.BaseURL,
			APIKey:  apiKey,
			Headers: prov.Headers,
			APIVer:  "2023-06-01",
			Timeout: 0,
		}, b)
		if err != nil {
			return false, 0, err.Error()
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		ok := resp.StatusCode >= 200 && resp.StatusCode < 300
		errMsg := ""
		if !ok {
			errMsg = "upstream messages request failed"
		}
		return ok, resp.StatusCode, errMsg
	}
	return false, 0, "unsupported provider type"
}

func (h *Handler) probeOpenAIChat(ctx context.Context, prov loadedProvider, apiKey, model string) (bool, int, string) {
	if strings.TrimSpace(model) == "" {
		return false, 0, "model is required"
	}
	payload := map[string]any{
		"model":      model,
		"stream":     false,
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
	}
	b, _ := json.Marshal(payload)
	resp, err := openaiProvider.DoChatCompletions(ctx, openaiProvider.Upstream{BaseURL: prov.BaseURL, APIKey: apiKey, Headers: prov.Headers}, b)
	if err != nil {
		return false, 0, err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	errMsg := ""
	if !ok {
		errMsg = "upstream chat request failed"
	}
	return ok, resp.StatusCode, errMsg
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func parseIntHeader(v string) int {
	i, _ := strconv.Atoi(strings.TrimSpace(v))
	return i
}
