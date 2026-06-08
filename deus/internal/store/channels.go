package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChannelRow is a per-caller payment channel mirror.
type ChannelRow struct {
	ID             string
	CallerDID      string
	CallerWallet   string
	EscrowAddr     string
	BalanceWei     string
	ReservedWei    string
	CumulativeWei  string
	RedeemedWei    string
	Nonce          int64
	LastVoucherSig *string
	WindowStart    time.Time
	WindowEnd      time.Time
	Status         string
	FundTx         *string
	SettleTx       *string
}

// OpenChannel inserts a new open channel row.
func (s *Store) OpenChannel(ctx context.Context, row ChannelRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO channels (
			caller_did, caller_wallet, escrow_addr, balance_wei, reserved_wei,
			cumulative_wei, nonce, window_start, window_end, status, fund_tx
		) VALUES ($1,$2,$3,$4,'0','0',0,$5,$6,'open',$7)
		RETURNING id::text`,
		row.CallerDID, row.CallerWallet, row.EscrowAddr, row.BalanceWei,
		row.WindowStart, row.WindowEnd, row.FundTx,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: open channel: %w", err)
	}
	return id, nil
}

// ActiveChannelForCaller returns an open channel for the caller in the current window.
func (s *Store) ActiveChannelForCaller(ctx context.Context, callerDID string) (ChannelRow, error) {
	var row ChannelRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, caller_did, caller_wallet, escrow_addr, balance_wei, reserved_wei,
		       cumulative_wei, COALESCE(redeemed_wei,'0'), nonce::bigint, last_voucher_sig,
		       window_start, window_end, status, fund_tx, settle_tx
		FROM channels
		WHERE caller_did = $1 AND status = 'open' AND window_end > now()
		ORDER BY window_start DESC
		LIMIT 1`, callerDID,
	).Scan(
		&row.ID, &row.CallerDID, &row.CallerWallet, &row.EscrowAddr, &row.BalanceWei,
		&row.ReservedWei, &row.CumulativeWei, &row.RedeemedWei, &row.Nonce, &row.LastVoucherSig,
		&row.WindowStart, &row.WindowEnd, &row.Status, &row.FundTx, &row.SettleTx,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ChannelRow{}, fmt.Errorf("store: channel not found")
		}
		return ChannelRow{}, fmt.Errorf("store: active channel: %w", err)
	}
	return row, nil
}

// ReserveChannelBalance atomically increments reserved_wei (load-bearing invariant §6.2).
func (s *Store) ReserveChannelBalance(ctx context.Context, channelID, amountWei string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE channels
		   SET reserved_wei = (reserved_wei::numeric + $2::numeric)::text
		 WHERE id = $1
		   AND status = 'open'
		   AND (balance_wei::numeric - reserved_wei::numeric) >= $2::numeric`,
		channelID, amountWei,
	)
	if err != nil {
		return fmt.Errorf("store: reserve channel: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: insufficient channel balance")
	}
	return nil
}

// ReleaseChannelReserve releases reserved wei back to available (void path).
func (s *Store) ReleaseChannelReserve(ctx context.Context, channelID, amountWei string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE channels
		   SET reserved_wei = GREATEST((reserved_wei::numeric - $2::numeric), 0)::text
		 WHERE id = $1`, channelID, amountWei,
	)
	return err
}

// FinalizeChannelCharge moves reserved to cumulative spend after delivery.
func (s *Store) FinalizeChannelCharge(ctx context.Context, channelID, chargeWei string, newNonce int64, cumulativeWei, voucherSig string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE channels SET
			reserved_wei = GREATEST((reserved_wei::numeric - $2::numeric), 0)::text,
			cumulative_wei = $3,
			nonce = $4,
			last_voucher_sig = $5
		WHERE id = $1`, channelID, chargeWei, cumulativeWei, newNonce, voucherSig,
	)
	if err != nil {
		return fmt.Errorf("store: finalize channel: %w", err)
	}
	return nil
}

// CosignVoucher atomically finalizes the channel charge and persists the
// co-signed voucher in ONE transaction, so the channel nonce/cumulative can
// never advance without a matching voucher row (and vice versa). Returns the new
// voucher id.
func (s *Store) CosignVoucher(ctx context.Context, chargeWei string, v VoucherRow) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin cosign tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE channels SET
			reserved_wei = GREATEST((reserved_wei::numeric - $2::numeric), 0)::text,
			cumulative_wei = $3,
			nonce = $4,
			last_voucher_sig = $5
		WHERE id = $1`, v.ChannelID, chargeWei, v.CumulativeWei, v.Nonce, v.CallerSig,
	); err != nil {
		return "", fmt.Errorf("store: finalize channel: %w", err)
	}

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO vouchers (
			channel_id, cumulative_wei, nonce, last_receipt_hash, eip712_digest, caller_sig
		) VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text`,
		v.ChannelID, v.CumulativeWei, v.Nonce, v.LastReceiptHash, v.Digest, v.CallerSig,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("store: insert voucher: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit cosign tx: %w", err)
	}
	return id, nil
}

// AdvanceChannelRedeemed adds settled wei to the channel's redeemed mirror so a
// single co-signed voucher cumulative bounds payout across windows/developers.
func (s *Store) AdvanceChannelRedeemed(ctx context.Context, channelID, amountWei string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE channels
		   SET redeemed_wei = (COALESCE(redeemed_wei,'0')::numeric + $2::numeric)::text
		 WHERE id = $1`, channelID, amountWei,
	)
	if err != nil {
		return fmt.Errorf("store: advance channel redeemed: %w", err)
	}
	return nil
}

// ReleaseExpiredChannelReserves zeroes dangling reservations on channels whose
// window has ended, so an async caller that never co-signs cannot lock the
// reserve forever (docs/08 §8.3 close/refund; audit F4).
func (s *Store) ReleaseExpiredChannelReserves(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `
		UPDATE channels
		   SET reserved_wei = '0'
		 WHERE status = 'open'
		   AND window_end < now()
		   AND reserved_wei::numeric > 0`)
	if err != nil {
		return 0, fmt.Errorf("store: release expired reserves: %w", err)
	}
	return ct.RowsAffected(), nil
}

// GetChannelByID loads a channel by uuid.
func (s *Store) GetChannelByID(ctx context.Context, id string) (ChannelRow, error) {
	var row ChannelRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, caller_did, caller_wallet, escrow_addr, balance_wei, reserved_wei,
		       cumulative_wei, COALESCE(redeemed_wei,'0'), nonce::bigint, last_voucher_sig,
		       window_start, window_end, status, fund_tx, settle_tx
		FROM channels WHERE id = $1`, id,
	).Scan(
		&row.ID, &row.CallerDID, &row.CallerWallet, &row.EscrowAddr, &row.BalanceWei,
		&row.ReservedWei, &row.CumulativeWei, &row.RedeemedWei, &row.Nonce, &row.LastVoucherSig,
		&row.WindowStart, &row.WindowEnd, &row.Status, &row.FundTx, &row.SettleTx,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ChannelRow{}, fmt.Errorf("store: channel not found")
		}
		return ChannelRow{}, fmt.Errorf("store: get channel: %w", err)
	}
	return row, nil
}

// CloseChannel marks a channel closed.
func (s *Store) CloseChannel(ctx context.Context, channelID, settleTx string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE channels SET status = 'closed', settle_tx = $2 WHERE id = $1`, channelID, settleTx,
	)
	return err
}
