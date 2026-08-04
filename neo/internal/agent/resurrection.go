// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"matrix/neo/internal/config"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/runtime/belief"
	"matrix/neo/internal/runtime/controller"
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
	return a.chatResurrectionWithGuidance(ctx, userInput, "")
}

func (a *Agent) chatResurrectionResume(
	ctx context.Context,
	objective string,
	guidance string,
) error {
	return a.chatResurrectionWithGuidance(ctx, objective, guidance)
}

func (a *Agent) chatResurrectionWithGuidance(
	ctx context.Context,
	userInput string,
	resumeGuidance string,
) error {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return nil
	}
	a.runtimeIncomplete = nil
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
	var activation loop.ActivationSource
	var recorder loop.TurnRecorder
	var evidence loop.EvidenceJournal
	var checkpoints loop.CheckpointStore = a.runtime.store
	var citationVerifier belief.CitationVerifier = a.runtime.pager.Cortex()
	var deliveryConsolidator loop.Consolidator = a.consolidator
	var neocortexAdapter *loop.NeocortexAdapter
	var recoveryCheckpoint *turnstate.Checkpoint
	if a.cfg.MemorySubstrate == config.SubstrateNeocortex {
		turnID = a.turnIntentID()
		seam, seamErr := a.runtime.pager.NewNeocortexLoopSeam(
			conversationID, a.cfg.ActivationBudget(),
		)
		if seamErr != nil {
			a.runtimeFailure = delegate.ClassDeterministic
			return seamErr
		}
		adapter, adapterErr := loop.NewNeocortexAdapter(seam)
		if adapterErr != nil {
			a.runtimeFailure = delegate.ClassDeterministic
			return adapterErr
		}
		neocortexAdapter = adapter
		activation, recorder, evidence = adapter, adapter, adapter
		citationVerifier = adapter
		deliveryConsolidator = adapter
		checkpointStore := &loop.NeocortexCheckpointStore{
			Seam: seam, Turns: a.runtime.store,
		}
		checkpoints = checkpointStore
		if strings.TrimSpace(resumeGuidance) != "" {
			state, loadErr := checkpointStore.LoadTurnState(ctx, turnID)
			switch {
			case loadErr == nil && state.Checkpoint != nil:
				recoveryCheckpoint = state.Checkpoint
			case loadErr == nil:
				a.runtimeFailure = delegate.ClassDeterministic
				return fmt.Errorf(
					"neo: neocortex recovery checkpoint is missing",
				)
			case !errors.Is(loadErr, sql.ErrNoRows):
				a.runtimeFailure = delegate.ClassDeterministic
				return loadErr
			}
		}
	} else {
		cortexAdapter, adapterErr := loop.NewCortexAdapter(
			a.runtime.pager, a.cfg, conversationID,
		)
		if adapterErr != nil {
			a.runtimeFailure = delegate.ClassDeterministic
			return adapterErr
		}
		activation, recorder = cortexAdapter, cortexAdapter
		evidence = &loop.CortexToolJournal{
			Cortex:    a.runtime.pager.Cortex(),
			CreatedBy: actorID,
		}
	}
	if recoveryCheckpoint == nil {
		if err := a.runtime.store.CreateTurnState(
			ctx,
			turnstate.TurnState{
				TurnID: turnID, ActorID: actorID,
				SessionID: conversationID, Content: userInput,
				Origin: func() string {
					if strings.TrimSpace(resumeGuidance) != "" {
						return "supervisor_resume"
					}
					return "user"
				}(),
				Status:    turnstate.StatusRunning,
				UpdatedAt: time.Now().UTC(),
			},
		); err != nil {
			a.runtimeFailure = delegate.ClassDeterministic
			return err
		}
	}
	beliefState, err := a.openBeliefState(
		ctx, conversationID, citationVerifier,
	)
	if err != nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return err
	}
	silentVoice := controller.New(controller.Cadence{}, nil, nil)
	observer := loop.NewReporterObserver(a.out, a.turnSeq-1)
	completionGate := loop.NewEvidenceCompletionGate(
		loop.RequestRequiresToolEvidence(userInput),
	)
	scopedTools := resurrectionToolSurface{
		parent: a.runtime.tools, allowed: a.advertised,
	}
	runtimeLoop, err := loop.New(
		a.runtime.generator,
		scopedTools,
		checkpoints,
		loop.Config{
			TurnID:         turnID,
			ConversationID: conversationID,
			ProjectRoot: func() string {
				if strings.TrimSpace(a.wsProjectRoot) != "" {
					return a.wsProjectRoot
				}
				return a.wsRoot
			}(),
			Model:           a.cfg.MainModel,
			SystemPrompt:    a.stableSystem(),
			MaxOutputTokens: 8192,
			MaxToolCalls:    resurrectionToolLimit(a.cfg),
			IdleTimeout:     a.cfg.RuntimeProvider.IdleTimeout,
			MaxTurnTokens:   a.cfg.ContextWindowTokens,
		},
		loop.Dependencies{
			Observer:        observer,
			CompletionGate:  completionGate,
			Activation:      activation,
			Recorder:        recorder,
			EvidenceJournal: evidence,
			EvidenceObserver: runtimeEvidenceObserver{
				ui:         runtimeToolObserver{observer: a.observer},
				belief:     beliefState,
				completion: completionGate,
			},
			Premises: beliefPremises{state: beliefState},
			Liveness: beliefState,
			Doubt:    silentVoice,
			Subgoals: rootSubgoal{},
			Delivery: &loop.DeliveryChoke{
				AgentName: a.agentName(), Reporter: a.out,
				Recorder: recorder, Consolidator: deliveryConsolidator,
				SuppressIncomplete: true,
				IntentID:           a.turnIntentID(), Attempt: a.supervisedAttempt,
			},
		},
	)
	if err != nil {
		a.runtimeFailure = delegate.ClassDeterministic
		return err
	}
	var response loop.Response
	var turnErr error
	history := resurrectionProtocolHistory(a.recentHistory)
	if len(history) == 0 {
		history = resurrectionProtocolHistory(a.working)
	}
	if recoveryCheckpoint != nil {
		response, turnErr = runtimeLoop.Resume(
			ctx, userInput, *recoveryCheckpoint,
		)
	} else if strings.TrimSpace(resumeGuidance) != "" {
		response, turnErr = runtimeLoop.TurnInternalWithHistory(
			ctx, userInput, history, resumeGuidance,
		)
	} else {
		response, turnErr = runtimeLoop.TurnWithHistory(ctx, userInput, history)
	}
	if neocortexAdapter != nil {
		if recordErr := neocortexAdapter.RecordError(); recordErr != nil {
			if turnErr == nil {
				turnErr = recordErr
			} else {
				turnErr = errors.Join(turnErr, recordErr)
			}
		}
	}
	a.runtimeLast = strings.TrimSpace(response.Content)
	if a.runtimeLast == "" {
		a.runtimeLast = runtimeBestEffort(response)
	}
	if turnErr == nil {
		if response.HonestPartial {
			a.runtimeIncomplete = &loop.IncompleteRecord{
				TurnID: turnID, ConversationID: conversationID,
				Phase: "honest_partial", AttemptCount: response.ProviderCalls,
				RecoveryAdvice: "resume_from_saved_partial",
				Repairs:        response.Repairs,
			}
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
		a.runtimeIncomplete = &loop.IncompleteRecord{
			TurnID: turnID, ConversationID: conversationID,
			Phase:            incomplete.Phase,
			LastToolEvidence: append(json.RawMessage(nil), incomplete.LastToolEvidence...),
			StartedAt:        incomplete.StartedAt,
			AttemptCount:     incomplete.AttemptCount,
			RecoveryAdvice:   incomplete.RecoveryAdvice,
			Repairs:          incomplete.Repairs,
			CauseDetail:      incomplete.CauseDetail,
			ProviderError:    incomplete.ProviderError,
		}
		return fmt.Errorf("%w: %v", ErrIncomplete, incomplete)
	}
	return turnErr
}

// LastRuntimeIncomplete returns the exact typed runtime failure behind the
// supervisor's user-facing status. It intentionally excludes checkpoints and
// tool payloads while preserving the phase, repair counters, and provider
// error needed to diagnose a cascading failure.
func (a *Agent) LastRuntimeIncomplete() (loop.IncompleteRecord, bool) {
	if a == nil || a.runtimeIncomplete == nil {
		return loop.IncompleteRecord{}, false
	}
	record := *a.runtimeIncomplete
	record.LastToolEvidence = append(
		json.RawMessage(nil), a.runtimeIncomplete.LastToolEvidence...,
	)
	return record, true
}

func resurrectionProtocolHistory(history []llm.Message) []protocol.Message {
	result := make([]protocol.Message, 0, len(history))
	for _, message := range history {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		switch message.Role {
		case llm.RoleUser:
			result = append(result, protocol.Message{
				Role: protocol.RoleUser, Content: content,
			})
		case llm.RoleAssistant:
			result = append(result, protocol.Message{
				Role: protocol.RoleAssistant, Content: content,
			})
		}
	}
	return result
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

// openBeliefState restores this conversation's durable cognition so premises,
// predictions, and the measured liveness context survive the turn boundary. A
// conversation with no cognition row yet starts from the empty state; any other
// load failure is real and must not be papered over with a blank ledger.
func (a *Agent) openBeliefState(
	ctx context.Context,
	conversationID string,
	verifier belief.CitationVerifier,
) (*belief.State, error) {
	state, err := belief.New(
		conversationID, verifier, a.runtime.store,
	)
	if err != nil {
		return nil, err
	}
	if err := state.Restore(ctx); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return state, nil
}

// runtimeEvidenceObserver fans one committed execution out to the UI stream and
// then to the belief state. The UI leg is presentation and never fails the
// turn; the belief leg is fail-closed, so an execution whose citation does not
// verify against the cortex journal stops the turn rather than updating
// cognition from unanchored evidence.
type runtimeEvidenceObserver struct {
	ui         loop.EvidenceObserver
	belief     loop.EvidenceObserver
	completion loop.EvidenceObserver
}

func (observer runtimeEvidenceObserver) ObserveToolExecution(
	ctx context.Context,
	execution loop.ToolExecution,
) error {
	if observer.ui != nil {
		_ = observer.ui.ObserveToolExecution(ctx, execution)
	}
	if observer.belief != nil {
		if err := observer.belief.ObserveToolExecution(ctx, execution); err != nil {
			return err
		}
	}
	if observer.completion != nil {
		return observer.completion.ObserveToolExecution(ctx, execution)
	}
	return nil
}

// activePremiseLimit bounds how much of the ledger reaches the window. The
// unsupported set is what raises verification depth, so the newest entries are
// the ones worth carrying.
const activePremiseLimit = 12

// beliefPremises is the loop's PremiseSource: the premises the belief state
// holds no verified citation for. They steer memory activation and land in the
// window carrying their status, so an assumption is never read as a fact.
type beliefPremises struct {
	state *belief.State
}

func (source beliefPremises) ActivePremises() []string {
	if source.state == nil {
		return nil
	}
	snapshot := source.state.Snapshot()
	ids := make([]string, 0, len(snapshot.Premises))
	for id, premise := range snapshot.Premises {
		if premise.Status != belief.PremiseCited &&
			strings.TrimSpace(premise.Statement) != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > activePremiseLimit {
		ids = ids[len(ids)-activePremiseLimit:]
	}
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		premise := snapshot.Premises[id]
		result = append(result, string(premise.Status)+": "+
			strings.TrimSpace(premise.Statement))
	}
	return result
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
