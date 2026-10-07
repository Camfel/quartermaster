package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quartermaster/pkg/cri"
	"quartermaster/pkg/metrics"
	"quartermaster/pkg/types"
)

func TestRecordContainerStats(t *testing.T) {
	mock := cri.NewMockContainerClient()
	mock.OnContainerStats = func(string) (*cri.ContainerStats, error) {
		return &cri.ContainerStats{CPUUsageSeconds: 1, MemoryUsageBytes: 2, MemoryLimitBytes: 3}, nil
	}
	m := metrics.New()
	d := &Daemon{containerClient: mock, metrics: m}

	stack := &types.Stack{Spec: types.StackSpec{Services: []types.Service{
		{Name: "svc-a"},
		{Name: "svc-b"},
	}}}

	// Both running: both series are populated.
	d.recordContainerStats(context.Background(), []cri.ContainerInfo{
		{ID: "a", Name: "svc-a", Running: true},
		{ID: "b", Name: "svc-b", Running: true},
	}, stack)

	body := scrape(t, m)
	if !strings.Contains(body, `qm_container_cpu_seconds{service="svc-a"} 1`) {
		t.Errorf("expected svc-a CPU gauge, got:\n%s", body)
	}
	if !strings.Contains(body, `qm_container_cpu_seconds{service="svc-b"} 1`) {
		t.Errorf("expected svc-b CPU gauge, got:\n%s", body)
	}

	// svc-b no longer running: its series is removed, svc-a stays.
	d.recordContainerStats(context.Background(), []cri.ContainerInfo{
		{ID: "a", Name: "svc-a", Running: true},
		{ID: "b", Name: "svc-b", Running: false},
	}, stack)

	body = scrape(t, m)
	if !strings.Contains(body, `qm_container_cpu_seconds{service="svc-a"} 1`) {
		t.Errorf("expected svc-a to remain, got:\n%s", body)
	}
	if strings.Contains(body, `service="svc-b"`) {
		t.Errorf("expected svc-b series to be reset, got:\n%s", body)
	}

	// svc-b is recreated, then removed from the manifest entirely: its series
	// must still be dropped rather than lingering until restart.
	d.recordContainerStats(context.Background(), []cri.ContainerInfo{
		{ID: "a", Name: "svc-a", Running: true},
		{ID: "b", Name: "svc-b", Running: true},
	}, stack)
	d.recordContainerStats(context.Background(), []cri.ContainerInfo{
		{ID: "a", Name: "svc-a", Running: true},
	}, &types.Stack{Spec: types.StackSpec{Services: []types.Service{{Name: "svc-a"}}}})

	body = scrape(t, m)
	if strings.Contains(body, `service="svc-b"`) {
		t.Errorf("expected removed svc-b series to be dropped, got:\n%s", body)
	}
}

func TestUpdateUnhealthyMetric(t *testing.T) {
	m := metrics.New()
	d := &Daemon{metrics: m, status: &Status{}}

	healthy := true
	unhealthy := false
	d.status.Containers = []ContainerStatus{
		{Name: "ok", Healthy: &healthy},
		{Name: "bad", Healthy: &unhealthy},
		{Name: "unknown"},
	}
	d.updateUnhealthyMetric()

	body := scrape(t, m)
	if !strings.Contains(body, "qm_containers_unhealthy 1") {
		t.Errorf("expected one unhealthy container, got:\n%s", body)
	}
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}
