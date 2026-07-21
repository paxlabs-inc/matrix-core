package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrPlanStale is returned when the ledger state an execution plan was
// computed against changed before the transaction could lock it. The engine
// recomputes and retries.
var ErrPlanStale = errors.New("store: execution plan is stale")

// ErrModeDenied is returned when the durable market mode forbids the action
// inside the execution transaction.
var ErrModeDenied = errors.New("store: market mode denies this action")

// ErrOrderClaimed is returned when a triggered order could not be claimed
// (another worker holds it or it already left RESTING).
var ErrOrderClaimed = errors.New("store: order is not claimable")

// PerpIntentFill is one execution against the book, bound to its snapshot.
type PerpIntentFill struct {
	Contracts     int64
	PriceCents    int64
	NotionalMicro int64
	FeeMicro      int64
	Maker         bool
	Liquidation   bool
}

// PerpIntentOpen opens or increases a position: initial margin plus the fee
// leave the spendable balance (or a held reservation), the fee accrues to
// protocol liquidity, and the position takes the engine-computed entry price.
type PerpIntentOpen struct {
	Fill               PerpIntentFill
	Side               string
	InitialMarginMicro int64
	NewEntryPriceCents int64
	FromReservationID  string
}

// PerpIntentReduce closes contracts: the fee leaves position margin to the
// pool, realized PnL is one signed conserved transfer between margin and the
// pool, and the margin return goes back to the spendable balance.
type PerpIntentReduce struct {
	Fill              PerpIntentFill
	CloseContracts    int64
	RealizedPnLMicro  int64
	MarginReturnMicro int64
	FullClose         bool
}

// PerpIntentRest parks a GTC/stop order with held initial margin.
type PerpIntentRest struct {
	ReserveMicro int64
}

// PerpShadowObservation is non-economic comparison evidence captured for a
// SHADOW_RECORDED intent. It is inserted atomically with the terminal order
// and cannot reserve margin, create fills, or mutate a position.
type PerpShadowObservation struct {
	EngineExecutionPriceCents      int64
	ReferenceExecutionPriceCents   int64
	EngineMarginMicro              int64
	ReferenceMarginMicro           int64
	EngineFeeMicro                 int64
	ReferenceFeeMicro              int64
	EngineFundingMicro             int64
	ReferenceFundingMicro          int64
	EngineLiquidationPriceCents    int64
	ReferenceLiquidationPriceCents int64
	EnginePnLMicro                 int64
	ReferencePnLMicro              int64
	EngineError                    string
	ReferenceError                 string
	EngineErrorCode                string
	ReferenceErrorCode             string
	ExecutionToleranceCents        int64
	MismatchFields                 []string
	FeedGapDetected                bool
}

// PerpIntentDelegation is the transactional delegation recheck for a
// delegated intent: the grant row is locked and every bound re-verified
// inside the SAME transaction that accepts or fills risk increase.
type PerpIntentDelegation struct {
	DelegateDID string
	// AssertedTierRank is the engine-resolved rank of the owner's CURRENT
	// membership tier (0 = unresolved: fails closed for risk increase).
	AssertedTierRank int
	// GrantTierRank maps the locked grant's membership_tier at check time.
	GrantTierRank func(tier string) int
	// OrderNotionalMicro is this order's notional for the order/daily bounds.
	OrderNotionalMicro int64
	// ProjectedPositionNotionalMicro / ProjectedMarginMicro back the position
	// and leverage bounds after the action.
	ProjectedPositionNotionalMicro int64
	ProjectedMarginMicro           int64
	// DayStartUTC is the owner-timezone daily-window boundary as a UTC instant.
	DayStartUTC time.Time
	// RiskIncrease selects full bound enforcement; reduce/cancel only requires
	// the ACTIVE unexpired grant.
	RiskIncrease bool
}

// PerpIntent is the fully-computed, integer-only plan for one signed intent.
// The store executes it atomically, rechecking mode, balances, and the
// expected position state under locks; any drift is ErrPlanStale.
type PerpIntent struct {
	Operation   string
	RequestHash string

	Order PerpOrder

	AllowRiskIncrease bool
	AllowedModes      []string

	// Delegation is non-nil when acting != owner: the grant is locked and
	// rechecked inside this transaction.
	Delegation *PerpIntentDelegation

	ExpectPositionID        string
	ExpectPositionContracts int64

	TriggeredOrderID string

	Open   *PerpIntentOpen
	Reduce *PerpIntentReduce
	Rest   *PerpIntentRest
	Shadow *PerpShadowObservation

	FinalStatus string
}

// PerpExecResult is what one executed intent produced.
type PerpExecResult struct {
	Order      PerpOrder
	FillID     string
	PositionID string
	EventSeqLo int64
	EventSeqHi int64
	Replayed   bool
	Response   []byte
}

type perpStoredResponse struct {
	OrderID    string `json:"order_id"`
	FillID     string `json:"fill_id,omitempty"`
	PositionID string `json:"position_id,omitempty"`
	Status     string `json:"status"`
	EventSeqLo int64  `json:"event_seq_lo,omitempty"`
	EventSeqHi int64  `json:"event_seq_hi,omitempty"`
}

// ExecutePerpIntent runs the engine transaction: claim the exactly-once
// identity, lock account -> market -> pool -> position -> triggered order in
// canonical order, recheck the durable market mode, apply every movement and
// row of the plan, journal the private events, store the idempotent response,
// and commit. A retry of the same request returns the original response
// without any second movement.
func (s *Store) ExecutePerpIntent(ctx context.Context, intent PerpIntent) (PerpExecResult, error) {
	if intent.Order.OwnerDID == "" || intent.Order.IdempotencyKey == "" {
		return PerpExecResult{}, fmt.Errorf("store: intent requires owner and idempotency key")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpExecResult{}, fmt.Errorf("store: begin intent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	owner := intent.Order.OwnerDID
	tag, err := tx.Exec(ctx, `
		INSERT INTO perp_idempotency (owner_did, idempotency_key, operation, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_did, idempotency_key) DO NOTHING`,
		owner, intent.Order.IdempotencyKey, intent.Operation, intent.RequestHash)
	if err != nil {
		return PerpExecResult{}, fmt.Errorf("store: claim intent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var storedOp, storedHash, status string
		var stored []byte
		err = tx.QueryRow(ctx, `
			SELECT operation, request_hash, status, COALESCE(response, 'null'::jsonb)
			FROM perp_idempotency WHERE owner_did = $1 AND idempotency_key = $2`,
			owner, intent.Order.IdempotencyKey).Scan(&storedOp, &storedHash, &status, &stored)
		if err != nil {
			return PerpExecResult{}, fmt.Errorf("store: read intent claim: %w", err)
		}
		if storedOp != intent.Operation || storedHash != intent.RequestHash {
			return PerpExecResult{}, ErrIdempotencyConflict
		}
		if status != "done" {
			return PerpExecResult{}, ErrIdempotencyInFlight
		}
		var resp perpStoredResponse
		if err := json.Unmarshal(stored, &resp); err != nil {
			return PerpExecResult{}, fmt.Errorf("store: decode stored response: %w", err)
		}
		order, err := scanPerpOrder(tx.QueryRow(ctx, perpOrderSelect+` WHERE id = $1`, resp.OrderID))
		if err != nil {
			return PerpExecResult{}, err
		}
		return PerpExecResult{
			Order: order, FillID: resp.FillID, PositionID: resp.PositionID,
			EventSeqLo: resp.EventSeqLo, EventSeqHi: resp.EventSeqHi,
			Replayed: true, Response: stored,
		}, nil
	}

	var balance int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, owner).Scan(&balance)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpExecResult{}, ErrNotFound
		}
		return PerpExecResult{}, fmt.Errorf("store: lock intent account: %w", err)
	}

	if intent.Delegation != nil {
		if err := s.checkDelegationTx(ctx, tx, owner, intent.Order.MarketSymbol, intent.Order.OrderType, intent.Delegation); err != nil {
			return PerpExecResult{}, err
		}
	}

	var marketMode string
	err = tx.QueryRow(ctx, `SELECT mode FROM perp_markets WHERE symbol = $1 FOR UPDATE`, intent.Order.MarketSymbol).Scan(&marketMode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpExecResult{}, ErrNotFound
		}
		return PerpExecResult{}, fmt.Errorf("store: lock intent market: %w", err)
	}
	modeOK := false
	for _, m := range intent.AllowedModes {
		if m == marketMode {
			modeOK = true
			break
		}
	}
	if !modeOK {
		return PerpExecResult{}, ErrModeDenied
	}

	var poolCapital int64
	if err := tx.QueryRow(ctx, `SELECT capital_usdx FROM perp_pools WHERE id = 'liquidity' FOR UPDATE`).Scan(&poolCapital); err != nil {
		return PerpExecResult{}, fmt.Errorf("store: lock intent pool: %w", err)
	}

	var position *PerpPosition
	p, err := scanPerpPosition(tx.QueryRow(ctx, perpPositionSelect+`
		WHERE owner_did = $1 AND market_symbol = $2 AND status IN ('OPEN','LIQUIDATING') FOR UPDATE`,
		owner, intent.Order.MarketSymbol))
	switch {
	case err == nil:
		position = &p
	case errors.Is(err, ErrNotFound):
	default:
		return PerpExecResult{}, err
	}
	if intent.ExpectPositionID == "" {
		if position != nil && intent.Open == nil && intent.Reduce != nil {
			return PerpExecResult{}, ErrPlanStale
		}
		if position != nil && intent.Open != nil {
			return PerpExecResult{}, ErrPlanStale
		}
	} else {
		if position == nil || position.ID != intent.ExpectPositionID || position.Contracts != intent.ExpectPositionContracts {
			return PerpExecResult{}, ErrPlanStale
		}
	}

	order := intent.Order
	var acceptSeq int64
	if intent.TriggeredOrderID != "" {
		claimed, err := scanPerpOrder(tx.QueryRow(ctx,
			perpOrderSelect+` WHERE id = $1 AND status = 'RESTING' FOR UPDATE SKIP LOCKED`, intent.TriggeredOrderID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return PerpExecResult{}, ErrOrderClaimed
			}
			return PerpExecResult{}, err
		}
		order = claimed
	} else {
		if order.Status == "" {
			order.Status = "RECEIVED"
		}
		inserted, seq, err := s.insertPerpOrderTx(ctx, tx, order)
		if err != nil {
			return PerpExecResult{}, err
		}
		order = inserted
		acceptSeq = seq
	}

	if intent.Shadow != nil {
		if intent.Open != nil || intent.Reduce != nil || intent.Rest != nil {
			return PerpExecResult{}, fmt.Errorf("store: shadow observation cannot carry an economic plan")
		}
		if order.Status != "SHADOW_RECORDED" {
			return PerpExecResult{}, fmt.Errorf("store: shadow observation requires SHADOW_RECORDED order")
		}
		o := intent.Shadow
		var positionCount, fillCount int64
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM perp_positions WHERE owner_did = $1),
			  (SELECT count(*) FROM perp_fills WHERE owner_did = $1)`, owner).
			Scan(&positionCount, &fillCount); err != nil {
			return PerpExecResult{}, fmt.Errorf("store: read shadow economic baseline: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO perp_shadow_observations (
				order_id, owner_did, acting_did, market_symbol, order_type, side, contracts,
				snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms,
				engine_execution_price_cents, reference_execution_price_cents,
				engine_margin_usdx, reference_margin_usdx, engine_fee_usdx, reference_fee_usdx,
				engine_funding_usdx, reference_funding_usdx,
				engine_liquidation_price_cents, reference_liquidation_price_cents,
				engine_pnl_usdx, reference_pnl_usdx, engine_error, reference_error,
				engine_error_code, reference_error_code,
				execution_tolerance_cents, mismatch_fields, feed_gap_detected,
				account_balance_before_usdx, account_balance_after_usdx,
				position_count_before, position_count_after, fill_count_before, fill_count_after,
				matched)
			VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
				$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37)`,
			order.ID, order.OwnerDID, order.ActingDID, order.MarketSymbol, order.OrderType,
			order.Side, order.Contracts, order.SnapshotID, order.OrderbookSeq, order.StatsSeq,
			order.SourceTimestampMs, o.EngineExecutionPriceCents, o.ReferenceExecutionPriceCents,
			o.EngineMarginMicro, o.ReferenceMarginMicro, o.EngineFeeMicro, o.ReferenceFeeMicro,
			o.EngineFundingMicro, o.ReferenceFundingMicro, o.EngineLiquidationPriceCents,
			o.ReferenceLiquidationPriceCents, o.EnginePnLMicro, o.ReferencePnLMicro,
			o.EngineError, o.ReferenceError, o.EngineErrorCode, o.ReferenceErrorCode,
			o.ExecutionToleranceCents, o.MismatchFields, o.FeedGapDetected,
			balance, balance, positionCount, positionCount, fillCount, fillCount,
			len(o.MismatchFields) == 0 && !o.FeedGapDetected); err != nil {
			return PerpExecResult{}, fmt.Errorf("store: insert shadow observation: %w", err)
		}
	}

	var eventLo, eventHi int64
	note := func(seq int64) {
		if eventLo == 0 {
			eventLo = seq
		}
		if seq > eventHi {
			eventHi = seq
		}
	}
	if acceptSeq > 0 {
		note(acceptSeq)
	}

	result := PerpExecResult{}
	switch {
	case intent.Open != nil:
		if !intent.AllowRiskIncrease {
			return PerpExecResult{}, ErrModeDenied
		}
		open := intent.Open
		cost := open.InitialMarginMicro + open.Fill.FeeMicro
		if open.FromReservationID != "" {
			r, err := scanPerpReservation(tx.QueryRow(ctx, perpReservationSelect+` WHERE id = $1 FOR UPDATE`, open.FromReservationID))
			if err != nil {
				return PerpExecResult{}, err
			}
			if r.Status != "held" || r.AmountMicro < cost {
				return PerpExecResult{}, ErrPlanStale
			}
			if _, err := tx.Exec(ctx, `UPDATE perp_margin_reservations SET status = 'applied', updated_at = now() WHERE id = $1`, open.FromReservationID); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: apply intent reservation: %w", err)
			}
			if excess := r.AmountMicro - cost; excess > 0 {
				if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, owner, excess); err != nil {
					return PerpExecResult{}, fmt.Errorf("store: return reservation excess: %w", err)
				}
			}
		} else {
			if balance < cost {
				return PerpExecResult{}, ErrInsufficientFunds
			}
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, owner, cost); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: debit intent margin: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE perp_pools SET capital_usdx = capital_usdx + $1, updated_at = now() WHERE id = 'liquidity'`, open.Fill.FeeMicro); err != nil {
			return PerpExecResult{}, fmt.Errorf("store: credit intent fee: %w", err)
		}
		if position == nil {
			np, err := scanPerpPosition(tx.QueryRow(ctx, `
				INSERT INTO perp_positions (
					owner_did, market_symbol, side, contracts, entry_price_cents, margin_usdx,
					snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms)
				VALUES ($1,$2,$3,$4,$5,$6, NULLIF($7,''), NULLIF($8::bigint,0), NULLIF($9::bigint,0), NULLIF($10::bigint,0))
				RETURNING id::text, owner_did, market_symbol, side, contracts, entry_price_cents,
				          margin_usdx, realized_pnl_usdx, unsettled_funding_usdx, status, opened_at, updated_at`,
				owner, order.MarketSymbol, open.Side, open.Fill.Contracts, open.NewEntryPriceCents,
				open.InitialMarginMicro, order.SnapshotID, order.OrderbookSeq, order.StatsSeq, order.SourceTimestampMs))
			if err != nil {
				return PerpExecResult{}, fmt.Errorf("store: insert intent position: %w", err)
			}
			position = &np
		} else {
			if position.Side != open.Side {
				return PerpExecResult{}, ErrPlanStale
			}
			if _, err := tx.Exec(ctx, `
				UPDATE perp_positions SET contracts = contracts + $2, margin_usdx = margin_usdx + $3,
				       entry_price_cents = $4, updated_at = now() WHERE id = $1`,
				position.ID, open.Fill.Contracts, open.InitialMarginMicro, open.NewEntryPriceCents); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: increase intent position: %w", err)
			}
		}
		result.PositionID = position.ID
		fillID, fillSeq, err := s.insertPerpFillTx(ctx, tx, order, position.ID, open.Fill, 0)
		if err != nil {
			return PerpExecResult{}, err
		}
		result.FillID = fillID
		note(fillSeq)
		seq, err := appendPerpEvent(ctx, tx, owner, order.ActingDID, "position.updated", map[string]any{
			"reason": "filled", "position_id": position.ID, "order_id": order.ID,
			"contracts": open.Fill.Contracts, "margin_usdx": open.InitialMarginMicro,
			"fee_usdx": open.Fill.FeeMicro,
		})
		if err != nil {
			return PerpExecResult{}, err
		}
		note(seq)

	case intent.Reduce != nil:
		reduce := intent.Reduce
		if position == nil {
			return PerpExecResult{}, ErrPlanStale
		}
		if reduce.CloseContracts <= 0 || reduce.CloseContracts > position.Contracts {
			return PerpExecResult{}, ErrPlanStale
		}
		margin := position.MarginMicro
		if margin < reduce.Fill.FeeMicro {
			return PerpExecResult{}, ErrMarginInsufficient
		}
		margin -= reduce.Fill.FeeMicro
		if reduce.RealizedPnLMicro > 0 && poolCapital+reduce.Fill.FeeMicro < reduce.RealizedPnLMicro {
			return PerpExecResult{}, ErrPoolInsufficient
		}
		if reduce.RealizedPnLMicro < 0 && margin < -reduce.RealizedPnLMicro {
			return PerpExecResult{}, ErrMarginInsufficient
		}
		margin += reduce.RealizedPnLMicro
		returnMicro := reduce.MarginReturnMicro
		if reduce.FullClose {
			returnMicro = margin
		} else if margin < returnMicro {
			return PerpExecResult{}, ErrMarginInsufficient
		}
		poolDelta := reduce.Fill.FeeMicro - reduce.RealizedPnLMicro
		if _, err := tx.Exec(ctx, `UPDATE perp_pools SET capital_usdx = capital_usdx + $1, updated_at = now() WHERE id = 'liquidity'`, poolDelta); err != nil {
			return PerpExecResult{}, fmt.Errorf("store: settle intent pool: %w", err)
		}
		if returnMicro > 0 {
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, owner, returnMicro); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: return intent margin: %w", err)
			}
		}
		if reduce.FullClose {
			if _, err := tx.Exec(ctx, `
				UPDATE perp_positions SET contracts = 0, margin_usdx = 0, status = 'CLOSED',
				       realized_pnl_usdx = realized_pnl_usdx + $2, closed_at = now(), updated_at = now()
				WHERE id = $1`, position.ID, reduce.RealizedPnLMicro); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: close intent position: %w", err)
			}
		} else {
			newMargin := margin - returnMicro
			if _, err := tx.Exec(ctx, `
				UPDATE perp_positions SET contracts = contracts - $2, margin_usdx = $3,
				       realized_pnl_usdx = realized_pnl_usdx + $4, updated_at = now()
				WHERE id = $1`, position.ID, reduce.CloseContracts, newMargin, reduce.RealizedPnLMicro); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: reduce intent position: %w", err)
			}
		}
		result.PositionID = position.ID
		fillID, fillSeq, err := s.insertPerpFillTx(ctx, tx, order, position.ID, reduce.Fill, reduce.RealizedPnLMicro)
		if err != nil {
			return PerpExecResult{}, err
		}
		result.FillID = fillID
		note(fillSeq)
		seq, err := appendPerpEvent(ctx, tx, owner, order.ActingDID, "position.updated", map[string]any{
			"reason": "reduced", "position_id": position.ID, "order_id": order.ID,
			"closed_contracts": reduce.CloseContracts, "realized_pnl_usdx": reduce.RealizedPnLMicro,
			"margin_returned_usdx": returnMicro, "fee_usdx": reduce.Fill.FeeMicro,
			"closed": reduce.FullClose,
		})
		if err != nil {
			return PerpExecResult{}, err
		}
		note(seq)

	case intent.Rest != nil:
		if !intent.AllowRiskIncrease && !order.ReduceOnly {
			return PerpExecResult{}, ErrModeDenied
		}
		if intent.Rest.ReserveMicro > 0 {
			if balance < intent.Rest.ReserveMicro {
				return PerpExecResult{}, ErrInsufficientFunds
			}
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, owner, intent.Rest.ReserveMicro); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: debit intent reservation: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO perp_margin_reservations (owner_did, order_id, amount_usdx)
				VALUES ($1, $2, $3)`, owner, order.ID, intent.Rest.ReserveMicro); err != nil {
				return PerpExecResult{}, fmt.Errorf("store: insert intent reservation: %w", err)
			}
			seq, err := appendPerpEvent(ctx, tx, owner, order.ActingDID, "balance.updated", map[string]any{
				"reason": "margin.reserved", "order_id": order.ID, "amount_usdx": intent.Rest.ReserveMicro,
			})
			if err != nil {
				return PerpExecResult{}, err
			}
			note(seq)
		}
	}

	if intent.FinalStatus != "" && intent.FinalStatus != order.Status {
		if _, err := tx.Exec(ctx, `UPDATE perp_orders SET status = $2, filled_contracts = filled_contracts + $3, updated_at = now() WHERE id = $1`,
			order.ID, intent.FinalStatus, filledContracts(intent)); err != nil {
			return PerpExecResult{}, fmt.Errorf("store: finalize intent order: %w", err)
		}
		order.Status = intent.FinalStatus
		order.FilledContracts += filledContracts(intent)
		seq, err := appendPerpEvent(ctx, tx, owner, order.ActingDID, "order.updated", map[string]any{
			"order_id": order.ID, "to": intent.FinalStatus,
		})
		if err != nil {
			return PerpExecResult{}, err
		}
		note(seq)
	}

	result.Order = order
	result.EventSeqLo = eventLo
	result.EventSeqHi = eventHi
	resp, err := json.Marshal(perpStoredResponse{
		OrderID: order.ID, FillID: result.FillID, PositionID: result.PositionID,
		Status: order.Status, EventSeqLo: eventLo, EventSeqHi: eventHi,
	})
	if err != nil {
		return PerpExecResult{}, fmt.Errorf("store: encode intent response: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_idempotency SET status = 'done', response = $3, updated_at = now()
		WHERE owner_did = $1 AND idempotency_key = $2`, owner, intent.Order.IdempotencyKey, resp); err != nil {
		return PerpExecResult{}, fmt.Errorf("store: store intent response: %w", err)
	}
	result.Response = resp
	if err := tx.Commit(ctx); err != nil {
		return PerpExecResult{}, fmt.Errorf("store: commit intent: %w", err)
	}
	return result, nil
}

// checkDelegationTx locks the ACTIVE grant from owner to delegate and
// re-verifies every bound inside the caller's transaction (canonical lock
// order: account -> delegation -> market -> pool -> position). An expired
// grant is transitioned to EXPIRED here and rejected. Reduce/cancel intents
// only require the live grant; risk increase enforces tier, market, order
// type, order/position notional, leverage, and both owner-timezone daily
// windows against the committed fills of the day.
func (s *Store) checkDelegationTx(ctx context.Context, tx pgx.Tx, owner, symbol, orderType string, d *PerpIntentDelegation) error {
	row, err := scanPerpDelegation(tx.QueryRow(ctx, perpDelegationSelect+`
		WHERE owner_did = $1 AND delegate_did = $2 AND status = 'ACTIVE' FOR UPDATE`,
		owner, d.DelegateDID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrDelegationRequired
		}
		return err
	}
	if !row.ExpiresAt.After(time.Now()) {
		// The rejection rolls this transaction back, so the durable EXPIRED
		// transition happens in the ExpirePerpDelegations sweep, not here.
		return ErrDelegationRequired
	}
	if !d.RiskIncrease {
		return nil
	}
	if d.GrantTierRank == nil || d.AssertedTierRank <= 0 ||
		d.AssertedTierRank < d.GrantTierRank(row.MembershipTier) {
		return ErrMembershipRequired
	}
	marketAllowed := false
	for _, m := range row.AllowedMarkets {
		if m == symbol {
			marketAllowed = true
			break
		}
	}
	if !marketAllowed {
		return ErrDelegationLimit
	}
	typeAllowed := false
	for _, ot := range row.AllowedOrderTypes {
		if ot == orderType {
			typeAllowed = true
			break
		}
	}
	if !typeAllowed {
		return ErrDelegationLimit
	}
	if d.OrderNotionalMicro > row.MaxOrderNotionalMicro {
		return ErrDelegationLimit
	}
	if d.ProjectedPositionNotionalMicro > row.MaxPositionNotionalMicro {
		return ErrDelegationLimit
	}
	if row.MaxLeverageX > 0 && d.ProjectedMarginMicro > 0 {
		if d.ProjectedPositionNotionalMicro > row.MaxLeverageX*d.ProjectedMarginMicro {
			return ErrDelegationLimit
		}
	}
	var dayNotional, dayLoss int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(notional_usdx), 0), COALESCE(-SUM(LEAST(realized_pnl_usdx, 0)), 0)
		FROM perp_fills
		WHERE owner_did = $1 AND acting_did = $2 AND created_at >= $3`,
		owner, d.DelegateDID, d.DayStartUTC).Scan(&dayNotional, &dayLoss); err != nil {
		return fmt.Errorf("store: delegation daily counters: %w", err)
	}
	if row.MaxDailyNotionalMicro > 0 && dayNotional+d.OrderNotionalMicro > row.MaxDailyNotionalMicro {
		return ErrDelegationLimit
	}
	if row.MaxDailyRealizedLossMicro > 0 && dayLoss >= row.MaxDailyRealizedLossMicro {
		return ErrDelegationLimit
	}
	return nil
}

func filledContracts(intent PerpIntent) int64 {
	switch {
	case intent.Open != nil:
		return intent.Open.Fill.Contracts
	case intent.Reduce != nil:
		return intent.Reduce.Fill.Contracts
	default:
		return 0
	}
}

func (s *Store) insertPerpFillTx(ctx context.Context, tx pgx.Tx, order PerpOrder, positionID string,
	fill PerpIntentFill, realizedPnL int64) (string, int64, error) {

	if order.SnapshotID == "" || order.OrderbookSeq <= 0 || order.StatsSeq <= 0 || order.SourceTimestampMs <= 0 {
		return "", 0, fmt.Errorf("store: intent fill requires a complete snapshot reference")
	}
	var fillID string
	err := tx.QueryRow(ctx, `
		INSERT INTO perp_fills (
			order_id, position_id, owner_did, acting_did, market_symbol, side, contracts,
			price_cents, notional_usdx, fee_usdx, realized_pnl_usdx, maker, liquidation,
			snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING id::text`,
		order.ID, positionID, order.OwnerDID, order.ActingDID, order.MarketSymbol, order.Side,
		fill.Contracts, fill.PriceCents, fill.NotionalMicro, fill.FeeMicro, realizedPnL,
		fill.Maker, fill.Liquidation,
		order.SnapshotID, order.OrderbookSeq, order.StatsSeq, order.SourceTimestampMs).Scan(&fillID)
	if err != nil {
		return "", 0, fmt.Errorf("store: insert intent fill: %w", err)
	}
	seq, err := appendPerpEvent(ctx, tx, order.OwnerDID, order.ActingDID, "fill.created", map[string]any{
		"fill_id": fillID, "order_id": order.ID, "position_id": positionID,
		"symbol": order.MarketSymbol, "side": order.Side, "contracts": fill.Contracts,
		"price_cents": fill.PriceCents, "liquidation": fill.Liquidation,
	})
	if err != nil {
		return "", 0, err
	}
	return fillID, seq, nil
}
