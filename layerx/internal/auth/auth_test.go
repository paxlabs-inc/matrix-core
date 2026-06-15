package auth

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"
)

func TestParseDID(t *testing.T) {
	d, err := ParseDID("did:matrix:user-1:0123456789abcdef")
	if err != nil {
		t.Fatalf("ParseDID: %v", err)
	}
	if d.Label != "user-1" || d.KeyFP != "0123456789abcdef" {
		t.Fatalf("unexpected parse: %+v", d)
	}
	if _, err := ParseDID("not-a-did"); err == nil {
		t.Fatal("expected error on malformed did")
	}
}

func TestVerifySignatureRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fp := hex.EncodeToString(pub)[:16]
	did := "did:matrix:user-1:" + fp
	nonce := "test-nonce"
	sig := ed25519.Sign(priv, []byte(ChallengeMessage(did, nonce)))
	if err := VerifySignature(did, hex.EncodeToString(pub), nonce, hex.EncodeToString(sig)); err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
	// Wrong nonce must fail.
	if err := VerifySignature(did, hex.EncodeToString(pub), "other", hex.EncodeToString(sig)); err == nil {
		t.Fatal("expected signature failure on wrong nonce")
	}
}

func TestChallengesSingleUse(t *testing.T) {
	c := NewChallenges(time.Minute)
	nonce, _ := c.Create("did:matrix:u:0123456789abcdef")
	if !c.Consume(nonce, "did:matrix:u:0123456789abcdef") {
		t.Fatal("first consume should succeed")
	}
	if c.Consume(nonce, "did:matrix:u:0123456789abcdef") {
		t.Fatal("second consume must fail (single-use)")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tk := NewTokens("secret", time.Hour)
	tok, exp := tk.Mint("did:matrix:u:0123456789abcdef")
	if exp <= 0 {
		t.Fatal("expiresIn should be positive")
	}
	claims, err := tk.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.DID != "did:matrix:u:0123456789abcdef" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if _, err := tk.Verify("garbage"); err == nil {
		t.Fatal("expected verify failure on garbage token")
	}
}
