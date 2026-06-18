package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateKey(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	if len(key) != KeySize {
		t.Errorf("expected %d-byte key, got %d", KeySize, len(key))
	}

	// Two keys should be different
	key2, _ := GenerateKey()
	if string(key) == string(key2) {
		t.Error("two generated keys should be different")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	key, _ := GenerateKey()
	plaintext := []byte("s3cr3t-p4ssw0rd!")

	encrypted, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if encrypted == string(plaintext) {
		t.Error("encrypted data should differ from plaintext")
	}

	decrypted, err := Decrypt(encrypted, key)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if string(decrypted) != string(plaintext) {
		t.Errorf("decrypted data mismatch: got %q, want %q", string(decrypted), string(plaintext))
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	key1, _ := GenerateKey()
	key2, _ := GenerateKey()
	plaintext := []byte("secret")

	encrypted, _ := Encrypt(plaintext, key1)
	_, err := Decrypt(encrypted, key2)

	if err == nil {
		t.Fatal("expected decryption to fail with wrong key")
	}
}

func TestEncrypt_InvalidKeySize(t *testing.T) {
	_, err := Encrypt([]byte("data"), []byte("short"))
	if err == nil {
		t.Fatal("expected error for invalid key size")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")

	// First call: creates key
	key1, err := LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreateKey failed: %v", err)
	}
	if len(key1) != KeySize {
		t.Errorf("expected %d-byte key, got %d", KeySize, len(key1))
	}

	// Verify file permissions
	info, _ := os.Stat(keyPath)
	if info.Mode().Perm() != 0400 {
		t.Errorf("expected 0400 permissions, got %o", info.Mode().Perm())
	}

	// Second call: loads same key
	key2, err := LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (second call) failed: %v", err)
	}

	if string(key1) != string(key2) {
		t.Error("loaded key should match created key")
	}
}

func TestEncryptDecryptFile(t *testing.T) {
	dir := t.TempDir()
	key, _ := GenerateKey()
	filePath := filepath.Join(dir, "secret.enc")
	plaintext := []byte("my-database-password")

	if err := EncryptFile(filePath, plaintext, key); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}

	decrypted, err := DecryptFile(filePath, key)
	if err != nil {
		t.Fatalf("DecryptFile failed: %v", err)
	}

	if string(decrypted) != string(plaintext) {
		t.Errorf("decrypted data mismatch: got %q, want %q", string(decrypted), string(plaintext))
	}
}

func TestDecrypt_CorruptedData(t *testing.T) {
	key, _ := GenerateKey()

	_, err := Decrypt("not-valid-base64!!!", key)
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}

	_, err = Decrypt("dG9vLXNob3J0", key)
	if err == nil {
		t.Fatal("expected error for too-short ciphertext")
	}
}
