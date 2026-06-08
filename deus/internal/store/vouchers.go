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
