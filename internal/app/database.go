package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type migration struct {
	version int
	name    string
	sql     string
}

var databaseMigrations = []migration{
	{
		version: 1,
		name:    "sqlite foundation",
		sql: `CREATE TABLE application_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
	},
}

func openDatabase(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "cortex.db")
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", "rwc")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	q.Set("_defensive", "true")
	q.Set("_dqs", "false")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetConnMaxLifetime(0)
	db.SetMaxIdleConns(4)
	db.SetMaxOpenConns(8)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open Cortex database: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect Cortex database: %w", err)
	}
	if err := migrateDatabase(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrateDatabase(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read database schema: %w", err)
	}
	if current > len(databaseMigrations) {
		return fmt.Errorf("Cortex database schema %d is newer than supported schema %d", current, len(databaseMigrations))
	}
	for _, m := range databaseMigrations {
		if m.version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err = tx.ExecContext(ctx, m.sql); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)", m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano))
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}
