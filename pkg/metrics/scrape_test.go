package metrics

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ── Helper types for test unmarshalling ────────────────────────────────

type testScrapeConfig struct {
	ScrapeConfigs []testScrapeJob `yaml:"scrape_configs"`
}

type testScrapeJob struct {
	JobName       string           `yaml:"job_name"`
	StaticConfigs []testStaticConf `yaml:"static_configs"`
	MetricsPath   string           `yaml:"metrics_path,omitempty"`
}

type testStaticConf struct {
	Targets []string `yaml:"targets"`
}

func unmarshalConfig(t *testing.T, data []byte) testScrapeConfig {
	t.Helper()
	var cfg testScrapeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to unmarshal scrape config: %v", err)
	}
	return cfg
}

func findJob(t *testing.T, jobs []testScrapeJob, name string) *testScrapeJob {
	t.Helper()
	for i := range jobs {
		if jobs[i].JobName == name {
			return &jobs[i]
		}
	}
	return nil
}

// ── Tests ──────────────────────────────────────────────────────────────

func TestGenerateScrapeConfig_EmptyServices(t *testing.T) {
	data := GenerateScrapeConfig(nil)
	if len(data) == 0 {
		t.Fatal("expected non-empty scrape config")
	}

	cfg := unmarshalConfig(t, data)

	// Static targets must be present.
	qm := findJob(t, cfg.ScrapeConfigs, "quartermaster")
	if qm == nil {
		t.Fatal("quartermaster job missing")
	}
	if qm.MetricsPath != "/v1/metrics" {
		t.Errorf("expected /v1/metrics path, got %q", qm.MetricsPath)
	}
	if len(qm.StaticConfigs) != 1 || len(qm.StaticConfigs[0].Targets) != 1 {
		t.Fatal("expected one target for quartermaster")
	}
	if qm.StaticConfigs[0].Targets[0] != "127.0.0.1:9098" {
		t.Errorf("expected 127.0.0.1:9098, got %q", qm.StaticConfigs[0].Targets[0])
	}

	ne := findJob(t, cfg.ScrapeConfigs, "node_exporter")
	if ne == nil {
		t.Fatal("node_exporter job missing")
	}

	vm := findJob(t, cfg.ScrapeConfigs, "victoria-metrics")
	if vm == nil {
		t.Fatal("victoria-metrics job missing")
	}

	// No extra service jobs.
	if len(cfg.ScrapeConfigs) != 3 {
		t.Errorf("expected 3 jobs (static only), got %d", len(cfg.ScrapeConfigs))
	}
}

func TestGenerateScrapeConfig_WithServices(t *testing.T) {
	services := []ScrapeTarget{
		{JobName: "jellyfin", Address: "10.42.0.5:8096", MetricsPath: "/metrics"},
		{JobName: "authelia", Address: "10.42.0.7:9091"},
	}

	data := GenerateScrapeConfig(services)
	cfg := unmarshalConfig(t, data)

	if len(cfg.ScrapeConfigs) != 5 {
		t.Errorf("expected 5 jobs (3 static + 2 services), got %d", len(cfg.ScrapeConfigs))
	}

	// Jellyfin — custom path.
	jf := findJob(t, cfg.ScrapeConfigs, "jellyfin")
	if jf == nil {
		t.Fatal("jellyfin job missing")
	}
	if jf.MetricsPath != "/metrics" {
		t.Errorf("expected /metrics path, got %q", jf.MetricsPath)
	}
	if len(jf.StaticConfigs) != 1 || jf.StaticConfigs[0].Targets[0] != "10.42.0.5:8096" {
		t.Errorf("jellyfin target mismatch: %v", jf.StaticConfigs)
	}

	// Authelia — no path, should default to /metrics (no metrics_path key).
	au := findJob(t, cfg.ScrapeConfigs, "authelia")
	if au == nil {
		t.Fatal("authelia job missing")
	}
	if au.MetricsPath != "" {
		t.Errorf("expected empty metrics path (default), got %q", au.MetricsPath)
	}
}

func TestGenerateScrapeConfig_NoDuplicateJobNames(t *testing.T) {
	// Two services with the same job name — YAML handles it fine.
	services := []ScrapeTarget{
		{JobName: "sidecar", Address: "10.42.0.10:8080"},
		{JobName: "sidecar", Address: "10.42.0.11:8080"},
	}

	data := GenerateScrapeConfig(services)
	cfg := unmarshalConfig(t, data)

	// Both jobs should appear (YAML doesn't deduplicate by name).
	count := 0
	for _, j := range cfg.ScrapeConfigs {
		if j.JobName == "sidecar" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 sidecar jobs, got %d", count)
	}
}

func TestGenerateScrapeConfig_ValidYAML(t *testing.T) {
	services := []ScrapeTarget{
		{JobName: "jellyfin", Address: "10.42.0.5:8096", MetricsPath: "/metrics"},
	}

	data := GenerateScrapeConfig(services)
	if len(data) == 0 {
		t.Fatal("expected non-empty output")
	}

	// Should start with scrape_configs key.
	if !strings.Contains(string(data), "scrape_configs:") {
		t.Error("output does not contain scrape_configs key")
	}

	// Should be parseable.
	cfg := unmarshalConfig(t, data)
	if len(cfg.ScrapeConfigs) < 3 {
		t.Errorf("expected at least 3 jobs, got %d", len(cfg.ScrapeConfigs))
	}
}
