package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// InvocationRow is a ledger entry for one invoke.
type InvocationRow struct {
	ID             string
	IdempotencyKey string
	ServiceID      string
	EndpointID     string
	CallerDID      string
	CallerWallet   string
	QuoteID        *string
	Units          string
	PriceWei       string
	PricingVersion int
	ArgsHash       string
	ResultHash     string
	Outcome        string
	LatencyMS      *int
	Rail           string
	ChannelID      *string
	CreatedAt      time.Time
}

// InsertReservedInvocation creates a reserved ledger row (idempotent).
func (s *Store) InsertReservedInvocation(ctx context.Context, row InvocationRow) (string, error) {
	var id string
	rail := row.Rail
	if rail == "" {
		rail = "direct"
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO invocations (
			idempotency_key, service_id, endpoint_id, caller_did, caller_wallet,
			quote_id, units, price_wei, pricing_version, args_hash, outcome,
			rail, channel_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reserved',$11,$12)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text`,
		row.IdempotencyKey, row.ServiceID, row.EndpointID, row.CallerDID, row.CallerWallet,
		row.QuoteID, row.Units, row.PriceWei, row.PricingVersion, row.ArgsHash,
		rail, row.ChannelID,
	).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			existing, gerr := s.GetInvocationByIdempotency(ctx, row.IdempotencyKey)
			if gerr != nil {
				return "", gerr
			}
			return existing.ID, nil
		}
		return "", fmt.Errorf("store: reserve invocation: %w", err)
	}
	return id, nil
}

// GetInvocationByIdempotency loads by idempotency key.
func (s *Store) GetInvocationByIdempotency(ctx context.Context, key string) (InvocationRow, error) {
	return s.scanInvocation(ctx, `idempotency_key = $1`, key)
}

// GetInvocation loads by id.
func (s *Store) GetInvocation(ctx context.Context, id string) (InvocationRow, error) {
	return s.scanInvocation(ctx, `id = $1`, id)
}

func (s *Store) scanInvocation(ctx context.Context, where string, arg any) (InvocationRow, error) {
	var row InvocationRow
	var quoteID *string
	q := fmt.Sprintf(`
		SELECT id::text, idempotency_key, service_id::text, endpoint_id::text,
		       caller_did, COALESCE(caller_wallet,''), quote_id::text, units, price_wei,
		       pricing_version, COALESCE(args_hash,''), COALESCE(result_hash,''),
		       outcome, latency_ms, COALESCE(rail,'direct'), channel_id::text, created_at
		FROM invocations WHERE %s`, where)
	err := s.pool.QueryRow(ctx, q, arg).Scan(
		&row.ID, &row.IdempotencyKey, &row.ServiceID, &row.EndpointID,
		&row.CallerDID, &row.CallerWallet, &quoteID, &row.Units, &row.PriceWei,
		&row.PricingVersion, &row.ArgsHash, &row.ResultHash, &row.Outcome,
		&row.LatencyMS, &row.Rail, &row.ChannelID, &row.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return InvocationRow{}, fmt.Errorf("store: invocation not found")
		}
		return InvocationRow{}, fmt.Errorf("store: get invocation: %w", err)
	}
	row.QuoteID = quoteID
	return row, nil
}

// FinalizeInvocation marks success with result hash and charge.
func (s *Store) FinalizeInvocation(ctx context.Context, id, outcome, resultHash, units, priceWei string, latencyMS int) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE invocations SET
			outcome = $2, result_hash = $3, units = $4, price_wei = $5, latency_ms = $6
		WHERE id = $1 AND outcome = 'reserved'`, id, outcome, resultHash, units, priceWei, latencyMS)
	if err != nil {
		return fmt.Errorf("store: finalize invocation: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: invocation not in reserved state")
	}
	return nil
}

// VoidInvocation marks a reserved row voided (no charge).
func (s *Store) VoidInvocation(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE invocations SET outcome = 'voided', price_wei = '0'
		WHERE id = $1 AND outcome = 'reserved'`, id)
	if err != nil {
		return fmt.Errorf("store: void invocation: %w", err)
	}
	return nil
}
