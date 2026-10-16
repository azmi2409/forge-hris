// Package secrets is the shared AES-256-GCM envelope for operator-held
// credentials (Xero OAuth tokens for Worker A, Slack/Teams tokens for
// Worker B, ...). The database never sees plaintext secrets: callers
// encrypt before storing and decrypt after reading.
//
// The single key comes from the FORGE_HRIS_SECRET_KEY environment variable,
// which accepts either a raw 32-byte string or base64-encoded 32 bytes.
// Rotation is out of scope for the MVP: a key change renders previously
// stored blobs undecryptable, and reconnecting the integration re-keys
// them.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// envKeyName is the environment variable holding the encryption key.
const envKeyName = "FORGE_HRIS_SECRET_KEY"

// ErrKeyNotConfigured means the key env var is unset or malformed.
var ErrKeyNotConfigured = errors.New("secrets: " + envKeyName + " is not configured: set it to a raw 32-byte string or base64-encoded 32 bytes")

// keyBytes resolves the encryption key from the environment.
func keyBytes() ([]byte, error) {
	raw := os.Getenv(envKeyName)
	if raw == "" {
		return nil, ErrKeyNotConfigured
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(raw); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, ErrKeyNotConfigured
}

// KeyConfigured reports whether a usable key is present. Operators can call
// this at boot to fail fast instead of discovering it on first use.
func KeyConfigured() bool {
	_, err := keyBytes()
	return err == nil
}

// gcm builds the AES-256-GCM AEAD from the configured key.
func gcm() (cipher.AEAD, error) {
	key, err := keyBytes()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}
	return aead, nil
}

// Encrypt seals plaintext with AES-256-GCM under a fresh random 12-byte
// nonce and returns base64.StdEncoding(nonce || ciphertext).
func Encrypt(plaintext []byte) (string, error) {
	aead, err := gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: random nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt. It fails when the key is missing, the key is
// wrong, or the ciphertext was tampered with (GCM authentication).
func Decrypt(s string) ([]byte, error) {
	aead, err := gcm()
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("secrets: decode: %w", err)
	}
	n := aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("secrets: ciphertext too short")
	}
	nonce, sealed := raw[:n], raw[n:]
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plaintext, nil
}
