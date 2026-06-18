package reconciler

import (
	"context"
	"testing"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/types"
)

func TestReconcile_CreateNew(t *testing.T) {
	// Setup
	mockCC := cri.NewMockContainerClient()
	mockCC.OnCreateContainer = func(svc types.Service) (string, error) {
		return "new-id", nil
	}
	cm := config.NewConfigManager()
	r := NewReconciler(mockCC, cm)

	// Use path relative to the test file
	configPath := "../config/testdata/valid.yaml"
	ctx := context.Background()
	err := r.Reconcile(ctx, configPath)

	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
}

func TestReconcile_RemoveOld(t *testing.T) {
	// Setup: One container is running that isn't in the valid.yaml
	mockCC := cri.NewMockContainerClient()
	mockCC.OnListContainers = func() ([]cri.ContainerInfo, error) {
		return []cri.ContainerInfo{
			{ID: "ghost-id", Name: "ghost-service", Image: "alpine"},
		}, nil
	}
	mockCC.OnStopContainer = func(id string) error {
		if id != "ghost-id" {
			t.Errorf("Expected to stop ghost-id, got %s", id)
		}
		return nil
	}
	mockCC.OnDeleteContainer = func(id string) error {
		if id != "ghost-id" {
			t.Errorf("Expected to delete ghost-id, got %s", id)
		}
		return nil
	}
	cm := config.NewConfigManager()
	r := NewReconciler(mockCC, cm)

	// Use path relative to the test file
	configPath := "../config/testdata/valid.yaml"
	ctx := context.Background()
	err := r.Reconcile(ctx, configPath)

	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
}

func TestTopologicalSort(t *testing.T) {
	tests := []struct {
		name     string
		input    []types.Service
		expected []string // expected order of names
	}{
		{
			name:     "empty",
			input:    []types.Service{},
			expected: []string{},
		},
		{
			name: "no dependencies",
			input: []types.Service{
				{Name: "a", Image: "alpine"},
				{Name: "b", Image: "alpine"},
			},
			expected: []string{"a", "b"},
		},
		{
			name: "simple dependency",
			input: []types.Service{
				{Name: "web", Image: "nginx", DependsOn: []string{"db"}},
				{Name: "db", Image: "postgres"},
			},
			// db must come before web
			expected: []string{"db", "web"},
		},
		{
			name: "chain dependency",
			input: []types.Service{
				{Name: "app", Image: "app", DependsOn: []string{"web"}},
				{Name: "web", Image: "nginx", DependsOn: []string{"db"}},
				{Name: "db", Image: "postgres"},
			},
			expected: []string{"db", "web", "app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sorted := topologicalSort(tt.input)
			// Verify order
			for i, name := range tt.expected {
				if i >= len(sorted) {
					t.Errorf("expected at least %d results, got %d", len(tt.expected), len(sorted))
					return
				}
				if sorted[i].Name != name {
					t.Errorf("position %d: expected %q, got %q", i, name, sorted[i].Name)
				}
			}
		})
	}
}

func TestServiceConfigHash(t *testing.T) {
	svc1 := &types.Service{Name: "web", Image: "nginx:1.25", Env: []types.EnvVar{{Name: "PORT", Value: "80"}}}
	svc2 := &types.Service{Name: "web", Image: "nginx:1.25", Env: []types.EnvVar{{Name: "PORT", Value: "80"}}}
	svc3 := &types.Service{Name: "web", Image: "nginx:1.26", Env: []types.EnvVar{{Name: "PORT", Value: "80"}}}

	h1 := serviceConfigHash(svc1)
	h2 := serviceConfigHash(svc2)
	h3 := serviceConfigHash(svc3)

	if h1 != h2 {
		t.Error("identical services should have identical hashes")
	}
	if h1 == h3 {
		t.Error("services with different images should have different hashes")
	}
	if len(h1) != 64 {
		t.Errorf("expected SHA256 hash (64 chars), got %d", len(h1))
	}
}

func TestServiceConfigHash_DifferentFields(t *testing.T) {
	base := &types.Service{Name: "web", Image: "alpine"}
	baseHash := serviceConfigHash(base)

	// Different env
	svc := &types.Service{Name: "web", Image: "alpine", Env: []types.EnvVar{{Name: "X", Value: "1"}}}
	if serviceConfigHash(svc) == baseHash {
		t.Error("different env should produce different hash")
	}

	// Different ports
	svc = &types.Service{Name: "web", Image: "alpine", Ports: []types.Port{{Host: 80, Container: 80}}}
	if serviceConfigHash(svc) == baseHash {
		t.Error("different ports should produce different hash")
	}

	// Different user
	svc = &types.Service{Name: "web", Image: "alpine", User: "1000:1000"}
	if serviceConfigHash(svc) == baseHash {
		t.Error("different user should produce different hash")
	}

	// Different network profile
	svc = &types.Service{Name: "web", Image: "alpine", Network: "internal"}
	if serviceConfigHash(svc) == baseHash {
		t.Error("different network should produce different hash")
	}

	// Name change should NOT affect hash (name is identity, not config)
	svc = &types.Service{Name: "different-name", Image: "alpine"}
	if serviceConfigHash(svc) != baseHash {
		t.Error("different name should NOT change config hash")
	}
}
