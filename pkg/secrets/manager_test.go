package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()

	// Create a secret file
	secretPath := filepath.Join(dir, "db-password")
	if err := os.WriteFile(secretPath, []byte("s3cr3t\n"), 0600); err != nil {
		t.Fatalf("Failed to write test secret: %v", err)
	}

	m := NewManager(dir)
	secret, err := m.Resolve("DB_PASSWORD", "db-password")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if secret.Name != "DB_PASSWORD" {
		t.Errorf("Expected name DB_PASSWORD, got %s", secret.Name)
	}
	if string(secret.Content) != "s3cr3t" {
		t.Errorf("Expected content 's3cr3t', got '%s'", string(secret.Content))
	}
}

func TestResolve_NotFound(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	_, err := m.Resolve("X", "nonexistent")
	if err == nil {
		t.Fatal("Expected error for missing secret file")
	}
}

func TestResolve_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	_, err := m.Resolve("X", "../../etc/passwd")
	if err == nil {
		t.Fatal("Expected error for path traversal")
	}
}

func TestResolveAll(t *testing.T) {
	dir := t.TempDir()

	// Create secret files
	os.WriteFile(filepath.Join(dir, "secret1"), []byte("value1"), 0600)
	os.WriteFile(filepath.Join(dir, "secret2"), []byte("value2"), 0600)

	m := NewManager(dir)
	refs := []SecretRef{
		{Name: "SECRET_ONE", SecretRef: "secret1"},
		{Name: "SECRET_TWO", SecretRef: "secret2"},
	}

	result, err := m.ResolveAll(refs)
	if err != nil {
		t.Fatalf("ResolveAll failed: %v", err)
	}

	if string(result["SECRET_ONE"]) != "value1" {
		t.Errorf("Expected value1, got %s", string(result["SECRET_ONE"]))
	}
	if string(result["SECRET_TWO"]) != "value2" {
		t.Errorf("Expected value2, got %s", string(result["SECRET_TWO"]))
	}
}

func TestPrepareMountDir(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "pass"), []byte("hunter2"), 0600)
	os.WriteFile(filepath.Join(dir, "key"), []byte("abc123"), 0600)

	m := NewManager(dir)
	refs := []SecretRef{
		{Name: "DB_PASSWORD", SecretRef: "pass"},
		{Name: "API_KEY", SecretRef: "key"},
	}

	mountDir, cleanup, err := m.PrepareMountDir(refs)
	if err != nil {
		t.Fatalf("PrepareMountDir failed: %v", err)
	}
	defer cleanup()

	// Check files exist
	data, err := os.ReadFile(filepath.Join(mountDir, "DB_PASSWORD"))
	if err != nil {
		t.Fatalf("Expected DB_PASSWORD file: %v", err)
	}
	if string(data) != "hunter2" {
		t.Errorf("Expected hunter2, got %s", string(data))
	}

	data, err = os.ReadFile(filepath.Join(mountDir, "API_KEY"))
	if err != nil {
		t.Fatalf("Expected API_KEY file: %v", err)
	}
	if string(data) != "abc123" {
		t.Errorf("Expected abc123, got %s", string(data))
	}

	// Verify file permissions are read-only
	info, _ := os.Stat(filepath.Join(mountDir, "DB_PASSWORD"))
	if info.Mode().Perm() != 0400 {
		t.Errorf("Expected 0400 permissions, got %o", info.Mode().Perm())
	}

	// Verify cleanup works
	cleanup()
	if _, err := os.Stat(mountDir); !os.IsNotExist(err) {
		t.Error("Expected mount dir to be cleaned up")
	}
}

func TestTrimTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "with-nl"), []byte("value\n"), 0600)
	os.WriteFile(filepath.Join(dir, "without-nl"), []byte("value"), 0600)

	m := NewManager(dir)

	s1, _ := m.Resolve("S1", "with-nl")
	s2, _ := m.Resolve("S2", "without-nl")

	if string(s1.Content) != "value" {
		t.Errorf("Expected trailing newline to be trimmed, got '%s'", string(s1.Content))
	}
	if string(s2.Content) != "value" {
		t.Errorf("Expected no change, got '%s'", string(s2.Content))
	}
}
