package cri

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCloseLogFile(t *testing.T) {
	c := &ContainerdClient{logFiles: make(map[string]io.Closer)}

	f, err := os.OpenFile(filepath.Join(t.TempDir(), "c.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	c.setLogFile("abc", f)

	c.closeLogFile("abc")
	if _, err := f.WriteString("x"); err == nil {
		t.Error("expected the log file handle to be closed")
	}
	if _, ok := c.logFiles["abc"]; ok {
		t.Error("log file entry should be removed after close")
	}

	// Closing an unknown container must be a no-op, not a panic.
	c.closeLogFile("missing")
}

// TestLifecycleHelpersConcurrent exercises the mount/log maps from many
// goroutines; meaningful under `go test -race`.
func TestLifecycleHelpersConcurrent(t *testing.T) {
	c := &ContainerdClient{
		mountCleanups: make(map[string]func()),
		logFiles:      make(map[string]io.Closer),
	}
	dir := t.TempDir()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("ctr-%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.setMountCleanup(id, func() {})
			if fn, ok := c.takeMountCleanup(id); ok {
				fn()
			}
		}()
		go func() {
			defer wg.Done()
			f, err := os.OpenFile(filepath.Join(dir, id+".log"), os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return
			}
			c.setLogFile(id, f)
			c.closeLogFile(id)
		}()
	}
	wg.Wait()
}

func TestTakeMountCleanup(t *testing.T) {
	c := &ContainerdClient{mountCleanups: make(map[string]func())}

	called := false
	c.setMountCleanup("abc", func() { called = true })

	fn, ok := c.takeMountCleanup("abc")
	if !ok || fn == nil {
		t.Fatal("expected a registered cleanup function")
	}
	fn()
	if !called {
		t.Error("cleanup function was not called")
	}
	if _, ok := c.takeMountCleanup("abc"); ok {
		t.Error("cleanup should only be handed out once")
	}
}

func TestRotatingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	rl, err := newRotatingLog(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	for i := 0; i < 5; i++ {
		if _, err := rl.Write([]byte("0123456789")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Active file plus two backups.
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected %s to exist: %v", path+suffix, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Error("expected only two rotated backups")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 10 {
		t.Errorf("active log exceeds maxBytes: %d", fi.Size())
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("expected log mode 0600, got %o", fi.Mode().Perm())
	}

	// Rotation must not lose the active file's contents.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("active log should contain the most recent writes")
	}
}

func TestRotatingLogSurvivesRotationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	rl, err := newRotatingLog(path, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	// Occupy the rotation destination with a directory so the rename fails.
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}

	// Neither write may return an error: a failed rotation must not kill the
	// io.MultiWriter chain used for container logging.
	for _, s := range []string{"hello", "world"} {
		if _, err := rl.Write([]byte(s)); err != nil {
			t.Fatalf("write %q after failed rotation returned error: %v", s, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "world") {
		t.Errorf("expected writes to persist after a failed rotation, got %q", data)
	}
}
