package openresty

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mdns-gateway/internal/sites"
)

type fakeRunner struct{ validateError, reloadError error }

func TestCommandRunnerReturnsAfterDaemonFork(t *testing.T) {
	// 后台子进程保留标准输出；命令运行器应在启动命令退出后返回。
	binary := filepath.Join(t.TempDir(), "daemon-command")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 5 &\n"), 0755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := (commandRunner{binary: binary}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) >= 3*time.Second {
		t.Fatal("command runner waited for the background child")
	}
}

func (f *fakeRunner) Run(_ context.Context, args ...string) error {
	if len(args) > 0 && args[0] == "-t" {
		return f.validateError
	}
	if len(args) > 0 && args[0] == "-s" {
		return f.reloadError
	}
	return nil
}

func TestInvalidCandidateKeepsActive(t *testing.T) {
	runner := &fakeRunner{}
	manager := NewWithRunner(t.TempDir(), runner)
	first, _ := manager.BuildCandidate(nil)
	version, err := manager.Activate(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := manager.BuildCandidate(nil)
	runner.validateError = errors.New("invalid directive")
	if _, err := manager.Activate(context.Background(), second); err == nil {
		t.Fatal("invalid config accepted")
	}
	if manager.ActiveVersion() != version {
		t.Fatal("active release changed")
	}
}

func TestReloadFailureRestoresSymlink(t *testing.T) {
	runner := &fakeRunner{}
	manager := NewWithRunner(t.TempDir(), runner)
	first, _ := manager.BuildCandidate(nil)
	version, err := manager.Activate(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := manager.BuildCandidate(nil)
	runner.reloadError = errors.New("reload failed")
	if _, err := manager.Activate(context.Background(), second); err == nil {
		t.Fatal("reload failure lost")
	}
	if manager.ActiveVersion() != version {
		t.Fatal("active release not restored")
	}
	if _, err := os.Stat(filepath.Join(manager.root, "current", "nginx.conf")); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratePreservesCustom(t *testing.T) {
	custom := "server { listen 80; server_name media.local; location / { return 204; } }"
	state := []sites.Site{{Name: "Media", Domain: "media.local", TargetHost: "127.0.0.1", TargetPort: 8096, Protocol: "HTTP", Enabled: true, ConfigMode: "CUSTOM", CustomConfig: custom}}
	got, err := Generate(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, custom) {
		t.Fatal("custom configuration overwritten")
	}
}

func TestDuplicateServerNameIsRejected(t *testing.T) {
	state := []sites.Site{
		{Name: "Media", Domain: "media.local", TargetHost: "127.0.0.1", TargetPort: 80, Protocol: "HTTP", Enabled: true, ConfigMode: "GENERATED"},
		{Name: "Photo", Domain: "photo.local", TargetHost: "127.0.0.1", TargetPort: 80, Protocol: "HTTP", Enabled: true, ConfigMode: "CUSTOM", CustomConfig: "server { listen 80; server_name media.local; location / { return 204; } }"},
	}
	if _, err := Generate(state); err == nil {
		t.Fatal("duplicate server_name accepted")
	}
}

func TestGenerateWebSocketAndSSH(t *testing.T) {
	state := []sites.Site{
		{Name: "Chat", Domain: "chat.local", TargetHost: "192.0.2.10", TargetPort: 9000, Protocol: "WEBSOCKET", Enabled: true, ConfigMode: "GENERATED"},
		{Name: "Shell", Domain: "shell.local", TargetHost: "192.0.2.11", TargetPort: 22, ListenPort: 2222, Protocol: "SSH", Enabled: true, ConfigMode: "GENERATED"},
	}
	got, err := Generate(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"proxy_set_header Upgrade $http_upgrade;", "proxy_http_version 1.1;", "stream {", "listen 2222;", "proxy_pass 192.0.2.11:22;"} {
		if !strings.Contains(got, part) {
			t.Fatalf("missing %q in configuration", part)
		}
	}
	state = append(state, sites.Site{Name: "Other", Domain: "other.local", TargetHost: "192.0.2.12", TargetPort: 22, ListenPort: 2222, Protocol: "SSH", Enabled: true, ConfigMode: "GENERATED"})
	if _, err := Generate(state); err == nil || err.Error() != "PORT_IN_USE" {
		t.Fatalf("duplicate SSH listener: %v", err)
	}
}
