package chain

import (
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestLoadOperatorRoundTrip(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	want := crypto.PubkeyToAddress(key.PublicKey)
	keyHex := hex.EncodeToString(crypto.FromECDSA(key))

	// Bare hex and 0x-prefixed hex must both load and derive the same address.
	for _, in := range []string{keyHex, "0x" + keyHex, "  " + keyHex + "  "} {
		op, err := LoadOperator(in)
		if err != nil {
			t.Fatalf("LoadOperator(%q): %v", in, err)
		}
		if op.Address() != want {
			t.Fatalf("Address = %s, want %s", op.Address().Hex(), want.Hex())
		}
		if op.Key() == nil {
			t.Fatal("Key() returned nil")
		}
	}
}

func TestLoadOperatorRejectsBad(t *testing.T) {
	for _, in := range []string{"", "   ", "0x", "not-hex", "zz"} {
		if _, err := LoadOperator(in); err == nil {
			t.Fatalf("LoadOperator(%q) should fail closed", in)
		}
	}
}
