package sites

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// MDNS 隔离局域网状态；配置验证失败时绝不更改当前注册。
type MDNS interface {
	Probe(context.Context, string) (bool, error)
	Replace(context.Context, []Site) error
	Status() string
}
type Publisher interface {
	BuildCandidate([]Site) (string, error)
	Validate(context.Context, string) error
	Activate(context.Context, string) (string, error)
	ActiveVersion() string
}

type Service struct {
	DB              *sql.DB
	MDNS            MDNS
	Proxy           Publisher
	AdminListenAddr string
	portAvailable   func(int) bool
	mu              sync.Mutex
}

func NewService(db *sql.DB, mdns MDNS, proxy Publisher, adminListenAddr string) *Service {
	return &Service{DB: db, MDNS: mdns, Proxy: proxy, AdminListenAddr: adminListenAddr, portAvailable: canListenTCP}
}

func canListenTCP(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func (s *Service) List(ctx context.Context) ([]Site, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT s.id,s.name,s.domain,s.target_host,s.target_port,s.listen_port,s.protocol,s.enabled,s.config_mode,COALESCE(c.custom_config,'') FROM sites s LEFT JOIN site_configs c ON c.site_id=s.id ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Site
	for rows.Next() {
		var v Site
		if err := rows.Scan(&v.ID, &v.Name, &v.Domain, &v.TargetHost, &v.TargetPort, &v.ListenPort, &v.Protocol, &v.Enabled, &v.ConfigMode, &v.CustomConfig); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Service) Get(ctx context.Context, id int64) (Site, error) {
	list, err := s.List(ctx)
	if err != nil {
		return Site{}, err
	}
	for _, site := range list {
		if site.ID == id {
			return site, nil
		}
	}
	return Site{}, sql.ErrNoRows
}

// Save 先检测 mDNS，再对完整未来状态做候选验证，然后记录 Desired 并发布。
func (s *Service) Save(ctx context.Context, site Site) (Site, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldState, err := s.List(ctx)
	if err != nil {
		return Site{}, err
	}
	var previous *Site
	index := -1
	for i := range oldState {
		if oldState[i].ID == site.ID && site.ID != 0 {
			previous = &oldState[i]
			index = i
		}
		if oldState[i].Domain == site.Domain && oldState[i].ID != site.ID {
			return Site{}, errors.New("DOMAIN_EXISTS")
		}
	}
	if site.ID != 0 && previous == nil {
		return Site{}, sql.ErrNoRows
	}
	if previous != nil && previous.ConfigMode == "CUSTOM" && site.ConfigMode == "CUSTOM" && site.CustomConfig == "" {
		site.CustomConfig = previous.CustomConfig
	}
	if err := site.Validate(); err != nil {
		return Site{}, err
	}
	if site.Enabled && site.Protocol == "SSH" {
		_, adminPort, _ := net.SplitHostPort(s.AdminListenAddr)
		if strconv.Itoa(site.ListenPort) == adminPort {
			return Site{}, errors.New("PORT_IN_USE")
		}
		for _, current := range oldState {
			if current.ID != site.ID && current.Enabled && current.Protocol == "SSH" && current.ListenPort == site.ListenPort {
				return Site{}, errors.New("PORT_IN_USE")
			}
		}
		if previous == nil || !previous.Enabled || previous.Protocol != "SSH" || previous.ListenPort != site.ListenPort {
			// openresty -t 不会真正绑定端口，发布前先检查本机监听冲突。
			if !s.portAvailable(site.ListenPort) {
				return Site{}, errors.New("PORT_IN_USE")
			}
		}
	}
	if site.Enabled && (previous == nil || previous.Domain != site.Domain || !previous.Enabled) {
		conflict, err := s.MDNS.Probe(ctx, site.Domain)
		if err != nil {
			return Site{}, err
		}
		if conflict {
			return Site{}, errors.New("MDNS_CONFLICT")
		}
	}
	proposed := append([]Site(nil), oldState...)
	if index >= 0 {
		proposed[index] = site
	} else {
		proposed = append(proposed, site)
	}
	candidate, err := s.Proxy.BuildCandidate(proposed)
	if err != nil {
		return Site{}, err
	}
	if err := s.Proxy.Validate(ctx, candidate); err != nil {
		return Site{}, fmt.Errorf("OPENRESTY_CONFIG_INVALID: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Site{}, err
	}
	defer tx.Rollback()
	if site.ID == 0 {
		result, err := tx.ExecContext(ctx, `INSERT INTO sites(name,domain,target_host,target_port,listen_port,protocol,enabled,config_mode,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, site.Name, site.Domain, site.TargetHost, site.TargetPort, site.ListenPort, site.Protocol, site.Enabled, site.ConfigMode, now, now)
		if err != nil {
			return Site{}, err
		}
		if site.ID, err = result.LastInsertId(); err != nil {
			return Site{}, err
		}
	} else {
		_, err := tx.ExecContext(ctx, `UPDATE sites SET name=?,domain=?,target_host=?,target_port=?,listen_port=?,protocol=?,enabled=?,config_mode=?,updated_at=? WHERE id=?`, site.Name, site.Domain, site.TargetHost, site.TargetPort, site.ListenPort, site.Protocol, site.Enabled, site.ConfigMode, now, site.ID)
		if err != nil {
			return Site{}, err
		}
	}
	if site.ConfigMode == "CUSTOM" {
		_, err = tx.ExecContext(ctx, `INSERT INTO site_configs(site_id,custom_config,updated_at) VALUES(?,?,?) ON CONFLICT(site_id) DO UPDATE SET custom_config=excluded.custom_config,updated_at=excluded.updated_at`, site.ID, site.CustomConfig, now)
		if err != nil {
			return Site{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, "DELETE FROM site_configs WHERE site_id=?", site.ID); err != nil {
			return Site{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE system_config SET desired_version=desired_version+1,updated_at=? WHERE id=1", now); err != nil {
		return Site{}, err
	}
	if err := tx.Commit(); err != nil {
		return Site{}, err
	}
	version, err := s.Proxy.Activate(ctx, candidate)
	if err != nil {
		return site, fmt.Errorf("OPENRESTY_RELOAD_FAILED: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE system_config SET active_release=?,active_desired_version=desired_version,updated_at=? WHERE id=1", version, now); err != nil {
		return site, err
	}
	if err := s.MDNS.Replace(ctx, proposed); err != nil {
		return site, err
	}
	slog.Info("site saved", "site_id", site.ID, "domain", site.Domain, "release", version)
	return site, nil
}

// Delete 验证删除后的完整配置；仅在发布成功后删除 Desired 记录。
func (s *Service) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.List(ctx)
	if err != nil {
		return err
	}
	proposed := make([]Site, 0, len(state))
	found := false
	for _, v := range state {
		if v.ID == id {
			found = true
			continue
		}
		proposed = append(proposed, v)
	}
	if !found {
		return sql.ErrNoRows
	}
	candidate, err := s.Proxy.BuildCandidate(proposed)
	if err != nil {
		return err
	}
	if err := s.Proxy.Validate(ctx, candidate); err != nil {
		return fmt.Errorf("OPENRESTY_CONFIG_INVALID: %w", err)
	}
	version, err := s.Proxy.Activate(ctx, candidate)
	if err != nil {
		return fmt.Errorf("OPENRESTY_RELOAD_FAILED: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM site_configs WHERE site_id=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sites WHERE id=?", id); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE system_config SET desired_version=desired_version+1,active_desired_version=desired_version+1,active_release=?,updated_at=? WHERE id=1", version, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.MDNS.Replace(ctx, proposed); err != nil {
		return err
	}
	slog.Info("site deleted", "site_id", id, "release", version)
	return nil
}

func (s *Service) Reconcile(ctx context.Context) error {
	state, err := s.List(ctx)
	if err != nil {
		return err
	}
	return s.MDNS.Replace(ctx, state)
}
