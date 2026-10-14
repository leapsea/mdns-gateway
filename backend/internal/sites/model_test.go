package sites

import "testing"

func TestValidateDomain(t *testing.T) {
	for _, domain := range []string{"media.local", "NAS.local"} {
		v := Site{Name: "x", Domain: domain, TargetHost: "127.0.0.1", TargetPort: 80, Protocol: "HTTP", ConfigMode: "GENERATED"}
		if err := v.Validate(); err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
	}
	for _, domain := range []string{"example.com", "a.b.local", "-bad.local", "bad_.local"} {
		v := Site{Name: "x", Domain: domain, TargetHost: "127.0.0.1", TargetPort: 80, Protocol: "HTTP", ConfigMode: "GENERATED"}
		if err := v.Validate(); err == nil {
			t.Fatalf("accepted %s", domain)
		}
	}
}

func TestValidateProtocolAndListener(t *testing.T) {
	base := Site{Name: "SSH", Domain: "ssh.local", TargetHost: "192.0.2.10", TargetPort: 22, ListenPort: 2222, Protocol: "SSH", ConfigMode: "GENERATED"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{0, 80, 65536} {
		invalid := base
		invalid.ListenPort = port
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted SSH listener %d", port)
		}
	}
	websocket := base
	websocket.Protocol = "WEBSOCKET"
	websocket.ListenPort = 0
	if err := websocket.Validate(); err != nil {
		t.Fatal(err)
	}
}
