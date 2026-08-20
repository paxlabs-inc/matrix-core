// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"centra/agents/neo/internal/runtime/turnstate"
)

type EffectStateStore interface {
	BeginEffect(
		context.Context,
		string,
		string,
		json.RawMessage,
		bool,
	) error
	CompleteEffect(context.Context, string, turnstate.EffectResult) error
	LoadEffect(context.Context, string) (turnstate.EffectRecord, error)
}

type DurableEffectJournal struct {
	Store EffectStateStore
}

func (journal *DurableEffectJournal) BeginEffect(
	ctx context.Context,
	idempotencyKey string,
	toolName string,
	arguments json.RawMessage,
	retrySafe bool,
) error {
	if journal == nil || journal.Store == nil {
		return fmt.Errorf("runtime loop: effect store unavailable")
	}
	return journal.Store.BeginEffect(
		ctx, idempotencyKey, toolName, arguments, retrySafe,
	)
}

func (journal *DurableEffectJournal) CompleteEffect(
	ctx context.Context,
	idempotencyKey string,
	result ToolResult,
) error {
	if journal == nil || journal.Store == nil {
		return fmt.Errorf("runtime loop: effect store unavailable")
	}
	return journal.Store.CompleteEffect(
		ctx,
		idempotencyKey,
		turnstate.EffectResult{
			Content:        append(json.RawMessage(nil), result.Content...),
			IsError:        result.IsError,
			FailureClass:   result.FailureClass,
			Retryable:      result.Retryable,
			FailureMessage: result.FailureMessage,
		},
	)
}

func (journal *DurableEffectJournal) ReconcileEffect(
	ctx context.Context,
	idempotencyKey string,
) (ReconcileResult, error) {
	if journal == nil || journal.Store == nil {
		return ReconcileResult{}, fmt.Errorf(
			"runtime loop: effect store unavailable",
		)
	}
	record, err := journal.Store.LoadEffect(ctx, idempotencyKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReconcileResult{Status: ReconcileNotStarted}, nil
		}
		return ReconcileResult{}, err
	}
	if record.Status == turnstate.EffectStarted {
		if record.RetrySafe {
			return ReconcileResult{Status: ReconcileRetrySafe}, nil
		}
		return ReconcileResult{Status: ReconcileUnknown}, nil
	}
	if record.Status != turnstate.EffectCompleted || record.Result == nil {
		return ReconcileResult{Status: ReconcileUnknown}, nil
	}
	return ReconcileResult{
		Status: ReconcileCompleted,
		Result: ToolResult{
			Content: append(
				json.RawMessage(nil), record.Result.Content...,
			),
			IsError:        record.Result.IsError,
			FailureClass:   record.Result.FailureClass,
			Retryable:      record.Result.Retryable,
			FailureMessage: record.Result.FailureMessage,
		},
	}, nil
}

var _ EffectReconciler = (*DurableEffectJournal)(nil)
