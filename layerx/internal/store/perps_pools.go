package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrPoolInsufficient is returned when a conserved transfer would drive a
// protocol capital bucket negative. Callers treat it as an insolvency signal,
// never as permission to write an unbacked credit.
var ErrPoolInsufficient = errors.New("store: pool capital is insufficient")

// PerpPoolDID returns the ledger identity a pool's funding transfers are
// addressed to, so pool capital movements are ordinary journaled transfers.
func PerpPoolDID(pool string) string {
	return "did:layerx:perps:pool:" + pool
}

func validPerpPool(pool string) error {
	if pool != "liquidity" && pool != "insurance" {
		return fmt.Errorf("store: perp pool %q is unknown", pool)
	}
	return nil
}

func (s *Store) LookupPerpPoolFundingIntent(ctx context.Context, ownerDID,
	idempotencyKey string) (requestHash string, transferSeq int64, found bool, err error) {
	var seq *int64
	err = s.pool.QueryRow(ctx, `
		SELECT request_hash,transfer_seq
		FROM perp_pool_funding_intents
		WHERE owner_did=$1 AND idempotency_key=$2`,
		ownerDID, idempotencyKey).Scan(&requestHash, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("store: lookup pool funding intent: %w", err)
	}
	if seq != nil {
		transferSeq = *seq
	}
	return requestHash, transferSeq, true, nil
}

// PerpPoolBalances is the current protocol capital by bucket.
type PerpPoolBalances struct {
	LiquidityMicroUSDX int64
	InsuranceMicroUSDX int64
}

// PerpPoolCapital returns both capital buckets.
func (s *Store) PerpPoolCapital(ctx context.Context) (PerpPoolBalances, error) {
	var b PerpPoolBalances
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(capital_usdx) FILTER (WHERE id = 'liquidity'), 0),
		       COALESCE(SUM(capital_usdx) FILTER (WHERE id = 'insurance'), 0)
		FROM perp_pools`).Scan(&b.LiquidityMicroUSDX, &b.InsuranceMicroUSDX); err != nil {
		return PerpPoolBalances{}, fmt.Errorf("store: perp pool capital: %w", err)
	}
	return b, nil
}

// FundPerpPool moves value from a DID's spendable balance into a typed capital
// bucket as an ORDINARY transfer: the payer is locked and bound by spendable
// balance, a standard transfers row (Merkle leaf + sequencer sig via finalize)
// is emitted to the pool's ledger DID, and the funding event is journaled — all
// in one transaction. This is the ONLY way protocol capital increases.
func (s *Store) FundPerpPool(ctx context.Context, fromDID, pool string, amountMicro int64, tier string,
	finalize func(seq int64, ts time.Time) (leafHex, sigHex string)) (PayResult, error) {
	return s.fundPerpPool(ctx, fromDID, pool, amountMicro, tier, "", "", finalize)
}

// FundPerpPoolIdempotent is the response-loss-safe public funding path. The
// owner-scoped key claims exactly one request hash and replays its original
// transfer without a second debit.
func (s *Store) FundPerpPoolIdempotent(ctx context.Context, fromDID, pool string,
	amountMicro int64, tier, idempotencyKey, requestHash string,
	finalize func(seq int64, ts time.Time) (leafHex, sigHex string)) (PayResult, error) {
	if idempotencyKey == "" || requestHash == "" {
		return PayResult{}, fmt.Errorf("store: pool funding idempotency key and request hash are required")
	}
	return s.fundPerpPool(ctx, fromDID, pool, amountMicro, tier,
		idempotencyKey, requestHash, finalize)
}

func (s *Store) fundPerpPool(ctx context.Context, fromDID, pool string, amountMicro int64,
	tier, idempotencyKey, requestHash string,
	finalize func(seq int64, ts time.Time) (leafHex, sigHex string)) (PayResult, error) {
	if err := validPerpPool(pool); err != nil {
		return PayResult{}, err
	}
	if amountMicro <= 0 {
		return PayResult{}, fmt.Errorf("store: pool funding amount must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PayResult{}, fmt.Errorf("store: begin pool funding: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if idempotencyKey != "" {
		tag, err := tx.Exec(ctx, `
			INSERT INTO perp_pool_funding_intents (owner_did,idempotency_key,request_hash)
			VALUES ($1,$2,$3)
			ON CONFLICT (owner_did,idempotency_key) DO NOTHING`,
			fromDID, idempotencyKey, requestHash)
		if err != nil {
			return PayResult{}, fmt.Errorf("store: claim pool funding intent: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var storedHash string
			var seq *int64
			if err := tx.QueryRow(ctx, `
				SELECT request_hash,transfer_seq
				FROM perp_pool_funding_intents
				WHERE owner_did=$1 AND idempotency_key=$2
				FOR UPDATE`, fromDID, idempotencyKey).Scan(&storedHash, &seq); err != nil {
				return PayResult{}, fmt.Errorf("store: read pool funding replay: %w", err)
			}
			if storedHash != requestHash {
				return PayResult{}, ErrIdempotencyConflict
			}
			if seq == nil {
				return PayResult{}, ErrIdempotencyInFlight
			}
			var replay PayResult
			if err := tx.QueryRow(ctx, `
				SELECT seq,ts,leaf_hash,sig,tier FROM transfers WHERE seq=$1`, *seq).
				Scan(&replay.Seq, &replay.TS, &replay.LeafHex, &replay.SigHex, &replay.Tier); err != nil {
				return PayResult{}, fmt.Errorf("store: read pool funding transfer: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return PayResult{}, fmt.Errorf("store: commit pool funding replay: %w", err)
			}
			replay.Replayed = true
			return replay, nil
		}
	}

	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, fromDID).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PayResult{}, ErrNotFound
		}
		return PayResult{}, fmt.Errorf("store: lock pool funder: %w", err)
	}
	if bal < amountMicro {
		return PayResult{}, ErrInsufficientFunds
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, fromDID, amountMicro); err != nil {
		return PayResult{}, fmt.Errorf("store: debit pool funder: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_pools SET capital_usdx = capital_usdx + $2, updated_at = now() WHERE id = $1`, pool, amountMicro); err != nil {
		return PayResult{}, fmt.Errorf("store: credit pool: %w", err)
	}

	var seq int64
	if err := tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('transfers', 'seq'))`).Scan(&seq); err != nil {
		return PayResult{}, fmt.Errorf("store: allocate pool funding seq: %w", err)
	}
	ts := time.Now().UTC()
	leafHex, sigHex := finalize(seq, ts)
	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (seq, from_did, to_did, amount_usdx, tier, leaf_hash, sig, ref, ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		seq, fromDID, PerpPoolDID(pool), amountMicro, tier, leafHex, sigHex, "perps.pool."+pool, ts); err != nil {
		return PayResult{}, fmt.Errorf("store: insert pool funding transfer: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, fromDID, fromDID, "pool.funded", map[string]any{
		"pool": pool, "amount_usdx": amountMicro, "transfer_seq": seq,
	}); err != nil {
		return PayResult{}, err
	}
	if idempotencyKey != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE perp_pool_funding_intents SET transfer_seq=$3
			WHERE owner_did=$1 AND idempotency_key=$2`,
			fromDID, idempotencyKey, seq); err != nil {
			return PayResult{}, fmt.Errorf("store: complete pool funding intent: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PayResult{}, fmt.Errorf("store: commit pool funding: %w", err)
	}
	return PayResult{Seq: seq, TS: ts, LeafHex: leafHex, SigHex: sigHex, Tier: tier}, nil
}
