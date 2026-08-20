package auth

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"
)

func TestParseDID(t *testing.T) {
	d, err := ParseDID("did:matrix:11111111-2222-3333-4444-555555555555:0123456789abcdef")
	if err != nil {
		t.Fatalf("ParseDID: %v", err)
	}
	if d.KeyFP != "0123456789abcdef" {
		t.Fatalf("keyfp %q", d.KeyFP)
	}
	if !IsUUID(d.Label) {
		t.Fatalf("expected uuid label")
	}
	if OwnerFromDID(d) != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("owner %q", OwnerFromDID(d))
	}
}

func TestParseDIDRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "did:matrix:label", "did:matrix:label:short", "nope"} {
		if _, err := ParseDID(s); err == nil {
			t.Fatalf("expected error for %q", s)
		}
	}
}

func TestChallengeVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	pubHex := hex.EncodeToString(pub)
	did := "did:matrix:dev:" + pubHex[:16]

	ch := NewChallenges(time.Minute)
	nonce, msg := ch.Create(did)
	sig := ed25519.Sign(priv, []byte(msg))

	if !ch.Consume(nonce, did) {
		t.Fatal("consume should succeed once")
	}
	if ch.Consume(nonce, did) {
		t.Fatal("consume must be single-use")
	}
	if err := VerifySignature(did, pubHex, nonce, hex.EncodeToString(sig)); err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
}

func TestVerifySignatureRejectsWrongKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	pubHex := hex.EncodeToString(pub)
	did := "did:matrix:dev:" + pubHex[:16]
	msg := ChallengeMessage(did, "n1")
	sig := ed25519.Sign(priv, []byte(msg))
	// Present a different public key than the DID fingerprint.
	if err := VerifySignature(did, hex.EncodeToString(other), "n1", hex.EncodeToString(sig)); err == nil {
		t.Fatal("expected fingerprint mismatch error")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tk := NewTokens("super-secret", time.Hour)
	tok, exp := tk.Mint("did:matrix:dev:0123456789abcdef", "owner-1")
	if exp != 3600 {
		t.Fatalf("expires_in %d", exp)
	}
	claims, err := tk.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Owner != "owner-1" || claims.DID != "did:matrix:dev:0123456789abcdef" {
		t.Fatalf("claims %+v", claims)
	}
}

func TestTokenRejectsTamper(t *testing.T) {
	tk := NewTokens("secret", time.Hour)
	tok, _ := tk.Mint("did:matrix:dev:0123456789abcdef", "owner-1")
	if _, err := tk.Verify(tok + "x"); err == nil {
		t.Fatal("expected signature failure on tampered token")
	}
}

func TestTokenRejectsExpired(t *testing.T) {
	tk := NewTokens("secret", time.Hour)
	tk.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	tok, _ := tk.Mint("did:matrix:dev:0123456789abcdef", "owner-1")
	tk.now = time.Now
	if _, err := tk.Verify(tok); err == nil {
		t.Fatal("expected expiry failure")
	}
}
