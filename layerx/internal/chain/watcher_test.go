package chain

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestDIDClaimHexDeterministic(t *testing.T) {
	did := "did:matrix:cursor-agent:d205e18de628e38d"
	got := DIDClaimHex(did)

	// 32-byte keccak -> 64 lowercase hex chars, no 0x.
	if len(got) != 64 {
		t.Fatalf("claim length = %d, want 64", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("claim is not valid hex: %v", err)
	}
	// Deterministic + matches keccak256(did) exactly (the on-chain bytes32).
	want := hex.EncodeToString(crypto.Keccak256([]byte(did)))
	if got != want {
		t.Fatalf("claim = %s, want %s", got, want)
	}
	// Distinct DIDs -> distinct claims.
	if DIDClaimHex("did:matrix:other:0000000000000000") == got {
		t.Fatal("different DIDs must produce different claims")
	}
}

func TestNewWatcherGuards(t *testing.T) {
	good := "0x1D5f3ac9dE43Dd0665C3F527913dD825f67b3Daa"

	if _, err := NewWatcher(nil, good); err == nil {
		t.Error("nil client must error")
	}
	if _, err := NewWatcher(&Client{}, good); err == nil {
		t.Error("client without eth handle must error")
	}

	c, err := NewClient(context.Background(), "http://127.0.0.1:1", 0)
	if err != nil {
		t.Fatalf("NewClient (lazy): %v", err)
	}
	defer c.Close()
	if _, err := NewWatcher(c, "not-an-address"); err == nil {
		t.Error("invalid vault address must error")
	}
	if _, err := NewWatcher(c, good); err != nil {
		t.Fatalf("valid construction: %v", err)
	}
}
