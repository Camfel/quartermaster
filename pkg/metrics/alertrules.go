// Package metrics — VictoriaMetrics alerting rules generator.
//
// GenerateAlertRules produces a VictoriaMetrics-compatible alerting rules
// YAML file.  Rules cover host health (disk, CPU, memory), container
// availability, reconciliation health, and LKG status.
//
// The rules are evaluated by VictoriaMetrics.  Notifications are
// delivered via the Gotify push server configured in the daemon
// (direct HTTP POST, no scrape delay).  The rules file is written
// alongside vm-scrape.yml so VictoriaMetrics can load both.
package metrics

import (
	"log"

	"gopkg.in/yaml.v3"
)

// ── YAML marshalling types ────────────────────────────────────────────

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type alertGroup struct {
	Name  string      `yaml:"name"`
	Rules []alertRule `yaml:"rules"`
}

// ── Alert rules generation ────────────────────────────────────────────

// GenerateAlertRules produces a VictoriaMetrics-compatible alerting rules
// YAML file.  gotifyURL is included in rule annotations for documentation;
// actual notification delivery is handled by the daemon via direct Gotify
// HTTP POST for zero-latency critical alerts.
func GenerateAlertRules(gotifyURL string) []byte {
	rules := []alertRule{
		// ── Host alerts (from node_exporter metrics) ──────────────
		{
			Alert: "HostDiskFull",
			Expr:  `(node_filesystem_avail_bytes{fstype!="tmpfs"} / node_filesystem_size_bytes) < 0.1`,
			For:   "5m",
			Labels: map[string]string{
				"severity": "critical",
			},
			Annotations: map[string]string{
				"summary":     "Host disk is almost full ({{ $labels.instance }})",
				"description": "Filesystem {{ $labels.mountpoint }} has less than 10% free space.",
				"gotify_url":  gotifyURL,
			},
		},
		{
			Alert: "HostHighCPU",
			Expr:  `avg by(instance)(rate(node_cpu_seconds_total{mode!="idle"}[5m])) > 0.9`,
			For:   "5m",
			Labels: map[string]string{
				"severity": "warning",
			},
			Annotations: map[string]string{
				"summary":     "High CPU usage on host ({{ $labels.instance }})",
				"description": "CPU usage has been above 90% for 5 minutes.",
				"gotify_url":  gotifyURL,
			},
		},
		{
			Alert: "HostHighMem",
			Expr:  `(node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) < 0.1`,
			For:   "5m",
			Labels: map[string]string{
				"severity": "critical",
			},
			Annotations: map[string]string{
				"summary":     "Host memory is almost exhausted ({{ $labels.instance }})",
				"description": "Available memory has dropped below 10%.",
				"gotify_url":  gotifyURL,
			},
		},

		// ── Quartermaster alerts (from qm_* metrics) ──────────────
		{
			Alert: "ContainerDown",
			Expr:  `qm_containers_desired - qm_containers_running > 0`,
			For:   "5m",
			Labels: map[string]string{
				"severity": "critical",
			},
			Annotations: map[string]string{
				"summary":     "One or more containers are not running",
				"description": "Desired containers exceed running containers for 5 minutes.  Check qm status for details.",
				"gotify_url":  gotifyURL,
			},
		},
		{
			Alert: "ReconcileFailing",
			Expr:  `rate(qm_reconcile_total{outcome="error"}[15m]) > 0`,
			For:   "5m",
			Labels: map[string]string{
				"severity": "warning",
			},
			Annotations: map[string]string{
				"summary":     "Reconciliation is failing",
				"description": "Quartermaster reconciliation has been producing errors.  Check daemon logs.",
				"gotify_url":  gotifyURL,
			},
		},
		{
			Alert: "LKGDegraded",
			Expr:  `qm_lkg_healthy == 0`,
			For:   "5m",
			Labels: map[string]string{
				"severity": "critical",
			},
			Annotations: map[string]string{
				"summary":     "No valid Last Known Good manifest",
				"description": "Quartermaster has no healthy LKG manifest.  If reconciliation fails, rollback is impossible.",
				"gotify_url":  gotifyURL,
			},
		},
	}

	group := alertGroup{
		Name:  "quartermaster",
		Rules: rules,
	}

	out, err := yaml.Marshal(struct {
		Groups []alertGroup `yaml:"groups"`
	}{Groups: []alertGroup{group}})
	if err != nil {
		log.Printf("Warning: failed to marshal alert rules: %v", err)
		return nil
	}
	return out
}
