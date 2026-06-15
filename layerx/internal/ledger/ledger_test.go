package ledger

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

func TestTierForBoundary(t *testing.T) {
	signer, _, err := sig.New("")
	if err != nil {
		t.Fatalf("sig.New: %v", err)
	}
	const threshold = 1_000_000 // 1 USDX
	l := New(nil, signer, threshold)

	if got := l.tierFor(threshold - 1); got != types.TierMicropayment {
		t.Errorf("just below threshold = %q, want micropayment", got)
	}
	if got := l.tierFor(threshold); got != types.TierMaterial {
		t.Errorf("at threshold = %q, want material", got)
	}
	if got := l.tierFor(threshold + 1); got != types.TierMaterial {
		t.Errorf("above threshold = %q, want material", got)
	}
	if got := l.tierFor(1); got != types.TierMicropayment {
		t.Errorf("tiny = %q, want micropayment", got)
	}
}

func TestReceiptSigningBytesDeterministicAndDomainSeparated(t *testing.T) {
	a := receiptSigningBytes(7, "deadbeef")
	b := receiptSigningBytes(7, "deadbeef")
	if !bytes.Equal(a, b) {
		t.Fatal("signing preimage must be deterministic")
	}
	if !bytes.HasPrefix(a, []byte(receiptSigDomain)) {
		t.Fatal("signing preimage must be domain-separated")
	}
	if bytes.Equal(a, receiptSigningBytes(8, "deadbeef")) {
		t.Fatal("different seq must change the preimage")
	}
	if bytes.Equal(a, receiptSigningBytes(7, "cafebabe")) {
		t.Fatal("different leaf must change the preimage")
	}
}

func TestReceiptSignatureVerifies(t *testing.T) {
	signer, _, err := sig.New("")
	if err != nil {
		t.Fatalf("sig.New: %v", err)
	}
	l := New(nil, signer, 1_000_000)
	preimage := receiptSigningBytes(42, "00ff")
	sigHex := l.signer.Sign(preimage)

	rawSig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	pub, err := hex.DecodeString(l.signer.PublicHex())
	if err != nil {
		t.Fatalf("decode pub: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), preimage, rawSig) {
		t.Fatal("receipt signature must verify under the sequencer key")
	}
}
