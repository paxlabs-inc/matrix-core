// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"matrix/neo/internal/config"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	runtimeprovider "matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
	"matrix/neo/internal/tools"
)

type ResurrectionRuntime struct {
	generator *runtimeprovider.MiMoGenerator
	tools     *loop.ToolManagerAdapter
	store     *turnstate.Store
	journal   *loop.DurableEffectJournal
	pager     *memory.Pager

	closeOnce sync.Once
	closeErr  error
}

func OpenResurrectionRuntime(
	ctx context.Context,
	cfg config.Config,
	manager *tools.Manager,
	pager *memory.Pager,
	statePath string,
) (*ResurrectionRuntime, error) {
	if manager == nil || pager == nil || pager.Cortex() == nil {
		return nil, fmt.Errorf(
			"neo: resurrection runtime requires tools and cortex",
		)
	}
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return nil, fmt.Errorf(
			"neo: resurrection turnstate path is required",
		)
	}
	store, err := turnstate.Open(ctx, statePath, cfg.Vault)
	if err != nil {
		return nil, err
	}
	adapter := &runtimeprovider.MiMoAdapter{}
	gateway := strings.TrimSpace(cfg.RuntimeProvider.GatewayURL)
	if gateway == "" {
		gateway = strings.TrimSpace(cfg.GatewayURL)
	}
	client, err := runtimeprovider.New(
		adapter,
		runtimeprovider.Config{
			GatewayURL:     gateway,
			BearerEnv:      cfg.RuntimeProvider.BearerEnv,
			ActorDID:       cfg.ActorDID,
			SlotLabel:      "neo",
			MaxAttempts:    cfg.RuntimeProvider.MaxAttempts,
			BackoffInitial: cfg.RuntimeProvider.BackoffInitial,
			BackoffMax:     cfg.RuntimeProvider.BackoffMax,
			IdleTimeout:    cfg.RuntimeProvider.IdleTimeout,
		},
	)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		_ = store.Close(closeCtx)
		return nil, err
	}
	generator, err := runtimeprovider.NewMiMoGenerator(client, adapter)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		_ = store.Close(closeCtx)
		return nil, err
	}
	journal := &loop.DurableEffectJournal{Store: store}
	toolManager, err := loop.NewToolManagerAdapter(manager, journal)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		_ = store.Close(closeCtx)
		return nil, err
	}
	return &ResurrectionRuntime{
		generator: generator,
		tools:     toolManager,
		store:     store,
		journal:   journal,
		pager:     pager,
	}, nil
}

func (runtime *ResurrectionRuntime) Close(
	ctx context.Context,
) error {
	if runtime == nil || runtime.store == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		runtime.closeErr = runtime.store.Close(ctx)
	})
	return runtime.closeErr
}

func (a *Agent) chatResurrection(
	ctx context.Context,
	userInput string,
) error {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return nil
	}
	if a.runtimeErr != nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return a.runtimeErr
	}
	if a.runtime == nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return fmt.Errorf(
			"neo: resurrection runtime is unavailable",
		)
	}
	a.turnSeq++
	turnID := uuid.NewString()
	conversationID := strings.TrimSpace(a.convID)
	if conversationID == "" {
		conversationID = "cli"
	}
	actorID := strings.TrimSpace(a.cfg.ActorDID)
	if actorID == "" {
		actorID = strings.TrimSpace(a.cfg.VaultUser)
	}
	if actorID == "" {
		a.runtimeFailure = delegate.ClassDeterministic
		return fmt.Errorf(
			"neo: resurrection runtime actor is unavailable",
		)
	}
	if err := a.runtime.store.CreateTurnState(
		ctx,
		turnstate.TurnState{
			TurnID: turnID, ActorID: actorID,
			SessionID: conversationID, Content: userInput,
			Status:    turnstate.StatusRunning,
			UpdatedAt: time.Now().UTC(),
		},
	); err != nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return err
	}
	cortexAdapter, err := loop.NewCortexAdapter(
		a.runtime.pager, a.cfg, conversationID,
	)
	if err != nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return err
	}
	observer := loop.NewReporterObserver(a.out, a.turnSeq-1)
	scopedTools := resurrectionToolSurface{
		parent: a.runtime.tools, allowed: a.advertised,
	}
	runtimeLoop, err := loop.New(
		a.runtime.generator,
		scopedTools,
		a.runtime.store,
		loop.Config{
			TurnID:          turnID,
			ConversationID:  conversationID,
			Model:           a.cfg.MainModel,
			SystemPrompt:    a.stableSystem(),
			MaxOutputTokens: 8192,
			MaxToolCalls:    resurrectionToolLimit(a.cfg),
			IdleTimeout:     a.cfg.RuntimeProvider.IdleTimeout,
			MaxTurnTokens:   a.cfg.ContextWindowTokens,
		},
		loop.Dependencies{
			Observer:   observer,
			Activation: cortexAdapter,
			Recorder:   cortexAdapter,
			EvidenceJournal: &loop.CortexToolJournal{
				Cortex:    a.runtime.pager.Cortex(),
				CreatedBy: actorID,
			},
			EvidenceObserver: runtimeToolObserver{
				observer: a.observer,
			},
			Subgoals: rootSubgoal{},
			Delivery: &loop.DeliveryChoke{
				AgentName: a.agentName(), Reporter: a.out,
				Recorder: cortexAdapter, Consolidator: a.consolidator,
				SuppressIncomplete: true,
			},
		},
	)
	if err != nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return err
	}
	response, turnErr := runtimeLoop.Turn(ctx, userInput)
	a.runtimeLast = strings.TrimSpace(response.Content)
	if a.runtimeLast == "" {
		a.runtimeLast = runtimeBestEffort(response)
	}
	if turnErr == nil {
		if response.HonestPartial {
			a.runtimeFailure = delegate.ClassDeterministic
			return fmt.Errorf(
				"%w: the bounded runtime delivered saved partial work",
				ErrIncomplete,
			)
		}
		a.runtimeFailure = delegate.ClassNone
		return nil
	}
	a.runtimeFailure = runtimeFailureClass(turnErr)
	var incomplete *loop.Incomplete
	if errors.As(turnErr, &incomplete) {
		return fmt.Errorf("%w: %v", ErrIncomplete, incomplete)
	}
	return turnErr
}

type resurrectionToolSurface struct {
	parent  loop.ToolManager
	allowed map[string]struct{}
}

func (surface resurrectionToolSurface) Surface(
	ctx context.Context,
) []protocol.ToolDefinition {
	source := surface.parent.Surface(ctx)
	result := make([]protocol.ToolDefinition, 0, len(source))
	for _, definition := range source {
		if _, ok := surface.allowed[definition.Name]; ok {
			result = append(result, definition)
		}
	}
	return result
}

func (surface resurrectionToolSurface) Execute(
	ctx context.Context,
	call protocol.NormalizedToolCall,
	idempotencyKey string,
) (loop.ToolResult, error) {
	if _, ok := surface.allowed[call.Name]; !ok {
		return loop.ToolResult{}, fmt.Errorf(
			"neo: tool %q is outside this agent surface", call.Name,
		)
	}
	return surface.parent.Execute(ctx, call, idempotencyKey)
}

func (surface resurrectionToolSurface) Reconcile(
	ctx context.Context,
	idempotencyKey string,
) (loop.ReconcileResult, error) {
	return surface.parent.Reconcile(ctx, idempotencyKey)
}

func (a *Agent) chatAudioResurrection(
	ctx context.Context,
	userInput string,
	audio *AudioTurn,
) error {
	if audio == nil {
		return a.chatResurrection(ctx, userInput)
	}
	transcript := ""
	if audio.Transcript != nil {
		select {
		case result := <-audio.Transcript:
			if result.Err == nil {
				transcript = strings.TrimSpace(result.Text)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	durable := durableAudioText(transcript, audio.Ref)
	if audio.OnTranscript != nil {
		audio.OnTranscript(durable)
	}
	return a.chatResurrection(ctx, durable)
}

type runtimeToolObserver struct {
	observer ToolObserver
}

func (observer runtimeToolObserver) ObserveToolExecution(
	_ context.Context,
	execution loop.ToolExecution,
) error {
	if observer.observer == nil {
		return nil
	}
	args := map[string]interface{}{}
	_ = json.Unmarshal(execution.Call.Arguments, &args)
	id := strings.TrimSpace(execution.Call.ID)
	if id == "" {
		id = execution.IdempotencyKey
	}
	observer.observer(ToolEvent{
		ID: id, Name: execution.Call.Name, Args: args,
		Phase: ToolStart,
	})
	observer.observer(ToolEvent{
		ID: id, Name: execution.Call.Name, Args: args,
		Result: string(execution.Result),
		IsErr:  execution.Error != "",
		Phase:  ToolEnd,
	})
	return nil
}

type rootSubgoal struct{}

func (rootSubgoal) SubgoalFor(
	protocol.NormalizedToolCall,
) string {
	return "root"
}

func resurrectionToolLimit(cfg config.Config) int {
	if cfg.StepBudgetMax > 0 {
		return cfg.StepBudgetMax
	}
	if cfg.StepBudget > 0 {
		return cfg.StepBudget
	}
	return 50
}

func runtimeBestEffort(response loop.Response) string {
	for index := len(response.ToolEvents) - 1; index >= 0; index-- {
		event := response.ToolEvents[index]
		if len(event.Result) > 0 {
			return strings.TrimSpace(string(event.Result))
		}
	}
	return ""
}

func runtimeFailureClass(err error) delegate.FailureClass {
	switch {
	case runtimeprovider.IsFailureKind(
		err, runtimeprovider.FailureRejected,
	), runtimeprovider.IsFailureKind(
		err, runtimeprovider.FailureProtocol,
	):
		return delegate.ClassDeterministic
	default:
		return delegate.ClassTransient
	}
}
