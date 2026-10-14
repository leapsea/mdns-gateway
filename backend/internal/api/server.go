package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mdns-gateway/internal/auth"
	"mdns-gateway/internal/mdns"
	"mdns-gateway/internal/openresty"
	"mdns-gateway/internal/sites"
)

type Server struct {
	Auth  *auth.Service
	Sites *sites.Service
	MDNS  *mdns.Service
	Proxy *openresty.Manager
	DB    *sql.DB
	UI    http.Handler
	Log   *slog.Logger
}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func respond(w http.ResponseWriter, status int, data any, err *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "error": err})
}
func failure(w http.ResponseWriter, err error) {
	code := "INTERNAL_ERROR"
	message := "Internal server error"
	status := http.StatusBadRequest
	if errors.Is(err, sql.ErrNoRows) {
		code = "SITE_NOT_FOUND"
		message = "Site not found"
		status = http.StatusNotFound
	} else {
		for _, known := range []string{"OPENRESTY_CONFIG_INVALID", "OPENRESTY_RELOAD_FAILED", "DOMAIN_EXISTS", "DOMAIN_INVALID", "CONFIG_INVALID", "PORT_IN_USE", "MDNS_CONFLICT", "AUTH_INVALID", "AUTH_REQUIRED", "AUTH_BANNED", "TOTP_INVALID", "TOTP_NOT_CONFIGURED", "RECOVERY_CODE_INVALID"} {
			if strings.HasPrefix(err.Error(), known) {
				code = known
				message = err.Error()
				break
			}
		}
	}
	if code == "INTERNAL_ERROR" {
		status = http.StatusInternalServerError
	}
	if code == "AUTH_REQUIRED" {
		status = http.StatusUnauthorized
	}
	if code == "AUTH_BANNED" {
		status = http.StatusForbidden
	}
	if code == "AUTH_INVALID" {
		status = http.StatusUnauthorized
	}
	respond(w, status, nil, &apiError{code, message})
}
func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func ipOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}, nil) })
	mux.HandleFunc("GET /api/v1/auth/setup-status", func(w http.ResponseWriter, r *http.Request) {
		needs, err := s.Auth.NeedsSetup(r.Context())
		if err != nil {
			failure(w, err)
			return
		}
		rebind, err := s.Auth.NeedsCLIRebind(r.Context())
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]bool{"needsSetup": needs, "needsRebind": rebind}, nil)
	})
	mux.HandleFunc("POST /api/v1/auth/setup", s.setup)
	mux.HandleFunc("POST /api/v1/auth/setup/verify", s.setupVerify)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/recovery", s.recovery)
	mux.HandleFunc("POST /api/v1/auth/2fa/setup", s.rebind)
	mux.HandleFunc("POST /api/v1/auth/2fa/verify", s.rebindVerify)
	mux.HandleFunc("POST /api/v1/auth/logout", s.secure(s.logout))
	mux.HandleFunc("GET /api/v1/auth/me", s.secure(s.me))
	mux.HandleFunc("GET /api/v1/sites", s.secure(s.listSites))
	mux.HandleFunc("POST /api/v1/sites", s.secure(s.saveSite))
	mux.HandleFunc("POST /api/v1/sites/config/preview", s.secure(s.previewConfig))
	mux.HandleFunc("GET /api/v1/sites/{id}", s.secure(s.getSite))
	mux.HandleFunc("PUT /api/v1/sites/{id}", s.secure(s.saveSite))
	mux.HandleFunc("DELETE /api/v1/sites/{id}", s.secure(s.deleteSite))
	mux.HandleFunc("GET /api/v1/sites/{id}/config", s.secure(s.getConfig))
	mux.HandleFunc("PUT /api/v1/sites/{id}/config", s.secure(s.putConfig))
	mux.HandleFunc("POST /api/v1/sites/{id}/config/validate", s.secure(s.validateConfig))
	mux.HandleFunc("POST /api/v1/sites/{id}/config/reset", s.secure(s.resetConfig))
	mux.HandleFunc("GET /api/v1/system/status", s.secure(s.status))
	mux.HandleFunc("GET /api/v1/system/mdns", s.secure(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"status": s.MDNS.Status(), "conflicts": s.MDNS.Conflicts()}, nil)
	}))
	mux.HandleFunc("GET /api/v1/system/openresty", s.secure(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]string{"activeVersion": s.Proxy.ActiveVersion(), "runtime": s.Proxy.RuntimeStatus(), "applyStatus": s.Proxy.ApplyStatus()}, nil)
	}))
	mux.HandleFunc("GET /api/v1/system/version", s.secure(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]string{"version": "v0.1.0"}, nil)
	}))
	if s.UI != nil {
		mux.Handle("/", s.UI)
	}
	return s.logging(mux)
}
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
func (s *Server) secure(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		blocked, err := s.Auth.IsBlocked(r.Context(), ipOf(r))
		if err != nil {
			failure(w, err)
			return
		}
		if blocked {
			failure(w, errors.New("AUTH_BANNED"))
			return
		}
		if _, ok, err := s.Auth.ValidateSession(r.Context(), bearer(r)); err != nil {
			failure(w, err)
			return
		} else if !ok {
			failure(w, errors.New("AUTH_REQUIRED"))
			return
		}
		next(w, r)
	}
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Username string `json:"username"`
	}
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	id, uri, err := s.Auth.BeginSetup(r.Context(), v.Username)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"setupId": id, "otpauthUrl": uri}, nil)
}
func (s *Server) setupVerify(w http.ResponseWriter, r *http.Request) {
	var v struct {
		SetupID string `json:"setupId"`
		Code    string `json:"code"`
	}
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	token, codes, err := s.Auth.CompleteSetup(r.Context(), v.SetupID, v.Code)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"token": token, "recoveryCodes": codes}, nil)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Username string `json:"username"`
		Code     string `json:"code"`
	}
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("AUTH_INVALID"))
		return
	}
	token, err := s.Auth.Login(r.Context(), v.Username, v.Code, ipOf(r))
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"token": token}, nil)
}
func (s *Server) recovery(w http.ResponseWriter, r *http.Request) {
	blocked, checkErr := s.Auth.IsBlocked(r.Context(), ipOf(r))
	if checkErr != nil {
		failure(w, checkErr)
		return
	}
	if blocked {
		failure(w, errors.New("AUTH_BANNED"))
		return
	}
	var v struct {
		Username string `json:"username"`
		Code     string `json:"code"`
	}
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("RECOVERY_CODE_INVALID"))
		return
	}
	token, err := s.Auth.Recover(r.Context(), v.Username, v.Code, ipOf(r))
	if err != nil {
		_ = s.Auth.RecordFailure(r.Context(), v.Username, ipOf(r))
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"rebindToken": token}, nil)
}
func (s *Server) rebind(w http.ResponseWriter, r *http.Request) {
	blocked, checkErr := s.Auth.IsBlocked(r.Context(), ipOf(r))
	if checkErr != nil {
		failure(w, checkErr)
		return
	}
	if blocked {
		failure(w, errors.New("AUTH_BANNED"))
		return
	}
	var v struct {
		Username    string `json:"username"`
		RebindToken string `json:"rebindToken"`
	}
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	id, uri, err := s.Auth.BeginRebind(r.Context(), v.Username, v.RebindToken)
	if err != nil {
		_ = s.Auth.RecordFailure(r.Context(), v.Username, ipOf(r))
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"setupId": id, "otpauthUrl": uri}, nil)
}
func (s *Server) rebindVerify(w http.ResponseWriter, r *http.Request) {
	blocked, checkErr := s.Auth.IsBlocked(r.Context(), ipOf(r))
	if checkErr != nil {
		failure(w, checkErr)
		return
	}
	if blocked {
		failure(w, errors.New("AUTH_BANNED"))
		return
	}
	var v struct {
		SetupID string `json:"setupId"`
		Code    string `json:"code"`
	}
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	token, codes, err := s.Auth.CompleteRebind(r.Context(), v.SetupID, v.Code)
	if err != nil {
		_ = s.Auth.RecordFailure(r.Context(), "", ipOf(r))
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"token": token, "recoveryCodes": codes}, nil)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(bearer(r))
	respond(w, 200, map[string]string{"status": "ok"}, nil)
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	session, _ := s.Auth.GetSession(bearer(r))
	enabled, _ := s.Auth.UserBinding(r.Context(), session.Username)
	respond(w, 200, map[string]any{"session": session, "totpEnabled": enabled}, nil)
}

func siteID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, sql.ErrNoRows
	}
	return id, nil
}
func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sites.List(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, list, nil)
}
func (s *Server) getSite(w http.ResponseWriter, r *http.Request) {
	id, err := siteID(r)
	if err != nil {
		failure(w, err)
		return
	}
	v, err := s.Sites.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, v, nil)
}
func (s *Server) saveSite(w http.ResponseWriter, r *http.Request) {
	var v sites.Site
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	if r.Method == http.MethodPut {
		id, err := siteID(r)
		if err != nil {
			failure(w, err)
			return
		}
		v.ID = id
	} else {
		v.ID = 0
	}
	if v.ConfigMode == "" {
		v.ConfigMode = "GENERATED"
	}
	if v.Protocol == "" {
		v.Protocol = "HTTP"
	}
	result, err := s.Sites.Save(r.Context(), v)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, result, nil)
}
func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	id, err := siteID(r)
	if err != nil {
		failure(w, err)
		return
	}
	if err := s.Sites.Delete(r.Context(), id); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"status": "deleted"}, nil)
}
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	id, err := siteID(r)
	if err != nil {
		failure(w, err)
		return
	}
	v, err := s.Sites.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"configMode": v.ConfigMode, "customConfig": v.CustomConfig}, nil)
}
func (s *Server) previewConfig(w http.ResponseWriter, r *http.Request) {
	var v sites.Site
	if err := decode(r, &v); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	v.ConfigMode = "GENERATED"
	if err := v.Validate(); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]string{"config": openresty.GenerateServer(v)}, nil)
}
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	id, err := siteID(r)
	if err != nil {
		failure(w, err)
		return
	}
	v, err := s.Sites.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	var input struct {
		CustomConfig string `json:"customConfig"`
	}
	if err := decode(r, &input); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	v.ConfigMode = "CUSTOM"
	v.CustomConfig = input.CustomConfig
	result, err := s.Sites.Save(r.Context(), v)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, result, nil)
}
func (s *Server) validateConfig(w http.ResponseWriter, r *http.Request) {
	id, err := siteID(r)
	if err != nil {
		failure(w, err)
		return
	}
	v, err := s.Sites.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	var input struct {
		CustomConfig string `json:"customConfig"`
	}
	if err := decode(r, &input); err != nil {
		failure(w, errors.New("CONFIG_INVALID"))
		return
	}
	v.ConfigMode = "CUSTOM"
	v.CustomConfig = input.CustomConfig
	state, err := s.Sites.List(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	for i := range state {
		if state[i].ID == id {
			state[i] = v
		}
	}
	candidate, err := s.Proxy.BuildCandidate(state)
	if err == nil {
		err = s.Proxy.Validate(r.Context(), candidate)
	}
	if err != nil {
		failure(w, errors.New("OPENRESTY_CONFIG_INVALID: "+err.Error()))
		return
	}
	respond(w, 200, map[string]bool{"valid": true}, nil)
}
func (s *Server) resetConfig(w http.ResponseWriter, r *http.Request) {
	id, err := siteID(r)
	if err != nil {
		failure(w, err)
		return
	}
	v, err := s.Sites.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	v.ConfigMode = "GENERATED"
	v.CustomConfig = ""
	result, err := s.Sites.Save(r.Context(), v)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, result, nil)
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var desired, activeDesired int64
	var active string
	err := s.DB.QueryRowContext(r.Context(), "SELECT desired_version,active_desired_version,active_release FROM system_config WHERE id=1").Scan(&desired, &activeDesired, &active)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"version": "v0.1.0", "database": "OK", "mdns": s.MDNS.Status(), "openrestyActive": s.Proxy.ActiveVersion(), "openrestyRuntime": s.Proxy.RuntimeStatus(), "openrestyApply": s.Proxy.ApplyStatus(), "desiredVersion": desired, "activeRelease": active, "configPending": desired != activeDesired || (active != "" && active != s.Proxy.ActiveVersion())}, nil)
}
func Shutdown(ctx context.Context, server *http.Server) error { return server.Shutdown(ctx) }
