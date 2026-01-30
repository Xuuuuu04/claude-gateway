package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claude-gateway/internal/crypto"
	"claude-gateway/internal/metrics"
)

var ErrNotConfigured = errors.New("gateway not configured")
var ErrUnauthorized = errors.New("unauthorized client key")

type Router struct {
	db     *sql.DB
	m      *metrics.Metrics
	cipher *crypto.AESGCM

	cacheTTL time.Duration

	cacheMu sync.RWMutex
	cache   loadedConfig

	poolMu     sync.Mutex
	poolStates map[uint64]*poolState

	credState sync.Map

	routeCacheMu sync.RWMutex
	routeCache   map[string]routeCacheEntry
	routeCacheTT time.Duration
}

func New(db *sql.DB, m *metrics.Metrics, cipher *crypto.AESGCM) *Router {
	return &Router{
		db:           db,
		m:            m,
		cipher:       cipher,
		cacheTTL:     5 * time.Second,
		poolStates:   make(map[uint64]*poolState),
		routeCache:   make(map[string]routeCacheEntry),
		routeCacheTT: 90 * time.Second,
	}
}

type routeCacheEntry struct {
	credentialID uint64
	expiresAt    time.Time
}

type RoutedUpstream struct {
	PoolID       uint64
	CredentialID uint64
	ProviderID   uint64
	ProviderType string
	BaseURL      string
	APIKey       []byte
	Model        string
	Headers      map[string]string
	Timeout      time.Duration
}

func (r *Router) PickUpstream(ctx context.Context, clientKey, facade, model string) (RoutedUpstream, error) {
	return r.pickUpstream(ctx, clientKey, facade, model, nil)
}

func (r *Router) PickUpstreamExclude(ctx context.Context, clientKey, facade, model string, exclude map[uint64]bool) (RoutedUpstream, error) {
	return r.pickUpstream(ctx, clientKey, facade, model, exclude)
}

func (r *Router) RecordRouteResult(poolID uint64, facade, model string, credentialID uint64, ok bool, status int) {
	if poolID == 0 || credentialID == 0 {
		return
	}
	key := r.routeKey(poolID, facade, model)
	if ok {
		r.routeCacheMu.Lock()
		r.routeCache[key] = routeCacheEntry{credentialID: credentialID, expiresAt: time.Now().Add(r.routeCacheTT)}
		r.routeCacheMu.Unlock()
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500 || status == 0 {
		r.routeCacheMu.Lock()
		if cur, ok := r.routeCache[key]; ok && cur.credentialID == credentialID {
			delete(r.routeCache, key)
		}
		r.routeCacheMu.Unlock()
	}
}

func (r *Router) routeKey(poolID uint64, facade, model string) string {
	return fmt.Sprintf("%d|%s|%s", poolID, strings.ToLower(strings.TrimSpace(facade)), strings.TrimSpace(model))
}

func (r *Router) EndRequest(credentialID uint64, ok bool, status int, latency time.Duration) {
	if credentialID == 0 {
		return
	}
	v, _ := r.credState.LoadOrStore(credentialID, &credentialState{})
	st := v.(*credentialState)
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

func (r *Router) pickUpstream(ctx context.Context, clientKey, facade, model string, exclude map[uint64]bool) (RoutedUpstream, error) {
	cfg, err := r.getConfig(ctx)
	if err != nil {
		return RoutedUpstream{}, err
	}
	if len(cfg.pools) == 0 || len(cfg.credentials) == 0 || len(cfg.providers) == 0 {
		return RoutedUpstream{}, ErrNotConfigured
	}

	pool, ok := cfg.poolByClientKey[clientKey]
	if !ok {
		return RoutedUpstream{}, ErrUnauthorized
	}

	credID, ok := r.pickCredentialFromPool(cfg, pool, facade, model, exclude)
	if !ok {
		return RoutedUpstream{}, ErrNotConfigured
	}

	cred := cfg.credentials[credID]
	prov := cfg.providers[cred.ProviderID]

	apiKey, err := r.cipher.Decrypt(cred.APIKeyCiphertext)
	if err != nil {
		return RoutedUpstream{}, fmt.Errorf("decrypt credential: %w", err)
	}

	headers := make(map[string]string, 8)
	for k, v := range prov.DefaultHeaders {
		headers[k] = v
	}

	upModel := model
	if mapped, ok := prov.ModelMap[model]; ok && mapped != "" {
		upModel = mapped
	}
	if mapped, ok := pool.ModelMap[model]; ok && mapped != "" {
		upModel = mapped
	}

	r.startRequest(credID)

	return RoutedUpstream{
		PoolID:       pool.ID,
		CredentialID: credID,
		ProviderID:   cred.ProviderID,
		ProviderType: prov.Type,
		BaseURL:      prov.BaseURL,
		APIKey:       apiKey,
		Model:        upModel,
		Headers:      headers,
		Timeout:      30 * time.Second, // Default timeout
	}, nil
}

func (r *Router) startRequest(credentialID uint64) {
	v, _ := r.credState.LoadOrStore(credentialID, &credentialState{})
	st := v.(*credentialState)
	atomic.AddInt64(&st.inflight, 1)
}

func (r *Router) pickCredentialFromPool(cfg loadedConfig, pool poolRow, facade, model string, exclude map[uint64]bool) (uint64, bool) {
	now := time.Now()

	isAvailable := func(credID uint64) bool {
		if exclude != nil && exclude[credID] {
			return false
		}
		cred, ok := cfg.credentials[credID]
		if !ok || !cred.Enabled {
			return false
		}
		if r.isCredentialOpen(credID, now) {
			return false
		}
		if cred.ConcurrencyLimit > 0 && r.getInflight(credID) >= int64(cred.ConcurrencyLimit) {
			return false
		}
		return true
	}

	key := r.routeKey(pool.ID, facade, model)
	r.routeCacheMu.RLock()
	if ent, ok := r.routeCache[key]; ok {
		if now.Before(ent.expiresAt) && isAvailable(ent.credentialID) {
			r.routeCacheMu.RUnlock()
			return ent.credentialID, true
		}
	}
	r.routeCacheMu.RUnlock()

	// Try Tiers first if configured
	if len(pool.Tiers) > 0 {
		for tierIdx, tier := range pool.Tiers {
			if len(tier.Items) == 0 {
				continue
			}

			// Helper to pick a key from a provider
			pickKey := func(provID uint64) (uint64, bool) {
				creds := cfg.providerCreds[provID]
				if len(creds) == 0 {
					return 0, false
				}
				// Use a provider-specific state for round-robin across keys of the same provider
				state := r.getPoolState(uint64(10000000) + provID)
				for i := 0; i < len(creds); i++ {
					idx := int(atomic.AddUint64(&state.counter, 1)-1) % len(creds)
					cid := creds[idx]
					if isAvailable(cid) {
						return cid, true
					}
				}
				return 0, false
			}

			// Apply tier strategy to pick a provider
			switch tier.Strategy {
			case "weighted_rr":
				var expanded []uint64
				for _, it := range tier.Items {
					w := it.Weight
					if w <= 0 {
						w = 1
					}
					if w > 50 {
						w = 50
					}
					for i := 0; i < w; i++ {
						expanded = append(expanded, it.ProviderID)
					}
				}
				state := r.getPoolState(uint64(20000000) + pool.ID*100 + uint64(tierIdx))
				for i := 0; i < len(expanded); i++ {
					idx := int(atomic.AddUint64(&state.counter, 1)-1) % len(expanded)
					provID := expanded[idx]
					if cid, ok := pickKey(provID); ok {
						return cid, true
					}
				}
			case "priority":
				for _, it := range tier.Items {
					if cid, ok := pickKey(it.ProviderID); ok {
						return cid, true
					}
				}
			default: // random or simple rr
				state := r.getPoolState(uint64(20000000) + pool.ID*100 + uint64(tierIdx))
				for i := 0; i < len(tier.Items); i++ {
					idx := int(atomic.AddUint64(&state.counter, 1)-1) % len(tier.Items)
					provID := tier.Items[idx].ProviderID
					if cid, ok := pickKey(provID); ok {
						return cid, true
					}
				}
			}
		}
	}

	// Fallback to legacy CredentialIDs behavior
	switch pool.Strategy {
	case "least_inflight":
		var (
			bestID       uint64
			bestInflight int64
			found        bool
		)
		for _, credID := range pool.CredentialIDs {
			if !isAvailable(credID) {
				continue
			}
			in := r.getInflight(credID)
			if !found || in < bestInflight {
				bestID = credID
				bestInflight = in
				found = true
			}
		}
		return bestID, found
	case "priority_failover":
		for _, credID := range pool.CredentialIDs {
			if isAvailable(credID) {
				return credID, true
			}
		}
		return 0, false
	default: // weighted_rr (expanded)
		expanded := pool.ExpandedCredentialIDs
		if len(expanded) == 0 {
			expanded = pool.CredentialIDs
		}
		if len(expanded) == 0 {
			return 0, false
		}
		state := r.getPoolState(pool.ID)
		for i := 0; i < len(expanded); i++ {
			idx := int(atomic.AddUint64(&state.counter, 1)-1) % len(expanded)
			credID := expanded[idx]
			if isAvailable(credID) {
				return credID, true
			}
		}
		return 0, false
	}
}

func (r *Router) getInflight(credentialID uint64) int64 {
	v, ok := r.credState.Load(credentialID)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(&v.(*credentialState).inflight)
}

func (r *Router) isCredentialOpen(credentialID uint64, now time.Time) bool {
	v, ok := r.credState.Load(credentialID)
	if !ok {
		return false
	}
	st := v.(*credentialState)
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

type credentialState struct {
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

	providers       map[uint64]providerRow
	credentials     map[uint64]credentialRow
	providerCreds   map[uint64][]uint64
	pools           map[uint64]poolRow
	poolByClientKey map[string]poolRow
}

type providerRow struct {
	ID             uint64
	Type           string
	BaseURL        string
	DefaultHeaders map[string]string
	ModelMap       map[string]string
}

type credentialRow struct {
	ID               uint64
	ProviderID       uint64
	APIKeyCiphertext []byte
	Weight           int
	ConcurrencyLimit int
	Enabled          bool
}

type poolRow struct {
	ID                    uint64
	Name                  string
	ClientKey             string
	Strategy              string
	Tiers                 []Tier
	CredentialIDs         []uint64
	ExpandedCredentialIDs []uint64
	ModelMap              map[string]string
	Enabled               bool
}

type Tier struct {
	Name     string     `json:"name"`
	Strategy string     `json:"strategy"`
	Items    []TierItem `json:"items"`
}

type TierItem struct {
	ProviderID uint64 `json:"provider_id"`
	Weight     int    `json:"weight"`
}

func loadConfig(ctx context.Context, db *sql.DB) (loadedConfig, error) {
	cfg := loadedConfig{
		loadedAt:        time.Now(),
		providers:       map[uint64]providerRow{},
		credentials:     map[uint64]credentialRow{},
		providerCreds:   map[uint64][]uint64{},
		pools:           map[uint64]poolRow{},
		poolByClientKey: map[string]poolRow{},
	}

	if err := loadProviders(ctx, db, cfg.providers); err != nil {
		return loadedConfig{}, err
	}
	if err := loadCredentials(ctx, db, cfg.credentials, cfg.providerCreds); err != nil {
		return loadedConfig{}, err
	}
	if err := loadPools(ctx, db, cfg.pools, cfg.poolByClientKey); err != nil {
		return loadedConfig{}, err
	}

	expandPoolWeights(cfg.pools, cfg.credentials)
	return cfg, nil
}

func loadProviders(ctx context.Context, db *sql.DB, out map[uint64]providerRow) error {
	rows, err := db.QueryContext(ctx, `SELECT id, type, base_url, default_headers_json, model_map_json FROM providers WHERE enabled = 1`)
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
			modelMap []byte
		)
		if err := rows.Scan(&id, &typ, &baseURL, &hdrsJSON, &modelMap); err != nil {
			return err
		}
		out[id] = providerRow{
			ID:             id,
			Type:           typ,
			BaseURL:        baseURL,
			DefaultHeaders: parseStringMapJSON(hdrsJSON),
			ModelMap:       parseStringMapJSON(modelMap),
		}
	}
	return rows.Err()
}

func loadCredentials(ctx context.Context, db *sql.DB, out map[uint64]credentialRow, provOut map[uint64][]uint64) error {
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
		provOut[providerID] = append(provOut[providerID], id)
	}
	return rows.Err()
}

func loadPools(ctx context.Context, db *sql.DB, out map[uint64]poolRow, byKey map[string]poolRow) error {
	rows, err := db.QueryContext(ctx, `SELECT id, name, client_key, strategy, tiers_json, credential_ids_json, model_map_json, enabled FROM pools WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id        uint64
			name      string
			ckey      string
			strategy  string
			tiersJSON []byte
			idsJSON   []byte
			mmJSON    []byte
			enabled   bool
		)
		if err := rows.Scan(&id, &name, &ckey, &strategy, &tiersJSON, &idsJSON, &mmJSON, &enabled); err != nil {
			return err
		}
		var ids []uint64
		_ = json.Unmarshal(idsJSON, &ids)
		var tiers []Tier
		_ = json.Unmarshal(tiersJSON, &tiers)

		p := poolRow{
			ID:            id,
			Name:          name,
			ClientKey:     ckey,
			Strategy:      strategy,
			Tiers:         tiers,
			CredentialIDs: ids,
			ModelMap:      parseStringMapJSON(mmJSON),
			Enabled:       enabled,
		}
		out[id] = p
		if p.ClientKey != "" {
			byKey[p.ClientKey] = p
		}
	}
	return rows.Err()
}

func expandPoolWeights(pools map[uint64]poolRow, creds map[uint64]credentialRow) {
	for id, p := range pools {
		if !p.Enabled {
			continue
		}
		if p.Strategy != "weighted_rr" {
			p.ExpandedCredentialIDs = nil
			pools[id] = p
			continue
		}
		var expanded []uint64
		for _, credID := range p.CredentialIDs {
			cred, ok := creds[credID]
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
				expanded = append(expanded, credID)
			}
		}
		p.ExpandedCredentialIDs = expanded
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
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
