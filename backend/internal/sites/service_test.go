package sites

import (
	"context"
	"errors"
	"testing"

	"mdns-gateway/internal/database"
)

type fakeMDNS struct {
	conflict bool
	state    []Site
}

func (m *fakeMDNS) Probe(context.Context, string) (bool, error) { return m.conflict, nil }
func (m *fakeMDNS) Replace(_ context.Context, state []Site) error {
	m.state = append([]Site(nil), state...)
	return nil
}
func (m *fakeMDNS) Status() string { return "OK" }

type fakePublisher struct {
	validationError, activateError error
	active                         string
	lastState                      []Site
}

func (p *fakePublisher) BuildCandidate(state []Site) (string, error) {
	p.lastState = append([]Site(nil), state...)
	return "candidate", nil
}
func (p *fakePublisher) Validate(context.Context, string) error { return p.validationError }
func (p *fakePublisher) Activate(context.Context, string) (string, error) {
	if p.activateError != nil {
		return "", p.activateError
	}
	p.active = "release-1"
	return p.active, nil
}
func (p *fakePublisher) ActiveVersion() string { return p.active }

func TestSiteLifecycleAndValidationBoundary(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mdns := &fakeMDNS{}
	publisher := &fakePublisher{}
	service := NewService(db, mdns, publisher, ":8080")
	site := Site{Name: "Media", Domain: "media.local", TargetHost: "192.0.2.10", TargetPort: 8096, Protocol: "HTTP", Enabled: true, ConfigMode: "GENERATED"}
	mdns.conflict = true
	if _, err := service.Save(ctx, site); err == nil || err.Error() != "MDNS_CONFLICT" {
		t.Fatalf("conflict: %v", err)
	}
	mdns.conflict = false
	publisher.validationError = errors.New("bad syntax")
	if _, err := service.Save(ctx, site); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	list, _ := service.List(ctx)
	if len(list) != 0 || publisher.active != "" {
		t.Fatal("invalid candidate changed state")
	}
	publisher.validationError = nil
	saved, err := service.Save(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == 0 || publisher.active == "" || len(mdns.state) != 1 {
		t.Fatal("create not published")
	}
	saved.TargetPort = 8097
	saved, err = service.Save(ctx, saved)
	if err != nil {
		t.Fatal(err)
	}
	if publisher.lastState[0].TargetPort != 8097 {
		t.Fatal("update not generated")
	}
	publisher.validationError = errors.New("bad syntax")
	if err := service.Delete(ctx, saved.ID); err == nil {
		t.Fatal("invalid delete accepted")
	}
	list, _ = service.List(ctx)
	if len(list) != 1 {
		t.Fatal("validation failure deleted site")
	}
	publisher.validationError = nil
	if err := service.Delete(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = service.List(ctx)
	if len(list) != 0 || len(mdns.state) != 0 {
		t.Fatal("delete incomplete")
	}
}

func TestCustomConfigSurvivesOrdinaryEdit(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := NewService(db, &fakeMDNS{}, &fakePublisher{}, ":8080")
	custom := "server { listen 80; server_name media.local; location / { return 204; } }"
	site := Site{Name: "Media", Domain: "media.local", TargetHost: "192.0.2.10", TargetPort: 8096, Protocol: "HTTP", Enabled: true, ConfigMode: "CUSTOM", CustomConfig: custom}
	saved, err := service.Save(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	saved.TargetPort = 9000
	saved.CustomConfig = ""
	if _, err := service.Save(ctx, saved); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(ctx, saved.ID)
	if err != nil || got.CustomConfig != custom {
		t.Fatalf("custom lost: %#v, %v", got, err)
	}
}

func TestSSHListenerConflictsBeforePublishing(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := NewService(db, &fakeMDNS{}, &fakePublisher{}, ":8080")
	service.portAvailable = func(int) bool { return true }
	first := Site{Name: "Shell", Domain: "shell.local", TargetHost: "192.0.2.10", TargetPort: 22, ListenPort: 2222, Protocol: "SSH", Enabled: true, ConfigMode: "GENERATED"}
	if _, err := service.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Name, second.Domain = "Other", "other.local"
	if _, err := service.Save(ctx, second); err == nil || err.Error() != "PORT_IN_USE" {
		t.Fatalf("duplicate SSH port: %v", err)
	}
	second.ListenPort = 8080
	if _, err := service.Save(ctx, second); err == nil || err.Error() != "PORT_IN_USE" {
		t.Fatalf("admin port: %v", err)
	}
	service.portAvailable = func(int) bool { return false }
	second.ListenPort = 2223
	if _, err := service.Save(ctx, second); err == nil || err.Error() != "PORT_IN_USE" {
		t.Fatalf("occupied port: %v", err)
	}
}
