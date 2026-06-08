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
	PaymentRail    string
	LatencyMS      *int
	CreatedAt      time.Time
}

// InsertReservedInvocation creates a reserved ledger row (idempotent).
func (s *Store) InsertReservedInvocation(ctx context.Context, row InvocationRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO invocations (
			idempotency_key, service_id, endpoint_id, caller_did, caller_wallet,
			quote_id, units, price_wei, pricing_version, args_hash, outcome, payment_rail
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reserved','direct')
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text`,
		row.IdempotencyKey, row.ServiceID, row.EndpointID, row.CallerDID, row.CallerWallet,
		row.QuoteID, row.Units, row.PriceWei, row.PricingVersion, row.ArgsHash,
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
		       outcome, COALESCE(payment_rail,'direct'), latency_ms, created_at
		FROM invocations WHERE %s`, where)
	err := s.pool.QueryRow(ctx, q, arg).Scan(
		&row.ID, &row.IdempotencyKey, &row.ServiceID, &row.EndpointID,
		&row.CallerDID, &row.CallerWallet, &quoteID, &row.Units, &row.PriceWei,
		&row.PricingVersion, &row.ArgsHash, &row.ResultHash, &row.Outcome,
		&row.PaymentRail, &row.LatencyMS, &row.CreatedAt,
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
func (s *Store) FinalizeInvocation(ctx context.Context, id, outcome, resultHash, units, priceWei, paymentRail string, latencyMS int) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE invocations SET
			outcome = $2, result_hash = $3, units = $4, price_wei = $5, latency_ms = $6, payment_rail = $7
		WHERE id = $1 AND outcome = 'reserved'`, id, outcome, resultHash, units, priceWei, latencyMS, paymentRail)
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
		WHERE id = $1 AND outcome IN ('reserved', 'pending_voucher')`, id)
	if err != nil {
		return fmt.Errorf("store: void invocation: %w", err)
	}
	return nil
}

// FinalizePendingVoucher marks delivery awaiting caller voucher co-sign (net rail async path).
func (s *Store) FinalizePendingVoucher(ctx context.Context, id, resultHash, units, priceWei string, latencyMS int) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE invocations SET
			outcome = 'pending_voucher', result_hash = $2, units = $3, price_wei = $4, latency_ms = $5,
			payment_rail = 'net'
		WHERE id = $1 AND outcome = 'reserved'`, id, resultHash, units, priceWei, latencyMS)
	if err != nil {
		return fmt.Errorf("store: pending voucher invocation: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: invocation not in reserved state")
	}
	return nil
}

// CompletePendingVoucher promotes pending_voucher → ok after voucher co-sign.
func (s *Store) CompletePendingVoucher(ctx context.Context, id string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE invocations SET outcome = 'ok', payment_rail = 'net'
		WHERE id = $1 AND outcome = 'pending_voucher'`, id)
	if err != nil {
		return fmt.Errorf("store: complete pending voucher: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: invocation not pending voucher")
	}
	return nil
}

// PendingInvocationByReceiptDigest finds a net-rail row awaiting voucher co-sign.
func (s *Store) PendingInvocationByReceiptDigest(ctx context.Context, callerDID, receiptDigest string) (InvocationRow, error) {
	var row InvocationRow
	var quoteID *string
	err := s.pool.QueryRow(ctx, `
		SELECT i.id::text, i.idempotency_key, i.service_id::text, i.endpoint_id::text,
		       i.caller_did, COALESCE(i.caller_wallet,''), i.quote_id::text, i.units, i.price_wei,
		       i.pricing_version, COALESCE(i.args_hash,''), COALESCE(i.result_hash,''),
		       i.outcome, COALESCE(i.payment_rail,'direct'), i.latency_ms, i.created_at
		FROM invocations i
		JOIN receipts r ON r.invocation_id = i.id
		WHERE i.caller_did = $1 AND r.eip712_digest = $2 AND i.outcome = 'pending_voucher'`,
		callerDID, receiptDigest,
	).Scan(
		&row.ID, &row.IdempotencyKey, &row.ServiceID, &row.EndpointID,
		&row.CallerDID, &row.CallerWallet, &quoteID, &row.Units, &row.PriceWei,
		&row.PricingVersion, &row.ArgsHash, &row.ResultHash, &row.Outcome,
		&row.PaymentRail, &row.LatencyMS, &row.CreatedAt,
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

// VoidPendingVouchersForChannel voids stale pending_voucher rows and returns released charge wei per row.
func (s *Store) VoidPendingVouchersForChannel(ctx context.Context, callerDID string) ([]InvocationRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, idempotency_key, service_id::text, endpoint_id::text,
		       caller_did, COALESCE(caller_wallet,''), quote_id::text, units, price_wei,
		       pricing_version, COALESCE(args_hash,''), COALESCE(result_hash,''),
		       outcome, COALESCE(payment_rail,'net'), latency_ms, created_at
		FROM invocations
		WHERE caller_did = $1 AND outcome = 'pending_voucher'`, callerDID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list pending voucher: %w", err)
	}
	defer rows.Close()
	var pending []InvocationRow
	for rows.Next() {
		var row InvocationRow
		var quoteID *string
		if err := rows.Scan(
			&row.ID, &row.IdempotencyKey, &row.ServiceID, &row.EndpointID,
			&row.CallerDID, &row.CallerWallet, &quoteID, &row.Units, &row.PriceWei,
			&row.PricingVersion, &row.ArgsHash, &row.ResultHash, &row.Outcome,
			&row.PaymentRail, &row.LatencyMS, &row.CreatedAt,
		); err != nil {
			return nil, err
		}
		row.QuoteID = quoteID
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range pending {
		if err := s.VoidInvocation(ctx, pending[i].ID); err != nil {
			return nil, err
		}
	}
	return pending, nil
}
