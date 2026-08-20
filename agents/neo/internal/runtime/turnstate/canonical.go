// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"centra/agents/neo/internal/runtime/records"
)

const canonicalRecordSchema = "canonical-record.v1"

func (store *Store) CreateTurnRecord(ctx context.Context, record records.TurnRecord) error {
	if record.CurrentState != records.StateAccepted {
		return fmt.Errorf("turnstate: canonical turn must start Accepted")
	}
	if err := record.Validate(); err != nil {
		return err
	}
	encoded, err := records.EncodeTurn(record)
	if err != nil {
		return err
	}
	return store.insertCanonical(ctx, record.LogicalTurnID, records.KindTurn, "current", encoded)
}

func (store *Store) LoadTurnRecord(ctx context.Context, logicalTurnID string) (records.TurnRecord, error) {
	encoded, err := store.loadCanonical(ctx, logicalTurnID, records.KindTurn, "current")
	if err != nil {
		return records.TurnRecord{}, err
	}
	record, err := records.DecodeTurn(encoded)
	if err != nil {
		return records.TurnRecord{}, err
	}
	if err := record.Validate(); err != nil {
		return records.TurnRecord{}, err
	}
	return record, nil
}

func (store *Store) TransitionTurn(
	ctx context.Context,
	logicalTurnID string,
	next records.TurnState,
	mutate func(*records.TurnRecord) error,
) error {
	logicalTurnID = strings.TrimSpace(logicalTurnID)
	if logicalTurnID == "" || !next.Valid() {
		return fmt.Errorf("turnstate: valid canonical transition is required")
	}
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin canonical transition: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		encoded, err := store.loadCanonicalTx(runCtx, tx, logicalTurnID, records.KindTurn, "current")
		if err != nil {
			return err
		}
		record, err := records.DecodeTurn(encoded)
		if err != nil {
			return err
		}
		if !canonicalTransitionAllowed(record.CurrentState, next) {
			return fmt.Errorf("turnstate: invalid canonical transition %s -> %s", record.CurrentState, next)
		}
		record.CurrentState = next
		if mutate != nil {
			if err := mutate(&record); err != nil {
				return err
			}
		}
		if record.SynthesisDebt.Owed && (next == records.StateAnswerReady ||
			next == records.StateDelivering || next == records.StateDelivered) {
			return fmt.Errorf("turnstate: synthesis debt blocks terminal transition")
		}
		if err := record.Validate(); err != nil {
			return err
		}
		replacement, err := records.EncodeTurn(record)
		if err != nil {
			return err
		}
		sealed, err := store.sealCanonical(logicalTurnID, records.KindTurn, "current", replacement)
		if err != nil {
			return err
		}
		defer zero(sealed)
		if _, err := tx.ExecContext(runCtx,
			`UPDATE canonical_records SET state = ?, updated_at = ?
			 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
			sealed, store.now().UTC().UnixMicro(), logicalTurnID, string(records.KindTurn), "current",
		); err != nil {
			return fmt.Errorf("turnstate: update canonical turn: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit canonical transition: %w", err)
		}
		committed = true
		return nil
	})
}

func (store *Store) UpdateTurnRecord(ctx context.Context, logicalTurnID string, mutate func(*records.TurnRecord) error) error {
	logicalTurnID = strings.TrimSpace(logicalTurnID)
	if logicalTurnID == "" || mutate == nil {
		return fmt.Errorf("turnstate: canonical turn update is required")
	}
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin canonical update: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		encoded, err := store.loadCanonicalTx(runCtx, tx, logicalTurnID, records.KindTurn, "current")
		if err != nil {
			return err
		}
		record, err := records.DecodeTurn(encoded)
		if err != nil {
			return err
		}
		frozenState := record.CurrentState
		if err := mutate(&record); err != nil {
			return err
		}
		if record.CurrentState != frozenState {
			return fmt.Errorf("turnstate: UpdateTurnRecord cannot change state")
		}
		if err := record.Validate(); err != nil {
			return err
		}
		replacement, err := records.EncodeTurn(record)
		if err != nil {
			return err
		}
		sealed, err := store.sealCanonical(logicalTurnID, records.KindTurn, "current", replacement)
		if err != nil {
			return err
		}
		defer zero(sealed)
		if _, err := tx.ExecContext(runCtx,
			`UPDATE canonical_records SET state = ?, updated_at = ?
			 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
			sealed, store.now().UTC().UnixMicro(), logicalTurnID, string(records.KindTurn), "current",
		); err != nil {
			return fmt.Errorf("turnstate: update canonical turn record: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit canonical turn update: %w", err)
		}
		committed = true
		return nil
	})
}

func (store *Store) SaveCycleRecord(ctx context.Context, logicalTurnID string, record records.CycleRecord) error {
	encoded, err := records.EncodeCycle(record)
	if err != nil {
		return err
	}
	return store.upsertCanonical(ctx, logicalTurnID, records.KindCycle, strconv.FormatUint(record.GenerationNumber, 10), encoded)
}

func (store *Store) LoadCycleRecord(ctx context.Context, logicalTurnID string, generation uint64) (records.CycleRecord, error) {
	encoded, err := store.loadCanonical(ctx, logicalTurnID, records.KindCycle, strconv.FormatUint(generation, 10))
	if err != nil {
		return records.CycleRecord{}, err
	}
	return records.DecodeCycle(encoded)
}

func (store *Store) SaveContextManifest(ctx context.Context, logicalTurnID string, generation uint64, manifest records.ContextManifest) error {
	record, err := store.LoadCycleRecord(ctx, logicalTurnID, generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		record = records.CycleRecord{GenerationNumber: generation}
	}
	record.ContextManifest = manifest
	return store.SaveCycleRecord(ctx, logicalTurnID, record)
}

func (store *Store) AppendStreamOutput(ctx context.Context, logicalTurnID string, generation uint64, output records.StreamedOutput) error {
	record, err := store.LoadCycleRecord(ctx, logicalTurnID, generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		record = records.CycleRecord{GenerationNumber: generation}
	}
	record.StreamedOutputState = append(record.StreamedOutputState, output)
	return store.SaveCycleRecord(ctx, logicalTurnID, record)
}

func (store *Store) SaveEffectRecord(ctx context.Context, logicalTurnID, effectIdentity string, record records.EffectRecord) error {
	encoded, err := records.EncodeEffect(record)
	if err != nil {
		return err
	}
	return store.upsertCanonical(ctx, logicalTurnID, records.KindEffect, effectIdentity, encoded)
}

func (store *Store) LoadEffectRecord(ctx context.Context, logicalTurnID, effectIdentity string) (records.EffectRecord, error) {
	encoded, err := store.loadCanonical(ctx, logicalTurnID, records.KindEffect, effectIdentity)
	if err != nil {
		return records.EffectRecord{}, err
	}
	return records.DecodeEffect(encoded)
}

func (store *Store) SaveConvergenceRecord(ctx context.Context, logicalTurnID string, record records.ConvergenceRecord) error {
	encoded, err := records.EncodeConvergence(record)
	if err != nil {
		return err
	}
	return store.upsertCanonical(ctx, logicalTurnID, records.KindConvergence, "current", encoded)
}

func (store *Store) LoadConvergenceRecord(ctx context.Context, logicalTurnID string) (records.ConvergenceRecord, error) {
	encoded, err := store.loadCanonical(ctx, logicalTurnID, records.KindConvergence, "current")
	if err != nil {
		return records.ConvergenceRecord{}, err
	}
	return records.DecodeConvergence(encoded)
}

func (store *Store) SaveAnswerRecord(ctx context.Context, logicalTurnID, answerIdentity string, record records.AnswerRecord) error {
	encoded, err := records.EncodeAnswer(record)
	if err != nil {
		return err
	}
	return store.upsertCanonical(ctx, logicalTurnID, records.KindAnswer, answerIdentity, encoded)
}

func (store *Store) LoadAnswerRecord(ctx context.Context, logicalTurnID, answerIdentity string) (records.AnswerRecord, error) {
	encoded, err := store.loadCanonical(ctx, logicalTurnID, records.KindAnswer, answerIdentity)
	if err != nil {
		return records.AnswerRecord{}, err
	}
	return records.DecodeAnswer(encoded)
}

func (store *Store) SaveDeliveryRecord(ctx context.Context, logicalTurnID, deliveryIdentity string, record records.DeliveryRecord) error {
	encoded, err := records.EncodeDelivery(record)
	if err != nil {
		return err
	}
	return store.upsertCanonical(ctx, logicalTurnID, records.KindDelivery, deliveryIdentity, encoded)
}

func (store *Store) LoadDeliveryRecord(ctx context.Context, logicalTurnID, deliveryIdentity string) (records.DeliveryRecord, error) {
	encoded, err := store.loadCanonical(ctx, logicalTurnID, records.KindDelivery, deliveryIdentity)
	if err != nil {
		return records.DeliveryRecord{}, err
	}
	return records.DecodeDelivery(encoded)
}

func (store *Store) MarkAnswerReady(ctx context.Context, logicalTurnID, answerIdentity string) error {
	answerIdentity = strings.TrimSpace(answerIdentity)
	if answerIdentity == "" {
		return fmt.Errorf("turnstate: answer identity is required")
	}
	record, err := store.LoadTurnRecord(ctx, logicalTurnID)
	if err != nil {
		return err
	}
	if record.CurrentState == records.StateAnswerReady && record.AnswerIdentity == answerIdentity {
		return nil
	}
	path, err := compatibilityStatePath(record.CurrentState, records.StateAnswerReady)
	if err != nil {
		return err
	}
	for _, next := range path {
		var mutate func(*records.TurnRecord) error
		if next == records.StateAnswerReady {
			mutate = func(turn *records.TurnRecord) error {
				turn.AnswerIdentity = answerIdentity
				turn.SynthesisDebt = records.SynthesisDebt{}
				return nil
			}
		}
		if err := store.TransitionTurn(ctx, logicalTurnID, next, mutate); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) MarkDelivering(ctx context.Context, logicalTurnID, deliveryIdentity string) error {
	deliveryIdentity = strings.TrimSpace(deliveryIdentity)
	if deliveryIdentity == "" {
		return fmt.Errorf("turnstate: delivery identity is required")
	}
	record, err := store.LoadTurnRecord(ctx, logicalTurnID)
	if err != nil {
		return err
	}
	mutate := func(turn *records.TurnRecord) error {
		turn.DeliveryIdentity = deliveryIdentity
		turn.CumulativeBudgets.DeliveryAttempts++
		return nil
	}
	if record.CurrentState == records.StateDelivering {
		return store.UpdateTurnRecord(ctx, logicalTurnID, mutate)
	}
	return store.TransitionTurn(ctx, logicalTurnID, records.StateDelivering, mutate)
}

func (store *Store) MarkDeliveryRetry(ctx context.Context, logicalTurnID string) error {
	record, err := store.LoadTurnRecord(ctx, logicalTurnID)
	if err != nil {
		return err
	}
	if record.CurrentState == records.StateDeliveryRetry {
		return nil
	}
	return store.TransitionTurn(ctx, logicalTurnID, records.StateDeliveryRetry, nil)
}

func (store *Store) MarkDelivered(ctx context.Context, logicalTurnID string) error {
	record, err := store.LoadTurnRecord(ctx, logicalTurnID)
	if err != nil {
		return err
	}
	if record.CurrentState == records.StateDelivered {
		return nil
	}
	return store.TransitionTurn(ctx, logicalTurnID, records.StateDelivered, nil)
}

func (store *Store) upsertCanonical(ctx context.Context, logicalTurnID string, kind records.Kind, key string, encoded []byte) error {
	logicalTurnID = strings.TrimSpace(logicalTurnID)
	key = strings.TrimSpace(key)
	if logicalTurnID == "" || key == "" {
		return fmt.Errorf("turnstate: canonical record identity is required")
	}
	sealed, err := store.sealCanonical(logicalTurnID, kind, key, encoded)
	if err != nil {
		return err
	}
	defer zero(sealed)
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(runCtx,
			`INSERT INTO canonical_records(logical_turn_id, record_type, record_key, state, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(logical_turn_id, record_type, record_key) DO UPDATE SET
			   state = excluded.state, updated_at = excluded.updated_at`,
			logicalTurnID, string(kind), key, sealed, store.now().UTC().UnixMicro(),
		)
		if err != nil {
			return fmt.Errorf("turnstate: save canonical %s: %w", kind, err)
		}
		return nil
	})
}

func (store *Store) insertCanonical(ctx context.Context, logicalTurnID string, kind records.Kind, key string, encoded []byte) error {
	sealed, err := store.sealCanonical(logicalTurnID, kind, key, encoded)
	if err != nil {
		return err
	}
	defer zero(sealed)
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(runCtx,
			`INSERT INTO canonical_records(logical_turn_id, record_type, record_key, state, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			logicalTurnID, string(kind), key, sealed, store.now().UTC().UnixMicro(),
		)
		if err != nil {
			return fmt.Errorf("turnstate: create canonical %s: %w", kind, err)
		}
		return nil
	})
}

func (store *Store) loadCanonical(ctx context.Context, logicalTurnID string, kind records.Kind, key string) ([]byte, error) {
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	var sealed []byte
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT state FROM canonical_records
		 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
		logicalTurnID, string(kind), key,
	).Scan(&sealed); err != nil {
		return nil, fmt.Errorf("turnstate: load canonical %s: %w", kind, err)
	}
	return store.openCanonical(logicalTurnID, kind, key, sealed)
}

func (store *Store) loadCanonicalTx(ctx context.Context, tx *sql.Tx, logicalTurnID string, kind records.Kind, key string) ([]byte, error) {
	var sealed []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM canonical_records
		 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
		logicalTurnID, string(kind), key,
	).Scan(&sealed); err != nil {
		return nil, fmt.Errorf("turnstate: load canonical %s for update: %w", kind, err)
	}
	return store.openCanonical(logicalTurnID, kind, key, sealed)
}

func (store *Store) sealCanonical(logicalTurnID string, kind records.Kind, key string, plaintext []byte) ([]byte, error) {
	return store.seal(canonicalStream(logicalTurnID, kind, key), canonicalRecordSchema, plaintext)
}

func (store *Store) openCanonical(logicalTurnID string, kind records.Kind, key string, sealed []byte) ([]byte, error) {
	return store.open(canonicalStream(logicalTurnID, kind, key), canonicalRecordSchema, sealed)
}

func canonicalStream(logicalTurnID string, kind records.Kind, key string) string {
	return logicalTurnID + ":" + string(kind) + ":" + key
}

func canonicalTransitionAllowed(from, to records.TurnState) bool {
	allowed := map[records.TurnState]map[records.TurnState]bool{
		records.StateAccepted:               {records.StatePreparing: true},
		records.StatePreparing:              {records.StateGenerating: true, records.StateBlockedAwaitingPerson: true},
		records.StateGenerating:             {records.StateAwaitingTools: true, records.StateAnswerReady: true, records.StateRetryingGeneration: true, records.StateBlockedAwaitingPerson: true},
		records.StateAwaitingTools:          {records.StateExecutingTools: true},
		records.StateExecutingTools:         {records.StateSynthesisOwed: true, records.StateGenerating: true, records.StateReconciliationRequired: true},
		records.StateSynthesisOwed:          {records.StateGenerating: true},
		records.StateAnswerReady:            {records.StateDelivering: true},
		records.StateDelivering:             {records.StateDelivered: true, records.StateDeliveryRetry: true},
		records.StateDeliveryRetry:          {records.StateDelivering: true},
		records.StateReconciliationRequired: {records.StateExecutingTools: true, records.StateGenerating: true},
		records.StateRetryingGeneration:     {records.StateGenerating: true},
		records.StateBlockedAwaitingPerson:  {records.StatePreparing: true},
	}
	return allowed[from][to]
}
