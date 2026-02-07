package db

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func Migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
  version VARCHAR(255) PRIMARY KEY,
  applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		versions = append(versions, e.Name())
	}
	sort.Strings(versions)

	for _, v := range versions {
		applied, err := hasMigration(db, v)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		raw, err := migrationFS.ReadFile("migrations/" + v)
		if err != nil {
			return err
		}

		tx, err := db.Begin()
		if err != nil {
			return err
		}

		if err := execSQLScript(tx, string(raw)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, v); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

func hasMigration(db *sql.DB, version string) (bool, error) {
	var v string
	err := db.QueryRow(`SELECT version FROM schema_migrations WHERE version = ?`, version).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == version, nil
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func execSQLScript(ex execer, script string) error {
	stmts := splitSQLStatements(script)
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := ex.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func splitSQLStatements(script string) []string {
	var (
		out          []string
		start        int
		inSingle     bool
		inDouble     bool
		inBacktick   bool
		escape       bool
		runes        = []rune(script)
		previousRune rune
	)

	flush := func(end int) {
		if end <= start {
			start = end
			return
		}
		out = append(out, string(runes[start:end]))
		start = end
	}

	for i, r := range runes {
		if escape {
			escape = false
			previousRune = r
			continue
		}
		if r == '\\' && (inSingle || inDouble) {
			escape = true
			previousRune = r
			continue
		}

		switch r {
		case '\'':
			if !inDouble && !inBacktick {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && !inBacktick {
				inDouble = !inDouble
			}
		case '`':
			if !inSingle && !inDouble {
				inBacktick = !inBacktick
			}
		case ';':
			if !inSingle && !inDouble && !inBacktick {
				flush(i + 1)
			}
		}

		previousRune = r
		_ = previousRune
	}

	flush(len(runes))
	return out
}
