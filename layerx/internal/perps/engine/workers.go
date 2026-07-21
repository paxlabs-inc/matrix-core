package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	pmath "github.com/paxlabs-inc/layerx/internal/perps/math"
	"github.com/paxlabs-inc/layerx/internal/perps/pricing"
	"github.com/paxlabs-inc/layerx/internal/perps/risk"
	"github.com/paxlabs-inc/layerx/internal/store"
)

const fundingIntervalMs = int64(market.FundingIntervalSeconds) * 1_000

// RunFundingOnce settles the most recently COMPLETED funding interval for
// every open position. All Crossverse perpetual markets trade continuously.
// Exactly-once per (position, interval) is enforced by the store's unique
// funding constraint, so a restarted worker re-runs safely.
func (e *Engine) RunFundingOnce(ctx context.Context, nowMs int64) (int, error) {
	positions, err := e.Store.ListOpenPerpPositions(ctx, 0)
	if err != nil {
		return 0, err
	}
	intervalEnd := nowMs / fundingIntervalMs * fundingIntervalMs
	intervalStart := intervalEnd - fundingIntervalMs
	if intervalStart < 0 {
		return 0, nil
	}
	netBySymbol, err := e.netNotional(positions)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, p := range positions {
		mkt, err := market.Lookup(p.MarketSymbol)
		if err != nil {
			return settled, err
		}
		settledAlready, err := e.Store.HasPerpFundingEntry(ctx, p.ID, intervalStart)
		if err != nil {
			return settled, err
		}
		if settledAlready {
			continue
		}
		snap, err := e.Feed.Snapshot(p.MarketSymbol)
		if err != nil || snap.Health != crossverse.HealthHealthy {
			continue
		}
		capacity, err := e.marketCapacity(ctx, mkt)
		if err != nil {
			return settled, err
		}
		if capacity.EffectiveOICapMicro <= 0 {
			continue
		}
		skewPpb, err := risk.SkewFundingPpb(netBySymbol[p.MarketSymbol], capacity.EffectiveOICapMicro)
		if err != nil {
			return settled, err
		}
		appliedPpb, err := risk.AppliedFundingPpb(snap.EstimatedFundingPpb, skewPpb)
		if err != nil {
			return settled, err
		}
		notional, err := risk.Notional(p.Contracts)
		if err != nil {
			return settled, err
		}
		amount, err := risk.FundingTransfer(notional, appliedPpb, fundingIntervalMs, fundingIntervalMs)
		if err != nil {
			return settled, err
		}
		credit := -sideSign(p.Side) * amount
		ref := store.PerpSnapshotRef{
			SnapshotID: snap.SnapshotID, StatsSeq: snap.StatsSeq,
			OrderbookSeq: snap.OrderbookSeq, SourceTimestampMs: snap.SourceTimestampMs,
		}
		err = e.Store.ApplyPerpFunding(ctx, p.ID, intervalStart, intervalEnd, appliedPpb, credit, ref)
		switch {
		case errors.Is(err, store.ErrPoolInsufficient):
			if perr := e.Store.PauseAllPerpMarkets(ctx, e.LiquidatorDID, "perps.insolvency.funding"); perr != nil {
				return settled, perr
			}
			return settled, err
		case err != nil:
			return settled, err
		default:
			settled++
		}
	}
	return settled, nil
}

func (e *Engine) netNotional(positions []store.PerpPosition) (map[string]int64, error) {
	out := map[string]int64{}
	for _, p := range positions {
		n, err := risk.Notional(p.Contracts)
		if err != nil {
			return nil, err
		}
		out[p.MarketSymbol] += sideSign(p.Side) * n
	}
	return out, nil
}

func (e *Engine) marketCapacity(ctx context.Context, mkt market.Market) (risk.Capacity, error) {
	pools, err := e.Store.PerpPoolCapital(ctx)
	if err != nil {
		return risk.Capacity{}, err
	}
	return risk.ComputeCapacity(pools.LiquidityMicroUSDX, pools.InsuranceMicroUSDX, 0, 0, 0,
		mkt.MaxProtocolOIMicroUSDX, mkt.StressLossBps, mkt.LiquidationFeeBps)
}

const liquidationBufferBps = 200

// RunLiquidationOnce scans open positions against the live healthy mark and
// liquidates every eligible one: the smallest whole-contract close that leaves
// the remainder at or above initial margin plus a 200bps buffer after fees, or
// the full position if none exists. Claims are SKIP LOCKED so concurrent
// liquidators produce exactly one outcome; a deficit pauses every market.
func (e *Engine) RunLiquidationOnce(ctx context.Context) ([]store.PerpLiquidationResult, error) {
	positions, err := e.Store.ListOpenPerpPositions(ctx, 0)
	if err != nil {
		return nil, err
	}
	var results []store.PerpLiquidationResult
	for _, p := range positions {
		mkt, err := market.Lookup(p.MarketSymbol)
		if err != nil {
			return results, err
		}
		snap, err := e.Feed.Snapshot(p.MarketSymbol)
		if err != nil || snap.Health != crossverse.HealthHealthy {
			continue
		}
		notional, err := risk.Notional(p.Contracts)
		if err != nil {
			return results, err
		}
		upnl, err := risk.UnrealizedPnL(sideSign(p.Side), notional, snap.MarkPriceCents, p.EntryPriceCents)
		if err != nil {
			return results, err
		}
		equity, err := risk.Equity(p.MarginMicro, upnl, 0, 0)
		if err != nil {
			return results, err
		}
		mm, err := risk.MaintenanceMargin(notional, mkt.MaintenanceMarginBps)
		if err != nil {
			return results, err
		}
		estFee, err := risk.LiquidationFee(notional, mkt.LiquidationFeeBps)
		if err != nil {
			return results, err
		}
		eligible, err := risk.LiquidationEligible(equity, mm, estFee)
		if err != nil {
			return results, err
		}
		if !eligible {
			continue
		}
		res, err := e.liquidate(ctx, p, mkt, snap)
		if errors.Is(err, store.ErrOrderClaimed) || errors.Is(err, store.ErrPlanStale) {
			continue
		}
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

func (e *Engine) liquidate(ctx context.Context, p store.PerpPosition, mkt market.Market,
	snap crossverse.NormalizedSnapshot) (store.PerpLiquidationResult, error) {

	closeContracts, err := e.liquidationQuantity(p, mkt, snap)
	if err != nil {
		return store.PerpLiquidationResult{}, err
	}
	fill, pnl, fee, err := e.liquidationFill(p, mkt, snap, closeContracts)
	if err != nil {
		return store.PerpLiquidationResult{}, err
	}
	plan := store.PerpLiquidationPlan{
		PositionID: p.ID, ExpectContracts: p.Contracts,
		CloseContracts: closeContracts, FullClose: closeContracts == p.Contracts,
		Fill: store.PerpIntentFill{
			Contracts: closeContracts, PriceCents: fill, NotionalMicro: mustNotional(closeContracts),
			FeeMicro: fee, Liquidation: true,
		},
		RealizedPnLMicro: pnl,
		ActingDID:        e.LiquidatorDID,
		IdempotencyKey: fmt.Sprintf("perps.liq:%s:%s:%d:%d",
			p.ID, snap.SnapshotID, snap.OrderbookSeq, closeContracts),
		Ref: store.PerpSnapshotRef{
			SnapshotID: snap.SnapshotID, OrderbookSeq: snap.OrderbookSeq,
			StatsSeq: snap.StatsSeq, SourceTimestampMs: snap.SourceTimestampMs,
		},
	}
	return e.Store.LiquidatePerpPosition(ctx, plan)
}

func mustNotional(contracts int64) int64 {
	n, _ := risk.Notional(contracts)
	return n
}

func (e *Engine) liquidationFill(p store.PerpPosition, mkt market.Market,
	snap crossverse.NormalizedSnapshot, contracts int64) (priceCents, pnl, fee int64, err error) {

	levels := snap.Bids
	side := pricing.Sell
	if p.Side == "SHORT" {
		levels = snap.Asks
		side = pricing.Buy
	}
	vwap, err := pricing.BookVWAP(levels, contracts, side)
	if err != nil {
		return 0, 0, 0, err
	}
	closeNotional, err := risk.Notional(contracts)
	if err != nil {
		return 0, 0, 0, err
	}
	u, err := pricing.UtilizationBps(closeNotional, mkt.MaxProtocolOIMicroUSDX)
	if err != nil {
		return 0, 0, 0, err
	}
	skew, err := pricing.SkewImpactBps(mkt.MaxSkewImpactBps, u)
	if err != nil {
		return 0, 0, 0, err
	}
	impact, err := pricing.TotalImpactBps(mkt.BaseSpreadBps, skew)
	if err != nil {
		return 0, 0, 0, err
	}
	price, err := pricing.ExecutionPriceCents(vwap, impact, mkt.TickPriceUnits, side)
	if err != nil {
		return 0, 0, 0, err
	}
	pnl, err = risk.UnrealizedPnL(sideSign(p.Side), closeNotional, price, p.EntryPriceCents)
	if err != nil {
		return 0, 0, 0, err
	}
	fee, err = risk.LiquidationFee(closeNotional, mkt.LiquidationFeeBps)
	if err != nil {
		return 0, 0, 0, err
	}
	return price, pnl, fee, nil
}

func (e *Engine) liquidationQuantity(p store.PerpPosition, mkt market.Market,
	snap crossverse.NormalizedSnapshot) (int64, error) {

	for k := int64(1); k < p.Contracts; k++ {
		_, pnl, fee, err := e.liquidationFill(p, mkt, snap, k)
		if err != nil {
			if errors.Is(err, pricing.ErrInsufficientBookDepth) {
				break
			}
			return 0, err
		}
		remaining := p.Contracts - k
		remNotional, err := risk.Notional(remaining)
		if err != nil {
			return 0, err
		}
		remMargin := p.MarginMicro + pnl - fee
		if remMargin < 0 {
			continue
		}
		remUpnl, err := risk.UnrealizedPnL(sideSign(p.Side), remNotional, snap.MarkPriceCents, p.EntryPriceCents)
		if err != nil {
			return 0, err
		}
		remEquity, err := risk.Equity(remMargin, remUpnl, 0, 0)
		if err != nil {
			return 0, err
		}
		remIM, err := risk.InitialMargin(remNotional, mkt.InitialMarginBps)
		if err != nil {
			return 0, err
		}
		buffer, err := pmath.MulDiv(remNotional, liquidationBufferBps, 10_000, pmath.Ceil)
		if err != nil {
			return 0, err
		}
		if remEquity >= remIM+buffer {
			return k, nil
		}
	}
	return p.Contracts, nil
}

// RunTriggerOnce evaluates every RESTING order against the live healthy mark
// and executes triggered ones. The execution transaction claims the order row
// FOR UPDATE SKIP LOCKED, so concurrent trigger workers cannot double-fire.
func (e *Engine) RunTriggerOnce(ctx context.Context) (int, error) {
	orders, err := e.Store.ListRestingPerpOrders(ctx, "", 0)
	if err != nil {
		return 0, err
	}
	fired := 0
	for _, o := range orders {
		snap, err := e.Feed.Snapshot(o.MarketSymbol)
		if err != nil || snap.Health != crossverse.HealthHealthy {
			continue
		}
		if !triggered(o, snap.MarkPriceCents) {
			continue
		}
		err = e.executeTriggered(ctx, o, snap)
		switch {
		case errors.Is(err, store.ErrOrderClaimed), errors.Is(err, store.ErrPlanStale),
			errors.Is(err, ErrMarketStale), errors.Is(err, pricing.ErrInsufficientBookDepth):
			continue
		case errors.Is(err, store.ErrDelegationRequired), errors.Is(err, store.ErrDelegationLimit),
			errors.Is(err, store.ErrMembershipRequired):
			// The grant behind a delegated resting order ended or no longer
			// covers it: cancel the order and release its held margin instead
			// of retrying forever.
			if _, cerr := e.Store.CancelPerpOrder(ctx, o.OwnerDID, o.ID, e.LiquidatorDID,
				"perps.delegation.end:"+o.ID, "perps.delegation.end:"+o.ID, "delegation.ended"); cerr != nil &&
				!errors.Is(cerr, store.ErrOrderTerminal) {
				return fired, cerr
			}
			continue
		case err != nil:
			return fired, err
		default:
			fired++
		}
	}
	return fired, nil
}

func triggered(o store.PerpOrder, markCents int64) bool {
	switch o.OrderType {
	case "LIMIT":
		return true
	case "STOP_MARKET", "STOP_LIMIT":
		if o.Side == "BUY" {
			return markCents >= o.StopPriceCents
		}
		return markCents <= o.StopPriceCents
	case "TAKE_PROFIT":
		if o.Side == "SELL" {
			return markCents >= o.StopPriceCents
		}
		return markCents <= o.StopPriceCents
	case "STOP_LOSS":
		if o.Side == "SELL" {
			return markCents <= o.StopPriceCents
		}
		return markCents >= o.StopPriceCents
	default:
		return false
	}
}

func (e *Engine) executeTriggered(ctx context.Context, o store.PerpOrder, snap crossverse.NormalizedSnapshot) error {
	mkt, err := market.Lookup(o.MarketSymbol)
	if err != nil {
		return err
	}
	limit := int64(0)
	if o.OrderType == "LIMIT" || o.OrderType == "STOP_LIMIT" {
		limit = o.LimitPriceCents
	}
	var position *store.PerpPosition
	if p, err := e.Store.GetOpenPerpPosition(ctx, o.OwnerDID, o.MarketSymbol); err == nil {
		position = &p
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	reducing := position != nil && closesPosition(o.Side, position.Side)
	if o.ReduceOnly && !reducing {
		return store.ErrPlanStale
	}
	riskIncrease := !reducing
	if riskIncrease {
		allowed, err := e.Feed.RiskIncreaseAllowed(o.MarketSymbol)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrMarketStale
		}
	}
	contracts := o.Contracts - o.FilledContracts
	if reducing && contracts > position.Contracts {
		contracts = position.Contracts
	}
	fill, err := e.computeFill(mkt, snap, o.Side, contracts, limit, position)
	if err != nil {
		return err
	}
	execOrder := o
	execOrder.SnapshotID = snap.SnapshotID
	execOrder.OrderbookSeq = snap.OrderbookSeq
	execOrder.StatsSeq = snap.StatsSeq
	execOrder.SourceTimestampMs = snap.SourceTimestampMs
	execOrder.IdempotencyKey = o.IdempotencyKey + ":exec"
	intent := store.PerpIntent{
		Operation:         "perps.trigger",
		RequestHash:       execOrder.IdempotencyKey,
		Order:             execOrder,
		AllowRiskIncrease: riskIncrease,
		AllowedModes:      allowedModes(riskIncrease),
		TriggeredOrderID:  o.ID,
		FinalStatus:       "FILLED",
	}
	if o.ActingDID != o.OwnerDID {
		var projectedPos, projectedMargin int64
		if riskIncrease {
			projectedPos = fill.notionalMicro
			im, imErr := risk.InitialMargin(fill.notionalMicro, mkt.InitialMarginBps)
			if imErr != nil {
				return imErr
			}
			projectedMargin = im
			if position != nil {
				existing, nerr := risk.Notional(position.Contracts)
				if nerr != nil {
					return nerr
				}
				projectedPos += existing
				projectedMargin += position.MarginMicro
			}
		}
		check, derr := e.delegationCheck(ctx, o.OwnerDID, o.ActingDID, riskIncrease,
			fill.notionalMicro, projectedPos, projectedMargin)
		if derr != nil {
			return derr
		}
		intent.Delegation = check
	}
	if reducing {
		intent.ExpectPositionID = position.ID
		intent.ExpectPositionContracts = position.Contracts
		reduce, err := e.computeReduce(mkt, position, fill, contracts)
		if err != nil {
			return err
		}
		intent.Reduce = reduce
	} else {
		open, err := e.computeOpen(mkt, position, o.Side, fill, contracts)
		if err != nil {
			return err
		}
		if position != nil {
			intent.ExpectPositionID = position.ID
			intent.ExpectPositionContracts = position.Contracts
		}
		if r, err := e.Store.HeldReservationForOrder(ctx, o.ID); err == nil {
			open.FromReservationID = r.ID
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		intent.Open = open
	}
	_, err = e.Store.ExecutePerpIntent(ctx, intent)
	return err
}

// ReconcileOnce audits the conservation identity and the journal's gap-free
// law; any breach pauses every market with a recorded cause and reports the
// breach as an error.
func (e *Engine) ReconcileOnce(ctx context.Context) error {
	if _, err := e.Store.ExpirePerpDelegations(ctx); err != nil {
		return err
	}
	buckets, err := e.Store.PerpConservationBuckets(ctx)
	if err != nil {
		return err
	}
	for name, v := range map[string]int64{
		"spendable": buckets.SpendableMicroUSDX, "holds": buckets.OpenHoldsMicroUSDX,
		"reservations": buckets.HeldReservationsMicroUSDX, "margin": buckets.OpenPositionMarginMicroUSDX,
		"liquidity": buckets.LiquidityCapitalMicroUSDX, "insurance": buckets.InsuranceCapitalMicroUSDX,
	} {
		if v < 0 {
			_ = e.Store.PauseAllPerpMarkets(ctx, e.LiquidatorDID, "perps.reconciliation."+name)
			return fmt.Errorf("engine: conservation bucket %s is negative: %d", name, v)
		}
	}
	rep, err := e.Store.CheckPerpEventSequences(ctx)
	if err != nil {
		return err
	}
	if rep.Total != rep.MaxSeq || rep.OwnerGaps != 0 {
		_ = e.Store.PauseAllPerpMarkets(ctx, e.LiquidatorDID, "perps.reconciliation.journal")
		return fmt.Errorf("engine: perp journal is gapped: rows=%d max=%d ownerGaps=%d",
			rep.Total, rep.MaxSeq, rep.OwnerGaps)
	}
	return nil
}
