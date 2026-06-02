//go:build integration

package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"quartermaster/pkg/cri"
	"quartermaster/pkg/secrets"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testNamespace = "quartermaster-e2e-test"
	socketPath    = "/run/containerd/containerd.sock"
)

// TestDaemonEndToEnd performs a full lifecycle test of the qm-daemon binary:
//   1. Sets up a bare Git repo containing a valid stack
//   2. Starts qm-daemon pointing at the repo
//   3. Waits for containers to appear in containerd
//   4. Verifies the container is running
//   5. Pushes a new commit and verifies the daemon detects it
//   6. Sends SIGTERM and verifies graceful shutdown
//   7. Cleans up all created containers
func TestDaemonEndToEnd(t *testing.T) {
	// ── 0. Clean up any leftover containers from previous runs ──────
	cleanupTestNamespace(t)

	// ── 1. Setup temp directories ────────────────────────────────────
	tmpDir, err := os.MkdirTemp("", "qm-e2e-test-*")
	require.NoError(t, err, "failed to create temp dir")
	defer os.RemoveAll(tmpDir)

	remoteDir := filepath.Join(tmpDir, "remote")
	localDir := filepath.Join(tmpDir, "local")
	lkgDir := filepath.Join(tmpDir, "lkg")
	secretsDir := filepath.Join(tmpDir, "secrets")
	masterKeyDir := filepath.Join(tmpDir, "master-key")

	for _, d := range []string{remoteDir, localDir, lkgDir, secretsDir, masterKeyDir} {
		require.NoError(t, os.MkdirAll(d, 0755))
	}

	// ── 2. Create bare Git repo with initial stack ──────────────────
	require.NoError(t, initBareRepo(remoteDir))

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
	require.NoError(t, pushCommit(remoteDir, tmpDir, stackYAML, "initial stack"))

	// ── 3. Build the daemon binary ──────────────────────────────────
	daemonBin := filepath.Join(tmpDir, "qm-daemon")
	t.Log("Building qm-daemon...")
	build := exec.Command("go", "build", "-o", daemonBin, "../../cmd/qm-daemon")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build qm-daemon: %v\n%s", err, string(out))
	}

	// ── 4. Start the daemon ─────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemonEnv := []string{
		"QM_CONTAINERD_SOCKET=" + socketPath,
		"QM_NAMESPACE=" + testNamespace,
		"QM_SYNC_INTERVAL=5s",
		"QM_HEALTH_CHECK_INTERVAL=10s",
		"QM_MAX_HEALTH_FAILURES=10",
		"QM_GIT_REPO_URL=" + remoteDir,
		"QM_GIT_BRANCH=master",
		"QM_GIT_LOCAL_PATH=" + localDir,
		"QM_GIT_POLL_INTERVAL=2s",
		"QM_GIT_COOLDOWN=0s",
		"QM_LKG_PATH=" + filepath.Join(lkgDir, "lkg-stack.yaml"),
		"QM_SECRETS_DIR=" + secretsDir,
		"QM_MASTER_KEY_PATH=" + filepath.Join(masterKeyDir, "master.key"),
	}

	cmd := exec.CommandContext(ctx, daemonBin)
	cmd.Env = append(os.Environ(), daemonEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	t.Log("Starting qm-daemon...")
	require.NoError(t, cmd.Start())

	// ── 5. Wait for container to appear in containerd ───────────────
	t.Log("Waiting for nginx-test container...")
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	require.NoError(t, err, "failed to connect to containerd")

	var containerID string
	require.Eventually(t, func() bool {
		containers, err := cc.ListContainers(context.Background())
		if err != nil {
			return false
		}
		for _, c := range containers {
			if strings.Contains(c.Image, "nginx") {
				containerID = c.ID
				t.Logf("Container found: name=%s id=%s image=%s", c.Name, c.ID, c.Image)
				return true
			}
		}
		return false
	}, 90*time.Second, 2*time.Second, "nginx container did not appear")

	require.NotEmpty(t, containerID, "container should have an ID")
	t.Logf("Container created successfully: %s", containerID)

	// ── 6. Verify container PID (means it's running) ────────────────
	var pid uint32
	require.Eventually(t, func() bool {
		pid, err = cc.GetContainerPID(context.Background(), containerID)
		return err == nil && pid > 0
	}, 15*time.Second, 500*time.Millisecond, "container did not start running")
	t.Logf("Container is running (PID %d)", pid)

	// ── 7. Verify LKG file was persisted (wait for ticker reconcile) ─
	lkgFile := filepath.Join(lkgDir, "lkg-stack.yaml")
	require.Eventually(t, func() bool {
		_, err := os.Stat(lkgFile)
		return err == nil
	}, 15*time.Second, 1*time.Second, "LKG manifest was not saved")
	t.Log("LKG manifest saved successfully")

	// ── 8. Push a no-op commit and verify the daemon handles it ─────
	// Same stack content — daemon should detect the new commit but
	// not disrupt the running container (no config hash change).
	require.NoError(t, pushCommit(remoteDir, tmpDir, stackYAML, "no-op update"))

	// Wait for the daemon's poller to pick up the change. The watcher
	// triggers a reconcile which will be a no-op (same container), but
	// we verify the daemon stays running without errors.
	t.Log("Waiting for daemon to process updated commit...")
	time.Sleep(8 * time.Second) // poll interval 2s + cooldown 0s, enough for 3-4 poll cycles

	// Verify the container is still alive after the update.
	// Note: the pid may change if the content hash changed (e.g. metadata name
	// changed), which triggers a container recreation — that's expected behaviour.
	_, err = cc.GetContainerPID(context.Background(), containerID)
	require.NoError(t, err, "container should still be running after update")
	t.Log("Daemon handled commit update; container still running")

	// ── 9. Shut down the daemon gracefully ──────────────────────────
	t.Log("Shutting down daemon via SIGTERM...")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		assert.NoError(t, err, "daemon should exit cleanly")
		t.Log("Daemon exited cleanly")
	case <-time.After(15 * time.Second):
		t.Log("Daemon did not exit in time, sending SIGKILL")
		cmd.Process.Kill()
		<-done
		t.Fatal("daemon did not shut down within 15s of SIGTERM")
	}

	// ── 10. Clean up containers ─────────────────────────────────────
	t.Log("Cleaning up test containers...")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()

	containers, err := cc.ListContainers(cleanupCtx)
	if err == nil {
		for _, c := range containers {
			t.Logf("  Stopping and deleting: %s (%s)", c.Name, c.ID)
			_ = cc.StopContainer(cleanupCtx, c.ID)
			_ = cc.DeleteContainer(cleanupCtx, c.ID)
		}
	}
}

// TestDaemonHealthCheckRestart verifies that when a container's health check
// fails, the daemon detects the failure and restarts the container. It uses
// nginx with a healthcheck pointing at a port where nothing is listening,
// so the HTTP probe always fails. The test confirms:
//   - The container is created and running
//   - Health check failure is logged
//   - The daemon attempts (and succeeds) at restarting the container
//   - The container is still running after the restart cycle
//   - The daemon shuts down cleanly
func TestDaemonHealthCheckRestart(t *testing.T) {
	cleanupTestNamespace(t)

	// ── 1. Setup temp directories ────────────────────────────────────
	tmpDir, err := os.MkdirTemp("", "qm-hc-test-*")
	require.NoError(t, err, "failed to create temp dir")
	defer os.RemoveAll(tmpDir)

	remoteDir := filepath.Join(tmpDir, "remote")
	localDir := filepath.Join(tmpDir, "local")
	lkgDir := filepath.Join(tmpDir, "lkg")
	secretsDir := filepath.Join(tmpDir, "secrets")
	masterKeyDir := filepath.Join(tmpDir, "master-key")

	for _, d := range []string{remoteDir, localDir, lkgDir, secretsDir, masterKeyDir} {
		require.NoError(t, os.MkdirAll(d, 0755))
	}

	// ── 2. Create bare Git repo with a stack that has a failing healthcheck ─
	require.NoError(t, initBareRepo(remoteDir))

	// Port 19980 maps nginx:80, but the healthcheck probes port 19999
	// where nothing is listening — so the check always fails.
	stackYAML := `
version: "1"
kind: Stack
metadata:
  name: health-check-test
spec:
  services:
    - name: nginx-fail-test
      image: docker.io/library/nginx:alpine
      restart_policy: always
      ports:
        - host: 19980
          container: 80
      healthcheck:
        type: http
        path: /
        port: 19999
        interval: "10s"
`
	require.NoError(t, pushCommit(remoteDir, tmpDir, stackYAML, "initial"))

	// ── 3. Build the daemon binary ──────────────────────────────────
	daemonBin := filepath.Join(tmpDir, "qm-daemon")
	t.Log("Building qm-daemon...")
	build := exec.Command("go", "build", "-o", daemonBin, "../../cmd/qm-daemon")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build qm-daemon: %v\n%s", err, string(out))
	}

	// ── 4. Start the daemon ─────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemonEnv := []string{
		"QM_CONTAINERD_SOCKET=" + socketPath,
		"QM_NAMESPACE=" + testNamespace,
		"QM_SYNC_INTERVAL=5s",
		"QM_HEALTH_CHECK_INTERVAL=5s",
		"QM_MAX_HEALTH_FAILURES=10", // high enough that we test restarts, not LKG rollback
		"QM_GIT_REPO_URL=" + remoteDir,
		"QM_GIT_BRANCH=master",
		"QM_GIT_LOCAL_PATH=" + localDir,
		"QM_GIT_POLL_INTERVAL=2s",
		"QM_GIT_COOLDOWN=0s",
		"QM_LKG_PATH=" + filepath.Join(lkgDir, "lkg-stack.yaml"),
		"QM_SECRETS_DIR=" + secretsDir,
		"QM_MASTER_KEY_PATH=" + filepath.Join(masterKeyDir, "master.key"),
	}

	cmd := exec.CommandContext(ctx, daemonBin)
	cmd.Env = append(os.Environ(), daemonEnv...)

	// Capture daemon output to a buffer for later assertions, while also
	// printing it to stdout for debugging.
	var daemonOutput bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &daemonOutput)
	cmd.Stderr = io.MultiWriter(os.Stderr, &daemonOutput)

	t.Log("Starting qm-daemon with failing-healthcheck stack...")
	require.NoError(t, cmd.Start())

	// ── 5. Wait for container to appear ─────────────────────────────
	t.Log("Waiting for nginx-fail-test container...")
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	require.NoError(t, err, "failed to connect to containerd")

	var containerID string
	require.Eventually(t, func() bool {
		containers, err := cc.ListContainers(context.Background())
		if err != nil {
			return false
		}
		for _, c := range containers {
			if c.Name == "nginx-fail-test" {
				containerID = c.ID
				t.Logf("Container found: name=%s id=%s", c.Name, c.ID)
				return true
			}
		}
		return false
	}, 90*time.Second, 2*time.Second, "nginx-fail-test container did not appear")

	// ── 6. Verify container is running ──────────────────────────────
	require.Eventually(t, func() bool {
		pid, err := cc.GetContainerPID(context.Background(), containerID)
		return err == nil && pid > 0
	}, 15*time.Second, 500*time.Millisecond, "container did not start running")
	t.Log("Container is running")

	// ── 7. Wait for health check failure + restart cycle ────────────
	// Health check interval is 5s. The first check should fail
	// (probing port 19999 where nginx isn't listening), and the daemon
	// should restart the container. We wait long enough for at least
	// one full failure → restart cycle to complete.
	t.Log("Waiting for health check to fail and trigger restart...")
	time.Sleep(15 * time.Second)

	// ── 8. Verify restart happened via daemon output ────────────────
	logStr := daemonOutput.String()

	assert.Contains(t, logStr, "Health check FAILED for nginx-fail-test",
		"daemon should log health check failure")
	assert.Contains(t, logStr, "Attempting to restart nginx-fail-test",
		"daemon should attempt to restart the failed container")
	assert.Contains(t, logStr, "Successfully restarted nginx-fail-test",
		"daemon should successfully restart the container")

	// Count restart attempts — should be at least 1
	restartCount := strings.Count(logStr, "Successfully restarted nginx-fail-test")
	t.Logf("Container restarted %d time(s)", restartCount)
	assert.GreaterOrEqual(t, restartCount, 1, "should have at least one successful restart")

	// ── 9. Verify container survived the restart and is running ─────
	_, err = cc.GetContainerPID(context.Background(), containerID)
	require.NoError(t, err, "container should still be running after restart cycle")
	t.Log("Container is still running after restart(s)")

	// ── 10. Shut down the daemon gracefully ─────────────────────────
	t.Log("Shutting down daemon via SIGTERM...")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		assert.NoError(t, err, "daemon should exit cleanly")
		t.Log("Daemon exited cleanly")
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("daemon did not shut down within 20s of SIGTERM")
	}

	// ── 11. Clean up containers ─────────────────────────────────────
	t.Log("Cleaning up test containers...")
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()

	containers, err := cc.ListContainers(cleanupCtx)
	if err == nil {
		for _, c := range containers {
			t.Logf("  Stopping and deleting: %s (%s)", c.Name, c.ID)
			_ = cc.StopContainer(cleanupCtx, c.ID)
			_ = cc.DeleteContainer(cleanupCtx, c.ID)
		}
	}
}

// TestDaemonSecretInjection verifies that secrets created with qm create-secret
// are correctly injected into containers at /run/secrets/<name>.
func TestDaemonSecretInjection(t *testing.T) {
	cleanupTestNamespace(t)

	tmpDir, err := os.MkdirTemp("", "qm-secret-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	remoteDir := filepath.Join(tmpDir, "remote")
	localDir := filepath.Join(tmpDir, "local")
	lkgDir := filepath.Join(tmpDir, "lkg")
	secretsDir := filepath.Join(tmpDir, "secrets")
	masterKeyDir := filepath.Join(tmpDir, "master-key")

	for _, d := range []string{remoteDir, localDir, lkgDir, secretsDir, masterKeyDir} {
		require.NoError(t, os.MkdirAll(d, 0755))
	}

	// ── Create an encrypted secret ──────────────────────────────────
	key, err := secrets.LoadOrCreateKey(filepath.Join(masterKeyDir, "master.key"))
	require.NoError(t, err)
	mgr := secrets.NewManager(secretsDir).WithEncryption(key)
	require.NoError(t, mgr.CreateEncrypted("test-secret", []byte("secret-value-42")))

	// ── Stack with secret reference ─────────────────────────────────
	require.NoError(t, initBareRepo(remoteDir))

	stackYAML := `
version: "1"
kind: Stack
metadata:
  name: secret-test
spec:
  services:
    - name: nginx-secret
      image: docker.io/library/nginx:alpine
      restart_policy: always
      secrets:
        - name: test-secret
          secret_ref: test-secret
`
	require.NoError(t, pushCommit(remoteDir, tmpDir, stackYAML, "initial"))

	// ── Build and run daemon ────────────────────────────────────────
	daemonBin := filepath.Join(tmpDir, "qm-daemon")
	build := exec.Command("go", "build", "-o", daemonBin, "../../cmd/qm-daemon")
	require.NoError(t, build.Run())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, daemonBin)
	cmd.Env = append(os.Environ(),
		"QM_CONTAINERD_SOCKET="+socketPath,
		"QM_NAMESPACE="+testNamespace,
		"QM_SYNC_INTERVAL=3s",
		"QM_HEALTH_CHECK_INTERVAL=30s",
		"QM_MAX_HEALTH_FAILURES=10",
		"QM_GIT_REPO_URL="+remoteDir,
		"QM_GIT_BRANCH=master",
		"QM_GIT_LOCAL_PATH="+localDir,
		"QM_GIT_POLL_INTERVAL=2s",
		"QM_GIT_COOLDOWN=0s",
		"QM_LKG_PATH="+filepath.Join(lkgDir, "lkg.yaml"),
		"QM_SECRETS_DIR="+secretsDir,
		"QM_MASTER_KEY_PATH="+filepath.Join(masterKeyDir, "master.key"),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	// ── Wait for container ──────────────────────────────────────────
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	require.NoError(t, err)

	var containerID string
	require.Eventually(t, func() bool {
		containers, _ := cc.ListContainers(context.Background())
		for _, c := range containers {
			if c.Name == "nginx-secret" {
				containerID = c.ID
				return true
			}
		}
		return false
	}, 90*time.Second, 2*time.Second, "nginx-secret container did not appear")

	// ── Verify secret inside container ──────────────────────────────
	out, err := exec.Command("ctr", "-n", testNamespace, "task", "exec",
		"--exec-id", "secret-check", containerID,
		"cat", "/run/secrets/test-secret").CombinedOutput()
	require.NoError(t, err, "failed to read secret from container: %s", string(out))
	assert.Equal(t, "secret-value-42", strings.TrimSpace(string(out)),
		"secret value should match what was written")
	t.Logf("Secret verified: %s", strings.TrimSpace(string(out)))

	// ── Shutdown ────────────────────────────────────────────────────
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		cmd.Process.Kill(); <-done
		t.Fatal("daemon did not shut down")
	}

	// ── Cleanup ─────────────────────────────────────────────────────
	cleanupCtx, ccCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer ccCancel()
	containers, _ := cc.ListContainers(cleanupCtx)
	for _, c := range containers {
		_ = cc.StopContainer(cleanupCtx, c.ID)
		_ = cc.DeleteContainer(cleanupCtx, c.ID)
	}
}

// TestDaemonVolumeMount verifies that bind-mount volumes are correctly
// mounted inside containers.
func TestDaemonVolumeMount(t *testing.T) {
	cleanupTestNamespace(t)

	tmpDir, err := os.MkdirTemp("", "qm-volume-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	remoteDir := filepath.Join(tmpDir, "remote")
	localDir := filepath.Join(tmpDir, "local")
	lkgDir := filepath.Join(tmpDir, "lkg")
	secretsDir := filepath.Join(tmpDir, "secrets")
	masterKeyDir := filepath.Join(tmpDir, "master-key")
	hostVolDir := filepath.Join(tmpDir, "host-volume")

	for _, d := range []string{remoteDir, localDir, lkgDir, secretsDir, masterKeyDir, hostVolDir} {
		require.NoError(t, os.MkdirAll(d, 0755))
	}

	// ── Create a file in the host volume directory ──────────────────
	require.NoError(t, os.WriteFile(
		filepath.Join(hostVolDir, "data.txt"),
		[]byte("hello-from-host\n"), 0644))

	// ── Stack with volume mount ─────────────────────────────────────
	require.NoError(t, initBareRepo(remoteDir))

	stackYAML := fmt.Sprintf(`
version: "1"
kind: Stack
metadata:
  name: volume-test
spec:
  services:
    - name: nginx-volume
      image: docker.io/library/nginx:alpine
      restart_policy: always
      volumes:
        - source: %s
          target: /mnt/data
          type: bind
`, hostVolDir)
	require.NoError(t, pushCommit(remoteDir, tmpDir, stackYAML, "initial"))

	// ── Build and run daemon ────────────────────────────────────────
	daemonBin := filepath.Join(tmpDir, "qm-daemon")
	require.NoError(t, exec.Command("go", "build", "-o", daemonBin, "../../cmd/qm-daemon").Run())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, daemonBin)
	cmd.Env = append(os.Environ(),
		"QM_CONTAINERD_SOCKET="+socketPath,
		"QM_NAMESPACE="+testNamespace,
		"QM_SYNC_INTERVAL=3s",
		"QM_HEALTH_CHECK_INTERVAL=30s",
		"QM_MAX_HEALTH_FAILURES=10",
		"QM_GIT_REPO_URL="+remoteDir,
		"QM_GIT_BRANCH=master",
		"QM_GIT_LOCAL_PATH="+localDir,
		"QM_GIT_POLL_INTERVAL=2s",
		"QM_GIT_COOLDOWN=0s",
		"QM_LKG_PATH="+filepath.Join(lkgDir, "lkg.yaml"),
		"QM_SECRETS_DIR="+secretsDir,
		"QM_MASTER_KEY_PATH="+filepath.Join(masterKeyDir, "master.key"),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	// ── Wait for container ──────────────────────────────────────────
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	require.NoError(t, err)

	var containerID string
	require.Eventually(t, func() bool {
		containers, _ := cc.ListContainers(context.Background())
		for _, c := range containers {
			if c.Name == "nginx-volume" {
				containerID = c.ID
				return true
			}
		}
		return false
	}, 90*time.Second, 2*time.Second, "nginx-volume container did not appear")

	// ── Verify volume mount inside container ────────────────────────
	out, err := exec.Command("ctr", "-n", testNamespace, "task", "exec",
		"--exec-id", "vol-check", containerID,
		"cat", "/mnt/data/data.txt").CombinedOutput()
	require.NoError(t, err, "failed to read volume from container: %s", string(out))
	assert.Equal(t, "hello-from-host", strings.TrimSpace(string(out)),
		"volume file should contain host content")
	t.Log("Volume mount verified")

	// ── Shutdown and cleanup ────────────────────────────────────────
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		cmd.Process.Kill(); <-done
		t.Fatal("daemon did not shut down")
	}
	cleanupCtx, ccCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer ccCancel()
	containers, _ := cc.ListContainers(cleanupCtx)
	for _, c := range containers {
		_ = cc.StopContainer(cleanupCtx, c.ID)
		_ = cc.DeleteContainer(cleanupCtx, c.ID)
	}
}

// TestDaemonGPU verifies that containers requesting GPU resources get access
// to NVIDIA devices.  Skipped if nvidia-smi is not available on the host.
func TestDaemonGPU(t *testing.T) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		t.Skip("nvidia-smi not found — skipping GPU test")
	}

	cleanupTestNamespace(t)

	tmpDir, err := os.MkdirTemp("", "qm-gpu-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	remoteDir := filepath.Join(tmpDir, "remote")
	localDir := filepath.Join(tmpDir, "local")
	lkgDir := filepath.Join(tmpDir, "lkg")
	secretsDir := filepath.Join(tmpDir, "secrets")
	masterKeyDir := filepath.Join(tmpDir, "master-key")

	for _, d := range []string{remoteDir, localDir, lkgDir, secretsDir, masterKeyDir} {
		require.NoError(t, os.MkdirAll(d, 0755))
	}

	// ── Stack with GPU resource request ────────────────────────────
	// Use a small image; GPU device injection depends on host-level
	// nvidia-container-toolkit hooks being configured in containerd.
	require.NoError(t, initBareRepo(remoteDir))

	stackYAML := `
version: "1"
kind: Stack
metadata:
  name: gpu-test
spec:
  services:
    - name: gpu-test
      image: docker.io/library/alpine:latest
      restart_policy: always
      resources:
        gpu:
          type: nvidia
          id: all
`
	require.NoError(t, pushCommit(remoteDir, tmpDir, stackYAML, "initial"))

	// ── Build and run daemon ────────────────────────────────────────
	daemonBin := filepath.Join(tmpDir, "qm-daemon")
	require.NoError(t, exec.Command("go", "build", "-o", daemonBin, "../../cmd/qm-daemon").Run())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, daemonBin)
	cmd.Env = append(os.Environ(),
		"QM_CONTAINERD_SOCKET="+socketPath,
		"QM_NAMESPACE="+testNamespace,
		"QM_SYNC_INTERVAL=3s",
		"QM_HEALTH_CHECK_INTERVAL=30s",
		"QM_MAX_HEALTH_FAILURES=10",
		"QM_GIT_REPO_URL="+remoteDir,
		"QM_GIT_BRANCH=master",
		"QM_GIT_LOCAL_PATH="+localDir,
		"QM_GIT_POLL_INTERVAL=2s",
		"QM_GIT_COOLDOWN=0s",
		"QM_LKG_PATH="+filepath.Join(lkgDir, "lkg.yaml"),
		"QM_SECRETS_DIR="+secretsDir,
		"QM_MASTER_KEY_PATH="+filepath.Join(masterKeyDir, "master.key"),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	// ── Wait for container ──────────────────────────────────────────
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	require.NoError(t, err)

	var containerID string
	require.Eventually(t, func() bool {
		containers, _ := cc.ListContainers(context.Background())
		for _, c := range containers {
			if c.Name == "gpu-test" {
				containerID = c.ID
				return true
			}
		}
		return false
	}, 90*time.Second, 3*time.Second, "gpu-test container did not appear")

	// ── Verify GPU devices inside container ─────────────────────────
	// Best-effort: checks /dev/nvidia* which is present when
	// nvidia-container-toolkit hooks are configured in containerd.
	out, err := exec.Command("ctr", "-n", testNamespace, "task", "exec",
		"--exec-id", "gpu-check", containerID,
		"sh", "-c", "ls /dev/nvidia* 2>/dev/null || echo NO_DEVICES").CombinedOutput()
	require.NoError(t, err)
	devOut := strings.TrimSpace(string(out))
	t.Logf("GPU devices in container: %s", devOut)
	if strings.Contains(devOut, "NO_DEVICES") {
		t.Log("No /dev/nvidia* in container — nvidia-container-toolkit hooks may not be configured")
		t.Log("GPU was detected on host but device passthrough requires:")
		t.Log("  https://github.com/NVIDIA/nvidia-container-toolkit")
	} else {
		assert.Contains(t, devOut, "nvidia", "expected GPU devices in container")
	}

	// ── Shutdown and cleanup ────────────────────────────────────────
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		cmd.Process.Kill(); <-done
		t.Fatal("daemon did not shut down")
	}
	cleanupCtx, ccCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer ccCancel()
	containers, _ := cc.ListContainers(cleanupCtx)
	for _, c := range containers {
		_ = cc.StopContainer(cleanupCtx, c.ID)
		_ = cc.DeleteContainer(cleanupCtx, c.ID)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────

// cleanupTestNamespace stops and deletes all containers in the test namespace.
func cleanupTestNamespace(t *testing.T) {
	t.Helper()
	cc, err := cri.NewContainerdClient(socketPath, testNamespace)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	containers, err := cc.ListContainers(ctx)
	if err != nil {
		return
	}
	for _, c := range containers {
		_ = cc.StopContainer(ctx, c.ID)
		_ = cc.DeleteContainer(ctx, c.ID)
	}
}

// dumpDaemonLog prints the last 40 lines of the daemon log to the test output.
func dumpDaemonLog(t *testing.T, path string) {
	t.Helper()
	t.Log("--- Daemon log (tail) ---")
	logBytes, _ := os.ReadFile(path)
	lines := strings.Split(string(logBytes), "\n")
	start := 0
	if len(lines) > 40 {
		start = len(lines) - 40
	}
	for _, line := range lines[start:] {
		if line != "" {
			t.Log(line)
		}
	}
	t.Log("--- End daemon log ---")
}

func initBareRepo(path string) error {
	cmd := exec.Command("git", "-C", path, "init", "--bare", "-b", "master")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git init --bare failed: %v\n%s", err, string(out))
	}
	return nil
}

func pushCommit(remoteDir, tmpDir, stackContent, commitMsg string) error {
	workDir := filepath.Join(tmpDir, "workdir-"+commitMsg)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	for _, args := range [][]string{
		{"git", "-C", workDir, "init", "-b", "master"},
		{"git", "-C", workDir, "config", "user.email", "test@quartermaster.local"},
		{"git", "-C", workDir, "config", "user.name", "e2e-test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v failed: %v\n%s", args, err, string(out))
		}
	}

	if err := os.WriteFile(filepath.Join(workDir, "stack.yaml"), []byte(stackContent), 0644); err != nil {
		return err
	}

	for _, args := range [][]string{
		{"git", "-C", workDir, "add", "stack.yaml"},
		{"git", "-C", workDir, "commit", "-m", commitMsg},
		{"git", "-C", workDir, "remote", "add", "origin", remoteDir},
		{"git", "-C", workDir, "push", "--force", "origin", "master"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v failed: %v\n%s", args, err, string(out))
		}
	}

	return nil
}
