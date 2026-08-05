// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type EffectStatus string

const (
	EffectStarted   EffectStatus = "started"
	EffectCompleted EffectStatus = "completed"
)

type EffectResult struct {
	Content        json.RawMessage `json:"content"`
	IsError        bool            `json:"is_error"`
	FailureClass   string          `json:"failure_class,omitempty"`
	Retryable      bool            `json:"retryable,omitempty"`
	FailureMessage string          `json:"failure_message,omitempty"`
}

type EffectRecord struct {
	IdempotencyKey string        `json:"idempotency_key"`
	ToolName       string        `json:"tool_name"`
	ArgumentsHash  [32]byte      `json:"arguments_hash"`
	RetrySafe      bool          `json:"retry_safe"`
	Status         EffectStatus  `json:"status"`
	Result         *EffectResult `json:"result,omitempty"`
	StartedAt      time.Time     `json:"started_at"`
	CompletedAt    *time.Time    `json:"completed_at,omitempty"`
}

// SavePendingEffect is the production dispatch boundary: the turn checkpoint
// cannot contain a PendingCall unless the matching effect-start record exists,
// and an effect-start record for that pending call cannot commit alone.
func (store *Store) SavePendingEffect(
	ctx context.Context,
	turnID string,
	checkpoint Checkpoint,
	idempotencyKey string,
	toolName string,
	arguments json.RawMessage,
	retrySafe bool,
) error {
	turnID = strings.TrimSpace(turnID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	toolName = strings.TrimSpace(toolName)
	checkpoint.SavedAt = store.now().UTC()
	if turnID == "" || checkpoint.PendingCall == nil ||
		checkpoint.PendingCall.IdempotencyKey != idempotencyKey ||
		idempotencyKey == "" || toolName == "" || len(arguments) == 0 ||
		!json.Valid(arguments) {
		return fmt.Errorf("turnstate: valid atomic pending effect is required")
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	record := EffectRecord{
		IdempotencyKey: idempotencyKey,
		ToolName:       toolName,
		ArgumentsHash:  sha256.Sum256(arguments),
		RetrySafe:      retrySafe,
		Status:         EffectStarted,
		StartedAt:      store.now().UTC(),
	}
	effectEnvelope, err := store.sealEffect(record)
	if err != nil {
		return err
	}
	defer zero(effectEnvelope)
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin atomic pending effect: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		var turnEnvelope []byte
		if err := tx.QueryRowContext(runCtx,
			`SELECT state FROM turn_state WHERE turn_id = ?`, turnID,
		).Scan(&turnEnvelope); err != nil {
			return fmt.Errorf("turnstate: load turn for pending effect: %w", err)
		}
		state, err := store.openTurn(turnID, turnEnvelope)
		if err != nil {
			return err
		}
		state.Checkpoint = &checkpoint
		state.UpdatedAt = store.now().UTC()
		if err := state.Validate(); err != nil {
			return err
		}
		replacement, err := store.sealTurn(state)
		if err != nil {
			return err
		}
		defer zero(replacement)

		var existingEnvelope []byte
		err = tx.QueryRowContext(runCtx,
			`SELECT state FROM effect_state WHERE idempotency_key = ?`,
			idempotencyKey,
		).Scan(&existingEnvelope)
		switch {
		case err == nil:
			existing, openErr := store.openEffect(idempotencyKey, existingEnvelope)
			if openErr != nil {
				return openErr
			}
			if existing.ToolName != record.ToolName ||
				existing.ArgumentsHash != record.ArgumentsHash ||
				existing.RetrySafe != record.RetrySafe {
				return fmt.Errorf("turnstate: effect idempotency conflict")
			}
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(runCtx,
				`INSERT INTO effect_state(
				 idempotency_key, tool_name, status, state, updated_at
				 ) VALUES (?, ?, ?, ?, ?)`,
				record.IdempotencyKey, record.ToolName, string(record.Status),
				effectEnvelope, record.StartedAt.UnixMicro(),
			); err != nil {
				return fmt.Errorf("turnstate: insert pending effect: %w", err)
			}
		default:
			return fmt.Errorf("turnstate: inspect pending effect: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`UPDATE turn_state SET status = ?, state = ?, updated_at = ?
			 WHERE turn_id = ?`,
			string(state.Status), replacement, state.UpdatedAt.UnixMicro(), turnID,
		); err != nil {
			return fmt.Errorf("turnstate: save pending checkpoint: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit atomic pending effect: %w", err)
		}
		committed = true
		return nil
	})
}

func (store *Store) BeginEffect(
	ctx context.Context,
	idempotencyKey string,
	toolName string,
	arguments json.RawMessage,
	retrySafe bool,
) error {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	toolName = strings.TrimSpace(toolName)
	if idempotencyKey == "" || toolName == "" ||
		len(arguments) == 0 || !json.Valid(arguments) {
		return fmt.Errorf("turnstate: valid effect identity is required")
	}
	record := EffectRecord{
		IdempotencyKey: idempotencyKey,
		ToolName:       toolName,
		ArgumentsHash:  sha256.Sum256(arguments),
		RetrySafe:      retrySafe,
		Status:         EffectStarted,
		StartedAt:      store.now().UTC(),
	}
	envelope, err := store.sealEffect(record)
	if err != nil {
		return err
	}
	defer zero(envelope)
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin effect: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		var existingEnvelope []byte
		err = tx.QueryRowContext(runCtx,
			`SELECT state FROM effect_state WHERE idempotency_key = ?`,
			idempotencyKey,
		).Scan(&existingEnvelope)
		switch {
		case err == nil:
			existing, openErr := store.openEffect(idempotencyKey, existingEnvelope)
			if openErr != nil {
				return openErr
			}
			if existing.ToolName != record.ToolName ||
				existing.ArgumentsHash != record.ArgumentsHash ||
				existing.RetrySafe != record.RetrySafe {
				return fmt.Errorf("turnstate: effect idempotency conflict")
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("turnstate: commit existing effect: %w", commitErr)
			}
			committed = true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("turnstate: inspect effect: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`INSERT INTO effect_state(
			   idempotency_key, tool_name, status, state, updated_at
			 ) VALUES (?, ?, ?, ?, ?)`,
			record.IdempotencyKey, record.ToolName, string(record.Status),
			envelope, record.StartedAt.UnixMicro(),
		); err != nil {
			return fmt.Errorf("turnstate: insert effect: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit effect: %w", err)
		}
		committed = true
		return nil
	})
}

func (store *Store) CompleteEffect(
	ctx context.Context,
	idempotencyKey string,
	result EffectResult,
) error {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(result.Content) == 0 ||
		!json.Valid(result.Content) {
		return fmt.Errorf("turnstate: valid effect result is required")
	}
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin effect completion: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		var envelope []byte
		if err := tx.QueryRowContext(runCtx,
			`SELECT state FROM effect_state WHERE idempotency_key = ?`,
			idempotencyKey,
		).Scan(&envelope); err != nil {
			return fmt.Errorf("turnstate: load effect for completion: %w", err)
		}
		record, err := store.openEffect(idempotencyKey, envelope)
		if err != nil {
			return err
		}
		if record.Status == EffectCompleted {
			if record.Result == nil || !effectResultsEqual(*record.Result, result) {
				return fmt.Errorf("turnstate: effect completion conflict")
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("turnstate: commit existing completion: %w", err)
			}
			committed = true
			return nil
		}
		now := store.now().UTC()
		record.Status = EffectCompleted
		record.Result = &result
		record.CompletedAt = &now
		replacement, err := store.sealEffect(record)
		if err != nil {
			return err
		}
		defer zero(replacement)
		if _, err := tx.ExecContext(runCtx,
			`UPDATE effect_state
			 SET status = ?, state = ?, updated_at = ?
			 WHERE idempotency_key = ?`,
			string(record.Status), replacement, now.UnixMicro(), idempotencyKey,
		); err != nil {
			return fmt.Errorf("turnstate: update effect completion: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit effect completion: %w", err)
		}
		committed = true
		return nil
	})
}

func (store *Store) LoadEffect(
	ctx context.Context,
	idempotencyKey string,
) (EffectRecord, error) {
	if err := store.checkOpen(); err != nil {
		return EffectRecord{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return EffectRecord{}, fmt.Errorf("turnstate: effect key is required")
	}
	var envelope []byte
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT state FROM effect_state WHERE idempotency_key = ?`,
		idempotencyKey,
	).Scan(&envelope); err != nil {
		return EffectRecord{}, fmt.Errorf("turnstate: load effect: %w", err)
	}
	return store.openEffect(idempotencyKey, envelope)
}

func (store *Store) sealEffect(record EffectRecord) ([]byte, error) {
	plaintext, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("turnstate: encode effect: %w", err)
	}
	defer zero(plaintext)
	return store.seal(record.IdempotencyKey, "effect-state.v1", plaintext)
}

func (store *Store) openEffect(
	idempotencyKey string,
	envelope []byte,
) (EffectRecord, error) {
	plaintext, err := store.open(
		idempotencyKey, "effect-state.v1", envelope,
	)
	if err != nil {
		return EffectRecord{}, fmt.Errorf("turnstate: open effect: %w", err)
	}
	defer zero(plaintext)
	var record EffectRecord
	if err := json.Unmarshal(plaintext, &record); err != nil {
		return EffectRecord{}, fmt.Errorf("turnstate: decode effect: %w", err)
	}
	if record.IdempotencyKey != idempotencyKey ||
		record.ToolName == "" || record.StartedAt.IsZero() ||
		(record.Status != EffectStarted && record.Status != EffectCompleted) ||
		(record.Status == EffectCompleted &&
			(record.Result == nil || record.CompletedAt == nil)) {
		return EffectRecord{}, fmt.Errorf("turnstate: invalid effect record")
	}
	return record, nil
}

func effectResultsEqual(left EffectResult, right EffectResult) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil &&
		string(leftJSON) == string(rightJSON)
}
