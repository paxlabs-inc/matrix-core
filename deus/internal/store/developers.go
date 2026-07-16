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

// UpsertDeveloperByAccount ensures a developer row exists for an authenticated
// Deus Markets account. Account ownership is independent of an EVM wallet.
func (s *Store) UpsertDeveloperByAccount(ctx context.Context, accountID, accountDID, displayName string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO developers (supabase_user_id, account_did, display_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (supabase_user_id) WHERE supabase_user_id IS NOT NULL DO UPDATE
		  SET account_did = EXCLUDED.account_did,
		      display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), developers.display_name)
		RETURNING id::text`,
		accountID, accountDID, displayName,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: upsert account developer: %w", err)
	}
	return id, nil
}

// SetDeveloperPayeeDID records the developer's LayerX payee DID — the identity
// LXP settlements pay directly (synced from the manifest at registration).
func (s *Store) SetDeveloperPayeeDID(ctx context.Context, developerID, payeeDID string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE developers SET payee_did = $2 WHERE id = $1`, developerID, payeeDID)
	if err != nil {
		return fmt.Errorf("store: set payee did: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: developer not found")
	}
	return nil
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

// DeveloperByAccountID loads a developer row by its authenticated account subject.
func (s *Store) DeveloperByAccountID(ctx context.Context, accountID string) (DeveloperRow, error) {
	return s.developerByQuery(ctx, `supabase_user_id = $1`, accountID)
}

// DeveloperByID loads a developer row by primary key.
func (s *Store) DeveloperByID(ctx context.Context, developerID string) (DeveloperRow, error) {
	return s.developerByQuery(ctx, `id = $1`, developerID)
}
