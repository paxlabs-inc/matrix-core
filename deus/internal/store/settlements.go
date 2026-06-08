package store

import (
	"context"
	"fmt"
	"time"
)

// SettlementRow is a batched payout window.
type SettlementRow struct {
	ID              string
	DeveloperID     string
	Rail            string
	TotalWei        string
	InvocationCount int
	MerkleRoot      string
	TxHash          *string
	WindowStart     time.Time
	WindowEnd       time.Time
	Status          string
}

// InsertSettlement creates a pending settlement batch.
func (s *Store) InsertSettlement(ctx context.Context, row SettlementRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO settlements (
			developer_id, rail, total_wei, invocation_count, merkle_root,
			window_start, window_end, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')
		RETURNING id::text`,
		row.DeveloperID, row.Rail, row.TotalWei, row.InvocationCount, row.MerkleRoot,
		row.WindowStart, row.WindowEnd,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert settlement: %w", err)
	}
	return id, nil
}

// UnsettledInvocations lists finalized unsettled rows for a developer.
func (s *Store) UnsettledInvocations(ctx context.Context, developerID string) ([]InvocationRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id::text, i.idempotency_key, i.service_id::text, i.endpoint_id::text,
		       i.caller_did, COALESCE(i.caller_wallet,''), i.quote_id::text, i.units, i.price_wei,
		       i.pricing_version, COALESCE(i.args_hash,''), COALESCE(i.result_hash,''),
		       i.outcome, i.latency_ms, i.created_at
		FROM invocations i
		JOIN services s ON s.id = i.service_id
		WHERE s.developer_id = $1
		  AND i.outcome = 'ok'
		  AND i.settlement_id IS NULL
		ORDER BY i.created_at ASC`, developerID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: unsettled invocations: %w", err)
	}
	defer rows.Close()
	var out []InvocationRow
	for rows.Next() {
		var row InvocationRow
		var quoteID *string
		if err := rows.Scan(
			&row.ID, &row.IdempotencyKey, &row.ServiceID, &row.EndpointID,
			&row.CallerDID, &row.CallerWallet, &quoteID, &row.Units, &row.PriceWei,
			&row.PricingVersion, &row.ArgsHash, &row.ResultHash, &row.Outcome,
			&row.LatencyMS, &row.CreatedAt,
		); err != nil {
			return nil, err
		}
		row.QuoteID = quoteID
		out = append(out, row)
	}
	return out, rows.Err()
}

// UnsettledNetInvocations lists finalized unsettled NET-rail rows for a
// developer, joined to their funding channel. Direct/hosted rows are paid inline
// and stream rows settle on 0x0906, so only the net rail batches here. Rows are
// ordered by channel then time so the settler can walk each channel's spend in
// voucher order.
func (s *Store) UnsettledNetInvocations(ctx context.Context, developerID string) ([]InvocationRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id::text, i.idempotency_key, i.service_id::text, i.endpoint_id::text,
		       i.caller_did, COALESCE(i.caller_wallet,''), i.quote_id::text, i.units, i.price_wei,
		       i.pricing_version, COALESCE(i.args_hash,''), COALESCE(i.result_hash,''),
		       i.outcome, i.latency_ms, COALESCE(i.rail,'direct'), i.channel_id::text, i.created_at
		FROM invocations i
		JOIN services s ON s.id = i.service_id
		WHERE s.developer_id = $1
		  AND i.outcome = 'ok'
		  AND i.rail = 'net'
		  AND i.channel_id IS NOT NULL
		  AND i.settlement_id IS NULL
		ORDER BY i.channel_id, i.created_at ASC`, developerID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: unsettled net invocations: %w", err)
	}
	defer rows.Close()
	var out []InvocationRow
	for rows.Next() {
		var row InvocationRow
		var quoteID *string
		if err := rows.Scan(
			&row.ID, &row.IdempotencyKey, &row.ServiceID, &row.EndpointID,
			&row.CallerDID, &row.CallerWallet, &quoteID, &row.Units, &row.PriceWei,
			&row.PricingVersion, &row.ArgsHash, &row.ResultHash, &row.Outcome,
			&row.LatencyMS, &row.Rail, &row.ChannelID, &row.CreatedAt,
		); err != nil {
			return nil, err
		}
		row.QuoteID = quoteID
		out = append(out, row)
	}
	return out, rows.Err()
}

// MarkVoucherRedeemed stamps the settlement that redeemed a voucher.
func (s *Store) MarkVoucherRedeemed(ctx context.Context, voucherID, settlementID string) error {
	if voucherID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE vouchers SET redeemed_in = $2 WHERE id = $1`, voucherID, settlementID,
	)
	if err != nil {
		return fmt.Errorf("store: mark voucher redeemed: %w", err)
	}
	return nil
}

// MarkInvocationsSettled attaches settlement_id to invocation rows.
func (s *Store) MarkInvocationsSettled(ctx context.Context, settlementID string, invocationIDs []string) error {
	if len(invocationIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE invocations SET settlement_id = $1
		WHERE id = ANY($2::uuid[])`, settlementID, invocationIDs,
	)
	if err != nil {
		return fmt.Errorf("store: mark settled: %w", err)
	}
	return nil
}

// CompleteSettlement marks settlement done with tx hash.
func (s *Store) CompleteSettlement(ctx context.Context, settlementID, txHash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE settlements SET status = 'completed', tx_hash = $2 WHERE id = $1`,
		settlementID, txHash,
	)
	return err
}

// ReceiptDigestsForInvocations returns receipt digests in invocation order.
func (s *Store) ReceiptDigestsForInvocations(ctx context.Context, ids []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT eip712_digest FROM receipts WHERE invocation_id = ANY($1::uuid[])
		ORDER BY invocation_id`, ids,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
