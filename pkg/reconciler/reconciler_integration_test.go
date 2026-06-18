//go:build integration

package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconciler_Integration(t *testing.T) {
	// 1. Setup Environment
	// Use a dedicated namespace for testing to avoid messing with host containers
	testNamespace := "quartermaster-test"
	socketPath := "/run/containerd/containerd.sock"

	// Initialize CRI Client
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	require.NoError(t, err, "Failed to connect to containerd")

	// Setup Config Manager with a temp directory
	tmpDir, err := os.MkdirTemp("", "qm-integration-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	cm := config.NewConfigManager()

	// Initialize Reconciler
	reconciler := NewReconciler(cc, cm)

	// 2. Test Case: Create Service
	t.Run("CreateService", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		stackName := "test-stack-create"
		stackPath := filepath.Join(tmpDir, stackName+".yaml")

		stackContent := `
version: "1"
kind: Stack
metadata:
  name: test-stack
spec:
  services:
    - name: alpine-service
      image: docker.io/library/alpine:latest
      restart_policy: always
`
		err := os.WriteFile(stackPath, []byte(stackContent), 0644)
		require.NoError(t, err)

		// Run reconciliation
		err = reconciler.Reconcile(ctx, stackPath)
		require.NoError(t, err)

		// Verify container exists
		containers, err := cc.ListContainers(ctx)
		require.NoError(t, err)

		found := false
		for _, c := range containers {
			// Since we aren't using labels, we can't rely on c.Name being "alpine-service"
			// unless we check the image.
			if c.Image == "docker.io/library/alpine:latest" {
				found = true
				break
			}
		}
		assert.True(t, found, "Expected a container with alpine:latest image to be created")
	})

	// 3. Test Case: Delete Service
	t.Run("DeleteService", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Use the same stack but with NO services
		stackName := "test-stack-delete"
		stackPath := filepath.Join(tmpDir, stackName+".yaml")

		stackContent := `
version: "1"
kind: Stack
metadata:
  name: empty-stack
spec:
  services: []
`
		err := os.WriteFile(stackPath, []byte(stackContent), 0644)
		require.NoError(t, err)

		// Run reconciliation
		err = reconciler.Reconcile(ctx, stackPath)
		require.NoError(t, err)

		// Verify container is gone
		containers, err := cc.ListContainers(ctx)
		require.NoError(t, err)

		for _, c := range containers {
			assert.NotEqual(t, "docker.io/library/alpine:latest", c.Image, "alpine:latest container should have been deleted")
		}
	})
}
