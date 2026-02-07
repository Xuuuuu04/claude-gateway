package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"claude-gateway/src/internal/crypto"
	"claude-gateway/src/internal/db"
)

type channelRow struct {
	ID           uint64
	Name         string
	ProviderType string
	BaseURL      string
	ModelMap     []byte
	HasMM        bool
	SourceType   int64
	Weight       int
	Enabled      bool
}

type keyRow struct {
	ChannelID uint64
	Name      string
	APIKey    string
	Weight    int
	Conc      *int
	Enabled   bool
}

func main() {
	var (
		mode               = flag.String("mode", "inspect", "inspect|migrate")
		dsn                = flag.String("dsn", strings.TrimSpace(os.Getenv("MYSQL_DSN")), "MySQL DSN (default MYSQL_DSN)")
		dryRun             = flag.Bool("dry-run", true, "do not commit changes")
		initDefaultRouting = flag.Bool("init-default-routing", true, "create channels/default pool/default rule")

		channelTable       = flag.String("channel-table", "", "APICenter channel table")
		keyTable           = flag.String("key-table", "", "APICenter key table (optional; leave empty for OneAPI/APICenter channels.key multiline schema)")
		channelIDCol       = flag.String("channel-id-col", "id", "channel id column")
		channelNameCol     = flag.String("channel-name-col", "name", "channel name column")
		channelTypeCol     = flag.String("channel-type-col", "type", "channel type column")
		channelBaseURLCol  = flag.String("channel-baseurl-col", "base_url", "channel base_url column")
		channelModelMapCol = flag.String("channel-modelmap-col", "model_mapping", "channel model map json column (optional)")
		channelKeyCol      = flag.String("channel-key-col", "key", "channel key column for OneAPI/APICenter schema")
		channelStatusCol   = flag.String("channel-status-col", "status", "channel status column for OneAPI/APICenter schema")
		channelWeightCol   = flag.String("channel-weight-col", "weight", "channel weight column for OneAPI/APICenter schema")

		keyChannelIDCol = flag.String("key-channelid-col", "channel_id", "key -> channel id column")
		keyNameCol      = flag.String("key-name-col", "name", "key name column")
		keyValueCol     = flag.String("key-value-col", "api_key", "key value column")
		keyWeightCol    = flag.String("key-weight-col", "weight", "key weight column (optional)")
		keyConcCol      = flag.String("key-concurrency-col", "", "key concurrency_limit column (optional)")
		keyEnabledCol   = flag.String("key-enabled-col", "", "key enabled column (optional)")

		limit = flag.Int("limit", 0, "limit imported keys (0 = no limit)")
	)
	flag.Parse()

	if strings.TrimSpace(*dsn) == "" {
		log.Fatalf("dsn is required (MYSQL_DSN)")
	}

	sqlDB, err := db.Open(*dsn)
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer sqlDB.Close()

	switch strings.ToLower(strings.TrimSpace(*mode)) {
	case "inspect":
		if err := inspectDB(sqlDB); err != nil {
			log.Fatalf("inspect: %v", err)
		}
		return
	case "migrate":
	default:
		log.Fatalf("invalid mode: %s", *mode)
	}

	if strings.TrimSpace(*channelTable) == "" {
		log.Fatalf("channel-table is required in migrate mode")
	}

	if err := db.Migrate(sqlDB); err != nil {
		log.Fatalf("db migrate: %v", err)
	}

	keyEnc := strings.TrimSpace(os.Getenv("KEY_ENC_MASTER_B64"))
	if keyEnc == "" {
		log.Fatalf("KEY_ENC_MASTER_B64 is required for migrate mode")
	}
	cipher, err := crypto.NewAESGCMFromBase64Key(keyEnc)
	if err != nil {
		log.Fatalf("cipher: %v", err)
	}

	var (
		channels []channelRow
		keys     []keyRow
	)
	if strings.TrimSpace(*keyTable) == "" {
		channels, keys, err = loadOneAPIChannelsAsKeys(sqlDB, *channelTable, *channelIDCol, *channelNameCol, *channelTypeCol, *channelBaseURLCol, *channelModelMapCol, *channelKeyCol, *channelStatusCol, *channelWeightCol, *limit)
		if err != nil {
			log.Fatalf("load oneapi/apicenter channels: %v", err)
		}
	} else {
		channels, err = loadAPICenterChannels(sqlDB, *channelTable, *channelIDCol, *channelNameCol, *channelTypeCol, *channelBaseURLCol, *channelModelMapCol)
		if err != nil {
			log.Fatalf("load channels: %v", err)
		}
		keys, err = loadAPICenterKeys(sqlDB, *keyTable, *keyChannelIDCol, *keyNameCol, *keyValueCol, *keyWeightCol, *keyConcCol, *keyEnabledCol, *limit)
		if err != nil {
			log.Fatalf("load keys: %v", err)
		}
	}

	tx, err := sqlDB.Begin()
	if err != nil {
		log.Fatalf("tx begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	providerIDs := map[string]uint64{}
	createdProviders := 0
	createdCreds := 0
	createdChannels := 0
	usedChannelIDs := make([]uint64, 0, len(keys))

	keysByChannel := map[uint64][]keyRow{}
	for _, k := range keys {
		keysByChannel[k.ChannelID] = append(keysByChannel[k.ChannelID], k)
	}

	for _, ch := range channels {
		chKeys := keysByChannel[ch.ID]
		if len(chKeys) == 0 {
			continue
		}

		pKey := ch.ProviderType + "\n" + ch.BaseURL
		provID, ok := providerIDs[pKey]
		if !ok {
			id, created, err := ensureProvider(tx, ch.ProviderType, ch.BaseURL, ch)
			if err != nil {
				log.Fatalf("ensure provider: %v", err)
			}
			provID = id
			providerIDs[pKey] = id
			if created {
				createdProviders++
			}
		}

		for i, k := range chKeys {
			name := strings.TrimSpace(k.Name)
			if name == "" {
				name = strings.TrimSpace(ch.Name)
				if name == "" {
					name = "key"
				}
				name += "-" + strconv.Itoa(i+1)
			}
			if strings.TrimSpace(k.APIKey) == "" {
				continue
			}

			if exists, err := credentialExists(tx, provID, name); err != nil {
				log.Fatalf("credential exists: %v", err)
			} else if exists {
				continue
			}

			blob, err := cipher.Encrypt([]byte(k.APIKey))
			if err != nil {
				log.Fatalf("encrypt: %v", err)
			}
			last4v := last4(k.APIKey)
			weight := k.Weight
			if weight <= 0 {
				weight = 1
			}
			var conc any = nil
			if k.Conc != nil {
				conc = *k.Conc
			}

			res, err := tx.Exec(`INSERT INTO credentials(provider_id, name, api_key_ciphertext, key_last4, weight, concurrency_limit, enabled) VALUES (?,?,?,?,?,?,?)`,
				provID, name, blob, last4v, weight, conc, k.Enabled)
			if err != nil {
				log.Fatalf("insert credential: %v", err)
			}
			credID64, _ := res.LastInsertId()
			createdCreds++

			if *initDefaultRouting {
				chName := "auto-" + name
				res, err := tx.Exec(`INSERT INTO channels(credential_id, name, tags_json, model_map_json, extra_headers_json, enabled) VALUES (?,?,?,?,?,?)`,
					uint64(credID64), chName, []byte("null"), []byte("null"), []byte("null"), true)
				if err != nil {
					log.Fatalf("insert channel: %v", err)
				}
				chID64, _ := res.LastInsertId()
				createdChannels++
				usedChannelIDs = append(usedChannelIDs, uint64(chID64))
			}
		}
	}

	var defaultPoolID uint64
	if *initDefaultRouting && len(usedChannelIDs) > 0 {
		id, err := ensureDefaultPool(tx, usedChannelIDs)
		if err != nil {
			log.Fatalf("ensure default pool: %v", err)
		}
		defaultPoolID = id
		if err := ensureDefaultRule(tx, defaultPoolID); err != nil {
			log.Fatalf("ensure default rule: %v", err)
		}
	}

	log.Printf("providers created: %d", createdProviders)
	log.Printf("credentials created: %d", createdCreds)
	if *initDefaultRouting {
		log.Printf("channels created: %d", createdChannels)
		log.Printf("default pool id: %d", defaultPoolID)
	}

	if *dryRun {
		log.Printf("dry-run enabled: rollback")
		return
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}
	log.Printf("done")
}

func inspectDB(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return err
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Println("tables:")
	for _, t := range tables {
		fmt.Println("  -", t)
	}

	re := regexp.MustCompile(`(?i)(api|center|channel|provider|key|credential)`)
	var suspect []string
	for _, t := range tables {
		if re.MatchString(t) {
			suspect = append(suspect, t)
		}
	}
	sort.Strings(suspect)
	if len(suspect) == 0 {
		return nil
	}

	fmt.Println("\nsuspected tables:")
	for _, t := range suspect {
		fmt.Println("  -", t)
		cRows, err := sqlDB.Query(`SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position`, t)
		if err != nil {
			return err
		}
		var cols []string
		for cRows.Next() {
			var cn, ct string
			if err := cRows.Scan(&cn, &ct); err != nil {
				_ = cRows.Close()
				return err
			}
			cols = append(cols, cn+" ("+ct+")")
		}
		_ = cRows.Close()
		for _, c := range cols {
			fmt.Println("      ", c)
		}
	}
	return nil
}

func loadOneAPIChannelsAsKeys(sqlDB *sql.DB, channelTable, idCol, nameCol, typeCol, baseURLCol, modelMapCol, keyCol, statusCol, weightCol string, limit int) ([]channelRow, []keyRow, error) {
	tbl, err := safeIdent(channelTable)
	if err != nil {
		return nil, nil, err
	}
	idc, err := safeIdent(idCol)
	if err != nil {
		return nil, nil, err
	}
	nc, err := safeIdent(nameCol)
	if err != nil {
		return nil, nil, err
	}
	tc, err := safeIdent(typeCol)
	if err != nil {
		return nil, nil, err
	}
	bc, err := safeIdent(baseURLCol)
	if err != nil {
		return nil, nil, err
	}
	mmc, err := safeIdent(modelMapCol)
	if err != nil {
		return nil, nil, err
	}
	kc, err := safeIdent(keyCol)
	if err != nil {
		return nil, nil, err
	}
	sc, err := safeIdent(statusCol)
	if err != nil {
		return nil, nil, err
	}
	wc, err := safeIdent(weightCol)
	if err != nil {
		return nil, nil, err
	}

	q := "SELECT " + idc + ", " + nc + ", " + tc + ", " + bc + ", " + mmc + ", " + kc + ", " + sc + ", " + wc + " FROM " + tbl
	rows, err := sqlDB.Query(q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		channels []channelRow
		keys     []keyRow
	)

	for rows.Next() {
		var (
			id      uint64
			nameV   sql.NullString
			typeV   sql.NullInt64
			baseV   sql.NullString
			mmV     sql.NullString
			keyV    sql.NullString
			statusV sql.NullInt64
			weightV sql.NullInt64
		)
		if err := rows.Scan(&id, &nameV, &typeV, &baseV, &mmV, &keyV, &statusV, &weightV); err != nil {
			return nil, nil, err
		}

		srcType := int64(0)
		if typeV.Valid {
			srcType = typeV.Int64
		}
		baseURL := strings.TrimSpace(baseV.String)
		provType := mapProviderTypeFromOneAPI(srcType, baseURL)
		if baseURL == "" {
			baseURL = defaultBaseURLFromOneAPI(srcType, provType)
		}
		if baseURL == "" {
			continue
		}

		enabled := true
		if statusV.Valid {
			enabled = statusV.Int64 == 1
		}
		weight := 1
		if weightV.Valid && weightV.Int64 > 0 {
			weight = int(weightV.Int64)
		}

		modelMapRaw := strings.TrimSpace(mmV.String)
		hasMM := false
		var mmBytes []byte
		if modelMapRaw != "" && json.Valid([]byte(modelMapRaw)) {
			mmBytes = []byte(modelMapRaw)
			hasMM = true
		} else {
			mmBytes = nil
		}

		ch := channelRow{
			ID:           id,
			Name:         strings.TrimSpace(nameV.String),
			ProviderType: provType,
			BaseURL:      baseURL,
			ModelMap:     mmBytes,
			HasMM:        hasMM,
			SourceType:   srcType,
			Weight:       weight,
			Enabled:      enabled,
		}
		channels = append(channels, ch)

		keyText := normalizeNewlines(keyV.String)
		keyLines := splitNonEmptyLines(keyText)
		if len(keyLines) == 0 {
			continue
		}
		baseName := strings.TrimSpace(ch.Name)
		if baseName == "" {
			baseName = fmt.Sprintf("channel-%d", ch.ID)
		}
		for i, k := range keyLines {
			name := baseName
			if len(keyLines) > 1 {
				name = fmt.Sprintf("%s-%d", baseName, i+1)
			}
			keys = append(keys, keyRow{
				ChannelID: ch.ID,
				Name:      name,
				APIKey:    k,
				Weight:    weight,
				Conc:      nil,
				Enabled:   enabled,
			})
			if limit > 0 && len(keys) >= limit {
				return channels, keys, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return channels, keys, nil
}

func mapProviderTypeFromOneAPI(oneType int64, baseURL string) string {
	u := strings.ToLower(strings.TrimSpace(baseURL))
	if strings.Contains(u, "/anthropic") || strings.Contains(u, "anthropic") || oneType == 14 {
		return "anthropic"
	}
	if strings.Contains(u, "generativelanguage.googleapis.com") || strings.Contains(u, "gemini") {
		return "gemini"
	}
	return "openai"
}

func defaultBaseURLFromOneAPI(oneType int64, providerType string) string {
	switch oneType {
	case 20:
		return "https://openrouter.ai/api"
	case 40:
		return "https://api.siliconflow.cn"
	case 43:
		return "https://api.deepseek.com"
	default:
		if providerType == "anthropic" {
			return "https://api.anthropic.com"
		}
		return ""
	}
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func splitNonEmptyLines(s string) []string {
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func safeIdent(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("empty identifier")
	}
	parts := strings.Split(name, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(p) {
			return "", fmt.Errorf("invalid identifier: %s", name)
		}
		out = append(out, "`"+p+"`")
	}
	return strings.Join(out, "."), nil
}

func loadAPICenterChannels(sqlDB *sql.DB, table, idCol, nameCol, typeCol, baseURLCol, modelMapCol string) ([]channelRow, error) {
	tbl, err := safeIdent(table)
	if err != nil {
		return nil, err
	}
	idc, err := safeIdent(idCol)
	if err != nil {
		return nil, err
	}
	nc, err := safeIdent(nameCol)
	if err != nil {
		return nil, err
	}
	tc, err := safeIdent(typeCol)
	if err != nil {
		return nil, err
	}
	bc, err := safeIdent(baseURLCol)
	if err != nil {
		return nil, err
	}

	hasMM := strings.TrimSpace(modelMapCol) != ""
	var mmc string
	if hasMM {
		mmc, err = safeIdent(modelMapCol)
		if err != nil {
			return nil, err
		}
	}

	q := "SELECT " + idc + ", " + nc + ", " + tc + ", " + bc
	if hasMM {
		q += ", " + mmc
	}
	q += " FROM " + tbl

	rows, err := sqlDB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []channelRow
	for rows.Next() {
		var (
			id      uint64
			name    sql.NullString
			typ     sql.NullString
			baseURL sql.NullString
			mm      []byte
		)
		if hasMM {
			if err := rows.Scan(&id, &name, &typ, &baseURL, &mm); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&id, &name, &typ, &baseURL); err != nil {
				return nil, err
			}
		}
		out = append(out, channelRow{
			ID:           id,
			Name:         strings.TrimSpace(name.String),
			ProviderType: strings.TrimSpace(typ.String),
			BaseURL:      strings.TrimSpace(baseURL.String),
			ModelMap:     mm,
			HasMM:        hasMM,
			SourceType:   0,
			Weight:       1,
			Enabled:      true,
		})
	}
	return out, rows.Err()
}

func loadAPICenterKeys(sqlDB *sql.DB, table, channelIDCol, nameCol, valueCol, weightCol, concCol, enabledCol string, limit int) ([]keyRow, error) {
	tbl, err := safeIdent(table)
	if err != nil {
		return nil, err
	}
	cidc, err := safeIdent(channelIDCol)
	if err != nil {
		return nil, err
	}
	nc, err := safeIdent(nameCol)
	if err != nil {
		return nil, err
	}
	vc, err := safeIdent(valueCol)
	if err != nil {
		return nil, err
	}

	hasWeight := strings.TrimSpace(weightCol) != ""
	hasConc := strings.TrimSpace(concCol) != ""
	hasEnabled := strings.TrimSpace(enabledCol) != ""

	var wc, cc, ec string
	if hasWeight {
		wc, err = safeIdent(weightCol)
		if err != nil {
			return nil, err
		}
	}
	if hasConc {
		cc, err = safeIdent(concCol)
		if err != nil {
			return nil, err
		}
	}
	if hasEnabled {
		ec, err = safeIdent(enabledCol)
		if err != nil {
			return nil, err
		}
	}

	q := "SELECT " + cidc + ", " + nc + ", " + vc
	if hasWeight {
		q += ", " + wc
	}
	if hasConc {
		q += ", " + cc
	}
	if hasEnabled {
		q += ", " + ec
	}
	q += " FROM " + tbl

	rows, err := sqlDB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []keyRow
	for rows.Next() {
		var (
			channelID uint64
			name      sql.NullString
			key       sql.NullString
			weightV   sql.NullInt64
			concV     sql.NullInt64
			enabledV  sql.NullBool
		)
		scanArgs := []any{&channelID, &name, &key}
		if hasWeight {
			scanArgs = append(scanArgs, &weightV)
		}
		if hasConc {
			scanArgs = append(scanArgs, &concV)
		}
		if hasEnabled {
			scanArgs = append(scanArgs, &enabledV)
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, err
		}
		weight := 1
		if hasWeight && weightV.Valid {
			weight = int(weightV.Int64)
		}
		var conc *int
		if hasConc && concV.Valid {
			v := int(concV.Int64)
			conc = &v
		}
		enabled := true
		if hasEnabled && enabledV.Valid {
			enabled = enabledV.Bool
		}
		out = append(out, keyRow{
			ChannelID: channelID,
			Name:      strings.TrimSpace(name.String),
			APIKey:    strings.TrimSpace(key.String),
			Weight:    weight,
			Conc:      conc,
			Enabled:   enabled,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func ensureProvider(tx *sql.Tx, typ, baseURL string, src channelRow) (id uint64, created bool, err error) {
	if strings.TrimSpace(typ) == "" || strings.TrimSpace(baseURL) == "" {
		return 0, false, fmt.Errorf("provider type/base_url required")
	}
	var existingID uint64
	var existingMM []byte
	err = tx.QueryRow(`SELECT id, model_map_json FROM providers WHERE type=? AND base_url=? LIMIT 1`, typ, baseURL).Scan(&existingID, &existingMM)
	if err == nil {
		if src.HasMM && len(existingMM) == 0 && len(src.ModelMap) > 0 {
			_, _ = tx.Exec(`UPDATE providers SET model_map_json=? WHERE id=?`, src.ModelMap, existingID)
		}
		return existingID, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}

	mm := []byte("null")
	if src.HasMM && len(src.ModelMap) > 0 {
		if json.Valid(src.ModelMap) {
			mm = src.ModelMap
		}
	}
	res, err := tx.Exec(`INSERT INTO providers(type, base_url, default_headers_json, model_map_json, enabled) VALUES (?,?,?,?,?)`,
		typ, baseURL, []byte("null"), mm, true)
	if err != nil {
		return 0, false, err
	}
	id64, _ := res.LastInsertId()
	return uint64(id64), true, nil
}

func credentialExists(tx *sql.Tx, providerID uint64, name string) (bool, error) {
	var id uint64
	err := tx.QueryRow(`SELECT id FROM credentials WHERE provider_id=? AND name=? LIMIT 1`, providerID, name).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return id != 0, nil
}

func ensureDefaultPool(tx *sql.Tx, channelIDs []uint64) (uint64, error) {
	var poolID uint64
	var idsJSON []byte
	err := tx.QueryRow(`SELECT id, channel_ids_json FROM pools WHERE name = 'default' LIMIT 1`).Scan(&poolID, &idsJSON)
	if err == sql.ErrNoRows {
		idsJSON, _ = json.Marshal(uniqueUint64(channelIDs))
		res, err := tx.Exec(`INSERT INTO pools(name, strategy, channel_ids_json, enabled) VALUES (?,?,?,?)`, "default", "weighted_rr", idsJSON, true)
		if err != nil {
			return 0, err
		}
		id64, _ := res.LastInsertId()
		return uint64(id64), nil
	}
	if err != nil {
		return 0, err
	}

	var existing []uint64
	_ = json.Unmarshal(idsJSON, &existing)
	merged := uniqueUint64(append(existing, channelIDs...))
	newJSON, _ := json.Marshal(merged)
	if _, err := tx.Exec(`UPDATE pools SET channel_ids_json = ? WHERE id = ?`, newJSON, poolID); err != nil {
		return 0, err
	}
	return poolID, nil
}

func ensureDefaultRule(tx *sql.Tx, poolID uint64) error {
	var id uint64
	err := tx.QueryRow(`SELECT id FROM routing_rules WHERE pool_id = ? AND JSON_LENGTH(match_json) = 0 LIMIT 1`, poolID).Scan(&id)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec(`INSERT INTO routing_rules(priority, match_json, pool_id, fallback_pool_id, enabled) VALUES (?,?,?,?,?)`,
		9999, []byte(`{}`), poolID, nil, true)
	return err
}

func uniqueUint64(in []uint64) []uint64 {
	seen := map[uint64]bool{}
	out := make([]uint64, 0, len(in))
	for _, v := range in {
		if v == 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func last4(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return s
	}
	return s[len(s)-4:]
}
