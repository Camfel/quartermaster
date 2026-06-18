package git

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcher_Start(t *testing.T) {
	// Git 2.35.2+ enforces directory ownership checks.  CI runners clone into
	// a different user's directory, so we need to mark all dirs as safe.
	exec.Command("git", "config", "--global", "--add", "safe.directory", "*").Run()

	// Create a temporary directory for our "remote" and our "local"
	tmpDir, err := os.MkdirTemp("", "watcher-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	remoteDir := filepath.Join(tmpDir, "remote")
	require.NoError(t, os.Mkdir(remoteDir, 0755))
	require.NoError(t, exec.Command("git", "-C", remoteDir, "init", "--bare").Run())

	localDir := filepath.Join(tmpDir, "local")
	require.NoError(t, os.Mkdir(localDir, 0755))

	// Helper to create a commit in the remote
	createCommit := func(msg string) error {
		workDir := filepath.Join(tmpDir, "workdir-"+msg)
		require.NoError(t, os.Mkdir(workDir, 0755))
		require.NoError(t, exec.Command("git", "-C", workDir, "init", "-b", "master").Run())
		require.NoError(t, os.WriteFile(filepath.Join(workDir, "file.txt"), []byte(msg), 0644))
		require.NoError(t, exec.Command("git", "-C", workDir, "add", ".").Run())
		require.NoError(t, exec.Command("git", "-C", workDir, "commit", "-m", msg).Run())
		require.NoError(t, exec.Command("git", "-C", workDir, "remote", "add", "origin", remoteDir).Run())
		cmd := exec.Command("git", "-C", workDir, "push", "origin", "master", "--force")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("push failed: %v, output: %s", err, string(out))
		}
		return os.RemoveAll(workDir)
	}

	// 1. Initialize remote with an initial commit
	require.NoError(t, createCommit("initial"))

	// Setup watcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changedCalled := false
	var detectedHash string

	watcher := NewWatcher(
		"file://"+remoteDir,
		"master",
		"", // token
		"", // sshKeyPath
		"", // sshKnownHostsPath
		localDir,
		500*time.Millisecond,
		0,
		func(ctx context.Context, newHash string) {
			changedCalled = true
			detectedHash = newHash
		},
	)

	// Start watcher in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Start(ctx)
	}()

	// Wait for initial clone/setup
	time.Sleep(2 * time.Second)

	// 2. Create second commit
	require.NoError(t, createCommit("second"))

	// Check if the REMOTE repo actually has the new commit
	cmd := exec.Command("git", "-C", remoteDir, "rev-parse", "master")
	out, err := cmd.Output()
	require.NoError(t, err, "remote repo should have the new commit")
	log.Printf("Remote repo hash after push: %s", string(out))

	// Check if the local repo actually has the new commit
	cmd = exec.Command("git", "-C", localDir, "rev-parse", "master")
	out, err = cmd.Output()
	require.NoError(t, err, "local repo should have the new commit")
	log.Printf("Local repo hash after push: %s", string(out))

	// Wait for watcher to detect it
	require.Eventually(t, func() bool {
		return changedCalled
	}, 10*time.Second, 100*time.Millisecond)

	assert.True(t, changedCalled)
	assert.NotEmpty(t, detectedHash)

	// Cleanup
	cancel()
	err = <-errChan
	if err != nil && err != context.Canceled {
		t.Errorf("watcher exited with error: %v", err)
	}
}
