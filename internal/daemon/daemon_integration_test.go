//go:build integration

// Package daemon_test runs an end-to-end test of the qm-daemon binary.
package daemon_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"quartermaster/pkg/cri"

	"github.com/stretchr/testify/require"
)

const (
	testNamespace  = "quartermaster-e2e-test"
	containerdSock = "/run/containerd/containerd.sock"
)

func TestDaemonEndToEnd(t *testing.T) {
	// ── 0. Clean up leftovers ─────────────────────────────────────
	cleanupTestNamespace(t)

	// ── 1. Temp directories ───────────────────────────────────────
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "etc", "quartermaster")
	secretsDir := filepath.Join(settingsDir, "secrets")
	lkgDir := filepath.Join(tmpDir, "var", "lib", "quartermaster")
	socketDir := filepath.Join(tmpDir, "run", "quartermaster")
	for _, d := range []string{settingsDir, secretsDir, lkgDir, socketDir} {
		require.NoError(t, os.MkdirAll(d, 0755))
	}

	// ── 2. Write settings.json ────────────────────────────────────
	settingsJSON := `{
  "containerd_socket": "` + containerdSock + `",
  "namespace": "` + testNamespace + `",
  "sync_interval": "5s",
  "max_health_failures": 10,
  "secrets_dir": "` + secretsDir + `",
  "master_key_path": "` + settingsDir + `/master.key",
  "lkg_path": "` + lkgDir + `/lkg-stack.yaml",
  "socket_path": "` + socketDir + `"
}`
	settingsPath := filepath.Join(settingsDir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(settingsJSON), 0644))

	// ── 3. Write stack.yaml ───────────────────────────────────────
	stackYAML := `
version: "1"
kind: Stack
metadata:
  name: e2e-test
spec:
  services:
    - name: nginx-test
      image: docker.io/library/nginx:alpine
      restart_policy: always
`
	stackPath := filepath.Join(tmpDir, "stack.yaml")
	require.NoError(t, os.WriteFile(stackPath, []byte(stackYAML), 0644))

	// ── 4. Build qm-daemon ────────────────────────────────────────
	daemonBin := filepath.Join(tmpDir, "qm-daemon")
	t.Log("Building qm-daemon...")
	out, err := exec.Command("go", "build", "-o", daemonBin, "../../cmd/qm-daemon").CombinedOutput()
	require.NoError(t, err, "build failed:\n%s", string(out))

	// ── 5. Start qm-daemon ───────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, daemonBin,
		"--config", settingsPath,
		"--stack", stackPath,
		"--sync-interval", "5s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	t.Log("Starting qm-daemon...")
	require.NoError(t, cmd.Start())

	// ── 6. Wait for container ─────────────────────────────────────
	t.Log("Waiting for nginx-test container...")
	cc, err := cri.NewContainerdClient(containerdSock, testNamespace)
	require.NoError(t, err)

	var containerID string
	require.Eventually(t, func() bool {
		containers, err := cc.ListContainers(context.Background())
		if err != nil {
			return false
		}
		for _, c := range containers {
			if c.Name == "nginx-test" {
				containerID = c.ID
				t.Logf("Container found: id=%s running=%v", c.ID, c.Running)
				return c.Running
			}
		}
		return false
	}, 120*time.Second, 3*time.Second, "nginx-test container did not become ready")
	t.Logf("nginx-test is running (id=%s)", containerID)

	// ── 7. Verify LKG saved ──────────────────────────────────────
	lkgPath := filepath.Join(lkgDir, "lkg-stack.yaml")
	require.Eventually(t, func() bool {
		_, err := os.Stat(lkgPath)
		return err == nil
	}, 30*time.Second, 1*time.Second, "LKG manifest was not saved")

	// ── 8. Graceful shutdown ─────────────────────────────────────
	t.Log("Sending SIGTERM...")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		require.NoError(t, err, "daemon exited with error")
		t.Log("Daemon shut down gracefully")
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("Daemon did not shut down within 15s")
	}
}

func cleanupTestNamespace(t *testing.T) {
	cc, err := cri.NewContainerdClient(containerdSock, testNamespace)
	if err != nil {
		return
	}
	containers, _ := cc.ListContainers(context.Background())
	for _, c := range containers {
		t.Logf("Cleaning up leftover container: %s (%s)", c.Name, c.ID)
		cc.StopContainer(context.Background(), c.ID)
		cc.DeleteContainer(context.Background(), c.ID)
	}
}
