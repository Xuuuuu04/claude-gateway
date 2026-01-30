package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	HTTPAddr           string
	MySQLDSN           string
	AdminToken         string
	KeyEncMasterB64    string
	CORSAllowedOrigins []string
}

func FromEnv() (Config, error) {
	httpAddr := getenvDefault("HTTP_ADDR", ":8080")
	mysqlDSN := os.Getenv("MYSQL_DSN")
	if strings.TrimSpace(mysqlDSN) == "" {
		return Config{}, fmt.Errorf("MYSQL_DSN is required")
	}
	adminToken := os.Getenv("ADMIN_TOKEN")
	if strings.TrimSpace(adminToken) == "" {
		return Config{}, fmt.Errorf("ADMIN_TOKEN is required")
	}
	keyEnc := os.Getenv("KEY_ENC_MASTER_B64")
	if strings.TrimSpace(keyEnc) == "" {
		return Config{}, fmt.Errorf("KEY_ENC_MASTER_B64 is required")
	}

	origins := os.Getenv("CORS_ALLOWED_ORIGINS")
	allowed := []string{"*"}
	if strings.TrimSpace(origins) != "" {
		allowed = splitCSV(origins)
	}

	return Config{
		HTTPAddr:           httpAddr,
		MySQLDSN:           mysqlDSN,
		AdminToken:         adminToken,
		KeyEncMasterB64:    keyEnc,
		CORSAllowedOrigins: allowed,
	}, nil
}

func getenvDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"*"}
	}
	return out
}
