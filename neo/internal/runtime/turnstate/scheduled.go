// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"matrix/neo/internal/runtime/protocol"
)

type WakeKind string

const (
	WakeHeartbeat    WakeKind = "HEARTBEAT"
	WakeAutomatrix   WakeKind = "AUTOMATRIX"
	WakeMorningBrief WakeKind = "MORNING_BRIEF"
)

type ScheduledWake struct {
	AlarmID      string
	ActorID      string
	SessionID    string
	Message      string
	ScheduledFor time.Time
}

type TurnMessage struct {
	MessageID string               `json:"message_id"`
	TurnID    string               `json:"turn_id"`
	SessionID string               `json:"session_id"`
	Role      protocol.MessageRole `json:"role"`
	Content   string               `json:"content"`
	CreatedAt time.Time            `json:"created_at"`
}

func ClassifyWake(message string) (WakeKind, error) {
	upper := strings.ToUpper(message)
	switch {
	case strings.Contains(upper, string(WakeMorningBrief)):
		return WakeMorningBrief, nil
	case strings.Contains(upper, string(WakeAutomatrix)):
		return WakeAutomatrix, nil
	case strings.Contains(upper, string(WakeHeartbeat)):
		return WakeHeartbeat, nil
	default:
		return "", fmt.Errorf("turnstate: unsupported scheduled wake convention")
	}
}

func OccurrenceID(
	kind WakeKind,
	alarmID string,
	scheduledFor time.Time,
) (string, error) {
	alarmID = strings.TrimSpace(alarmID)
	if alarmID == "" || scheduledFor.IsZero() {
		return "", fmt.Errorf("turnstate: alarm and scheduled time are required")
	}
	switch kind {
	case WakeHeartbeat, WakeAutomatrix, WakeMorningBrief:
	default:
		return "", fmt.Errorf("turnstate: invalid wake kind %q", kind)
	}
	canonical := string(kind) + "\x00" + alarmID + "\x00" +
		scheduledFor.UTC().Format(time.RFC3339Nano)
	digest := sha256.Sum256([]byte(canonical))
	return "scheduled-" + hex.EncodeToString(digest[:16]), nil
}

func (store *Store) CreateScheduledTurn(
	ctx context.Context,
	wake ScheduledWake,
) (TurnState, bool, error) {
	kind, err := ClassifyWake(wake.Message)
	if err != nil {
		return TurnState{}, false, err
	}
	occurrenceID, err := OccurrenceID(
		kind, wake.AlarmID, wake.ScheduledFor,
	)
	if err != nil {
		return TurnState{}, false, err
	}
	now := store.now().UTC()
	state := TurnState{
		TurnID: occurrenceID, ActorID: strings.TrimSpace(wake.ActorID),
		SessionID: strings.TrimSpace(wake.SessionID),
		Content:   strings.TrimSpace(wake.Message),
		Origin:    "chronos:" + string(kind),
		Status:    StatusRunning, UpdatedAt: now,
	}
	if err := state.Validate(); err != nil {
		return TurnState{}, false, err
	}
	message := TurnMessage{
		MessageID: occurrenceID + "-user",
		TurnID:    occurrenceID, SessionID: state.SessionID,
		Role: protocol.RoleUser, Content: state.Content, CreatedAt: now,
	}
	stateEnvelope, err := store.sealTurn(state)
	if err != nil {
		return TurnState{}, false, err
	}
	defer zero(stateEnvelope)
	messagePlaintext, err := json.Marshal(message)
	if err != nil {
		return TurnState{}, false,
			fmt.Errorf("turnstate: encode scheduled message: %w", err)
	}
	defer zero(messagePlaintext)
	messageEnvelope, err := store.seal(
		message.MessageID, "turn-message.v1", messagePlaintext,
	)
	if err != nil {
		return TurnState{}, false,
			fmt.Errorf("turnstate: seal scheduled message: %w", err)
	}
	defer zero(messageEnvelope)

	created := false
	err = store.submit(ctx, func(runCtx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(runCtx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin scheduled turn: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		var existing string
		err = tx.QueryRowContext(runCtx,
			`SELECT turn_id FROM scheduled_occurrences
			 WHERE occurrence_id = ?`, occurrenceID,
		).Scan(&existing)
		switch {
		case err == nil:
			return nil
		case !errorsIsNoRows(err):
			return fmt.Errorf("turnstate: inspect scheduled occurrence: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`INSERT INTO turn_state(
			 turn_id, actor_id, session_id, status, state, updated_at
			 ) VALUES (?, ?, ?, ?, ?, ?)`,
			state.TurnID, state.ActorID, state.SessionID, string(state.Status),
			stateEnvelope, state.UpdatedAt.UnixMicro(),
		); err != nil {
			return fmt.Errorf("turnstate: insert scheduled turn: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`INSERT INTO turn_messages(
			 message_id, turn_id, session_id, role, content, created_at
			 ) VALUES (?, ?, ?, ?, ?, ?)`,
			message.MessageID, message.TurnID, message.SessionID,
			string(message.Role), messageEnvelope, message.CreatedAt.UnixMicro(),
		); err != nil {
			return fmt.Errorf("turnstate: insert scheduled message: %w", err)
		}
		if _, err := tx.ExecContext(runCtx,
			`INSERT INTO scheduled_occurrences(
			 occurrence_id, kind, alarm_id, scheduled_for, turn_id
			 ) VALUES (?, ?, ?, ?, ?)`,
			occurrenceID, string(kind), strings.TrimSpace(wake.AlarmID),
			wake.ScheduledFor.UTC().UnixMicro(), state.TurnID,
		); err != nil {
			return fmt.Errorf("turnstate: insert scheduled occurrence: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit scheduled turn: %w", err)
		}
		committed = true
		created = true
		return nil
	})
	if err != nil {
		return TurnState{}, false, err
	}
	if !created {
		existing, err := store.LoadTurnState(ctx, occurrenceID)
		return existing, false, err
	}
	return state, true, nil
}

func (store *Store) ListTurnMessages(
	ctx context.Context,
	turnID string,
) ([]TurnMessage, error) {
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := store.readDB.QueryContext(ctx,
		`SELECT message_id, content FROM turn_messages
		 WHERE turn_id = ? ORDER BY created_at, message_id`,
		strings.TrimSpace(turnID),
	)
	if err != nil {
		return nil, fmt.Errorf("turnstate: list turn messages: %w", err)
	}
	defer rows.Close()
	var messages []TurnMessage
	for rows.Next() {
		var messageID string
		var envelope []byte
		if err := rows.Scan(&messageID, &envelope); err != nil {
			return nil, fmt.Errorf("turnstate: scan turn message: %w", err)
		}
		plaintext, err := store.open(
			messageID, "turn-message.v1", envelope,
		)
		if err != nil {
			return nil, fmt.Errorf("turnstate: open turn message: %w", err)
		}
		var message TurnMessage
		decodeErr := json.Unmarshal(plaintext, &message)
		zero(plaintext)
		if decodeErr != nil {
			return nil, fmt.Errorf("turnstate: decode turn message: %w", decodeErr)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("turnstate: iterate turn messages: %w", err)
	}
	return messages, nil
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}
