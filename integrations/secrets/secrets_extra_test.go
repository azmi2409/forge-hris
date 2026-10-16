package secrets

// Supplementary coverage owned by Worker A (Xero): KeyConfigured,
// alternate base64 key encodings, and malformed-envelope inputs. The core
// round-trip/wrong-key/tamper/missing-key contract lives in
// secrets_test.go (Worker B's file).

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestKeyConfigured(t *testing.T) {
	t.Setenv(envKeyName, "")
	if KeyConfigured() {
		t.Fatal("want false with empty key")
	}
	t.Setenv(envKeyName, "0123456789abcdef0123456789abcdef")
	if !KeyConfigured() {
		t.Fatal("want true with raw 32-byte key")
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKeyName, base64.RawURLEncoding.EncodeToString(b[:]))
	if !KeyConfigured() {
		t.Fatal("want true with raw-URL base64 key")
	}
	t.Setenv(envKeyName, strings.Repeat("a", 31))
	if KeyConfigured() {
		t.Fatal("want false with 31-byte key")
	}
}

func TestURLEncodingKeyRoundTrip(t *testing.T) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKeyName, base64.URLEncoding.EncodeToString(b[:]))
	enc, err := Encrypt([]byte("xero refresh token"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	dec, err := Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(dec) != "xero refresh token" {
		t.Fatalf("mismatch: %q", dec)
	}
}

func TestDecryptShortEnvelope(t *testing.T) {
	t.Setenv(envKeyName, "0123456789abcdef0123456789abcdef")
	short := base64.StdEncoding.EncodeToString([]byte("tiny"))
	if _, err := Decrypt(short); err == nil {
		t.Fatal("want error on envelope shorter than the nonce")
	}
	if _, err := Decrypt(""); err == nil {
		t.Fatal("want error on empty envelope")
	}
}
