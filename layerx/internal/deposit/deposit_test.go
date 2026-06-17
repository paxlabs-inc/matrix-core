package deposit

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/store"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeSource struct {
	head    uint64
	events  []chain.DepositEvent
	reserve *big.Int
	calls   int
}

func (f *fakeSource) HeadBlock(context.Context) (uint64, error) { return f.head, nil }

func (f *fakeSource) FilterDeposits(_ context.Context, from, to uint64) ([]chain.DepositEvent, error) {
	f.calls++
	var out []chain.DepositEvent
	for _, e := range f.events {
		if e.Block >= from && e.Block <= to {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeSource) ReserveBalance(context.Context) (*big.Int, error) {
	if f.reserve == nil {
		return big.NewInt(0), nil
	}
	return f.reserve, nil
}

type fakeLedger struct {
	cursor    int64
	cursorSet bool
	claims    map[string]string // claim -> did
	credited  map[string]int64  // depositKey -> amount
	byDID     map[string]int64  // did -> total credited (for circulating)
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		claims:   map[string]string{},
		credited: map[string]int64{},
		byDID:    map[string]int64{},
	}
}

func (f *fakeLedger) GetCursor(context.Context, string) (int64, bool, error) {
	return f.cursor, f.cursorSet, nil
}

func (f *fakeLedger) SetCursor(_ context.Context, _ string, block int64) error {
	f.cursor = block
	f.cursorSet = true
	return nil
}

func (f *fakeLedger) ResolveDIDClaim(_ context.Context, claim string) (string, error) {
	if did, ok := f.claims[claim]; ok {
		return did, nil
	}
	return "", store.ErrNotFound
}

func (f *fakeLedger) CreditDeposit(_ context.Context, did, _ string, depositTx string, amountMicro int64) error {
	if _, ok := f.credited[depositTx]; ok {
		return nil // idempotent on the deposit key
	}
	f.credited[depositTx] = amountMicro
	f.byDID[did] += amountMicro
	return nil
}

func (f *fakeLedger) CirculatingUSDX(context.Context) (int64, error) {
	var total int64
	for _, v := range f.byDID {
		total += v
	}
	return total, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func depositEvent(did string, block uint64, tx string, logIndex uint, amount int64) chain.DepositEvent {
	return chain.DepositEvent{
		ClaimHex:   chain.DIDClaimHex(did),
		Depositor:  common.HexToAddress("0x000000000000000000000000000000000000beef"),
		TokenIn:    common.HexToAddress("0x0000000000000000000000000000000000000000"),
		AmountIn:   big.NewInt(amount),
		USDXMinted: big.NewInt(amount),
		TxHash:     tx,
		LogIndex:   logIndex,
		Block:      block,
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestSyncCreditsResolvedSkipsUnresolved(t *testing.T) {
	led := newFakeLedger()
	led.claims[chain.DIDClaimHex("did:a")] = "did:a"
	src := &fakeSource{
		head: 100,
		events: []chain.DepositEvent{
			depositEvent("did:a", 20, "0xtx1", 0, 1_000_000),       // resolved -> credited
			depositEvent("did:unknown", 21, "0xtx2", 0, 5_000_000), // unresolved -> skipped
		},
	}
	w := New(src, led, nil, time.Second, 10, 0)

	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if led.byDID["did:a"] != 1_000_000 {
		t.Fatalf("did:a credited = %d, want 1_000_000", led.byDID["did:a"])
	}
	if len(led.credited) != 1 {
		t.Fatalf("only the resolved deposit must be credited, got %d", len(led.credited))
	}
	if led.cursor != 100 {
		t.Fatalf("cursor = %d, want 100 (advanced to safe head)", led.cursor)
	}
}

func TestSyncIdempotentAndResumes(t *testing.T) {
	led := newFakeLedger()
	led.claims[chain.DIDClaimHex("did:a")] = "did:a"
	src := &fakeSource{
		head:   100,
		events: []chain.DepositEvent{depositEvent("did:a", 20, "0xtx1", 0, 1_000_000)},
	}
	w := New(src, led, nil, time.Second, 0, 0)

	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync 1: %v", err)
	}
	// A second sync over the same head must not re-scan or re-credit (cursor+1 > head).
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if led.byDID["did:a"] != 1_000_000 {
		t.Fatalf("balance must not double-credit, got %d", led.byDID["did:a"])
	}

	// New head + new event -> resumes from the persisted cursor.
	src.head = 150
	src.events = append(src.events, depositEvent("did:a", 120, "0xtx2", 0, 2_000_000))
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync 3: %v", err)
	}
	if led.byDID["did:a"] != 3_000_000 {
		t.Fatalf("after resume balance = %d, want 3_000_000", led.byDID["did:a"])
	}
	if led.cursor != 150 {
		t.Fatalf("cursor = %d, want 150", led.cursor)
	}
}

func TestSyncMultipleEventsSameTx(t *testing.T) {
	led := newFakeLedger()
	led.claims[chain.DIDClaimHex("did:a")] = "did:a"
	src := &fakeSource{
		head: 100,
		events: []chain.DepositEvent{
			depositEvent("did:a", 20, "0xtx1", 0, 1_000_000),
			depositEvent("did:a", 20, "0xtx1", 1, 2_000_000), // same tx, different log index
		},
	}
	w := New(src, led, nil, time.Second, 0, 0)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if led.byDID["did:a"] != 3_000_000 {
		t.Fatalf("both events in one tx must each credit; got %d want 3_000_000", led.byDID["did:a"])
	}
	if len(led.credited) != 2 {
		t.Fatalf("expected 2 distinct deposit keys, got %d", len(led.credited))
	}
}

func TestSyncWaitsForConfirmations(t *testing.T) {
	led := newFakeLedger()
	led.claims[chain.DIDClaimHex("did:a")] = "did:a"
	src := &fakeSource{
		head:   100,
		events: []chain.DepositEvent{depositEvent("did:a", 98, "0xtx1", 0, 1_000_000)},
	}
	// 5 confirmations -> safe head 95; the block-98 deposit is not yet final.
	w := New(src, led, nil, time.Second, 0, 5)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(led.credited) != 0 {
		t.Fatal("a deposit shallower than the confirmation depth must not be credited yet")
	}
	if led.cursor != 95 {
		t.Fatalf("cursor = %d, want 95 (safe head)", led.cursor)
	}
}
