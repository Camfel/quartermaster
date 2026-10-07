package cri

import (
	"os"
	"path/filepath"
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
