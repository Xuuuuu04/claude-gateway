package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"claude-gateway/internal/crypto"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	dsn := "root:xsy19507@tcp(8.155.162.119:3306)/claude_gateway?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci&tls=skip-verify"
	masterKeyB64 := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	cipher, err := crypto.NewAESGCMFromBase64Key(masterKeyB64)
	if err != nil {
		log.Fatal(err)
	}

	file, err := os.Open("migration_data.tsv")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Skip header
	if scanner.Scan() {
	}

	var channelIDs []uint64

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}

		// id, type, name, base_url, key, models, weight
		// 0   1     2     3         4    5       6
		idStr := parts[0]
		typeStr := parts[1]
		name := parts[2]
		baseURL := parts[3]
		apiKey := parts[4]
		modelsStr := parts[5]
		weightStr := parts[6]

		oneapiType, _ := strconv.Atoi(typeStr)
		providerType := "openai"
		switch oneapiType {
		case 14:
			providerType = "anthropic"
		case 17:
			providerType = "gemini"
		default:
			providerType = "openai"
		}

		if baseURL == "" {
			if providerType == "anthropic" {
				baseURL = "https://api.anthropic.com"
			} else if providerType == "openai" {
				baseURL = "https://api.openai.com"
			} else if providerType == "gemini" {
				baseURL = "https://generativelanguage.googleapis.com"
			}
		}

		// 1. Provider
		var providerID int64
		err = db.QueryRow("SELECT id FROM providers WHERE type = ? AND base_url = ?", providerType, baseURL).Scan(&providerID)
		if err == sql.ErrNoRows {
			res, err := db.Exec("INSERT INTO providers (type, base_url, default_headers_json, enabled) VALUES (?, ?, ?, ?)",
				providerType, baseURL, []byte("null"), 1)
			if err != nil {
				log.Printf("Failed to insert provider %s: %v", name, err)
				continue
			}
			providerID, _ = res.LastInsertId()
		}

		// 2. Credential
		ciphertext, err := cipher.Encrypt([]byte(apiKey))
		if err != nil {
			log.Printf("Failed to encrypt key for %s: %v", name, err)
			continue
		}
		last4 := ""
		if len(apiKey) > 4 {
			last4 = apiKey[len(apiKey)-4:]
		} else {
			last4 = apiKey
		}
		weight, _ := strconv.Atoi(weightStr)
		if weight <= 0 {
			weight = 1
		}

		res, err := db.Exec("INSERT INTO credentials (provider_id, name, api_key_ciphertext, key_last4, weight, enabled) VALUES (?, ?, ?, ?, ?, ?)",
			providerID, name+"-"+idStr, ciphertext, last4, weight, 1)
		if err != nil {
			log.Printf("Failed to insert credential %s: %v", name, err)
			continue
		}
		credentialID, _ := res.LastInsertId()

		// 3. Channel
		// Prepare model map if there are specific model mappings (optional)
		// For now, we'll just use the name
		res, err = db.Exec("INSERT INTO channels (credential_id, name, tags_json, model_map_json, extra_headers_json, enabled) VALUES (?, ?, ?, ?, ?, ?)",
			credentialID, name+"-"+idStr, []byte("null"), []byte("null"), []byte("null"), 1)
		if err != nil {
			log.Printf("Failed to insert channel %s: %v", name, err)
			continue
		}
		channelID, _ := res.LastInsertId()
		channelIDs = append(channelIDs, uint64(channelID))
	}

	if len(channelIDs) > 0 {
		// 4. Pool
		idsJSON, _ := json.Marshal(channelIDs)
		var poolID int64
		err = db.QueryRow("SELECT id FROM pools WHERE name = 'default'").Scan(&poolID)
		if err == sql.ErrNoRows {
			res, err := db.Exec("INSERT INTO pools (name, strategy, channel_ids_json, enabled) VALUES (?, ?, ?, ?)",
				"default", "round_robin", idsJSON, 1)
			if err != nil {
				log.Printf("Failed to insert pool: %v", err)
			} else {
				poolID, _ = res.LastInsertId()
			}
		} else {
			_, err = db.Exec("UPDATE pools SET channel_ids_json = ? WHERE id = ?", idsJSON, poolID)
			if err != nil {
				log.Printf("Failed to update pool: %v", err)
			}
		}

		// 5. Rule
		var ruleCount int
		_ = db.QueryRow("SELECT COUNT(*) FROM routing_rules").Scan(&ruleCount)
		if ruleCount == 0 && poolID > 0 {
			_, err = db.Exec("INSERT INTO routing_rules (priority, match_json, pool_id, enabled) VALUES (?, ?, ?, ?)",
				10, []byte("{}"), poolID, 1)
			if err != nil {
				log.Printf("Failed to insert rule: %v", err)
			}
		}
	}

	fmt.Printf("Successfully migrated %d channels\n", len(channelIDs))
}
