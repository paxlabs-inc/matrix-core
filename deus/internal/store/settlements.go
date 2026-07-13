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
