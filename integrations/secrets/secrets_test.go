package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func testKey(t *testing.T) {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKeyName, base64.StdEncoding.EncodeToString(b[:]))
}

func TestRoundTrip(t *testing.T) {
	testKey(t)
	enc, err := Encrypt([]byte("xoxb-signing-secret-value"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == "xoxb-signing-secret-value" {
		t.Fatal("envelope must not contain the plaintext")
	}
	dec, err := Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, []byte("xoxb-signing-secret-value")) {
		t.Fatalf("round trip mismatch: %q", dec)
	}
	// Fresh nonce per encryption: two envelopes of the same plaintext differ.
	enc2, err := Encrypt([]byte("xoxb-signing-secret-value"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == enc2 {
		t.Fatal("nonces must be random; envelopes must differ")
	}
}

func TestRawKey(t *testing.T) {
	t.Helper()
	k := "0123456789abcdef0123456789abcdef" // 32 raw bytes
	t.Setenv(envKeyName, k)
	enc, err := Encrypt([]byte("hello"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	dec, err := Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(dec) != "hello" {
		t.Fatalf("mismatch: %q", dec)
	}
}

func TestMissingKey(t *testing.T) {
	t.Setenv(envKeyName, "")
	if _, err := Encrypt([]byte("x")); err == nil {
		t.Fatal("want error with missing key")
	}
	if _, err := Decrypt("eA=="); err == nil {
		t.Fatal("want error with missing key")
	}
}

func TestBadKeyLength(t *testing.T) {
	t.Setenv(envKeyName, "too-short")
	if _, err := Encrypt([]byte("x")); err == nil {
		t.Fatal("want error with short key")
	}
}

func TestTamperedEnvelope(t *testing.T) {
	testKey(t)
	enc, err := Encrypt([]byte("sensitive"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xff
	bad := base64.StdEncoding.EncodeToString(raw)
	if _, err := Decrypt(bad); err == nil {
		t.Fatal("want error on tampered envelope")
	}
	if _, err := Decrypt("not-base64!!!"); err == nil {
		t.Fatal("want error on bad base64")
	}
}

func TestWrongKeyFails(t *testing.T) {
	testKey(t)
	enc, err := Encrypt([]byte("sensitive"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Rotate the key: the old envelope must not open.
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKeyName, base64.StdEncoding.EncodeToString(b[:]))
	if _, err := Decrypt(enc); err == nil {
		t.Fatal("want error decrypting with the wrong key")
	}
}
