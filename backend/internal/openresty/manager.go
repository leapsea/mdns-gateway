package openresty

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mdns-gateway/internal/sites"
)

// Runner 是 OpenResty 进程边界，集成测试可替换为受控实现。
type Runner interface {
	Run(context.Context, ...string) error
}

type commandRunner struct{ binary string }

func (r commandRunner) Run(ctx context.Context, args ...string) error {
	// OpenResty 启动后会转入后台；子进程继承输出描述符时，CombinedOutput 会一直等待管道关闭。
	output, err := os.CreateTemp("", "mdns-gateway-openresty-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	command := exec.CommandContext(ctx, r.binary, args...)
	command.Stdout = output
	command.Stderr = output
	err = command.Run()
	if err != nil {
		out, _ := os.ReadFile(output.Name())
		return fmt.Errorf("openresty %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type Manager struct {
	root      string
	runner    Runner
	mu        sync.Mutex
	lastApply string
}

func New(root, binary string) *Manager {
	return &Manager{root: root, runner: commandRunner{binary: binary}}
}
func NewWithRunner(root string, runner Runner) *Manager { return &Manager{root: root, runner: runner} }

func (m *Manager) CurrentConfig() string { return filepath.Join(m.root, "current", "nginx.conf") }

// BuildCandidate 生成完整配置，候选目录不在运行配置的 include 路径内。
func (m *Manager) BuildCandidate(state []sites.Site) (string, error) {
	if err := os.MkdirAll(filepath.Join(m.root, "candidates"), 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(filepath.Join(m.root, "candidates"), "candidate-")
	if err != nil {
		return "", err
	}
	content, err := Generate(state)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	content = fmt.Sprintf("pid %s;\nerror_log %s;\n", filepath.Join(m.root, "nginx.pid"), filepath.Join(m.root, "error.log")) + content
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(content), 0600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func Generate(state []sites.Site) (string, error) {
	var httpServers, streamServers strings.Builder
	seen := map[string]bool{}
	serverNames := map[string]bool{}
	listenPorts := map[int]bool{}
	for _, site := range state {
		if !site.Enabled {
			continue
		}
		if err := site.Validate(); err != nil {
			return "", err
		}
		if seen[site.Domain] {
			return "", errors.New("DOMAIN_EXISTS")
		}
		seen[site.Domain] = true
		if site.Protocol == "SSH" {
			if listenPorts[site.ListenPort] {
				return "", errors.New("PORT_IN_USE")
			}
			listenPorts[site.ListenPort] = true
			streamServers.WriteString(GenerateServer(site))
			streamServers.WriteByte('\n')
			continue
		}
		if site.ConfigMode == "CUSTOM" {
			for _, match := range serverNamePattern.FindAllStringSubmatch(site.CustomConfig, -1) {
				for _, name := range strings.Fields(match[1]) {
					name = strings.ToLower(name)
					if serverNames[name] {
						return "", errors.New("DOMAIN_EXISTS")
					}
					serverNames[name] = true
				}
			}
			httpServers.WriteString(site.CustomConfig)
			httpServers.WriteByte('\n')
			continue
		}
		if serverNames[site.Domain] {
			return "", errors.New("DOMAIN_EXISTS")
		}
		serverNames[site.Domain] = true
		httpServers.WriteString(GenerateServer(site))
		httpServers.WriteByte('\n')
	}
	var b strings.Builder
	b.WriteString("worker_processes auto;\nevents { worker_connections 1024; }\nhttp {\n    map $http_upgrade $connection_upgrade { default upgrade; '' close; }\n")
	b.WriteString(httpServers.String())
	b.WriteString("}\n")
	if streamServers.Len() > 0 {
		b.WriteString("stream {\n")
		b.WriteString(streamServers.String())
		b.WriteString("}\n")
	}
	return b.String(), nil
}

var serverNamePattern = regexp.MustCompile(`(?m)\bserver_name\s+([^;]+);`)

// GenerateServer 为普通模式和高级编辑器提供同一份服务配置。
func GenerateServer(site sites.Site) string {
	if site.Protocol == "SSH" {
		return fmt.Sprintf("server {\n    listen %d;\n    proxy_pass %s;\n    proxy_connect_timeout 5s;\n    proxy_timeout 24h;\n}", site.ListenPort, net.JoinHostPort(site.TargetHost, fmt.Sprint(site.TargetPort)))
	}
	upgrade := ""
	if site.Protocol == "WEBSOCKET" {
		upgrade = "        proxy_http_version 1.1;\n        proxy_set_header Upgrade $http_upgrade;\n        proxy_set_header Connection $connection_upgrade;\n        proxy_read_timeout 3600s;\n"
	}
	return fmt.Sprintf("server {\n    listen 80;\n    server_name %s;\n\n    location / {\n        proxy_pass http://%s;\n        proxy_set_header Host $host;\n        proxy_set_header X-Real-IP $remote_addr;\n%s    }\n}", site.Domain, net.JoinHostPort(site.TargetHost, fmt.Sprint(site.TargetPort)), upgrade)
}

func (m *Manager) Validate(ctx context.Context, candidate string) error {
	err := m.validate(ctx, candidate)
	m.mu.Lock()
	if err != nil {
		m.lastApply = "CONFIG_ERROR"
	}
	m.mu.Unlock()
	if err != nil {
		slog.Warn("openresty validation failed", "candidate", candidate, "error", err)
	} else {
		slog.Info("openresty validation passed", "candidate", candidate)
	}
	return err
}

func (m *Manager) validate(ctx context.Context, candidate string) error {
	return m.runner.Run(ctx, "-t", "-c", filepath.Join(candidate, "nginx.conf"))
}

// Activate 在串行临界区中发布、reload、回退；只有 reload 成功才返回新版本。
func (m *Manager) Activate(ctx context.Context, candidate string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validate(ctx, candidate); err != nil {
		m.lastApply = "CONFIG_ERROR"
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(m.root, "releases"), 0700); err != nil {
		return "", err
	}
	version := fmt.Sprintf("%d", time.Now().UnixNano())
	release := filepath.Join(m.root, "releases", version)
	if err := os.Rename(candidate, release); err != nil {
		return "", err
	}
	current := filepath.Join(m.root, "current")
	previous, _ := os.Readlink(current)
	tmp := filepath.Join(m.root, ".current-next")
	os.Remove(tmp)
	if err := os.Symlink(release, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, current); err != nil {
		return "", err
	}
	if err := m.runner.Run(ctx, "-s", "reload", "-c", m.CurrentConfig()); err != nil {
		m.lastApply = "RELOAD_ERROR"
		slog.Error("openresty reload failed", "release", version, "error", err)
		if previous != "" {
			os.Symlink(previous, tmp)
			if rollbackErr := os.Rename(tmp, current); rollbackErr != nil {
				return "", fmt.Errorf("reload: %w; rollback: %v", err, rollbackErr)
			}
		}
		return "", err
	}
	m.prune(release, previous)
	m.lastApply = "HEALTHY"
	slog.Info("openresty release active", "release", version)
	return version, nil
}

func (m *Manager) Start(ctx context.Context) error {
	if _, err := os.Stat(m.CurrentConfig()); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "-t", "-c", m.CurrentConfig()); err != nil {
		return err
	}
	if err := m.runner.Run(ctx, "-c", m.CurrentConfig()); err != nil {
		return err
	}
	m.mu.Lock()
	m.lastApply = "HEALTHY"
	m.mu.Unlock()
	return nil
}

// Bootstrap 只在没有 Active Release 时创建空配置；重启永远读取现有 Active。
func (m *Manager) Bootstrap(ctx context.Context) error {
	if m.ActiveVersion() != "" {
		return m.Start(ctx)
	}
	candidate, err := m.BuildCandidate(nil)
	if err != nil {
		return err
	}
	if err := m.Validate(ctx, candidate); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(m.root, "releases"), 0700); err != nil {
		return err
	}
	release := filepath.Join(m.root, "releases", fmt.Sprintf("bootstrap-%d", time.Now().UnixNano()))
	if err := os.Rename(candidate, release); err != nil {
		return err
	}
	if err := os.Symlink(release, filepath.Join(m.root, "current")); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *Manager) Stop(ctx context.Context) error {
	err := m.runner.Run(ctx, "-s", "quit", "-c", m.CurrentConfig())
	return err
}

func (m *Manager) RuntimeStatus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(m.root, "nginx.pid"))
	if err != nil {
		return "STOPPED"
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return "STOPPED"
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return "STOPPED"
	}
	return "HEALTHY"
}

func (m *Manager) ApplyStatus() string { m.mu.Lock(); defer m.mu.Unlock(); return m.lastApply }

func (m *Manager) ActiveVersion() string {
	link, err := os.Readlink(filepath.Join(m.root, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func (m *Manager) prune(current, previous string) {
	entries, err := os.ReadDir(filepath.Join(m.root, "releases"))
	if err != nil || len(entries) <= 5 {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > 5 {
		name := names[0]
		names = names[1:]
		path := filepath.Join(m.root, "releases", name)
		if path != current && path != previous {
			_ = os.RemoveAll(path)
		}
	}
}
