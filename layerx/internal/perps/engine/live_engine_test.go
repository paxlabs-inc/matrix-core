package engine

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/mode"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
)

// TestLiveEngineOrderFlow drives the REAL engine end-to-end: live Crossverse
// market data, real Postgres ledger, real pricing/risk math, exactly-once
// idempotent execution, stop-order trigger worker, funding worker, and the
// reconciliation gate. Skips unless both CROSSVERSE_TEST_URL and
// LAYERX_TEST_POSTGRES_URI are set.
func TestLiveEngineOrderFlow(t *testing.T) {
	baseURL := os.Getenv("CROSSVERSE_TEST_URL")
	if baseURL == "" {
		t.Skip("CROSSVERSE_TEST_URL not set")
	}
	st, ctx := newEngineStore(t)

	m, err := crossverse.New(crossverse.Config{BaseURL: baseURL, Symbols: []string{"BTC"}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	feedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(feedCtx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for live BTC feed + aggregate")
		}
		allowed, err := m.RiskIncreaseAllowed("BTC")
		if err != nil {
			t.Fatal(err)
		}
		if allowed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if row, err := st.GetPerpMarket(ctx, "BTC"); err != nil {
		t.Fatal(err)
	} else if row.Mode != "ACTIVE" {
		if _, err := st.SetPerpMarketMode(ctx, "BTC", "ACTIVE", "", "did:op", row.Mode == "PAUSED"); err != nil {
			t.Fatal(err)
		}
	}
	modes, err := mode.NewRegistry(mode.Active, map[string]mode.Mode{"BTC": mode.Active})
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := sig.New("")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Feed: m, Modes: modes, Signer: signer, LiquidatorDID: "did:layerx:perps:liquidator"}

	owner := uniqueEngineDID("live-engine")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 30_000_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FundPerpPool(ctx, owner, "liquidity", 21_000_000_000, "material",
		func(seq int64, ts time.Time) (string, string) {
			leaf := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, owner, store.PerpPoolDID("liquidity"), 21_000_000_000, ts.UnixNano()))
			return leaf, "testsig"
		}); err != nil {
		t.Fatal(err)
	}

	buyKey := uniqueEngineDID("buy")
	buy := OrderRequest{
		OwnerDID: owner, Symbol: "BTC", Side: "BUY", OrderType: "MARKET",
		Contracts: 2, TimeInForce: "IOC", IdempotencyKey: buyKey, RequestHash: "h-" + buyKey,
	}
	res, err := e.PlaceOrder(ctx, buy)
	if err != nil {
		t.Fatalf("live market buy: %v", err)
	}
	if res.Order.Status != "FILLED" || res.FillID == "" || res.PositionID == "" || res.Replayed {
		t.Fatalf("buy result = %+v", res)
	}
	if res.Receipt.SequencerSignature == "" || res.Receipt.SnapshotID == "" || res.Receipt.EventSeqLo == 0 {
		t.Fatalf("receipt incomplete: %+v", res.Receipt)
	}

	replay, err := e.PlaceOrder(ctx, buy)
	if err != nil || !replay.Replayed || replay.Order.ID != res.Order.ID || replay.FillID != res.FillID {
		t.Fatalf("idempotent replay = %+v %v", replay, err)
	}

	pos, err := st.GetPerpPosition(ctx, res.PositionID)
	if err != nil || pos.Side != "LONG" || pos.Contracts != 2 || pos.MarginMicro <= 0 {
		t.Fatalf("live position = %+v %v", pos, err)
	}

	if settled, err := e.RunFundingOnce(ctx, time.Now().UnixMilli()); err != nil {
		t.Fatalf("funding worker: %v (settled %d)", err, settled)
	}
	if _, err := e.RunLiquidationOnce(ctx); err != nil {
		t.Fatalf("liquidation worker: %v", err)
	}

	stopKey := uniqueEngineDID("stop")
	snap, err := m.Snapshot("BTC")
	if err != nil {
		t.Fatal(err)
	}
	stop := OrderRequest{
		OwnerDID: owner, Symbol: "BTC", Side: "SELL", OrderType: "STOP_LOSS",
		Contracts: 1, StopPriceCents: snap.MarkPriceCents + 100_000, TimeInForce: "GTC",
		ReduceOnly: true, IdempotencyKey: stopKey, RequestHash: "h-" + stopKey,
	}
	stopRes, err := e.PlaceOrder(ctx, stop)
	if err != nil || stopRes.Order.Status != "RESTING" {
		t.Fatalf("stop-loss rest = %+v %v", stopRes, err)
	}
	fired, err := e.RunTriggerOnce(ctx)
	if err != nil {
		t.Fatalf("trigger worker: %v", err)
	}
	if fired == 0 {
		t.Fatal("stop-loss below mark must trigger immediately")
	}
	triggeredOrder, err := st.GetPerpOrder(ctx, stopRes.Order.ID)
	if err != nil || triggeredOrder.Status != "FILLED" {
		t.Fatalf("triggered order = %+v %v", triggeredOrder, err)
	}
	pos, err = st.GetPerpPosition(ctx, res.PositionID)
	if err != nil || pos.Contracts != 1 {
		t.Fatalf("position after trigger = %+v %v", pos, err)
	}

	closeKey := uniqueEngineDID("close")
	closeRes, err := e.PlaceOrder(ctx, OrderRequest{
		OwnerDID: owner, Symbol: "BTC", Side: "SELL", OrderType: "MARKET",
		Contracts: 1, TimeInForce: "IOC", ReduceOnly: true,
		IdempotencyKey: closeKey, RequestHash: "h-" + closeKey,
	})
	if err != nil {
		t.Fatalf("live close: %v", err)
	}
	closed, err := st.GetPerpPosition(ctx, closeRes.PositionID)
	if err != nil || closed.Status != "CLOSED" || closed.Contracts != 0 || closed.MarginMicro != 0 {
		t.Fatalf("closed position = %+v %v", closed, err)
	}
	if _, err := st.GetOpenPerpPosition(ctx, owner, "BTC"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open position after close = %v, want ErrNotFound", err)
	}

	if err := e.ReconcileOnce(ctx); err != nil {
		t.Fatalf("reconciliation: %v", err)
	}
}
