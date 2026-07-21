package engine

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	"github.com/paxlabs-inc/layerx/internal/store"
)

func newEngineStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping engine integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	st, err := store.New(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "../../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.SyncPerpMarkets(ctx, market.All()); err != nil {
		t.Fatalf("sync markets: %v", err)
	}
	return st, ctx
}

func uniqueEngineDID(label string) string {
	return fmt.Sprintf("did:matrix:%s-%d:0123456789abcdef", label, time.Now().UnixNano())
}

func TestPerpJournalAnchoring(t *testing.T) {
	st, ctx := newEngineStore(t)
	e := &Engine{Store: st, LiquidatorDID: "did:layerx:perps:liquidator"}
	settler := &chain.DevSettler{}

	owner := uniqueEngineDID("anchor")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 10_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}
	if _, err := st.FundPerpPool(ctx, owner, "liquidity", 1_000_000, "material",
		func(seq int64, ts time.Time) (string, string) {
			leaf := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, owner, store.PerpPoolDID("liquidity"), 1_000_000, ts.UnixNano()))
			return leaf, "testsig"
		}); err != nil {
		t.Fatalf("FundPerpPool: %v", err)
	}

	batchID, err := e.AnchorPerpJournalOnce(ctx, settler)
	if err != nil {
		t.Fatalf("AnchorPerpJournalOnce: %v", err)
	}
	if batchID == "" {
		t.Fatal("expected a new batch over the journal")
	}
	pending, err := st.ListUnanchoredPerpBatches(ctx)
	if err != nil {
		t.Fatalf("ListUnanchoredPerpBatches: %v", err)
	}
	for _, b := range pending {
		if b.ID == batchID {
			t.Fatalf("batch %s still unanchored: %+v", batchID, b)
		}
	}

	last, err := st.LastAnchoredPerpEventSeq(ctx)
	if err != nil || last == 0 {
		t.Fatalf("LastAnchoredPerpEventSeq = %d %v", last, err)
	}
	rep, err := st.CheckPerpEventSequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last != rep.MaxSeq {
		t.Fatalf("anchor cursor %d != journal max %d", last, rep.MaxSeq)
	}

	again, err := e.AnchorPerpJournalOnce(ctx, settler)
	if err != nil {
		t.Fatalf("second anchor pass: %v", err)
	}
	if again != "" {
		t.Fatalf("second pass sealed %s with no new events", again)
	}

	if err := e.RecoverPerpBatches(ctx, settler); err != nil {
		t.Fatalf("RecoverPerpBatches: %v", err)
	}

	events, err := st.ListPerpJournal(ctx, 0, 10_000)
	if err != nil || len(events) == 0 {
		t.Fatalf("ListPerpJournal: %d %v", len(events), err)
	}
	rederived := PerpJournalRoot(events)
	if hex.EncodeToString(rederived[:]) == "" || rederived == [32]byte{} {
		t.Fatal("re-derived root is zero")
	}
}

func TestPerpEventLeafDeterminism(t *testing.T) {
	e1 := store.PerpEvent{Seq: 7, OwnerDID: "did:a", EventType: "fill.created", Payload: []byte(`{"x":1}`)}
	e2 := store.PerpEvent{Seq: 7, OwnerDID: "did:a", EventType: "fill.created", Payload: []byte(`{"x":1}`)}
	if PerpEventLeaf(e1) != PerpEventLeaf(e2) {
		t.Fatal("identical events must hash identically")
	}
	e2.Seq = 8
	if PerpEventLeaf(e1) == PerpEventLeaf(e2) {
		t.Fatal("seq must be bound into the leaf")
	}
	e2 = e1
	e2.Payload = []byte(`{"x":2}`)
	if PerpEventLeaf(e1) == PerpEventLeaf(e2) {
		t.Fatal("payload must be bound into the leaf")
	}
	root := PerpJournalRoot([]store.PerpEvent{e1, e2})
	if root == [32]byte{} {
		t.Fatal("root must be nonzero")
	}
}
