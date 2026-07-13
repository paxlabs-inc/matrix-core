package settle

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeSettler struct {
	tx            string
	err           error
	calls         int
	lastRoot      string
	lastWindowEnd time.Time
	lastDeltas    []chain.NetDelta
	roots         []string
}

func (f *fakeSettler) AnchorBatch(_ context.Context, rootHex string, _ int, windowEnd time.Time, deltas []chain.NetDelta) (string, error) {
	f.calls++
	f.lastRoot = rootHex
	f.lastWindowEnd = windowEnd
	f.lastDeltas = deltas
	f.roots = append(f.roots, rootHex)
	if f.err != nil {
		return "", f.err
	}
	return f.tx, nil
}

type fakeLedger struct {
	unsettled []store.UnsettledTransfer
	sealedID  string
	pending   []store.PendingBatch
	accounts  map[string]types.Account

	anchored map[string]string
	failed   map[string]string

	queuedWithdrawals    []store.QueuedWithdrawal
	submittedWithdrawals []store.QueuedWithdrawal
	sealedRoot           string
	sealedItems          []store.SealedWithdrawal
	settledWithdrawals   map[string]string // id -> payout tx
	withdrawalErrors     map[string]string // payout_root -> err
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		accounts:           map[string]types.Account{},
		anchored:           map[string]string{},
		failed:             map[string]string{},
		settledWithdrawals: map[string]string{},
		withdrawalErrors:   map[string]string{},
	}
}

func (f *fakeLedger) ListUnsettled(context.Context, int) ([]store.UnsettledTransfer, error) {
	return f.unsettled, nil
}

func (f *fakeLedger) SweepExpiredHolds(context.Context) (int, error) {
	return 0, nil
}

func (f *fakeLedger) SealBatch(context.Context, string, []int64, time.Time, time.Time) (string, error) {
	return f.sealedID, nil
}

func (f *fakeLedger) MarkAnchored(_ context.Context, batchID, anchorTx string) error {
	f.anchored[batchID] = anchorTx
	return nil
}

func (f *fakeLedger) MarkBatchFailed(_ context.Context, batchID, errText string) error {
	f.failed[batchID] = errText
	return nil
}

func (f *fakeLedger) GetAccount(_ context.Context, did string) (types.Account, error) {
	if a, ok := f.accounts[did]; ok {
		return a, nil
	}
	return types.Account{}, store.ErrNotFound
}

func (f *fakeLedger) ListUnanchoredBatches(context.Context) ([]store.PendingBatch, error) {
	return f.pending, nil
}

func (f *fakeLedger) ListQueuedWithdrawals(context.Context, int) ([]store.QueuedWithdrawal, error) {
	return f.queuedWithdrawals, nil
}

func (f *fakeLedger) ListSubmittedWithdrawals(context.Context) ([]store.QueuedWithdrawal, error) {
	return f.submittedWithdrawals, nil
}

func (f *fakeLedger) SealWithdrawals(_ context.Context, payoutRoot string, items []store.SealedWithdrawal) error {
	f.sealedRoot = payoutRoot
	f.sealedItems = items
	return nil
}

func (f *fakeLedger) MarkWithdrawalSettled(_ context.Context, id, payoutTx string) error {
	f.settledWithdrawals[id] = payoutTx
	return nil
}

func (f *fakeLedger) RecordWithdrawalError(_ context.Context, payoutRoot, errText string) error {
	f.withdrawalErrors[payoutRoot] = errText
	return nil
}

func leafHex(b byte) string {
	x := make([]byte, 32)
	x[0] = b
	return hex.EncodeToString(x)
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestComputeNetDeltas(t *testing.T) {
	// a -> b 3, b -> c 1: net a=-3, b=+2, c=+1 (sums to zero).
	transfers := []store.UnsettledTransfer{
		{Seq: 1, FromDID: "a", ToDID: "b", AmountMicro: 3},
		{Seq: 2, FromDID: "b", ToDID: "c", AmountMicro: 1},
	}
	net := computeNetDeltas(transfers)
	if net["a"] != -3 || net["b"] != 2 || net["c"] != 1 {
		t.Fatalf("net deltas = %v, want a=-3 b=2 c=1", net)
	}
	var sum int64
	for _, v := range net {
		sum += v
	}
	if sum != 0 {
		t.Fatalf("net deltas must sum to zero, got %d", sum)
	}

	// A round-trip that nets to zero for one DID drops it entirely.
	rt := computeNetDeltas([]store.UnsettledTransfer{
		{Seq: 1, FromDID: "a", ToDID: "b", AmountMicro: 5},
		{Seq: 2, FromDID: "b", ToDID: "a", AmountMicro: 5},
	})
	if len(rt) != 0 {
		t.Fatalf("fully-netted window must drop all DIDs, got %v", rt)
	}
}

func TestSettleNowMarksAnchoredAfterConfirm(t *testing.T) {
	led := newFakeLedger()
	led.sealedID = "batch-1"
	led.unsettled = []store.UnsettledTransfer{
		{Seq: 1, FromDID: "a", ToDID: "b", AmountMicro: 2_000_000, LeafHex: leafHex(0x11)},
	}
	st := &fakeSettler{tx: "0xConfirmedTx"}

	w := New(led, st, nil, time.Hour)
	id, err := w.SettleNow(context.Background())
	if err != nil {
		t.Fatalf("SettleNow: %v", err)
	}
	if id != "batch-1" {
		t.Fatalf("batch id = %q, want batch-1", id)
	}
	if st.calls != 1 {
		t.Fatalf("AnchorBatch calls = %d, want 1", st.calls)
	}
	if got := led.anchored["batch-1"]; got != "0xConfirmedTx" {
		t.Fatalf("anchored tx = %q, want 0xConfirmedTx (mark only after confirm)", got)
	}
	if len(led.failed) != 0 {
		t.Fatalf("no batch should be marked failed, got %v", led.failed)
	}
	if st.lastWindowEnd.IsZero() {
		t.Fatal("windowEnd should be passed to AnchorBatch")
	}
}

func TestSettleNowFailureMarksFailed(t *testing.T) {
	led := newFakeLedger()
	led.sealedID = "batch-2"
	led.unsettled = []store.UnsettledTransfer{
		{Seq: 1, FromDID: "a", ToDID: "b", AmountMicro: 1, LeafHex: leafHex(0x22)},
	}
	st := &fakeSettler{err: errors.New("chain down")}

	w := New(led, st, nil, time.Hour)
	if _, err := w.SettleNow(context.Background()); err == nil {
		t.Fatal("SettleNow must surface the anchor error")
	}
	if _, ok := led.failed["batch-2"]; !ok {
		t.Fatalf("batch-2 must be marked failed, failed=%v", led.failed)
	}
	if _, ok := led.anchored["batch-2"]; ok {
		t.Fatal("a failed batch must NOT be marked anchored")
	}
}

func TestSettleNowNothingToSettle(t *testing.T) {
	led := newFakeLedger()
	st := &fakeSettler{tx: "0xunused"}
	w := New(led, st, nil, time.Hour)
	id, err := w.SettleNow(context.Background())
	if err != nil {
		t.Fatalf("SettleNow empty: %v", err)
	}
	if id != "" {
		t.Fatalf("empty window must return no batch id, got %q", id)
	}
	if st.calls != 0 {
		t.Fatal("no AnchorBatch call when there is nothing to settle")
	}
}

func TestRecoverPendingReanchors(t *testing.T) {
	led := newFakeLedger()
	led.pending = []store.PendingBatch{
		{ID: "old-1", RootHex: leafHex(0x33), WindowEnd: time.Unix(1_700_000_000, 0), TransferCount: 2, Status: "sealed"},
		{ID: "old-2", RootHex: leafHex(0x44), WindowEnd: time.Unix(1_700_000_100, 0), TransferCount: 1, Status: "failed"},
	}
	st := &fakeSettler{tx: "0xRecovered"}

	w := New(led, st, nil, time.Hour)
	if err := w.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	if st.calls != 2 {
		t.Fatalf("AnchorBatch calls = %d, want 2 (one per pending batch)", st.calls)
	}
	if led.anchored["old-1"] != "0xRecovered" || led.anchored["old-2"] != "0xRecovered" {
		t.Fatalf("both pending batches must be re-anchored, anchored=%v", led.anchored)
	}
}

const evmA = "0x000000000000000000000000000000000000000a"
const evmB = "0x00000000000000000000000000000000000000ff"

func TestBuildWithdrawalGroupDeterministic(t *testing.T) {
	// Same set in different input order -> identical root + payouts (idempotent).
	a := []store.QueuedWithdrawal{
		{ID: "w-2", DID: "did:b", EVMAddress: evmB, AmountMicro: 2_000_000},
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_000},
	}
	b := []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_000},
		{ID: "w-2", DID: "did:b", EVMAddress: evmB, AmountMicro: 2_000_000},
	}
	r1, items1, deltas1 := buildWithdrawalGroup(a)
	r2, _, _ := buildWithdrawalGroup(b)
	if r1 == "" || r1 != r2 {
		t.Fatalf("withdrawal root must be order-independent: %q vs %q", r1, r2)
	}
	if len(items1) != 2 || len(deltas1) != 2 {
		t.Fatalf("expected 2 items/deltas, got %d/%d", len(items1), len(deltas1))
	}
	// Sorted by id -> w-1 then w-2.
	if items1[0].ID != "w-1" || items1[1].ID != "w-2" {
		t.Fatalf("items not sorted by id: %+v", items1)
	}
	// A different amount -> a different root (commitment binds the payout).
	c := []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_001},
		{ID: "w-2", DID: "did:b", EVMAddress: evmB, AmountMicro: 2_000_000},
	}
	r3, _, _ := buildWithdrawalGroup(c)
	if r3 == r1 {
		t.Fatal("changing an amount must change the payout root")
	}
}

func TestSettleWithdrawalsHappyPath(t *testing.T) {
	led := newFakeLedger()
	led.queuedWithdrawals = []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_000},
		{ID: "w-2", DID: "did:b", EVMAddress: evmB, AmountMicro: 2_000_000},
	}
	st := &fakeSettler{tx: "0xPayout"}
	w := New(led, st, nil, time.Hour)

	root, err := w.SettleWithdrawals(context.Background())
	if err != nil {
		t.Fatalf("SettleWithdrawals: %v", err)
	}
	if root == "" {
		t.Fatal("expected a non-empty payout root")
	}
	if st.calls != 1 {
		t.Fatalf("AnchorBatch calls = %d, want 1", st.calls)
	}
	if led.sealedRoot != root || len(led.sealedItems) != 2 {
		t.Fatalf("seal mismatch: root=%q items=%d", led.sealedRoot, len(led.sealedItems))
	}
	if led.settledWithdrawals["w-1"] != "0xPayout" || led.settledWithdrawals["w-2"] != "0xPayout" {
		t.Fatalf("both withdrawals must be settled with the payout tx, got %v", led.settledWithdrawals)
	}
	if len(st.lastDeltas) != 2 {
		t.Fatalf("expected 2 on-chain payout deltas, got %d", len(st.lastDeltas))
	}
}

func TestSettleWithdrawalsSkipsUnmapped(t *testing.T) {
	led := newFakeLedger()
	led.queuedWithdrawals = []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: "", AmountMicro: 1_000_000}, // no binding -> skipped
	}
	st := &fakeSettler{tx: "0xPayout"}
	w := New(led, st, nil, time.Hour)

	root, err := w.SettleWithdrawals(context.Background())
	if err != nil {
		t.Fatalf("SettleWithdrawals: %v", err)
	}
	if root != "" {
		t.Fatalf("nothing payable must return no root, got %q", root)
	}
	if st.calls != 0 {
		t.Fatal("must not anchor when nothing is payable")
	}
}

func TestSettleWithdrawalsFailureSurfaced(t *testing.T) {
	led := newFakeLedger()
	led.queuedWithdrawals = []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_000},
	}
	st := &fakeSettler{err: errors.New("chain down")}
	w := New(led, st, nil, time.Hour)

	if _, err := w.SettleWithdrawals(context.Background()); err == nil {
		t.Fatal("a failed payout must surface an error")
	}
	if led.sealedRoot == "" {
		t.Fatal("the set must still be sealed (frozen for crash-safe retry)")
	}
	if _, ok := led.withdrawalErrors[led.sealedRoot]; !ok {
		t.Fatalf("the failure must be recorded on the payout root, errors=%v", led.withdrawalErrors)
	}
	if len(led.settledWithdrawals) != 0 {
		t.Fatal("no withdrawal may be marked settled on failure")
	}
}

func TestSettleWithdrawalsNothingQueued(t *testing.T) {
	led := newFakeLedger()
	st := &fakeSettler{tx: "0xunused"}
	w := New(led, st, nil, time.Hour)
	root, err := w.SettleWithdrawals(context.Background())
	if err != nil {
		t.Fatalf("SettleWithdrawals empty: %v", err)
	}
	if root != "" || st.calls != 0 {
		t.Fatalf("empty queue must be a no-op, root=%q calls=%d", root, st.calls)
	}
}

func TestRecoverPendingWithdrawalsReanchors(t *testing.T) {
	// Seal a group to learn its frozen root, then present it as 'submitted' for
	// recovery — the recomputed root must match and re-anchor idempotently.
	group := []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_000},
		{ID: "w-2", DID: "did:b", EVMAddress: evmB, AmountMicro: 2_000_000},
	}
	root, _, _ := buildWithdrawalGroup(group)
	for i := range group {
		group[i].PayoutRoot = root
	}
	led := newFakeLedger()
	led.submittedWithdrawals = group
	st := &fakeSettler{tx: "0xRecoveredPayout"}
	w := New(led, st, nil, time.Hour)

	if err := w.RecoverPendingWithdrawals(context.Background()); err != nil {
		t.Fatalf("RecoverPendingWithdrawals: %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("AnchorBatch calls = %d, want 1 (one per payout group)", st.calls)
	}
	if st.lastRoot != root {
		t.Fatalf("recovery must re-anchor the frozen root %q, got %q", root, st.lastRoot)
	}
	if led.settledWithdrawals["w-1"] != "0xRecoveredPayout" || led.settledWithdrawals["w-2"] != "0xRecoveredPayout" {
		t.Fatalf("recovered group must be marked settled, got %v", led.settledWithdrawals)
	}
}

func TestRecoverPendingWithdrawalsRejectsRootMismatch(t *testing.T) {
	// A submitted row whose frozen root does NOT match its (id,evm,amount) is a
	// data-integrity fault: recovery must skip it, never pay.
	led := newFakeLedger()
	led.submittedWithdrawals = []store.QueuedWithdrawal{
		{ID: "w-1", DID: "did:a", EVMAddress: evmA, AmountMicro: 1_000_000, PayoutRoot: leafHex(0xee)},
	}
	st := &fakeSettler{tx: "0xShouldNotPay"}
	w := New(led, st, nil, time.Hour)

	if err := w.RecoverPendingWithdrawals(context.Background()); err != nil {
		t.Fatalf("RecoverPendingWithdrawals: %v", err)
	}
	if st.calls != 0 {
		t.Fatal("a root mismatch must not trigger a payout")
	}
	if len(led.settledWithdrawals) != 0 {
		t.Fatal("a mismatched group must not be settled")
	}
}
