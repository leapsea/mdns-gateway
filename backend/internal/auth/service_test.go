package auth

import (
	"context"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"mdns-gateway/internal/database"
)

func TestBanScheduleAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ip := "192.0.2.1"
	for level, want := range []time.Duration{10 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour} {
		for i := 0; i < 2; i++ {
			if err := s.recordFailure(ctx, "admin", ip); err != nil {
				t.Fatal(err)
			}
		}
		var until string
		if err := db.QueryRow("SELECT banned_until FROM ip_blocks WHERE ip=?", ip).Scan(&until); err != nil {
			t.Fatal(err)
		}
		got, _ := time.Parse(time.RFC3339Nano, until)
		if delta := time.Until(got) - want; delta < -time.Second || delta > time.Second {
			t.Fatalf("level %d: got %v want %v", level+1, time.Until(got), want)
		}
	}
	_, err = db.Exec("UPDATE ip_blocks SET last_failure_at=? WHERE ip=?", time.Now().Add(-8*24*time.Hour).UTC().Format(time.RFC3339Nano), ip)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.IsBlocked(ctx, ip)
	if err != nil || blocked {
		t.Fatalf("not reset: %v %v", blocked, err)
	}
	var level int
	if err := db.QueryRow("SELECT ban_level FROM ip_blocks WHERE ip=?", ip).Scan(&level); err != nil || level != 0 {
		t.Fatalf("level %d, %v", level, err)
	}
}

func TestSessionIsMemoryOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, _ := New(db, dir, time.Hour)
	token := first.newSession(1, "admin", 0)
	if _, ok := first.GetSession(token); !ok {
		t.Fatal("new session missing")
	}
	second, _ := New(db, dir, time.Hour)
	if _, ok := second.GetSession(token); ok {
		t.Fatal("session survived restart")
	}
}

func TestSessionExpiry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, dir, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	token := s.newSession(1, "admin", 0)
	if _, ok := s.GetSession(token); ok {
		t.Fatal("expired session accepted")
	}
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO users(id,username,totp_enabled,created_at,updated_at) VALUES(1,'admin',1,'now','now')")
	if err != nil {
		t.Fatal(err)
	}
	codes, err := s.makeRecovery(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Recover(ctx, "admin", codes[0], "192.0.2.1")
	if err != nil || token == "" {
		t.Fatalf("recover: %v", err)
	}
	if _, err := s.Recover(ctx, "admin", codes[0], "192.0.2.1"); err == nil {
		t.Fatal("recovery code reused")
	}
	var enabled bool
	if err := db.QueryRow("SELECT totp_enabled FROM users WHERE id=1").Scan(&enabled); err != nil || enabled {
		t.Fatalf("TOTP still enabled: %v", err)
	}
	if _, _, err := s.BeginRebind(ctx, "admin", ""); err == nil {
		t.Fatal("recovery reset accepted username-only rebind")
	}
	if _, _, err := s.BeginRebind(ctx, "admin", token); err != nil {
		t.Fatalf("recovery rebind token rejected: %v", err)
	}
}

func TestCLIResetRebindAndSessionRevocation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	web, _ := New(db, dir, time.Hour)
	cli, _ := New(db, dir, time.Hour)
	if _, err := db.Exec("INSERT INTO users(id,username,totp_enabled,created_at,updated_at) VALUES(1,'admin',1,'now','now')"); err != nil {
		t.Fatal(err)
	}
	oldToken := web.newSession(1, "admin", 0)
	staleToken := web.newSession(1, "admin", 0)
	if err := cli.Reset2FA(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := web.ValidateSession(ctx, oldToken); err != nil || ok {
		t.Fatalf("old session survived CLI reset: ok=%v err=%v", ok, err)
	}
	if needed, err := web.NeedsCLIRebind(ctx); err != nil || !needed {
		t.Fatalf("CLI rebind not reported: needed=%v err=%v", needed, err)
	}
	setupID, _, err := web.BeginRebind(ctx, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := web.CompleteRebind(ctx, setupID, "123"); err == nil {
		t.Fatal("invalid TOTP code accepted")
	}
	code, err := totp.GenerateCode(web.pending[setupID].secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	newToken, codes, err := web.CompleteRebind(ctx, setupID, code)
	if err != nil || newToken == "" || len(codes) == 0 {
		t.Fatalf("rebind failed: %v", err)
	}
	if _, ok, err := web.ValidateSession(ctx, newToken); err != nil || !ok {
		t.Fatalf("new session invalid: ok=%v err=%v", ok, err)
	}
	if _, ok, err := web.ValidateSession(ctx, staleToken); err != nil || ok {
		t.Fatalf("pre-reset session revived after rebind: ok=%v err=%v", ok, err)
	}
	if needed, err := web.NeedsCLIRebind(ctx); err != nil || needed {
		t.Fatalf("CLI rebind remained open: needed=%v err=%v", needed, err)
	}
}
