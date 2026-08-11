// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sessionjournal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"matrix/vault"

	_ "modernc.org/sqlite"
)

const (
	DefaultPath = "/data/neo-session-journal.db"
	storeName   = "neo.session-journal"
	schemaV1    = "event.v1"
)

type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	vault  *vault.Session
	user   string
	now    func() time.Time
	closed bool
}

func Dir(override, cortexRoot string) string {
	if value := strings.TrimSpace(override); value != "" {
		return value
	}
	if value := strings.TrimSpace(cortexRoot); value != "" {
		return filepath.Join(filepath.Dir(value), "neo-session-journal.db")
	}
	return ""
}

func Open(ctx context.Context, path string, session *vault.Session) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("session journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("session journal create directory: %w", err)
	}
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("session journal open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session journal connect: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session journal secure database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	user := ""
	if session != nil && session.UserVault() != nil {
		user = session.UserVault().User()
	}
	return &Store{db: db, vault: session, user: user, now: time.Now}, nil
}

func sqliteDSN(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("session journal resolve path: %w", err)
	}
	parameters := url.Values{}
	for _, pragma := range []string{
		"journal_mode(WAL)",
		"wal_autocheckpoint(1000)",
		"busy_timeout(5000)",
		"synchronous(FULL)",
		"foreign_keys(ON)",
	} {
		parameters.Add("_pragma", pragma)
	}
	return (&url.URL{Scheme: "file", Path: absolute, RawQuery: parameters.Encode()}).String(), nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS journal_sequence (
			conversation_id TEXT PRIMARY KEY,
			next_sequence INTEGER NOT NULL CHECK(next_sequence > 0)
		)`,
		`CREATE TABLE IF NOT EXISTS journal_event (
			conversation_id TEXT NOT NULL,
			sequence INTEGER NOT NULL CHECK(sequence > 0),
			turn_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			event BLOB NOT NULL,
			PRIMARY KEY(conversation_id, sequence)
		)`,
		`CREATE INDEX IF NOT EXISTS journal_event_turn
		 ON journal_event(conversation_id, turn_id, sequence)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("session journal migrate: %w", err)
		}
	}
	return nil
}

func (store *Store) Append(ctx context.Context, event Event) (Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.appendLocked(ctx, event)
}

func (store *Store) appendLocked(ctx context.Context, event Event) (Event, error) {
	if store == nil || store.closed || store.db == nil {
		return Event{}, ErrClosed
	}
	event.ConversationID = strings.TrimSpace(event.ConversationID)
	event.TurnID = strings.TrimSpace(event.TurnID)
	event.Sequence = 0
	if event.CreatedAt.IsZero() {
		event.CreatedAt = store.now().UTC()
	} else {
		event.CreatedAt = event.CreatedAt.UTC()
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("session journal begin append: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO journal_sequence(conversation_id, next_sequence)
		 VALUES (?, 2)
		 ON CONFLICT(conversation_id) DO UPDATE
		 SET next_sequence = journal_sequence.next_sequence + 1
		 RETURNING next_sequence - 1`, event.ConversationID,
	).Scan(&event.Sequence); err != nil {
		return Event{}, fmt.Errorf("session journal allocate sequence: %w", err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("session journal encode event: %w", err)
	}
	sealed, err := store.seal(event.ConversationID, event.Sequence, encoded)
	zero(encoded)
	if err != nil {
		return Event{}, err
	}
	defer zero(sealed)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO journal_event(
		 conversation_id, sequence, turn_id, kind, created_at, event
		 ) VALUES (?, ?, ?, ?, ?, ?)`,
		event.ConversationID, event.Sequence, event.TurnID, string(event.Kind),
		event.CreatedAt.UnixMicro(), sealed,
	); err != nil {
		return Event{}, fmt.Errorf("session journal insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("session journal commit event: %w", err)
	}
	committed = true
	return event, nil
}

func (store *Store) Read(ctx context.Context, conversationID string, fromSequence int64, limit int) ([]Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.readLocked(ctx, conversationID, fromSequence, limit)
}

func (store *Store) readLocked(ctx context.Context, conversationID string, fromSequence int64, limit int) ([]Event, error) {
	if store == nil || store.closed || store.db == nil {
		return nil, ErrClosed
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, fmt.Errorf("session journal conversation ID is required")
	}
	if fromSequence < 1 {
		fromSequence = 1
	}
	query := `SELECT sequence, turn_id, kind, created_at, event
	 FROM journal_event
	 WHERE conversation_id = ? AND sequence >= ?
	 ORDER BY sequence`
	arguments := []any{conversationID, fromSequence}
	if limit > 0 {
		query += ` LIMIT ?`
		arguments = append(arguments, limit)
	}
	rows, err := store.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("session journal read: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var sequence int64
		var turnID, kind string
		var createdMicros int64
		var sealed []byte
		if err := rows.Scan(&sequence, &turnID, &kind, &createdMicros, &sealed); err != nil {
			return nil, fmt.Errorf("session journal scan: %w", err)
		}
		event, err := store.openEvent(conversationID, sequence, sealed)
		zero(sealed)
		if err != nil {
			return nil, err
		}
		if event.ConversationID != conversationID || event.Sequence != sequence || event.TurnID != turnID || string(event.Kind) != kind || event.CreatedAt.UnixMicro() != createdMicros {
			return nil, fmt.Errorf("session journal envelope mismatch at %s:%d", conversationID, sequence)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session journal iterate: %w", err)
	}
	return events, nil
}

func (store *Store) Tail(ctx context.Context, conversationID string) (TailState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.tailLocked(ctx, conversationID)
}

func (store *Store) tailLocked(ctx context.Context, conversationID string) (TailState, error) {
	events, err := store.readLocked(ctx, conversationID, 1, 0)
	if err != nil {
		return TailState{}, err
	}
	return inspectTail(events), nil
}

func (store *Store) FinalizeTail(ctx context.Context, conversationID, reason string) (Event, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	tail, err := store.tailLocked(ctx, conversationID)
	if err != nil {
		return Event{}, false, err
	}
	if !tail.Abnormal() {
		return Event{}, false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "abnormal session tail finalized for resume"
	}
	recovery := &Recovery{
		State: "resumable", Reason: reason,
		FinalizesSequence: tail.Event.Sequence,
	}
	if tail.Event.ToolCall != nil {
		recovery.PendingCallID = tail.Event.ToolCall.CallID
	}
	event, err := store.appendLocked(ctx, Event{
		ConversationID: strings.TrimSpace(conversationID),
		TurnID:         tail.Event.TurnID,
		Attempt:        tail.Event.Attempt,
		Kind:           KindRecovery,
		DisplayContent: reason,
		Recovery:       recovery,
	})
	return event, err == nil, err
}

func inspectTail(events []Event) TailState {
	pending := make(map[string]Event)
	partial := make(map[int64]Event)
	finalized := make(map[int64]struct{})
	for _, event := range events {
		switch event.Kind {
		case KindToolCall:
			pending[event.ToolCall.CallID] = event
		case KindToolResult:
			delete(pending, event.ToolResult.CallID)
		case KindAssistantMessage:
			if event.Message.Partial {
				partial[event.Sequence] = event
			}
		case KindRecovery:
			if event.Recovery.FinalizesSequence > 0 {
				finalized[event.Recovery.FinalizesSequence] = struct{}{}
				delete(partial, event.Recovery.FinalizesSequence)
				if event.Recovery.PendingCallID != "" {
					delete(pending, event.Recovery.PendingCallID)
				}
			}
		}
	}
	state := TailState{Condition: TailClean}
	for _, event := range pending {
		if _, ok := finalized[event.Sequence]; ok {
			continue
		}
		if event.Sequence > state.Event.Sequence {
			state = TailState{Condition: TailPendingToolCall, Event: event}
		}
	}
	for sequence, event := range partial {
		if _, ok := finalized[sequence]; ok {
			continue
		}
		if event.Sequence > state.Event.Sequence {
			state = TailState{Condition: TailPartialAssistant, Event: event}
		}
	}
	return state
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store == nil || store.closed {
		return nil
	}
	store.closed = true
	return store.db.Close()
}

func (store *Store) seal(conversationID string, sequence int64, plaintext []byte) ([]byte, error) {
	sealed, err := store.vault.MaybeSealRecord(store.ad(conversationID, sequence), plaintext)
	if err != nil {
		return nil, fmt.Errorf("session journal seal event: %w", err)
	}
	return append([]byte(nil), sealed...), nil
}

func (store *Store) openEvent(conversationID string, sequence int64, sealed []byte) (Event, error) {
	plaintext := sealed
	if vault.IsVault(sealed) {
		if store.vault == nil || store.vault.UserVault() == nil {
			return Event{}, vault.ErrVaultRequired
		}
		var err error
		plaintext, err = store.vault.UserVault().OpenRecord(store.ad(conversationID, sequence), sealed)
		if err != nil {
			return Event{}, fmt.Errorf("session journal open event: %w", err)
		}
		defer zero(plaintext)
	}
	var event Event
	if err := json.Unmarshal(plaintext, &event); err != nil {
		return Event{}, fmt.Errorf("session journal decode event: %w", err)
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (store *Store) ad(conversationID string, sequence int64) vault.AD {
	return vault.AD{
		User: store.user, Store: storeName, Stream: conversationID,
		Seq: uint64(sequence), Schema: schemaV1,
	}
}

func zero(data []byte) {
	for index := range data {
		data[index] = 0
	}
}

func IsClosed(err error) bool { return errors.Is(err, ErrClosed) }
