package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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

func (r *Router) GetPoolModels(ctx context.Context, clientKey string) ([]string, error) {
	cfg, err := r.getConfig(ctx)
	if err != nil {
		return nil, err
	}
	pool, ok := cfg.poolByClientKey[clientKey]
	if !ok {
		return nil, ErrUnauthorized
	}

	modelSet := make(map[string]bool)

	// 1. Add all keys from pool's model map
	for k := range pool.ModelMap {
		if k != "" {
			modelSet[k] = true
		}
	}

	// 2. Identify all providers and models in this pool
	providerIDs := make(map[uint64]bool)
	for _, cid := range pool.CredentialIDs {
		if cred, ok := cfg.credentials[cid]; ok {
			providerIDs[cred.ProviderID] = true
		}
	}
	for _, tier := range pool.Tiers {
		for _, m := range tier.Models {
			if m != "" {
				modelSet[m] = true
			}
		}
		for _, it := range tier.Items {
			providerIDs[it.ProviderID] = true
		}
	}

	// 3. Add models and map keys from these providers
	for pid := range providerIDs {
		if prov, ok := cfg.providers[pid]; ok {
			for m := range prov.Models {
				modelSet[m] = true
			}
			for k := range prov.ModelMap {
				if k != "" {
					modelSet[k] = true
				}
			}
		}
	}

	// 4. Convert to slice and sort
	res := make([]string, 0, len(modelSet))
	for m := range modelSet {
		res = append(res, m)
	}
	sort.Strings(res)
	return res, nil
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
	st.lastStatus = status
	st.total++
	latMs := float64(latency.Milliseconds())
	if st.ewmaLatency == 0 {
		st.ewmaLatency = latMs
	} else if latMs > 0 {
		st.ewmaLatency = st.ewmaLatency*0.8 + latMs*0.2
	}

	if ok && status >= 200 && status < 500 && status != http.StatusTooManyRequests && status != http.StatusUnauthorized && status != http.StatusForbidden {
		st.successes++
		st.failures = 0
		st.openUntil = time.Time{}
		return
	}

	st.failures++
	st.lastErrorAt = now
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		st.openUntil = now.Add(15 * time.Minute)
	case http.StatusTooManyRequests:
		d := time.Duration(5*(1<<minInt(st.failures, 6))) * time.Second
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		st.openUntil = now.Add(d)
	default:
		if status >= 500 || status == 0 {
			d := time.Duration(15*(1<<minInt(st.failures, 6))) * time.Second
			if d > 5*time.Minute {
				d = 5 * time.Minute
			}
			st.openUntil = now.Add(d)
		} else if st.failures >= 3 {
			st.openUntil = now.Add(30 * time.Second)
		}
	}
}

func (r *Router) credentialScore(credentialID uint64, baseWeight int) float64 {
	if baseWeight <= 0 {
		baseWeight = 1
	}
	inflight := float64(r.getInflight(credentialID))
	v, ok := r.credState.Load(credentialID)
	if !ok {
		return float64(baseWeight) / (1 + inflight)
	}
	st := v.(*credentialState)
	st.mu.Lock()
	defer st.mu.Unlock()
	health := float64(st.successes+1) / float64(st.total+2)
	failPenalty := 1.0 / (1.0 + float64(st.failures))
	latPenalty := 1.0
	if st.ewmaLatency > 0 {
		// Weaken the impact of latency.
		// Old: 1.0 / (1.0 + st.ewmaLatency/800.0) -> 3s diff leads to ~4x score diff
		// New: 1.0 / (1.0 + st.ewmaLatency/2500.0) -> 3s diff leads to ~2x score diff
		latPenalty = 1.0 / (1.0 + st.ewmaLatency/2500.0)
	}
	inflightPenalty := 1.0 / (1.0 + inflight)
	return float64(baseWeight) * health * failPenalty * latPenalty * inflightPenalty
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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
	if len(prov.Models) > 0 {
		if _, ok := prov.Models[upModel]; !ok {
			if fb := pickModelForAlias(prov.Models, model); fb != "" {
				upModel = fb
			}
		}
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
		if !now.Before(ent.expiresAt) {
			r.routeCacheMu.RUnlock()
			r.routeCacheMu.Lock()
			if ent2, ok2 := r.routeCache[key]; ok2 && !now.Before(ent2.expiresAt) {
				delete(r.routeCache, key)
			}
			r.routeCacheMu.Unlock()
		} else {
			r.routeCacheMu.RUnlock()
		}
	} else {
		r.routeCacheMu.RUnlock()
	}

	// Try Tiers first if configured
	if len(pool.Tiers) > 0 {
		for _, tier := range pool.Tiers {
			if !tierAppliesToModel(tier, model) {
				continue
			}
			if len(tier.Items) == 0 {
				continue
			}

			pickBestKey := func(provID uint64) (uint64, float64, bool) {
				prov, ok := cfg.providers[provID]
				if !ok {
					return 0, 0, false
				}
				upModel := model
				if mapped, ok := prov.ModelMap[model]; ok && mapped != "" {
					upModel = mapped
				}
				if mapped, ok := pool.ModelMap[model]; ok && mapped != "" {
					upModel = mapped
				}
				if len(prov.Models) > 0 {
					if _, ok := prov.Models[upModel]; !ok {
						fb := pickModelForAlias(prov.Models, model)
						if fb == "" {
							return 0, 0, false
						}
						upModel = fb
					}
				}
				creds := cfg.providerCreds[provID]
				if len(creds) == 0 {
					return 0, 0, false
				}
				var (
					bestID    uint64
					bestScore float64
					found     bool
				)
				for _, cid := range creds {
					if !isAvailable(cid) {
						continue
					}
					cred := cfg.credentials[cid]
					score := r.credentialScore(cid, cred.Weight)
					if !found || score > bestScore {
						bestID = cid
						bestScore = score
						found = true
					}
				}
				return bestID, bestScore, found
			}

			if tier.Strategy == "priority" {
				for _, it := range tier.Items {
					if cid, _, ok := pickBestKey(it.ProviderID); ok {
						return cid, true
					}
				}
				continue
			}

			var (
				candidates []struct {
					cid   uint64
					score float64
				}
				totalWeight float64
			)
			for _, it := range tier.Items {
				w := float64(it.Weight)
				if w <= 0 {
					w = 1
				}
				cid, keyScore, ok := pickBestKey(it.ProviderID)
				if !ok {
					continue
				}
				score := w * keyScore
				candidates = append(candidates, struct {
					cid   uint64
					score float64
				}{cid, score})
				totalWeight += score
			}

			if len(candidates) > 0 {
				if totalWeight <= 0 {
					return candidates[0].cid, true
				}
				// Reuse the counter for a stable-ish but distributed pick
				st := r.getPoolState(pool.ID)
				count := atomic.AddUint64(&st.counter, 1)

				pick := float64(count%1000) / 1000.0 * totalWeight
				var current float64
				for _, cand := range candidates {
					current += cand.score
					if pick <= current {
						return cand.cid, true
					}
				}
				return candidates[len(candidates)-1].cid, true
			}
		}
		return 0, false
	}

	// Fallback to legacy pool strategy if no tiers
	ids := pool.CredentialIDs
	if pool.Strategy == "weighted_rr" && len(pool.ExpandedCredentialIDs) > 0 {
		ids = pool.ExpandedCredentialIDs
	}

	if len(ids) == 0 {
		return 0, false
	}

	// Simple round-robin or weighted-rr (if using expanded IDs)
	// We use a counter per pool for rotation
	st := r.getPoolState(pool.ID)
	count := atomic.AddUint64(&st.counter, 1)

	// Try up to len(ids) times to find an available one
	n := uint64(len(ids))
	for i := uint64(0); i < n; i++ {
		idx := (count + i) % n
		cid := ids[idx]
		if isAvailable(cid) {
			return cid, true
		}
	}

	return 0, false
}

func tierAppliesToModel(t Tier, model string) bool {
	if len(t.Models) == 0 {
		return true
	}
	m := strings.TrimSpace(model)
	if m == "" {
		return true
	}
	for _, x := range t.Models {
		if strings.EqualFold(strings.TrimSpace(x), m) {
			return true
		}
	}
	return false
}

func pickModelForAlias(models map[string]bool, alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" || len(models) == 0 {
		return ""
	}
	isSmall := strings.Contains(alias, "small") || strings.Contains(alias, "haiku") || strings.Contains(alias, "fast")

	type cand struct {
		id    string
		score int
	}
	best := cand{}
	found := false

	scoreOf := func(id string) int {
		s := strings.ToLower(id)
		score := 0
		if isSmall {
			if strings.Contains(s, "haiku") {
				score += 6
			}
			if strings.Contains(s, "mini") || strings.Contains(s, "small") {
				score += 5
			}
			if strings.Contains(s, "flash") || strings.Contains(s, "lite") {
				score += 4
			}
			if strings.Contains(s, "nano") {
				score += 3
			}
			if strings.Contains(s, "3b") || strings.Contains(s, "4b") || strings.Contains(s, "7b") || strings.Contains(s, "8b") {
				score += 2
			}
			if strings.Contains(s, "sonnet") || strings.Contains(s, "opus") {
				score -= 3
			}
		} else {
			if strings.Contains(s, "sonnet") {
				score += 6
			}
			if strings.Contains(s, "opus") {
				score += 5
			}
			if strings.Contains(s, "claude") {
				score += 4
			}
			if strings.Contains(s, "gpt-4") || strings.Contains(s, "kimi") || strings.Contains(s, "glm") || strings.Contains(s, "deepseek") {
				score += 3
			}
			if strings.Contains(s, "coder") || strings.Contains(s, "code") {
				score += 4
			}
			if strings.Contains(s, "reasoner") || strings.Contains(s, "thinking") {
				score += 2
			}
			if strings.Contains(s, "haiku") || strings.Contains(s, "mini") || strings.Contains(s, "flash") || strings.Contains(s, "lite") {
				score -= 2
			}
		}
		score -= minInt(len(id)/12, 6)
		return score
	}

	for id := range models {
		sc := scoreOf(id)
		if !found || sc > best.score || (sc == best.score && (len(id) < len(best.id) || (len(id) == len(best.id) && id < best.id))) {
			best = cand{id: id, score: sc}
			found = true
		}
	}
	if !found {
		return ""
	}
	return best.id
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
	successes   int64
	total       int64
	ewmaLatency float64
	openUntil   time.Time
	lastSeen    time.Time
	lastErrorAt time.Time
	lastLatency time.Duration
	lastStatus  int
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
	Models         map[string]bool
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
	Models   []string   `json:"models,omitempty"`
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
	rows, err := db.QueryContext(ctx, `SELECT id, type, base_url, default_headers_json, model_map_json, models_json FROM providers WHERE enabled = 1`)
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
			modelsJS []byte
		)
		if err := rows.Scan(&id, &typ, &baseURL, &hdrsJSON, &modelMap, &modelsJS); err != nil {
			return err
		}
		out[id] = providerRow{
			ID:             id,
			Type:           typ,
			BaseURL:        baseURL,
			DefaultHeaders: parseStringMapJSON(hdrsJSON),
			ModelMap:       parseStringMapJSON(modelMap),
			Models:         parseStringSetJSON(modelsJS),
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
		_ = unmarshalMaybeJSONString(tiersJSON, &tiers)

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

func unmarshalMaybeJSONString(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		return json.Unmarshal([]byte(s), v)
	}
	return json.Unmarshal(raw, v)
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

func parseStringSetJSON(raw []byte) map[string]bool {
	if len(raw) == 0 {
		return map[string]bool{}
	}
	var decoded any
	if err := unmarshalMaybeJSONString(raw, &decoded); err != nil {
		return map[string]bool{}
	}
	out := map[string]bool{}
	switch v := decoded.(type) {
	case []any:
		for _, it := range v {
			switch x := it.(type) {
			case string:
				s := strings.TrimSpace(x)
				if s != "" {
					out[s] = true
				}
			case map[string]any:
				if id, _ := x["id"].(string); strings.TrimSpace(id) != "" {
					out[strings.TrimSpace(id)] = true
				}
			}
		}
	case map[string]any:
		if data, ok := v["data"].([]any); ok {
			for _, it := range data {
				m, _ := it.(map[string]any)
				if m == nil {
					continue
				}
				if id, _ := m["id"].(string); strings.TrimSpace(id) != "" {
					out[strings.TrimSpace(id)] = true
				}
			}
		}
	}
	return out
}
