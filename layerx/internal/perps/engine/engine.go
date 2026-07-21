package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	pmath "github.com/paxlabs-inc/layerx/internal/perps/math"
	"github.com/paxlabs-inc/layerx/internal/perps/mode"
	"github.com/paxlabs-inc/layerx/internal/perps/pricing"
	"github.com/paxlabs-inc/layerx/internal/perps/risk"
	shadowref "github.com/paxlabs-inc/layerx/internal/perps/shadow"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
)

var (
	ErrMarketStale       = errors.New("engine: market data is not healthy")
	ErrRejected          = errors.New("engine: order rejected")
	ErrReduceExceeds     = errors.New("engine: reduce exceeds open position; position flips land with the API wave")
	ErrNoPosition        = errors.New("engine: no open position to reduce")
	ErrReduceOnlyNoRisk  = errors.New("engine: reduce-only order would increase risk")
	ErrStopPriceRequired = errors.New("engine: stop order requires a stop price")
	ErrCanaryDenied      = errors.New("engine: canary mode admits allowlisted staff owners, manual actors, and canary markets only")
	ErrRolloutDenied     = errors.New("engine: active rollout does not admit this owner or actor")
	ErrPositionLimit     = errors.New("engine: projected position notional exceeds the market position limit")
	ErrMarketOILimit     = errors.New("engine: projected internal open interest exceeds the effective market cap")
	ErrPoolCapacity      = errors.New("engine: funded pool capacity does not activate this market")
)

// canaryMarkets is the locked canary allowlist ([locked] canary_markets).
var canaryMarkets = map[string]bool{"BTC": true, "ETH": true}

// Entitlements resolves an owner's CURRENT membership tier. Resolver absence
// or failure fails closed: delegated risk increase is blocked while cancel
// and owner-authorized reduction stay available.
type Entitlements interface {
	Tier(ctx context.Context, ownerDID string) (string, error)
}

// StaticEntitlements is the internal config-backed resolver (owner DID ->
// tier). An external assertion service can replace it behind the same seam.
type StaticEntitlements map[string]string

func (s StaticEntitlements) Tier(_ context.Context, ownerDID string) (string, error) {
	return s[ownerDID], nil
}

// TierRank orders membership tiers for downgrade checks. Unknown/empty tiers
// rank 0 and therefore never satisfy a granted tier.
func TierRank(tier string) int {
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

// Feed is the market-data surface the engine needs; *crossverse.Manager
// satisfies it.
type Feed interface {
	Snapshot(symbol string) (crossverse.NormalizedSnapshot, error)
	RiskIncreaseAllowed(symbol string) (bool, error)
}

// RolloutAdmission enforces the durable Wave 10-12 traffic ramp for ACTIVE
// risk-increasing intents. Reduction and cancellation paths bypass it.
type RolloutAdmission interface {
	Admit(ctx context.Context, ownerDID, actingDID string) error
}

// Engine executes signed perps intents against the store's atomic transaction.
type Engine struct {
	Store         *store.Store
	Feed          Feed
	Modes         *mode.Registry
	Signer        *sig.Signer
	LiquidatorDID string
	// CanaryDIDs is the explicit staff owner allowlist admitted while a
	// market's effective mode is CANARY. Empty denies everyone.
	CanaryDIDs map[string]bool
	// Entitlements resolves owner membership tiers for delegated risk
	// increase. Nil fails closed (MEMBERSHIP_REQUIRED).
	Entitlements Entitlements
	// Rollout is required by production ACTIVE mode. layerxd always wires the
	// durable store-backed admission controller; nil preserves isolated engine
	// construction outside the daemon.
	Rollout RolloutAdmission
}

// delegationCheck builds the transactional delegation recheck for a delegated
// intent (nil when the owner acts for itself). The daily-window boundary is
// the owner's configured IANA timezone materialized as a UTC instant.
func (e *Engine) delegationCheck(ctx context.Context, owner, acting string, riskIncrease bool,
	orderNotional, projectedPosNotional, projectedMargin int64) (*store.PerpIntentDelegation, error) {

	if acting == "" || acting == owner {
		return nil, nil
	}
	tierRank := 0
	if riskIncrease && e.Entitlements != nil {
		tier, err := e.Entitlements.Tier(ctx, owner)
		if err != nil {
			// Resolver unavailability fails closed for risk increase.
			tierRank = 0
		} else {
			tierRank = TierRank(tier)
		}
	}
	tz, err := e.Store.GetPerpOwnerTimezone(ctx, owner)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).UTC()
	return &store.PerpIntentDelegation{
		DelegateDID:                    acting,
		AssertedTierRank:               tierRank,
		GrantTierRank:                  TierRank,
		OrderNotionalMicro:             orderNotional,
		ProjectedPositionNotionalMicro: projectedPosNotional,
		ProjectedMarginMicro:           projectedMargin,
		DayStartUTC:                    dayStart,
		RiskIncrease:                   riskIncrease,
	}, nil
}

// OrderRequest is one parsed, authenticated order intent.
type OrderRequest struct {
	OwnerDID        string
	ActingDID       string
	Symbol          string
	Side            string
	OrderType       string
	Contracts       int64
	LimitPriceCents int64
	StopPriceCents  int64
	TimeInForce     string
	ReduceOnly      bool
	ClientOrderID   string
	IdempotencyKey  string
	RequestHash     string
}

// Receipt is the sequencer-signed attestation of one executed intent.
type Receipt struct {
	ReceiptID          string
	OwnerDID           string
	ActingDID          string
	OrderID            string
	FillIDs            []string
	IdempotencyKey     string
	EventSeqLo         int64
	EventSeqHi         int64
	SnapshotID         string
	OrderbookSeq       int64
	SequencerSignature string
	SequencerPublicKey string
}

// Result is the engine's response for one intent.
type Result struct {
	Order      store.PerpOrder
	FillID     string
	PositionID string
	Replayed   bool
	Receipt    Receipt
}

const receiptDomain = "layerx.perps.receipt.v1"

// SignResult signs a sequencer receipt over an executed store result — the
// exported seam the API layer uses for non-order operations (cancel, margin,
// delegation) that bypass PlaceOrder.
func (e *Engine) SignResult(res store.PerpExecResult) Receipt {
	return e.signReceipt(res)
}

func (e *Engine) signReceipt(res store.PerpExecResult) Receipt {
	r := Receipt{
		OwnerDID: res.Order.OwnerDID, ActingDID: res.Order.ActingDID,
		OrderID: res.Order.ID, IdempotencyKey: res.Order.IdempotencyKey,
		EventSeqLo: res.EventSeqLo, EventSeqHi: res.EventSeqHi,
		SnapshotID: res.Order.SnapshotID, OrderbookSeq: res.Order.OrderbookSeq,
	}
	if res.FillID != "" {
		r.FillIDs = []string{res.FillID}
	}
	pre := receiptPreimage(r)
	sum := sha256.Sum256(pre)
	r.ReceiptID = hex.EncodeToString(sum[:16])
	if e.Signer != nil {
		r.SequencerSignature = e.Signer.Sign(pre)
		r.SequencerPublicKey = e.Signer.PublicHex()
	}
	return r
}

func receiptPreimage(r Receipt) []byte {
	var b []byte
	b = append(b, []byte(receiptDomain)...)
	appendStr := func(s string) {
		var u4 [4]byte
		binary.BigEndian.PutUint32(u4[:], uint32(len(s)))
		b = append(b, u4[:]...)
		b = append(b, s...)
	}
	appendInt := func(v int64) {
		var u8 [8]byte
		binary.BigEndian.PutUint64(u8[:], uint64(v))
		b = append(b, u8[:]...)
	}
	appendStr(r.OwnerDID)
	appendStr(r.ActingDID)
	appendStr(r.OrderID)
	appendInt(int64(len(r.FillIDs)))
	for _, id := range r.FillIDs {
		appendStr(id)
	}
	appendStr(r.IdempotencyKey)
	appendInt(r.EventSeqLo)
	appendInt(r.EventSeqHi)
	appendStr(r.SnapshotID)
	appendInt(r.OrderbookSeq)
	return b
}

func allowedModes(riskIncrease bool) []string {
	if riskIncrease {
		return []string{"CANARY", "ACTIVE"}
	}
	return []string{"CANARY", "ACTIVE", "REDUCE_ONLY"}
}

func sideSign(side string) int64 {
	if side == "LONG" {
		return risk.SideLong
	}
	return risk.SideShort
}

// fillableContracts walks the book counting depth executable within an
// optional limit-price constraint (0 = none).
func fillableContracts(levels []crossverse.Level, want, limitCents int64, side pricing.Side) int64 {
	var got int64
	for _, lv := range levels {
		if limitCents > 0 {
			if side == pricing.Buy && lv.PriceCents > limitCents {
				break
			}
			if side == pricing.Sell && lv.PriceCents < limitCents {
				break
			}
		}
		got += lv.Contracts
		if got >= want {
			return want
		}
	}
	return got
}

// PlaceOrder runs the locked transaction order: authenticate happens upstream;
// here the engine validates the request, gates on mode/feed health,
// calculates the quote server-side from the live snapshot, builds the integer
// plan, and executes it atomically with exactly-once idempotency. Stale-plan
// races recompute and retry.
func (e *Engine) PlaceOrder(ctx context.Context, req OrderRequest) (Result, error) {
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	mkt, err := market.Lookup(req.Symbol)
	if err != nil {
		return Result{}, err
	}
	if e.Feed == nil {
		return Result{}, ErrMarketStale
	}
	for attempt := 0; attempt < 3; attempt++ {
		res, err := e.placeOnce(ctx, req, mkt)
		if errors.Is(err, store.ErrPlanStale) {
			continue
		}
		return res, err
	}
	return Result{}, store.ErrPlanStale
}

func validateRequest(req OrderRequest) error {
	if req.Contracts <= 0 {
		return fmt.Errorf("engine: contracts must be positive")
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return fmt.Errorf("engine: side %q is invalid", req.Side)
	}
	switch req.TimeInForce {
	case "GTC", "IOC", "FOK":
	default:
		return fmt.Errorf("engine: time in force %q is invalid", req.TimeInForce)
	}
	switch req.OrderType {
	case "MARKET", "LIMIT":
	case "STOP_MARKET", "STOP_LIMIT":
		if req.StopPriceCents <= 0 {
			return ErrStopPriceRequired
		}
	case "TAKE_PROFIT", "STOP_LOSS":
		if req.StopPriceCents <= 0 {
			return ErrStopPriceRequired
		}
		if !req.ReduceOnly {
			return fmt.Errorf("engine: %s must be reduce-only", req.OrderType)
		}
	default:
		return fmt.Errorf("engine: order type %q is invalid", req.OrderType)
	}
	if req.OrderType == "LIMIT" || req.OrderType == "STOP_LIMIT" {
		if req.LimitPriceCents <= 0 {
			return fmt.Errorf("engine: limit order requires a limit price")
		}
	}
	if req.IdempotencyKey == "" || req.OwnerDID == "" {
		return fmt.Errorf("engine: owner and idempotency key are required")
	}
	return nil
}

func (e *Engine) placeOnce(ctx context.Context, req OrderRequest, mkt market.Market) (Result, error) {
	snap, err := e.Feed.Snapshot(req.Symbol)
	if err != nil {
		return Result{}, err
	}
	if snap.Health != crossverse.HealthHealthy {
		return Result{}, ErrMarketStale
	}
	ref := store.PerpSnapshotRef{
		SnapshotID: snap.SnapshotID, OrderbookSeq: snap.OrderbookSeq,
		StatsSeq: snap.StatsSeq, SourceTimestampMs: snap.SourceTimestampMs,
	}
	acting := req.ActingDID
	if acting == "" {
		acting = req.OwnerDID
	}
	base := store.PerpOrder{
		OwnerDID: req.OwnerDID, ActingDID: acting, MarketSymbol: mkt.Symbol,
		Side: req.Side, OrderType: req.OrderType, Contracts: req.Contracts,
		LimitPriceCents: req.LimitPriceCents, StopPriceCents: req.StopPriceCents,
		TimeInForce: req.TimeInForce, ReduceOnly: req.ReduceOnly,
		ClientOrderID: req.ClientOrderID, IdempotencyKey: req.IdempotencyKey,
		SnapshotID: ref.SnapshotID, OrderbookSeq: ref.OrderbookSeq,
		StatsSeq: ref.StatsSeq, SourceTimestampMs: ref.SourceTimestampMs,
	}

	var position *store.PerpPosition
	if p, err := e.Store.GetOpenPerpPosition(ctx, req.OwnerDID, mkt.Symbol); err == nil {
		position = &p
	} else if !errors.Is(err, store.ErrNotFound) {
		return Result{}, err
	}

	reducing := position != nil && closesPosition(req.Side, position.Side)
	if req.ReduceOnly && !reducing {
		return Result{}, ErrReduceOnlyNoRisk
	}
	riskIncrease := !reducing
	if e.Modes != nil {
		eff := e.Modes.Effective(mkt.Symbol)
		if eff == mode.Shadow {
			// SHADOW records the intent terminally with zero economic writes.
			base.Status = "SHADOW_RECORDED"
			shadowIntent := store.PerpIntent{
				Operation:    "perps.order",
				RequestHash:  req.RequestHash,
				Order:        base,
				AllowedModes: []string{"SHADOW"},
				Shadow:       e.buildShadowObservation(mkt, snap, req, position),
			}
			if acting != req.OwnerDID {
				check, derr := e.delegationCheck(ctx, req.OwnerDID, acting, false, 0, 0, 0)
				if derr != nil {
					return Result{}, derr
				}
				shadowIntent.Delegation = check
			}
			res, err := e.Store.ExecutePerpIntent(ctx, shadowIntent)
			if err != nil {
				return Result{}, err
			}
			return e.wrap(res), nil
		}
		if eff == mode.Canary {
			if riskIncrease &&
				(!e.CanaryDIDs[req.OwnerDID] || acting != req.OwnerDID || !canaryMarkets[mkt.Symbol]) {
				return Result{}, ErrCanaryDenied
			}
			if riskIncrease && e.Rollout != nil {
				if err := e.Rollout.Admit(ctx, req.OwnerDID, acting); err != nil {
					return Result{}, err
				}
			}
		}
		if eff == mode.Active && riskIncrease && e.Rollout != nil {
			if err := e.Rollout.Admit(ctx, req.OwnerDID, acting); err != nil {
				return Result{}, err
			}
		}
		perms := eff.Permissions()
		if riskIncrease && !perms.Increase {
			return Result{}, store.ErrModeDenied
		}
		if !riskIncrease && !perms.Reduce {
			return Result{}, store.ErrModeDenied
		}
	}

	if req.OrderType == "STOP_MARKET" || req.OrderType == "STOP_LIMIT" ||
		req.OrderType == "TAKE_PROFIT" || req.OrderType == "STOP_LOSS" {
		return e.restOrder(ctx, req, mkt, base, riskIncrease, position)
	}

	if riskIncrease {
		allowed, err := e.Feed.RiskIncreaseAllowed(req.Symbol)
		if err != nil {
			return Result{}, err
		}
		if !allowed {
			return Result{}, ErrMarketStale
		}
	}

	levels := snap.Asks
	side := pricing.Buy
	if req.Side == "SELL" {
		levels = snap.Bids
		side = pricing.Sell
	}
	fillable := fillableContracts(levels, req.Contracts, req.LimitPriceCents, side)

	if fillable == 0 {
		switch req.TimeInForce {
		case "GTC":
			if req.OrderType == "LIMIT" {
				return e.restOrder(ctx, req, mkt, base, riskIncrease, position)
			}
			return e.terminalOrder(ctx, base, riskIncrease, "REJECTED")
		case "IOC":
			return e.terminalOrder(ctx, base, riskIncrease, "CANCELLED")
		default:
			return e.terminalOrder(ctx, base, riskIncrease, "REJECTED")
		}
	}
	if fillable < req.Contracts {
		switch req.TimeInForce {
		case "FOK":
			return e.terminalOrder(ctx, base, riskIncrease, "REJECTED")
		case "GTC":
			if req.OrderType == "LIMIT" {
				return e.restOrder(ctx, req, mkt, base, riskIncrease, position)
			}
		}
	}
	execContracts := fillable
	if reducing && execContracts > position.Contracts {
		return Result{}, ErrReduceExceeds
	}
	if reducing && req.Contracts > position.Contracts {
		return Result{}, ErrReduceExceeds
	}
	if riskIncrease {
		if err := e.checkIncreaseLimits(ctx, mkt, position, execContracts); err != nil {
			return Result{}, err
		}
	}

	fill, err := e.computeFill(mkt, snap, req.Side, execContracts, req.LimitPriceCents, position)
	if err != nil {
		if errors.Is(err, pricing.ErrInsufficientBookDepth) {
			return e.terminalOrder(ctx, base, riskIncrease, "REJECTED")
		}
		return Result{}, err
	}

	finalStatus := "FILLED"
	if execContracts < req.Contracts {
		finalStatus = "CANCELLED"
	}
	intent := store.PerpIntent{
		Operation:         "perps.order",
		RequestHash:       req.RequestHash,
		Order:             base,
		AllowRiskIncrease: riskIncrease,
		AllowedModes:      allowedModes(riskIncrease),
		FinalStatus:       finalStatus,
	}
	if reducing {
		intent.ExpectPositionID = position.ID
		intent.ExpectPositionContracts = position.Contracts
		reduce, err := e.computeReduce(mkt, position, fill, execContracts)
		if err != nil {
			return Result{}, err
		}
		intent.Reduce = reduce
	} else {
		open, err := e.computeOpen(mkt, position, req.Side, fill, execContracts)
		if err != nil {
			return Result{}, err
		}
		if position != nil {
			intent.ExpectPositionID = position.ID
			intent.ExpectPositionContracts = position.Contracts
		}
		intent.Open = open
	}
	if acting != req.OwnerDID {
		var projectedPos, projectedMargin int64
		if intent.Open != nil {
			projectedPos = fill.notionalMicro
			projectedMargin = intent.Open.InitialMarginMicro
			if position != nil {
				existing, nerr := risk.Notional(position.Contracts)
				if nerr != nil {
					return Result{}, nerr
				}
				projectedPos += existing
				projectedMargin += position.MarginMicro
			}
		}
		check, derr := e.delegationCheck(ctx, req.OwnerDID, acting, riskIncrease,
			fill.notionalMicro, projectedPos, projectedMargin)
		if derr != nil {
			return Result{}, derr
		}
		intent.Delegation = check
	}
	res, err := e.Store.ExecutePerpIntent(ctx, intent)
	if err != nil {
		return Result{}, err
	}
	return e.wrap(res), nil
}

func (e *Engine) buildShadowObservation(mkt market.Market, snap crossverse.NormalizedSnapshot,
	req OrderRequest, position *store.PerpPosition) *store.PerpShadowObservation {

	observation := &store.PerpShadowObservation{
		ExecutionToleranceCents: mkt.TickPriceUnits,
		FeedGapDetected: snap.SnapshotID == "" || snap.OrderbookSeq <= 0 ||
			snap.StatsSeq <= 0 || snap.SourceTimestampMs <= 0,
	}
	engineFill, engineErr := e.computeFill(mkt, snap, req.Side, req.Contracts, req.LimitPriceCents, position)
	if engineErr == nil {
		observation.EngineExecutionPriceCents = engineFill.priceCents
		observation.EngineFeeMicro = engineFill.feeMicro
		observation.EngineMarginMicro, engineErr = risk.InitialMargin(engineFill.notionalMicro, mkt.InitialMarginBps)
		if engineErr == nil {
			var maintenance, liquidationFee int64
			maintenance, engineErr = risk.MaintenanceMargin(engineFill.notionalMicro, mkt.MaintenanceMarginBps)
			if engineErr == nil {
				liquidationFee, engineErr = risk.LiquidationFee(engineFill.notionalMicro, mkt.LiquidationFeeBps)
			}
			if engineErr == nil {
				side := risk.SideLong
				if req.Side == "SELL" {
					side = risk.SideShort
				}
				observation.EngineLiquidationPriceCents, engineErr = risk.LiquidationPriceCents(
					side, engineFill.priceCents, engineFill.notionalMicro, maintenance, liquidationFee,
					0, 0, observation.EngineMarginMicro, mkt.TickPriceUnits)
			}
		}
		if engineErr == nil {
			var applied int64
			applied, engineErr = risk.AppliedFundingPpb(snap.EstimatedFundingPpb, 0)
			if engineErr == nil {
				intervalMs := int64(market.FundingIntervalSeconds) * 1_000
				observation.EngineFundingMicro, engineErr = risk.FundingTransfer(
					engineFill.notionalMicro, applied, intervalMs, intervalMs)
			}
		}
		if engineErr == nil && position != nil {
			var notional int64
			notional, engineErr = risk.Notional(position.Contracts)
			if engineErr == nil {
				observation.EnginePnLMicro, engineErr = risk.UnrealizedPnL(
					sideSign(position.Side), notional, snap.MarkPriceCents, position.EntryPriceCents)
			}
		}
	}
	if engineErr != nil {
		observation.EngineError = engineErr.Error()
		observation.EngineErrorCode = shadowErrorCode(engineErr)
	}

	var refPosition *shadowref.Position
	if position != nil {
		refPosition = &shadowref.Position{
			Side: position.Side, Contracts: position.Contracts, EntryPriceCents: position.EntryPriceCents,
		}
	}
	reference, referenceErr := shadowref.Calculate(
		mkt, snap, req.Side, req.Contracts, req.LimitPriceCents, refPosition)
	if referenceErr != nil {
		observation.ReferenceError = referenceErr.Error()
		observation.ReferenceErrorCode = shadowErrorCode(referenceErr)
	} else {
		observation.ReferenceExecutionPriceCents = reference.ExecutionPriceCents
		observation.ReferenceMarginMicro = reference.MarginMicro
		observation.ReferenceFeeMicro = reference.FeeMicro
		observation.ReferenceFundingMicro = reference.FundingMicro
		observation.ReferenceLiquidationPriceCents = reference.LiquidationPriceCents
		observation.ReferencePnLMicro = reference.PnLMicro
	}

	if (engineErr == nil) != (referenceErr == nil) {
		observation.MismatchFields = append(observation.MismatchFields, "outcome")
	}
	if engineErr != nil && referenceErr != nil &&
		observation.EngineErrorCode != observation.ReferenceErrorCode {
		observation.MismatchFields = append(observation.MismatchFields, "outcome")
	}
	if engineErr == nil && referenceErr == nil {
		delta := observation.EngineExecutionPriceCents - observation.ReferenceExecutionPriceCents
		if delta < 0 {
			delta = -delta
		}
		if delta > observation.ExecutionToleranceCents {
			observation.MismatchFields = append(observation.MismatchFields, "execution_price")
		}
		if observation.EngineMarginMicro != observation.ReferenceMarginMicro {
			observation.MismatchFields = append(observation.MismatchFields, "margin")
		}
		if observation.EngineFeeMicro != observation.ReferenceFeeMicro {
			observation.MismatchFields = append(observation.MismatchFields, "fee")
		}
		if observation.EngineFundingMicro != observation.ReferenceFundingMicro {
			observation.MismatchFields = append(observation.MismatchFields, "funding")
		}
		if observation.EngineLiquidationPriceCents != observation.ReferenceLiquidationPriceCents {
			observation.MismatchFields = append(observation.MismatchFields, "liquidation")
		}
		if observation.EnginePnLMicro != observation.ReferencePnLMicro {
			observation.MismatchFields = append(observation.MismatchFields, "pnl")
		}
	}
	return observation
}

func shadowErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, pricing.ErrInsufficientBookDepth) ||
		strings.Contains(err.Error(), "insufficient book depth") {
		return "INSUFFICIENT_BOOK"
	}
	if strings.Contains(err.Error(), "limit") {
		return "LIMIT"
	}
	if strings.Contains(err.Error(), "overflow") {
		return "MATH_OVERFLOW"
	}
	return "CALCULATION"
}

type computedFill struct {
	priceCents    int64
	notionalMicro int64
	feeMicro      int64
	impactBps     int64
}

// checkIncreaseLimits enforces the risk-increase capacity gates: per-owner
// position notional, funded pool activation, and the effective internal OI cap
// derived solely from committed LayerX positions.
func (e *Engine) checkIncreaseLimits(ctx context.Context, mkt market.Market,
	position *store.PerpPosition, addContracts int64) error {

	add, err := risk.Notional(addContracts)
	if err != nil {
		return err
	}
	projected := add
	if position != nil {
		existing, err := risk.Notional(position.Contracts)
		if err != nil {
			return err
		}
		projected, err = pmath.CheckedAdd(projected, existing)
		if err != nil {
			return err
		}
	}
	if projected > mkt.MaxPositionMicroUSDX {
		return ErrPositionLimit
	}
	pools, err := e.Store.PerpPoolCapital(ctx)
	if err != nil {
		return err
	}
	capacity, err := risk.ComputeCapacity(pools.LiquidityMicroUSDX, pools.InsuranceMicroUSDX, 0, 0, 0,
		mkt.MaxProtocolOIMicroUSDX, mkt.StressLossBps, mkt.LiquidationFeeBps)
	if err != nil {
		return err
	}
	if !capacity.ActivationAllowed || capacity.EffectiveOICapMicro <= 0 {
		return ErrPoolCapacity
	}
	oi, err := e.Store.PerpMarketOpenInterestMicro(ctx, mkt.Symbol)
	if err != nil {
		return err
	}
	total, err := pmath.CheckedAdd(oi, add)
	if err != nil {
		return err
	}
	if total > capacity.EffectiveOICapMicro {
		return ErrMarketOILimit
	}
	return nil
}

func (e *Engine) computeFill(mkt market.Market, snap crossverse.NormalizedSnapshot,
	orderSide string, contracts, limitCents int64, position *store.PerpPosition) (computedFill, error) {

	levels := snap.Asks
	side := pricing.Buy
	if orderSide == "SELL" {
		levels = snap.Bids
		side = pricing.Sell
	}
	vwap, err := pricing.BookVWAP(levels, contracts, side)
	if err != nil {
		return computedFill{}, err
	}
	notional, err := risk.Notional(contracts)
	if err != nil {
		return computedFill{}, err
	}
	projected := notional
	if orderSide == "SELL" {
		projected = -notional
	}
	if position != nil {
		existing, err := risk.Notional(position.Contracts)
		if err != nil {
			return computedFill{}, err
		}
		projected += sideSign(position.Side) * existing
	}
	u, err := pricing.UtilizationBps(projected, mkt.MaxProtocolOIMicroUSDX)
	if err != nil {
		return computedFill{}, err
	}
	skew, err := pricing.SkewImpactBps(mkt.MaxSkewImpactBps, u)
	if err != nil {
		return computedFill{}, err
	}
	impact, err := pricing.TotalImpactBps(mkt.BaseSpreadBps, skew)
	if err != nil {
		return computedFill{}, err
	}
	price, err := pricing.ExecutionPriceCents(vwap, impact, mkt.TickPriceUnits, side)
	if err != nil {
		return computedFill{}, err
	}
	if limitCents > 0 {
		if side == pricing.Buy && price > limitCents {
			return computedFill{}, pricing.ErrInsufficientBookDepth
		}
		if side == pricing.Sell && price < limitCents {
			return computedFill{}, pricing.ErrInsufficientBookDepth
		}
	}
	fee, err := risk.Fee(notional, mkt.TakerFeeBps)
	if err != nil {
		return computedFill{}, err
	}
	return computedFill{priceCents: price, notionalMicro: notional, feeMicro: fee, impactBps: impact}, nil
}

func (e *Engine) computeOpen(mkt market.Market, position *store.PerpPosition,
	orderSide string, fill computedFill, contracts int64) (*store.PerpIntentOpen, error) {

	im, err := risk.InitialMargin(fill.notionalMicro, mkt.InitialMarginBps)
	if err != nil {
		return nil, err
	}
	side := "LONG"
	if orderSide == "SELL" {
		side = "SHORT"
	}
	entry := fill.priceCents
	if position != nil {
		side = position.Side
		oldPx, err := pmath.CheckedMul(position.EntryPriceCents, position.Contracts)
		if err != nil {
			return nil, err
		}
		newPx, err := pmath.CheckedMul(fill.priceCents, contracts)
		if err != nil {
			return nil, err
		}
		total, err := pmath.CheckedAdd(oldPx, newPx)
		if err != nil {
			return nil, err
		}
		rounding := pmath.Ceil
		if position.Side == "SHORT" {
			rounding = pmath.Floor
		}
		entry, err = pmath.MulDiv(total, 1, position.Contracts+contracts, rounding)
		if err != nil {
			return nil, err
		}
	}
	return &store.PerpIntentOpen{
		Fill: store.PerpIntentFill{
			Contracts: contracts, PriceCents: fill.priceCents,
			NotionalMicro: fill.notionalMicro, FeeMicro: fill.feeMicro,
		},
		Side:               side,
		InitialMarginMicro: im,
		NewEntryPriceCents: entry,
	}, nil
}

func (e *Engine) computeReduce(mkt market.Market, position *store.PerpPosition,
	fill computedFill, contracts int64) (*store.PerpIntentReduce, error) {

	closeNotional, err := risk.Notional(contracts)
	if err != nil {
		return nil, err
	}
	pnl, err := risk.UnrealizedPnL(sideSign(position.Side), closeNotional, fill.priceCents, position.EntryPriceCents)
	if err != nil {
		return nil, err
	}
	fee, err := risk.Fee(closeNotional, mkt.TakerFeeBps)
	if err != nil {
		return nil, err
	}
	full := contracts == position.Contracts
	var marginReturn int64
	if !full {
		marginReturn, err = pmath.MulDiv(position.MarginMicro, contracts, position.Contracts, pmath.Floor)
		if err != nil {
			return nil, err
		}
	}
	return &store.PerpIntentReduce{
		Fill: store.PerpIntentFill{
			Contracts: contracts, PriceCents: fill.priceCents,
			NotionalMicro: closeNotional, FeeMicro: fee,
		},
		CloseContracts:    contracts,
		RealizedPnLMicro:  pnl,
		MarginReturnMicro: marginReturn,
		FullClose:         full,
	}, nil
}

func (e *Engine) restOrder(ctx context.Context, req OrderRequest, mkt market.Market,
	base store.PerpOrder, riskIncrease bool, position *store.PerpPosition) (Result, error) {

	var reserve, notional, im int64
	if riskIncrease {
		if err := e.checkIncreaseLimits(ctx, mkt, position, req.Contracts); err != nil {
			return Result{}, err
		}
		var err error
		notional, err = risk.Notional(req.Contracts)
		if err != nil {
			return Result{}, err
		}
		im, err = risk.InitialMargin(notional, mkt.InitialMarginBps)
		if err != nil {
			return Result{}, err
		}
		fee, err := risk.Fee(notional, mkt.TakerFeeBps)
		if err != nil {
			return Result{}, err
		}
		reserve, err = pmath.CheckedAdd(im, fee)
		if err != nil {
			return Result{}, err
		}
		allowed, err := e.Feed.RiskIncreaseAllowed(req.Symbol)
		if err != nil {
			return Result{}, err
		}
		if !allowed {
			return Result{}, ErrMarketStale
		}
	}
	base.Status = "RESTING"
	intent := store.PerpIntent{
		Operation:         "perps.order",
		RequestHash:       req.RequestHash,
		Order:             base,
		AllowRiskIncrease: riskIncrease,
		AllowedModes:      allowedModes(riskIncrease),
		Rest:              &store.PerpIntentRest{ReserveMicro: reserve},
	}
	if base.ActingDID != req.OwnerDID {
		projectedPos := notional
		projectedMargin := im
		if position != nil && riskIncrease {
			existing, nerr := risk.Notional(position.Contracts)
			if nerr != nil {
				return Result{}, nerr
			}
			projectedPos += existing
			projectedMargin += position.MarginMicro
		}
		check, derr := e.delegationCheck(ctx, req.OwnerDID, base.ActingDID, riskIncrease,
			notional, projectedPos, projectedMargin)
		if derr != nil {
			return Result{}, derr
		}
		intent.Delegation = check
	}
	res, err := e.Store.ExecutePerpIntent(ctx, intent)
	if err != nil {
		return Result{}, err
	}
	return e.wrap(res), nil
}

func (e *Engine) terminalOrder(ctx context.Context, base store.PerpOrder, riskIncrease bool, status string) (Result, error) {
	base.Status = status
	intent := store.PerpIntent{
		Operation:         "perps.order",
		RequestHash:       base.IdempotencyKey,
		Order:             base,
		AllowRiskIncrease: riskIncrease,
		AllowedModes:      allowedModes(false),
	}
	if base.ActingDID != base.OwnerDID {
		check, derr := e.delegationCheck(ctx, base.OwnerDID, base.ActingDID, false, 0, 0, 0)
		if derr != nil {
			return Result{}, derr
		}
		intent.Delegation = check
	}
	res, err := e.Store.ExecutePerpIntent(ctx, intent)
	if err != nil {
		return Result{}, err
	}
	return e.wrap(res), nil
}

func (e *Engine) wrap(res store.PerpExecResult) Result {
	return Result{
		Order: res.Order, FillID: res.FillID, PositionID: res.PositionID,
		Replayed: res.Replayed, Receipt: e.signReceipt(res),
	}
}

func closesPosition(orderSide, positionSide string) bool {
	return (orderSide == "SELL" && positionSide == "LONG") ||
		(orderSide == "BUY" && positionSide == "SHORT")
}

// QuoteRequest asks for a server-side quote without executing.
type QuoteRequest struct {
	OwnerDID        string
	Symbol          string
	Side            string
	Contracts       int64
	LimitPriceCents int64
	ReduceOnly      bool
}

// QuoteResult is the advisory quote: evidence, never authority — an order
// always recalculates from the then-live snapshot.
type QuoteResult struct {
	QuoteID               string
	Ref                   store.PerpSnapshotRef
	ExecutionPriceCents   int64
	PriceImpactBps        int64
	FeeMicro              int64
	RequiredMarginMicro   int64
	LiquidationPriceCents int64
	FundingEstimateMicro  int64
	ExpiresAtMs           int64
	MarketMode            string
}

// Quote computes the full advisory quote from the live healthy snapshot using
// the exact fixed-point execution formulas.
func (e *Engine) Quote(ctx context.Context, req QuoteRequest, nowMs int64) (QuoteResult, error) {
	if req.Contracts <= 0 {
		return QuoteResult{}, fmt.Errorf("engine: contracts must be positive")
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return QuoteResult{}, fmt.Errorf("engine: side %q is invalid", req.Side)
	}
	mkt, err := market.Lookup(req.Symbol)
	if err != nil {
		return QuoteResult{}, err
	}
	if e.Feed == nil {
		return QuoteResult{}, ErrMarketStale
	}
	snap, err := e.Feed.Snapshot(req.Symbol)
	if err != nil {
		return QuoteResult{}, err
	}
	if snap.Health != crossverse.HealthHealthy {
		return QuoteResult{}, ErrMarketStale
	}
	var position *store.PerpPosition
	if p, err := e.Store.GetOpenPerpPosition(ctx, req.OwnerDID, mkt.Symbol); err == nil {
		position = &p
	} else if !errors.Is(err, store.ErrNotFound) {
		return QuoteResult{}, err
	}
	fill, err := e.computeFill(mkt, snap, req.Side, req.Contracts, req.LimitPriceCents, position)
	if err != nil {
		return QuoteResult{}, err
	}
	im, err := risk.InitialMargin(fill.notionalMicro, mkt.InitialMarginBps)
	if err != nil {
		return QuoteResult{}, err
	}
	mm, err := risk.MaintenanceMargin(fill.notionalMicro, mkt.MaintenanceMarginBps)
	if err != nil {
		return QuoteResult{}, err
	}
	liqFee, err := risk.LiquidationFee(fill.notionalMicro, mkt.LiquidationFeeBps)
	if err != nil {
		return QuoteResult{}, err
	}
	side := risk.SideLong
	if req.Side == "SELL" {
		side = risk.SideShort
	}
	liqPrice, err := risk.LiquidationPriceCents(side, fill.priceCents, fill.notionalMicro,
		mm, liqFee, 0, 0, im, mkt.TickPriceUnits)
	if err != nil {
		return QuoteResult{}, err
	}
	appliedPpb, err := risk.AppliedFundingPpb(snap.EstimatedFundingPpb, 0)
	if err != nil {
		return QuoteResult{}, err
	}
	intervalMs := int64(market.FundingIntervalSeconds) * 1_000
	funding, err := risk.FundingTransfer(fill.notionalMicro, appliedPpb, intervalMs, intervalMs)
	if err != nil {
		return QuoteResult{}, err
	}
	marketMode := ""
	if e.Modes != nil {
		marketMode = string(e.Modes.Effective(mkt.Symbol))
	}
	res := QuoteResult{
		Ref: store.PerpSnapshotRef{
			SnapshotID: snap.SnapshotID, OrderbookSeq: snap.OrderbookSeq,
			StatsSeq: snap.StatsSeq, SourceTimestampMs: snap.SourceTimestampMs,
		},
		ExecutionPriceCents:   fill.priceCents,
		PriceImpactBps:        fill.impactBps,
		FeeMicro:              fill.feeMicro,
		RequiredMarginMicro:   im,
		LiquidationPriceCents: liqPrice,
		FundingEstimateMicro:  funding,
		ExpiresAtMs:           nowMs + market.QuoteTTLMs,
		MarketMode:            marketMode,
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d|%s|%d|%d",
		req.Symbol, req.Side, req.Contracts, req.LimitPriceCents,
		snap.SnapshotID, snap.OrderbookSeq, res.ExecutionPriceCents)))
	res.QuoteID = hex.EncodeToString(sum[:16])
	return res, nil
}

// marginRemoveBufferBps is the equity buffer over initial margin that must
// survive a margin removal.
const marginRemoveBufferBps = 200

// RemoveMarginFloor computes the minimum margin that must remain on a position
// after a REMOVE: post-remove equity must stay at or above initial margin plus
// a 200bps notional buffer at the live healthy mark.
func (e *Engine) RemoveMarginFloor(p store.PerpPosition) (int64, error) {
	mkt, err := market.Lookup(p.MarketSymbol)
	if err != nil {
		return 0, err
	}
	if e.Feed == nil {
		return 0, ErrMarketStale
	}
	snap, err := e.Feed.Snapshot(p.MarketSymbol)
	if err != nil {
		return 0, err
	}
	if snap.Health != crossverse.HealthHealthy {
		return 0, ErrMarketStale
	}
	notional, err := risk.Notional(p.Contracts)
	if err != nil {
		return 0, err
	}
	upnl, err := risk.UnrealizedPnL(sideSign(p.Side), notional, snap.MarkPriceCents, p.EntryPriceCents)
	if err != nil {
		return 0, err
	}
	im, err := risk.InitialMargin(notional, mkt.InitialMarginBps)
	if err != nil {
		return 0, err
	}
	buffer, err := pmath.MulDiv(notional, marginRemoveBufferBps, 10_000, pmath.Ceil)
	if err != nil {
		return 0, err
	}
	floor, err := pmath.CheckedAdd(im, buffer)
	if err != nil {
		return 0, err
	}
	floor, err = pmath.CheckedSub(floor, upnl)
	if err != nil {
		return 0, err
	}
	if floor < 0 {
		floor = 0
	}
	return floor, nil
}
