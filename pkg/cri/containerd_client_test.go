package cri

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCloseLogFile(t *testing.T) {
	c := &ContainerdClient{logFiles: make(map[string]*os.File)}

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
		logFiles:      make(map[string]*os.File),
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
