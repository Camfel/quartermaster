package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"quartermaster/pkg/cri"
)

// ── Registry isolation ──────────────────────────────────────────────────

func TestNew_CreatesIsolatedRegistry(t *testing.T) {
	m := New()
	if m.reg == nil {
		t.Fatal("expected non-nil registry")
	}
	// Verify the custom registry has metrics.  CounterVec and HistogramVec
	// families only appear in Gather() after at least one child is created,
	// so we check that the gauges (always present) and the histogram are here.
	ourFams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	alwaysPresent := map[string]bool{
		"qm_reconcile_duration_seconds": true,
		"qm_containers_desired":         true,
		"qm_containers_running":         true,
		"qm_containers_unhealthy":       true,
		"qm_bridge_ips_used":            true,
		"qm_bridge_ips_free":            true,
		"qm_lkg_healthy":                true,
	}
	found := make(map[string]bool)
	for _, f := range ourFams {
		if !strings.HasPrefix(f.GetName(), "qm_") {
			t.Errorf("unexpected non-QM metric %q in custom registry", f.GetName())
		}
		found[f.GetName()] = true
	}
	for name := range alwaysPresent {
		if !found[name] {
			t.Errorf("expected metric %q missing from custom registry", name)
		}
	}

	// Verify no QM metrics leak to the default registry.
	defFams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range defFams {
		if strings.HasPrefix(f.GetName(), "qm_") {
			t.Errorf("QM metric %q leaked to default registry", f.GetName())
		}
	}
}

// ── Reconciliation metrics ──────────────────────────────────────────────

func TestRecordReconcile(t *testing.T) {
	m := New()
	m.RecordReconcile("success", 250*time.Millisecond)
	m.RecordReconcile("error", 1*time.Second)

	fams := gatherFams(t, m)

	// Check per-label counter values.
	checkLabelCounter(t, fams, "qm_reconcile_total", []string{"outcome"}, map[string]float64{
		"success": 1,
		"error":   1,
	})

	// Check histogram sample count.
	rh := findHistogram(t, fams, "qm_reconcile_duration_seconds")
	if rh == nil {
		t.Fatal("qm_reconcile_duration_seconds not found")
	}
	if rh.GetSampleCount() != 2 {
		t.Errorf("expected 2 observations, got %d", rh.GetSampleCount())
	}
}

// ── Container metrics ───────────────────────────────────────────────────

func TestSetContainers(t *testing.T) {
	m := New()
	m.SetContainers(12, 10)

	fams := gatherFams(t, m)

	des := findGauge(t, fams, "qm_containers_desired")
	if des == nil || des.GetValue() != 12 {
		t.Errorf("expected 12 desired, got %v", gaugeVal(des))
	}
	run := findGauge(t, fams, "qm_containers_running")
	if run == nil || run.GetValue() != 10 {
		t.Errorf("expected 10 running, got %v", gaugeVal(run))
	}
}

func TestSetUnhealthy(t *testing.T) {
	m := New()
	m.SetUnhealthy(3)

	fams := gatherFams(t, m)

	unh := findGauge(t, fams, "qm_containers_unhealthy")
	if unh == nil || unh.GetValue() != 3 {
		t.Errorf("expected 3 unhealthy, got %v", gaugeVal(unh))
	}
}

// ── Health-check metrics ───────────────────────────────────────────────

func TestRecordHealthCheck(t *testing.T) {
	m := New()
	m.RecordHealthCheck("authelia", "http", "pass", 123*time.Millisecond)
	m.RecordHealthCheck("authelia", "http", "fail", 456*time.Millisecond)
	m.RecordHealthCheck("jellyfin", "tcp", "pass", 78*time.Millisecond)

	fams := gatherFams(t, m)

	// Verify per-label counter values.
	checkLabeledCounter(t, fams, "qm_health_check_total",
		[]string{"service", "type", "result"},
		map[string]float64{
			"authelia|http|pass": 1,
			"authelia|http|fail": 1,
			"jellyfin|tcp|pass":  1,
		})

	// Verify histogram per-label observation counts.
	chkLabeledHistogram(t, fams, "qm_health_check_duration_seconds",
		[]string{"service", "type"},
		map[string]uint64{
			"authelia|http": 2,
			"jellyfin|tcp":  1,
		})
}

// ── Bridge IP metrics ──────────────────────────────────────────────────

func TestSetBridgeIPs(t *testing.T) {
	m := New()
	m.SetBridgeIPs(14, 239)

	fams := gatherFams(t, m)

	used := findGauge(t, fams, "qm_bridge_ips_used")
	if used == nil || used.GetValue() != 14 {
		t.Errorf("expected 14 used, got %v", gaugeVal(used))
	}
	free := findGauge(t, fams, "qm_bridge_ips_free")
	if free == nil || free.GetValue() != 239 {
		t.Errorf("expected 239 free, got %v", gaugeVal(free))
	}
}

// ── LKG metric ─────────────────────────────────────────────────────────

func TestSetLKGHealthy(t *testing.T) {
	m := New()
	m.SetLKGHealthy(true)

	fams := gatherFams(t, m)
	g := findGauge(t, fams, "qm_lkg_healthy")
	if g == nil || g.GetValue() != 1 {
		t.Errorf("expected LKG healthy = 1, got %v", gaugeVal(g))
	}

	m.SetLKGHealthy(false)
	fams = gatherFams(t, m)
	g = findGauge(t, fams, "qm_lkg_healthy")
	if g == nil || g.GetValue() != 0 {
		t.Errorf("expected LKG healthy = 0, got %v", gaugeVal(g))
	}
}

// ── Per-container resource metrics ──────────────────────────────────────

func TestRecordContainerStats(t *testing.T) {
	m := New()

	m.RecordContainerStats("jellyfin", &cri.ContainerStats{
		CPUUsageSeconds:  12.5,
		MemoryUsageBytes: 512 * 1024 * 1024,
		MemoryLimitBytes: 1024 * 1024 * 1024,
	})
	m.RecordContainerStats("sonarr", &cri.ContainerStats{
		CPUUsageSeconds:  3.1,
		MemoryUsageBytes: 256 * 1024 * 1024,
		MemoryLimitBytes: 0,
	})

	fams := gatherFams(t, m)

	// CPU counter is cumulative — check per-service delta.
	checkLabelCounter(t, fams, "qm_container_cpu_seconds_total", []string{"service"}, map[string]float64{
		"jellyfin": 12.5,
		"sonarr":   3.1,
	})

	// Memory gauges.
	jellyMem := findGaugeVec(t, fams, "qm_container_memory_bytes", "service", "jellyfin")
	if jellyMem == nil || jellyMem.GetValue() != 512*1024*1024 {
		t.Errorf("jellyfin memory: expected %d, got %v", 512*1024*1024, gaugeVal(jellyMem))
	}

	sonarrMemLimit := findGaugeVec(t, fams, "qm_container_memory_limit_bytes", "service", "sonarr")
	if sonarrMemLimit == nil || sonarrMemLimit.GetValue() != 0 {
		t.Errorf("sonarr mem limit: expected 0, got %v", gaugeVal(sonarrMemLimit))
	}
}

func TestResetContainerStats(t *testing.T) {
	m := New()

	m.RecordContainerStats("radarr", &cri.ContainerStats{
		CPUUsageSeconds:  5.0,
		MemoryUsageBytes: 128 * 1024 * 1024,
		MemoryLimitBytes: 512 * 1024 * 1024,
	})

	// Reset should remove the label values.
	m.ResetContainerStats("radarr")

	fams := gatherFams(t, m)

	// After reset, the "radarr" labelled series should be gone.
	for _, f := range fams {
		for _, metric := range f.GetMetric() {
			for _, lbl := range metric.GetLabel() {
				if lbl.GetName() == "service" && lbl.GetValue() == "radarr" {
					t.Errorf("metric %q still has radarr label after reset", f.GetName())
				}
			}
		}
	}
}

func TestRecordContainerStats_NilStats(t *testing.T) {
	m := New()
	// Must not panic.
	m.RecordContainerStats("test", nil)
}

// ── HTTP handler ────────────────────────────────────────────────────────

func TestHandler_ReturnsPrometheusText(t *testing.T) {
	m := New()
	m.RecordReconcile("success", 100*time.Millisecond)
	m.SetContainers(5, 5)

	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "qm_reconcile_total") {
		t.Errorf("expected qm_reconcile_total in output, got:\n%s", body)
	}
	if !strings.Contains(body, "qm_containers_desired") {
		t.Errorf("expected qm_containers_desired in output, got:\n%s", body)
	}
}

func TestHandler_ContentType(t *testing.T) {
	m := New()
	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain content-type, got %q", ct)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────

func gatherFams(t *testing.T, m *Metrics) []*dto.MetricFamily {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return fams
}

func findGauge(t *testing.T, fams []*dto.MetricFamily, name string) *dto.Gauge {
	t.Helper()
	for _, f := range fams {
		if f.GetName() == name {
			if len(f.GetMetric()) == 0 {
				return nil
			}
			return f.GetMetric()[0].GetGauge()
		}
	}
	return nil
}

// findGaugeVec returns the Gauge for a specific label/value pair within a GaugeVec.
func findGaugeVec(t *testing.T, fams []*dto.MetricFamily, name, labelName, wantValue string) *dto.Gauge {
	t.Helper()
	for _, f := range fams {
		if f.GetName() == name {
			for _, m := range f.GetMetric() {
				if labelValue(m, labelName) == wantValue {
					return m.GetGauge()
				}
			}
			return nil
		}
	}
	return nil
}

func findCounter(t *testing.T, fams []*dto.MetricFamily, name string) *dto.Counter {
	t.Helper()
	for _, f := range fams {
		if f.GetName() == name {
			var val float64
			for _, m := range f.GetMetric() {
				val += m.GetCounter().GetValue()
			}
			return &dto.Counter{Value: &val}
		}
	}
	return nil
}

func findHistogram(t *testing.T, fams []*dto.MetricFamily, name string) *dto.Histogram {
	t.Helper()
	for _, f := range fams {
		if f.GetName() == name {
			var sampleCount uint64
			for _, m := range f.GetMetric() {
				sampleCount += m.GetHistogram().GetSampleCount()
			}
			return &dto.Histogram{SampleCount: &sampleCount}
		}
	}
	return nil
}

// ── Label-aware helpers ────────────────────────────────────────────────

// checkLabelCounter verifies a CounterVec's per-label values.
// labels are the label names; expected maps label-value-combos (joined by "|") to counts.
func checkLabelCounter(t *testing.T, fams []*dto.MetricFamily, name string, labels []string, expected map[string]float64) {
	t.Helper()
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		if f.GetType() != dto.MetricType_COUNTER {
			t.Errorf("%s: expected COUNTER type, got %v", name, f.GetType())
			return
		}
		for _, m := range f.GetMetric() {
			keyParts := make([]string, len(labels))
			for i, lbl := range labels {
				keyParts[i] = labelValue(m, lbl)
			}
			key := strings.Join(keyParts, "|")
			if want, ok := expected[key]; ok {
				got := m.GetCounter().GetValue()
				if got != want {
					t.Errorf("%s{%s}: expected %v, got %v", name, key, want, got)
				}
				delete(expected, key)
			}
		}
		for key := range expected {
			t.Errorf("%s{%s}: expected but not found", name, key)
		}
		return
	}
	t.Errorf("%s: metric family not found", name)
}

// checkLabeledCounter is an alias for checkLabelCounter for clarity.
func checkLabeledCounter(t *testing.T, fams []*dto.MetricFamily, name string, labels []string, expected map[string]float64) {
	t.Helper()
	checkLabelCounter(t, fams, name, labels, expected)
}

// chkLabeledHistogram verifies a HistogramVec's per-label observation counts.
func chkLabeledHistogram(t *testing.T, fams []*dto.MetricFamily, name string, labels []string, expected map[string]uint64) {
	t.Helper()
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		if f.GetType() != dto.MetricType_HISTOGRAM {
			t.Errorf("%s: expected HISTOGRAM type, got %v", name, f.GetType())
			return
		}
		for _, m := range f.GetMetric() {
			keyParts := make([]string, len(labels))
			for i, lbl := range labels {
				keyParts[i] = labelValue(m, lbl)
			}
			key := strings.Join(keyParts, "|")
			if want, ok := expected[key]; ok {
				got := m.GetHistogram().GetSampleCount()
				if got != want {
					t.Errorf("%s{%s}: expected %d observations, got %d", name, key, want, got)
				}
				delete(expected, key)
			}
		}
		for key := range expected {
			t.Errorf("%s{%s}: expected but not found", name, key)
		}
		return
	}
	t.Errorf("%s: metric family not found", name)
}

// labelValue returns the value of a label on a metric, or "" if not present.
func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func gaugeVal(g *dto.Gauge) float64 {
	if g == nil {
		return -1
	}
	return g.GetValue()
}
