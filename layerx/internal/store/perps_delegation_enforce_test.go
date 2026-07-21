package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// These tests drive ExecutePerpIntent's transactional delegation enforcement
// directly against real Postgres: grant presence, expiry transition, tier
// downgrade, market/type/notional/leverage bounds, daily windows, reduce-only
// availability, and the revocation race.

func newDelegationStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping delegation enforcement test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	st, err := New(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.SetPerpMarketMode(ctx, "BTC", "ACTIVE", "", "did:op", true); err != nil {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("activate BTC: %v", err)
		}
		t.Skip("perp markets not synced; run a suite that seeds them first")
	}
	return st, ctx
}

func delegDID(label string) string {
	return fmt.Sprintf("did:matrix:%s-%d:0123456789abcdef", label, time.Now().UnixNano())
}

// testTierRank mirrors the engine's tier ordering without importing the
// engine package (which would cycle).
func testTierRank(tier string) int {
	switch tier {
	case "basic":
		return 1
	case "pro":
		return 2
	case "elite":
		return 3
	default:
		return 0
	}
}

// delegatedOpenIntent builds a fully-populated delegated open plan: 2
// contracts at 100.00, 20 USDX notional, 4 USDX initial margin, 0.01 USDX fee.
func delegatedOpenIntent(owner, delegate, key string, check *PerpIntentDelegation) PerpIntent {
	order := PerpOrder{
		OwnerDID: owner, ActingDID: delegate, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "MARKET", Contracts: 2, TimeInForce: "IOC",
		IdempotencyKey: key, SnapshotID: "deleg-snap", OrderbookSeq: 1, StatsSeq: 1,
		SourceTimestampMs: 1,
	}
	return PerpIntent{
		Operation: "perps.order", RequestHash: "h-" + key, Order: order,
		AllowRiskIncrease: true, AllowedModes: []string{"ACTIVE", "CANARY"},
		Delegation:  check,
		FinalStatus: "FILLED",
		Open: &PerpIntentOpen{
			Fill: PerpIntentFill{Contracts: 2, PriceCents: 10_000, NotionalMicro: 20_000_000, FeeMicro: 10_000},
			Side: "LONG", InitialMarginMicro: 4_000_000, NewEntryPriceCents: 10_000,
		},
	}
}

func baseCheck(delegate string, riskIncrease bool) *PerpIntentDelegation {
	return &PerpIntentDelegation{
		DelegateDID:                    delegate,
		AssertedTierRank:               testTierRank("pro"),
		GrantTierRank:                  testTierRank,
		OrderNotionalMicro:             20_000_000,
		ProjectedPositionNotionalMicro: 20_000_000,
		ProjectedMarginMicro:           4_000_000,
		DayStartUTC:                    time.Now().UTC().Truncate(24 * time.Hour),
		RiskIncrease:                   riskIncrease,
	}
}

func TestPerpDelegationEnforcement(t *testing.T) {
	st, ctx := newDelegationStore(t)
	owner := delegDID("d-owner")
	delegate := delegDID("d-agent")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 500_000_000); err != nil {
		t.Fatal(err)
	}

	// 1. No delegation at all: DELEGATION_REQUIRED.
	_, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "no-grant-"+owner, baseCheck(delegate, true)))
	if !errors.Is(err, ErrDelegationRequired) {
		t.Fatalf("no grant = %v, want ErrDelegationRequired", err)
	}

	// 2. Create the bounded grant.
	grant, err := st.CreatePerpDelegation(ctx, PerpDelegation{
		OwnerDID: owner, DelegateDID: delegate, MembershipTier: "pro",
		AllowedMarkets: []string{"BTC"}, AllowedOrderTypes: []string{"MARKET"},
		MaxOrderNotionalMicro: 50_000_000, MaxPositionNotionalMicro: 100_000_000,
		MaxLeverageX: 5, MaxDailyNotionalMicro: 30_000_000, MaxDailyRealizedLossMicro: 10_000_000,
		GrantSignature: "sig", PublicKey: "pk", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Membership: unresolved (rank 0) and downgraded (basic < pro) both fail.
	c := baseCheck(delegate, true)
	c.AssertedTierRank = 0
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "tier0-"+owner, c)); !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("unresolved tier = %v, want ErrMembershipRequired", err)
	}
	c = baseCheck(delegate, true)
	c.AssertedTierRank = testTierRank("basic")
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "tier1-"+owner, c)); !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("downgraded tier = %v, want ErrMembershipRequired", err)
	}

	// 4. Bounds: market, order type, order notional, position notional, leverage.
	badMarket := delegatedOpenIntent(owner, delegate, "mkt-"+owner, baseCheck(delegate, true))
	badMarket.Order.MarketSymbol = "ETH"
	if _, err := st.SetPerpMarketMode(ctx, "ETH", "ACTIVE", "", "did:op", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExecutePerpIntent(ctx, badMarket); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("market bound = %v, want ErrDelegationLimit", err)
	}
	badType := delegatedOpenIntent(owner, delegate, "typ-"+owner, baseCheck(delegate, true))
	badType.Order.OrderType = "LIMIT"
	badType.Order.LimitPriceCents = 10_000
	if _, err := st.ExecutePerpIntent(ctx, badType); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("type bound = %v, want ErrDelegationLimit", err)
	}
	c = baseCheck(delegate, true)
	c.OrderNotionalMicro = 60_000_000
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "onot-"+owner, c)); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("order notional bound = %v, want ErrDelegationLimit", err)
	}
	c = baseCheck(delegate, true)
	c.ProjectedPositionNotionalMicro = 150_000_000
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "pnot-"+owner, c)); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("position notional bound = %v, want ErrDelegationLimit", err)
	}
	c = baseCheck(delegate, true)
	c.ProjectedPositionNotionalMicro = 90_000_000
	c.ProjectedMarginMicro = 10_000_000
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "lev-"+owner, c)); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("leverage bound = %v, want ErrDelegationLimit", err)
	}

	// 5. A conforming delegated open executes and audits both DIDs.
	res, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "good-"+owner, baseCheck(delegate, true)))
	if err != nil {
		t.Fatalf("conforming delegated open: %v", err)
	}
	fill, err := st.GetPerpFill(ctx, res.FillID)
	if err != nil || fill.OwnerDID != owner || fill.ActingDID != delegate {
		t.Fatalf("fill audit = %+v %v", fill, err)
	}

	// 6. Daily notional window: 20 already filled today + 20 more > 30 cap.
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "daily-"+owner, baseCheck(delegate, true))); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("daily notional = %v, want ErrDelegationLimit", err)
	}

	// 7. Reduce-only needs only the live grant — no bounds.
	pos, err := st.GetOpenPerpPosition(ctx, owner, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	reduceIntent := PerpIntent{
		Operation: "perps.order", RequestHash: "h-red", Order: PerpOrder{
			OwnerDID: owner, ActingDID: delegate, MarketSymbol: "BTC", Side: "SELL",
			OrderType: "MARKET", Contracts: 1, TimeInForce: "IOC", ReduceOnly: true,
			IdempotencyKey: "red-" + owner, SnapshotID: "deleg-snap", OrderbookSeq: 2, StatsSeq: 2,
			SourceTimestampMs: 2,
		},
		AllowedModes:            []string{"ACTIVE", "CANARY", "REDUCE_ONLY"},
		Delegation:              baseCheck(delegate, false),
		ExpectPositionID:        pos.ID,
		ExpectPositionContracts: pos.Contracts,
		FinalStatus:             "FILLED",
		Reduce: &PerpIntentReduce{
			Fill:           PerpIntentFill{Contracts: 1, PriceCents: 10_000, NotionalMicro: 10_000_000, FeeMicro: 5_000},
			CloseContracts: 1, RealizedPnLMicro: 0, MarginReturnMicro: 1_000_000,
		},
	}
	if _, err := st.ExecutePerpIntent(ctx, reduceIntent); err != nil {
		t.Fatalf("delegated reduce with live grant: %v", err)
	}

	// 8. Revocation: new delegated action (even reduce) requires owner auth.
	if _, _, err := st.RevokePerpDelegation(ctx, grant.ID, owner); err != nil {
		t.Fatal(err)
	}
	reduceIntent.Order.IdempotencyKey = "red2-" + owner
	reduceIntent.RequestHash = "h-red2"
	pos, err = st.GetOpenPerpPosition(ctx, owner, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	reduceIntent.ExpectPositionID = pos.ID
	reduceIntent.ExpectPositionContracts = pos.Contracts
	if _, err := st.ExecutePerpIntent(ctx, reduceIntent); !errors.Is(err, ErrDelegationRequired) {
		t.Fatalf("post-revoke delegated reduce = %v, want ErrDelegationRequired", err)
	}

	// 9. Expiry: a short grant transitions to EXPIRED inside the check.
	short, err := st.CreatePerpDelegation(ctx, PerpDelegation{
		OwnerDID: owner, DelegateDID: delegate, MembershipTier: "pro",
		AllowedMarkets: []string{"BTC"}, AllowedOrderTypes: []string{"MARKET"},
		MaxOrderNotionalMicro: 50_000_000, MaxPositionNotionalMicro: 100_000_000,
		MaxLeverageX: 5, MaxDailyNotionalMicro: 300_000_000, MaxDailyRealizedLossMicro: 10_000_000,
		GrantSignature: "sig", PublicKey: "pk", ExpiresAt: time.Now().Add(1200 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "exp-"+owner, baseCheck(delegate, true))); !errors.Is(err, ErrDelegationRequired) {
		t.Fatalf("expired grant = %v, want ErrDelegationRequired", err)
	}
	if n, err := st.ExpirePerpDelegations(ctx); err != nil || n < 1 {
		t.Fatalf("expiry sweep = %d %v, want >= 1", n, err)
	}
	expired, err := st.GetPerpDelegation(ctx, short.ID)
	if err != nil || expired.Status != "EXPIRED" {
		t.Fatalf("grant must transition to EXPIRED, got %+v %v", expired, err)
	}
}

// TestPerpDelegationRevocationRace runs revocation concurrently with delegated
// opens: every accepted open must have run under a live grant, and once the
// revoke commits every later attempt fails DELEGATION_REQUIRED with the
// delegate's resting orders cancelled and margin released.
func TestPerpDelegationRevocationRace(t *testing.T) {
	st, ctx := newDelegationStore(t)
	owner := delegDID("r-owner")
	delegate := delegDID("r-agent")
	if err := st.CreditDeposit(ctx, owner, "0xabc", "0xdep-"+owner, 1_000_000_000); err != nil {
		t.Fatal(err)
	}
	grant, err := st.CreatePerpDelegation(ctx, PerpDelegation{
		OwnerDID: owner, DelegateDID: delegate, MembershipTier: "pro",
		AllowedMarkets: []string{"BTC"}, AllowedOrderTypes: []string{"MARKET", "LIMIT"},
		MaxOrderNotionalMicro: 500_000_000, MaxPositionNotionalMicro: 500_000_000,
		MaxLeverageX: 5, MaxDailyNotionalMicro: 100_000_000_000, MaxDailyRealizedLossMicro: 100_000_000_000,
		GrantSignature: "sig", PublicKey: "pk", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A delegate resting order with held margin, so revocation has work to do.
	resting, err := st.InsertPerpOrder(ctx, PerpOrder{
		OwnerDID: owner, ActingDID: delegate, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "LIMIT", Contracts: 1, LimitPriceCents: 100, TimeInForce: "GTC",
		IdempotencyKey: "rest-" + owner, Status: "RESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReservePerpMargin(ctx, owner, resting.ID, 3_000_000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]error, 8)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = st.RevokePerpDelegation(ctx, grant.ID, owner)
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			check := baseCheck(delegate, true)
			check.ProjectedPositionNotionalMicro = 500_000_000 // stay inside bounds regardless of prior fills
			check.ProjectedMarginMicro = 100_000_000           // 5x leverage over the projected notional
			key := fmt.Sprintf("race-%d-%s", i, owner)
			_, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, key, check))
			results[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil && !errors.Is(err, ErrDelegationRequired) && !errors.Is(err, ErrPlanStale) {
			t.Fatalf("race intent %d = %v (only success, ErrDelegationRequired, or ErrPlanStale are lawful)", i, err)
		}
	}
	// After the revoke commits, every new delegated intent is rejected.
	if _, err := st.ExecutePerpIntent(ctx, delegatedOpenIntent(owner, delegate, "post-race-"+owner, baseCheck(delegate, true))); !errors.Is(err, ErrDelegationRequired) {
		t.Fatalf("post-revoke intent = %v, want ErrDelegationRequired", err)
	}
	// The resting order was cancelled with its reservation released.
	order, err := st.GetPerpOrder(ctx, resting.ID)
	if err != nil || order.Status != "CANCELLED" {
		t.Fatalf("resting delegate order = %+v %v, want CANCELLED", order, err)
	}
	if r, err := st.HeldReservationForOrder(ctx, resting.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reservation must be released, got %+v %v", r, err)
	}
	d, err := st.GetPerpDelegation(ctx, grant.ID)
	if err != nil || d.Status != "REVOKED" {
		t.Fatalf("grant = %+v %v, want REVOKED", d, err)
	}
}
