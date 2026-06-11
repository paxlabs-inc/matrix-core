package cryptox

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func key(s string) []byte { k := sha256.Sum256([]byte(s)); return k[:] }

func TestSealOpenRoundTrip(t *testing.T) {
	k := key("vault-secret")
	pt := []byte("ya29.a0AfH6S-provider-refresh-token")
	sealed, err := Seal(k, pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := Open(k, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
	}
}

func TestSealUniqueNonce(t *testing.T) {
	k := key("vault-secret")
	a, _ := Seal(k, []byte("same"))
	b, _ := Seal(k, []byte("same"))
	if a == b {
		t.Fatal("expected distinct ciphertexts for the same plaintext (nonce reuse)")
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	sealed, _ := Seal(key("right"), []byte("secret"))
	if _, err := Open(key("wrong"), sealed); err == nil {
		t.Fatal("expected auth failure opening with the wrong key")
	}
}

func TestOpenTamperFails(t *testing.T) {
	k := key("right")
	sealed, _ := Seal(k, []byte("secret"))
	// Flip a character to simulate tampering.
	b := []byte(sealed)
	b[len(b)-2] ^= 0x01
	if _, err := Open(k, string(b)); err == nil {
		t.Fatal("expected auth failure opening tampered ciphertext")
	}
}

func TestKeySizeEnforced(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("x")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
