package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpsertDeveloperByWallet ensures a developer row exists for wallet_address.
func (s *Store) UpsertDeveloperByWallet(ctx context.Context, wallet, payout, displayName string) (string, error) {
	if payout == "" {
		payout = wallet
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO developers (wallet_address, payout_address, display_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (wallet_address) DO UPDATE
		  SET payout_address = EXCLUDED.payout_address,
		      display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), developers.display_name)
		RETURNING id::text`,
		wallet, payout, displayName,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: upsert developer: %w", err)
	}
	return id, nil
}

// DeveloperWalletByID returns wallet_address for a developer id.
func (s *Store) DeveloperWalletByID(ctx context.Context, developerID string) (string, error) {
	var wallet string
	err := s.pool.QueryRow(ctx, `SELECT wallet_address FROM developers WHERE id = $1`, developerID).Scan(&wallet)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("store: developer not found: %w", err)
		}
		return "", fmt.Errorf("store: developer wallet: %w", err)
	}
	return wallet, nil
}
