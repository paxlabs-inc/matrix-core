// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/neo/internal/runtime/turnstate"
	"matrix/neo/internal/task"
)

type ResumeLoopFactory func(turnstate.TurnState) (*Loop, error)

type BootReconcileResult struct {
	Examined   int
	Resumed    int
	Completed  int
	Incomplete int
	Surfaced   int
}

type TaskLedgerReconciler struct {
	Tasks    *task.Store
	Turns    *turnstate.Store
	NewLoop  ResumeLoopFactory
	Reporter DeliveryReporter
}

func ResumeIncomplete(
	ctx context.Context,
	userContent string,
	incomplete *Incomplete,
	next *Loop,
) (Response, error) {
	if incomplete == nil || next == nil {
		return Response{}, fmt.Errorf(
			"runtime loop: respawn requires a typed incomplete and loop",
		)
	}
	if err := validateCheckpoint(
		strings.TrimSpace(userContent), incomplete.Checkpoint,
	); err != nil {
		return Response{}, fmt.Errorf(
			"runtime loop: reject invalid respawn checkpoint: %w", err,
		)
	}
	return next.Resume(ctx, userContent, incomplete.Checkpoint)
}

func (reconciler *TaskLedgerReconciler) Reconcile(
	ctx context.Context,
) (BootReconcileResult, error) {
	var result BootReconcileResult
	if reconciler == nil || reconciler.Tasks == nil ||
		!reconciler.Tasks.Enabled() {
		return result, nil
	}
	if reconciler.Turns == nil || reconciler.NewLoop == nil {
		return result, fmt.Errorf(
			"runtime loop: task reconciliation requires turnstate and loop factory",
		)
	}
	for _, active := range reconciler.Tasks.Running() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Examined++
		state, err := reconciler.Turns.LoadTurnState(ctx, active.RunID)
		if err != nil {
			reconciler.surfaceOrphan(
				"I couldn't recover the saved work for an interrupted task. " +
					"I stopped instead of starting it over and risking repeated actions.",
			)
			reconciler.Tasks.Finish(
				active.ConvID, active.RunID, task.StatusCeiling,
			)
			result.Surfaced++
			continue
		}
		switch state.Status {
		case turnstate.StatusCompleted:
			reconciler.Tasks.Finish(
				active.ConvID, active.RunID, task.StatusDone,
			)
			result.Completed++
			continue
		case turnstate.StatusCancelled, turnstate.StatusInterrupted:
			reconciler.Tasks.Finish(
				active.ConvID, active.RunID, task.StatusInterrupted,
			)
			continue
		case turnstate.StatusFailed:
			reconciler.surfaceOrphan(
				"I couldn't finish an interrupted task. Its completed work remains saved, but it needs a fresh direction before continuing.",
			)
			reconciler.Tasks.Finish(
				active.ConvID, active.RunID, task.StatusCeiling,
			)
			result.Surfaced++
			continue
		}
		incomplete, err := incompleteFromState(state)
		if err != nil {
			reconciler.surfaceOrphan(
				"I found an interrupted task, but its saved resume point is not safe to use. I stopped instead of repeating completed work.",
			)
			reconciler.Tasks.Finish(
				active.ConvID, active.RunID, task.StatusCeiling,
			)
			result.Surfaced++
			continue
		}
		next, err := reconciler.NewLoop(state)
		if err != nil {
			return result, fmt.Errorf(
				"runtime loop: construct resume loop for %s: %w",
				active.RunID, err,
			)
		}
		_ = reconciler.Turns.SetTurnRecovery(
			ctx, active.RunID, turnstate.StatusRecovering,
			turnstate.Recovery{
				Phase: incomplete.Phase,
				LastToolEvidence: append(
					[]byte(nil), incomplete.LastToolEvidence...,
				),
				StartedAt:    incomplete.StartedAt,
				AttemptCount: incomplete.AttemptCount,
				Advice:       incomplete.RecoveryAdvice,
			},
		)
		response, resumeErr := ResumeIncomplete(
			ctx, state.Content, incomplete, next,
		)
		result.Resumed++
		if resumeErr == nil {
			reconciler.Tasks.Finish(
				active.ConvID, active.RunID, task.StatusDone,
			)
			result.Completed++
			_ = response
			continue
		}
		var nextIncomplete *Incomplete
		if errors.As(resumeErr, &nextIncomplete) {
			reconciler.Tasks.Checkpoint(
				active.ConvID, active.RunID,
				nextIncomplete.AttemptCount,
				nextIncomplete.Phase+":"+nextIncomplete.RecoveryAdvice,
			)
			result.Incomplete++
			continue
		}
		reconciler.surfaceOrphan(
			"I couldn't safely resume an interrupted task. Its completed work remains saved, and I stopped without repeating any action.",
		)
		reconciler.Tasks.Finish(
			active.ConvID, active.RunID, task.StatusCeiling,
		)
		result.Surfaced++
	}
	return result, nil
}

func incompleteFromState(state turnstate.TurnState) (*Incomplete, error) {
	if state.Checkpoint == nil {
		return nil, fmt.Errorf("turn %s has no checkpoint", state.TurnID)
	}
	if err := validateCheckpoint(state.Content, *state.Checkpoint); err != nil {
		return nil, err
	}
	incomplete := &Incomplete{
		Phase:          "boot_reconciliation",
		StartedAt:      state.Checkpoint.SavedAt,
		AttemptCount:   state.Checkpoint.ProviderAttempts,
		RecoveryAdvice: "resume_from_checkpoint",
		Checkpoint:     *state.Checkpoint,
	}
	if state.Recovery != nil {
		incomplete.Phase = state.Recovery.Phase
		incomplete.StartedAt = state.Recovery.StartedAt
		incomplete.AttemptCount = state.Recovery.AttemptCount
		incomplete.RecoveryAdvice = state.Recovery.Advice
		incomplete.LastToolEvidence = append(
			[]byte(nil), state.Recovery.LastToolEvidence...,
		)
	}
	if incomplete.StartedAt.IsZero() {
		incomplete.StartedAt = time.Now().UTC()
	}
	if incomplete.AttemptCount < 1 {
		incomplete.AttemptCount = 1
	}
	return incomplete, nil
}

func (reconciler *TaskLedgerReconciler) surfaceOrphan(content string) {
	if reconciler.Reporter != nil {
		reconciler.Reporter.Say(content, false)
	}
}
