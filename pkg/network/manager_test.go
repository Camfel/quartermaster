package network

import "testing"

func TestValidProfile(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"public", true},
		{"internal", true},
		{"vpn", true},
		{"", true}, // empty → public
		{"PUBLIC", true},
		{"Vpn", true},
		{"invalid", false},
		{"bridge", false},
		{"host", false},
		// Old names are no longer valid.
		{"default", false},
		{"vpn-gateway", false},
		{"dmz", false},
	}
	for _, tt := range tests {
		if got := ValidProfile(tt.input); got != tt.expected {
			t.Errorf("ValidProfile(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestNormaliseProfile(t *testing.T) {
	if NormaliseProfile("") != ProfilePublic {
		t.Error("empty should normalise to public")
	}
	if NormaliseProfile("INTERNAL") != ProfileInternal {
		t.Error("INTERNAL should normalise to internal")
	}
	if NormaliseProfile("Vpn") != ProfileVPN {
		t.Error("Vpn should normalise to vpn")
	}
}

func TestRegisterAndGatewayPID(t *testing.T) {
	m := NewManager()

	pid, ok := m.GatewayPID()
	if ok || pid != 0 {
		t.Error("expected no gateway before registration")
	}

	m.RegisterVPNGateway(4242)
	pid, ok = m.GatewayPID()
	if !ok || pid != 4242 {
		t.Errorf("expected pid 4242, got %d (ok=%v)", pid, ok)
	}

	m.UnregisterVPNGateway()
	_, ok = m.GatewayPID()
	if ok {
		t.Error("expected no gateway after unregistration")
	}
}

func TestNetworkNamespacePath(t *testing.T) {
	path := NetworkNamespacePath(1234)
	if path != "/proc/1234/ns/net" {
		t.Errorf("expected /proc/1234/ns/net, got %s", path)
	}
}

func TestResolveProfile(t *testing.T) {
	m := NewManager()

	// public / internal — never a gateway, never shares.
	for _, p := range []string{"public", "internal"} {
		isGW, sharePID, err := m.ResolveProfile(p, nil, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", p, err)
		}
		if isGW {
			t.Errorf("%s: should not be gateway", p)
		}
		if sharePID != 0 {
			t.Errorf("%s: should not share namespace", p)
		}
	}

	// vpn with no dependencies — becomes gateway.
	isGW, sharePID, err := m.ResolveProfile("vpn", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !isGW {
		t.Error("vpn with no deps should be gateway")
	}
	if sharePID != 0 {
		t.Error("gateway should not share a namespace")
	}

	// vpn depends on a running vpn service — joins its namespace.
	running := map[string]uint32{"wireguard": 9999}
	isGW, sharePID, err = m.ResolveProfile("vpn", []string{"wireguard"}, running)
	if err != nil {
		t.Fatal(err)
	}
	if isGW {
		t.Error("dependent should not be gateway")
	}
	if sharePID != 9999 {
		t.Errorf("expected sharePID 9999, got %d", sharePID)
	}

	// vpn depends on a service that isn't running yet — becomes gateway (fallback).
	running2 := map[string]uint32{"other": 8888}
	isGW, sharePID, err = m.ResolveProfile("vpn", []string{"wireguard"}, running2)
	if err != nil {
		t.Fatal(err)
	}
	if !isGW {
		t.Error("vpn with unresolved dep should still be gateway")
	}

	// bogus profile.
	_, _, err = m.ResolveProfile("bogus", nil, nil)
	if err == nil {
		t.Error("expected error for bogus profile")
	}
}
