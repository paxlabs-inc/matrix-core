// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
)

type LoopFactory func(
	context.Context,
	TaskPacket,
	uuid.UUID,
	ToolManager,
	loop.GuidanceSource,
) (*loop.Loop, error)

type LoopExecutor struct {
	Parent   ToolManager
	Metadata ToolMetadata
	Store    TurnRuntimeStore
	NewLoop  LoopFactory
}

type TurnRuntimeStore interface {
	loop.CheckpointStore
	CreateTurnState(context.Context, turnstate.TurnState) error
	LoadTurnState(context.Context, string) (turnstate.TurnState, error)
}

func (executor *LoopExecutor) Surface(
	ctx context.Context,
) []protocol.ToolDefinition {
	if executor == nil || executor.Parent == nil {
		return nil
	}
	return executor.Parent.Surface(ctx)
}

func (executor *LoopExecutor) Execute(
	ctx context.Context,
	packet TaskPacket,
	attemptID uuid.UUID,
	steering <-chan SteeringRecord,
) (WorkerResult, error) {
	if executor == nil || executor.Parent == nil ||
		executor.Metadata == nil || executor.Store == nil ||
		executor.NewLoop == nil {
		return WorkerResult{}, fmt.Errorf(
			"runtime work: specialist loop executor is unavailable",
		)
	}
	if attemptID == uuid.Nil {
		return WorkerResult{}, fmt.Errorf(
			"runtime work: specialist attempt ID is required",
		)
	}
	userContent := specialistPrompt(packet)
	durable, loadErr := executor.Store.LoadTurnState(
		ctx, attemptID.String(),
	)
	resume := loadErr == nil && durable.Checkpoint != nil
	if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
		return WorkerResult{}, loadErr
	}
	if errors.Is(loadErr, sql.ErrNoRows) {
		if err := executor.Store.CreateTurnState(
			ctx,
			turnstate.TurnState{
				TurnID:    attemptID.String(),
				ActorID:   packet.ActorID,
				SessionID: packet.SupervisorID.String(),
				Content:   userContent,
				Status:    turnstate.StatusRunning,
				UpdatedAt: time.Now().UTC(),
			},
		); err != nil {
			return WorkerResult{}, err
		}
	}
	scoped, err := NewScopedToolManager(
		executor.Parent, packet, executor.Metadata,
	)
	if err != nil {
		return WorkerResult{}, err
	}
	runtimeLoop, err := executor.NewLoop(
		ctx, packet, attemptID, scoped,
		steeringGuidance{records: steering},
	)
	if err != nil {
		return WorkerResult{}, err
	}
	if runtimeLoop == nil {
		return WorkerResult{}, fmt.Errorf(
			"runtime work: specialist loop factory returned nil",
		)
	}
	started := time.Now()
	var response loop.Response
	var turnErr error
	if resume {
		response, turnErr = runtimeLoop.Resume(
			ctx, userContent, *durable.Checkpoint,
		)
	} else {
		response, turnErr = runtimeLoop.Turn(ctx, userContent)
	}
	result := WorkerResult{
		AttemptID: attemptID,
		WorkerID: packet.SupervisorID.String() + "/" +
			packet.ID + "/" + attemptID.String(),
		Status:   TaskCompleted,
		Progress: 100,
		Summary:  boundedText(response.Content),
		Usage: BudgetUsage{
			Tokens:             int64(response.Usage.TotalTokens),
			ModelMicrocents:    response.Usage.ModelCostMicrocents,
			ModelCostKnown:     response.Usage.ModelCostKnown,
			ProviderMicrocents: response.Usage.ProviderSpendMicrocents,
			ProviderSpendKnown: response.Usage.ProviderSpendKnown,
			ToolCalls:          len(response.ToolEvents),
			WallSeconds: int(time.Since(started).Round(time.Second) /
				time.Second),
		},
	}
	criteria := make(map[string]struct{}, len(packet.Criteria))
	for _, criterion := range packet.Criteria {
		criteria[criterion] = struct{}{}
	}
	for _, event := range response.ToolEvents {
		if event.Citation != nil {
			if _, ok := criteria[event.SubgoalID]; ok {
				result.Evidence = append(result.Evidence, EvidenceRef{
					Criterion: event.SubgoalID,
					Citation:  *event.Citation,
				})
			}
		}
		class := packet.ToolClasses[event.Call.Name]
		if class != "read" {
			state := "completed"
			if event.Error != "" || event.Citation == nil {
				state = "outcome_unknown"
			}
			result.ExternalEffects = append(
				result.ExternalEffects,
				ExternalEffect{
					ToolName:       event.Call.Name,
					IdempotencyKey: event.IdempotencyKey,
					State:          state,
				},
			)
		}
	}
	if turnErr != nil {
		result.Status = TaskRetrying
		result.Progress = min(99, progressFromResponse(response))
		var incomplete *loop.Incomplete
		if errors.As(turnErr, &incomplete) &&
			incomplete.Checkpoint.PendingCall != nil {
			pending := incomplete.Checkpoint.PendingCall
			if packet.ToolClasses[pending.ToolName] != "read" {
				result.ExternalEffects = append(
					result.ExternalEffects,
					ExternalEffect{
						ToolName:       pending.ToolName,
						IdempotencyKey: pending.IdempotencyKey,
						State:          "started",
					},
				)
			}
		}
	}
	return result, turnErr
}

type steeringGuidance struct {
	records <-chan SteeringRecord
}

func (source steeringGuidance) DrainGuidance() []string {
	var guidance []string
	for {
		select {
		case record, ok := <-source.records:
			if !ok {
				return guidance
			}
			if instruction := strings.TrimSpace(record.Instruction); instruction != "" {
				guidance = append(guidance, instruction)
			}
		default:
			return guidance
		}
	}
}

func specialistPrompt(packet TaskPacket) string {
	var builder strings.Builder
	builder.WriteString(packet.Prompt)
	builder.WriteString("\n\nThe immutable delegated work packet is ")
	builder.WriteString(packet.ID)
	builder.WriteString(". Finish only with current tool evidence anchored to every criterion: ")
	builder.WriteString(strings.Join(packet.Criteria, ", "))
	builder.WriteString(". Stay within the advertised tool subset and authority scope.")
	return builder.String()
}

func progressFromResponse(response loop.Response) int {
	if len(response.ToolEvents) == 0 {
		return 5
	}
	return min(90, 10+len(response.ToolEvents)*10)
}

var _ Executor = (*LoopExecutor)(nil)
