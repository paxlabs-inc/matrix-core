package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RegisterDIDClaim records the keccak256(did) -> did mapping the deposit watcher
// uses to reverse an on-chain Deposit `did` bytes32 back to the off-chain DID.
// Idempotent: the same claim always maps to the same DID, so a re-register is a
// no-op (the DID is refreshed defensively in case of a label change).
func (s *Store) RegisterDIDClaim(ctx context.Context, claim, did string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO did_claims (claim, did) VALUES ($1, $2)
		ON CONFLICT (claim) DO UPDATE SET did = EXCLUDED.did`, claim, did)
	if err != nil {
		return fmt.Errorf("store: register did claim: %w", err)
	}
	return nil
}

// ResolveDIDClaim returns the DID a claim maps to, or ErrNotFound when the claim
// was never registered (an unrecognized deposit the watcher cannot attribute).
func (s *Store) ResolveDIDClaim(ctx context.Context, claim string) (string, error) {
	var did string
	err := s.pool.QueryRow(ctx, `SELECT did FROM did_claims WHERE claim = $1`, claim).Scan(&did)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: resolve did claim: %w", err)
	}
	return did, nil
}

// GetCursor returns the last fully-processed block for a named watcher. The bool
// is false when no cursor exists yet (first run).
func (s *Store) GetCursor(ctx context.Context, name string) (int64, bool, error) {
	var block int64
	err := s.pool.QueryRow(ctx, `SELECT last_block FROM chain_cursors WHERE name = $1`, name).Scan(&block)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("store: get cursor: %w", err)
	}
	return block, true, nil
}

// SetCursor advances a watcher's last-processed block (upsert).
func (s *Store) SetCursor(ctx context.Context, name string, block int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chain_cursors (name, last_block) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET last_block = EXCLUDED.last_block, updated_at = now()`, name, block)
	if err != nil {
		return fmt.Errorf("store: set cursor: %w", err)
	}
	return nil
}

// CirculatingUSDX returns the sum of all off-chain spendable balances in
// micro-USDX — the off-chain side of the reserve invariant i1 (circulating USDX
// == USDL held in the vault).
func (s *Store) CirculatingUSDX(ctx context.Context) (int64, error) {
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(balance_usdx), 0) FROM accounts`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: circulating usdx: %w", err)
	}
	return total, nil
}
