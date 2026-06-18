package metrics

import (
	"log"

	"gopkg.in/yaml.v3"
)

// ScrapeTarget describes a single Prometheus/VictoriaMetrics scrape target.
type ScrapeTarget struct {
	JobName     string // unique job name (service name or "quartermaster")
	Address     string // "host:port"
	MetricsPath string // e.g. "/metrics" (default if empty)
}

// ── YAML marshalling types ────────────────────────────────────────────

type scrapeJob struct {
	JobName       string         `yaml:"job_name"`
	StaticConfigs []staticConfig `yaml:"static_configs"`
	MetricsPath   string         `yaml:"metrics_path,omitempty"`
}

type staticConfig struct {
	Targets []string `yaml:"targets"`
}

// ── Scrape config generation ──────────────────────────────────────────

// GenerateScrapeConfig produces a VictoriaMetrics-compatible scrape.yml.
// It always includes static infrastructure targets (quartermaster itself,
// node_exporter, victoria-metrics self-scrape).  Service targets from the
// manifest are added if non-empty.
//
// The returned YAML can be written directly to a file that VictoriaMetrics
// reads via -promscrape.config.
func GenerateScrapeConfig(services []ScrapeTarget) []byte {
	// ── Static infrastructure targets ──────────────────────────────
	jobs := []scrapeJob{
		{
			JobName: "quartermaster",
			StaticConfigs: []staticConfig{
				{Targets: []string{"127.0.0.1:9098"}},
			},
			MetricsPath: "/v1/metrics",
		},
		{
			JobName: "node_exporter",
			StaticConfigs: []staticConfig{
				{Targets: []string{"127.0.0.1:9100"}},
			},
		},
		{
			JobName: "victoria-metrics",
			StaticConfigs: []staticConfig{
				{Targets: []string{"127.0.0.1:8428"}},
			},
		},
	}

	// ── Service targets from the manifest ─────────────────────────
	for _, svc := range services {
		job := scrapeJob{
			JobName: svc.JobName,
			StaticConfigs: []staticConfig{
				{Targets: []string{svc.Address}},
			},
		}
		if svc.MetricsPath != "" {
			job.MetricsPath = svc.MetricsPath
		}
		jobs = append(jobs, job)
	}

	// ── Marshal to YAML ───────────────────────────────────────────
	out, err := yaml.Marshal(struct {
		ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
	}{ScrapeConfigs: jobs})
	if err != nil {
		log.Printf("Warning: failed to marshal scrape config: %v", err)
		return nil
	}
	return out
}
