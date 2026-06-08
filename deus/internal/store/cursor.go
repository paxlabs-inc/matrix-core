package store

import (
	"context"
	"fmt"
)

// IndexCursor is the indexer bookmark.
type IndexCursor struct {
	LastBlock    int64
	LastLogIndex int
}

// GetIndexCursor reads the singleton cursor row.
func (s *Store) GetIndexCursor(ctx context.Context) (IndexCursor, error) {
	var c IndexCursor
	err := s.pool.QueryRow(ctx, `SELECT last_block, last_log_index FROM index_cursor WHERE id = 1`).Scan(&c.LastBlock, &c.LastLogIndex)
	if err != nil {
		return IndexCursor{}, fmt.Errorf("store: get index cursor: %w", err)
	}
	return c, nil
}

// SetIndexCursor persists the indexer bookmark atomically.
func (s *Store) SetIndexCursor(ctx context.Context, c IndexCursor) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE index_cursor SET last_block = $1, last_log_index = $2, updated_at = now() WHERE id = 1`,
		c.LastBlock, c.LastLogIndex,
	)
	if err != nil {
		return fmt.Errorf("store: set index cursor: %w", err)
	}
	return nil
}
