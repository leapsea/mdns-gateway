package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshDatabaseCanReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO users(username,totp_enabled,created_at,updated_at) VALUES('admin',0,'now','now')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var username string
	var allowed, epoch int
	if err := db.QueryRow("SELECT username,cli_rebind_allowed,auth_epoch FROM users WHERE username='admin'").Scan(&username, &allowed, &epoch); err != nil {
		t.Fatal(err)
	}
	if username != "admin" || allowed != 0 || epoch != 0 {
		t.Fatalf("reopen changed user: %s %d %d", username, allowed, epoch)
	}
}

func TestIncompatibleSchemaFailsClearly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT NOT NULL); INSERT INTO users VALUES (1,'admin')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if db, err = Open(ctx, dir); err == nil {
		_ = db.Close()
		t.Fatal("incompatible schema accepted")
	} else if !strings.Contains(err.Error(), "database schema is incompatible") {
		t.Fatalf("unclear schema error: %v", err)
	}
}
