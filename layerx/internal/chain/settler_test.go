package chain

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func rootHexWith(b byte) string {
	r := make([]byte, 32)
	r[31] = b
	return hex.EncodeToString(r)
}

func testOperator(t *testing.T) *Operator {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	op, err := LoadOperator(hex.EncodeToString(crypto.FromECDSA(key)))
	if err != nil {
		t.Fatalf("LoadOperator: %v", err)
	}
	return op
}

func TestNewPaxeerSettlerGuards(t *testing.T) {
	op := testOperator(t)
	good := "0x1D5f3ac9dE43Dd0665C3F527913dD825f67b3Daa"

	if _, err := NewPaxeerSettler(nil, op, good, good, nil); err == nil {
		t.Error("nil client must error")
	}
	// A client with a nil eth handle is also rejected.
	if _, err := NewPaxeerSettler(&Client{}, op, good, good, nil); err == nil {
		t.Error("client without eth handle must error")
	}
}

func TestNewPaxeerSettlerAddressValidation(t *testing.T) {
	op := testOperator(t)
	good := "0x1D5f3ac9dE43Dd0665C3F527913dD825f67b3Daa"

	// Dial a lazy HTTP endpoint with chain-id check disabled (expectedChainID=0)
	// so we get a non-nil client without any network round-trip; NewPaxeerSettler
	// itself performs no I/O, only address validation.
	c, err := NewClient(context.Background(), "http://127.0.0.1:1", 0)
	if err != nil {
		t.Fatalf("NewClient (lazy): %v", err)
	}
	defer c.Close()

	if _, err := NewPaxeerSettler(c, op, "not-an-address", good, nil); err == nil {
		t.Error("invalid vault address must error")
	}
	if _, err := NewPaxeerSettler(c, op, good, "also-bad", nil); err == nil {
		t.Error("invalid anchor address must error")
	}
	if _, err := NewPaxeerSettler(c, nil, good, good, nil); err == nil {
		t.Error("nil operator must error")
	}
	ps, err := NewPaxeerSettler(c, op, good, good, nil)
	if err != nil {
		t.Fatalf("valid construction: %v", err)
	}
	if ps.OperatorAddress() != op.Address() {
		t.Fatal("OperatorAddress mismatch")
	}
}

func TestBuildBatchRootAndID(t *testing.T) {
	ps := &PaxeerSettler{}
	we := time.Unix(1_700_000_000, 0)

	batch, err := ps.buildBatch(rootHexWith(0xab), we, nil)
	if err != nil {
		t.Fatalf("buildBatch: %v", err)
	}
	if len(batch.Payouts) != 0 {
		t.Fatalf("nil deltas must yield no payouts, got %d", len(batch.Payouts))
	}
	if batch.WindowEnd != 1_700_000_000 {
		t.Fatalf("windowEnd = %d, want 1700000000", batch.WindowEnd)
	}
	// batchId is the deterministic derivation of the root.
	if batch.BatchId != batchIDForRoot(batch.Root) {
		t.Fatal("batchId must derive from root")
	}
	// Idempotent: same root -> same batchId.
	again, err := ps.buildBatch(rootHexWith(0xab), we, nil)
	if err != nil {
		t.Fatalf("buildBatch again: %v", err)
	}
	if again.BatchId != batch.BatchId {
		t.Fatal("same root must yield the same batchId (idempotent)")
	}
	// Different root -> different batchId.
	other, err := ps.buildBatch(rootHexWith(0xcd), we, nil)
	if err != nil {
		t.Fatalf("buildBatch other: %v", err)
	}
	if other.BatchId == batch.BatchId {
		t.Fatal("different roots must yield different batchIds")
	}
}

func TestBuildBatchRejectsBadRoots(t *testing.T) {
	ps := &PaxeerSettler{}
	if _, err := ps.buildBatch("not-32-bytes", time.Time{}, nil); err == nil {
		t.Error("short root must error")
	}
	// All-zero root is refused (the anchor contract reverts ZeroRoot).
	if _, err := ps.buildBatch(rootHexWith(0x00), time.Time{}, nil); err == nil {
		t.Error("zero root must be refused")
	}
}

func TestBuildPayouts(t *testing.T) {
	// nil / empty -> no payouts (the normal internal-only settlement window).
	if p, err := buildPayouts(nil); err != nil || p != nil {
		t.Fatalf("nil deltas: got (%v, %v), want (nil, nil)", p, err)
	}

	low := "0x000000000000000000000000000000000000000a"
	high := "0x00000000000000000000000000000000000000ff"
	deltas := []NetDelta{
		{DID: "did:b", EVMAddress: high, AmountMicro: 2_000_000},
		{DID: "did:zero", EVMAddress: low, AmountMicro: 0},      // skipped
		{DID: "did:neg", EVMAddress: low, AmountMicro: -5},      // skipped (net debit)
		{DID: "did:a", EVMAddress: low, AmountMicro: 1_000_000}, // paid
	}
	payouts, err := buildPayouts(deltas)
	if err != nil {
		t.Fatalf("buildPayouts: %v", err)
	}
	if len(payouts) != 2 {
		t.Fatalf("expected 2 payouts (positive only), got %d", len(payouts))
	}
	// Deterministically sorted by recipient ascending.
	if payouts[0].Recipient != common.HexToAddress(low) || payouts[1].Recipient != common.HexToAddress(high) {
		t.Fatalf("payouts not sorted by recipient: %+v", payouts)
	}
	if payouts[0].Amount.Int64() != 1_000_000 || payouts[1].Amount.Int64() != 2_000_000 {
		t.Fatalf("payout amounts wrong: %+v", payouts)
	}

	// A positive delta with no valid EVM address is a hard error.
	if _, err := buildPayouts([]NetDelta{{DID: "did:x", EVMAddress: "", AmountMicro: 10}}); err == nil {
		t.Error("positive delta without EVM address must error")
	}
}

// PaxeerSettler must satisfy the Settler interface.
var _ Settler = (*PaxeerSettler)(nil)
