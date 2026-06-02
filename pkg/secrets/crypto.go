// Package crypto provides encryption/decryption for secrets at rest.
// Uses NaCl secretbox (XSalsa20-Poly1305) with a 32-byte master key.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/secretbox"
)

const (
	// KeySize is the size of a NaCl secretbox key in bytes.
	KeySize = 32

	// NonceSize is the size of a NaCl secretbox nonce in bytes.
	NonceSize = 24
)

// GenerateKey creates a new random 32-byte master key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate key: %w", err)
	}
	return key, nil
}

// LoadOrCreateKey reads the master key from a path, creating it if it doesn't exist.
// The key file is created with 0400 permissions.
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(string(data))
		if err != nil {
			return nil, fmt.Errorf("invalid key encoding in %s: %w", path, err)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("key in %s is %d bytes, expected %d", path, len(key), KeySize)
		}
		return key, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read key file %s: %w", path, err)
	}

	// Create new key
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded), 0400); err != nil {
		return nil, fmt.Errorf("failed to write key file %s: %w", path, err)
	}

	return key, nil
}

// Encrypt encrypts plaintext using the master key. Returns base64-encoded
// ciphertext (nonce + encrypted data).
func Encrypt(plaintext, key []byte) (string, error) {
	if len(key) != KeySize {
		return "", fmt.Errorf("invalid key size: %d", len(key))
	}

	var nonce [NonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	var keyArr [KeySize]byte
	copy(keyArr[:], key)

	encrypted := secretbox.Seal(nonce[:], plaintext, &nonce, &keyArr)

	return base64.StdEncoding.EncodeToString(encrypted), nil
}

// Decrypt decrypts a base64-encoded ciphertext using the master key.
func Decrypt(encoded string, key []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("invalid key size: %d", len(key))
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	if len(ciphertext) < NonceSize+1 {
		return nil, fmt.Errorf("ciphertext too short (%d bytes)", len(ciphertext))
	}

	var nonce [NonceSize]byte
	copy(nonce[:], ciphertext[:NonceSize])

	var keyArr [KeySize]byte
	copy(keyArr[:], key)

	decrypted, ok := secretbox.Open(nil, ciphertext[NonceSize:], &nonce, &keyArr)
	if !ok {
		return nil, fmt.Errorf("decryption failed: wrong key or corrupted data")
	}

	return decrypted, nil
}

// EncryptFile encrypts data and writes it to a file.
func EncryptFile(path string, plaintext, key []byte) error {
	encoded, err := Encrypt(plaintext, key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(encoded), 0400)
}

// DecryptFile reads and decrypts a file.
func DecryptFile(path string, key []byte) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decrypt(string(data), key)
}
