package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChannelRow is a per-caller payment channel mirror.
type ChannelRow struct {
	ID              string
	CallerDID       string
	CallerWallet    string
	EscrowAddr      string
	BalanceWei      string
	ReservedWei     string
	CumulativeWei   string
	Nonce           int64
	LastVoucherSig  *string
	WindowStart     time.Time
	WindowEnd       time.Time
	Status          string
	FundTx          *string
	SettleTx        *string
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
		       cumulative_wei, nonce::bigint, last_voucher_sig, window_start, window_end,
		       status, fund_tx, settle_tx
		FROM channels
		WHERE caller_did = $1 AND status = 'open' AND window_end > now()
		ORDER BY window_start DESC
		LIMIT 1`, callerDID,
	).Scan(
		&row.ID, &row.CallerDID, &row.CallerWallet, &row.EscrowAddr, &row.BalanceWei,
		&row.ReservedWei, &row.CumulativeWei, &row.Nonce, &row.LastVoucherSig,
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

// GetChannelByID loads a channel by uuid.
func (s *Store) GetChannelByID(ctx context.Context, id string) (ChannelRow, error) {
	var row ChannelRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, caller_did, caller_wallet, escrow_addr, balance_wei, reserved_wei,
		       cumulative_wei, nonce::bigint, last_voucher_sig, window_start, window_end,
		       status, fund_tx, settle_tx
		FROM channels WHERE id = $1`, id,
	).Scan(
		&row.ID, &row.CallerDID, &row.CallerWallet, &row.EscrowAddr, &row.BalanceWei,
		&row.ReservedWei, &row.CumulativeWei, &row.Nonce, &row.LastVoucherSig,
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
