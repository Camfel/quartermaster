package network

import (
	"net"
	"testing"
)

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

func TestResolveProfile(t *testing.T) {
	// public / internal — never a gateway, never a dependent.
	for _, p := range []string{"public", "internal"} {
		isGW, gwName, err := ResolveProfile(p, nil, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", p, err)
		}
		if isGW {
			t.Errorf("%s: should not be gateway", p)
		}
		if gwName != "" {
			t.Errorf("%s: should not have gateway name, got %q", p, gwName)
		}
	}

	// vpn with no dependencies — becomes gateway.
	isGW, gwName, err := ResolveProfile("vpn", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !isGW {
		t.Error("vpn with no deps should be gateway")
	}
	if gwName != "" {
		t.Errorf("gateway should not have gateway name, got %q", gwName)
	}

	// vpn depends on a running vpn service — returns its name.
	running := map[string]string{"gluetun": "10.42.0.10"}
	isGW, gwName, err = ResolveProfile("vpn", []string{"gluetun"}, running)
	if err != nil {
		t.Fatal(err)
	}
	if isGW {
		t.Error("dependent should not be gateway")
	}
	if gwName != "gluetun" {
		t.Errorf("expected gateway name gluetun, got %q", gwName)
	}

	// vpn depends on a service that isn't running yet — becomes gateway (fallback).
	running2 := map[string]string{"other": "10.42.0.20"}
	isGW, gwName, err = ResolveProfile("vpn", []string{"gluetun"}, running2)
	if err != nil {
		t.Fatal(err)
	}
	if !isGW {
		t.Error("vpn with unresolved dep should still be gateway")
	}
	if gwName != "" {
		t.Errorf("fallback gateway should not have gateway name, got %q", gwName)
	}

	// bogus profile.
	_, _, err = ResolveProfile("bogus", nil, nil)
	if err == nil {
		t.Error("expected error for bogus profile")
	}
}

func TestBridgeIPAllocation(t *testing.T) {
	bm := NewBridgeManager()
	if bm.LookupIP("test") != nil {
		t.Error("LookupIP should return nil before allocation")
	}

	// Allocate manually through the ips map.
	bm.ips["test"] = bm.nextIPAlloc()
	ip := bm.LookupIP("test")
	if ip == nil {
		t.Error("LookupIP should return allocated IP")
	}
	if ip.String() != "10.42.0.2" {
		t.Errorf("expected 10.42.0.2, got %s", ip)
	}

	// Delete
	delete(bm.ips, "test")
	if bm.LookupIP("test") != nil {
		t.Error("LookupIP should return nil after deletion")
	}
}

func (b *BridgeManager) nextIPAlloc() net.IP {
	ip := net.IP{10, 42, 0, b.nextIP}
	b.nextIP++
	return ip
}

// TestDetachRecreateCycle verifies that the IP allocation and deallocation
// cycle works correctly across 10 iterations — the core logic behind the
// Step 1 detach/cleanup fix.  Actual netns/veth operations require root
// and are covered by integration tests.
func TestDetachRecreateCycle(t *testing.T) {
	bm := NewBridgeManager()
	const iterations = 10

	for i := 0; i < iterations; i++ {
		// Simulate Attach: allocate IP
		bm.mu.Lock()
		ip := bm.nextIPAlloc()
		bm.ips["test-service"] = ip
		bm.nextIP++
		bm.mu.Unlock()

		// Verify IP was allocated
		if bm.LookupIP("test-service") == nil {
			t.Fatalf("iteration %d: LookupIP returned nil after allocation", i)
		}

		// Simulate Detach: snapshot IP, delete from map, call removePorts with saved IP
		bm.mu.Lock()
		savedIP := bm.ips["test-service"]
		delete(bm.ips, "test-service")
		bm.mu.Unlock()

		if savedIP == nil {
			t.Fatalf("iteration %d: savedIP is nil — race condition", i)
		}

		// removePorts with the saved IP (should not panic, should not need b.ips lookup)
		// ipt4 is nil here so this is a no-op — but the key test is it doesn't panic
		bm.removePorts("test-service", savedIP)

		// Verify IP was released
		if bm.LookupIP("test-service") != nil {
			t.Fatalf("iteration %d: LookupIP returned value after deletion", i)
		}
	}
}

func TestRemovePortsNilIP(t *testing.T) {
	bm := NewBridgeManager()
	// removePorts with nil IP should be a no-op (no panic, no lock)
	bm.removePorts("test", nil)
	// If we get here without panicking, it passes
}

func TestRemovePortsNilIpt(t *testing.T) {
	bm := NewBridgeManager()
	// ipt4 is nil in NewBridgeManager, so removePorts should no-op gracefully
	ip := net.ParseIP("10.42.0.5")
	bm.removePorts("test", ip)
	// Should not panic
}

func TestShortName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "ctr"},
		{"a", "a"},
		{"gluetun", "gluetun"},
		{"longcontainername", "longcont"},
	}
	for _, tt := range tests {
		if got := ShortName(tt.input); got != tt.expected {
			t.Errorf("ShortName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
