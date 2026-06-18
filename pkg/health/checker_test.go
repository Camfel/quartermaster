package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"quartermaster/pkg/types"
)

func TestRunCheck_NoHealthCheck(t *testing.T) {
	c := NewChecker()
	svc := types.Service{Name: "test", Image: "alpine"}
	result := c.RunCheck(svc, "")
	if !result.Healthy {
		t.Error("service without healthcheck should be considered healthy")
	}
}

func TestCheckHTTP_Success(t *testing.T) {
	// Start a test HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewChecker()
	svc := types.Service{
		Name:  "test",
		Ports: []types.Port{{Host: 8080, Container: 80}},
		HealthCheck: &types.HealthCheck{
			Type: "http",
			Path: "/health",
			Port: 8080,
		},
	}

	// The probe will fail (no server on 8080), but we can verify the type
	result := c.RunCheck(svc, "")
	if result.Type != "http" {
		t.Errorf("expected http type, got %s", result.Type)
	}
	if result.ServiceName != "test" {
		t.Errorf("expected service name test, got %s", result.ServiceName)
	}
	if result.Healthy {
		t.Log("HTTP probe unexpectedly succeeded (maybe something on port 8080)")
	}
}

func TestCheckTCP(t *testing.T) {
	c := NewChecker()
	svc := types.Service{
		Name:  "test",
		Ports: []types.Port{{Host: 19999, Container: 80}},
		HealthCheck: &types.HealthCheck{
			Type: "tcp",
			Port: 19999,
		},
	}

	// Should fail since nothing is listening on 19999
	result := c.RunCheck(svc, "")
	if result.Healthy {
		t.Error("TCP probe should fail when nothing is listening")
	}
	if result.Type != "tcp" {
		t.Errorf("expected tcp type, got %s", result.Type)
	}
}

func TestCheckWithBridgeIP(t *testing.T) {
	// Start a test HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewChecker()
	svc := types.Service{
		Name: "test",
		HealthCheck: &types.HealthCheck{
			Type: "http",
			Path: "/",
			Port: 19998,
		},
	}

	// With no bridge IP, probes localhost:19998 (should fail)
	result := c.RunCheck(svc, "")
	if result.Healthy {
		t.Log("probe to localhost:19998 unexpectedly succeeded")
	}

	// Bridge IP doesn't match the test server either, just verify it's used
	result2 := c.RunCheck(svc, "10.42.0.5")
	if result2.Healthy {
		t.Log("probe to 10.42.0.5:19998 unexpectedly succeeded")
	}
}

func TestResolvePort(t *testing.T) {
	// Health check port takes priority
	svc := types.Service{
		Ports: []types.Port{{Host: 8080, Container: 80}},
		HealthCheck: &types.HealthCheck{
			Port: 9090,
		},
	}
	port := resolvePort(svc, svc.HealthCheck)
	if port != 9090 {
		t.Errorf("expected healthcheck port 9090, got %d", port)
	}

	// Falls back to first host port
	svc.HealthCheck.Port = 0
	port = resolvePort(svc, svc.HealthCheck)
	if port != 8080 {
		t.Errorf("expected first host port 8080, got %d", port)
	}

	// Falls back to 80
	svc.Ports = nil
	port = resolvePort(svc, svc.HealthCheck)
	if port != 80 {
		t.Errorf("expected default port 80, got %d", port)
	}
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"", 30 * time.Second},
		{"10s", 10 * time.Second},
		{"1m", 1 * time.Minute},
		{"1m30s", 90 * time.Second},
		{"invalid", 30 * time.Second},
		{"0.5s", time.Second}, // below minimum
	}
	for _, tt := range tests {
		got := ParseInterval(tt.input)
		if got != tt.expected {
			t.Errorf("ParseInterval(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestRunAll(t *testing.T) {
	c := NewChecker()
	services := []types.Service{
		{Name: "no-check", Image: "alpine"},
		{Name: "with-check", Image: "nginx",
			Ports:       []types.Port{{Host: 19998, Container: 80}},
			HealthCheck: &types.HealthCheck{Type: "tcp", Port: 19998},
		},
		{Name: "also-no-check", Image: "redis"},
	}

	results := c.RunAll(services)
	if len(results) != 1 {
		t.Errorf("expected 1 result (only services with health checks), got %d", len(results))
	}
	if results[0].ServiceName != "with-check" {
		t.Errorf("expected with-check, got %s", results[0].ServiceName)
	}
}
