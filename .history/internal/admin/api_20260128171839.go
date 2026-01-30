package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) logsStream(w http.ResponseWriter, r *http.Request) {
	if h.bus == nil {
		http.Error(w, "log stream disabled", http.StatusNotImplemented)
		return
	}
	h.bus.ServeSSE(w, r)
}

type providerDTO struct {
	ID                uint64          `json:"id"`
	Type              string          `json:"type"`
	BaseURL           string          `json:"base_url"`
	DefaultHeadersRaw json.RawMessage `json:"default_headers_json,omitempty"`
	Enabled           bool            `json:"enabled"`
}

func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, type, base_url, default_headers_json, enabled FROM providers ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []providerDTO{}
	for rows.Next() {
		var p providerDTO
		var hdrs []byte
		if err := rows.Scan(&p.ID, &p.Type, &p.BaseURL, &hdrs, &p.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.DefaultHeadersRaw = hdrs
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
	res, err := h.db.ExecContext(r.Context(), `INSERT INTO providers(type, base_url, default_headers_json, enabled) VALUES (?,?,?,?)`,
		in.Type, in.BaseURL, in.DefaultHeadersRaw, in.Enabled)
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
	_, err = h.db.ExecContext(r.Context(), `UPDATE providers SET type=?, base_url=?, default_headers_json=?, enabled=? WHERE id=?`,
		in.Type, in.BaseURL, in.DefaultHeadersRaw, in.Enabled, id)
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
	Enabled          bool   `json:"enabled"`
}

func (h *Handler) listCredentials(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, provider_id, name, key_last4, weight, concurrency_limit, enabled FROM credentials ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []credentialDTO{}
	for rows.Next() {
		var c credentialDTO
		var conc sql.NullInt64
		if err := rows.Scan(&c.ID, &c.ProviderID, &c.Name, &c.KeyLast4, &c.Weight, &conc, &c.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if conc.Valid {
			v := int(conc.Int64)
			c.ConcurrencyLimit = &v
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

	var (
		blob   []byte
		last4v string
	)
	if strings.TrimSpace(in.APIKey) != "" {
		blob, err = h.cipher.Encrypt([]byte(in.APIKey))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		last4v = last4(in.APIKey)
	} else {
		if err := h.db.QueryRowContext(r.Context(), `SELECT api_key_ciphertext, key_last4 FROM credentials WHERE id=?`, id).Scan(&blob, &last4v); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
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
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	_, err = h.db.ExecContext(r.Context(), `DELETE FROM credentials WHERE id=?`, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type channelDTO struct {
	ID               uint64          `json:"id"`
	CredentialID     uint64          `json:"credential_id"`
	Name             string          `json:"name"`
	TagsJSON         json.RawMessage `json:"tags_json,omitempty"`
	ModelMapJSON     json.RawMessage `json:"model_map_json,omitempty"`
	ExtraHeadersJSON json.RawMessage `json:"extra_headers_json,omitempty"`
	Enabled          bool            `json:"enabled"`
}

func (h *Handler) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, credential_id, name, tags_json, model_map_json, extra_headers_json, enabled FROM channels ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []channelDTO{}
	for rows.Next() {
		var c channelDTO
		var tags, mm, hdr []byte
		if err := rows.Scan(&c.ID, &c.CredentialID, &c.Name, &tags, &mm, &hdr, &c.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		c.TagsJSON = tags
		c.ModelMapJSON = mm
		c.ExtraHeadersJSON = hdr
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	var in channelDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.CredentialID == 0 || strings.TrimSpace(in.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "credential_id and name are required"})
		return
	}
	if in.TagsJSON == nil {
		in.TagsJSON = []byte("null")
	}
	if in.ModelMapJSON == nil {
		in.ModelMapJSON = []byte("null")
	}
	if in.ExtraHeadersJSON == nil {
		in.ExtraHeadersJSON = []byte("null")
	}
	res, err := h.db.ExecContext(r.Context(),
		`INSERT INTO channels(credential_id, name, tags_json, model_map_json, extra_headers_json, enabled) VALUES (?,?,?,?,?,?)`,
		in.CredentialID, in.Name, in.TagsJSON, in.ModelMapJSON, in.ExtraHeadersJSON, in.Enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	in.ID = uint64(id)
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in channelDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.TagsJSON == nil {
		in.TagsJSON = []byte("null")
	}
	if in.ModelMapJSON == nil {
		in.ModelMapJSON = []byte("null")
	}
	if in.ExtraHeadersJSON == nil {
		in.ExtraHeadersJSON = []byte("null")
	}
	_, err = h.db.ExecContext(r.Context(),
		`UPDATE channels SET credential_id=?, name=?, tags_json=?, model_map_json=?, extra_headers_json=?, enabled=? WHERE id=?`,
		in.CredentialID, in.Name, in.TagsJSON, in.ModelMapJSON, in.ExtraHeadersJSON, in.Enabled, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	_, err = h.db.ExecContext(r.Context(), `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type poolDTO struct {
	ID         uint64   `json:"id"`
	Name       string   `json:"name"`
	Strategy   string   `json:"strategy"`
	ChannelIDs []uint64 `json:"channel_ids"`
	Enabled    bool     `json:"enabled"`
}

func (h *Handler) listPools(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, name, strategy, channel_ids_json, enabled FROM pools ORDER BY id DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	var out []poolDTO
	for rows.Next() {
		var p poolDTO
		var idsJSON []byte
		if err := rows.Scan(&p.ID, &p.Name, &p.Strategy, &idsJSON, &p.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		_ = json.Unmarshal(idsJSON, &p.ChannelIDs)
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
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Strategy) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and strategy are required"})
		return
	}
	idsJSON, _ := json.Marshal(in.ChannelIDs)
	res, err := h.db.ExecContext(r.Context(), `INSERT INTO pools(name, strategy, channel_ids_json, enabled) VALUES (?,?,?,?)`,
		in.Name, in.Strategy, idsJSON, in.Enabled)
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
	idsJSON, _ := json.Marshal(in.ChannelIDs)
	_, err = h.db.ExecContext(r.Context(), `UPDATE pools SET name=?, strategy=?, channel_ids_json=?, enabled=? WHERE id=?`,
		in.Name, in.Strategy, idsJSON, in.Enabled, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) deletePool(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	_, err = h.db.ExecContext(r.Context(), `DELETE FROM pools WHERE id=?`, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type ruleDTO struct {
	ID             uint64          `json:"id"`
	Priority       int             `json:"priority"`
	MatchJSON      json.RawMessage `json:"match_json"`
	PoolID         uint64          `json:"pool_id"`
	FallbackPoolID *uint64         `json:"fallback_pool_id,omitempty"`
	Enabled        bool            `json:"enabled"`
}

func (h *Handler) listRules(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id, priority, match_json, pool_id, fallback_pool_id, enabled FROM routing_rules ORDER BY priority ASC, id ASC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	var out []ruleDTO
	for rows.Next() {
		var rr ruleDTO
		var match []byte
		var fb sql.NullInt64
		if err := rows.Scan(&rr.ID, &rr.Priority, &match, &rr.PoolID, &fb, &rr.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		rr.MatchJSON = match
		if fb.Valid {
			v := uint64(fb.Int64)
			rr.FallbackPoolID = &v
		}
		out = append(out, rr)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createRule(w http.ResponseWriter, r *http.Request) {
	var in ruleDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(in.MatchJSON) == 0 {
		in.MatchJSON = []byte(`{}`)
	}
	var fb any = nil
	if in.FallbackPoolID != nil {
		fb = *in.FallbackPoolID
	}
	res, err := h.db.ExecContext(r.Context(),
		`INSERT INTO routing_rules(priority, match_json, pool_id, fallback_pool_id, enabled) VALUES (?,?,?,?,?)`,
		in.Priority, in.MatchJSON, in.PoolID, fb, in.Enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	in.ID = uint64(id)
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) updateRule(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var in ruleDTO
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(in.MatchJSON) == 0 {
		in.MatchJSON = []byte(`{}`)
	}
	var fb any = nil
	if in.FallbackPoolID != nil {
		fb = *in.FallbackPoolID
	}
	_, err = h.db.ExecContext(r.Context(),
		`UPDATE routing_rules SET priority=?, match_json=?, pool_id=?, fallback_pool_id=?, enabled=? WHERE id=?`,
		in.Priority, in.MatchJSON, in.PoolID, fb, in.Enabled, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	in.ID = id
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) deleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	_, err = h.db.ExecContext(r.Context(), `DELETE FROM routing_rules WHERE id=?`, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseID(raw string) (uint64, error) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		return 0, err
	}
	return v, nil
}

func readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func last4(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
