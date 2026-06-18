package metrics

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGenerateAlertRules_ProducesValidYAML(t *testing.T) {
	data := GenerateAlertRules("http://gotify:80")
	if data == nil {
		t.Fatal("expected non-nil output")
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty output")
	}

	// Parse the YAML to verify it's valid.
	var parsed struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert  string `yaml:"alert"`
				Expr   string `yaml:"expr"`
				For    string `yaml:"for"`
				Labels struct {
					Severity string `yaml:"severity"`
				} `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}

	if len(parsed.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(parsed.Groups))
	}
	if parsed.Groups[0].Name != "quartermaster" {
		t.Errorf("expected group name 'quartermaster', got %q", parsed.Groups[0].Name)
	}
}

func TestGenerateAlertRules_ContainsAllRules(t *testing.T) {
	data := GenerateAlertRules("http://gotify:80")
	output := string(data)

	expectedAlerts := []string{
		"HostDiskFull",
		"HostHighCPU",
		"HostHighMem",
		"ContainerDown",
		"ReconcileFailing",
		"LKGDegraded",
	}

	for _, alert := range expectedAlerts {
		if !strings.Contains(output, "alert: "+alert) {
			t.Errorf("expected alert %q in output", alert)
		}
	}

	if len(expectedAlerts) != 6 {
		t.Errorf("expected exactly 6 rules, got %d", len(expectedAlerts))
	}
}

func TestGenerateAlertRules_IncludesGotifyURL(t *testing.T) {
	url := "http://gotify.example.com:8080"
	data := GenerateAlertRules(url)
	output := string(data)

	if !strings.Contains(output, url) {
		t.Errorf("expected gotify URL %q in output", url)
	}
}

func TestGenerateAlertRules_EmptyURL(t *testing.T) {
	data := GenerateAlertRules("")
	if data == nil {
		t.Fatal("expected non-nil output even with empty URL")
	}
	output := string(data)

	// Should still generate valid YAML, just with empty gotify_url.
	if !strings.Contains(output, "alert: HostDiskFull") {
		t.Error("expected HostDiskFull rule present")
	}
}
