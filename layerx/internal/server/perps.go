// Perps API (spec/layerx-perps wave 6): public market reads, authenticated
// DID-scoped account/order/position/margin/fill/delegation writes, server-side
// quotes, and the owner-private durable SSE event stream.
//
// Every write is authorized by the existing LayerX DID intent envelope
// (from_did, public_key, nonce, signature) over
// auth.IntentMessage("perps."+op, ...) — or by an X-LayerX-Agent principal
// token. Direct-signature retries follow the locked order: verify the
// signature WITHOUT consuming the nonce, look up (owner_did,idempotency_key),
// replay the stored result on a hash match even when the nonce was already
// consumed, conflict on a mismatch, and only a NEW key consumes the nonce.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/engine"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	"github.com/paxlabs-inc/layerx/internal/perps/mode"
	"github.com/paxlabs-inc/layerx/internal/perps/pricing"
	"github.com/paxlabs-inc/layerx/internal/perps/risk"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// PerpsFeed is the market-data surface the API reads; *crossverse.Manager
// satisfies it.
type PerpsFeed interface {
	Snapshot(symbol string) (crossverse.NormalizedSnapshot, error)
	RecentTrades(symbol string) ([]crossverse.Trade, error)
}

// PerpsDeps wires the perps surface into the Server. Engine nil means no
// write path is available (reads still work off the durable rows); Feed nil
// fails every live-data read closed.
type PerpsDeps struct {
	Engine *engine.Engine
	Feed   PerpsFeed
	Modes  *mode.Registry
}

// Stable perps error codes (design.body.md).
const (
	codePerpsDisabled      = "PERPS_DISABLED"
	codeMarketDisabled     = "MARKET_DISABLED"
	codeShadowOnly         = "SHADOW_ONLY"
	codeCanaryDenied       = "CANARY_DENIED"
	codeRolloutDenied      = "ROLLOUT_DENIED"
	codeReduceOnly         = "REDUCE_ONLY"
	codeMarketPaused       = "MARKET_PAUSED"
	codeMarketStale        = "MARKET_STALE"
	codeMarketDiverged     = "MARKET_DIVERGED"
	codeInsufficientBook   = "INSUFFICIENT_BOOK_DEPTH"
	codeInsufficientMargn  = "INSUFFICIENT_MARGIN"
	codePositionLimit      = "POSITION_LIMIT"
	codeMarketOILimit      = "MARKET_OI_LIMIT"
	codePoolCapacity       = "POOL_CAPACITY"
	codeIdemConflict       = "IDEMPOTENCY_CONFLICT"
	codeOrderTerminal      = "ORDER_TERMINAL"
	codeDelegationRequired = "DELEGATION_REQUIRED"
	codeDelegationLimit    = "DELEGATION_LIMIT"
	codeMembershipRequired = "MEMBERSHIP_REQUIRED"
)

// perpsNotifier wakes an owner's SSE streams after a commit touching their
// private events (post-commit publication; the durable journal is the truth).
type perpsNotifier struct {
	mu   sync.Mutex
	next int
	subs map[string]map[int]chan struct{}
}

func newPerpsNotifier() *perpsNotifier {
	return &perpsNotifier{subs: map[string]map[int]chan struct{}{}}
}

func (n *perpsNotifier) subscribe(owner string) (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.next
	n.next++
	ch := make(chan struct{}, 1)
	if n.subs[owner] == nil {
		n.subs[owner] = map[int]chan struct{}{}
	}
	n.subs[owner][id] = ch
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.subs[owner], id)
		if len(n.subs[owner]) == 0 {
			delete(n.subs, owner)
		}
	}
}

func (n *perpsNotifier) wake(owner string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.subs[owner] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func writePerpsFail(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	writeJSON(w, status, types.Fail(types.NewError(code, msg, retryable)))
}

// mountPerps registers the perps routes.
func (s *Server) mountPerps(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/perps/markets", s.handlePerpsMarkets)
	mux.HandleFunc("GET /v1/perps/markets/{symbol}", s.handlePerpsMarket)
	mux.HandleFunc("GET /v1/perps/trades/{symbol}", s.handlePerpsTrades)
	mux.HandleFunc("GET /v1/perps/orderbook/{symbol}", s.handlePerpsOrderbook)

	mux.HandleFunc("GET /v1/perps/account", s.handlePerpsAccount)
	mux.HandleFunc("GET /v1/perps/rollout", s.handlePerpsRollout)
	mux.HandleFunc("GET /v1/perps/orders", s.handlePerpsOrders)
	mux.HandleFunc("POST /v1/perps/orders", s.handlePerpsPlaceOrder)
	mux.HandleFunc("DELETE /v1/perps/orders/{id}", s.handlePerpsCancelOrder)
	mux.HandleFunc("GET /v1/perps/positions", s.handlePerpsPositions)
	mux.HandleFunc("POST /v1/perps/positions/{id}/close", s.handlePerpsClosePosition)
	mux.HandleFunc("POST /v1/perps/positions/{id}/margin", s.handlePerpsPositionMargin)
	mux.HandleFunc("GET /v1/perps/fills", s.handlePerpsFills)
	mux.HandleFunc("GET /v1/perps/stream", s.handlePerpsStream)
	mux.HandleFunc("POST /v1/perps/quote", s.handlePerpsQuote)
	mux.HandleFunc("POST /v1/perps/pools/{pool}/fund", s.handlePerpsFundPool)
	mux.HandleFunc("GET /v1/perps/delegations", s.handlePerpsDelegations)
	mux.HandleFunc("POST /v1/perps/delegations", s.handlePerpsCreateDelegation)
	mux.HandleFunc("DELETE /v1/perps/delegations/{id}", s.handlePerpsRevokeDelegation)
}

type perpRolloutView struct {
	Stage                   string `json:"stage"`
	TrafficPercent          int    `json:"traffic_percent"`
	Cohort                  int    `json:"cohort"`
	ActiveManualAdmitted    bool   `json:"active_manual_admitted"`
	ActiveDelegatedAdmitted bool   `json:"active_delegated_admitted"`
	AgentsEnabled           bool   `json:"agents_enabled"`
	LegacyCutoverBlock      int64  `json:"legacy_cutover_block,omitempty"`
	LegacyCloseOnly         bool   `json:"legacy_close_only"`
	DiamondWritesRetired    bool   `json:"diamond_writes_retired"`
}

func (s *Server) handlePerpsRollout(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	state, err := s.store.GetPerpRolloutState(r.Context())
	if err != nil {
		s.log.Error("perps rollout read failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read perps rollout")
		return
	}
	view := perpRolloutView{
		Stage: state.Stage, TrafficPercent: state.TrafficPercent,
		Cohort: engine.RolloutCohort(owner), AgentsEnabled: state.AgentsEnabled,
		LegacyCutoverBlock: state.LegacyCutoverBlock, LegacyCloseOnly: state.LegacyCloseOnly,
		DiamondWritesRetired: state.DiamondWritesRetired,
	}
	if s.perps != nil && s.perps.Engine != nil && s.perps.Engine.Rollout != nil {
		view.ActiveManualAdmitted = s.perps.Engine.Rollout.Admit(r.Context(), owner, owner) == nil
		view.ActiveDelegatedAdmitted = s.perps.Engine.Rollout.Admit(
			r.Context(), owner, "did:matrix:rollout-eligibility-probe") == nil
	}
	writeJSON(w, http.StatusOK, types.OK(view))
}

type perpFundPoolBody struct {
	FromDID        string `json:"from_did"`
	PublicKey      string `json:"public_key"`
	Nonce          string `json:"nonce"`
	Signature      string `json:"signature"`
	IdempotencyKey string `json:"idempotency_key"`
	AmountUSDX     string `json:"amount_usdx"`
}

// handlePerpsFundPool accepts ordinary owner-authorized USDX into liquidity or
// insurance capital. It is intentionally not an operator/admin credit path.
func (s *Server) handlePerpsFundPool(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writePerpsFail(w, http.StatusServiceUnavailable, codePerpsDisabled,
			"perps pool funding is unavailable", false)
		return
	}
	pool := strings.TrimSpace(r.PathValue("pool"))
	if pool != "liquidity" && pool != "insurance" {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "pool must be liquidity or insurance")
		return
	}
	var b perpFundPoolBody
	if !decodePerpsBody(w, r, &b) {
		return
	}
	amount, err := types.ParseUSDX(b.AmountUSDX)
	if err != nil || amount <= 0 {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "amount_usdx must be a positive USDX decimal")
		return
	}
	canonicalAmount := types.FormatUSDX(amount)
	if b.IdempotencyKey == "" {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "idempotency_key is required")
		return
	}
	fields := []string{pool, canonicalAmount, b.IdempotencyKey}
	requestHash := perpsRequestHash("pool.fund", fields)
	var fromDID string
	if strings.TrimSpace(r.Header.Get("X-LayerX-Agent")) != "" {
		claims, ok := s.principal(w, r)
		if !ok {
			return
		}
		fromDID = claims.DID
	} else {
		if _, err := auth.ParseDID(b.FromDID); err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "from_did must be a valid did:matrix")
			return
		}
		preimage := auth.IntentMessage("perps.pool.fund", b.FromDID, b.Nonce, fields...)
		if err := auth.VerifyIntentSignature(b.FromDID, b.PublicKey, b.Signature, preimage); err != nil {
			writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "intent signature: "+err.Error())
			return
		}
		storedHash, _, found, err := s.store.LookupPerpPoolFundingIntent(
			r.Context(), b.FromDID, b.IdempotencyKey)
		if err != nil {
			s.log.Error("pool funding idempotency lookup failed", "error", err.Error())
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not check idempotency")
			return
		}
		if found {
			if storedHash != requestHash {
				writePerpsFail(w, http.StatusConflict, codeIdemConflict,
					"idempotency key was used for a different request", false)
				return
			}
		} else if !s.challenges.Consume(b.Nonce, b.FromDID) {
			writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized,
				"unknown, expired, or already-used nonce")
			return
		}
		fromDID = b.FromDID
	}
	if s.perps == nil || s.perps.Engine == nil || !s.perps.Engine.CanaryDIDs[fromDID] {
		writePerpsFail(w, http.StatusForbidden, codeCanaryDenied,
			"perps pool funding is restricted to configured owner staff DIDs", false)
		return
	}
	receipt, replayed, err := s.ledger.FundPerpPool(
		r.Context(), fromDID, pool, amount, b.IdempotencyKey, requestHash)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrIdempotencyConflict):
			writePerpsFail(w, http.StatusConflict, codeIdemConflict,
				"idempotency key was used for a different request", false)
		case errors.Is(err, store.ErrInsufficientFunds), errors.Is(err, store.ErrNotFound):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "insufficient escrow-backed balance")
		default:
			s.log.Error("perps pool funding failed", "pool", pool, "error", err.Error())
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not fund perps pool")
		}
		return
	}
	if !replayed && receipt.Tier == types.TierMaterial && s.settler != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := s.settler.SettleNow(ctx); err != nil {
				s.log.Error("perps pool funding settlement failed", "seq", receipt.Seq, "error", err.Error())
			}
		}()
	}
	if !replayed {
		s.publishTransfer(receipt, fromDID, store.PerpPoolDID(pool))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"receipt": receipt, "replayed": replayed,
	}))
}

// ─── views ──────────────────────────────────────────────────────────────────

type perpSnapshotRefView struct {
	SnapshotID        string `json:"snapshot_id"`
	OrderbookSeq      int64  `json:"orderbook_seq"`
	StatsSeq          int64  `json:"stats_seq"`
	SourceTimestampMs int64  `json:"source_timestamp_ms"`
}

func toRefView(r store.PerpSnapshotRef) perpSnapshotRefView {
	return perpSnapshotRefView{
		SnapshotID: r.SnapshotID, OrderbookSeq: r.OrderbookSeq,
		StatsSeq: r.StatsSeq, SourceTimestampMs: r.SourceTimestampMs,
	}
}

type perpMarketView struct {
	Symbol                 string               `json:"symbol"`
	Class                  string               `json:"class"`
	TickPriceUnits         int64                `json:"tick_price_units"`
	MinOrderContracts      int64                `json:"min_order_contracts"`
	MinPositionContracts   int64                `json:"min_position_contracts"`
	InitialMarginBps       int64                `json:"initial_margin_bps"`
	MaintenanceMarginBps   int64                `json:"maintenance_margin_bps"`
	MaxLeverageX           int64                `json:"max_leverage_x"`
	MaxPositionMicroUSDX   int64                `json:"max_position_micro_usdx"`
	MaxProtocolOIMicroUSDX int64                `json:"max_protocol_oi_micro_usdx"`
	MakerFeeBps            int64                `json:"maker_fee_bps"`
	TakerFeeBps            int64                `json:"taker_fee_bps"`
	LiquidationFeeBps      int64                `json:"liquidation_fee_bps"`
	BaseSpreadBps          int64                `json:"base_spread_bps"`
	MaxSkewImpactBps       int64                `json:"max_skew_impact_bps"`
	DivergenceLimitBps     int64                `json:"divergence_limit_bps"`
	StressLossBps          int64                `json:"stress_loss_bps"`
	Session                string               `json:"session"`
	Mode                   string               `json:"mode"`
	EffectiveMode          string               `json:"effective_mode"`
	PausedCause            string               `json:"paused_cause,omitempty"`
	Health                 string               `json:"health"`
	MarkPriceCents         int64                `json:"mark_price_cents"`
	IndexPriceCents        int64                `json:"index_price_cents"`
	BestBidCents           int64                `json:"best_bid_cents"`
	BestAskCents           int64                `json:"best_ask_cents"`
	SourceTimestampMs      int64                `json:"source_timestamp_ms"`
	SnapshotRef            *perpSnapshotRefView `json:"snapshot_ref,omitempty"`
}

func (s *Server) toMarketView(row store.PerpMarketRow) perpMarketView {
	effective := s.perpsEffective(row.Symbol)
	if durable, err := mode.Parse(row.Mode); err == nil {
		effective = mode.MoreRestrictive(effective, durable)
	}
	v := perpMarketView{
		Symbol: row.Symbol, Class: row.Class, TickPriceUnits: row.TickPriceUnits,
		MinOrderContracts: row.MinOrderContracts, MinPositionContracts: row.MinPositionContracts,
		InitialMarginBps: row.InitialMarginBps, MaintenanceMarginBps: row.MaintenanceMarginBps,
		MaxLeverageX: row.MaxLeverageX, MaxPositionMicroUSDX: row.MaxPositionMicroUSDX,
		MaxProtocolOIMicroUSDX: row.MaxProtocolOIMicroUSDX, MakerFeeBps: row.MakerFeeBps,
		TakerFeeBps: row.TakerFeeBps, LiquidationFeeBps: row.LiquidationFeeBps,
		BaseSpreadBps: row.BaseSpreadBps, MaxSkewImpactBps: row.MaxSkewImpactBps,
		DivergenceLimitBps: row.DivergenceLimitBps, StressLossBps: row.StressLossBps,
		Session: string(row.Session), Mode: row.Mode, PausedCause: row.PausedCause,
		EffectiveMode: string(effective),
		Health:        string(crossverse.HealthStopped),
	}
	if s.perps != nil && s.perps.Feed != nil {
		if snap, err := s.perps.Feed.Snapshot(row.Symbol); err == nil {
			v.Health = string(snap.Health)
			v.MarkPriceCents = snap.MarkPriceCents
			v.IndexPriceCents = snap.IndexPriceCents
			v.BestBidCents = snap.BestBidCents
			v.BestAskCents = snap.BestAskCents
			v.SourceTimestampMs = snap.SourceTimestampMs
			ref := toRefView(store.PerpSnapshotRef{
				SnapshotID: snap.SnapshotID, OrderbookSeq: snap.OrderbookSeq,
				StatsSeq: snap.StatsSeq, SourceTimestampMs: snap.SourceTimestampMs,
			})
			v.SnapshotRef = &ref
		}
	}
	return v
}

type perpOrderView struct {
	ID              string `json:"id"`
	OwnerDID        string `json:"owner_did"`
	ActingDID       string `json:"acting_did"`
	Symbol          string `json:"symbol"`
	Side            string `json:"side"`
	Type            string `json:"type"`
	Contracts       int64  `json:"contracts"`
	FilledContracts int64  `json:"filled_contracts"`
	LimitPriceCents int64  `json:"limit_price_cents,omitempty"`
	StopPriceCents  int64  `json:"stop_price_cents,omitempty"`
	TimeInForce     string `json:"time_in_force"`
	ReduceOnly      bool   `json:"reduce_only"`
	ClientOrderID   string `json:"client_order_id,omitempty"`
	Status          string `json:"status"`
	Venue           string `json:"venue"`
	CreatedAtMs     int64  `json:"created_at_ms"`
	UpdatedAtMs     int64  `json:"updated_at_ms"`
}

func toOrderView(o store.PerpOrder) perpOrderView {
	return perpOrderView{
		ID: o.ID, OwnerDID: o.OwnerDID, ActingDID: o.ActingDID, Symbol: o.MarketSymbol,
		Side: o.Side, Type: o.OrderType, Contracts: o.Contracts, FilledContracts: o.FilledContracts,
		LimitPriceCents: o.LimitPriceCents, StopPriceCents: o.StopPriceCents,
		TimeInForce: o.TimeInForce, ReduceOnly: o.ReduceOnly, ClientOrderID: o.ClientOrderID,
		Status: o.Status, Venue: "layerx",
		CreatedAtMs: o.CreatedAt.UnixMilli(), UpdatedAtMs: o.UpdatedAt.UnixMilli(),
	}
}

type perpFillView struct {
	ID               string              `json:"id"`
	OrderID          string              `json:"order_id"`
	PositionID       string              `json:"position_id"`
	OwnerDID         string              `json:"owner_did"`
	ActingDID        string              `json:"acting_did"`
	Symbol           string              `json:"symbol"`
	Side             string              `json:"side"`
	Contracts        int64               `json:"contracts"`
	PriceCents       int64               `json:"price_cents"`
	NotionalMicro    int64               `json:"notional_micro_usdx"`
	FeeMicro         int64               `json:"fee_micro_usdx"`
	RealizedPnLMicro int64               `json:"realized_pnl_micro_usdx"`
	Maker            bool                `json:"maker"`
	Liquidation      bool                `json:"liquidation"`
	ExecutedAtMs     int64               `json:"executed_at_ms"`
	SnapshotRef      perpSnapshotRefView `json:"snapshot_ref"`
}

func toFillView(f store.PerpFill) perpFillView {
	return perpFillView{
		ID: f.ID, OrderID: f.OrderID, PositionID: f.PositionID, OwnerDID: f.OwnerDID,
		ActingDID: f.ActingDID, Symbol: f.MarketSymbol, Side: f.Side, Contracts: f.Contracts,
		PriceCents: f.PriceCents, NotionalMicro: f.NotionalMicro, FeeMicro: f.FeeMicro,
		RealizedPnLMicro: f.RealizedPnLMicro, Maker: f.Maker, Liquidation: f.Liquidation,
		ExecutedAtMs: f.CreatedAt.UnixMilli(), SnapshotRef: toRefView(f.Ref),
	}
}

type perpPositionView struct {
	ID                    string `json:"id"`
	Venue                 string `json:"venue"`
	OwnerDID              string `json:"owner_did"`
	Symbol                string `json:"symbol"`
	Side                  string `json:"side"`
	Contracts             int64  `json:"contracts"`
	EntryPriceCents       int64  `json:"entry_price_cents"`
	MarkPriceCents        int64  `json:"mark_price_cents,omitempty"`
	MarginMicro           int64  `json:"margin_micro_usdx"`
	InitialMarginMicro    int64  `json:"initial_margin_micro_usdx,omitempty"`
	MaintMarginMicro      int64  `json:"maintenance_margin_micro_usdx,omitempty"`
	UnrealizedPnLMicro    int64  `json:"unrealized_pnl_micro_usdx,omitempty"`
	RealizedPnLMicro      int64  `json:"realized_pnl_micro_usdx"`
	UnsettledFundingMicro int64  `json:"unsettled_funding_micro_usdx"`
	LiquidationPriceCents int64  `json:"liquidation_price_cents,omitempty"`
	Status                string `json:"status"`
	OpenedAtMs            int64  `json:"opened_at_ms"`
	UpdatedAtMs           int64  `json:"updated_at_ms"`
}

// toPositionView renders a position; mark-dependent risk fields are computed
// only from a live HEALTHY snapshot (never a stale or client-supplied price).
func (s *Server) toPositionView(p store.PerpPosition) perpPositionView {
	v := perpPositionView{
		ID: p.ID, Venue: "layerx", OwnerDID: p.OwnerDID, Symbol: p.MarketSymbol,
		Side: p.Side, Contracts: p.Contracts, EntryPriceCents: p.EntryPriceCents,
		MarginMicro: p.MarginMicro, RealizedPnLMicro: p.RealizedPnLMicro,
		UnsettledFundingMicro: p.UnsettledFundingMicro, Status: p.Status,
		OpenedAtMs: p.OpenedAt.UnixMilli(), UpdatedAtMs: p.UpdatedAt.UnixMilli(),
	}
	if p.Status == "CLOSED" || s.perps == nil || s.perps.Feed == nil {
		return v
	}
	snap, err := s.perps.Feed.Snapshot(p.MarketSymbol)
	if err != nil || snap.Health != crossverse.HealthHealthy {
		return v
	}
	mkt, err := market.Lookup(p.MarketSymbol)
	if err != nil {
		return v
	}
	notional, err := risk.Notional(p.Contracts)
	if err != nil {
		return v
	}
	v.MarkPriceCents = snap.MarkPriceCents
	if im, err := risk.InitialMargin(notional, mkt.InitialMarginBps); err == nil {
		v.InitialMarginMicro = im
	}
	mm, mmErr := risk.MaintenanceMargin(notional, mkt.MaintenanceMarginBps)
	if mmErr == nil {
		v.MaintMarginMicro = mm
	}
	side := risk.SideLong
	if p.Side == "SHORT" {
		side = risk.SideShort
	}
	if upnl, err := risk.UnrealizedPnL(side, notional, snap.MarkPriceCents, p.EntryPriceCents); err == nil {
		v.UnrealizedPnLMicro = upnl
	}
	if mmErr == nil {
		if fee, err := risk.LiquidationFee(notional, mkt.LiquidationFeeBps); err == nil {
			if lp, err := risk.LiquidationPriceCents(side, p.EntryPriceCents, notional, mm, fee,
				0, 0, p.MarginMicro, mkt.TickPriceUnits); err == nil {
				v.LiquidationPriceCents = lp
			}
		}
	}
	return v
}

type perpAccountView struct {
	OwnerDID                     string `json:"owner_did"`
	SpendableMicroUSDX           int64  `json:"spendable_micro_usdx"`
	MarginMicroUSDX              int64  `json:"margin_micro_usdx"`
	AvailableWithdrawalMicroUSDX int64  `json:"available_withdrawal_micro_usdx"`
	RealizedPnLMicroUSDX         int64  `json:"realized_pnl_micro_usdx"`
	UnsettledFundingMicroUSDX    int64  `json:"unsettled_funding_micro_usdx"`
	OpenOrderMarginMicroUSDX     int64  `json:"open_order_margin_micro_usdx"`
	EventID                      int64  `json:"event_id"`
}

func toAccountView(a store.PerpAccount) perpAccountView {
	// Spendable balance is already net of margin and reservations, so the
	// conservative available-withdrawal floor is the spendable balance itself;
	// position equity above initial margin stays locked until reduced.
	return perpAccountView{
		OwnerDID: a.OwnerDID, SpendableMicroUSDX: a.SpendableMicro,
		MarginMicroUSDX: a.PositionMarginMicro, AvailableWithdrawalMicroUSDX: a.SpendableMicro,
		RealizedPnLMicroUSDX: a.RealizedPnLMicro, UnsettledFundingMicroUSDX: a.UnsettledFundingMicro,
		OpenOrderMarginMicroUSDX: a.OpenOrderMarginMicro, EventID: a.LastOwnerEventID,
	}
}

type perpReceiptView struct {
	ReceiptID          string   `json:"receipt_id"`
	OwnerDID           string   `json:"owner_did"`
	ActingDID          string   `json:"acting_did"`
	OrderID            string   `json:"order_id,omitempty"`
	FillIDs            []string `json:"fill_ids,omitempty"`
	IdempotencyKey     string   `json:"idempotency_key"`
	EventSeqLo         int64    `json:"event_seq_lo"`
	EventSeqHi         int64    `json:"event_seq_hi"`
	SnapshotID         string   `json:"snapshot_id,omitempty"`
	OrderbookSeq       int64    `json:"orderbook_seq,omitempty"`
	SequencerSignature string   `json:"sequencer_signature"`
	SequencerPublicKey string   `json:"sequencer_public_key"`
}

func toReceiptView(r engine.Receipt) perpReceiptView {
	return perpReceiptView{
		ReceiptID: r.ReceiptID, OwnerDID: r.OwnerDID, ActingDID: r.ActingDID,
		OrderID: r.OrderID, FillIDs: r.FillIDs, IdempotencyKey: r.IdempotencyKey,
		EventSeqLo: r.EventSeqLo, EventSeqHi: r.EventSeqHi,
		SnapshotID: r.SnapshotID, OrderbookSeq: r.OrderbookSeq,
		SequencerSignature: r.SequencerSignature, SequencerPublicKey: r.SequencerPublicKey,
	}
}

type perpDelegationView struct {
	ID                        string   `json:"id"`
	OwnerDID                  string   `json:"owner_did"`
	DelegateDID               string   `json:"delegate_did"`
	MembershipTier            string   `json:"membership_tier"`
	AllowedMarkets            []string `json:"allowed_markets"`
	AllowedOrderTypes         []string `json:"allowed_order_types"`
	MaxOrderNotionalMicro     int64    `json:"maximum_order_notional_micro_usdx"`
	MaxPositionNotionalMicro  int64    `json:"maximum_position_notional_micro_usdx"`
	MaxLeverageX              int64    `json:"maximum_leverage_x"`
	MaxDailyNotionalMicro     int64    `json:"maximum_daily_notional_micro_usdx"`
	MaxDailyRealizedLossMicro int64    `json:"maximum_daily_realized_loss_micro_usdx"`
	Status                    string   `json:"status"`
	ExpiresAtMs               int64    `json:"expires_at_ms"`
	CreatedAtMs               int64    `json:"created_at_ms"`
}

func toDelegationView(d store.PerpDelegation) perpDelegationView {
	return perpDelegationView{
		ID: d.ID, OwnerDID: d.OwnerDID, DelegateDID: d.DelegateDID,
		MembershipTier: d.MembershipTier, AllowedMarkets: d.AllowedMarkets,
		AllowedOrderTypes:         d.AllowedOrderTypes,
		MaxOrderNotionalMicro:     d.MaxOrderNotionalMicro,
		MaxPositionNotionalMicro:  d.MaxPositionNotionalMicro,
		MaxLeverageX:              d.MaxLeverageX,
		MaxDailyNotionalMicro:     d.MaxDailyNotionalMicro,
		MaxDailyRealizedLossMicro: d.MaxDailyRealizedLossMicro,
		Status:                    d.Status, ExpiresAtMs: d.ExpiresAt.UnixMilli(),
		CreatedAtMs: d.CreatedAt.UnixMilli(),
	}
}

// ─── mode + error mapping ───────────────────────────────────────────────────

func (s *Server) perpsEffective(symbol string) mode.Mode {
	if s.perps == nil || s.perps.Modes == nil {
		return mode.Off
	}
	return s.perps.Modes.Effective(symbol)
}

// perpsModeCode maps a denied mode to its stable code.
func (s *Server) perpsModeCode(symbol string) (int, string, string) {
	eff := s.perpsEffective(symbol)
	switch eff {
	case mode.Off:
		if s.perps != nil && s.perps.Modes != nil && s.perps.Modes.Global() == mode.Off {
			return http.StatusServiceUnavailable, codePerpsDisabled, "perps are disabled"
		}
		return http.StatusServiceUnavailable, codeMarketDisabled, "market is disabled"
	case mode.Shadow:
		return http.StatusForbidden, codeShadowOnly, "market accepts shadow-recorded intents only"
	case mode.Paused:
		return http.StatusServiceUnavailable, codeMarketPaused, "market is paused"
	case mode.ReduceOnly:
		return http.StatusForbidden, codeReduceOnly, "market permits reduction and cancel only"
	case mode.Canary:
		return http.StatusForbidden, codeCanaryDenied, "canary access denied"
	}
	// Boot mode permits it, so the durable market row denied it.
	ctx, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRead()
	if row, err := s.store.GetPerpMarket(ctx, symbol); err == nil {
		switch row.Mode {
		case "PAUSED":
			return http.StatusServiceUnavailable, codeMarketPaused, "market is paused: " + row.PausedCause
		case "REDUCE_ONLY":
			return http.StatusForbidden, codeReduceOnly, "market permits reduction and cancel only"
		}
	}
	return http.StatusServiceUnavailable, codeMarketDisabled, "market mode denies this action"
}

// perpsError maps engine/store errors to the stable perps error codes.
func (s *Server) perpsError(w http.ResponseWriter, symbol string, err error) {
	switch {
	case errors.Is(err, engine.ErrCanaryDenied):
		writePerpsFail(w, http.StatusForbidden, codeCanaryDenied, err.Error(), false)
	case errors.Is(err, engine.ErrRolloutDenied):
		writePerpsFail(w, http.StatusForbidden, codeRolloutDenied, err.Error(), false)
	case errors.Is(err, engine.ErrMarketStale):
		code := codeMarketStale
		msg := "market data is not fresh; risk increase fails closed"
		if s.perps != nil && s.perps.Feed != nil && symbol != "" {
			if snap, ferr := s.perps.Feed.Snapshot(symbol); ferr == nil {
				switch snap.Health {
				case crossverse.HealthStaleDivergence:
					code = codeMarketDiverged
					msg = "mark/index divergence exceeds the market limit"
				}
			}
		}
		writePerpsFail(w, http.StatusServiceUnavailable, code, msg, true)
	case errors.Is(err, engine.ErrPositionLimit):
		writePerpsFail(w, http.StatusUnprocessableEntity, codePositionLimit, err.Error(), false)
	case errors.Is(err, engine.ErrMarketOILimit):
		writePerpsFail(w, http.StatusUnprocessableEntity, codeMarketOILimit, err.Error(), false)
	case errors.Is(err, engine.ErrPoolCapacity), errors.Is(err, store.ErrPoolInsufficient):
		writePerpsFail(w, http.StatusUnprocessableEntity, codePoolCapacity, err.Error(), false)
	case errors.Is(err, engine.ErrReduceOnlyNoRisk), errors.Is(err, engine.ErrReduceExceeds):
		writePerpsFail(w, http.StatusUnprocessableEntity, codeReduceOnly, err.Error(), false)
	case errors.Is(err, pricing.ErrInsufficientBookDepth):
		writePerpsFail(w, http.StatusUnprocessableEntity, codeInsufficientBook, err.Error(), true)
	case errors.Is(err, store.ErrDelegationRequired):
		writePerpsFail(w, http.StatusForbidden, codeDelegationRequired, err.Error(), false)
	case errors.Is(err, store.ErrDelegationLimit):
		writePerpsFail(w, http.StatusForbidden, codeDelegationLimit, err.Error(), false)
	case errors.Is(err, store.ErrMembershipRequired):
		writePerpsFail(w, http.StatusForbidden, codeMembershipRequired, err.Error(), false)
	case errors.Is(err, store.ErrModeDenied):
		status, code, msg := s.perpsModeCode(symbol)
		writePerpsFail(w, status, code, msg, false)
	case errors.Is(err, store.ErrInsufficientFunds), errors.Is(err, store.ErrMarginInsufficient):
		writePerpsFail(w, http.StatusPaymentRequired, codeInsufficientMargn, err.Error(), false)
	case errors.Is(err, store.ErrIdempotencyConflict):
		writePerpsFail(w, http.StatusConflict, codeIdemConflict, "idempotency key was used for a different request", false)
	case errors.Is(err, store.ErrIdempotencyInFlight):
		writePerpsFail(w, http.StatusConflict, codeIdemConflict, "original request is still executing; retry", true)
	case errors.Is(err, store.ErrOrderTerminal):
		writePerpsFail(w, http.StatusConflict, codeOrderTerminal, "order is in a terminal state", false)
	case errors.Is(err, store.ErrPositionClosed):
		writePerpsFail(w, http.StatusConflict, codeOrderTerminal, "position is closed", false)
	case errors.Is(err, store.ErrNotFound):
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "not found")
	default:
		s.log.Error("perps request failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "perps request failed")
	}
}

// ─── auth ───────────────────────────────────────────────────────────────────

// perpIntentEnvelope is the DID intent envelope carried at the top level of
// every authenticated perps write body. OwnerDID names the account a
// DELEGATE acts for (empty or equal to the actor = acting for itself); when
// present it is appended to the signed canonical fields so a delegate's
// signature binds exactly one owner.
type perpIntentEnvelope struct {
	FromDID        string `json:"from_did"`
	PublicKey      string `json:"public_key"`
	Nonce          string `json:"nonce"`
	Signature      string `json:"signature"`
	IdempotencyKey string `json:"idempotency_key"`
	OwnerDID       string `json:"owner_did"`
}

// perpsRequestHash is the canonical exactly-once identity of one operation:
// nonce-free, so the same signed request replays across nonce consumption.
func perpsRequestHash(op string, fields []string) string {
	sum := sha256.Sum256([]byte("perps." + op + "\x00" + strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])
}

// perpsReadOwner authenticates a read: the owner derives ONLY from the
// X-LayerX-Agent principal token, never a query parameter.
func (s *Server) perpsReadOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := s.principal(w, r)
	if !ok {
		return "", false
	}
	return claims.DID, true
}

// perpsWriteAuth authorizes a perps write and returns (owner, acting,
// requestHash). It implements the locked direct-signature retry order. A
// delegate names its owner in owner_did (appended to the signed canonical
// fields); ownerOnly rejects delegated acting entirely (delegation lifecycle
// ops are owner-authorized by definition). The engine transaction remains the
// authority on whether the delegation actually permits the action.
func (s *Server) perpsWriteAuth(w http.ResponseWriter, r *http.Request, op string,
	env perpIntentEnvelope, fields []string, ownerOnly bool) (owner, acting, requestHash string, ok bool) {

	if env.IdempotencyKey == "" {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "idempotency_key is required")
		return "", "", "", false
	}

	resolve := func(actor string) (string, bool) {
		ownerDID := strings.TrimSpace(env.OwnerDID)
		if ownerDID == "" || ownerDID == actor {
			return actor, true
		}
		if _, err := auth.ParseDID(ownerDID); err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "owner_did must be a valid did:matrix")
			return "", false
		}
		if ownerOnly {
			writeFail(w, http.StatusForbidden, types.CodeUnauthorized, "this operation is owner-authorized only")
			return "", false
		}
		// The owner binds into the canonical fields so a delegate signature
		// covers exactly one owner.
		fields = append(fields, ownerDID)
		return ownerDID, true
	}

	if strings.TrimSpace(r.Header.Get("X-LayerX-Agent")) != "" {
		claims, pok := s.principal(w, r)
		if !pok {
			return "", "", "", false
		}
		ownerDID, rok := resolve(claims.DID)
		if !rok {
			return "", "", "", false
		}
		return ownerDID, claims.DID, perpsRequestHash(op, fields), true
	}

	if env.FromDID == "" || env.Signature == "" {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized,
			"authorize with an X-LayerX-Agent token or a DID-signed intent (from_did, public_key, nonce, signature)")
		return "", "", "", false
	}
	if _, err := auth.ParseDID(env.FromDID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "from_did must be a valid did:matrix")
		return "", "", "", false
	}
	ownerDID, rok := resolve(env.FromDID)
	if !rok {
		return "", "", "", false
	}
	hash := perpsRequestHash(op, fields)
	preimage := auth.IntentMessage("perps."+op, env.FromDID, env.Nonce, fields...)
	if err := auth.VerifyIntentSignature(env.FromDID, env.PublicKey, env.Signature, preimage); err != nil {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "intent signature: "+err.Error())
		return "", "", "", false
	}
	// Look up the exactly-once identity BEFORE the nonce: a retry of the same
	// request replays even when its nonce was already consumed. Idempotency is
	// owner-scoped, matching the engine's claim.
	_, storedHash, _, _, found, err := s.store.LookupPerpIdempotency(r.Context(), ownerDID, env.IdempotencyKey)
	if err != nil {
		s.log.Error("perps idempotency lookup failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not check idempotency")
		return "", "", "", false
	}
	if found {
		// The request hash binds the operation and its canonical fields, so a
		// hash match IS the same-request proof (the stored operation label may
		// be the engine's internal name, e.g. close -> perps.order).
		if storedHash != hash {
			writePerpsFail(w, http.StatusConflict, codeIdemConflict, "idempotency key was used for a different request", false)
			return "", "", "", false
		}
		return ownerDID, env.FromDID, hash, true
	}
	if !s.challenges.Consume(env.Nonce, env.FromDID) {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "unknown, expired, or already-used nonce")
		return "", "", "", false
	}
	return ownerDID, env.FromDID, hash, true
}

// perpsWriteReady fails closed when no engine is wired (perps disabled).
func (s *Server) perpsWriteReady(w http.ResponseWriter) bool {
	if s.perps == nil || s.perps.Engine == nil {
		writePerpsFail(w, http.StatusServiceUnavailable, codePerpsDisabled, "perps are not enabled on this deployment", false)
		return false
	}
	return true
}

// decodePerpsBody decodes the body into v and rejects any client-supplied
// pricing/risk field: the server never trusts client execution economics.
func decodePerpsBody(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "could not read body")
		return false
	}
	if len(raw) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "invalid json body: "+err.Error())
			return false
		}
		for _, k := range []string{
			"execution_price_cents", "price_cents", "mark_price_cents", "index_price_cents",
			"fee_micro_usdx", "fee_usdx", "margin_micro_usdx", "required_margin_micro_usdx",
			"initial_margin_micro_usdx", "liquidation_price_cents",
		} {
			if _, bad := probe[k]; bad {
				writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest,
					"client-supplied field "+k+" is rejected; the server calculates all execution economics")
				return false
			}
		}
		if err := json.Unmarshal(raw, v); err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "invalid json body: "+err.Error())
			return false
		}
	}
	return true
}

// ─── public reads ───────────────────────────────────────────────────────────

func (s *Server) handlePerpsMarkets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListPerpMarkets(r.Context())
	if err != nil {
		s.log.Error("list perp markets failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not list markets")
		return
	}
	views := make([]perpMarketView, 0, len(rows))
	for _, row := range rows {
		views = append(views, s.toMarketView(row))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"markets": views, "count": len(views)}))
}

func (s *Server) handlePerpsMarket(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.PathValue("symbol")))
	row, err := s.store.GetPerpMarket(r.Context(), symbol)
	if err != nil {
		s.perpsError(w, symbol, err)
		return
	}
	writeJSON(w, http.StatusOK, types.OK(s.toMarketView(row)))
}

func (s *Server) perpsPublicSnapshot(w http.ResponseWriter, r *http.Request) (crossverse.NormalizedSnapshot, string, bool) {
	symbol := strings.ToUpper(strings.TrimSpace(r.PathValue("symbol")))
	if _, err := market.Lookup(symbol); err != nil {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "unknown market")
		return crossverse.NormalizedSnapshot{}, symbol, false
	}
	if !s.perpsEffective(symbol).Permissions().PublicReads {
		status, code, msg := s.perpsModeCode(symbol)
		writePerpsFail(w, status, code, msg, false)
		return crossverse.NormalizedSnapshot{}, symbol, false
	}
	if s.perps == nil || s.perps.Feed == nil {
		writePerpsFail(w, http.StatusServiceUnavailable, codeMarketStale, "market data feed is not running", true)
		return crossverse.NormalizedSnapshot{}, symbol, false
	}
	snap, err := s.perps.Feed.Snapshot(symbol)
	if err != nil {
		writePerpsFail(w, http.StatusServiceUnavailable, codeMarketStale, err.Error(), true)
		return crossverse.NormalizedSnapshot{}, symbol, false
	}
	return snap, symbol, true
}

func (s *Server) handlePerpsOrderbook(w http.ResponseWriter, r *http.Request) {
	snap, symbol, ok := s.perpsPublicSnapshot(w, r)
	if !ok {
		return
	}
	if snap.Health != crossverse.HealthHealthy {
		writePerpsFail(w, http.StatusServiceUnavailable, codeMarketStale, "order book is not fresh", true)
		return
	}
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	if depth <= 0 || depth > 50 {
		depth = 20
	}
	levels := func(in []crossverse.Level) [][2]int64 {
		out := make([][2]int64, 0, depth)
		for i, lv := range in {
			if i >= depth {
				break
			}
			out = append(out, [2]int64{lv.PriceCents, lv.Contracts})
		}
		return out
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"symbol": symbol,
		"snapshot_ref": toRefView(store.PerpSnapshotRef{
			SnapshotID: snap.SnapshotID, OrderbookSeq: snap.OrderbookSeq,
			StatsSeq: snap.StatsSeq, SourceTimestampMs: snap.SourceTimestampMs,
		}),
		"bids": levels(snap.Bids),
		"asks": levels(snap.Asks),
	}))
}

func (s *Server) handlePerpsTrades(w http.ResponseWriter, r *http.Request) {
	snap, symbol, ok := s.perpsPublicSnapshot(w, r)
	if !ok {
		return
	}
	trades, err := s.perps.Feed.RecentTrades(symbol)
	if err != nil {
		writePerpsFail(w, http.StatusServiceUnavailable, codeMarketStale, err.Error(), true)
		return
	}
	limit := parseLimit(r)
	before := parseInt64(r.URL.Query().Get("before"))
	ref := toRefView(store.PerpSnapshotRef{
		SnapshotID: snap.SnapshotID, OrderbookSeq: snap.OrderbookSeq,
		StatsSeq: snap.StatsSeq, SourceTimestampMs: snap.SourceTimestampMs,
	})
	type tradeView struct {
		ID            string              `json:"id"`
		Symbol        string              `json:"symbol"`
		Side          string              `json:"side"`
		PriceCents    int64               `json:"price_cents"`
		Contracts     int64               `json:"contracts"`
		NotionalMicro int64               `json:"notional_micro_usdx"`
		Liquidation   bool                `json:"liquidation"`
		ExecutedAtMs  int64               `json:"executed_at_ms"`
		SnapshotRef   perpSnapshotRefView `json:"snapshot_ref"`
	}
	items := make([]tradeView, 0, limit)
	var nextCursor *string
	for i := len(trades) - 1; i >= 0; i-- { // newest first
		t := trades[i]
		if before > 0 && t.TradeTimeMs >= before {
			continue
		}
		if len(items) == limit {
			c := strconv.FormatInt(items[len(items)-1].ExecutedAtMs, 10)
			nextCursor = &c
			break
		}
		items = append(items, tradeView{
			ID:     fmt.Sprintf("%s-%d-%d", symbol, t.TradeTimeMs, i),
			Symbol: symbol, Side: t.Side, PriceCents: t.PriceCents, Contracts: t.Contracts,
			NotionalMicro: t.NotionalMicroUSDX, Liquidation: t.Liquidation,
			ExecutedAtMs: t.TradeTimeMs, SnapshotRef: ref,
		})
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"items": items, "next_cursor": nextCursor}))
}

// ─── authenticated reads ────────────────────────────────────────────────────

func (s *Server) handlePerpsAccount(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	a, err := s.store.GetPerpAccount(r.Context(), owner)
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, types.OK(toAccountView(a)))
}

func (s *Server) handlePerpsOrders(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	rows, err := s.store.ListPerpOrders(r.Context(), owner,
		strings.ToUpper(strings.TrimSpace(q.Get("status"))),
		strings.ToUpper(strings.TrimSpace(q.Get("symbol"))), parseLimit(r))
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	views := make([]perpOrderView, 0, len(rows))
	for _, o := range rows {
		views = append(views, toOrderView(o))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"items": views, "next_cursor": nil}))
}

func (s *Server) handlePerpsPositions(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListPerpPositionsByOwner(r.Context(), owner,
		strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))), parseLimit(r))
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	views := make([]perpPositionView, 0, len(rows))
	for _, p := range rows {
		views = append(views, s.toPositionView(p))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"items": views, "next_cursor": nil}))
}

func (s *Server) handlePerpsFills(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListPerpFills(r.Context(), owner,
		strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol"))), parseLimit(r))
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	views := make([]perpFillView, 0, len(rows))
	for _, f := range rows {
		views = append(views, toFillView(f))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"items": views, "next_cursor": nil}))
}

func (s *Server) handlePerpsDelegations(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListPerpDelegations(r.Context(), owner, parseLimit(r))
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	views := make([]perpDelegationView, 0, len(rows))
	for _, d := range rows {
		views = append(views, toDelegationView(d))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"items": views, "next_cursor": nil}))
}

// ─── order placement ────────────────────────────────────────────────────────

type perpOrderBody struct {
	perpIntentEnvelope
	Symbol          string `json:"symbol"`
	Side            string `json:"side"`
	Type            string `json:"type"`
	Contracts       int64  `json:"contracts"`
	LimitPriceCents int64  `json:"limit_price_cents"`
	StopPriceCents  int64  `json:"stop_price_cents"`
	TimeInForce     string `json:"time_in_force"`
	ReduceOnly      bool   `json:"reduce_only"`
	ClientOrderID   string `json:"client_order_id"`
}

func orderCanonicalFields(b perpOrderBody) []string {
	return []string{
		b.Symbol, b.Side, b.Type, strconv.FormatInt(b.Contracts, 10),
		strconv.FormatInt(b.LimitPriceCents, 10), strconv.FormatInt(b.StopPriceCents, 10),
		b.TimeInForce, strconv.FormatBool(b.ReduceOnly), b.ClientOrderID, b.IdempotencyKey,
	}
}

// replayPerpOrder rebuilds a stored engine response {order, fills, receipt}
// without re-entering the engine (the direct-signature retry path).
func (s *Server) replayPerpOrder(w http.ResponseWriter, r *http.Request, owner, key string) bool {
	_, _, status, stored, found, err := s.store.LookupPerpIdempotency(r.Context(), owner, key)
	if err != nil || !found {
		return false
	}
	if status != "done" {
		writePerpsFail(w, http.StatusConflict, codeIdemConflict, "original request is still executing; retry", true)
		return true
	}
	var resp struct {
		OrderID    string `json:"order_id"`
		FillID     string `json:"fill_id"`
		PositionID string `json:"position_id"`
		EventSeqLo int64  `json:"event_seq_lo"`
		EventSeqHi int64  `json:"event_seq_hi"`
	}
	if err := json.Unmarshal(stored, &resp); err != nil || resp.OrderID == "" {
		return false
	}
	order, err := s.store.GetPerpOrder(r.Context(), resp.OrderID)
	if err != nil {
		return false
	}
	fills := []perpFillView{}
	if resp.FillID != "" {
		if f, err := s.store.GetPerpFill(r.Context(), resp.FillID); err == nil {
			fills = append(fills, toFillView(f))
		}
	}
	receipt := s.perps.Engine.SignResult(store.PerpExecResult{
		Order: order, FillID: resp.FillID, PositionID: resp.PositionID,
		EventSeqLo: resp.EventSeqLo, EventSeqHi: resp.EventSeqHi,
	})
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"order": toOrderView(order), "fills": fills, "receipt": toReceiptView(receipt),
		"replayed": true,
	}))
	return true
}

func (s *Server) handlePerpsPlaceOrder(w http.ResponseWriter, r *http.Request) {
	var b perpOrderBody
	if !decodePerpsBody(w, r, &b) {
		return
	}
	b.Symbol = strings.ToUpper(strings.TrimSpace(b.Symbol))
	b.Side = strings.ToUpper(strings.TrimSpace(b.Side))
	b.Type = strings.ToUpper(strings.TrimSpace(b.Type))
	b.TimeInForce = strings.ToUpper(strings.TrimSpace(b.TimeInForce))
	if b.TimeInForce == "" {
		b.TimeInForce = "GTC"
	}
	mkt, err := market.Lookup(b.Symbol)
	if err != nil {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "unknown market")
		return
	}
	if b.Contracts < mkt.MinOrderContracts {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest,
			fmt.Sprintf("contracts must be at least %d", mkt.MinOrderContracts))
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	owner, acting, hash, ok := s.perpsWriteAuth(w, r, "order", b.perpIntentEnvelope, orderCanonicalFields(b), false)
	if !ok {
		return
	}
	if s.replayPerpOrder(w, r, owner, b.IdempotencyKey) {
		return
	}
	res, err := s.perps.Engine.PlaceOrder(r.Context(), engine.OrderRequest{
		OwnerDID: owner, ActingDID: acting, Symbol: b.Symbol, Side: b.Side,
		OrderType: b.Type, Contracts: b.Contracts,
		LimitPriceCents: b.LimitPriceCents, StopPriceCents: b.StopPriceCents,
		TimeInForce: b.TimeInForce, ReduceOnly: b.ReduceOnly,
		ClientOrderID: b.ClientOrderID, IdempotencyKey: b.IdempotencyKey, RequestHash: hash,
	})
	if err != nil {
		s.perpsError(w, b.Symbol, err)
		return
	}
	s.perpsN.wake(owner)
	fills := []perpFillView{}
	if res.FillID != "" {
		if f, ferr := s.store.GetPerpFill(r.Context(), res.FillID); ferr == nil {
			fills = append(fills, toFillView(f))
		}
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"order": toOrderView(res.Order), "fills": fills, "receipt": toReceiptView(res.Receipt),
		"replayed": res.Replayed,
	}))
}

// ─── cancel ─────────────────────────────────────────────────────────────────

func (s *Server) handlePerpsCancelOrder(w http.ResponseWriter, r *http.Request) {
	orderID := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(orderID) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "order not found")
		return
	}
	var b struct{ perpIntentEnvelope }
	if !decodePerpsBody(w, r, &b) {
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	owner, acting, hash, ok := s.perpsWriteAuth(w, r, "cancel", b.perpIntentEnvelope,
		[]string{orderID, b.IdempotencyKey}, false)
	if !ok {
		return
	}
	// A delegate may cancel the owner's orders it placed even after
	// revocation; cancelling other owner orders requires a live grant.
	if acting != owner {
		if o, oerr := s.store.GetPerpOrder(r.Context(), orderID); oerr == nil &&
			o.OwnerDID == owner && o.ActingDID != acting {
			if _, derr := s.store.GetActivePerpDelegation(r.Context(), owner, acting); derr != nil {
				s.perpsError(w, "", store.ErrDelegationRequired)
				return
			}
		}
	}
	res, err := s.store.CancelPerpOrder(r.Context(), owner, orderID, acting, b.IdempotencyKey, hash, "user.cancel")
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	if !res.Replayed {
		s.perpsN.wake(owner)
	}
	receipt := s.perps.Engine.SignResult(store.PerpExecResult{
		Order: res.Order, EventSeqLo: res.EventSeqLo, EventSeqHi: res.EventSeqHi,
	})
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"order": toOrderView(res.Order), "released_micro_usdx": res.ReleasedMicro,
		"receipt": toReceiptView(receipt), "replayed": res.Replayed,
	}))
}

// ─── close / margin ─────────────────────────────────────────────────────────

func (s *Server) loadOwnedPosition(w http.ResponseWriter, r *http.Request, owner string) (store.PerpPosition, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(id) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "position not found")
		return store.PerpPosition{}, false
	}
	p, err := s.store.GetPerpPosition(r.Context(), id)
	if err != nil || p.OwnerDID != owner {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "position not found")
		return store.PerpPosition{}, false
	}
	return p, true
}

func (s *Server) handlePerpsClosePosition(w http.ResponseWriter, r *http.Request) {
	var b struct {
		perpIntentEnvelope
		Contracts int64 `json:"contracts"`
	}
	if !decodePerpsBody(w, r, &b) {
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	positionID := strings.TrimSpace(r.PathValue("id"))
	owner, acting, hash, ok := s.perpsWriteAuth(w, r, "close", b.perpIntentEnvelope,
		[]string{positionID, strconv.FormatInt(b.Contracts, 10), b.IdempotencyKey}, false)
	if !ok {
		return
	}
	if s.replayPerpOrder(w, r, owner, b.IdempotencyKey) {
		return
	}
	p, ok := s.loadOwnedPosition(w, r, owner)
	if !ok {
		return
	}
	if p.Status == "CLOSED" {
		writePerpsFail(w, http.StatusConflict, codeOrderTerminal, "position is already closed", false)
		return
	}
	contracts := b.Contracts
	if contracts <= 0 || contracts > p.Contracts {
		contracts = p.Contracts
	}
	side := "SELL"
	if p.Side == "SHORT" {
		side = "BUY"
	}
	res, err := s.perps.Engine.PlaceOrder(r.Context(), engine.OrderRequest{
		OwnerDID: owner, ActingDID: acting, Symbol: p.MarketSymbol, Side: side,
		OrderType: "MARKET", Contracts: contracts, TimeInForce: "IOC", ReduceOnly: true,
		IdempotencyKey: b.IdempotencyKey, RequestHash: hash,
	})
	if err != nil {
		s.perpsError(w, p.MarketSymbol, err)
		return
	}
	s.perpsN.wake(owner)
	position, perr := s.store.GetPerpPosition(r.Context(), p.ID)
	if perr != nil {
		position = p
	}
	account, aerr := s.store.GetPerpAccount(r.Context(), owner)
	if aerr != nil {
		s.perpsError(w, "", aerr)
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"position": s.toPositionView(position), "account": toAccountView(account),
		"order": toOrderView(res.Order), "receipt": toReceiptView(res.Receipt),
		"replayed": res.Replayed,
	}))
}

func (s *Server) handlePerpsPositionMargin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		perpIntentEnvelope
		Operation   string `json:"operation"`
		AmountMicro int64  `json:"amount_micro_usdx"`
	}
	if !decodePerpsBody(w, r, &b) {
		return
	}
	b.Operation = strings.ToUpper(strings.TrimSpace(b.Operation))
	if b.Operation != "ADD" && b.Operation != "REMOVE" {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "operation must be ADD or REMOVE")
		return
	}
	if b.AmountMicro <= 0 {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "amount_micro_usdx must be positive")
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	positionID := strings.TrimSpace(r.PathValue("id"))
	owner, acting, hash, ok := s.perpsWriteAuth(w, r, "margin", b.perpIntentEnvelope,
		[]string{positionID, b.Operation, strconv.FormatInt(b.AmountMicro, 10), b.IdempotencyKey}, false)
	if !ok {
		return
	}
	p, ok := s.loadOwnedPosition(w, r, owner)
	if !ok {
		return
	}
	// Delegated margin changes require a live grant (value only ever moves
	// between the OWNER's spendable balance and the OWNER's position).
	if acting != owner {
		if _, derr := s.store.GetActivePerpDelegation(r.Context(), owner, acting); derr != nil {
			s.perpsError(w, "", store.ErrDelegationRequired)
			return
		}
	}
	delta := b.AmountMicro
	minRemaining := int64(0)
	// A replayed retry returns the stored result before validation, so the
	// floor only needs computing for a NEW remove.
	_, _, idemStatus, _, idemFound, lerr := s.store.LookupPerpIdempotency(r.Context(), owner, b.IdempotencyKey)
	if lerr != nil {
		s.perpsError(w, "", lerr)
		return
	}
	replaying := idemFound && idemStatus == "done"
	if b.Operation == "REMOVE" {
		delta = -b.AmountMicro
		if !replaying {
			floor, ferr := s.perps.Engine.RemoveMarginFloor(p)
			if ferr != nil {
				s.perpsError(w, p.MarketSymbol, ferr)
				return
			}
			minRemaining = floor
		}
	}
	res, err := s.store.AdjustPerpPositionMargin(r.Context(), owner, p.ID, acting,
		delta, minRemaining, b.IdempotencyKey, hash)
	if err != nil {
		s.perpsError(w, p.MarketSymbol, err)
		return
	}
	if !res.Replayed {
		s.perpsN.wake(owner)
	}
	account, aerr := s.store.GetPerpAccount(r.Context(), owner)
	if aerr != nil {
		s.perpsError(w, "", aerr)
		return
	}
	receipt := s.perps.Engine.SignResult(store.PerpExecResult{
		Order:      store.PerpOrder{OwnerDID: owner, ActingDID: acting, IdempotencyKey: b.IdempotencyKey},
		PositionID: res.Position.ID, EventSeqLo: res.EventSeqLo, EventSeqHi: res.EventSeqHi,
	})
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"position": s.toPositionView(res.Position), "account": toAccountView(account),
		"receipt": toReceiptView(receipt), "replayed": res.Replayed,
	}))
}

// ─── quote ──────────────────────────────────────────────────────────────────

func (s *Server) handlePerpsQuote(w http.ResponseWriter, r *http.Request) {
	var b struct {
		perpIntentEnvelope
		Symbol          string `json:"symbol"`
		Side            string `json:"side"`
		Type            string `json:"type"`
		Contracts       int64  `json:"contracts"`
		LimitPriceCents int64  `json:"limit_price_cents"`
		TimeInForce     string `json:"time_in_force"`
		ReduceOnly      bool   `json:"reduce_only"`
	}
	if !decodePerpsBody(w, r, &b) {
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	b.Symbol = strings.ToUpper(strings.TrimSpace(b.Symbol))
	b.Side = strings.ToUpper(strings.TrimSpace(b.Side))
	// A quote is a read with no economic effect: the agent token authenticates,
	// or a DID-signed envelope verifies WITHOUT consuming its nonce.
	var owner string
	if strings.TrimSpace(r.Header.Get("X-LayerX-Agent")) != "" {
		claims, ok := s.principal(w, r)
		if !ok {
			return
		}
		owner = claims.DID
	} else {
		if b.FromDID == "" || b.Signature == "" {
			writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "authorize with an X-LayerX-Agent token or a DID-signed intent")
			return
		}
		fields := []string{b.Symbol, b.Side, strconv.FormatInt(b.Contracts, 10),
			strconv.FormatInt(b.LimitPriceCents, 10), b.IdempotencyKey}
		preimage := auth.IntentMessage("perps.quote", b.FromDID, b.Nonce, fields...)
		if err := auth.VerifyIntentSignature(b.FromDID, b.PublicKey, b.Signature, preimage); err != nil {
			writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "intent signature: "+err.Error())
			return
		}
		owner = b.FromDID
	}
	eff := s.perpsEffective(b.Symbol)
	if !eff.Permissions().ShadowCalculate {
		status, code, msg := s.perpsModeCode(b.Symbol)
		writePerpsFail(w, status, code, msg, false)
		return
	}
	q, err := s.perps.Engine.Quote(r.Context(), engine.QuoteRequest{
		OwnerDID: owner, Symbol: b.Symbol, Side: b.Side, Contracts: b.Contracts,
		LimitPriceCents: b.LimitPriceCents, ReduceOnly: b.ReduceOnly,
	}, time.Now().UnixMilli())
	if err != nil {
		s.perpsError(w, b.Symbol, err)
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"quote_id":                    q.QuoteID,
		"snapshot_ref":                toRefView(q.Ref),
		"execution_price_cents":       q.ExecutionPriceCents,
		"price_impact_bps":            q.PriceImpactBps,
		"fee_micro_usdx":              q.FeeMicro,
		"required_margin_micro_usdx":  q.RequiredMarginMicro,
		"liquidation_price_cents":     q.LiquidationPriceCents,
		"funding_estimate_micro_usdx": q.FundingEstimateMicro,
		"expires_at_ms":               q.ExpiresAtMs,
		"market_mode":                 q.MarketMode,
	}))
}

// ─── delegations ────────────────────────────────────────────────────────────

type perpDelegationBody struct {
	perpIntentEnvelope
	OwnerDID                  string   `json:"owner_did"`
	DelegateDID               string   `json:"delegate_did"`
	MembershipTier            string   `json:"membership_tier"`
	AllowedMarkets            []string `json:"allowed_markets"`
	AllowedOrderTypes         []string `json:"allowed_order_types"`
	MaxOrderNotionalMicro     int64    `json:"maximum_order_notional_micro_usdx"`
	MaxPositionNotionalMicro  int64    `json:"maximum_position_notional_micro_usdx"`
	MaxLeverageX              int64    `json:"maximum_leverage_x"`
	MaxDailyNotionalMicro     int64    `json:"maximum_daily_notional_micro_usdx"`
	MaxDailyRealizedLossMicro int64    `json:"maximum_daily_realized_loss_micro_usdx"`
	ExpiresAtMs               int64    `json:"expires_at_ms"`
	GrantSignature            string   `json:"grant_signature"`
}

func delegationGrantFields(b perpDelegationBody) []string {
	return []string{
		b.OwnerDID, b.DelegateDID, b.MembershipTier,
		strings.Join(b.AllowedMarkets, ","), strings.Join(b.AllowedOrderTypes, ","),
		strconv.FormatInt(b.MaxOrderNotionalMicro, 10),
		strconv.FormatInt(b.MaxPositionNotionalMicro, 10),
		strconv.FormatInt(b.MaxLeverageX, 10),
		strconv.FormatInt(b.MaxDailyNotionalMicro, 10),
		strconv.FormatInt(b.MaxDailyRealizedLossMicro, 10),
		strconv.FormatInt(b.ExpiresAtMs, 10),
	}
}

func (s *Server) handlePerpsCreateDelegation(w http.ResponseWriter, r *http.Request) {
	var b perpDelegationBody
	if !decodePerpsBody(w, r, &b) {
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	if _, err := auth.ParseDID(b.DelegateDID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "delegate_did must be a valid did:matrix")
		return
	}
	if b.DelegateDID == b.OwnerDID {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "a delegation cannot grant to its own owner")
		return
	}
	for _, sym := range b.AllowedMarkets {
		if _, err := market.Lookup(sym); err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "allowed_markets contains unknown market "+sym)
			return
		}
	}
	if b.ExpiresAtMs <= time.Now().UnixMilli() {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "expires_at_ms is in the past")
		return
	}
	fields := append(delegationGrantFields(b), b.GrantSignature, b.IdempotencyKey)
	owner, acting, hash, ok := s.perpsWriteAuth(w, r, "delegation.create", b.perpIntentEnvelope, fields, true)
	if !ok {
		return
	}
	if owner != b.OwnerDID {
		writeFail(w, http.StatusForbidden, types.CodeUnauthorized, "only the owner DID may create its delegation")
		return
	}
	// The grant signature is the owner's independent authorization over the
	// canonical grant, verified against the owner DID key fingerprint. A
	// delegate can never mint its own grant (transport auth is owner-only too).
	grantMsg := auth.PerpGrantMessage(delegationGrantFields(b)...)
	if err := auth.VerifyIntentSignature(b.OwnerDID, b.PublicKey, b.GrantSignature, grantMsg); err != nil {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "grant signature: "+err.Error())
		return
	}
	stored, claimed, err := s.store.ClaimPerpIdempotency(r.Context(), owner, b.IdempotencyKey, "perps.delegation.create", hash)
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	var d store.PerpDelegation
	replayed := false
	if !claimed {
		var resp struct {
			DelegationID string `json:"delegation_id"`
		}
		if jerr := json.Unmarshal(stored, &resp); jerr != nil || resp.DelegationID == "" {
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "stored delegation response is unreadable")
			return
		}
		d, err = s.store.GetPerpDelegation(r.Context(), resp.DelegationID)
		if err != nil {
			s.perpsError(w, "", err)
			return
		}
		replayed = true
	} else {
		d, err = s.store.CreatePerpDelegation(r.Context(), store.PerpDelegation{
			OwnerDID: b.OwnerDID, DelegateDID: b.DelegateDID, MembershipTier: b.MembershipTier,
			AllowedMarkets: b.AllowedMarkets, AllowedOrderTypes: b.AllowedOrderTypes,
			MaxOrderNotionalMicro:     b.MaxOrderNotionalMicro,
			MaxPositionNotionalMicro:  b.MaxPositionNotionalMicro,
			MaxLeverageX:              b.MaxLeverageX,
			MaxDailyNotionalMicro:     b.MaxDailyNotionalMicro,
			MaxDailyRealizedLossMicro: b.MaxDailyRealizedLossMicro,
			GrantSignature:            b.GrantSignature, PublicKey: b.PublicKey,
			ExpiresAt: time.UnixMilli(b.ExpiresAtMs),
		})
		if err != nil {
			s.perpsError(w, "", err)
			return
		}
		resp, _ := json.Marshal(map[string]string{"delegation_id": d.ID})
		if cerr := s.store.CompletePerpIdempotency(r.Context(), owner, b.IdempotencyKey, resp); cerr != nil {
			s.log.Error("complete delegation idempotency failed", "error", cerr.Error())
		}
		s.perpsN.wake(owner)
	}
	receipt := s.perps.Engine.SignResult(store.PerpExecResult{
		Order: store.PerpOrder{OwnerDID: owner, ActingDID: acting, IdempotencyKey: b.IdempotencyKey},
	})
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"delegation": toDelegationView(d), "receipt": toReceiptView(receipt), "replayed": replayed,
	}))
}

func (s *Server) handlePerpsRevokeDelegation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(id) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "delegation not found")
		return
	}
	var b struct{ perpIntentEnvelope }
	if !decodePerpsBody(w, r, &b) {
		return
	}
	if !s.perpsWriteReady(w) {
		return
	}
	owner, acting, hash, ok := s.perpsWriteAuth(w, r, "delegation.revoke", b.perpIntentEnvelope,
		[]string{id, b.IdempotencyKey}, true)
	if !ok {
		return
	}
	stored, claimed, err := s.store.ClaimPerpIdempotency(r.Context(), owner, b.IdempotencyKey, "perps.delegation.revoke", hash)
	if err != nil {
		s.perpsError(w, "", err)
		return
	}
	var d store.PerpDelegation
	var cancelled []string
	if !claimed {
		var resp struct {
			DelegationID string   `json:"delegation_id"`
			Cancelled    []string `json:"cancelled_order_ids"`
		}
		if jerr := json.Unmarshal(stored, &resp); jerr != nil || resp.DelegationID == "" {
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "stored revoke response is unreadable")
			return
		}
		d, err = s.store.GetPerpDelegation(r.Context(), resp.DelegationID)
		if err != nil {
			s.perpsError(w, "", err)
			return
		}
		cancelled = resp.Cancelled
	} else {
		d, cancelled, err = s.store.RevokePerpDelegation(r.Context(), id, owner)
		if err != nil {
			if errors.Is(err, store.ErrNotOwner) {
				writeFail(w, http.StatusForbidden, types.CodeUnauthorized, "only the granting owner may revoke")
				return
			}
			if errors.Is(err, store.ErrDelegationTerminal) {
				writePerpsFail(w, http.StatusConflict, codeOrderTerminal, "delegation is terminal", false)
				return
			}
			s.perpsError(w, "", err)
			return
		}
		resp, _ := json.Marshal(map[string]any{"delegation_id": d.ID, "cancelled_order_ids": cancelled})
		if cerr := s.store.CompletePerpIdempotency(r.Context(), owner, b.IdempotencyKey, resp); cerr != nil {
			s.log.Error("complete revoke idempotency failed", "error", cerr.Error())
		}
		s.perpsN.wake(owner)
	}
	receipt := s.perps.Engine.SignResult(store.PerpExecResult{
		Order: store.PerpOrder{OwnerDID: owner, ActingDID: acting, IdempotencyKey: b.IdempotencyKey},
	})
	if cancelled == nil {
		cancelled = []string{}
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{
		"delegation": toDelegationView(d), "cancelled_order_ids": cancelled,
		"receipt": toReceiptView(receipt),
	}))
}

// ─── owner-private SSE stream ───────────────────────────────────────────────

// handlePerpsStream streams the owner's durable private events. Last-Event-ID
// (or ?since=) resumes strictly after the owner's event id; replay reads the
// journal, then the stream tails new commits via the post-commit notifier with
// a periodic durable poll as the fallback for worker-side commits.
func (s *Server) handlePerpsStream(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.perpsReadOwner(w, r)
	if !ok {
		return
	}
	flusher, fok := w.(http.Flusher)
	if !fok {
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	cursor := parseInt64(r.Header.Get("Last-Event-ID"))
	if cursor <= 0 {
		cursor = parseInt64(r.URL.Query().Get("since"))
	}
	if cursor < 0 {
		cursor = 0
	}

	wakeCh, cancel := s.perpsN.subscribe(owner)
	defer cancel()

	_, _ = io.WriteString(w, "retry: 3000\n\n")
	flusher.Flush()

	emit := func() bool {
		for {
			events, err := s.store.ListPerpOwnerEvents(r.Context(), owner, cursor, 200)
			if err != nil {
				return false
			}
			for _, ev := range events {
				data, jerr := json.Marshal(map[string]any{
					"id": ev.OwnerEventID, "type": ev.EventType,
					"occurred_at": ev.OccurredAt.UTC().Format(time.RFC3339Nano),
					"owner_did":   ev.OwnerDID, "acting_did": ev.ActingDID,
					"data": json.RawMessage(ev.Payload),
				})
				if jerr != nil {
					continue
				}
				writeSSE(w, ev.OwnerEventID, ev.EventType, data)
				cursor = ev.OwnerEventID
			}
			flusher.Flush()
			if len(events) < 200 {
				return true
			}
		}
	}
	if !emit() {
		return
	}

	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wakeCh:
			if !emit() {
				return
			}
		case <-poll.C:
			if !emit() {
				return
			}
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
