// Package metrics exposes Quartermaster operational metrics in Prometheus
// text format via a /v1/metrics HTTP endpoint.  All metrics are registered
// on a dedicated prometheus.Registry so they never leak into the global
// default registry.
//
// Usage:
//
//	m := metrics.New()
//	m.RecordReconcile("success", 250*time.Millisecond)
//	mux.Handle("/v1/metrics", m.Handler())
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"quartermaster/pkg/cri"
)

// Metrics holds all Quartermaster Prometheus metrics and a dedicated registry.
type Metrics struct {
	reg *prometheus.Registry

	// ── Reconciliation ──────────────────────────────────────────────
	reconcileTotal    *prometheus.CounterVec
	reconcileDuration prometheus.Histogram

	// ── Containers ──────────────────────────────────────────────────
	containersDesired   prometheus.Gauge
	containersRunning   prometheus.Gauge
	containersUnhealthy prometheus.Gauge

	// ── Health checks ───────────────────────────────────────────────
	healthCheckTotal    *prometheus.CounterVec
	healthCheckDuration *prometheus.HistogramVec

	// ── Bridge IPAM ─────────────────────────────────────────────────
	bridgeIPsUsed prometheus.Gauge
	bridgeIPsFree prometheus.Gauge

	// ── LKG status ──────────────────────────────────────────────────
	lkgHealthy prometheus.Gauge

	// ── Per-container resource usage ────────────────────────────────
	containerCPUSecs  *prometheus.CounterVec
	containerMemBytes *prometheus.GaugeVec
	containerMemLimit *prometheus.GaugeVec
}

// New creates a Metrics instance with all counters, gauges, and histograms
// pre-registered on a private prometheus.Registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		reg: reg,

		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qm_reconcile_total",
			Help: "Total number of reconciliation passes, labelled by outcome (success/error).",
		}, []string{"outcome"}),

		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "qm_reconcile_duration_seconds",
			Help:    "Duration of reconciliation passes in seconds.",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60},
		}),

		containersDesired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qm_containers_desired",
			Help: "Number of services declared in the merged manifest.",
		}),

		containersRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qm_containers_running",
			Help: "Number of containers currently in running state.",
		}),

		containersUnhealthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qm_containers_unhealthy",
			Help: "Number of containers currently failing health checks.",
		}),

		healthCheckTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qm_health_check_total",
			Help: "Total number of health-check probe results, labelled by service, type, and result (pass/fail).",
		}, []string{"service", "type", "result"}),

		healthCheckDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "qm_health_check_duration_seconds",
			Help:    "Duration of health-check probes in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "type"}),

		bridgeIPsUsed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qm_bridge_ips_used",
			Help: "Number of bridge IP addresses currently allocated to services.",
		}),

		bridgeIPsFree: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qm_bridge_ips_free",
			Help: "Number of bridge IP addresses still available (out of 253 usable).",
		}),

		lkgHealthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qm_lkg_healthy",
			Help: "Whether a valid Last Known Good manifest is available (1 = healthy, 0 = degraded).",
		}),

		containerCPUSecs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qm_container_cpu_seconds_total",
			Help: "Cumulative CPU time consumed by the container, in seconds.",
		}, []string{"service"}),

		containerMemBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qm_container_memory_bytes",
			Help: "Current memory usage of the container, in bytes.",
		}, []string{"service"}),

		containerMemLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qm_container_memory_limit_bytes",
			Help: "Memory limit of the container, in bytes (0 if unlimited).",
		}, []string{"service"}),
	}

	// ── Register all collectors ───────────────────────────────────
	reg.MustRegister(
		m.reconcileTotal,
		m.reconcileDuration,
		m.containersDesired,
		m.containersRunning,
		m.containersUnhealthy,
		m.healthCheckTotal,
		m.healthCheckDuration,
		m.bridgeIPsUsed,
		m.bridgeIPsFree,
		m.lkgHealthy,
		m.containerCPUSecs,
		m.containerMemBytes,
		m.containerMemLimit,
	)

	return m
}

// ── Public instrumentation methods ─────────────────────────────────────

// RecordReconcile increments the reconcile counter and observes the duration.
// outcome should be "success" or "error".
func (m *Metrics) RecordReconcile(outcome string, duration time.Duration) {
	m.reconcileTotal.WithLabelValues(outcome).Inc()
	m.reconcileDuration.Observe(duration.Seconds())
}

// SetContainers records the desired and running container counts.
func (m *Metrics) SetContainers(desired, running int) {
	m.containersDesired.Set(float64(desired))
	m.containersRunning.Set(float64(running))
}

// SetUnhealthy records the number of containers currently unhealthy.
func (m *Metrics) SetUnhealthy(count int) {
	m.containersUnhealthy.Set(float64(count))
}

// RecordHealthCheck increments the health-check counter and observes duration.
// checkType is "http", "tcp", or "none"; result is "pass" or "fail".
func (m *Metrics) RecordHealthCheck(service, checkType, result string, duration time.Duration) {
	m.healthCheckTotal.WithLabelValues(service, checkType, result).Inc()
	m.healthCheckDuration.WithLabelValues(service, checkType).Observe(duration.Seconds())
}

// SetBridgeIPs records how many bridge IPs are used/free.
func (m *Metrics) SetBridgeIPs(used, free int) {
	m.bridgeIPsUsed.Set(float64(used))
	m.bridgeIPsFree.Set(float64(free))
}

// SetLKGHealthy sets the LKG gauge (1 = healthy, 0 = degraded).
func (m *Metrics) SetLKGHealthy(healthy bool) {
	if healthy {
		m.lkgHealthy.Set(1)
	} else {
		m.lkgHealthy.Set(0)
	}
}

// RecordContainerStats records per-container CPU and memory metrics.
// Call this after each successful reconciliation pass for every running
// container.  Stats may be nil (container not yet running).
func (m *Metrics) RecordContainerStats(service string, stats *cri.ContainerStats) {
	if stats == nil {
		return
	}
	// CPU is cumulative — add the delta.
	if stats.CPUUsageSeconds > 0 {
		m.containerCPUSecs.WithLabelValues(service).Add(stats.CPUUsageSeconds)
	}
	m.containerMemBytes.WithLabelValues(service).Set(float64(stats.MemoryUsageBytes))
	m.containerMemLimit.WithLabelValues(service).Set(float64(stats.MemoryLimitBytes))
}

// ResetContainerStats removes all per-container metrics for a service.
// Call when a container is stopped or deleted so stale series don't
// persist in the Prometheus output.
func (m *Metrics) ResetContainerStats(service string) {
	m.containerCPUSecs.DeleteLabelValues(service)
	m.containerMemBytes.DeleteLabelValues(service)
	m.containerMemLimit.DeleteLabelValues(service)
}

// ── HTTP ───────────────────────────────────────────────────────────────

// Handler returns an http.Handler that serves all registered metrics in
// Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		Registry: m.reg,
	})
}
