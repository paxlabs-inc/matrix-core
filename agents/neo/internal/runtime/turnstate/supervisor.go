// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type SupervisorRecord struct {
	RunID   string
	ActorID string
	Status  string
	State   json.RawMessage
}

func (store *Store) SaveSupervisorRun(
	ctx context.Context,
	record SupervisorRecord,
) error {
	record.RunID = strings.TrimSpace(record.RunID)
	record.ActorID = strings.TrimSpace(record.ActorID)
	record.Status = strings.TrimSpace(record.Status)
	if record.RunID == "" || record.ActorID == "" ||
		record.Status == "" || len(record.State) == 0 ||
		!json.Valid(record.State) {
		return fmt.Errorf("turnstate: valid supervisor record is required")
	}
	envelope, err := store.seal(
		record.RunID, "supervisor-run.v1", record.State,
	)
	if err != nil {
		return fmt.Errorf("turnstate: seal supervisor run: %w", err)
	}
	defer zero(envelope)
	now := store.now().UTC()
	return store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(runCtx,
			`INSERT INTO supervisor_runs(
			   run_id, actor_id, status, state, updated_at
			 ) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(run_id) DO UPDATE SET
			   actor_id = excluded.actor_id,
			   status = excluded.status,
			   state = excluded.state,
			   updated_at = excluded.updated_at`,
			record.RunID, record.ActorID, record.Status,
			envelope, now.UnixMicro(),
		)
		if err != nil {
			return fmt.Errorf("turnstate: save supervisor run: %w", err)
		}
		return nil
	})
}

func (store *Store) LoadSupervisorRun(
	ctx context.Context,
	runID string,
) (SupervisorRecord, error) {
	if err := store.checkOpen(); err != nil {
		return SupervisorRecord{}, err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return SupervisorRecord{}, fmt.Errorf(
			"turnstate: supervisor run ID is required",
		)
	}
	var record SupervisorRecord
	var envelope []byte
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT actor_id, status, state
		 FROM supervisor_runs WHERE run_id = ?`,
		runID,
	).Scan(&record.ActorID, &record.Status, &envelope); err != nil {
		return SupervisorRecord{}, fmt.Errorf(
			"turnstate: load supervisor run: %w", err,
		)
	}
	state, err := store.open(runID, "supervisor-run.v1", envelope)
	if err != nil {
		return SupervisorRecord{}, fmt.Errorf(
			"turnstate: open supervisor run: %w", err,
		)
	}
	if !json.Valid(state) {
		zero(state)
		return SupervisorRecord{}, fmt.Errorf(
			"turnstate: supervisor run state is not JSON",
		)
	}
	record.RunID = runID
	record.State = state
	return record, nil
}

func (store *Store) ListSupervisorRuns(
	ctx context.Context,
	actorID string,
) ([]SupervisorRecord, error) {
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return nil, fmt.Errorf("turnstate: supervisor actor is required")
	}
	rows, err := store.readDB.QueryContext(ctx,
		`SELECT run_id, status, state
		 FROM supervisor_runs
		 WHERE actor_id = ?
		 ORDER BY updated_at, run_id`,
		actorID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"turnstate: list supervisor runs: %w", err,
		)
	}
	defer rows.Close()
	var records []SupervisorRecord
	for rows.Next() {
		var record SupervisorRecord
		var envelope []byte
		if err := rows.Scan(
			&record.RunID, &record.Status, &envelope,
		); err != nil {
			return nil, fmt.Errorf(
				"turnstate: scan supervisor run: %w", err,
			)
		}
		state, err := store.open(
			record.RunID, "supervisor-run.v1", envelope,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"turnstate: open supervisor run: %w", err,
			)
		}
		if !json.Valid(state) {
			zero(state)
			return nil, fmt.Errorf(
				"turnstate: supervisor run state is not JSON",
			)
		}
		record.ActorID = actorID
		record.State = state
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"turnstate: iterate supervisor runs: %w", err,
		)
	}
	return records, nil
}

func (store *Store) ListAllSupervisorRuns(
	ctx context.Context,
) ([]SupervisorRecord, error) {
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := store.readDB.QueryContext(ctx,
		`SELECT run_id, actor_id, status, state
		 FROM supervisor_runs
		 ORDER BY updated_at, run_id`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"turnstate: list all supervisor runs: %w", err,
		)
	}
	defer rows.Close()
	var records []SupervisorRecord
	for rows.Next() {
		var record SupervisorRecord
		var envelope []byte
		if err := rows.Scan(
			&record.RunID, &record.ActorID, &record.Status, &envelope,
		); err != nil {
			return nil, fmt.Errorf(
				"turnstate: scan supervisor run: %w", err,
			)
		}
		state, err := store.open(
			record.RunID, "supervisor-run.v1", envelope,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"turnstate: open supervisor run: %w", err,
			)
		}
		if !json.Valid(state) {
			zero(state)
			return nil, fmt.Errorf(
				"turnstate: supervisor run state is not JSON",
			)
		}
		record.State = state
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"turnstate: iterate supervisor runs: %w", err,
		)
	}
	return records, nil
}
