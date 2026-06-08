package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StreamRow mirrors a PaymentStreams session.
type StreamRow struct {
	ID                string
	ChainStreamID     string
	ServiceID         string
	CallerDID         string
	CallerWallet      string
	PayeeAddress      string
	RatePerSecondWei  string
	CapWei            string
	SettledWei        string
	MeteredWei        string
	Status            string
	OpenTx            *string
	LastSettleTx      *string
	CloseTx           *string
	OpenedAt          time.Time
	ClosedAt          *time.Time
	LastMeteredAt     time.Time
}

// InsertStream persists a newly opened stream.
func (s *Store) InsertStream(ctx context.Context, row StreamRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO streams (
			chain_stream_id, service_id, caller_did, caller_wallet, payee_address,
			rate_per_second_wei, cap_wei, settled_wei, metered_wei, status, open_tx,
			opened_at, last_metered_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		row.ChainStreamID, row.ServiceID, row.CallerDID, row.CallerWallet, row.PayeeAddress,
		row.RatePerSecondWei, row.CapWei, row.SettledWei, row.MeteredWei, row.Status, row.OpenTx,
		row.OpenedAt, row.LastMeteredAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert stream: %w", err)
	}
	return id, nil
}

// GetStreamByID loads a stream by primary key.
func (s *Store) GetStreamByID(ctx context.Context, id string) (StreamRow, error) {
	var row StreamRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, chain_stream_id, service_id, caller_did, caller_wallet, payee_address,
		       rate_per_second_wei, cap_wei, settled_wei, metered_wei, status,
		       open_tx, last_settle_tx, close_tx, opened_at, closed_at, last_metered_at
		FROM streams WHERE id = $1`, id,
	).Scan(
		&row.ID, &row.ChainStreamID, &row.ServiceID, &row.CallerDID, &row.CallerWallet,
		&row.PayeeAddress, &row.RatePerSecondWei, &row.CapWei, &row.SettledWei, &row.MeteredWei,
		&row.Status, &row.OpenTx, &row.LastSettleTx, &row.CloseTx, &row.OpenedAt, &row.ClosedAt,
		&row.LastMeteredAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return StreamRow{}, fmt.Errorf("store: stream not found")
		}
		return StreamRow{}, fmt.Errorf("store: get stream: %w", err)
	}
	return row, nil
}

// UpdateStreamMetered advances metered accrual attributed to invocations.
func (s *Store) UpdateStreamMetered(ctx context.Context, id, meteredWei string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE streams SET metered_wei = $2, last_metered_at = $3 WHERE id = $1 AND status = 'open'`,
		id, meteredWei, at,
	)
	if err != nil {
		return fmt.Errorf("store: update stream metered: %w", err)
	}
	return nil
}

// UpdateStreamSettled records an on-chain settle.
func (s *Store) UpdateStreamSettled(ctx context.Context, id, settledWei, txHash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE streams SET settled_wei = $2, last_settle_tx = $3 WHERE id = $1 AND status = 'open'`,
		id, settledWei, txHash,
	)
	if err != nil {
		return fmt.Errorf("store: update stream settled: %w", err)
	}
	return nil
}

// CloseStream marks a stream closed.
func (s *Store) CloseStream(ctx context.Context, id, closeTx string, closedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE streams SET status = 'closed', close_tx = $2, closed_at = $3 WHERE id = $1`,
		id, closeTx, closedAt,
	)
	if err != nil {
		return fmt.Errorf("store: close stream: %w", err)
	}
	return nil
}
