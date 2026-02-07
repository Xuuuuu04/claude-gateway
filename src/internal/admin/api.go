package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) logsStream(w http.ResponseWriter, r *http.Request) {
	if h.bus == nil {
		http.Error(w, "log stream disabled", http.StatusNotImplemented)
		return
	}
	h.bus.ServeSSE(w, r)
}

func (h *Handler) listLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	page := 1
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 {
			limit = l
		}
	}
	if v := r.URL.Query().Get("page"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			page = p
		}
	}
	offset := (page - 1) * limit

	var total int64
	_ = h.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM request_logs").Scan(&total)

	rows, err := h.db.QueryContext(r.Context(),
		`SELECT id, pool_id, provider_id, credential_id, client_key, src_ip, user_agent, is_test, stream, request_bytes, response_bytes, input_tokens, output_tokens, facade, req_model, upstream_model, status, latency_ms, ttft_ms, tps, error_msg, ts 
		 FROM request_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()

	type logEntry struct {
		ID            uint64  `json:"id"`
		PoolID        uint64  `json:"pool_id"`
		ProviderID    uint64  `json:"provider_id"`
		CredentialID  uint64  `json:"credential_id"`
		ClientKey     string  `json:"client_key"`
		SrcIP         string  `json:"src_ip,omitempty"`
		UserAgent     string  `json:"user_agent,omitempty"`
		IsTest        bool    `json:"is_test,omitempty"`
		Stream        bool    `json:"stream,omitempty"`
		RequestBytes  int     `json:"request_bytes,omitempty"`
		ResponseBytes int     `json:"response_bytes,omitempty"`
		InputTokens   int64   `json:"input_tokens,omitempty"`
		OutputTokens  int64   `json:"output_tokens,omitempty"`
		Facade        string  `json:"facade"`
		RequestModel  string  `json:"request_model"`
		UpstreamModel string  `json:"upstream_model"`
		Status        int     `json:"status"`
		LatencyMs     int64   `json:"latency_ms"`
		TTFTMs        int64   `json:"ttft_ms,omitempty"`
		TPS           float64 `json:"tps,omitempty"`
		Error         string  `json:"error"`
		CreatedAt     string  `json:"created_at"`
	}

	out := []logEntry{}
	for rows.Next() {
		var l logEntry
		var (
			poolID, provID, credID     sql.NullInt64
			srcIP, ua, upModel, errMsg sql.NullString
			isTest, stream             bool
			reqBytes, respBytes        sql.NullInt64
			inTok, outTok              sql.NullInt64
			status, latency            sql.NullInt64
			ttft                       sql.NullInt64
			tps                        sql.NullFloat64
			ts                         time.Time
		)
		if err := rows.Scan(&l.ID, &poolID, &provID, &credID, &l.ClientKey, &srcIP, &ua, &isTest, &stream, &reqBytes, &respBytes, &inTok, &outTok, &l.Facade, &l.RequestModel, &upModel, &status, &latency, &ttft, &tps, &errMsg, &ts); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		l.PoolID = uint64(poolID.Int64)
		l.ProviderID = uint64(provID.Int64)
		l.CredentialID = uint64(credID.Int64)
		l.SrcIP = srcIP.String
		l.UserAgent = ua.String
		l.IsTest = isTest
		l.Stream = stream
		l.RequestBytes = int(reqBytes.Int64)
		l.ResponseBytes = int(respBytes.Int64)
		l.InputTokens = inTok.Int64
		l.OutputTokens = outTok.Int64
		l.UpstreamModel = upModel.String
		l.Status = int(status.Int64)
		l.LatencyMs = latency.Int64
		l.TTFTMs = ttft.Int64
		l.TPS = tps.Float64
		l.Error = errMsg.String
		l.CreatedAt = ts.Format(time.RFC3339)
		out = append(out, l)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total,
		"page":  page,
		"limit": limit,
		"items": out,
	})
}

func (h *Handler) getStats(w http.ResponseWriter, r *http.Request) {
	// Daily stats
	dailyRows, err := h.db.QueryContext(r.Context(),
		`SELECT 
			DATE(ts) as day, 
			COUNT(*) as total,
			SUM(CASE WHEN status >= 200 AND status < 400 THEN 1 ELSE 0 END) as success,
			IFNULL(SUM(input_tokens), 0) as input_tokens,
			IFNULL(SUM(output_tokens), 0) as output_tokens,
			IFNULL(AVG(latency_ms), 0) as avg_latency,
			IFNULL(AVG(ttft_ms), 0) as avg_ttft,
			IFNULL(AVG(tps), 0) as avg_tps
		 FROM request_logs 
		 GROUP BY day 
		 ORDER BY day DESC 
		 LIMIT 30`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer dailyRows.Close()

	type dayStat struct {
		Day          string  `json:"day"`
		Total        int64   `json:"total"`
		Success      int64   `json:"success"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
		AvgLatencyMs float64 `json:"avg_latency"`
		AvgTTFTMs    float64 `json:"avg_ttft"`
		AvgTPS       float64 `json:"avg_tps"`
	}
	days := []dayStat{}
	for dailyRows.Next() {
		var s dayStat
		if err := dailyRows.Scan(&s.Day, &s.Total, &s.Success, &s.InputTokens, &s.OutputTokens, &s.AvgLatencyMs, &s.AvgTTFTMs, &s.AvgTPS); err != nil {
			continue
		}
		days = append(days, s)
	}

	// Hourly stats for last 24h
	hourlyRows, err := h.db.QueryContext(r.Context(),
		`SELECT 
			DATE_FORMAT(ts, '%Y-%m-%d %H:00:00') as hour, 
			COUNT(*) as total,
			IFNULL(SUM(input_tokens), 0) as input_tokens,
			IFNULL(SUM(output_tokens), 0) as output_tokens
		 FROM request_logs 
		 WHERE ts > DATE_SUB(NOW(), INTERVAL 24 HOUR)
		 GROUP BY hour 
		 ORDER BY hour DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer hourlyRows.Close()

	type hourStat struct {
		Hour         string `json:"hour"`
		Total        int64  `json:"total"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
	}
	hours := []hourStat{}
	for hourlyRows.Next() {
		var s hourStat
		if err := hourlyRows.Scan(&s.Hour, &s.Total, &s.InputTokens, &s.OutputTokens); err != nil {
			continue
		}
		hours = append(hours, s)
	}

	// Overall summary for today
	var today dayStat
	err = h.db.QueryRowContext(r.Context(),
		`SELECT 
			COUNT(*),
			SUM(CASE WHEN status >= 200 AND status < 400 THEN 1 ELSE 0 END),
			IFNULL(SUM(input_tokens), 0),
			IFNULL(SUM(output_tokens), 0),
			IFNULL(AVG(latency_ms), 0),
			IFNULL(AVG(ttft_ms), 0),
			IFNULL(AVG(tps), 0)
		 FROM request_logs 
		 WHERE DATE(ts) = DATE(NOW())`).Scan(&today.Total, &today.Success, &today.InputTokens, &today.OutputTokens, &today.AvgLatencyMs, &today.AvgTTFTMs, &today.AvgTPS)
	if err != nil {
		// ignore
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"today": today,
		"days":  days,
		"hours": hours,
	})
}

func (h *Handler) getDetailedStats(w http.ResponseWriter, r *http.Request) {
	// Model distribution (last 7 days)
	modelRows, err := h.db.QueryContext(r.Context(),
		`SELECT req_model, COUNT(*) as count, SUM(input_tokens + output_tokens) as tokens
		 FROM request_logs 
		 WHERE ts > DATE_SUB(NOW(), INTERVAL 7 DAY)
		 GROUP BY req_model ORDER BY count DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer modelRows.Close()

	models := []map[string]any{}
	for modelRows.Next() {
		var name string
		var count, tokens int64
		if err := modelRows.Scan(&name, &count, &tokens); err == nil {
			models = append(models, map[string]any{"name": name, "count": count, "tokens": tokens})
		}
	}

	// Provider distribution (last 7 days)
	provRows, err := h.db.QueryContext(r.Context(),
		`SELECT p.display_name, COUNT(l.id) as count, SUM(l.input_tokens + l.output_tokens) as tokens
		 FROM request_logs l
		 JOIN providers p ON l.provider_id = p.id
		 WHERE l.ts > DATE_SUB(NOW(), INTERVAL 7 DAY)
		 GROUP BY p.display_name ORDER BY count DESC`)
	if err != nil {
		// fallback if join fails or table empty
	} else {
		defer provRows.Close()
	}

	providers := []map[string]any{}
	if provRows != nil {
		for provRows.Next() {
			var name string
			var count, tokens int64
			if err := provRows.Scan(&name, &count, &tokens); err == nil {
				providers = append(providers, map[string]any{"name": name, "count": count, "tokens": tokens})
			}
		}
	}

	// Error distribution (last 7 days)
	errRows, err := h.db.QueryContext(r.Context(),
		`SELECT status, COUNT(*) as count
		 FROM request_logs 
		 WHERE ts > DATE_SUB(NOW(), INTERVAL 7 DAY) AND status >= 400
		 GROUP BY status ORDER BY count DESC`)
	if err != nil { /* ignore */
	} else {
		defer errRows.Close()
	}

	errors := []map[string]any{}
	if errRows != nil {
		for errRows.Next() {
			var status int
			var count int64
			if err := errRows.Scan(&status, &count); err == nil {
				errors = append(errors, map[string]any{"status": status, "count": count})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":    models,
		"providers": providers,
		"errors":    errors,
	})
}

type providerDTO struct {
	ID                uint64          `json:"id"`
	Type              string          `json:"type"`
	DisplayName       string          `json:"display_name,omitempty"`
	GroupName         string          `json:"group_name,omitempty"`
	BaseURL           string          `json:"base_url"`
	DefaultHeadersRaw json.RawMessage `json:"default_headers_json,omitempty"`
	ModelMapRaw       json.RawMessage `json:"model_map_json,omitempty"`
	Notes             string          `json:"notes,omitempty"`
	ModelsJSON        json.RawMessage `json:"models_json,omitempty"`
	ModelsRefreshedAt string          `json:"models_refreshed_at,omitempty"`
	Enabled           bool            `json:"enabled"`
}

func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, type, display_name, group_name, base_url, default_headers_json, model_map_json, notes, models_json, models_refreshed_at, enabled FROM providers ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []providerDTO{}
	for rows.Next() {
		var p providerDTO
		var (
			displayName sql.NullString
			groupName   sql.NullString
			hdrs        []byte
			mm          []byte
			notes       sql.NullString
			modelsJSON  []byte
			refreshedAt sql.NullTime
		)
		if err := rows.Scan(&p.ID, &p.Type, &displayName, &groupName, &p.BaseURL, &hdrs, &mm, &notes, &modelsJSON, &refreshedAt, &p.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if displayName.Valid {
			p.DisplayName = displayName.String
		}
		if groupName.Valid {
			p.GroupName = groupName.String
		}
		p.DefaultHeadersRaw = hdrs
		p.ModelMapRaw = mm
		if notes.Valid {
			p.Notes = notes.String
		}
		p.ModelsJSON = modelsJSON
		if refreshedAt.Valid {
			p.ModelsRefreshedAt = refreshedAt.Time.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createProvider(w http.ResponseWriter, r *http.Request) {
	var in providerDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Type) == "" || strings.TrimSpace(in.BaseURL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type and base_url are required"})
		return
	}
	if in.DefaultHeadersRaw == nil {
		in.DefaultHeadersRaw = []byte("null")
	}
	if in.ModelMapRaw == nil {
		in.ModelMapRaw = []byte("null")
	}
	var (
		displayName any = nil
		groupName   any = nil
		notes       any = nil
		modelsJSON  any = nil
	)
	if strings.TrimSpace(in.DisplayName) != "" {
		displayName = strings.TrimSpace(in.DisplayName)
	}
	if strings.TrimSpace(in.GroupName) != "" {
		groupName = strings.TrimSpace(in.GroupName)
	}
	if strings.TrimSpace(in.Notes) != "" {
		notes = strings.TrimSpace(in.Notes)
	}
	if len(in.ModelsJSON) > 0 && string(in.ModelsJSON) != "null" {
		modelsJSON = in.ModelsJSON
	}

	res, err := h.db.ExecContext(r.Context(), `INSERT INTO providers(type, display_name, group_name, base_url, default_headers_json, model_map_json, notes, models_json, enabled) VALUES (?,?,?,?,?,?,?,?,?)`,
		in.Type, displayName, groupName, in.BaseURL, in.DefaultHeadersRaw, in.ModelMapRaw, notes, modelsJSON, in.Enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	in.ID = uint64(id)
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) updateProvider(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in providerDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.DefaultHeadersRaw == nil {
		in.DefaultHeadersRaw = []byte("null")
	}
	if in.ModelMapRaw == nil {
		in.ModelMapRaw = []byte("null")
	}
	var (
		displayName any = nil
		groupName   any = nil
		notes       any = nil
		modelsJSON  any = nil
	)
	if strings.TrimSpace(in.DisplayName) != "" {
		displayName = strings.TrimSpace(in.DisplayName)
	}
	if strings.TrimSpace(in.GroupName) != "" {
		groupName = strings.TrimSpace(in.GroupName)
	}
	if strings.TrimSpace(in.Notes) != "" {
		notes = strings.TrimSpace(in.Notes)
	}
	if len(in.ModelsJSON) > 0 && string(in.ModelsJSON) != "null" {
		modelsJSON = in.ModelsJSON
	}

	_, err = h.db.ExecContext(r.Context(), `UPDATE providers SET type=?, display_name=?, group_name=?, base_url=?, default_headers_json=?, model_map_json=?, notes=?, models_json=?, enabled=? WHERE id=?`,
		in.Type, displayName, groupName, in.BaseURL, in.DefaultHeadersRaw, in.ModelMapRaw, notes, modelsJSON, in.Enabled, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	_, err = h.db.ExecContext(r.Context(), `DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type credentialDTO struct {
	ID               uint64 `json:"id"`
	ProviderID       uint64 `json:"provider_id"`
	Name             string `json:"name"`
	APIKey           string `json:"api_key,omitempty"`
	KeyLast4         string `json:"key_last4,omitempty"`
	Weight           int    `json:"weight"`
	ConcurrencyLimit *int   `json:"concurrency_limit,omitempty"`
	LastTestAt       string `json:"last_test_at,omitempty"`
	LastTestOK       *bool  `json:"last_test_ok,omitempty"`
	LastTestStatus   *int   `json:"last_test_status,omitempty"`
	LastTestLatency  *int64 `json:"last_test_latency_ms,omitempty"`
	LastTestError    string `json:"last_test_error,omitempty"`
	LastTestModel    string `json:"last_test_model,omitempty"`
	Enabled          bool   `json:"enabled"`
}

func (h *Handler) listCredentials(w http.ResponseWriter, r *http.Request) {
	q := `SELECT id, provider_id, name, key_last4, weight, concurrency_limit, last_test_at, last_test_ok, last_test_status, last_test_latency_ms, last_test_error, last_test_model, enabled FROM credentials`
	var args []any
	if v := strings.TrimSpace(r.URL.Query().Get("provider_id")); v != "" {
		pid, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid provider_id"})
			return
		}
		q += ` WHERE provider_id = ?`
		args = append(args, pid)
	}
	q += ` ORDER BY id DESC`
	rows, err := h.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []credentialDTO{}
	for rows.Next() {
		var c credentialDTO
		var (
			conc      sql.NullInt64
			testAt    sql.NullTime
			testOK    sql.NullBool
			testSt    sql.NullInt64
			testLat   sql.NullInt64
			testErr   sql.NullString
			testModel sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.ProviderID, &c.Name, &c.KeyLast4, &c.Weight, &conc, &testAt, &testOK, &testSt, &testLat, &testErr, &testModel, &c.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if conc.Valid {
			v := int(conc.Int64)
			c.ConcurrencyLimit = &v
		}
		if testAt.Valid {
			c.LastTestAt = testAt.Time.UTC().Format(time.RFC3339Nano)
		}
		if testOK.Valid {
			v := testOK.Bool
			c.LastTestOK = &v
		}
		if testSt.Valid {
			v := int(testSt.Int64)
			c.LastTestStatus = &v
		}
		if testLat.Valid {
			v := testLat.Int64
			c.LastTestLatency = &v
		}
		if testErr.Valid {
			c.LastTestError = testErr.String
		}
		if testModel.Valid {
			c.LastTestModel = testModel.String
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createCredential(w http.ResponseWriter, r *http.Request) {
	var in credentialDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.ProviderID == 0 || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.APIKey) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider_id, name, api_key are required"})
		return
	}
	blob, err := h.cipher.Encrypt([]byte(in.APIKey))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	last4 := last4(in.APIKey)
	weight := in.Weight
	if weight <= 0 {
		weight = 1
	}
	var conc any = nil
	if in.ConcurrencyLimit != nil {
		conc = *in.ConcurrencyLimit
	}
	res, err := h.db.ExecContext(r.Context(),
		`INSERT INTO credentials(provider_id, name, api_key_ciphertext, key_last4, weight, concurrency_limit, enabled) VALUES (?,?,?,?,?,?,?)`,
		in.ProviderID, in.Name, blob, last4, weight, conc, in.Enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	in.ID = uint64(id)
	in.APIKey = ""
	in.KeyLast4 = last4
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) createCredentialsBulk(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name             string `json:"name"`
		APIKey           string `json:"api_key,omitempty"`
		Weight           int    `json:"weight"`
		ConcurrencyLimit *int   `json:"concurrency_limit,omitempty"`
		Enabled          bool   `json:"enabled"`
	}
	var in struct {
		ProviderID uint64 `json:"provider_id"`
		Items      []item `json:"items"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.ProviderID == 0 || len(in.Items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider_id and items are required"})
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(r.Context(),
		`INSERT INTO credentials(provider_id, name, api_key_ciphertext, key_last4, weight, concurrency_limit, enabled) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer stmt.Close()

	for _, it := range in.Items {
		if strings.TrimSpace(it.Name) == "" || strings.TrimSpace(it.APIKey) == "" {
			continue
		}
		blob, _ := h.cipher.Encrypt([]byte(it.APIKey))
		last4v := last4(it.APIKey)
		weight := it.Weight
		if weight <= 0 {
			weight = 1
		}
		var conc any = nil
		if it.ConcurrencyLimit != nil {
			conc = *it.ConcurrencyLimit
		}
		_, _ = stmt.ExecContext(r.Context(), in.ProviderID, it.Name, blob, last4v, weight, conc, it.Enabled)
	}
	_ = tx.Commit()
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (h *Handler) updateCredential(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in credentialDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	var blob []byte
	var last4v string
	if strings.TrimSpace(in.APIKey) != "" {
		blob, _ = h.cipher.Encrypt([]byte(in.APIKey))
		last4v = last4(in.APIKey)
	} else {
		_ = h.db.QueryRowContext(r.Context(), `SELECT api_key_ciphertext, key_last4 FROM credentials WHERE id=?`, id).Scan(&blob, &last4v)
	}

	weight := in.Weight
	if weight <= 0 {
		weight = 1
	}
	var conc any = nil
	if in.ConcurrencyLimit != nil {
		conc = *in.ConcurrencyLimit
	}

	_, err = h.db.ExecContext(r.Context(),
		`UPDATE credentials SET provider_id=?, name=?, api_key_ciphertext=?, key_last4=?, weight=?, concurrency_limit=?, enabled=? WHERE id=?`,
		in.ProviderID, in.Name, blob, last4v, weight, conc, in.Enabled, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id
	in.APIKey = ""
	in.KeyLast4 = last4v
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) deleteCredential(w http.ResponseWriter, r *http.Request) {
	id, _ := parseID(chi.URLParam(r, "id"))
	_, _ = h.db.ExecContext(r.Context(), `DELETE FROM credentials WHERE id=?`, id)
	w.WriteHeader(http.StatusNoContent)
}

type poolDTO struct {
	ID            uint64          `json:"id"`
	Name          string          `json:"name"`
	ClientKey     string          `json:"client_key"`
	Strategy      string          `json:"strategy"`
	TiersJSON     json.RawMessage `json:"tiers_json,omitempty"`
	CredentialIDs []uint64        `json:"credential_ids"`
	ModelMapJSON  json.RawMessage `json:"model_map_json,omitempty"`
	Enabled       bool            `json:"enabled"`
}

func (h *Handler) listPools(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, name, client_key, strategy, tiers_json, credential_ids_json, model_map_json, enabled FROM pools ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []poolDTO{}
	for rows.Next() {
		var p poolDTO
		var tiersJSON, idsJSON, mmJSON []byte
		if err := rows.Scan(&p.ID, &p.Name, &p.ClientKey, &p.Strategy, &tiersJSON, &idsJSON, &mmJSON, &p.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.TiersJSON = tiersJSON
		_ = json.Unmarshal(idsJSON, &p.CredentialIDs)
		if p.CredentialIDs == nil {
			p.CredentialIDs = []uint64{}
		}
		p.ModelMapJSON = mmJSON
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createPool(w http.ResponseWriter, r *http.Request) {
	var in poolDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.ClientKey) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and client_key are required"})
		return
	}
	if in.CredentialIDs == nil {
		in.CredentialIDs = []uint64{}
	}
	idsJSON, _ := json.Marshal(in.CredentialIDs)
	if in.ModelMapJSON == nil {
		in.ModelMapJSON = []byte("null")
	}
	if in.TiersJSON == nil {
		in.TiersJSON = []byte("null")
	}
	res, err := h.db.ExecContext(r.Context(),
		`INSERT INTO pools(name, client_key, strategy, tiers_json, credential_ids_json, model_map_json, enabled) VALUES (?,?,?,?,?,?,?)`,
		in.Name, in.ClientKey, in.Strategy, in.TiersJSON, idsJSON, in.ModelMapJSON, in.Enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	in.ID = uint64(id)
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) updatePool(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in poolDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.CredentialIDs == nil {
		in.CredentialIDs = []uint64{}
	}
	idsJSON, _ := json.Marshal(in.CredentialIDs)
	if in.ModelMapJSON == nil {
		in.ModelMapJSON = []byte("null")
	}
	if in.TiersJSON == nil {
		in.TiersJSON = []byte("null")
	}
	_, err = h.db.ExecContext(r.Context(),
		`UPDATE pools SET name=?, client_key=?, strategy=?, tiers_json=?, credential_ids_json=?, model_map_json=?, enabled=? WHERE id=?`,
		in.Name, in.ClientKey, in.Strategy, in.TiersJSON, idsJSON, in.ModelMapJSON, in.Enabled, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) deletePool(w http.ResponseWriter, r *http.Request) {
	id, _ := parseID(chi.URLParam(r, "id"))
	_, _ = h.db.ExecContext(r.Context(), `DELETE FROM pools WHERE id=?`, id)
	w.WriteHeader(http.StatusNoContent)
}

func readJSON(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

func writeJSON(w http.ResponseWriter, status int, val any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(val)
}

func parseID(s string) (uint64, error) {
	return strconv.ParseUint(s, 10, 64)
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
