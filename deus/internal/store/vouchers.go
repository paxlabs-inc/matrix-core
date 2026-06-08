package store

import (
	"context"
	"fmt"
	"time"
)

// VoucherRow is a persisted caller-co-signed cumulative voucher.
type VoucherRow struct {
	ID              string
	ChannelID       string
	CumulativeWei   string
	Nonce           int64
	LastReceiptHash string
	Digest          string
	CallerSig       string
	RedeemedIn      *string
	CreatedAt       time.Time
}

// CosignVoucherAtomic finalizes channel charge and inserts voucher in one transaction.
func (s *Store) CosignVoucherAtomic(ctx context.Context, channelID, chargeWei string, nonce int64, cumulativeWei, voucherSig string, v VoucherRow) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin cosign tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx, `
		UPDATE channels SET
			reserved_wei = GREATEST((reserved_wei::numeric - $2::numeric), 0)::text,
			cumulative_wei = $3,
			nonce = $4,
			last_voucher_sig = $5
		WHERE id = $1`, channelID, chargeWei, cumulativeWei, nonce, voucherSig,
	)
	if err != nil {
		return "", fmt.Errorf("store: finalize channel in tx: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return "", fmt.Errorf("store: channel not found")
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO vouchers (
			channel_id, cumulative_wei, nonce, last_receipt_hash, eip712_digest, caller_sig
		) VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text`,
		v.ChannelID, v.CumulativeWei, v.Nonce, v.LastReceiptHash, v.Digest, v.CallerSig,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert voucher in tx: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit cosign tx: %w", err)
	}
	return id, nil
}

// MarkVoucherRedeemed records settlement redemption for a voucher.
func (s *Store) MarkVoucherRedeemed(ctx context.Context, voucherID, settlementID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE vouchers SET redeemed_in = $2::uuid WHERE id = $1`, voucherID, settlementID,
	)
	if err != nil {
		return fmt.Errorf("store: mark voucher redeemed: %w", err)
	}
	return nil
}

// InsertVoucher stores a co-signed voucher.
func (s *Store) InsertVoucher(ctx context.Context, v VoucherRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO vouchers (
			channel_id, cumulative_wei, nonce, last_receipt_hash, eip712_digest, caller_sig
		) VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text`,
		v.ChannelID, v.CumulativeWei, v.Nonce, v.LastReceiptHash, v.Digest, v.CallerSig,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert voucher: %w", err)
	}
	return id, nil
}

// HighestVoucherForChannel returns the voucher with max nonce for settlement.
func (s *Store) HighestVoucherForChannel(ctx context.Context, channelID string) (VoucherRow, error) {
	var v VoucherRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, channel_id::text, cumulative_wei, nonce, last_receipt_hash,
		       eip712_digest, caller_sig, redeemed_in::text, created_at
		FROM vouchers
		WHERE channel_id = $1
		ORDER BY nonce DESC
		LIMIT 1`, channelID,
	).Scan(
		&v.ID, &v.ChannelID, &v.CumulativeWei, &v.Nonce, &v.LastReceiptHash,
		&v.Digest, &v.CallerSig, &v.RedeemedIn, &v.CreatedAt,
	)
	if err != nil {
		return VoucherRow{}, fmt.Errorf("store: highest voucher: %w", err)
	}
	return v, nil
}
