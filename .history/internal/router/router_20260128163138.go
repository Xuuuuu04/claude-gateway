package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"claude-gateway/internal/crypto"
	"claude-gateway/internal/metrics"
)

var ErrNotConfigured = errors.New("gateway not configured")

type Router struct {
	db     *sql.DB
	m      *metrics.Metrics
	cipher *crypto.AESGCM

	cacheTTL time.Duration

	cacheMu sync.RWMutex
	cache   loadedConfig

	poolMu     sync.Mutex
	poolStates map[uint64]*poolState

	chState sync.Map
}

func New(db *sql.DB, m *metrics.Metrics, cipher *crypto.AESGCM) *Router {
	return &Router{
		db:         db,
		m:          m,
		cipher:     cipher,
		cacheTTL:   5 * time.Second,
		poolStates: make(map[uint64]*poolState),
	}
}

type RoutedUpstream struct {
	PoolID       uint64
	ChannelID    uint64
	ProviderType string
	BaseURL      string
	APIKey       []byte
	Model        string
	Headers      map[string]string
	Timeout      time.Duration
}

func (r *Router) PickUpstream(ctx context.Context, facade string, model string) (RoutedUpstream, error) {
	return r.pickUpstream(ctx, facade, model, nil)
}

func (r *Router) PickUpstreamExclude(ctx context.Context, facade string, model string, exclude map[uint64]bool) (RoutedUpstream, error) {
	return r.pickUpstream(ctx, facade, model, exclude)
}

func (r *Router) EndRequest(channelID uint64, ok bool, status int, latency time.Duration) {
	if channelID == 0 {
		return
	}
	v, _ := r.chState.LoadOrStore(channelID, &channelState{})
	st := v.(*channelState)
	atomic.AddInt64(&st.inflight, -1)

	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()

	st.lastLatency = latency
	st.lastSeen = now

	if ok && status >= 200 && status < 500 {
		st.failures = 0
		st.openUntil = time.Time{}
		return
	}

	st.failures++
	st.lastErrorAt = now
	if st.failures >= 3 {
		st.openUntil = now.Add(30 * time.Second)
	}
}

func (r *Router) pickUpstream(ctx context.Context, facade string, model string, exclude map[uint64]bool) (RoutedUpstream, error) {
	cfg, err := r.getConfig(ctx)
	if err != nil {
		return RoutedUpstream{}, err
	}
	if len(cfg.pools) == 0 || len(cfg.channels) == 0 || len(cfg.credentials) == 0 || len(cfg.providers) == 0 {
		return RoutedUpstream{}, ErrNotConfigured
	}

	poolID, fallbackPoolID := matchRule(cfg.rules, facade, model)
	if poolID == 0 {
		poolID = cfg.defaultPoolID
	}
	if poolID == 0 {
		return RoutedUpstream{}, ErrNotConfigured
	}

	tryPools := []uint64{poolID}
	if fallbackPoolID != 0 && fallbackPoolID != poolID {
		tryPools = append(tryPools, fallbackPoolID)
	}

	for _, pid := range tryPools {
		pool, ok := cfg.pools[pid]
		if !ok {
			continue
		}

		chID, ok := r.pickChannelFromPool(cfg, pool, exclude)
		if !ok {
			continue
		}

		ch := cfg.channels[chID]
		cred := cfg.credentials[ch.CredentialID]
		prov := cfg.providers[cred.ProviderID]

		apiKey, err := r.cipher.Decrypt(cred.APIKeyCiphertext)
		if err != nil {
			return RoutedUpstream{}, fmt.Errorf("decrypt credential: %w", err)
		}

		headers := make(map[string]string, 8)
		for k, v := range prov.DefaultHeaders {
			headers[k] = v
		}
		for k, v := range ch.ExtraHeaders {
			headers[k] = v
		}

		upModel := model
		if mapped, ok := ch.ModelMap[model]; ok && mapped != "" {
			upModel = mapped
		}

		r.startRequest(chID)

		return RoutedUpstream{
			PoolID:       pid,
			ChannelID:    chID,
			ProviderType: prov.Type,
			BaseURL:      prov.BaseURL,
			APIKey:       apiKey,
			Model:        upModel,
			Headers:      headers,
			Timeout:      time.Duration(ch.TimeoutMs) * time.Millisecond,
		}, nil
	}

	return RoutedUpstream{}, ErrNotConfigured
}

func (r *Router) startRequest(channelID uint64) {
	v, _ := r.chState.LoadOrStore(channelID, &channelState{})
	st := v.(*channelState)
	atomic.AddInt64(&st.inflight, 1)
}

func (r *Router) pickChannelFromPool(cfg loadedConfig, pool poolRow, exclude map[uint64]bool) (uint64, bool) {
	now := time.Now()

	isAvailable := func(chID uint64) bool {
		if exclude != nil && exclude[chID] {
			return false
		}
		ch, ok := cfg.channels[chID]
		if !ok || !ch.Enabled {
			return false
		}
		cred, ok := cfg.credentials[ch.CredentialID]
		if !ok || !cred.Enabled {
			return false
		}
		if r.isChannelOpen(chID, now) {
			return false
		}
		if cred.ConcurrencyLimit > 0 && r.getInflight(chID) >= int64(cred.ConcurrencyLimit) {
			return false
		}
		return true
	}

	switch pool.Strategy {
	case "least_inflight":
		var (
			bestID       uint64
			bestInflight int64
			found        bool
		)
		for _, chID := range pool.ChannelIDs {
			if !isAvailable(chID) {
				continue
			}
			in := r.getInflight(chID)
			if !found || in < bestInflight {
				bestID = chID
				bestInflight = in
				found = true
			}
		}
		return bestID, found
	case "priority_failover":
		for _, chID := range pool.ChannelIDs {
			if isAvailable(chID) {
				return chID, true
			}
		}
		return 0, false
	default:
		expanded := pool.ExpandedChannelIDs
		if len(expanded) == 0 {
			expanded = pool.ChannelIDs
		}
		if len(expanded) == 0 {
			return 0, false
		}
		state := r.getPoolState(pool.ID)
		for i := 0; i < len(expanded); i++ {
			idx := int(atomic.AddUint64(&state.counter, 1)-1) % len(expanded)
			chID := expanded[idx]
			if isAvailable(chID) {
				return chID, true
			}
		}
		return 0, false
	}
}

func (r *Router) getInflight(channelID uint64) int64 {
	v, ok := r.chState.Load(channelID)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(&v.(*channelState).inflight)
}

func (r *Router) isChannelOpen(channelID uint64, now time.Time) bool {
	v, ok := r.chState.Load(channelID)
	if !ok {
		return false
	}
	st := v.(*channelState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return !st.openUntil.IsZero() && now.Before(st.openUntil)
}

func (r *Router) getPoolState(poolID uint64) *poolState {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	if st, ok := r.poolStates[poolID]; ok {
		return st
	}
	st := &poolState{}
	r.poolStates[poolID] = st
	return st
}

func (r *Router) getConfig(ctx context.Context) (loadedConfig, error) {
	now := time.Now()
	r.cacheMu.RLock()
	if !r.cache.loadedAt.IsZero() && now.Sub(r.cache.loadedAt) <= r.cacheTTL {
		c := r.cache
		r.cacheMu.RUnlock()
		return c, nil
	}
	r.cacheMu.RUnlock()

	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if !r.cache.loadedAt.IsZero() && now.Sub(r.cache.loadedAt) <= r.cacheTTL {
		return r.cache, nil
	}

	c, err := loadConfig(ctx, r.db)
	if err != nil {
		return loadedConfig{}, err
	}
	r.cache = c
	return c, nil
}

type poolState struct {
	counter uint64
}

type channelState struct {
	inflight int64

	mu          sync.Mutex
	failures    int
	openUntil   time.Time
	lastSeen    time.Time
	lastErrorAt time.Time
	lastLatency time.Duration
}

type loadedConfig struct {
	loadedAt time.Time

	providers   map[uint64]providerRow
	credentials map[uint64]credentialRow
	channels    map[uint64]channelRow
	pools       map[uint64]poolRow
	rules       []ruleRow

	defaultPoolID uint64
}

type providerRow struct {
	ID             uint64
	Type           string
	BaseURL        string
	DefaultHeaders map[string]string
}

type credentialRow struct {
	ID               uint64
	ProviderID       uint64
	APIKeyCiphertext []byte
	Weight           int
	ConcurrencyLimit int
	Enabled          bool
}

type channelRow struct {
	ID           uint64
	CredentialID uint64
	Enabled      bool
	TimeoutMs    int
	ModelMap     map[string]string
	ExtraHeaders map[string]string
}

type poolRow struct {
	ID                 uint64
	Name               string
	Strategy           string
	ChannelIDs         []uint64
	ExpandedChannelIDs []uint64
	Enabled            bool
}

type ruleRow struct {
	Priority       int
	Facade         string
	Model          string
	ModelRegex     *regexp.Regexp
	PoolID         uint64
	FallbackPoolID uint64
	Enabled        bool
}

func loadConfig(ctx context.Context, db *sql.DB) (loadedConfig, error) {
	cfg := loadedConfig{
		loadedAt:    time.Now(),
		providers:   map[uint64]providerRow{},
		credentials: map[uint64]credentialRow{},
		channels:    map[uint64]channelRow{},
		pools:       map[uint64]poolRow{},
		rules:       []ruleRow{},
	}

	if err := loadProviders(ctx, db, cfg.providers); err != nil {
		return loadedConfig{}, err
	}
	if err := loadCredentials(ctx, db, cfg.credentials); err != nil {
		return loadedConfig{}, err
	}
	if err := loadChannels(ctx, db, cfg.channels); err != nil {
		return loadedConfig{}, err
	}
	if err := loadPools(ctx, db, cfg.pools); err != nil {
		return loadedConfig{}, err
	}
	if err := loadRules(ctx, db, &cfg.rules); err != nil {
		return loadedConfig{}, err
	}

	cfg.defaultPoolID = pickDefaultPool(cfg.pools)
	expandPoolWeights(cfg.pools, cfg.channels, cfg.credentials)
	return cfg, nil
}

func loadProviders(ctx context.Context, db *sql.DB, out map[uint64]providerRow) error {
	rows, err := db.QueryContext(ctx, `SELECT id, type, base_url, default_headers_json FROM providers WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id       uint64
			typ      string
			baseURL  string
			hdrsJSON []byte
		)
		if err := rows.Scan(&id, &typ, &baseURL, &hdrsJSON); err != nil {
			return err
		}
		out[id] = providerRow{
			ID:             id,
			Type:           typ,
			BaseURL:        baseURL,
			DefaultHeaders: parseStringMapJSON(hdrsJSON),
		}
	}
	return rows.Err()
}

func loadCredentials(ctx context.Context, db *sql.DB, out map[uint64]credentialRow) error {
	rows, err := db.QueryContext(ctx, `SELECT id, provider_id, api_key_ciphertext, weight, concurrency_limit, enabled FROM credentials WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id         uint64
			providerID uint64
			blob       []byte
			weight     int
			conc       sql.NullInt64
			enabled    bool
		)
		if err := rows.Scan(&id, &providerID, &blob, &weight, &conc, &enabled); err != nil {
			return err
		}
		if weight <= 0 {
			weight = 1
		}
		out[id] = credentialRow{
			ID:               id,
			ProviderID:       providerID,
			APIKeyCiphertext: blob,
			Weight:           weight,
			ConcurrencyLimit: int(conc.Int64),
			Enabled:          enabled,
		}
	}
	return rows.Err()
}

func loadChannels(ctx context.Context, db *sql.DB, out map[uint64]channelRow) error {
	rows, err := db.QueryContext(ctx, `SELECT id, credential_id, enabled, model_map_json, extra_headers_json FROM channels WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id        uint64
			credID    uint64
			enabled   bool
			modelMap  []byte
			extraHdrs []byte
		)
		if err := rows.Scan(&id, &credID, &enabled, &modelMap, &extraHdrs); err != nil {
			return err
		}
		out[id] = channelRow{
			ID:           id,
			CredentialID: credID,
			Enabled:      enabled,
			TimeoutMs:    0,
			ModelMap:     parseStringMapJSON(modelMap),
			ExtraHeaders: parseStringMapJSON(extraHdrs),
		}
	}
	return rows.Err()
}

func loadPools(ctx context.Context, db *sql.DB, out map[uint64]poolRow) error {
	rows, err := db.QueryContext(ctx, `SELECT id, name, strategy, channel_ids_json, enabled FROM pools WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id       uint64
			name     string
			strategy string
			idsJSON  []byte
			enabled  bool
		)
		if err := rows.Scan(&id, &name, &strategy, &idsJSON, &enabled); err != nil {
			return err
		}
		var ids []uint64
		_ = json.Unmarshal(idsJSON, &ids)
		out[id] = poolRow{
			ID:         id,
			Name:       name,
			Strategy:   strategy,
			ChannelIDs: ids,
			Enabled:    enabled,
		}
	}
	return rows.Err()
}

func loadRules(ctx context.Context, db *sql.DB, out *[]ruleRow) error {
	rows, err := db.QueryContext(ctx, `SELECT priority, match_json, pool_id, fallback_pool_id, enabled FROM routing_rules WHERE enabled = 1 ORDER BY priority ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type match struct {
		Facade     string `json:"facade"`
		Model      string `json:"model"`
		ModelRegex string `json:"model_regex"`
	}

	var rules []ruleRow
	for rows.Next() {
		var (
			priority   int
			matchJSON  []byte
			poolID     uint64
			fallbackID sql.NullInt64
			enabled    bool
		)
		if err := rows.Scan(&priority, &matchJSON, &poolID, &fallbackID, &enabled); err != nil {
			return err
		}

		var m match
		_ = json.Unmarshal(matchJSON, &m)
		var re *regexp.Regexp
		if m.ModelRegex != "" {
			if compiled, err := regexp.Compile(m.ModelRegex); err == nil {
				re = compiled
			}
		}

		rules = append(rules, ruleRow{
			Priority:       priority,
			Facade:         m.Facade,
			Model:          m.Model,
			ModelRegex:     re,
			PoolID:         poolID,
			FallbackPoolID: uint64(fallbackID.Int64),
			Enabled:        enabled,
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	*out = rules
	return nil
}

func matchRule(rules []ruleRow, facade, model string) (poolID uint64, fallbackPoolID uint64) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if r.Facade != "" && r.Facade != facade {
			continue
		}
		if r.Model != "" && r.Model != model {
			continue
		}
		if r.ModelRegex != nil && !r.ModelRegex.MatchString(model) {
			continue
		}
		return r.PoolID, r.FallbackPoolID
	}
	return 0, 0
}

func pickDefaultPool(pools map[uint64]poolRow) uint64 {
	for _, p := range pools {
		if p.Enabled && p.Name == "default" {
			return p.ID
		}
	}
	type pair struct {
		id   uint64
		name string
	}
	var ps []pair
	for id, p := range pools {
		if p.Enabled {
			ps = append(ps, pair{id: id, name: p.Name})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].name < ps[j].name })
	if len(ps) == 0 {
		return 0
	}
	return ps[0].id
}

func expandPoolWeights(pools map[uint64]poolRow, channels map[uint64]channelRow, creds map[uint64]credentialRow) {
	for id, p := range pools {
		if !p.Enabled {
			continue
		}
		if p.Strategy != "weighted_rr" {
			p.ExpandedChannelIDs = nil
			pools[id] = p
			continue
		}
		var expanded []uint64
		for _, chID := range p.ChannelIDs {
			ch, ok := channels[chID]
			if !ok || !ch.Enabled {
				continue
			}
			cred, ok := creds[ch.CredentialID]
			if !ok || !cred.Enabled {
				continue
			}
			w := cred.Weight
			if w <= 0 {
				w = 1
			}
			if w > 50 {
				w = 50
			}
			for i := 0; i < w; i++ {
				expanded = append(expanded, chID)
			}
		}
		p.ExpandedChannelIDs = expanded
		pools[id] = p
	}
}

func parseStringMapJSON(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch tv := v.(type) {
		case string:
			out[k] = tv
		default:
			b, _ := json.Marshal(tv)
			out[k] = string(b)
		}
	}
	return out
}
