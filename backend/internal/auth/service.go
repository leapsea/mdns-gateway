package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

type Session struct {
	UserID       int64     `json:"userId"`
	AuthEpoch    int64     `json:"-"`
	Username     string    `json:"username"`
	CreatedAt    time.Time `json:"createdAt"`
	LastActivity time.Time `json:"lastActivity"`
	ExpiresAt    time.Time `json:"expiresAt"`
}
type pendingSetup struct {
	userID           int64
	username, secret string
	expires          time.Time
}
type Service struct {
	DB       *sql.DB
	key      []byte
	ttl      time.Duration
	mu       sync.Mutex
	banMu    sync.Mutex
	sessions map[string]Session
	pending  map[string]pendingSetup
}

func New(db *sql.DB, dataDir string, ttl time.Duration) (*Service, error) {
	key, err := loadKey(dataDir)
	if err != nil {
		return nil, err
	}
	return &Service{DB: db, key: key, ttl: ttl, sessions: map[string]Session{}, pending: map[string]pendingSetup{}}, nil
}

// loadKey 将主密钥放入持久卷并限制文件权限；丢失主密钥时无法解密现有 TOTP。
func loadKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, "master.key")
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != 32 {
			return nil, errors.New("invalid master key")
		}
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Service) seal(clear string) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, []byte(clear), nil), nil
}
func (s *Service) open(ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < g.NonceSize() {
		return "", errors.New("short secret")
	}
	plain, err := g.Open(nil, ciphertext[:g.NonceSize()], ciphertext[g.NonceSize():], nil)
	return string(plain), err
}

func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n == 0, err
}

func (s *Service) NeedsCLIRebind(ctx context.Context) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE totp_enabled=0 AND cli_rebind_allowed=1").Scan(&n)
	return n > 0, err
}

// BeginSetup 仅在无用户时开放创建；并发首设由数据库唯一索引约束。
func (s *Service) BeginSetup(ctx context.Context, username string) (string, string, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 {
		return "", "", errors.New("AUTH_INVALID")
	}
	needs, err := s.NeedsSetup(ctx)
	if err != nil {
		return "", "", err
	}
	if !needs {
		return "", "", errors.New("AUTH_REQUIRED")
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "mDNS Gateway", AccountName: username, Algorithm: otp.AlgorithmSHA1, Digits: otp.DigitsSix, Period: 30})
	if err != nil {
		return "", "", err
	}
	id, err := randomToken(24)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.pending[id] = pendingSetup{username: username, secret: key.Secret(), expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return id, key.URL(), nil
}

func (s *Service) CompleteSetup(ctx context.Context, setupID, code string) (string, []string, error) {
	s.mu.Lock()
	p, ok := s.pending[setupID]
	if ok && time.Now().After(p.expires) {
		delete(s.pending, setupID)
		ok = false
	}
	s.mu.Unlock()
	if !ok || !validCode(p.secret, code) {
		return "", nil, errors.New("TOTP_INVALID")
	}
	needs, err := s.NeedsSetup(ctx)
	if err != nil {
		return "", nil, err
	}
	if !needs {
		return "", nil, errors.New("AUTH_REQUIRED")
	}
	sealed, err := s.seal(p.secret)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.DB.ExecContext(ctx, "INSERT INTO users(username,totp_secret,totp_enabled,created_at,updated_at) VALUES(?,?,1,?,?)", p.username, sealed, now, now)
	if err != nil {
		return "", nil, err
	}
	userID, _ := result.LastInsertId()
	codes, err := s.makeRecovery(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	delete(s.pending, setupID)
	s.mu.Unlock()
	return s.newSession(userID, p.username, 0), codes, nil
}

func validCode(secret, code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return totp.Validate(code, secret)
}

func (s *Service) Login(ctx context.Context, username, code, ip string) (string, error) {
	blocked, err := s.IsBlocked(ctx, ip)
	if err != nil {
		return "", err
	}
	if blocked {
		return "", errors.New("AUTH_BANNED")
	}
	var id, authEpoch int64
	var sealed []byte
	var enabled bool
	err = s.DB.QueryRowContext(ctx, "SELECT id,totp_secret,totp_enabled,auth_epoch FROM users WHERE username=?", username).Scan(&id, &sealed, &enabled, &authEpoch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	valid := false
	if err == nil && enabled {
		secret, openErr := s.open(sealed)
		if openErr != nil {
			return "", openErr
		}
		valid = validCode(secret, code)
	}
	if !valid {
		_ = s.recordFailure(ctx, username, ip)
		return "", errors.New("AUTH_INVALID")
	}
	if err := s.event(ctx, username, ip, true, "LOGIN_SUCCESS"); err != nil {
		return "", err
	}
	return s.newSession(id, username, authEpoch), nil
}

func (s *Service) newSession(userID int64, username string, authEpoch int64) string {
	token, _ := randomToken(32)
	now := time.Now()
	s.mu.Lock()
	s.sessions[token] = Session{userID, authEpoch, username, now, now, now.Add(s.ttl)}
	s.mu.Unlock()
	return token
}
func (s *Service) GetSession(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[token]
	if !ok {
		return Session{}, false
	}
	if time.Now().After(session.ExpiresAt) {
		delete(s.sessions, token)
		return Session{}, false
	}
	session.LastActivity = time.Now()
	s.sessions[token] = session
	return session, true
}

// ValidateSession 对照数据库认证代数，使独立 CLI 进程执行的重置立即撤销旧会话。
func (s *Service) ValidateSession(ctx context.Context, token string) (Session, bool, error) {
	session, ok := s.GetSession(token)
	if !ok {
		return Session{}, false, nil
	}
	var epoch int64
	var enabled bool
	err := s.DB.QueryRowContext(ctx, "SELECT auth_epoch,totp_enabled FROM users WHERE id=?", session.UserID).Scan(&epoch, &enabled)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!enabled || epoch != session.AuthEpoch) {
		s.Logout(token)
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	return session, true, nil
}
func (s *Service) Logout(token string) { s.mu.Lock(); delete(s.sessions, token); s.mu.Unlock() }

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func hashCode(code string) []byte {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.ReplaceAll(code, "-", ""))))
	return sum[:]
}

func (s *Service) makeRecovery(ctx context.Context, userID int64) ([]string, error) {
	codes := make([]string, 8)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range codes {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		v := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		codes[i] = v[:4] + "-" + v[4:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO recovery_codes(user_id,code_hash,created_at) VALUES(?,?,?)", userID, hashCode(codes[i]), now); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// Recover 消耗单次恢复码，并使原 TOTP 失效。之后须重新绑定。
func (s *Service) Recover(ctx context.Context, username, code, ip string) (string, error) {
	var id int64
	if err := s.DB.QueryRowContext(ctx, "SELECT id FROM users WHERE username=?", username).Scan(&id); err != nil {
		return "", errors.New("RECOVERY_CODE_INVALID")
	}
	h := hashCode(code)
	token, err := randomToken(24)
	if err != nil {
		return "", err
	}
	tokenHash := sha256.Sum256([]byte(token))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE recovery_codes SET used_at=? WHERE user_id=? AND code_hash=? AND used_at IS NULL", now, id, h)
	if err != nil {
		return "", err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", errors.New("RECOVERY_CODE_INVALID")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET totp_secret=NULL,totp_enabled=0,cli_rebind_allowed=0,auth_epoch=auth_epoch+1,updated_at=? WHERE id=?", now, id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM rebind_tokens WHERE user_id=?", id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO rebind_tokens(token_hash,user_id,expires_at) VALUES(?,?,?)", tokenHash[:], id, time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.invalidateUser(id)
	_ = s.event(ctx, username, ip, true, "TOTP_RESET")
	return token, nil
}

func (s *Service) Reset2FA(ctx context.Context, username string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE users SET totp_secret=NULL,totp_enabled=0,cli_rebind_allowed=1,auth_epoch=auth_epoch+1,updated_at=? WHERE username=?", now, username)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE username=?", username).Scan(&id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM rebind_tokens WHERE user_id=?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateUser(id)
	_ = s.event(ctx, username, "local", true, "TOTP_RESET")
	return nil
}
func (s *Service) invalidateUser(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, v := range s.sessions {
		if v.UserID == id {
			delete(s.sessions, token)
		}
	}
}

func (s *Service) BeginRebind(ctx context.Context, username, rebindToken string) (string, string, error) {
	var id int64
	var enabled, cliAllowed bool
	if err := s.DB.QueryRowContext(ctx, "SELECT id,totp_enabled,cli_rebind_allowed FROM users WHERE username=?", username).Scan(&id, &enabled, &cliAllowed); err != nil {
		return "", "", err
	}
	if enabled {
		return "", "", errors.New("TOTP_NOT_CONFIGURED")
	}
	if !cliAllowed {
		h := sha256.Sum256([]byte(rebindToken))
		now := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := s.DB.ExecContext(ctx, "UPDATE rebind_tokens SET used_at=? WHERE token_hash=? AND user_id=? AND used_at IS NULL AND expires_at>?", now, h[:], id, now)
		if err != nil {
			return "", "", err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return "", "", errors.New("AUTH_INVALID")
		}
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "mDNS Gateway", AccountName: username, Algorithm: otp.AlgorithmSHA1, Digits: otp.DigitsSix, Period: 30})
	if err != nil {
		return "", "", err
	}
	token, _ := randomToken(24)
	s.mu.Lock()
	s.pending[token] = pendingSetup{userID: id, username: username, secret: key.Secret(), expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return token, key.URL(), nil
}
func (s *Service) CompleteRebind(ctx context.Context, setupID, code string) (string, []string, error) {
	s.mu.Lock()
	p, ok := s.pending[setupID]
	if ok && time.Now().After(p.expires) {
		delete(s.pending, setupID)
		ok = false
	}
	s.mu.Unlock()
	if !ok || p.userID == 0 || !validCode(p.secret, code) {
		return "", nil, errors.New("TOTP_INVALID")
	}
	sealed, err := s.seal(p.secret)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.DB.ExecContext(ctx, "UPDATE users SET totp_secret=?,totp_enabled=1,cli_rebind_allowed=0,auth_epoch=auth_epoch+1,updated_at=? WHERE id=? AND totp_enabled=0", sealed, now, p.userID)
	if err != nil {
		return "", nil, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return "", nil, errors.New("AUTH_INVALID")
	}
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=?", p.userID); err != nil {
		return "", nil, err
	}
	codes, err := s.makeRecovery(ctx, p.userID)
	if err != nil {
		return "", nil, err
	}
	var epoch int64
	if err := s.DB.QueryRowContext(ctx, "SELECT auth_epoch FROM users WHERE id=?", p.userID).Scan(&epoch); err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	delete(s.pending, setupID)
	s.mu.Unlock()
	return s.newSession(p.userID, p.username, epoch), codes, nil
}

func (s *Service) event(ctx context.Context, username, ip string, success bool, reason string) error {
	slog.Info("authentication event", "username", username, "ip", ip, "success", success, "reason", reason)
	_, err := s.DB.ExecContext(ctx, "INSERT INTO auth_events(username,ip,success,reason,created_at) VALUES(?,?,?,?,?)", username, ip, success, reason, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// IsBlocked 同时清理超过七天无失败的历史；成功登录不重置惩罚等级。
func (s *Service) IsBlocked(ctx context.Context, ip string) (bool, error) {
	s.banMu.Lock()
	defer s.banMu.Unlock()
	var last, banned sql.NullString
	var count, level int
	err := s.DB.QueryRowContext(ctx, "SELECT failure_count,ban_level,last_failure_at,banned_until FROM ip_blocks WHERE ip=?", ip).Scan(&count, &level, &last, &banned)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if last.Valid {
		t, _ := time.Parse(time.RFC3339Nano, last.String)
		if time.Since(t) >= 7*24*time.Hour {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_, err = s.DB.ExecContext(ctx, "UPDATE ip_blocks SET failure_count=0,ban_level=0,banned_until=NULL,updated_at=? WHERE ip=?", now, ip)
			if err == nil {
				_ = s.event(ctx, "", ip, true, "IP_RESET")
			}
			return false, err
		}
	}
	if !banned.Valid {
		return false, nil
	}
	until, _ := time.Parse(time.RFC3339Nano, banned.String)
	return time.Now().Before(until), nil
}

func (s *Service) recordFailure(ctx context.Context, username, ip string) error {
	s.banMu.Lock()
	defer s.banMu.Unlock()
	var count, level int
	err := s.DB.QueryRowContext(ctx, "SELECT failure_count,ban_level FROM ip_blocks WHERE ip=?", ip).Scan(&count, &level)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	count++
	now := time.Now().UTC()
	var until any
	if count >= 2 {
		count = 0
		level++
		duration := 10 * time.Minute
		if level > 1 {
			duration = 30 * time.Minute
		}
		for i := 3; i <= level && duration < 7*24*time.Hour; i++ {
			duration *= 2
		}
		if duration > 7*24*time.Hour {
			duration = 7 * 24 * time.Hour
		}
		until = now.Add(duration).Format(time.RFC3339Nano)
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO ip_blocks(ip,failure_count,ban_level,last_failure_at,banned_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(ip) DO UPDATE SET failure_count=excluded.failure_count,ban_level=excluded.ban_level,last_failure_at=excluded.last_failure_at,banned_until=excluded.banned_until,updated_at=excluded.updated_at`, ip, count, level, now.Format(time.RFC3339Nano), until, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if err := s.event(ctx, username, ip, false, "LOGIN_FAILED"); err != nil {
		return err
	}
	if until != nil {
		return s.event(ctx, username, ip, false, "IP_BANNED")
	}
	return nil
}

// RecordFailure 将恢复凭据失败纳入同一 IP 封禁规则。
func (s *Service) RecordFailure(ctx context.Context, username, ip string) error {
	return s.recordFailure(ctx, username, ip)
}

func (s *Service) UserBinding(ctx context.Context, username string) (bool, error) {
	var enabled bool
	err := s.DB.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE username=?", username).Scan(&enabled)
	return enabled, err
}
func (s *Service) DebugState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("sessions=%d", len(s.sessions))
}
