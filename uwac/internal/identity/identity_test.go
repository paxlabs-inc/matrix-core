package identity

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"
)

// makeDID builds a valid did:matrix:<label>:<keyfp> for a fresh keypair.
func makeDID(t *testing.T, label string) (did, pubHex string, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	pubHex = hex.EncodeToString(pub)
	did = "did:matrix:" + label + ":" + pubHex[:16]
	return did, pubHex, priv
}

func TestParseDID(t *testing.T) {
	did, pubHex, _ := makeDID(t, "executor")
	d, err := ParseDID(did)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.KeyFP != pubHex[:16] {
		t.Fatalf("keyfp mismatch: %s vs %s", d.KeyFP, pubHex[:16])
	}
	if _, err := ParseDID("did:web:example.com"); err == nil {
		t.Fatal("expected malformed did error")
	}
}

func TestOwnerFromDID(t *testing.T) {
	uuid := "d17e78e5-0000-4000-8000-000000000abc"
	did, _, _ := makeDID(t, uuid)
	d, _ := ParseDID(did)
	owner, ok := OwnerFromDID(d)
	if !ok || owner != uuid {
		t.Fatalf("expected owner %s, got %q ok=%v", uuid, owner, ok)
	}
	nd, _, _ := makeDID(t, "executor")
	d2, _ := ParseDID(nd)
	if _, ok := OwnerFromDID(d2); ok {
		t.Fatal("non-uuid label should not resolve an owner")
	}
}

func TestVerifyRoundTrip(t *testing.T) {
	did, pubHex, priv := makeDID(t, "executor")
	ch := NewChallenges(time.Minute)
	nonce, msg := ch.Create(did)
	sig := ed25519.Sign(priv, []byte(msg))
	if err := Verify(did, pubHex, nonce, hex.EncodeToString(sig)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyWrongKeyForDID(t *testing.T) {
	did, _, _ := makeDID(t, "executor")
	// A different keypair signs; its pub does not match the DID fingerprint.
	_, otherPubHex, otherPriv := makeDID(t, "executor")
	msg := ChallengeMessage(did, "n1")
	sig := ed25519.Sign(otherPriv, []byte(msg))
	if err := Verify(did, otherPubHex, "n1", hex.EncodeToString(sig)); err == nil {
		t.Fatal("expected fingerprint-mismatch failure")
	}
}

func TestChallengeSingleUse(t *testing.T) {
	did := "did:matrix:executor:0123456789abcdef"
	ch := NewChallenges(time.Minute)
	nonce, _ := ch.Create(did)
	if !ch.Consume(nonce, did) {
		t.Fatal("first consume should succeed")
	}
	if ch.Consume(nonce, did) {
		t.Fatal("second consume must fail (single-use)")
	}
}

func TestChallengeExpiry(t *testing.T) {
	did := "did:matrix:executor:0123456789abcdef"
	ch := NewChallenges(time.Nanosecond)
	nonce, _ := ch.Create(did)
	time.Sleep(time.Millisecond)
	if ch.Consume(nonce, did) {
		t.Fatal("expired nonce must not consume")
	}
}

func TestChallengeDIDMismatch(t *testing.T) {
	ch := NewChallenges(time.Minute)
	nonce, _ := ch.Create("did:matrix:executor:0123456789abcdef")
	if ch.Consume(nonce, "did:matrix:executor:fedcba9876543210") {
		t.Fatal("nonce must be bound to the issuing DID")
	}
}
