package sig

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func TestNewFromSeedDeterministic(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	seedHex := hex.EncodeToString(seed)

	s1, ephemeral, err := New(seedHex)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ephemeral {
		t.Fatal("a seeded signer must not be ephemeral")
	}
	s2, _, err := New("0x" + seedHex) // 0x prefix tolerated
	if err != nil {
		t.Fatalf("New with 0x: %v", err)
	}
	if s1.PublicHex() != s2.PublicHex() {
		t.Fatal("same seed must yield the same public key")
	}
}

func TestSignVerifies(t *testing.T) {
	s, _, err := New("")
	if err != nil {
		t.Fatalf("New ephemeral: %v", err)
	}
	msg := []byte("layerx.receipt.sig.v1:1:deadbeef")
	sigHex := s.Sign(msg)
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	pub, err := hex.DecodeString(s.PublicHex())
	if err != nil {
		t.Fatalf("decode pub: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		t.Fatal("signature must verify under the signer public key")
	}
	if ed25519.Verify(ed25519.PublicKey(pub), []byte("other"), sig) {
		t.Fatal("signature must NOT verify for a different message")
	}
}

func TestNewRejectsBadSeed(t *testing.T) {
	for _, bad := range []string{"zz", "abcd", hex.EncodeToString(make([]byte, 16))} {
		if _, _, err := New(bad); err == nil {
			t.Errorf("New(%q) should reject a malformed seed", bad)
		}
	}
}

func TestEphemeralWhenEmpty(t *testing.T) {
	_, ephemeral, err := New("   ")
	if err != nil {
		t.Fatalf("New blank: %v", err)
	}
	if !ephemeral {
		t.Fatal("empty seed must produce an ephemeral key")
	}
}
