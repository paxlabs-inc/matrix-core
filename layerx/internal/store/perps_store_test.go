package store

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/perps/market"
)

func newPerpTestStore(t *testing.T) (*Store, context.Context) {
	st, ctx := newTestStore(t)
	if err := st.SyncPerpMarkets(ctx, market.All()); err != nil {
		t.Fatalf("SyncPerpMarkets: %v", err)
	}
	return st, ctx
}

func perpTestOrder(t *testing.T, st *Store, ctx context.Context, owner, symbol string) PerpOrder {
	t.Helper()
	o, err := st.InsertPerpOrder(ctx, PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: symbol,
		Side: "BUY", OrderType: "MARKET", Contracts: 10,
		TimeInForce: "IOC", IdempotencyKey: uniqueDID("idem"),
		Status: "ACCEPTED",
	})
	if err != nil {
		t.Fatalf("InsertPerpOrder: %v", err)
	}
	return o
}

func checkDenseEvents(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	rep, err := st.CheckPerpEventSequences(ctx)
	if err != nil {
		t.Fatalf("CheckPerpEventSequences: %v", err)
	}
	if rep.Total != rep.MaxSeq {
		t.Fatalf("perp journal has gaps: %d rows, max seq %d", rep.Total, rep.MaxSeq)
	}
	if rep.OwnerGaps != 0 {
		t.Fatalf("%d owners have gapped owner_event_id sequences", rep.OwnerGaps)
	}
}

func TestPerpMarketsSyncAndMode(t *testing.T) {
	st, ctx := newPerpTestStore(t)

	rows, err := st.ListPerpMarkets(ctx)
	if err != nil {
		t.Fatalf("ListPerpMarkets: %v", err)
	}
	if len(rows) != market.Count() {
		t.Fatalf("markets = %d, want %d", len(rows), market.Count())
	}
	btc, err := st.GetPerpMarket(ctx, "BTC")
	if err != nil {
		t.Fatalf("GetPerpMarket: %v", err)
	}
	locked, _ := market.Lookup("BTC")
	if btc.Market != locked {
		t.Fatalf("BTC row %+v differs from registry %+v", btc.Market, locked)
	}
	if btc.Mode == "PAUSED" {
		if _, err := st.SetPerpMarketMode(ctx, "BTC", "OFF", "", "did:op", true); err != nil {
			t.Fatalf("reset paused BTC: %v", err)
		}
	} else if btc.Mode != "OFF" {
		if _, err := st.SetPerpMarketMode(ctx, "BTC", "OFF", "", "did:op", false); err != nil {
			t.Fatalf("reset BTC: %v", err)
		}
	}
	if err := st.SyncPerpMarkets(ctx, market.All()); err != nil {
		t.Fatalf("resync: %v", err)
	}

	if _, err := st.SetPerpMarketMode(ctx, "BTC", "PAUSED", "", "did:op", false); err == nil {
		t.Fatal("pausing without a cause must be rejected")
	}
	if _, err := st.SetPerpMarketMode(ctx, "BTC", "SHADOW", "", "did:op", false); err != nil {
		t.Fatalf("set SHADOW: %v", err)
	}
	paused, err := st.SetPerpMarketMode(ctx, "BTC", "PAUSED", "drill", "did:op", false)
	if err != nil || paused.PausedCause != "drill" {
		t.Fatalf("pause: %v %+v", err, paused)
	}
	if _, err := st.SetPerpMarketMode(ctx, "BTC", "ACTIVE", "", "did:op", false); !errors.Is(err, ErrMarketPauseHeld) {
		t.Fatalf("leaving PAUSED without clear = %v, want ErrMarketPauseHeld", err)
	}
	cleared, err := st.SetPerpMarketMode(ctx, "BTC", "OFF", "", "did:op", true)
	if err != nil || cleared.Mode != "OFF" || cleared.PausedCause != "" {
		t.Fatalf("clear pause: %v %+v", err, cleared)
	}
	checkDenseEvents(t, st, ctx)
}

func TestPerpOrderIdempotency(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("perp-order")
	key := uniqueDID("key")

	o, err := st.InsertPerpOrder(ctx, PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: "BTC",
		Side: "BUY", OrderType: "LIMIT", Contracts: 5, LimitPriceCents: 7_000_000,
		TimeInForce: "GTC", IdempotencyKey: key, Status: "RESTING",
	})
	if err != nil {
		t.Fatalf("InsertPerpOrder: %v", err)
	}
	if _, err := st.InsertPerpOrder(ctx, PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: "ETH",
		Side: "SELL", OrderType: "MARKET", Contracts: 1,
		TimeInForce: "IOC", IdempotencyKey: key,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency insert = %v, want ErrIdempotencyConflict", err)
	}
	got, err := st.GetPerpOrderByIdempotency(ctx, owner, key)
	if err != nil || got.ID != o.ID {
		t.Fatalf("GetPerpOrderByIdempotency: %v %+v", err, got)
	}

	if _, err := st.UpdatePerpOrderStatus(ctx, o.ID, "CANCELLED", owner); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := st.UpdatePerpOrderStatus(ctx, o.ID, "RESTING", owner); !errors.Is(err, ErrOrderTerminal) {
		t.Fatalf("transition out of terminal = %v, want ErrOrderTerminal", err)
	}

	hash := "hash-a"
	resp, claimed, err := st.ClaimPerpIdempotency(ctx, owner, key, "perps.order", hash)
	if err != nil || !claimed || resp != nil {
		t.Fatalf("claim: %v claimed=%v resp=%s", err, claimed, resp)
	}
	if _, _, err := st.ClaimPerpIdempotency(ctx, owner, key, "perps.order", hash); !errors.Is(err, ErrIdempotencyInFlight) {
		t.Fatalf("in-flight retry = %v, want ErrIdempotencyInFlight", err)
	}
	if err := st.CompletePerpIdempotency(ctx, owner, key, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	resp, claimed, err = st.ClaimPerpIdempotency(ctx, owner, key, "perps.order", hash)
	if err != nil || claimed || string(resp) != `{"ok": true}` {
		t.Fatalf("replay = %v claimed=%v resp=%s", err, claimed, resp)
	}
	if _, _, err := st.ClaimPerpIdempotency(ctx, owner, key, "perps.order", "hash-b"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different-hash retry = %v, want ErrIdempotencyConflict", err)
	}
	checkDenseEvents(t, st, ctx)
}

func TestPerpMarginPositionLifecycle(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("perp-pos")
	funder := uniqueDID("perp-lp")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 100_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}
	if err := st.CreditDeposit(ctx, funder, "0xabc", "0xdep-"+funder, 100_000_000); err != nil {
		t.Fatalf("CreditDeposit funder: %v", err)
	}

	order := perpTestOrder(t, st, ctx, owner, "BTC")
	if _, err := st.ReservePerpMargin(ctx, owner, order.ID, 200_000_000); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("over-reserve = %v, want ErrInsufficientFunds", err)
	}
	res, err := st.ReservePerpMargin(ctx, owner, order.ID, 20_000_000)
	if err != nil {
		t.Fatalf("ReservePerpMargin: %v", err)
	}
	acct, _ := st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 80_000_000 {
		t.Fatalf("balance after reserve = %d, want 80_000_000", acct.BalanceUSDX)
	}

	rel, err := st.ReleasePerpMarginReservation(ctx, res.ID)
	if err != nil || rel.Status != "released" {
		t.Fatalf("release: %v %+v", err, rel)
	}
	if _, err := st.ReleasePerpMarginReservation(ctx, res.ID); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	acct, _ = st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 100_000_000 {
		t.Fatalf("balance after release = %d, want 100_000_000 (once)", acct.BalanceUSDX)
	}

	res, err = st.ReservePerpMargin(ctx, owner, order.ID, 20_000_000)
	if err != nil {
		t.Fatalf("re-reserve: %v", err)
	}
	ref := PerpSnapshotRef{SnapshotID: "proc-1", OrderbookSeq: 100, StatsSeq: 10, SourceTimestampMs: 1}
	pos, err := st.OpenPerpPositionFromReservation(ctx, res.ID, "BTC", "LONG", 10, 7_000_000, ref)
	if err != nil {
		t.Fatalf("OpenPerpPositionFromReservation: %v", err)
	}
	if pos.MarginMicro != 20_000_000 || pos.Status != "OPEN" {
		t.Fatalf("position = %+v", pos)
	}
	if _, err := st.ReleasePerpMarginReservation(ctx, res.ID); !errors.Is(err, ErrReservationClosed) {
		t.Fatalf("release applied reservation = %v, want ErrReservationClosed", err)
	}
	res2, err := st.ReservePerpMargin(ctx, owner, order.ID, 1_000_000)
	if err != nil {
		t.Fatalf("second reservation: %v", err)
	}
	if _, err := st.OpenPerpPositionFromReservation(ctx, res2.ID, "BTC", "LONG", 1, 7_000_000, ref); err == nil {
		t.Fatal("second open position on same (owner, market) must be rejected")
	}
	if _, err := st.ReleasePerpMarginReservation(ctx, res2.ID); err != nil {
		t.Fatalf("cleanup release: %v", err)
	}

	pools0, err := st.PerpPoolCapital(ctx)
	if err != nil {
		t.Fatalf("PerpPoolCapital: %v", err)
	}
	if _, err := st.RealizePerpPnL(ctx, pos.ID, pools0.LiquidityMicroUSDX+5_000_000); !errors.Is(err, ErrPoolInsufficient) {
		t.Fatalf("pnl beyond pool capital = %v, want ErrPoolInsufficient", err)
	}
	if _, err := st.FundPerpPool(ctx, funder, "liquidity", 50_000_000, "material", finalize(funder, PerpPoolDID("liquidity"), 50_000_000)); err != nil {
		t.Fatalf("FundPerpPool: %v", err)
	}
	if _, err := st.FundPerpPool(ctx, funder, "insurance", 10_000_000, "material", finalize(funder, PerpPoolDID("insurance"), 10_000_000)); err != nil {
		t.Fatalf("FundPerpPool insurance: %v", err)
	}
	pools, err := st.PerpPoolCapital(ctx)
	if err != nil ||
		pools.LiquidityMicroUSDX != pools0.LiquidityMicroUSDX+50_000_000 ||
		pools.InsuranceMicroUSDX != pools0.InsuranceMicroUSDX+10_000_000 {
		t.Fatalf("pools = %+v (base %+v) err %v", pools, pools0, err)
	}

	p2, err := st.RealizePerpPnL(ctx, pos.ID, 5_000_000)
	if err != nil || p2.MarginMicro != 25_000_000 || p2.RealizedPnLMicro != 5_000_000 {
		t.Fatalf("positive pnl: %v %+v", err, p2)
	}
	if _, err := st.RealizePerpPnL(ctx, pos.ID, -30_000_000); !errors.Is(err, ErrMarginInsufficient) {
		t.Fatalf("pnl below margin = %v, want ErrMarginInsufficient", err)
	}

	if err := st.ApplyPerpFunding(ctx, pos.ID, 1000, 2000, 120_000, -1_000_000, ref); err != nil {
		t.Fatalf("ApplyPerpFunding: %v", err)
	}
	if err := st.ApplyPerpFunding(ctx, pos.ID, 1000, 2000, 120_000, -1_000_000, ref); err != nil {
		t.Fatalf("idempotent funding replay: %v", err)
	}
	if err := st.ApplyPerpFunding(ctx, pos.ID, 1000, 2000, 120_000, -2_000_000, ref); err == nil {
		t.Fatal("funding replay with different values must be rejected")
	}
	p3, err := st.GetPerpPosition(ctx, pos.ID)
	if err != nil || p3.MarginMicro != 24_000_000 {
		t.Fatalf("margin after funding = %+v err %v", p3, err)
	}

	fill, err := st.InsertPerpFill(ctx, PerpFill{
		OrderID: order.ID, PositionID: pos.ID, OwnerDID: owner, ActingDID: owner,
		MarketSymbol: "BTC", Side: "BUY", Contracts: 10, PriceCents: 7_000_000,
		NotionalMicro: 100_000_000, FeeMicro: 50_000, Ref: ref,
	})
	if err != nil || fill.ID == "" {
		t.Fatalf("InsertPerpFill: %v", err)
	}
	if _, err := st.InsertPerpFill(ctx, PerpFill{
		OrderID: order.ID, PositionID: pos.ID, OwnerDID: owner, ActingDID: owner,
		MarketSymbol: "BTC", Side: "BUY", Contracts: 1, PriceCents: 7_000_000,
		NotionalMicro: 10_000_000, FeeMicro: 5_000,
	}); err == nil {
		t.Fatal("fill without snapshot reference must be rejected")
	}

	reduced, err := st.ReducePerpPosition(ctx, pos.ID, 4, 9_000_000)
	if err != nil || reduced.Contracts != 6 || reduced.MarginMicro != 15_000_000 {
		t.Fatalf("reduce: %v %+v", err, reduced)
	}
	closed, err := st.ReducePerpPosition(ctx, pos.ID, 6, 0)
	if err != nil || closed.Status != "CLOSED" || closed.MarginMicro != 0 {
		t.Fatalf("close: %v %+v", err, closed)
	}
	acct, _ = st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 80_000_000+9_000_000+15_000_000 {
		t.Fatalf("balance after close = %d, want 104_000_000", acct.BalanceUSDX)
	}
	if _, err := st.GetOpenPerpPosition(ctx, owner, "BTC"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open position after close = %v, want ErrNotFound", err)
	}
	if _, err := st.RealizePerpPnL(ctx, pos.ID, 1); !errors.Is(err, ErrPositionClosed) {
		t.Fatalf("pnl on closed = %v, want ErrPositionClosed", err)
	}
	if _, err := st.AddPerpPositionMargin(ctx, pos.ID, 1); !errors.Is(err, ErrPositionClosed) {
		t.Fatalf("margin add on closed = %v, want ErrPositionClosed", err)
	}
	checkDenseEvents(t, st, ctx)
}

func TestPerpDelegationRevocation(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	owner := uniqueDID("perp-owner")
	agent := uniqueDID("perp-agent")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 50_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	d, err := st.CreatePerpDelegation(ctx, PerpDelegation{
		OwnerDID: owner, DelegateDID: agent, MembershipTier: "pro",
		AllowedMarkets: []string{"BTC", "ETH"}, AllowedOrderTypes: []string{"MARKET", "LIMIT"},
		MaxOrderNotionalMicro: 100_000_000, MaxPositionNotionalMicro: 200_000_000,
		MaxLeverageX: 2, MaxDailyNotionalMicro: 500_000_000, MaxDailyRealizedLossMicro: 50_000_000,
		GrantSignature: "sig", PublicKey: "pk", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil || d.Status != "ACTIVE" {
		t.Fatalf("CreatePerpDelegation: %v %+v", err, d)
	}
	if _, err := st.CreatePerpDelegation(ctx, PerpDelegation{
		OwnerDID: owner, DelegateDID: agent, MembershipTier: "pro",
		AllowedMarkets: []string{"BTC"}, AllowedOrderTypes: []string{"MARKET"},
		MaxOrderNotionalMicro: 1, MaxPositionNotionalMicro: 1, MaxLeverageX: 1,
		MaxDailyNotionalMicro: 1, MaxDailyRealizedLossMicro: 0,
		GrantSignature: "sig2", PublicKey: "pk", ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("second active grant for the same pair must be rejected")
	}

	o, err := st.InsertPerpOrder(ctx, PerpOrder{
		OwnerDID: owner, ActingDID: agent, MarketSymbol: "BTC",
		Side: "BUY", OrderType: "LIMIT", Contracts: 2, LimitPriceCents: 7_000_000,
		TimeInForce: "GTC", IdempotencyKey: uniqueDID("idem"), Status: "RESTING",
	})
	if err != nil {
		t.Fatalf("delegate order: %v", err)
	}
	if _, err := st.ReservePerpMargin(ctx, owner, o.ID, 10_000_000); err != nil {
		t.Fatalf("delegate reservation: %v", err)
	}

	if _, _, err := st.RevokePerpDelegation(ctx, d.ID, agent); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("delegate self-revoke = %v, want ErrNotOwner", err)
	}
	revoked, cancelled, err := st.RevokePerpDelegation(ctx, d.ID, owner)
	if err != nil || revoked.Status != "REVOKED" {
		t.Fatalf("revoke: %v %+v", err, revoked)
	}
	if len(cancelled) != 1 || cancelled[0] != o.ID {
		t.Fatalf("cancelled = %v, want [%s]", cancelled, o.ID)
	}
	oc, _ := st.GetPerpOrder(ctx, o.ID)
	if oc.Status != "CANCELLED" {
		t.Fatalf("order status = %q, want CANCELLED", oc.Status)
	}
	acct, _ := st.GetAccount(ctx, owner)
	if acct.BalanceUSDX != 50_000_000 {
		t.Fatalf("balance after revoke = %d, want 50_000_000 (margin returned)", acct.BalanceUSDX)
	}
	again, cancelledAgain, err := st.RevokePerpDelegation(ctx, d.ID, owner)
	if err != nil || again.Status != "REVOKED" || len(cancelledAgain) != 0 {
		t.Fatalf("idempotent revoke: %v %+v %v", err, again, cancelledAgain)
	}
	checkDenseEvents(t, st, ctx)
}

// TestPerpConservationProperty drives a seeded random interleaving of every
// perps value movement (pool funding, margin reserve/release, position open,
// realized PnL, funding, reduce, close) plus the existing ledger ops (pay,
// withdraw) against the REAL store, asserting after every step that (1) every
// conservation bucket equals the model exactly, (2) the store's circulating
// supply moved exactly as the model predicts, and (3) the perps event journal
// remains gap-free globally and per owner.
func TestPerpConservationProperty(t *testing.T) {
	st, ctx := newPerpTestStore(t)
	rng := rand.New(rand.NewSource(7))

	const nAccounts = 4
	const deposit = int64(200_000_000)
	dids := make([]string, nAccounts)
	balance := map[string]int64{}
	for i := range dids {
		dids[i] = uniqueDID("perp-prop")
		if err := st.CreditDeposit(ctx, dids[i], "0xabc", "0xdep-"+dids[i], deposit); err != nil {
			t.Fatalf("CreditDeposit: %v", err)
		}
		balance[dids[i]] = deposit
	}

	type reservation struct {
		id     string
		owner  string
		amount int64
	}
	type position struct {
		id        string
		owner     string
		symbol    string
		contracts int64
		margin    int64
	}
	var reservations []reservation
	var positions []position
	pool := map[string]int64{"liquidity": 0, "insurance": 0}
	var withdrawn int64

	orderFor := func(owner, symbol string) string {
		return perpTestOrder(t, st, ctx, owner, symbol).ID
	}
	symbols := []string{"BTC", "ETH", "SOL", "XRP"}
	ref := PerpSnapshotRef{SnapshotID: "prop-proc", OrderbookSeq: 1, StatsSeq: 1, SourceTimestampMs: 1}

	buckets0, err := st.PerpConservationBuckets(ctx)
	if err != nil {
		t.Fatalf("PerpConservationBuckets: %v", err)
	}
	supply0, err := st.Supply(ctx)
	if err != nil {
		t.Fatalf("Supply: %v", err)
	}

	modelReserved := func() int64 {
		var v int64
		for _, r := range reservations {
			v += r.amount
		}
		return v
	}
	modelMargin := func() int64 {
		var v int64
		for _, p := range positions {
			v += p.margin
		}
		return v
	}
	modelBalances := func() int64 {
		var v int64
		for _, d := range dids {
			v += balance[d]
		}
		return v
	}

	check := func(step int) {
		t.Helper()
		b, err := st.PerpConservationBuckets(ctx)
		if err != nil {
			t.Fatalf("step %d: buckets: %v", step, err)
		}
		if got, want := b.SpendableMicroUSDX-buckets0.SpendableMicroUSDX, modelBalances()-int64(nAccounts)*deposit; got != want {
			t.Fatalf("step %d: spendable delta = %d, model %d", step, got, want)
		}
		if got, want := b.HeldReservationsMicroUSDX-buckets0.HeldReservationsMicroUSDX, modelReserved(); got != want {
			t.Fatalf("step %d: reservations = %d, model %d", step, got, want)
		}
		if got, want := b.OpenPositionMarginMicroUSDX-buckets0.OpenPositionMarginMicroUSDX, modelMargin(); got != want {
			t.Fatalf("step %d: position margin = %d, model %d", step, got, want)
		}
		if got, want := b.LiquidityCapitalMicroUSDX-buckets0.LiquidityCapitalMicroUSDX, pool["liquidity"]; got != want {
			t.Fatalf("step %d: liquidity = %d, model %d", step, got, want)
		}
		if got, want := b.InsuranceCapitalMicroUSDX-buckets0.InsuranceCapitalMicroUSDX, pool["insurance"]; got != want {
			t.Fatalf("step %d: insurance = %d, model %d", step, got, want)
		}
		sup, err := st.Supply(ctx)
		if err != nil {
			t.Fatalf("step %d: Supply: %v", step, err)
		}
		wantCirc := modelBalances() + modelReserved() + modelMargin() + pool["liquidity"] + pool["insurance"] - int64(nAccounts)*deposit
		if got := sup.CirculatingMicroUSDX - supply0.CirculatingMicroUSDX; got != wantCirc {
			t.Fatalf("step %d: circulating delta = %d, model %d", step, got, wantCirc)
		}
		total := modelBalances() + modelReserved() + modelMargin() + pool["liquidity"] + pool["insurance"] + withdrawn
		if total != int64(nAccounts)*deposit {
			t.Fatalf("step %d: conservation broken: %d, want %d", step, total, int64(nAccounts)*deposit)
		}
	}

	for step := 0; step < 150; step++ {
		owner := dids[rng.Intn(nAccounts)]
		amount := int64(rng.Intn(5_000_000) + 1)
		switch op := rng.Intn(9); op {
		case 0:
			to := dids[rng.Intn(nAccounts)]
			_, err := st.Pay(ctx, owner, to, amount, "micropayment", "", finalize(owner, to, amount))
			switch {
			case owner == to:
				if err == nil {
					t.Fatalf("step %d: self-pay accepted", step)
				}
			case balance[owner] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: pay underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: pay: %v", step, err)
			default:
				balance[owner] -= amount
				balance[to] += amount
			}
		case 1:
			which := []string{"liquidity", "insurance"}[rng.Intn(2)]
			_, err := st.FundPerpPool(ctx, owner, which, amount, "material", finalize(owner, PerpPoolDID(which), amount))
			switch {
			case balance[owner] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: fund underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: fund pool: %v", step, err)
			default:
				balance[owner] -= amount
				pool[which] += amount
			}
		case 2:
			symbol := symbols[rng.Intn(len(symbols))]
			r, err := st.ReservePerpMargin(ctx, owner, orderFor(owner, symbol), amount)
			switch {
			case balance[owner] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: reserve underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: reserve: %v", step, err)
			default:
				balance[owner] -= amount
				reservations = append(reservations, reservation{id: r.ID, owner: owner, amount: amount})
			}
		case 3:
			if len(reservations) == 0 {
				continue
			}
			i := rng.Intn(len(reservations))
			r := reservations[i]
			if _, err := st.ReleasePerpMarginReservation(ctx, r.id); err != nil {
				t.Fatalf("step %d: release: %v", step, err)
			}
			balance[r.owner] += r.amount
			reservations = append(reservations[:i], reservations[i+1:]...)
		case 4:
			if len(reservations) == 0 {
				continue
			}
			i := rng.Intn(len(reservations))
			r := reservations[i]
			symbol := symbols[rng.Intn(len(symbols))]
			hasOpen := false
			for _, p := range positions {
				if p.owner == r.owner && p.symbol == symbol {
					hasOpen = true
					break
				}
			}
			side := []string{"LONG", "SHORT"}[rng.Intn(2)]
			contracts := int64(rng.Intn(20) + 1)
			p, err := st.OpenPerpPositionFromReservation(ctx, r.id, symbol, side, contracts, 7_000_000, ref)
			if hasOpen {
				if err == nil {
					t.Fatalf("step %d: duplicate open position accepted", step)
				}
				continue
			}
			if err != nil {
				t.Fatalf("step %d: open position: %v", step, err)
			}
			positions = append(positions, position{id: p.ID, owner: r.owner, symbol: symbol, contracts: contracts, margin: r.amount})
			reservations = append(reservations[:i], reservations[i+1:]...)
		case 5:
			if len(positions) == 0 {
				continue
			}
			i := rng.Intn(len(positions))
			p := positions[i]
			delta := int64(rng.Intn(4_000_000)+1) * int64(1-2*rng.Intn(2))
			_, err := st.RealizePerpPnL(ctx, p.id, delta)
			switch {
			case delta > 0 && pool["liquidity"] < delta:
				if !errors.Is(err, ErrPoolInsufficient) {
					t.Fatalf("step %d: pnl over pool = %v", step, err)
				}
			case delta < 0 && p.margin < -delta:
				if !errors.Is(err, ErrMarginInsufficient) {
					t.Fatalf("step %d: pnl over margin = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: pnl: %v", step, err)
			default:
				positions[i].margin += delta
				pool["liquidity"] -= delta
			}
		case 6:
			if len(positions) == 0 {
				continue
			}
			i := rng.Intn(len(positions))
			p := positions[i]
			transfer := int64(rng.Intn(2_000_000)+1) * int64(1-2*rng.Intn(2))
			err := st.ApplyPerpFunding(ctx, p.id, int64(step)*1_000, int64(step)*1_000+500, 120_000, transfer, ref)
			switch {
			case transfer > 0 && pool["liquidity"] < transfer:
				if !errors.Is(err, ErrPoolInsufficient) {
					t.Fatalf("step %d: funding over pool = %v", step, err)
				}
			case transfer < 0 && p.margin < -transfer:
				if !errors.Is(err, ErrMarginInsufficient) {
					t.Fatalf("step %d: funding over margin = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: funding: %v", step, err)
			default:
				positions[i].margin += transfer
				pool["liquidity"] -= transfer
			}
		case 7:
			if len(positions) == 0 {
				continue
			}
			i := rng.Intn(len(positions))
			p := positions[i]
			closeContracts := int64(rng.Intn(int(p.contracts)) + 1)
			if closeContracts == p.contracts {
				if _, err := st.ReducePerpPosition(ctx, p.id, closeContracts, 0); err != nil {
					t.Fatalf("step %d: close: %v", step, err)
				}
				balance[p.owner] += p.margin
				positions = append(positions[:i], positions[i+1:]...)
			} else {
				ret := int64(0)
				if p.margin > 0 {
					ret = int64(rng.Intn(int(p.margin) + 1))
				}
				if _, err := st.ReducePerpPosition(ctx, p.id, closeContracts, ret); err != nil {
					t.Fatalf("step %d: reduce: %v", step, err)
				}
				balance[p.owner] += ret
				positions[i].contracts -= closeContracts
				positions[i].margin -= ret
			}
		case 8:
			_, err := st.QueueWithdrawal(ctx, owner, amount, "", "material")
			switch {
			case balance[owner] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: withdraw underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: withdraw: %v", step, err)
			default:
				balance[owner] -= amount
				withdrawn += amount
			}
		default:
			_ = op
		}
		check(step)
	}
	checkDenseEvents(t, st, ctx)

	for _, r := range reservations {
		if _, err := st.ReleasePerpMarginReservation(ctx, r.id); err != nil {
			t.Fatalf("final release: %v", err)
		}
		balance[r.owner] += r.amount
	}
	reservations = nil
	for _, p := range positions {
		if _, err := st.ReducePerpPosition(ctx, p.id, p.contracts, 0); err != nil {
			t.Fatalf("final close: %v", err)
		}
		balance[p.owner] += p.margin
	}
	positions = nil
	check(-1)
	checkDenseEvents(t, st, ctx)
}
