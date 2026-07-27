// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

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
	sqliteDriver "modernc.org/sqlite"
)

const (
	DefaultPath         = "/data/neo-turnstate.db"
	writerQueueCapacity = 64
	maxBusyRetries      = 3
)

var ErrClosed = errors.New("turnstate: store closed")

type writeRequest struct {
	ctx    context.Context
	run    func(context.Context, *sql.DB) error
	result chan error
}

type Store struct {
	readDB  *sql.DB
	writeDB *sql.DB
	vault   *vault.Session
	user    string
	now     func() time.Time

	lifecycleMu sync.RWMutex
	closed      bool
	writes      chan writeRequest
	writerDone  chan struct{}
}

func Open(
	ctx context.Context,
	path string,
	session *vault.Session,
) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("turnstate: database path is required")
	}
	if session == nil || !session.Encrypting() || session.UserVault() == nil {
		return nil, vault.ErrVaultRequired
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("turnstate: create data directory: %w", err)
	}
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}
	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("turnstate: open writer: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	if err := writeDB.PingContext(ctx); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("turnstate: connect writer: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("turnstate: secure database permissions: %w", err)
	}
	if err := applyMigrations(ctx, writeDB, time.Now().UTC()); err != nil {
		_ = writeDB.Close()
		return nil, err
	}
	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("turnstate: open readers: %w", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)
	if err := readDB.PingContext(ctx); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("turnstate: connect readers: %w", err)
	}
	store := &Store{
		readDB: readDB, writeDB: writeDB,
		vault: session, user: session.UserVault().User(),
		now:        time.Now,
		writes:     make(chan writeRequest, writerQueueCapacity),
		writerDone: make(chan struct{}),
	}
	go store.writer()
	return store, nil
}

func sqliteDSN(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("turnstate: resolve database path: %w", err)
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
	return (&url.URL{
		Scheme: "file", Path: absolute, RawQuery: parameters.Encode(),
	}).String(), nil
}

func (store *Store) CreateTurnState(ctx context.Context, state TurnState) error {
	state.UpdatedAt = store.now().UTC()
	if state.Status != StatusRunning {
		return fmt.Errorf("turnstate: new turn must be running")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	envelope, err := store.sealTurn(state)
	if err != nil {
		return err
	}
	defer zero(envelope)
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(runCtx,
			`INSERT INTO turn_state(
			 turn_id, actor_id, session_id, status, state, updated_at
			 ) VALUES (?, ?, ?, ?, ?, ?)`,
			state.TurnID, state.ActorID, state.SessionID, string(state.Status),
			envelope, state.UpdatedAt.UnixMicro(),
		)
		if err != nil {
			return fmt.Errorf("turnstate: create turn: %w", err)
		}
		return nil
	})
}

func (store *Store) LoadTurnState(
	ctx context.Context,
	turnID string,
) (TurnState, error) {
	if err := store.checkOpen(); err != nil {
		return TurnState{}, err
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return TurnState{}, fmt.Errorf("turnstate: turn ID is required")
	}
	var envelope []byte
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT state FROM turn_state WHERE turn_id = ?`, turnID,
	).Scan(&envelope); err != nil {
		return TurnState{}, fmt.Errorf("turnstate: load turn: %w", err)
	}
	return store.openTurn(turnID, envelope)
}

func (store *Store) SaveTurnCheckpoint(
	ctx context.Context,
	turnID string,
	checkpoint Checkpoint,
) error {
	checkpoint.SavedAt = store.now().UTC()
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	return store.updateTurn(ctx, turnID, func(state *TurnState) error {
		state.Checkpoint = &checkpoint
		return nil
	})
}

func (store *Store) SetTurnRecovery(
	ctx context.Context,
	turnID string,
	status Status,
	recovery Recovery,
) error {
	if status != StatusRecovering && status != StatusIncomplete &&
		status != StatusInterrupted {
		return fmt.Errorf("turnstate: invalid recovery status %q", status)
	}
	if err := recovery.Validate(); err != nil {
		return err
	}
	return store.updateTurn(ctx, turnID, func(state *TurnState) error {
		state.Status = status
		state.Recovery = &recovery
		return nil
	})
}

func (store *Store) SetTurnStatus(
	ctx context.Context,
	turnID string,
	status Status,
) error {
	if !status.Valid() {
		return fmt.Errorf("turnstate: invalid turn status %q", status)
	}
	return store.updateTurn(ctx, turnID, func(state *TurnState) error {
		state.Status = status
		return nil
	})
}

func (store *Store) SetTurnFailure(
	ctx context.Context,
	turnID string,
	status Status,
	code string,
) error {
	code = strings.TrimSpace(code)
	if (status != StatusFailed && status != StatusCancelled) ||
		code == "" || len(code) > 96 {
		return fmt.Errorf("turnstate: valid terminal failure code is required")
	}
	for _, current := range code {
		if (current < 'a' || current > 'z') &&
			(current < '0' || current > '9') && current != '_' {
			return fmt.Errorf("turnstate: invalid terminal failure code")
		}
	}
	return store.updateTurn(ctx, turnID, func(state *TurnState) error {
		state.Status = status
		state.FailureCode = code
		return nil
	})
}

func (store *Store) RecoverableTurnStates(
	ctx context.Context,
) ([]TurnState, error) {
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := store.readDB.QueryContext(ctx,
		`SELECT turn_id, state FROM turn_state
		 WHERE status IN (?, ?, ?, ?)
		 ORDER BY updated_at, turn_id`,
		string(StatusRunning), string(StatusRecovering),
		string(StatusIncomplete), string(StatusInterrupted),
	)
	if err != nil {
		return nil, fmt.Errorf("turnstate: query recoverable turns: %w", err)
	}
	defer rows.Close()
	var states []TurnState
	for rows.Next() {
		var turnID string
		var envelope []byte
		if err := rows.Scan(&turnID, &envelope); err != nil {
			return nil, fmt.Errorf("turnstate: scan recoverable turn: %w", err)
		}
		state, err := store.openTurn(turnID, envelope)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turnstate: iterate recoverable turns: %w", err)
	}
	return states, nil
}

func (store *Store) SaveCognitionState(
	ctx context.Context,
	sessionID string,
	state json.RawMessage,
) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(state) == 0 || !json.Valid(state) {
		return fmt.Errorf("turnstate: valid cognition state and session are required")
	}
	envelope, err := store.seal(
		sessionID, "cognition-state.v1", state,
	)
	if err != nil {
		return fmt.Errorf("turnstate: seal cognition state: %w", err)
	}
	defer zero(envelope)
	now := store.now().UTC()
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(runCtx,
			`INSERT INTO cognition_state(session_id, state, updated_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(session_id) DO UPDATE SET
			   state = excluded.state,
			   updated_at = excluded.updated_at`,
			sessionID, envelope, now.UnixMicro(),
		)
		if err != nil {
			return fmt.Errorf("turnstate: save cognition state: %w", err)
		}
		return nil
	})
}

func (store *Store) LoadCognitionState(
	ctx context.Context,
	sessionID string,
) (json.RawMessage, error) {
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("turnstate: cognition session is required")
	}
	var envelope []byte
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT state FROM cognition_state WHERE session_id = ?`, sessionID,
	).Scan(&envelope); err != nil {
		return nil, fmt.Errorf("turnstate: load cognition state: %w", err)
	}
	plaintext, err := store.open(sessionID, "cognition-state.v1", envelope)
	if err != nil {
		return nil, fmt.Errorf("turnstate: open cognition state: %w", err)
	}
	if !json.Valid(plaintext) {
		zero(plaintext)
		return nil, fmt.Errorf("turnstate: cognition state is not JSON")
	}
	return plaintext, nil
}

func (store *Store) SchemaVersion(ctx context.Context) (int, error) {
	if err := store.checkOpen(); err != nil {
		return 0, err
	}
	var version int
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`,
	).Scan(&version); err != nil {
		return 0, fmt.Errorf("turnstate: schema version: %w", err)
	}
	return version, nil
}

func (store *Store) updateTurn(
	ctx context.Context,
	turnID string,
	update func(*TurnState) error,
) error {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" || update == nil {
		return fmt.Errorf("turnstate: turn update requires an ID and mutation")
	}
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin turn update: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		var envelope []byte
		if err := tx.QueryRowContext(runCtx,
			`SELECT state FROM turn_state WHERE turn_id = ?`, turnID,
		).Scan(&envelope); err != nil {
			return fmt.Errorf("turnstate: load turn for update: %w", err)
		}
		state, err := store.openTurn(turnID, envelope)
		if err != nil {
			return err
		}
		if err := update(&state); err != nil {
			return err
		}
		state.UpdatedAt = store.now().UTC()
		if err := state.Validate(); err != nil {
			return err
		}
		replacement, err := store.sealTurn(state)
		if err != nil {
			return err
		}
		defer zero(replacement)
		if _, err := tx.ExecContext(runCtx,
			`UPDATE turn_state
			 SET status = ?, state = ?, updated_at = ?
			 WHERE turn_id = ?`,
			string(state.Status), replacement, state.UpdatedAt.UnixMicro(), turnID,
		); err != nil {
			return fmt.Errorf("turnstate: update turn: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit turn update: %w", err)
		}
		committed = true
		return nil
	})
}

func (store *Store) sealTurn(state TurnState) ([]byte, error) {
	plaintext, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("turnstate: encode turn: %w", err)
	}
	defer zero(plaintext)
	return store.seal(state.TurnID, "turn-state.v1", plaintext)
}

func (store *Store) openTurn(turnID string, envelope []byte) (TurnState, error) {
	plaintext, err := store.open(turnID, "turn-state.v1", envelope)
	if err != nil {
		return TurnState{}, fmt.Errorf("turnstate: open turn: %w", err)
	}
	defer zero(plaintext)
	var state TurnState
	if err := json.Unmarshal(plaintext, &state); err != nil {
		return TurnState{}, fmt.Errorf("turnstate: decode turn: %w", err)
	}
	if err := state.Validate(); err != nil {
		return TurnState{}, err
	}
	return state, nil
}

func (store *Store) seal(
	stream string,
	schema string,
	plaintext []byte,
) ([]byte, error) {
	return store.vault.MaybeSealRecord(vault.AD{
		User: store.user, Store: "neo.turnstate",
		Stream: stream, Schema: schema,
	}, plaintext)
}

func (store *Store) open(
	stream string,
	schema string,
	envelope []byte,
) ([]byte, error) {
	defer zero(envelope)
	return store.vault.UserVault().OpenRecord(vault.AD{
		User: store.user, Store: "neo.turnstate",
		Stream: stream, Schema: schema,
	}, envelope)
}

func (store *Store) writer() {
	defer close(store.writerDone)
	for request := range store.writes {
		if err := request.ctx.Err(); err != nil {
			request.result <- err
			continue
		}
		request.result <- runWithBusyRetry(
			request.ctx, store.writeDB, request.run,
		)
	}
}

func (store *Store) submit(
	ctx context.Context,
	run func(context.Context, *sql.DB) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request := writeRequest{
		ctx: ctx, run: run, result: make(chan error, 1),
	}
	store.lifecycleMu.RLock()
	defer store.lifecycleMu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	select {
	case store.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-request.result
}

func (store *Store) checkOpen() error {
	store.lifecycleMu.RLock()
	defer store.lifecycleMu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	return nil
}

func (store *Store) Close(ctx context.Context) error {
	store.lifecycleMu.Lock()
	if store.closed {
		store.lifecycleMu.Unlock()
		return nil
	}
	store.closed = true
	close(store.writes)
	store.lifecycleMu.Unlock()
	select {
	case <-store.writerDone:
	case <-ctx.Done():
		return fmt.Errorf("turnstate: wait for writer: %w", ctx.Err())
	}
	var closeErrors []error
	if _, err := store.writeDB.ExecContext(ctx,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	); err != nil {
		closeErrors = append(closeErrors,
			fmt.Errorf("turnstate: checkpoint WAL: %w", err))
	}
	if err := store.readDB.Close(); err != nil {
		closeErrors = append(closeErrors,
			fmt.Errorf("turnstate: close readers: %w", err))
	}
	if err := store.writeDB.Close(); err != nil {
		closeErrors = append(closeErrors,
			fmt.Errorf("turnstate: close writer: %w", err))
	}
	return errors.Join(closeErrors...)
}

func runWithBusyRetry(
	ctx context.Context,
	db *sql.DB,
	run func(context.Context, *sql.DB) error,
) error {
	for attempt := 0; attempt <= maxBusyRetries; attempt++ {
		err := run(ctx, db)
		if !isSQLiteBusy(err) || attempt == maxBusyRetries {
			return err
		}
		timer := time.NewTimer(
			10 * time.Millisecond * time.Duration(1<<attempt),
		)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
	return fmt.Errorf("turnstate: unreachable busy retry state")
}

func isSQLiteBusy(err error) bool {
	var sqliteError *sqliteDriver.Error
	if !errors.As(err, &sqliteError) {
		return false
	}
	code := sqliteError.Code() & 0xff
	return code == 5 || code == 6
}

func zero(content []byte) {
	for index := range content {
		content[index] = 0
	}
}
