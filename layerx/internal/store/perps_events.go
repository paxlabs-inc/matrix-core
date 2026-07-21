package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PerpEvent is one immutable row of the append-only perps journal. Seq is the
// gap-free global sequence; OwnerEventID is the gap-free per-owner private
// stream id (0 for market-scoped events with no owner).
type PerpEvent struct {
	Seq          int64
	OwnerDID     string
	OwnerEventID int64
	ActingDID    string
	EventType    string
	Payload      []byte
	OccurredAt   time.Time
}

// appendPerpEvent inserts one journal row inside the caller's transaction,
// allocating both gap-free sequences as MAX+1 under a transaction-scoped
// advisory lock (so a rolled-back transaction cannot leave a hole the way an
// IDENTITY sequence would).
func appendPerpEvent(ctx context.Context, tx pgx.Tx, ownerDID, actingDID, eventType string, payload any) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("store: marshal perp event payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('perp_events'))`); err != nil {
		return 0, fmt.Errorf("store: lock perp events: %w", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM perp_events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: allocate perp event seq: %w", err)
	}
	if ownerDID == "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO perp_events (seq, acting_did, event_type, payload)
			VALUES ($1, NULLIF($2,''), $3, $4)`, seq, actingDID, eventType, body); err != nil {
			return 0, fmt.Errorf("store: insert perp event: %w", err)
		}
		return seq, nil
	}
	var ownerEventID int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(owner_event_id), 0) + 1 FROM perp_events WHERE owner_did = $1`, ownerDID).
		Scan(&ownerEventID); err != nil {
		return 0, fmt.Errorf("store: allocate perp owner event id: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO perp_events (seq, owner_did, owner_event_id, acting_did, event_type, payload)
		VALUES ($1, $2, $3, NULLIF($4,''), $5, $6)`,
		seq, ownerDID, ownerEventID, actingDID, eventType, body); err != nil {
		return 0, fmt.Errorf("store: insert perp owner event: %w", err)
	}
	return seq, nil
}

func scanPerpEvents(rows pgx.Rows) ([]PerpEvent, error) {
	defer rows.Close()
	var out []PerpEvent
	for rows.Next() {
		var e PerpEvent
		var owner *string
		var ownerEventID *int64
		var acting *string
		if err := rows.Scan(&e.Seq, &owner, &ownerEventID, &acting, &e.EventType, &e.Payload, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("store: scan perp event: %w", err)
		}
		if owner != nil {
			e.OwnerDID = *owner
		}
		if ownerEventID != nil {
			e.OwnerEventID = *ownerEventID
		}
		if acting != nil {
			e.ActingDID = *acting
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const perpEventSelect = `
	SELECT seq, owner_did, owner_event_id, acting_did, event_type, payload, occurred_at
	FROM perp_events`

// ListPerpOwnerEvents returns the owner's private events strictly after
// afterOwnerEventID, oldest first (the Last-Event-ID replay read).
func (s *Store) ListPerpOwnerEvents(ctx context.Context, ownerDID string, afterOwnerEventID int64, limit int) ([]PerpEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, perpEventSelect+`
		WHERE owner_did = $1 AND owner_event_id > $2
		ORDER BY owner_event_id ASC LIMIT $3`, ownerDID, afterOwnerEventID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list perp owner events: %w", err)
	}
	return scanPerpEvents(rows)
}

// ListPerpJournal returns global journal rows strictly after afterSeq, oldest
// first (the anchoring/reconciliation read).
func (s *Store) ListPerpJournal(ctx context.Context, afterSeq int64, limit int) ([]PerpEvent, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, perpEventSelect+`
		WHERE seq > $1 ORDER BY seq ASC LIMIT $2`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list perp journal: %w", err)
	}
	return scanPerpEvents(rows)
}

// PerpEventSequenceReport proves the journal's gap-free law: Total rows must
// equal MaxSeq (seq is dense from 1) and every owner's row count must equal its
// max owner_event_id (dense from 1). Gaps counts the violating owners.
type PerpEventSequenceReport struct {
	Total     int64
	MaxSeq    int64
	OwnerGaps int64
}

// CheckPerpEventSequences audits both gap-free sequences in one read.
func (s *Store) CheckPerpEventSequences(ctx context.Context) (PerpEventSequenceReport, error) {
	var r PerpEventSequenceReport
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(seq), 0),
		       (SELECT COUNT(*) FROM (
		            SELECT owner_did FROM perp_events WHERE owner_did IS NOT NULL
		            GROUP BY owner_did
		            HAVING COUNT(*) <> MAX(owner_event_id) OR MIN(owner_event_id) <> 1
		        ) g)
		FROM perp_events`).Scan(&r.Total, &r.MaxSeq, &r.OwnerGaps); err != nil {
		return PerpEventSequenceReport{}, fmt.Errorf("store: check perp event sequences: %w", err)
	}
	return r, nil
}
