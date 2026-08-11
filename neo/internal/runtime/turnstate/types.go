// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"matrix/neo/internal/runtime/protocol"
)

type Status string

const (
	StatusRunning     Status = "running"
	StatusRecovering  Status = "recovering"
	StatusIncomplete  Status = "incomplete"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"
)

type PendingCall struct {
	CallID         string          `json:"call_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	ToolName       string          `json:"tool_name"`
	Arguments      json.RawMessage `json:"arguments"`
	Expect         string          `json:"expect,omitempty"`
	DispatchedAt   time.Time       `json:"dispatched_at"`
}

type CodingVerification struct {
	Command    string    `json:"command"`
	CWD        string    `json:"cwd,omitempty"`
	ExitCode   int       `json:"exit_code"`
	Succeeded  bool      `json:"succeeded"`
	TimedOut   bool      `json:"timed_out,omitempty"`
	VerifiedAt time.Time `json:"verified_at"`
}

type CodingCheckpoint struct {
	ProjectRoot      string              `json:"project_root"`
	Requirements     []string            `json:"requirements"`
	FilesChanged     []string            `json:"files_changed,omitempty"`
	LastVerification *CodingVerification `json:"last_verification,omitempty"`
	CurrentFailures  []string            `json:"current_failures,omitempty"`
	NextAction       string              `json:"next_action"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

type Checkpoint struct {
	Messages         []protocol.Message `json:"messages"`
	ToolEvents       []json.RawMessage  `json:"tool_events,omitempty"`
	Step             int                `json:"step"`
	ProviderAttempts int                `json:"provider_attempts"`
	Plan             json.RawMessage    `json:"plan,omitempty"`
	Revision         json.RawMessage    `json:"revision,omitempty"`
	Runtime          json.RawMessage    `json:"runtime,omitempty"`
	Coding           *CodingCheckpoint  `json:"coding,omitempty"`
	PendingCall      *PendingCall       `json:"pending_call,omitempty"`
	SavedAt          time.Time          `json:"saved_at"`
}

type Recovery struct {
	Phase            string          `json:"phase"`
	LastToolEvidence json.RawMessage `json:"last_tool_evidence,omitempty"`
	StartedAt        time.Time       `json:"started_at"`
	AttemptCount     int             `json:"attempt_count"`
	Advice           string          `json:"advice"`
}

type TurnState struct {
	TurnID      string      `json:"turn_id"`
	ActorID     string      `json:"actor_id"`
	SessionID   string      `json:"session_id"`
	Content     string      `json:"content"`
	Surface     string      `json:"surface,omitempty"`
	Origin      string      `json:"origin,omitempty"`
	Status      Status      `json:"status"`
	Checkpoint  *Checkpoint `json:"checkpoint,omitempty"`
	Recovery    *Recovery   `json:"recovery,omitempty"`
	FailureCode string      `json:"failure_code,omitempty"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

func (state TurnState) Validate() error {
	if strings.TrimSpace(state.TurnID) == "" ||
		strings.TrimSpace(state.ActorID) == "" ||
		strings.TrimSpace(state.SessionID) == "" ||
		strings.TrimSpace(state.Content) == "" ||
		!state.Status.Valid() ||
		state.UpdatedAt.IsZero() {
		return fmt.Errorf("turnstate: invalid durable turn state")
	}
	if state.Checkpoint != nil {
		if err := state.Checkpoint.Validate(); err != nil {
			return err
		}
	}
	if state.Recovery != nil {
		if err := state.Recovery.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (status Status) Valid() bool {
	switch status {
	case StatusRunning, StatusRecovering, StatusIncomplete, StatusCompleted,
		StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	default:
		return false
	}
}

func (checkpoint Checkpoint) Validate() error {
	if checkpoint.Step < 0 || checkpoint.ProviderAttempts < 0 ||
		checkpoint.SavedAt.IsZero() {
		return fmt.Errorf("turnstate: invalid checkpoint counters")
	}
	for _, event := range checkpoint.ToolEvents {
		if len(event) == 0 || !json.Valid(event) {
			return fmt.Errorf("turnstate: invalid checkpoint tool event")
		}
	}
	for _, value := range []json.RawMessage{
		checkpoint.Plan, checkpoint.Revision, checkpoint.Runtime,
	} {
		if len(value) > 0 && !json.Valid(value) {
			return fmt.Errorf("turnstate: invalid checkpoint JSON")
		}
	}
	if checkpoint.PendingCall != nil {
		if err := checkpoint.PendingCall.Validate(); err != nil {
			return err
		}
	}
	if checkpoint.Coding != nil {
		if strings.TrimSpace(checkpoint.Coding.ProjectRoot) == "" ||
			len(checkpoint.Coding.Requirements) == 0 ||
			strings.TrimSpace(checkpoint.Coding.NextAction) == "" ||
			checkpoint.Coding.UpdatedAt.IsZero() {
			return fmt.Errorf("turnstate: invalid coding checkpoint")
		}
		if checkpoint.Coding.LastVerification != nil && checkpoint.Coding.LastVerification.VerifiedAt.IsZero() {
			return fmt.Errorf("turnstate: invalid coding verification")
		}
	}
	return nil
}

func (checkpoint Checkpoint) ValidateForResume() error {
	return checkpoint.Validate()
}

func (call PendingCall) Validate() error {
	if strings.TrimSpace(call.CallID) == "" ||
		strings.TrimSpace(call.IdempotencyKey) == "" ||
		strings.TrimSpace(call.ToolName) == "" ||
		len(call.Arguments) == 0 ||
		!json.Valid(call.Arguments) ||
		call.DispatchedAt.IsZero() {
		return fmt.Errorf("turnstate: invalid pending call")
	}
	return nil
}

func (recovery Recovery) Validate() error {
	if strings.TrimSpace(recovery.Phase) == "" ||
		recovery.StartedAt.IsZero() ||
		recovery.AttemptCount < 1 ||
		strings.TrimSpace(recovery.Advice) == "" {
		return fmt.Errorf("turnstate: invalid recovery")
	}
	if len(recovery.LastToolEvidence) > 0 &&
		!json.Valid(recovery.LastToolEvidence) {
		return fmt.Errorf("turnstate: invalid recovery evidence")
	}
	return nil
}
