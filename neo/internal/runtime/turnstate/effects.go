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

	"matrix/neo/internal/runtime/records"
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

type logicalTurnContextKey struct{}

func ContextWithLogicalTurn(ctx context.Context, logicalTurnID string) context.Context {
	return context.WithValue(ctx, logicalTurnContextKey{}, strings.TrimSpace(logicalTurnID))
}

func LogicalTurnFromContext(ctx context.Context) string {
	value, _ := ctx.Value(logicalTurnContextKey{}).(string)
	return strings.TrimSpace(value)
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
	turn, err := store.LoadTurnRecord(ctx, turnID)
	if err != nil {
		return err
	}
	path, err := compatibilityStatePath(turn.CurrentState, records.StateAwaitingTools)
	if err != nil {
		return err
	}
	for _, next := range path {
		if err := store.TransitionTurn(ctx, turnID, next, nil); err != nil {
			return err
		}
	}
	cyclePayload, err := json.Marshal(compatibilityCycle{Checkpoint: &checkpoint})
	if err != nil {
		return err
	}
	cycle := records.CycleRecord{
		GenerationNumber: uint64(checkpoint.Step), ProviderRequest: cyclePayload,
		ContextManifest: records.ContextManifest{}, StreamedOutputState: []records.StreamedOutput{},
		ProposedToolCalls:    []records.ProposedToolCall{{CallID: checkpoint.PendingCall.CallID, Operation: toolName, NormalizedArguments: arguments}},
		ObservedToolOutcomes: []records.ToolOutcome{}, NextIntendedAction: "dispatch pending effect",
	}
	canonicalEffect := records.EffectRecord{
		Operation: toolName, NormalizedArguments: append(json.RawMessage(nil), arguments...),
		SideEffectClass:     records.SideEffectNonIdempotentReconciliable,
		IdempotencyStrategy: idempotencyKey, ReconciliationStrategy: "authoritative-effect-state",
		EffectState: records.EffectStarted,
	}
	if retrySafe {
		canonicalEffect.SideEffectClass = records.SideEffectReadOnly
	}
	cycleBytes, err := records.EncodeCycle(cycle)
	if err != nil {
		return err
	}
	effectBytes, err := records.EncodeEffect(canonicalEffect)
	if err != nil {
		return err
	}
	cycleKey := fmt.Sprint(cycle.GenerationNumber)
	cycleEnvelope, err := store.sealCanonical(turnID, records.KindCycle, cycleKey, cycleBytes)
	if err != nil {
		return err
	}
	defer zero(cycleEnvelope)
	effectEnvelope, err := store.sealCanonical(turnID, records.KindEffect, idempotencyKey, effectBytes)
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

		turnBytes, err := store.loadCanonicalTx(runCtx, tx, turnID, records.KindTurn, "current")
		if err != nil {
			return err
		}
		turnRecord, err := records.DecodeTurn(turnBytes)
		if err != nil {
			return err
		}
		if turnRecord.CurrentState != records.StateAwaitingTools {
			return fmt.Errorf("turnstate: pending effect requires AwaitingTools")
		}
		turnRecord.CurrentState = records.StateExecutingTools
		turnReplacement, err := records.EncodeTurn(turnRecord)
		if err != nil {
			return err
		}
		turnEnvelope, err := store.sealCanonical(turnID, records.KindTurn, "current", turnReplacement)
		if err != nil {
			return err
		}
		defer zero(turnEnvelope)
		now := store.now().UTC().UnixMicro()
		var existingEnvelope []byte
		existingErr := tx.QueryRowContext(runCtx,
			`SELECT state FROM canonical_records
			 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
			turnID, string(records.KindEffect), idempotencyKey,
		).Scan(&existingEnvelope)
		if existingErr == nil {
			existingBytes, openErr := store.openCanonical(turnID, records.KindEffect, idempotencyKey, existingEnvelope)
			if openErr != nil {
				return openErr
			}
			existing, decodeErr := records.DecodeEffect(existingBytes)
			if decodeErr != nil {
				return decodeErr
			}
			if existing.Operation != canonicalEffect.Operation ||
				string(existing.NormalizedArguments) != string(canonicalEffect.NormalizedArguments) ||
				existing.SideEffectClass != canonicalEffect.SideEffectClass {
				return fmt.Errorf("turnstate: effect idempotency conflict")
			}
		} else if !errors.Is(existingErr, sql.ErrNoRows) {
			return fmt.Errorf("turnstate: inspect canonical effect: %w", existingErr)
		}
		if _, err := tx.ExecContext(runCtx,
			`INSERT INTO canonical_records(logical_turn_id, record_type, record_key, state, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(logical_turn_id, record_type, record_key) DO UPDATE SET state=excluded.state, updated_at=excluded.updated_at`,
			turnID, string(records.KindCycle), cycleKey, cycleEnvelope, now,
		); err != nil {
			return fmt.Errorf("turnstate: save pending cycle: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`INSERT INTO canonical_records(logical_turn_id, record_type, record_key, state, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(logical_turn_id, record_type, record_key) DO UPDATE SET state=excluded.state, updated_at=excluded.updated_at`,
			turnID, string(records.KindEffect), idempotencyKey, effectEnvelope, now,
		); err != nil {
			return fmt.Errorf("turnstate: save pending effect: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`UPDATE canonical_records SET state=?, updated_at=?
			 WHERE logical_turn_id=? AND record_type=? AND record_key=?`,
			turnEnvelope, now, turnID, string(records.KindTurn), "current",
		); err != nil {
			return fmt.Errorf("turnstate: transition pending effect: %w", err)
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
	_, existing, err := store.loadCanonicalEffectByIdentity(ctx, idempotencyKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			turnID, _ := ctx.Value(logicalTurnContextKey{}).(string)
			if turnID == "" {
				turnID, err = store.singleCanonicalTurn(ctx)
				if err != nil {
					return fmt.Errorf("turnstate: effect must be prepared by atomic dispatch: %w", err)
				}
			}
			class := records.SideEffectNonIdempotentReconciliable
			if retrySafe {
				class = records.SideEffectReadOnly
			}
			return store.SaveEffectRecord(ctx, turnID, idempotencyKey, records.EffectRecord{
				Operation: toolName, NormalizedArguments: append(json.RawMessage(nil), arguments...),
				SideEffectClass: class, IdempotencyStrategy: idempotencyKey,
				ReconciliationStrategy: "authoritative-effect-state", EffectState: records.EffectStarted,
			})
		}
		return err
	}
	wantClass := records.SideEffectNonIdempotentReconciliable
	if retrySafe {
		wantClass = records.SideEffectReadOnly
	}
	if existing.Operation != toolName || string(existing.NormalizedArguments) != string(arguments) || existing.SideEffectClass != wantClass {
		return fmt.Errorf("turnstate: effect idempotency conflict")
	}
	return nil
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
	turnID, _, err := store.loadCanonicalEffectByIdentity(ctx, idempotencyKey)
	if err != nil {
		return err
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
		encoded, err := store.loadCanonicalTx(runCtx, tx, turnID, records.KindEffect, idempotencyKey)
		if err != nil {
			return fmt.Errorf("turnstate: load effect for completion: %w", err)
		}
		record, err := records.DecodeEffect(encoded)
		if err != nil {
			return err
		}
		outcome := canonicalOutcome(result, idempotencyKey)
		if record.EffectState == records.EffectCompleted {
			if record.Result == nil || !canonicalOutcomeEqual(*record.Result, outcome) {
				return fmt.Errorf("turnstate: effect completion conflict")
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("turnstate: commit existing completion: %w", err)
			}
			committed = true
			return nil
		}
		record.EffectState = records.EffectCompleted
		record.Result = &outcome
		replacement, err := records.EncodeEffect(record)
		if err != nil {
			return err
		}
		effectEnvelope, err := store.sealCanonical(turnID, records.KindEffect, idempotencyKey, replacement)
		if err != nil {
			return err
		}
		defer zero(effectEnvelope)
		now := store.now().UTC()
		if _, err := tx.ExecContext(runCtx,
			`UPDATE canonical_records SET state = ?, updated_at = ?
			 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
			effectEnvelope, now.UnixMicro(), turnID, string(records.KindEffect), idempotencyKey,
		); err != nil {
			return fmt.Errorf("turnstate: update effect completion: %w", err)
		}
		turnBytes, err := store.loadCanonicalTx(runCtx, tx, turnID, records.KindTurn, "current")
		if err != nil {
			return err
		}
		turn, err := records.DecodeTurn(turnBytes)
		if err != nil {
			return err
		}
		if turn.CurrentState == records.StateExecutingTools {
			turn.CurrentState = records.StateSynthesisOwed
			turn.SynthesisDebt.Owed = true
			turn.SynthesisDebt.UnconsumedEvidence = appendUniqueString(turn.SynthesisDebt.UnconsumedEvidence, idempotencyKey+":result")
			turnReplacement, err := records.EncodeTurn(turn)
			if err != nil {
				return err
			}
			turnEnvelope, err := store.sealCanonical(turnID, records.KindTurn, "current", turnReplacement)
			if err != nil {
				return err
			}
			defer zero(turnEnvelope)
			if _, err := tx.ExecContext(runCtx,
				`UPDATE canonical_records SET state = ?, updated_at = ?
				 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
				turnEnvelope, now.UnixMicro(), turnID, string(records.KindTurn), "current",
			); err != nil {
				return fmt.Errorf("turnstate: persist synthesis debt: %w", err)
			}
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
	_, canonical, err := store.loadCanonicalEffectByIdentity(ctx, idempotencyKey)
	if err == nil {
		return compatibilityEffect(idempotencyKey, canonical), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EffectRecord{}, err
	}
	var envelope []byte
	if err := store.readDB.QueryRowContext(ctx, `SELECT state FROM effect_state WHERE idempotency_key = ?`, idempotencyKey).Scan(&envelope); err != nil {
		return EffectRecord{}, fmt.Errorf("turnstate: load legacy effect: %w", err)
	}
	return store.openEffect(idempotencyKey, envelope)
}

func (store *Store) loadCanonicalEffectByIdentity(ctx context.Context, idempotencyKey string) (string, records.EffectRecord, error) {
	var turnID string
	var sealed []byte
	err := store.readDB.QueryRowContext(ctx,
		`SELECT logical_turn_id, state FROM canonical_records
		 WHERE record_type = ? AND record_key = ? ORDER BY updated_at DESC LIMIT 1`,
		string(records.KindEffect), idempotencyKey,
	).Scan(&turnID, &sealed)
	if err != nil {
		return "", records.EffectRecord{}, err
	}
	plaintext, err := store.openCanonical(turnID, records.KindEffect, idempotencyKey, sealed)
	if err != nil {
		return "", records.EffectRecord{}, err
	}
	record, err := records.DecodeEffect(plaintext)
	return turnID, record, err
}

func (store *Store) singleCanonicalTurn(ctx context.Context) (string, error) {
	rows, err := store.readDB.QueryContext(ctx,
		`SELECT logical_turn_id FROM canonical_records
		 WHERE record_type = ? AND record_key = ? ORDER BY logical_turn_id LIMIT 2`,
		string(records.KindTurn), "current",
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("logical turn identity is ambiguous")
	}
	return ids[0], nil
}

func canonicalOutcome(result EffectResult, idempotencyKey string) records.ToolOutcome {
	layer := records.FailureNone
	status := records.EffectCompleted
	if result.IsError {
		layer = records.FailureTool
	}
	return records.ToolOutcome{
		FailureLayer: layer, Retryable: result.Retryable, EffectStatus: status,
		Evidence:        []records.EvidenceReference{{Identity: idempotencyKey + ":result", Kind: "tool-result", Content: append(json.RawMessage(nil), result.Content...)}},
		NormalizedCause: result.FailureClass, SuggestedRecovery: result.FailureMessage,
	}
}

func canonicalOutcomeEqual(left, right records.ToolOutcome) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func compatibilityEffect(idempotencyKey string, record records.EffectRecord) EffectRecord {
	result := (*EffectResult)(nil)
	if record.Result != nil {
		content := json.RawMessage(`null`)
		if len(record.Result.Evidence) > 0 && len(record.Result.Evidence[0].Content) > 0 {
			content = append(json.RawMessage(nil), record.Result.Evidence[0].Content...)
		}
		result = &EffectResult{
			Content: content, IsError: record.Result.FailureLayer != records.FailureNone,
			FailureClass: string(record.Result.FailureLayer), Retryable: record.Result.Retryable,
			FailureMessage: record.Result.SuggestedRecovery,
		}
	}
	status := EffectStarted
	if record.EffectState == records.EffectCompleted {
		status = EffectCompleted
	}
	now := time.Unix(0, 1).UTC()
	return EffectRecord{
		IdempotencyKey: idempotencyKey, ToolName: record.Operation,
		ArgumentsHash: sha256.Sum256(record.NormalizedArguments),
		RetrySafe:     record.SideEffectClass == records.SideEffectReadOnly,
		Status:        status, Result: result, StartedAt: now,
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
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
