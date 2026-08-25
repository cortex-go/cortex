package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabaseFoundationMigratesAndProtectsFile(t *testing.T) {
	dir := t.TempDir()
	db, err := openDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRowContext(context.Background(), "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(databaseMigrations) {
		t.Fatalf("schema version = %d, want %d", version, len(databaseMigrations))
	}
	info, err := os.Stat(filepath.Join(dir, "cortex.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database mode = %o, want 600", info.Mode().Perm())
	}
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, err = %v", foreignKeys, err)
	}
}

func TestDatabaseRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := openDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version, name, applied_at) VALUES(999, 'future', 'now')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := openDatabase(dir); err == nil {
		t.Fatal("newer schema was accepted")
	}
}
