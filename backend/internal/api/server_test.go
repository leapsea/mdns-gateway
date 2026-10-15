package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"mdns-gateway/internal/auth"
	"mdns-gateway/internal/database"
	"mdns-gateway/internal/mdns"
	"mdns-gateway/internal/openresty"
	"mdns-gateway/internal/sites"
)

type dashboardMDNS struct{}

func (dashboardMDNS) Probe(context.Context, string) (bool, error) { return false, nil }
func (dashboardMDNS) Replace(context.Context, []sites.Site) error { return nil }
func (dashboardMDNS) Status() string                              { return "OK" }

type dashboardPublisher struct{}

func (dashboardPublisher) BuildCandidate([]sites.Site) (string, error) { return "candidate", nil }
func (dashboardPublisher) Validate(context.Context, string) error      { return nil }
func (dashboardPublisher) Activate(context.Context, string) (string, error) {
	return "test-release", nil
}
func (dashboardPublisher) ActiveVersion() string { return "test-release" }

// 使用真实认证和 SQLite，仅替换会访问网络或启动外部进程的发布边界。
func dashboardServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authService, err := auth.New(db, dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO sites(id,name,domain,target_host,target_port,protocol,enabled,config_mode,created_at,updated_at) VALUES(1,'Media','media.local','192.0.2.10',2030,'HTTP',1,'CUSTOM','now','now');
		INSERT INTO site_configs(site_id,custom_config,updated_at) VALUES(1,'private-upstream-credential','now')`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Auth: authService, DB: db, MDNS: &mdns.Service{},
		Sites: sites.NewService(db, dashboardMDNS{}, dashboardPublisher{}, ":8080"),
		Proxy: openresty.New(dir+"/openresty", "unused-openresty"),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s, s.Handler()
}

func dashboardRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/api/v1"+path, bytes.NewBufferString(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func dashboardLogin(t *testing.T, s *Server) string {
	t.Helper()
	id, uri, err := s.Auth.BeginSetup(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(parsed.Query().Get("secret"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.Auth.CompleteSetup(context.Background(), id, code)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestDashboardGuestCanReadWithoutConfigContents(t *testing.T) {
	_, handler := dashboardServer(t)
	for _, path := range []string{"/sites", "/system/status", "/system/mdns", "/system/openresty", "/system/version"} {
		response := dashboardRequest(handler, http.MethodGet, path, "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("guest GET %s: %d %s", path, response.Code, response.Body)
		}
	}
	response := dashboardRequest(handler, http.MethodGet, "/sites", "", "")
	if !strings.Contains(response.Body.String(), "media.local") || strings.Contains(response.Body.String(), "private-upstream-credential") || strings.Contains(response.Body.String(), "customConfig") {
		t.Fatalf("incorrect public site projection: %s", response.Body)
	}
}

func TestDashboardGuestCannotEditOrReadPrivateData(t *testing.T) {
	_, handler := dashboardServer(t)
	// 列表可读不代表配置和写入端点开放，过期令牌也不获得编辑权限。
	for _, token := range []string{"", "expired-session"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/sites"}, {http.MethodPut, "/sites/1"}, {http.MethodDelete, "/sites/1"},
			{http.MethodPost, "/sites/config/preview"}, {http.MethodGet, "/sites/1"},
			{http.MethodGet, "/sites/1/config"}, {http.MethodPut, "/sites/1/config"},
			{http.MethodPost, "/sites/1/config/validate"}, {http.MethodPost, "/sites/1/config/reset"},
			{http.MethodGet, "/auth/me"},
		} {
			response := dashboardRequest(handler, route.method, route.path, token, "{}")
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("guest %s %s: %d %s", route.method, route.path, response.Code, response.Body)
			}
		}
	}
}

func TestDashboardLoginGrantsEditingAndResetRevokesIt(t *testing.T) {
	s, handler := dashboardServer(t)
	token := dashboardLogin(t, s)
	response := dashboardRequest(handler, http.MethodGet, "/auth/me", token, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"username":"admin"`) {
		t.Fatalf("missing account information: %d %s", response.Code, response.Body)
	}
	response = dashboardRequest(handler, http.MethodGet, "/sites/1", token, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "private-upstream-credential") {
		t.Fatalf("authenticated editor cannot read config: %d %s", response.Code, response.Body)
	}
	body, _ := json.Marshal(sites.Site{Name: "Added", Domain: "added.local", TargetHost: "192.0.2.11", TargetPort: 8096, Protocol: "HTTP", Enabled: true, ConfigMode: "GENERATED"})
	response = dashboardRequest(handler, http.MethodPost, "/sites", token, string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated write: %d %s", response.Code, response.Body)
	}
	if err := s.Auth.Reset2FA(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	response = dashboardRequest(handler, http.MethodDelete, "/sites/1", token, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session retained write access: %d %s", response.Code, response.Body)
	}
	response = dashboardRequest(handler, http.MethodGet, "/sites", token, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "added.local") {
		t.Fatalf("revoked session lost public browsing: %d %s", response.Code, response.Body)
	}
}
