package database

import (
	"context"
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
)

// Open 初始化本地 SQLite。关联由 Service 维护，不设外键或触发器。
func Open(ctx context.Context, dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, statement := range schema {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	// 只接受当前表结构；旧库由部署者明确移走后重新初始化。
	for _, query := range []string{
		"SELECT id,username,totp_enabled,cli_rebind_allowed,auth_epoch FROM users LIMIT 0",
		"SELECT id,domain,target_host,target_port,listen_port,protocol FROM sites LIMIT 0",
	} {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			_ = tx.Rollback()
			db.Close()
			return nil, fmt.Errorf("database schema is incompatible; move gateway.db aside to initialize a new database: %w", err)
		}
		_ = rows.Close()
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, username TEXT NOT NULL UNIQUE, totp_secret BLOB, totp_enabled INTEGER NOT NULL DEFAULT 0, cli_rebind_allowed INTEGER NOT NULL DEFAULT 0, auth_epoch INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS recovery_codes (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, code_hash BLOB NOT NULL, used_at TEXT, created_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS recovery_codes_user_idx ON recovery_codes(user_id)`,
	`CREATE TABLE IF NOT EXISTS rebind_tokens (token_hash BLOB PRIMARY KEY, user_id INTEGER NOT NULL, expires_at TEXT NOT NULL, used_at TEXT)`,
	`CREATE TABLE IF NOT EXISTS auth_events (id INTEGER PRIMARY KEY, username TEXT, ip TEXT NOT NULL, success INTEGER NOT NULL, reason TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS ip_blocks (ip TEXT PRIMARY KEY, failure_count INTEGER NOT NULL, ban_level INTEGER NOT NULL, last_failure_at TEXT, banned_until TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sites (id INTEGER PRIMARY KEY, name TEXT NOT NULL, domain TEXT NOT NULL UNIQUE, target_host TEXT NOT NULL, target_port INTEGER NOT NULL, listen_port INTEGER NOT NULL DEFAULT 0, protocol TEXT NOT NULL, enabled INTEGER NOT NULL, config_mode TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS site_configs (id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL UNIQUE, custom_config TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS system_config (id INTEGER PRIMARY KEY CHECK(id=1), desired_version INTEGER NOT NULL DEFAULT 0, active_desired_version INTEGER NOT NULL DEFAULT 0, active_release TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL)`,
	`INSERT OR IGNORE INTO system_config(id, desired_version, active_desired_version, active_release, updated_at) VALUES(1,0,0,'',datetime('now'))`,
}
