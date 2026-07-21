package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
)

func testPostgresURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set")
	}
	return uri
}

func activateMarket(t *testing.T, st *Store, ctx context.Context, symbol string) {
	t.Helper()
	m, err := st.GetPerpMarket(ctx, symbol)
	if err != nil {
		t.Fatalf("GetPerpMarket: %v", err)
	}
	if m.Mode == "ACTIVE" {
		return
	}
	clear := m.Mode == "PAUSED"
	if _, err := st.SetPerpMarketMode(ctx, symbol, "ACTIVE", "", "did:op", clear); err != nil {
		t.Fatalf("activate %s: %v", symbol, err)
	}
}

func testRef() PerpSnapshotRef {
	return PerpSnapshotRef{SnapshotID: "intent-proc", OrderbookSeq: 42, StatsSeq: 7, SourceTimestampMs: 1_752_950_000_000}
}

func openIntent(owner, symbol, key string, contracts, priceCents, marginMicro, feeMicro int64) PerpIntent {
	ref := testRef()
	notional := contracts * 10_000_000
	return PerpIntent{
		Operation:   "perps.order",
		RequestHash: "hash-" + key,
		Order: PerpOrder{
			OwnerDID: owner, ActingDID: owner, MarketSymbol: symbol,
			Side: "BUY", OrderType: "MARKET", Contracts: contracts,
			TimeInForce: "IOC", IdempotencyKey: key,
			SnapshotID: ref.SnapshotID, OrderbookSeq: ref.OrderbookSeq,
			StatsSeq: ref.StatsSeq, SourceTimestampMs: ref.SourceTimestampMs,
		},
		AllowRiskIncrease: true,
		AllowedModes:      []string{"CANARY", "ACTIVE"},
		Open: &PerpIntentOpen{
			Fill: PerpIntentFill{
				Contracts: contracts, PriceCents: priceCents,
				NotionalMicro: notional, FeeMicro: feeMicro,
			},
			Side: "LONG", InitialMarginMicro: marginMicro, NewEntryPriceCents: priceCents,
		},
		FinalStatus: "FILLED",
	}
}

func reduceIntent(owner, symbol, key, positionID string, expectContracts, closeContracts, priceCents,
	pnlMicro, feeMicro, marginReturn int64, full bool) PerpIntent {

	ref := testRef()
	notional := closeContracts * 10_000_000
	return PerpIntent{
		Operation:   "perps.order",
		RequestHash: "hash-" + key,
		Order: PerpOrder{
			OwnerDID: owner, ActingDID: owner, MarketSymbol: symbol,
			Side: "SELL", OrderType: "MARKET", Contracts: closeContracts,
			TimeInForce: "IOC", ReduceOnly: true, IdempotencyKey: key,
			SnapshotID: ref.SnapshotID, OrderbookSeq: ref.OrderbookSeq,
			StatsSeq: ref.StatsSeq, SourceTimestampMs: ref.SourceTimestampMs,
		},
		AllowedModes:            []string{"CANARY", "ACTIVE", "REDUCE_ONLY"},
		ExpectPositionID:        positionID,
		ExpectPositionContracts: expectContracts,
		Reduce: &PerpIntentReduce{
			Fill: PerpIntentFill{
				Contracts: closeContracts, PriceCents: priceCents,
				NotionalMicro: notional, FeeMicro: feeMicro,
			},
			CloseContracts: closeContracts, RealizedPnLMicro: pnlMicro,
			MarginReturnMicro: marginReturn, FullClose: full,
		},
		FinalStatus: "FILLED",
	}
}

func TestPerpIntentExactlyOne(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("intent-one")
	activateMarket(t, st, ctx, "BTC")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 100_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	intent := openIntent(owner, "BTC", uniqueDID("key"), 3, 7_000_000, 6_000_000, 15_000)
	const n = 8
	results := make([]PerpExecResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.ExecutePerpIntent(ctx, intent)
		}(i)
	}
	wg.Wait()

	originals := 0
	var canonical map[string]any
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if !results[i].Replayed {
			originals++
		}
		var decoded map[string]any
		if err := json.Unmarshal(results[i].Response, &decoded); err != nil {
			t.Fatalf("worker %d response: %v", i, err)
		}
		if canonical == nil {
			canonical = decoded
		} else if !reflect.DeepEqual(canonical, decoded) {
			t.Fatalf("responses diverge: %v vs %v", canonical, decoded)
		}
	}
	if originals != 1 {
		t.Fatalf("original executions = %d, want exactly 1", originals)
	}
	fills, err := st.ListPerpFills(ctx, owner, "BTC", 0)
	if err != nil || len(fills) != 1 {
		t.Fatalf("fills = %d %v, want exactly 1", len(fills), err)
	}
	acct, _ := st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 100_000_000-6_000_000-15_000 {
		t.Fatalf("balance = %d, want single debit", acct.BalanceUSDX)
	}
	checkDenseEvents(t, st, ctx)
}

func TestPerpIntentResponseLossAndRestart(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("intent-loss")
	activateMarket(t, st, ctx, "ETH")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 50_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}
	intent := openIntent(owner, "ETH", uniqueDID("key"), 2, 200_000, 4_000_000, 10_000)
	first, err := st.ExecutePerpIntent(ctx, intent)
	if err != nil || first.Replayed {
		t.Fatalf("first execution: %v replayed=%v", err, first.Replayed)
	}

	replay, err := st.ExecutePerpIntent(ctx, intent)
	if err != nil || !replay.Replayed {
		t.Fatalf("response-loss retry: %v replayed=%v", err, replay.Replayed)
	}
	if replay.Order.ID != first.Order.ID || replay.FillID != first.FillID || replay.PositionID != first.PositionID {
		t.Fatalf("replay identity differs: %+v vs %+v", replay, first)
	}

	uri := testPostgresURI(t)
	st2, err := New(ctx, uri)
	if err != nil {
		t.Fatalf("restart store: %v", err)
	}
	defer st2.Close()
	afterRestart, err := st2.ExecutePerpIntent(ctx, intent)
	if err != nil || !afterRestart.Replayed || afterRestart.Order.ID != first.Order.ID {
		t.Fatalf("restart retry: %v %+v", err, afterRestart)
	}

	conflicting := intent
	conflicting.RequestHash = "different"
	if _, err := st.ExecutePerpIntent(ctx, conflicting); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting retry = %v, want ErrIdempotencyConflict", err)
	}
	acct, _ := st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 50_000_000-4_000_000-10_000 {
		t.Fatalf("balance = %d, want single debit across retries", acct.BalanceUSDX)
	}
}

func TestPerpIntentPartialCloseConservation(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("intent-close")
	funder := uniqueDID("intent-lp")
	activateMarket(t, st, ctx, "SOL")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if err := st.CreditDeposit(ctx, funder, "0xabc", "0xdep-"+funder, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FundPerpPool(ctx, funder, "liquidity", 50_000_000, "material", finalize(funder, PerpPoolDID("liquidity"), 50_000_000)); err != nil {
		t.Fatal(err)
	}
	b0, err := st.PerpConservationBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sum0 := b0.SpendableMicroUSDX + b0.OpenHoldsMicroUSDX + b0.HeldReservationsMicroUSDX +
		b0.OpenPositionMarginMicroUSDX + b0.LiquidityCapitalMicroUSDX + b0.InsuranceCapitalMicroUSDX

	open, err := st.ExecutePerpIntent(ctx, openIntent(owner, "SOL", uniqueDID("key"), 10, 20_000, 33_400_000, 50_000))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	partial, err := st.ExecutePerpIntent(ctx, reduceIntent(owner, "SOL", uniqueDID("key"), open.PositionID,
		10, 4, 20_500, 1_000_000, 20_000, 13_360_000, false))
	if err != nil {
		t.Fatalf("partial close: %v", err)
	}
	pos, err := st.GetPerpPosition(ctx, partial.PositionID)
	if err != nil || pos.Contracts != 6 || pos.Status != "OPEN" {
		t.Fatalf("position after partial = %+v %v", pos, err)
	}
	if pos.MarginMicro != 33_400_000-20_000+1_000_000-13_360_000 {
		t.Fatalf("margin after partial = %d", pos.MarginMicro)
	}
	if pos.RealizedPnLMicro != 1_000_000 {
		t.Fatalf("realized pnl = %d", pos.RealizedPnLMicro)
	}
	full, err := st.ExecutePerpIntent(ctx, reduceIntent(owner, "SOL", uniqueDID("key"), open.PositionID,
		6, 6, 19_800, -700_000, 30_000, 0, true))
	if err != nil {
		t.Fatalf("full close: %v", err)
	}
	closed, err := st.GetPerpPosition(ctx, full.PositionID)
	if err != nil || closed.Status != "CLOSED" || closed.MarginMicro != 0 || closed.Contracts != 0 {
		t.Fatalf("closed position = %+v %v", closed, err)
	}

	b1, err := st.PerpConservationBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sum1 := b1.SpendableMicroUSDX + b1.OpenHoldsMicroUSDX + b1.HeldReservationsMicroUSDX +
		b1.OpenPositionMarginMicroUSDX + b1.LiquidityCapitalMicroUSDX + b1.InsuranceCapitalMicroUSDX
	if sum0 != sum1 {
		t.Fatalf("conservation broken across open/partial/full close: %d -> %d", sum0, sum1)
	}
	checkDenseEvents(t, st, ctx)
}

func TestPerpIntentModeGate(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("intent-mode")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 50_000_000); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetPerpMarket(ctx, "XRP")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "OFF" {
		clear := m.Mode == "PAUSED"
		if _, err := st.SetPerpMarketMode(ctx, "XRP", "OFF", "", "did:op", clear); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ExecutePerpIntent(ctx, openIntent(owner, "XRP", uniqueDID("key"), 1, 300, 5_000_000, 5_000)); !errors.Is(err, ErrModeDenied) {
		t.Fatalf("OFF market intent = %v, want ErrModeDenied", err)
	}
	activateMarket(t, st, ctx, "XRP")
	if _, err := st.ExecutePerpIntent(ctx, openIntent(owner, "XRP", uniqueDID("key"), 1, 300, 5_000_000, 5_000)); err != nil {
		t.Fatalf("ACTIVE market intent: %v", err)
	}
}

func TestPerpLiquidationWaterfallAndInsolvencyPause(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("liq-owner")
	funder := uniqueDID("liq-lp")
	activateMarket(t, st, ctx, "AVAX")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if err := st.CreditDeposit(ctx, funder, "0xabc", "0xdep-"+funder, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FundPerpPool(ctx, funder, "insurance", 2_000_000, "material", finalize(funder, PerpPoolDID("insurance"), 2_000_000)); err != nil {
		t.Fatal(err)
	}

	open, err := st.ExecutePerpIntent(ctx, openIntent(owner, "AVAX", uniqueDID("key"), 5, 3_000, 16_670_000, 25_000))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ref := testRef()

	res, err := st.LiquidatePerpPosition(ctx, PerpLiquidationPlan{
		PositionID: open.PositionID, ExpectContracts: 5, CloseContracts: 2, FullClose: false,
		Fill:             PerpIntentFill{Contracts: 2, PriceCents: 2_400, NotionalMicro: 20_000_000, FeeMicro: 150_000, Liquidation: true},
		RealizedPnLMicro: -4_000_000, ActingDID: "did:liq", IdempotencyKey: uniqueDID("liq"), Ref: ref,
	})
	if err != nil {
		t.Fatalf("partial liquidation: %v", err)
	}
	if res.MarginAbsorbedMicro != 4_000_000 || res.InsurancePaidMicro != 0 || res.DeficitMicro != 0 || res.Paused {
		t.Fatalf("partial waterfall = %+v", res)
	}
	replay, err := st.LiquidatePerpPosition(ctx, PerpLiquidationPlan{
		PositionID: open.PositionID, ExpectContracts: 5, CloseContracts: 2, FullClose: false,
		Fill:             PerpIntentFill{Contracts: 2, PriceCents: 2_400, NotionalMicro: 20_000_000, FeeMicro: 150_000, Liquidation: true},
		RealizedPnLMicro: -4_000_000, ActingDID: "did:liq", IdempotencyKey: uniqueDID("liq"), Ref: ref,
	})
	if !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale re-liquidation = %v %+v, want ErrPlanStale", err, replay)
	}

	pos, err := st.GetPerpPosition(ctx, open.PositionID)
	if err != nil || pos.Contracts != 3 {
		t.Fatalf("position after partial liq = %+v %v", pos, err)
	}
	balBefore, _ := st.GetAccount(ctx, owner)
	insBefore := insuranceLeft(t, st, ctx)

	res2, err := st.LiquidatePerpPosition(ctx, PerpLiquidationPlan{
		PositionID: open.PositionID, ExpectContracts: 3, CloseContracts: 3, FullClose: true,
		Fill:             PerpIntentFill{Contracts: 3, PriceCents: 1_200, NotionalMicro: 30_000_000, FeeMicro: 225_000, Liquidation: true},
		RealizedPnLMicro: -100_000_000_000, ActingDID: "did:liq", IdempotencyKey: uniqueDID("liq"), Ref: ref,
	})
	if err != nil {
		t.Fatalf("insolvent liquidation: %v", err)
	}
	if res2.DeficitMicro <= 0 || !res2.Paused {
		t.Fatalf("insolvency waterfall = %+v, want deficit and pause", res2)
	}
	wantMargin := pos.MarginMicro - 225_000
	if res2.MarginAbsorbedMicro != wantMargin {
		t.Fatalf("margin absorbed = %d, want %d (all remaining after fee)", res2.MarginAbsorbedMicro, wantMargin)
	}
	if res2.InsurancePaidMicro != insBefore || insuranceLeft(t, st, ctx) != 0 {
		t.Fatalf("insurance paid = %d, want full drain of %d", res2.InsurancePaidMicro, insBefore)
	}
	balAfter, _ := st.GetAccount(ctx, owner)
	if balAfter.BalanceUSDX != balBefore.BalanceUSDX {
		t.Fatalf("user credit written during insolvency: %d -> %d", balBefore.BalanceUSDX, balAfter.BalanceUSDX)
	}
	markets, err := st.ListPerpMarkets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range markets {
		if m.Mode != "PAUSED" {
			t.Fatalf("market %s mode = %s, want PAUSED after insolvency", m.Symbol, m.Mode)
		}
	}
	events, err := st.ListPerpJournal(ctx, 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	foundInsolvency := false
	for _, ev := range events {
		if ev.EventType == "perps.insolvency" {
			foundInsolvency = true
		}
	}
	if !foundInsolvency {
		t.Fatal("perps.insolvency event not journaled")
	}
	checkDenseEvents(t, st, ctx)
}

func insuranceLeft(t *testing.T, st *Store, ctx context.Context) int64 {
	t.Helper()
	pools, err := st.PerpPoolCapital(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return pools.InsuranceMicroUSDX
}

func TestPerpTriggeredOrderExactlyOnce(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("trigger")
	activateMarket(t, st, ctx, "LINK")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 100_000_000); err != nil {
		t.Fatal(err)
	}
	ref := testRef()
	restKey := uniqueDID("rest")
	rest, err := st.ExecutePerpIntent(ctx, PerpIntent{
		Operation: "perps.order", RequestHash: "hash-" + restKey,
		Order: PerpOrder{
			OwnerDID: owner, ActingDID: owner, MarketSymbol: "LINK",
			Side: "BUY", OrderType: "STOP_MARKET", Contracts: 2, StopPriceCents: 1_500,
			TimeInForce: "GTC", IdempotencyKey: restKey, Status: "RESTING",
			SnapshotID: ref.SnapshotID, OrderbookSeq: ref.OrderbookSeq,
			StatsSeq: ref.StatsSeq, SourceTimestampMs: ref.SourceTimestampMs,
		},
		AllowRiskIncrease: true,
		AllowedModes:      []string{"CANARY", "ACTIVE"},
		Rest:              &PerpIntentRest{ReserveMicro: 6_700_000},
	})
	if err != nil {
		t.Fatalf("rest: %v", err)
	}
	r, err := st.HeldReservationForOrder(ctx, rest.Order.ID)
	if err != nil {
		t.Fatalf("HeldReservationForOrder: %v", err)
	}

	trigger := func() (PerpExecResult, error) {
		return st.ExecutePerpIntent(ctx, PerpIntent{
			Operation: "perps.trigger", RequestHash: "hash-trigger",
			Order: PerpOrder{
				OwnerDID: owner, ActingDID: owner, MarketSymbol: "LINK",
				Side: "BUY", OrderType: "STOP_MARKET", Contracts: 2,
				TimeInForce: "GTC", IdempotencyKey: restKey + ":exec",
				SnapshotID: ref.SnapshotID, OrderbookSeq: ref.OrderbookSeq,
				StatsSeq: ref.StatsSeq, SourceTimestampMs: ref.SourceTimestampMs,
			},
			AllowRiskIncrease: true,
			AllowedModes:      []string{"CANARY", "ACTIVE"},
			TriggeredOrderID:  rest.Order.ID,
			Open: &PerpIntentOpen{
				Fill: PerpIntentFill{Contracts: 2, PriceCents: 1_510, NotionalMicro: 20_000_000, FeeMicro: 10_000},
				Side: "LONG", InitialMarginMicro: 6_680_000, NewEntryPriceCents: 1_510,
				FromReservationID: r.ID,
			},
			FinalStatus: "FILLED",
		})
	}

	const n = 6
	var wg sync.WaitGroup
	results := make([]PerpExecResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = trigger()
		}(i)
	}
	wg.Wait()
	originals := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("trigger %d: %v", i, errs[i])
		}
		if !results[i].Replayed {
			originals++
		}
	}
	if originals != 1 {
		t.Fatalf("triggered executions = %d, want exactly 1", originals)
	}
	o, err := st.GetPerpOrder(ctx, rest.Order.ID)
	if err != nil || o.Status != "FILLED" || o.FilledContracts != 2 {
		t.Fatalf("order after trigger = %+v %v", o, err)
	}
	rr, err := st.GetPerpMarginReservation(ctx, r.ID)
	if err != nil || rr.Status != "applied" {
		t.Fatalf("reservation after trigger = %+v %v", rr, err)
	}
	acct, _ := st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 100_000_000-6_700_000+10_000 {
		t.Fatalf("balance = %d, want reserve consumed once with excess returned", acct.BalanceUSDX)
	}
	checkDenseEvents(t, st, ctx)
}
