package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/reconciler"
	"quartermaster/pkg/types"
)

func newTestDaemon(t *testing.T, lkgPath string) (*Daemon, *cri.MockContainerClient) {
	t.Helper()
	cm := config.NewConfigManager()
	mock := cri.NewMockContainerClient()
	return &Daemon{
		reconciler:    reconciler.NewReconciler(mock, cm),
		configManager: cm,
		lkgPath:       lkgPath,
		syncInterval:  time.Minute,
		status:        &Status{},
	}, mock
}

func TestRollbackToLKG_Success(t *testing.T) {
	lkgPath := filepath.Join(t.TempDir(), "lkg.yaml")
	cm := config.NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "lkg"},
		Spec: types.StackSpec{Services: []types.Service{
			{Name: "svc", Image: "alpine"},
		}},
	}
	if err := cm.SaveStack(lkgPath, stack); err != nil {
		t.Fatal(err)
	}

	d, mock := newTestDaemon(t, lkgPath)
	if !d.rollbackToLKG(context.Background()) {
		t.Fatal("expected rollback to succeed")
	}
	if !d.status.lkgHealthy() {
		t.Error("expected LKG to be marked healthy after a successful rollback")
	}

	// The LKG service should have been created by the rollback pass.
	containers, err := mock.ListContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Name != "svc" {
		t.Errorf("expected the LKG service to be reconciled, got %+v", containers)
	}
}

func TestRollbackToLKG_InvalidManifest(t *testing.T) {
	// Missing LKG file: rollback must fail cleanly and mark LKG unhealthy.
	d, _ := newTestDaemon(t, filepath.Join(t.TempDir(), "missing.yaml"))

	if d.rollbackToLKG(context.Background()) {
		t.Fatal("expected rollback to fail for a missing LKG manifest")
	}
	if d.status.lkgHealthy() {
		t.Error("expected LKG to be marked unhealthy when the manifest is invalid")
	}
}
