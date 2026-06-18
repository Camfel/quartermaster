// Package secrets handles secure injection of sensitive data into containers.
// Secrets are read from the filesystem (default: /etc/quartermaster/secrets/)
// and exposed to containers as read-only files mounted at /run/secrets/.
package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretData holds the resolved content for a single secret reference.
type SecretData struct {
	Name    string // The name used in the container (e.g., "DB_PASSWORD")
	Content []byte // The raw secret content
	Path    string // The original filesystem path
}

// Manager handles loading secrets from the host filesystem.
// Supports both plaintext and encrypted (NaCl secretbox) modes.
type Manager struct {
	secretsDir string
	masterKey  []byte // if set, secrets are decrypted with this key
}

// NewManager creates a new Secret Manager that reads from the given directory.
func NewManager(secretsDir string) *Manager {
	return &Manager{secretsDir: secretsDir}
}

// WithEncryption sets the master key for decrypting secrets at rest.
// When set, secret files are expected to be encrypted (base64-encoded NaCl secretbox).
// When nil (default), secrets are read as plaintext.
func (m *Manager) WithEncryption(masterKey []byte) *Manager {
	m.masterKey = masterKey
	return m
}

// Resolve reads a single secret reference from disk and returns its content.
// secretRef is the filename within the secrets directory.
func (m *Manager) Resolve(name, secretRef string) (*SecretData, error) {
	// Prevent path traversal
	cleanRef := filepath.Clean(secretRef)
	if strings.Contains(cleanRef, "..") {
		return nil, fmt.Errorf("secret ref %q contains path traversal", secretRef)
	}

	path := filepath.Join(m.secretsDir, cleanRef)

	var data []byte

	// If encryption is enabled, try to decrypt; fall back to plaintext
	if len(m.masterKey) == KeySize {
		decrypted, decErr := DecryptFile(path, m.masterKey)
		if decErr == nil {
			data = decrypted
		} else {
			// Fall back to plaintext for backwards compatibility
			raw, rawErr := os.ReadFile(path)
			if rawErr != nil {
				if os.IsNotExist(rawErr) {
					return nil, fmt.Errorf("secret %q: file %q not found in %s", name, secretRef, m.secretsDir)
				}
				return nil, fmt.Errorf("secret %q: failed to read %s: %w", name, path, rawErr)
			}
			data = raw
		}
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("secret %q: file %q not found in %s", name, secretRef, m.secretsDir)
			}
			return nil, fmt.Errorf("secret %q: failed to read %s: %w", name, path, err)
		}
	}

	// Trim trailing newline (common in secret files)
	content := data
	if len(content) > 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}

	return &SecretData{
		Name:    name,
		Content: content,
		Path:    path,
	}, nil
}

// ResolveAll resolves a list of secret references into a map of name -> content.
func (m *Manager) ResolveAll(refs []SecretRef) (map[string][]byte, error) {
	result := make(map[string][]byte, len(refs))
	for _, ref := range refs {
		secret, err := m.Resolve(ref.Name, ref.SecretRef)
		if err != nil {
			return nil, err
		}
		result[secret.Name] = secret.Content
	}
	return result, nil
}

// PrepareMountDir creates a temporary directory containing the resolved secrets
// as individual files. The caller is responsible for cleaning it up.
// Returns the path to the directory and a cleanup function.
func (m *Manager) PrepareMountDir(refs []SecretRef) (string, func(), error) {
	secrets, err := m.ResolveAll(refs)
	if err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp("", "quartermaster-secrets-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create secrets tmp dir: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(dir)
	}

	for name, content := range secrets {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0400); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("failed to write secret %q: %w", name, err)
		}
	}

	return dir, cleanup, nil
}

// SecretRef is a local type alias matching the design in pkg/types.
// We avoid importing pkg/types to keep this package dependency-free.
type SecretRef struct {
	Name      string
	SecretRef string
}

// CreateEncrypted encrypts a secret value and stores it in the secrets directory.
// The secret is encrypted with the manager's master key.
func (m *Manager) CreateEncrypted(name string, value []byte) error {
	if len(m.masterKey) != KeySize {
		return fmt.Errorf("cannot create encrypted secret: master key not configured")
	}

	path := filepath.Join(m.secretsDir, name)
	return EncryptFile(path, value, m.masterKey)
}

// ListNames returns the names of all secrets in the secrets directory.
func (m *Manager) ListNames() ([]string, error) {
	entries, err := os.ReadDir(m.secretsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}
