// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/cortexclient"
	"matrix/neo/internal/runtime/address"
	runtimecompose "matrix/neo/internal/runtime/compose"
	"matrix/neo/internal/runtime/converge"
	"matrix/neo/internal/runtime/liveness"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/records"
	runtimestream "matrix/neo/internal/runtime/stream"
	"matrix/neo/internal/runtime/turnstate"
)

const (
	defaultMaxToolCalls       = 128
	defaultIdleTimeout        = 2 * time.Minute
	defaultRepairLimit        = 2
	defaultEmptyRepairLimit   = 1
	defaultCompletionDeferral = 8

	// defaultMaxTurnTokens bounds the cumulative tokens ONE turn may merge
	// across every provider call it makes. It sits below the recorded ~1M
	// non-convergence burn while leaving headroom for a genuinely long turn on
	// the 1M window. Exhaustion is never a failure: it routes to the honest
	// partial, which delivers the work already done.
	defaultMaxTurnTokens = 750_000
)

const (
	textualToolRepairPrompt = "The previous provider response used invalid textual tool-call markup. Do not repeat or quote that markup. Either emit a native structured tool call that satisfies the active JSON Schema, or return a normal final answer with no tool-call tags."
	textualToolFinalPrompt  = "Tool use is now disabled because repeated responses used unsafe tool-call markup. Give the user an honest ordinary-text answer from the available conversation and evidence. State any action you could not complete. Do not emit or quote tool names, tool-call tags, function tags, parameter tags, XML, JSON, or code fences."
	expectationRepairPrompt = "The previous uncertain network/search probe was not executed because it omitted its non-empty expect hypothesis. Resubmit that probe with a one-line predicted outcome shape, or answer normally without tools. Deterministic file, mutation, and shell calls do not use expect."
	expectationFinalPrompt  = "Tool use is now disabled because repeated uncertain probes omitted their required expect hypothesis. Give the user an honest ordinary-text answer from the available conversation and evidence, and state any action you could not complete. Do not call tools."
	emptyFinalRepairPrompt  = "The previous response ended without a user-facing answer. Answer the original user request now as ordinary text using the available conversation and tool results. Do not call tools, emit tool-call markup, or return an empty response."
)

const (
	explorationExhaustedResult = `{"outcome":"error","failure_layer":"convergence","retryable":false,"effect_status":"not_started","evidence":[{"identity":"runtime-policy","kind":"inline","content":{"message":"exploration budget exhausted by the enforced liveness decision policy"}}],"normalized_cause":"exploration budget exhausted","suggested_recovery":"Synthesize from collected evidence or explain the remaining blocker.","artifact_references":[]}`
	parallelismDeferredResult  = `{"outcome":"error","failure_layer":"convergence","retryable":false,"effect_status":"not_started","evidence":[{"identity":"runtime-policy","kind":"inline","content":{"message":"dispatch deferred by the enforced liveness parallelism limit"}}],"normalized_cause":"parallelism limit","suggested_recovery":"Observe the completed sibling results before proposing another bounded batch.","artifact_references":[]}`
)

const (
	boundToolBudget        = "tool_budget"
	boundExplorationBudget = "exploration_budget"
	boundParallelism       = "parallelism"
	boundSameStrategy      = "same_strategy_retries"
	boundCircuitBreaker    = "circuit_breaker"
	boundTokenBudget       = "token_budget"
)

const tokenBudgetBoundary = "I stopped this turn at its work limit before I could write a full summary, so the account below is what is actually finished rather than a claim that the request is complete."
const toolBudgetBoundary = "I stopped dispatching tools because this logical turn reached its persisted convergence limit."
const circuitBoundary = "I stopped the repeated strategy because its persisted circuit breaker opened."

type Config struct {
	TurnID                  string
	ConversationID          string
	ProjectRoot             string
	Model                   string
	SystemPrompt            string
	MaxOutputTokens         int
	MaxToolCalls            int
	IdleTimeout             time.Duration
	TextualRepairLimit      int
	ExpectationRepairLimit  int
	EmptyRepairLimit        int
	FinalAnswerRepairLimit  int
	CompletionDeferrals     int
	MaxTurnTokens           int
	Breaker                 liveness.BreakerConfig
	AddressIdentity         address.Identity
	InitialUserAudioDataURL string
}

type Dependencies struct {
	Observer         GenerationObserver
	CompletionGate   CompletionGate
	Activation       ActivationSource
	Premises         PremiseSource
	Recorder         TurnRecorder
	Incompletes      IncompleteRecorder
	EvidenceJournal  EvidenceJournal
	EvidenceObserver EvidenceObserver
	Subgoals         SubgoalResolver
	Doubt            DoubtController
	Liveness         LivenessSource
	Guidance         GuidanceSource
	Delivery         *DeliveryChoke
}

type ToolExecution struct {
	Call           protocol.NormalizedToolCall     `json:"call"`
	Result         json.RawMessage                 `json:"result"`
	Error          string                          `json:"error,omitempty"`
	Expect         string                          `json:"expect,omitempty"`
	IdempotencyKey string                          `json:"idempotency_key"`
	MatchVerdict   string                          `json:"match_verdict"`
	SubgoalID      string                          `json:"subgoal_id"`
	Citation       *cortexclient.ToolEventCitation `json:"citation,omitempty"`
}

type Response struct {
	Content           string
	Generation        protocol.NormalizedGeneration
	ToolEvents        []ToolExecution
	ProviderCalls     int
	Usage             protocol.TokenUsage
	Checkpoint        *turnstate.Checkpoint
	ContentStreamed   bool
	ReasoningStreamed bool
	HonestPartial     bool
	Liveness          LivenessTrace
	Repairs           RepairDiagnostics
}

// LivenessTrace is the observability surface for the enforced policy: the
// policy the turn was actually run under and every bound the runtime enforced.
// A healthy turn carries a derived policy and no enforcements at all.
type LivenessTrace struct {
	Policy       liveness.Policy       `json:"policy"`
	TokenBudget  int                   `json:"token_budget"`
	TokensSpent  int                   `json:"tokens_spent"`
	Enforcements []LivenessEnforcement `json:"enforcements,omitempty"`
}

type LivenessEnforcement struct {
	Bound    string `json:"bound"`
	Tool     string `json:"tool,omitempty"`
	CallID   string `json:"call_id,omitempty"`
	Observed int    `json:"observed,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type Incomplete struct {
	Phase            string               `json:"phase"`
	LastToolEvidence json.RawMessage      `json:"last_tool_evidence,omitempty"`
	StartedAt        time.Time            `json:"started_at"`
	AttemptCount     int                  `json:"attempt_count"`
	RecoveryAdvice   string               `json:"recovery_advice"`
	Repairs          RepairDiagnostics    `json:"repairs"`
	CauseDetail      string               `json:"cause,omitempty"`
	ProviderError    string               `json:"provider_error,omitempty"`
	Checkpoint       turnstate.Checkpoint `json:"checkpoint"`
	Cause            error                `json:"-"`
}

type DeliveryRetry struct {
	Answer   string `json:"answer"`
	Attempts uint64 `json:"attempts"`
	Cause    string `json:"cause"`
}

func (retry *DeliveryRetry) Error() string {
	if retry == nil {
		return "<nil>"
	}
	return fmt.Sprintf("runtime loop: answer is ready; delivery retry required after %d attempt(s): %s", retry.Attempts, retry.Cause)
}

type IncompleteRecord struct {
	TurnID           string            `json:"turn_id"`
	ConversationID   string            `json:"conversation_id,omitempty"`
	Phase            string            `json:"phase"`
	LastToolEvidence json.RawMessage   `json:"last_tool_evidence,omitempty"`
	StartedAt        time.Time         `json:"started_at"`
	AttemptCount     int               `json:"attempt_count"`
	RecoveryAdvice   string            `json:"recovery_advice"`
	Repairs          RepairDiagnostics `json:"repairs"`
	CauseDetail      string            `json:"cause,omitempty"`
	ProviderError    string            `json:"provider_error,omitempty"`
}

type RepairDiagnostics struct {
	Textual             int `json:"textual"`
	Expectation         int `json:"expectation"`
	Empty               int `json:"empty"`
	FinalAnswer         int `json:"final_answer"`
	CompletionDeferrals int `json:"completion_deferrals"`
}

func (incomplete *Incomplete) Error() string {
	if incomplete == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"runtime loop incomplete in %s after %d attempt(s): %s",
		incomplete.Phase,
		incomplete.AttemptCount,
		incomplete.RecoveryAdvice,
	)
}

func (incomplete *Incomplete) Unwrap() error {
	if incomplete == nil {
		return nil
	}
	return incomplete.Cause
}

type cursor struct {
	Posture            converge.Posture      `json:"posture,omitempty"`
	ToolCalls          int                   `json:"tool_calls"`
	TextualRepairs     int                   `json:"textual_repairs"`
	ExpectationRepairs int                   `json:"expectation_repairs"`
	EmptyRepairs       int                   `json:"empty_repairs"`
	FinalRepairs       int                   `json:"final_repairs"`
	CompletionDefers   int                   `json:"completion_defers"`
	ExplorationCalls   int                   `json:"exploration_calls,omitempty"`
	LastSignature      string                `json:"last_signature,omitempty"`
	RepeatedCalls      int                   `json:"repeated_calls,omitempty"`
	FailureFingerprint string                `json:"failure_fingerprint,omitempty"`
	FailureRepeats     int                   `json:"failure_repeats,omitempty"`
	Controller         converge.State        `json:"convergence_controller,omitempty"`
	Circuit            liveness.BreakerState `json:"circuit,omitempty"`
	TokensSpent        int                   `json:"tokens_spent,omitempty"`
	InputTokens        int                   `json:"input_tokens,omitempty"`
	OutputTokens       int                   `json:"output_tokens,omitempty"`
}

func (state cursor) repaired() bool {
	return state.TextualRepairs > 0 || state.ExpectationRepairs > 0 ||
		state.EmptyRepairs > 0 || state.FinalRepairs > 0
}

func (state cursor) repairDiagnostics() RepairDiagnostics {
	return RepairDiagnostics{
		Textual:             state.TextualRepairs,
		Expectation:         state.ExpectationRepairs,
		Empty:               state.EmptyRepairs,
		FinalAnswer:         state.FinalRepairs,
		CompletionDeferrals: state.CompletionDefers,
	}
}

func (state *cursor) annotateFailure(
	call protocol.NormalizedToolCall,
	result ToolResult,
) ToolResult {
	if state == nil {
		return result
	}
	if !result.IsError {
		state.FailureFingerprint = ""
		state.FailureRepeats = 0
		return result
	}
	cause := strings.ToLower(strings.Join(strings.Fields(result.FailureMessage), " "))
	if cause == "" {
		cause = strings.ToLower(strings.Join(strings.Fields(string(result.Content)), " "))
	}
	payload := map[string]any{}
	if json.Unmarshal(result.Content, &payload) != nil {
		payload["outcome"] = "error"
		payload["failure_layer"] = result.FailureClass
		payload["effect_status"] = "completed"
		payload["evidence"] = string(result.Content)
	}
	effectStatus, _ := payload["effect_status"].(string)
	kind := converge.FailureDeterministic
	if result.Retryable {
		kind = converge.FailureTransient
	}
	if effectStatus == "outcome_unknown" || effectStatus == "unknown" {
		kind = converge.FailureUnknownEffect
	}
	decision := state.decide(kind, converge.Fingerprint{
		Operation: call.Name, NormalizedArguments: call.Arguments,
		Phase: "tool", FailureLayer: result.FailureClass,
		NormalizedCause: cause, EffectStatus: effectStatus,
	})
	fingerprint := converge.Fingerprint{
		Operation: call.Name, NormalizedArguments: call.Arguments,
		Phase: "tool", FailureLayer: result.FailureClass,
		NormalizedCause: cause, EffectStatus: effectStatus,
	}.Key()
	state.FailureFingerprint = fingerprint
	state.FailureRepeats = int(decision.Count)
	payload["failure_fingerprint"] = fingerprint
	payload["repeat_count"] = decision.Count
	if decision.RequiresStrategyChange || decision.Action == converge.ActionDegrade {
		payload["retryable"] = false
		payload["strategy_change_required"] = true
		payload["suggested_recovery"] = string(decision.Action) + ": do not repeat this operation unchanged; change strategy, degrade to verified evidence, or state the blocker honestly."
		result.Retryable = false
	}
	if encoded, err := json.Marshal(payload); err == nil {
		result.Content = encoded
	}
	return result
}

func (state *cursor) decide(kind converge.FailureKind, fingerprint converge.Fingerprint) converge.Decision {
	posture := state.Posture
	if posture == "" {
		posture = converge.Conversation
	}
	return (converge.Controller{}).Observe(
		&state.Controller,
		converge.Failure{Kind: kind, Fingerprint: fingerprint},
		converge.ForPosture(posture),
	)
}

type Loop struct {
	provider         Generator
	tools            ToolManager
	store            CheckpointStore
	config           Config
	observer         GenerationObserver
	gate             CompletionGate
	activation       ActivationSource
	premises         PremiseSource
	recorder         TurnRecorder
	incompletes      IncompleteRecorder
	evidenceJournal  EvidenceJournal
	evidenceObserver EvidenceObserver
	subgoals         SubgoalResolver
	doubt            DoubtController
	liveness         LivenessSource
	guidance         GuidanceSource
	delivery         *DeliveryChoke
	stream           *runtimestream.Transaction
	usage            provider.TurnUsage

	terminalOnce sync.Once
}

func New(
	generator Generator,
	toolManager ToolManager,
	store CheckpointStore,
	config Config,
	dependencies Dependencies,
) (*Loop, error) {
	if generator == nil || toolManager == nil || store == nil {
		return nil, fmt.Errorf(
			"runtime loop: provider, tools, and checkpoint store are required",
		)
	}
	if strings.TrimSpace(config.TurnID) == "" ||
		strings.TrimSpace(config.Model) == "" {
		return nil, fmt.Errorf("runtime loop: turn ID and model are required")
	}
	if config.MaxToolCalls < 0 || config.IdleTimeout < 0 ||
		config.MaxTurnTokens < 0 ||
		config.TextualRepairLimit < 0 ||
		config.ExpectationRepairLimit < 0 ||
		config.EmptyRepairLimit < 0 ||
		config.FinalAnswerRepairLimit < 0 ||
		config.CompletionDeferrals < 0 {
		return nil, fmt.Errorf("runtime loop: bounds cannot be negative")
	}
	if config.MaxToolCalls == 0 {
		config.MaxToolCalls = defaultMaxToolCalls
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.TextualRepairLimit == 0 {
		config.TextualRepairLimit = defaultRepairLimit
	}
	if config.ExpectationRepairLimit == 0 {
		config.ExpectationRepairLimit = defaultRepairLimit
	}
	if config.EmptyRepairLimit == 0 {
		config.EmptyRepairLimit = defaultEmptyRepairLimit
	}
	if config.FinalAnswerRepairLimit == 0 {
		config.FinalAnswerRepairLimit = defaultRepairLimit
	}
	if config.CompletionDeferrals == 0 {
		config.CompletionDeferrals = defaultCompletionDeferral
	}
	if config.MaxTurnTokens == 0 {
		config.MaxTurnTokens = defaultMaxTurnTokens
	}
	config.AddressIdentity = address.New(
		config.AddressIdentity.PreferredPersonName,
		config.AddressIdentity.AgentName,
	)
	delivery := dependencies.Delivery
	incompletes := dependencies.Incompletes
	if incompletes == nil {
		incompletes = &O1FailureReplayLane{}
	}
	if delivery == nil && dependencies.Recorder != nil {
		delivery = &DeliveryChoke{Recorder: dependencies.Recorder}
	} else if delivery != nil && delivery.Recorder == nil {
		delivery.Recorder = dependencies.Recorder
	}
	runtimeLoop := &Loop{
		provider: generator, tools: toolManager, store: store,
		config: config, observer: dependencies.Observer,
		gate:             dependencies.CompletionGate,
		activation:       dependencies.Activation,
		premises:         dependencies.Premises,
		recorder:         dependencies.Recorder,
		incompletes:      incompletes,
		evidenceJournal:  dependencies.EvidenceJournal,
		evidenceObserver: dependencies.EvidenceObserver,
		subgoals:         dependencies.Subgoals,
		doubt:            dependencies.Doubt,
		liveness:         dependencies.Liveness,
		guidance:         dependencies.Guidance,
		delivery:         delivery,
	}
	if dependencies.Observer != nil {
		var streamRecorder runtimestream.Recorder
		if recorder, ok := store.(runtimestream.Recorder); ok {
			streamRecorder = recorder
		}
		runtimeLoop.stream = runtimestream.New(
			config.TurnID, streamRecorder,
			observerStreamSink{observer: dependencies.Observer},
		)
	}
	return runtimeLoop, nil
}

func (loop *Loop) Turn(
	ctx context.Context,
	userContent string,
) (Response, error) {
	return loop.TurnWithHistory(ctx, userContent, nil)
}

// TurnWithHistory starts a turn with the visible same-conversation transcript
// represented as genuine role-separated protocol messages. The current user
// request is always appended last and is never recovered from ambient memory.
func (loop *Loop) TurnWithHistory(
	ctx context.Context,
	userContent string,
	history []protocol.Message,
) (Response, error) {
	userContent = strings.TrimSpace(userContent)
	if userContent == "" {
		return Response{}, fmt.Errorf("runtime loop: user message is required")
	}
	checkpoint := turnstate.Checkpoint{
		Messages: append(visibleConversationHistory(history), protocol.Message{
			Role: protocol.RoleUser, Content: userContent,
			AudioDataURL: strings.TrimSpace(loop.config.InitialUserAudioDataURL),
		}),
	}
	loop.initializeCodingCheckpoint(&checkpoint, userContent)
	if err := loop.save(ctx, &checkpoint, cursor{}); err != nil {
		return Response{}, err
	}
	if loop.recorder != nil {
		loop.recorder.RecordUser(userContent)
	}
	return loop.runTurn(ctx, userContent, checkpoint, cursor{})
}

// TurnInternal starts a supervised retry from the original user objective
// while keeping the supervisor instruction in the system lane. It does not
// record another user message.
func (loop *Loop) TurnInternal(
	ctx context.Context,
	userContent string,
	guidance string,
) (Response, error) {
	return loop.TurnInternalWithHistory(ctx, userContent, nil, guidance)
}

func (loop *Loop) TurnInternalWithHistory(
	ctx context.Context,
	userContent string,
	history []protocol.Message,
	guidance string,
) (Response, error) {
	userContent = strings.TrimSpace(userContent)
	if userContent == "" {
		return Response{}, fmt.Errorf("runtime loop: user objective is required")
	}
	checkpoint := turnstate.Checkpoint{
		Messages: append(visibleConversationHistory(history), protocol.Message{
			Role: protocol.RoleUser, Content: userContent,
		}),
	}
	loop.initializeCodingCheckpoint(&checkpoint, userContent)
	if guidance = strings.TrimSpace(guidance); guidance != "" {
		checkpoint.Messages = append(checkpoint.Messages, protocol.Message{
			Role: protocol.RoleSystem, Content: guidance,
		})
	}
	if err := loop.save(ctx, &checkpoint, cursor{}); err != nil {
		return Response{}, err
	}
	return loop.runTurn(ctx, userContent, checkpoint, cursor{})
}

func visibleConversationHistory(history []protocol.Message) []protocol.Message {
	result := make([]protocol.Message, 0, len(history)+1)
	for _, message := range history {
		if message.Role != protocol.RoleUser && message.Role != protocol.RoleAssistant {
			continue
		}
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		result = append(result, protocol.Message{
			Role: message.Role, Content: content,
		})
	}
	return result
}

func (loop *Loop) Resume(
	ctx context.Context,
	userContent string,
	checkpoint turnstate.Checkpoint,
) (Response, error) {
	userContent = strings.TrimSpace(userContent)
	if err := validateCheckpoint(userContent, checkpoint); err != nil {
		return Response{}, err
	}
	var state cursor
	if len(checkpoint.Runtime) > 0 {
		if err := json.Unmarshal(checkpoint.Runtime, &state); err != nil {
			return Response{}, fmt.Errorf(
				"runtime loop: decode checkpoint cursor: %w", err,
			)
		}
	}
	return loop.runTurn(ctx, userContent, checkpoint, state)
}

func (loop *Loop) runTurn(
	ctx context.Context,
	userContent string,
	checkpoint turnstate.Checkpoint,
	state cursor,
) (Response, error) {
	if store, ok := loop.store.(ConvergenceStore); ok {
		persisted, loadErr := store.LoadConvergenceRecord(ctx, loop.config.TurnID)
		if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
			return Response{}, fmt.Errorf("runtime loop: load convergence record: %w", loadErr)
		}
		if loadErr == nil {
			state.InputTokens = max(state.InputTokens, int(persisted.CumulativeInputTokens))
			state.OutputTokens = max(state.OutputTokens, int(persisted.CumulativeOutputTokens))
			state.TokensSpent = max(state.TokensSpent, state.InputTokens+state.OutputTokens)
			state.ToolCalls = max(state.ToolCalls, int(persisted.ToolCalls))
			state.TextualRepairs = max(state.TextualRepairs, int(persisted.RepairCounters["textual"]))
			state.ExpectationRepairs = max(state.ExpectationRepairs, int(persisted.RepairCounters["expectation"]))
			state.EmptyRepairs = max(state.EmptyRepairs, int(persisted.RepairCounters["empty"]))
			state.FinalRepairs = max(state.FinalRepairs, int(persisted.RepairCounters["final_answer"]))
			for _, usage := range persisted.ProviderUsage {
				checkpoint.ProviderAttempts = max(checkpoint.ProviderAttempts, int(usage.RequestCount))
			}
		}
	}
	response, err := responseFromCheckpoint(checkpoint)
	if err != nil {
		return Response{}, err
	}
	policy, err := loop.derivePolicy(userContent, state)
	if err != nil {
		return response, loop.incomplete(
			ctx, "liveness_policy", checkpoint, response,
			time.Now().UTC(), "resume_after_correcting_measured_context", err,
		)
	}
	response.Liveness.Policy = policy
	response.Liveness.TokenBudget = loop.config.MaxTurnTokens
	response.Liveness.TokensSpent = state.TokensSpent
	// Tokens already merged by an earlier run of this turn ride the checkpoint,
	// so a respawn continues one cumulative budget instead of buying a fresh
	// one on every resume.
	carriedTokens := state.TokensSpent
	carriedInputTokens := state.InputTokens
	carriedOutputTokens := state.OutputTokens
	toolCallLimit := loop.config.MaxToolCalls
	if policy.ToolCallBudget < toolCallLimit {
		toolCallLimit = policy.ToolCallBudget
	}
	breaker := loop.config.Breaker
	tools := loop.tools.Surface(ctx)
	if checkpoint.PendingCall != nil {
		if err := loop.reconcilePending(
			ctx, &checkpoint, &response, &state,
		); err != nil {
			return response, err
		}
	}
	synthesisOwed, err := loop.synthesisOwed(ctx)
	if err != nil {
		return response, loop.incomplete(
			ctx, "synthesis_debt", checkpoint, response,
			time.Now().UTC(), "resume_with_committed_tool_evidence", err,
		)
	}
	turnCircuitErr := state.Circuit.Enforce("turn")
	if !synthesisOwed {
		if turnCircuitErr != nil {
			response.Liveness.Enforcements = append(
				response.Liveness.Enforcements,
				LivenessEnforcement{Bound: boundCircuitBreaker},
			)
			return loop.deliverHonestPartial(
				ctx, userContent, checkpoint, response, state, circuitBoundary,
			)
		}
	}
	var nextRepair string
	var pendingDoubt string
	toolsDisabled := synthesisOwed && turnCircuitErr != nil
	revisionActive := false
	for {
		if err := ctx.Err(); err != nil {
			return response, loop.incomplete(
				context.WithoutCancel(ctx), "turn", checkpoint, response,
				time.Now().UTC(), "resume_from_checkpoint", err,
			)
		}
		synthesisOwed, err = loop.synthesisOwed(ctx)
		if err != nil {
			return response, loop.incomplete(
				ctx, "synthesis_debt", checkpoint, response,
				time.Now().UTC(), "resume_with_committed_tool_evidence", err,
			)
		}
		providerCircuitErr := state.Circuit.Enforce("provider")
		tokenExhausted := loop.config.MaxTurnTokens > 0 &&
			state.TokensSpent >= loop.config.MaxTurnTokens
		if synthesisOwed && (providerCircuitErr != nil || tokenExhausted) {
			toolsDisabled = true
		}
		if !synthesisOwed {
			if providerCircuitErr != nil {
				response.Liveness.Enforcements = append(
					response.Liveness.Enforcements,
					LivenessEnforcement{Bound: boundCircuitBreaker},
				)
				return loop.deliverHonestPartial(
					ctx, userContent, checkpoint, response, state, circuitBoundary,
				)
			}
		}
		// The cumulative token budget is checked HERE and nowhere else: at the
		// top of an iteration, before the next provider call, after every path
		// that could have delivered a finished answer has already returned. A
		// budget bound must never be able to eat a completed answer — that is
		// the o1-budget-kill class — so exhaustion only ever prevents MORE
		// generation, and it routes to the honest partial rather than a death.
		if !synthesisOwed && tokenExhausted {
			response.Liveness.Enforcements = append(
				response.Liveness.Enforcements,
				LivenessEnforcement{
					Bound: boundTokenBudget, Observed: state.TokensSpent,
					Limit: loop.config.MaxTurnTokens,
				},
			)
			return loop.deliverHonestPartial(
				ctx, userContent, checkpoint, response, state,
				tokenBudgetBoundary,
			)
		}
		activation := ""
		if loop.activation != nil {
			var premises []string
			if loop.premises != nil {
				premises = loop.premises.ActivePremises()
			}
			activation, err = loop.activation.Activate(
				ctx,
				ActivationRequest{
					ConversationID: loop.config.ConversationID,
					Query:          userContent,
					Premises:       premises,
				},
			)
			if err != nil {
				activation = "[memory_diagnostics]\nRecalled memory is temporarily unavailable; continue from the authoritative durable transcript."
			}
		}
		messages, manifest := composeRequestMessages(
			loop.config.ConversationID, loop.config.SystemPrompt,
			checkpoint.Messages, nextRepair, activation,
		)
		if store, ok := loop.store.(ContextManifestStore); ok {
			if manifestErr := store.SaveContextManifest(ctx, loop.config.TurnID, uint64(checkpoint.Step), manifest); manifestErr != nil {
				return response, fmt.Errorf("runtime loop: persist context manifest: %w", manifestErr)
			}
		}
		request := protocol.GenerationRequest{
			Model:           loop.config.Model,
			Messages:        messages,
			Tools:           tools,
			MaxOutputTokens: loop.config.MaxOutputTokens,
			Stream:          loop.stream != nil,
		}
		request.Messages = appendCodingCheckpoint(request.Messages, checkpoint.Coding)
		if loop.guidance != nil {
			request.Messages = appendGuidance(
				request.Messages,
				loop.guidance.DrainGuidance(),
			)
		}
		nextRepair = ""
		if toolsDisabled {
			request.Tools = nil
		}
		// A revision step runs tools-stripped so the model must revise the
		// plan rather than dispatch another sibling call, and the doubt line
		// rides this ONE request only — it is never appended to
		// checkpoint.Messages, so the durable window and the Neocortex
		// transcript stay free of controller guidance.
		if pendingDoubt != "" {
			request.Messages = foldDoubt(request.Messages, pendingDoubt)
			request.Tools = nil
			pendingDoubt = ""
			revisionActive = true
		}
		started := time.Now().UTC()
		if loop.stream != nil {
			loop.stream.Begin(uint64(checkpoint.Step))
		}
		generation, streamed, generateErr := loop.generate(ctx, request)
		checkpoint.ProviderAttempts++
		response.ProviderCalls = checkpoint.ProviderAttempts
		response.Usage = loop.usage.Snapshot().Usage
		state.InputTokens = carriedInputTokens + response.Usage.PromptTokens
		state.OutputTokens = carriedOutputTokens + response.Usage.CompletionTokens
		state.TokensSpent = carriedTokens + response.Usage.TotalTokens
		response.Liveness.TokensSpent = state.TokensSpent
		if generateErr != nil {
			revisionActive = false
			if loop.observer != nil && streamed {
				if resetErr := loop.retractAttempt(ctx); resetErr != nil {
					generateErr = errors.Join(generateErr, resetErr)
				}
			}
			protocolCorruption := errors.Is(generateErr, provider.ErrToolProtocol) ||
				errors.Is(generateErr, ErrTextualToolMarkup)
			kind := converge.FailureProcessUnusable
			if protocolCorruption {
				kind = converge.FailureProviderCorruption
			}
			convergenceDecision := state.decide(kind, converge.Fingerprint{
				Operation: "provider.generate", Phase: "provider",
				FailureLayer: "provider", NormalizedCause: generateErr.Error(),
				EffectStatus: "not_started",
			})
			if state.TextualRepairs < loop.config.TextualRepairLimit &&
				protocolCorruption && convergenceDecision.Retry {
				state.TextualRepairs++
				nextRepair = textualToolRepairPrompt
				if adaptive, ok := loop.provider.(interface {
					AdvanceGenerationStrategy(string) bool
				}); ok {
					adaptive.AdvanceGenerationStrategy(generateErr.Error())
				}
				if err := loop.save(ctx, &checkpoint, state); err != nil {
					return response, err
				}
				continue
			}
			if err := loop.save(context.WithoutCancel(ctx), &checkpoint, state); err != nil {
				return response, err
			}
			return response, loop.incomplete(
				ctx, "provider", checkpoint, response, started,
				"resume_from_checkpoint", generateErr,
			)
		}
		if revisionActive {
			revisionActive = false
			// The revision is an internal correction: retract whatever it
			// streamed so the user never sees the replan, then recommit the
			// revised plan into the model's own window. Any tool calls it
			// emitted under a tools-stripped request are discarded — sibling
			// dispatches are blocked for the whole revision step.
			if loop.observer != nil && streamed {
				if resetErr := loop.retractAttempt(ctx); resetErr != nil {
					return response, loop.incomplete(
						ctx, "observer_commit", checkpoint, response,
						started, "resume_from_checkpoint", resetErr,
					)
				}
			}
			revised := strings.TrimSpace(generation.Content)
			if revised == "" ||
				rejectTextualToolMarkup(revised) != nil {
				continue
			}
			checkpoint.Messages = append(
				checkpoint.Messages,
				protocol.Message{
					Role: protocol.RoleAssistant, Content: revised,
				},
			)
			checkpoint.Step++
			if err := loop.save(ctx, &checkpoint, state); err != nil {
				return response, err
			}
			if loop.recorder != nil {
				loop.recorder.RecordAssistant(
					checkpoint.Messages[len(checkpoint.Messages)-1],
				)
			}
			continue
		}
		if err := rejectTextualToolMarkup(
			generation.Content + generation.Reasoning,
		); err != nil {
			if loop.observer != nil && streamed {
				_ = loop.retractAttempt(ctx)
			}
			convergenceDecision := state.decide(converge.FailureProviderCorruption, converge.Fingerprint{
				Operation: "provider.conform", Phase: "provider",
				FailureLayer: "provider", NormalizedCause: err.Error(),
				EffectStatus: "not_started",
			})
			if state.TextualRepairs < loop.config.TextualRepairLimit && convergenceDecision.Retry {
				state.TextualRepairs++
				nextRepair = textualToolRepairPrompt
				if err := loop.save(ctx, &checkpoint, state); err != nil {
					return response, err
				}
				continue
			}
			if !toolsDisabled {
				toolsDisabled = true
				nextRepair = textualToolFinalPrompt
				continue
			}
			return loop.deliverHonestPartial(
				ctx, userContent, checkpoint, response, state, "",
			)
		}
		if addressErr := loop.config.AddressIdentity.ValidateVisible(
			generation.Reasoning, generation.Content,
		); addressErr != nil {
			if loop.observer != nil && streamed {
				_ = loop.retractAttempt(ctx)
			}
			convergenceDecision := state.decide(converge.FailureProviderCorruption, converge.Fingerprint{
				Operation: "provider.address_identity", Phase: "provider",
				FailureLayer: "validation", NormalizedCause: addressErr.Error(),
				EffectStatus: "not_started",
			})
			if state.FinalRepairs < loop.config.FinalAnswerRepairLimit && convergenceDecision.Retry {
				state.FinalRepairs++
				nextRepair = address.RepairInstruction(loop.config.AddressIdentity)
				if err := loop.save(ctx, &checkpoint, state); err != nil {
					return response, err
				}
				continue
			}
			return loop.deliverHonestPartial(ctx, userContent, checkpoint, response, state, "")
		}
		expectations, calls, expectationErr :=
			extractExpectations(generation.ToolCalls)
		if expectationErr != nil {
			if loop.observer != nil && streamed {
				_ = loop.retractAttempt(ctx)
			}
			convergenceDecision := state.decide(converge.FailureProviderCorruption, converge.Fingerprint{
				Operation: "provider.tool_expectation", Phase: "provider",
				FailureLayer: "provider", NormalizedCause: expectationErr.Error(),
				EffectStatus: "not_started",
			})
			if state.ExpectationRepairs <
				loop.config.ExpectationRepairLimit && convergenceDecision.Retry {
				state.ExpectationRepairs++
				nextRepair = expectationRepairPrompt
				if err := loop.save(ctx, &checkpoint, state); err != nil {
					return response, err
				}
				continue
			}
			if !toolsDisabled {
				toolsDisabled = true
				nextRepair = expectationFinalPrompt
				continue
			}
			return loop.deliverHonestPartial(
				ctx, userContent, checkpoint, response, state, "",
			)
		}
		generation.ToolCalls = calls
		response.Generation = generation
		if len(calls) == 0 {
			content := strings.TrimSpace(generation.Content)
			if content == "" {
				if loop.observer != nil && streamed {
					_ = loop.retractAttempt(ctx)
				}
				convergenceDecision := state.decide(converge.FailureProviderCorruption, converge.Fingerprint{
					Operation: "provider.empty_answer", Phase: "provider",
					FailureLayer: "provider", NormalizedCause: "empty answer",
					EffectStatus: "not_started",
				})
				if state.EmptyRepairs < loop.config.EmptyRepairLimit && convergenceDecision.Retry {
					state.EmptyRepairs++
					toolsDisabled = true
					nextRepair = emptyFinalRepairPrompt
					if err := loop.save(
						ctx, &checkpoint, state,
					); err != nil {
						return response, err
					}
					continue
				}
				return loop.deliverHonestPartial(
					ctx, userContent, checkpoint, response, state, "",
				)
			}
			if accepted, reason := finalAnswerAddressesRequestWithHistory(
				userContent, content, response.ToolEvents,
				checkpoint.Messages,
			); !accepted {
				if loop.observer != nil && streamed {
					_ = loop.retractAttempt(ctx)
				}
				convergenceDecision := state.decide(converge.FailureRepeatedSemantic, converge.Fingerprint{
					Operation: "answer.acceptance", Phase: "completion",
					FailureLayer: "provider", NormalizedCause: reason,
					EffectStatus: "completed",
				})
				if state.FinalRepairs <
					loop.config.FinalAnswerRepairLimit && convergenceDecision.Action != converge.ActionDegrade {
					state.FinalRepairs++
					nextRepair = finalAnswerRepairPrompt(reason)
					if err := loop.save(
						ctx, &checkpoint, state,
					); err != nil {
						return response, err
					}
					continue
				}
				return loop.deliverHonestPartial(
					ctx, userContent, checkpoint, response, state, "",
				)
			}
			if loop.gate != nil {
				decision, gateErr := loop.gate.CheckCompletion(ctx)
				if gateErr != nil {
					return response, loop.incomplete(
						ctx, "completion_gate", checkpoint, response,
						started, "resume_completion_check", gateErr,
					)
				}
				if !decision.Ready && decision.Stop {
					if reason := strings.TrimSpace(decision.Reason); reason != "" {
						content += "\n\nWork is paused at a real boundary: " +
							reason
					}
				} else if !decision.Ready {
					if loop.observer != nil && streamed {
						_ = loop.retractAttempt(ctx)
					}
					convergenceDecision := state.decide(converge.FailureRepeatedSemantic, converge.Fingerprint{
						Operation: "completion.gate", Phase: "completion",
						FailureLayer: "validation", NormalizedCause: decision.Reason,
						EffectStatus: "completed",
					})
					if state.CompletionDefers >=
						loop.config.CompletionDeferrals || convergenceDecision.Action == converge.ActionDegrade {
						return response, loop.incomplete(
							ctx, "evidence_convergence",
							checkpoint, response, started,
							"resume_next_verified_work_item", nil,
						)
					}
					state.CompletionDefers++
					nextRepair = completionContinuationPrompt(decision)
					if err := loop.save(
						ctx, &checkpoint, state,
					); err != nil {
						return response, err
					}
					continue
				}
			}
			generation.Content = content
			response.Generation = generation
			return loop.deliverAccepted(
				ctx, userContent, checkpoint, response, state,
				generation, streamed,
			)
		}
		if loop.observer != nil {
			observerErr := loop.commitToolAttempt(ctx, calls)
			if observerErr != nil {
				return response, loop.incomplete(
					ctx, "observer_commit", checkpoint, response,
					started, "resume_from_checkpoint", observerErr,
				)
			}
		}
		if state.Posture == "" {
			if toolCallLimit <= 4 {
				state.Posture = converge.Conversation
			} else if toolCallLimit <= 20 {
				state.Posture = converge.Exploration
			} else {
				state.Posture = converge.Execution
			}
		}
		for _, call := range calls {
			if state.Posture == converge.Conversation && isResearchReadOperation(call.Name) {
				state.Posture = converge.Exploration
				toolCallLimit = max(toolCallLimit, int(converge.ForPosture(converge.Exploration).ToolCalls))
			}
		}
		checkpoint.Messages = append(
			checkpoint.Messages,
			protocol.Message{
				Role:      protocol.RoleAssistant,
				Content:   generation.Content,
				Reasoning: generation.Reasoning,
				ToolCalls: cloneCalls(calls),
			},
		)
		checkpoint.Step++
		if err := loop.save(ctx, &checkpoint, state); err != nil {
			return response, err
		}
		if loop.recorder != nil {
			loop.recorder.RecordAssistant(
				checkpoint.Messages[len(checkpoint.Messages)-1],
			)
		}
		// Excess parallel calls are deferred, never dropped: every deferred
		// call gets an explicit tool-result marker below so the model sees a
		// real result for the call ID it emitted.
		dispatched := calls
		var deferred []protocol.NormalizedToolCall
		if len(dispatched) > policy.Parallelism {
			deferred = append(
				[]protocol.NormalizedToolCall(nil),
				dispatched[policy.Parallelism:]...,
			)
			dispatched = dispatched[:policy.Parallelism]
			for _, call := range deferred {
				response.Liveness.Enforcements = append(
					response.Liveness.Enforcements,
					LivenessEnforcement{
						Bound: boundParallelism, Tool: call.Name,
						CallID: call.ID, Observed: len(calls),
						Limit: policy.Parallelism,
					},
				)
			}
		}
		for index, call := range dispatched {
			state.ToolCalls++
			if state.ToolCalls > toolCallLimit {
				response.Liveness.Enforcements = append(
					response.Liveness.Enforcements,
					LivenessEnforcement{
						Bound: boundToolBudget, Tool: call.Name,
						CallID: call.ID, Observed: state.ToolCalls,
						Limit: toolCallLimit,
					},
				)
				return loop.deliverHonestPartial(
					ctx, userContent, checkpoint, response, state, toolBudgetBoundary,
				)
			}
			if liveness.IsExplorationTool(call.Name) {
				state.ExplorationCalls++
				if state.ExplorationCalls > policy.ExplorationBudget {
					loop.injectToolResult(
						&checkpoint, &response, call,
						json.RawMessage(explorationExhaustedResult),
						"exploration budget exhausted",
					)
					response.Liveness.Enforcements = append(
						response.Liveness.Enforcements,
						LivenessEnforcement{
							Bound: boundExplorationBudget, Tool: call.Name,
							CallID: call.ID, Observed: state.ExplorationCalls,
							Limit: policy.ExplorationBudget,
						},
					)
					if err := loop.save(ctx, &checkpoint, state); err != nil {
						return response, err
					}
					continue
				}
			}
			if err := state.Circuit.Enforce("tool"); err != nil {
				response.Liveness.Enforcements = append(
					response.Liveness.Enforcements,
					LivenessEnforcement{
						Bound: boundCircuitBreaker, Tool: call.Name, CallID: call.ID,
					},
				)
				return loop.deliverHonestPartial(
					ctx, userContent, checkpoint, response, state, circuitBoundary,
				)
			}
			signature := liveness.CanonicalToolSignature(
				call.Name, call.Arguments,
			)
			if signature == state.LastSignature &&
				state.RepeatedCalls >= policy.SameStrategyRetries+1 {
				state.RepeatedCalls++
				convergenceDecision := state.decide(converge.FailureRepeatedSemantic, converge.Fingerprint{
					Operation: call.Name, NormalizedArguments: call.Arguments,
					Phase: "tool_selection", FailureLayer: "convergence",
					NormalizedCause: "same operation and arguments repeated without new evidence",
					EffectStatus:    "not_started",
				})
				response.Liveness.Enforcements = append(
					response.Liveness.Enforcements,
					LivenessEnforcement{
						Bound: boundSameStrategy, Tool: call.Name,
						CallID: call.ID, Observed: state.RepeatedCalls,
						Limit: policy.SameStrategyRetries,
					},
				)
				failure := structuredToolFailure(
					"convergence", false, "not_started",
					"The same operation, arguments, phase, and normalized cause repeated without new evidence.",
					string(convergenceDecision.Action)+": change strategy, degrade to verified evidence already available, or state the blocker honestly.",
				)
				failure = state.annotateFailure(call, failure)
				loop.injectToolResult(
					&checkpoint, &response, call, failure.Content,
					failure.FailureMessage,
				)
				if err := loop.save(ctx, &checkpoint, state); err != nil {
					return response, err
				}
				continue
			}
			if signature == state.LastSignature {
				state.RepeatedCalls++
			} else {
				state.LastSignature = signature
				state.RepeatedCalls = 1
			}
			breaker.Observe(&state.Circuit, signature)
			idempotencyKey := makeIdempotencyKey(
				loop.config.TurnID, call,
			)
			atomicStore, hasAtomicStore := loop.store.(PendingEffectStore)
			metadataProvider, hasMetadata := loop.tools.(EffectMetadataProvider)
			var effectMetadata EffectMetadata
			useAtomicStart := hasAtomicStore && hasMetadata
			if useAtomicStart {
				var metadataErr error
				effectMetadata, metadataErr = metadataProvider.EffectMetadata(call)
				if metadataErr != nil {
					failure := structuredToolFailure(
						"effect_start", false, "not_started",
						metadataErr.Error(),
						"Correct the call or use another available operation; no dispatch occurred.",
					)
					failure = state.annotateFailure(call, failure)
					loop.injectToolResult(
						&checkpoint, &response, call, failure.Content,
						failure.FailureMessage,
					)
					if err := loop.save(ctx, &checkpoint, state); err != nil {
						return response, err
					}
					continue
				}
			} else if preparer, ok := loop.tools.(EffectPreparer); ok {
				if prepareErr := preparer.PrepareEffect(
					ctx, call, idempotencyKey,
				); prepareErr != nil {
					failure := structuredToolFailure(
						"effect_start", false, "not_started",
						prepareErr.Error(),
						"Correct the call or use another available operation; no dispatch occurred.",
					)
					failure = state.annotateFailure(call, failure)
					loop.injectToolResult(
						&checkpoint, &response, call, failure.Content,
						failure.FailureMessage,
					)
					if err := loop.save(ctx, &checkpoint, state); err != nil {
						return response, err
					}
					continue
				}
			}
			pending := &turnstate.PendingCall{
				CallID: call.ID, IdempotencyKey: idempotencyKey,
				ToolName: call.Name, Arguments: call.Arguments,
				Expect:       expectations[index],
				DispatchedAt: time.Now().UTC(),
			}
			checkpoint.PendingCall = pending
			if useAtomicStart {
				encoded, err := json.Marshal(state)
				if err != nil {
					return response, fmt.Errorf(
						"runtime loop: encode pending cursor: %w", err,
					)
				}
				checkpoint.Runtime = encoded
				checkpoint.SavedAt = time.Now().UTC()
				if err := atomicStore.SavePendingEffect(
					ctx, loop.config.TurnID, checkpoint,
					idempotencyKey, call.Name, call.Arguments,
					effectMetadata.RetrySafe,
				); err != nil {
					return response, fmt.Errorf(
						"runtime loop: atomically start pending effect: %w", err,
					)
				}
			} else if err := loop.save(ctx, &checkpoint, state); err != nil {
				return response, err
			}
			result, executeErr := loop.execute(
				ctx, call, idempotencyKey, started,
			)
			if executeErr != nil {
				if reconcileErr := loop.reconcilePending(
					context.WithoutCancel(ctx), &checkpoint, &response, &state,
				); reconcileErr != nil {
					return response, reconcileErr
				}
				continue
			}
			result = state.annotateFailure(call, result)
			if resultObserver, ok := loop.observer.(interface {
				ObserveToolResult(context.Context, protocol.NormalizedToolCall, ToolResult)
			}); ok {
				resultObserver.ObserveToolResult(ctx, call, result)
			}
			loop.updateCodingCheckpoint(&checkpoint, call, result)
			execution := ToolExecution{
				Call: call, Result: result.Content,
				Expect:         expectations[index],
				IdempotencyKey: idempotencyKey,
				MatchVerdict: matchToolExpectation(
					expectations[index], result,
				),
				SubgoalID: loop.subgoalFor(call),
			}
			execution.Error = executionError(result)
			if err := loop.commitEvidence(
				ctx, &execution, checkpoint, response, started,
			); err != nil {
				return response, err
			}
			if line, armed := loop.observeDoubt(
				ctx, checkpoint.ProviderAttempts, execution,
			); armed {
				pendingDoubt = line
			}
			response.ToolEvents = append(
				response.ToolEvents, execution,
			)
			checkpoint.ToolEvents = append(
				checkpoint.ToolEvents, encodeToolExecution(execution),
			)
			checkpoint.Messages = append(
				checkpoint.Messages,
				protocol.Message{
					Role:    protocol.RoleTool,
					Content: string(result.Content),
					Name:    call.Name, ToolCallID: call.ID,
				},
			)
			checkpoint.PendingCall = nil
			checkpoint.Step++
			if err := loop.save(ctx, &checkpoint, state); err != nil {
				return response, err
			}
			if loop.recorder != nil {
				loop.recorder.RecordTool(
					checkpoint.Messages[len(checkpoint.Messages)-1],
				)
			}
		}
		for _, call := range deferred {
			loop.injectToolResult(
				&checkpoint, &response, call,
				json.RawMessage(parallelismDeferredResult),
				"dispatch deferred by the enforced parallelism limit",
			)
		}
		if len(deferred) > 0 {
			if err := loop.save(ctx, &checkpoint, state); err != nil {
				return response, err
			}
		}
	}
}

func (loop *Loop) synthesisOwed(ctx context.Context) (bool, error) {
	store, ok := loop.store.(SynthesisDebtStore)
	if !ok {
		return false, nil
	}
	turn, err := store.LoadTurnRecord(ctx, loop.config.TurnID)
	if err != nil {
		return false, err
	}
	return turn.SynthesisDebt.Owed, nil
}

// injectToolResult lands a runtime-authored result for a call the enforced
// policy refused to dispatch. The marker is a TOOL RESULT, so it belongs in the
// durable window: the model emitted that call ID and must see a real result for
// it. No tool ran, so nothing is committed to the evidence journal, the belief
// state, or the silent voice — recording an execution that did not happen would
// be exactly the false-evidence class the runtime exists to prevent.
func (loop *Loop) injectToolResult(
	checkpoint *turnstate.Checkpoint,
	response *Response,
	call protocol.NormalizedToolCall,
	result json.RawMessage,
	failure string,
) {
	execution := ToolExecution{
		Call: call, Result: result, Error: failure,
		MatchVerdict: cortexclient.ToolMatchUnknown,
		SubgoalID:    loop.subgoalFor(call),
	}
	response.ToolEvents = append(response.ToolEvents, execution)
	checkpoint.ToolEvents = append(
		checkpoint.ToolEvents, encodeToolExecution(execution),
	)
	checkpoint.Messages = append(
		checkpoint.Messages,
		protocol.Message{
			Role: protocol.RoleTool, Content: string(result),
			Name: call.Name, ToolCallID: call.ID,
		},
	)
	checkpoint.Step++
	if loop.recorder != nil {
		loop.recorder.RecordTool(
			checkpoint.Messages[len(checkpoint.Messages)-1],
		)
	}
}

// derivePolicy reads the measured epistemic counters once per turn and derives
// the bounds the turn runs under. It fails closed: an unvalidatable policy is a
// typed incomplete over the checkpoint, never an unbounded turn.
func (loop *Loop) derivePolicy(
	userContent string,
	state cursor,
) (liveness.Policy, error) {
	var measured liveness.MeasuredContext
	if loop.liveness != nil {
		measured = loop.liveness.MeasuredContext()
	}
	return liveness.Derive(liveness.Inputs{
		Measured:    measured,
		Shape:       liveness.ClassifyShape(userContent),
		PriorRepair: state.repaired(),
	})
}

func (loop *Loop) generate(
	ctx context.Context,
	request protocol.GenerationRequest,
) (protocol.NormalizedGeneration, bool, error) {
	providerCtx, cancel := context.WithTimeout(ctx, loop.config.IdleTimeout)
	defer cancel()
	if loop.observer == nil {
		generation, err := loop.provider.Generate(
			providerCtx, request, &loop.usage,
		)
		return generation, false, err
	}
	streamer, ok := loop.provider.(StreamingGenerator)
	if !ok {
		generation, err := loop.provider.Generate(
			providerCtx, request, &loop.usage,
		)
		// A settled generation is not streaming. Do not replay it as fake deltas.
		return generation, false, err
	}
	streamed := false
	generation, err := streamer.GenerateStream(
		providerCtx, request, &loop.usage,
		func(chunk protocol.StreamChunk) error {
			if chunk.ReasoningDelta != "" {
				streamed = true
				if err := loop.streamDelta(providerCtx, records.StreamReasoning, chunk.ReasoningDelta); err != nil {
					return err
				}
			}
			if chunk.ContentDelta != "" {
				streamed = true
				return loop.streamDelta(providerCtx, records.StreamAnswer, chunk.ContentDelta)
			}
			return nil
		},
	)
	return generation, streamed, err
}

func (loop *Loop) streamDelta(ctx context.Context, channel records.StreamChannel, content string) error {
	if loop.stream != nil {
		return loop.stream.Delta(ctx, channel, content)
	}
	if loop.observer == nil {
		return nil
	}
	if channel == records.StreamReasoning {
		return loop.observer.ReasoningDelta(ctx, content)
	}
	return loop.observer.ContentDelta(ctx, content)
}

func (loop *Loop) retractAttempt(ctx context.Context) error {
	if loop.stream != nil {
		return loop.stream.Retract(ctx)
	}
	if loop.observer != nil {
		return loop.observer.Reset(ctx)
	}
	return nil
}

func (loop *Loop) commitAttempt(ctx context.Context) error {
	if loop.stream != nil {
		return loop.stream.Commit(ctx)
	}
	if loop.observer != nil {
		return loop.observer.CommitAttempt(ctx)
	}
	return nil
}

func (loop *Loop) commitToolAttempt(ctx context.Context, calls []protocol.NormalizedToolCall) error {
	if loop.stream != nil {
		if err := loop.stream.Retract(ctx); err != nil {
			return err
		}
		if toolObserver, ok := loop.observer.(interface {
			ObserveToolAttempt([]protocol.NormalizedToolCall)
		}); ok {
			toolObserver.ObserveToolAttempt(calls)
		}
		return nil
	}
	if toolObserver, ok := loop.observer.(interface {
		CommitToolAttempt(context.Context, []protocol.NormalizedToolCall) error
	}); ok {
		return toolObserver.CommitToolAttempt(ctx, calls)
	}
	return loop.retractAttempt(ctx)
}

func (loop *Loop) execute(
	ctx context.Context,
	call protocol.NormalizedToolCall,
	idempotencyKey string,
	started time.Time,
) (ToolResult, error) {
	toolCtx, cancel := context.WithTimeout(ctx, loop.config.IdleTimeout)
	defer cancel()
	toolCtx = turnstate.ContextWithLogicalTurn(toolCtx, loop.config.TurnID)
	result, err := loop.tools.Execute(
		toolCtx, call, idempotencyKey,
	)
	if errors.Is(toolCtx.Err(), context.DeadlineExceeded) &&
		ctx.Err() == nil {
		if err == nil {
			err = context.DeadlineExceeded
		}
		return result, fmt.Errorf(
			"tool idle since %s: %w", started.Format(time.RFC3339), err,
		)
	}
	return result, err
}

func (loop *Loop) reconcilePending(
	ctx context.Context,
	checkpoint *turnstate.Checkpoint,
	response *Response,
	state *cursor,
) error {
	pending := checkpoint.PendingCall
	reconciled, err := loop.tools.Reconcile(
		ctx, pending.IdempotencyKey,
	)
	if err != nil {
		return loop.incomplete(
			ctx, "effect_reconciliation", *checkpoint, *response,
			pending.DispatchedAt,
			"reconcile_effect_by_idempotency_key", err,
		)
	}
	call := protocol.NormalizedToolCall{
		ID: pending.CallID, Name: pending.ToolName,
		Arguments: append(json.RawMessage(nil), pending.Arguments...),
	}
	if reconciled.Status == ReconcileNotStarted {
		reconciled.Result = structuredToolFailure(
			"dispatch", false, "not_started",
			"No durable effect-start record exists; dispatch did not begin.",
			"Correct the tool call or choose another available operation.",
		)
	} else if reconciled.Status == ReconcileUnknown {
		reconciled.Result = structuredToolFailure(
			"effect_reconciliation", false, "outcome_unknown",
			"The effect may have started, but no authoritative completion is recorded.",
			"The effect is isolated from replay. Inspect authoritative external state before any further mutation.",
		)
	}
	if reconciled.Status == ReconcileRetrySafe {
		reconciled.Result, err = loop.execute(
			ctx, call, pending.IdempotencyKey, pending.DispatchedAt,
		)
		if err != nil {
			reconciled.Result = structuredToolFailure(
				"dispatch", true, "retry_safe",
				err.Error(),
				"Inspect the observation, change strategy, or retry later with backoff.",
			)
		}
	}
	reconciled.Result = state.annotateFailure(call, reconciled.Result)
	execution := ToolExecution{
		Call: call, Result: reconciled.Result.Content,
		Expect:         pending.Expect,
		IdempotencyKey: pending.IdempotencyKey,
		MatchVerdict: matchToolExpectation(
			pending.Expect, reconciled.Result,
		),
		SubgoalID: loop.subgoalFor(call),
	}
	loop.updateCodingCheckpoint(checkpoint, call, reconciled.Result)
	execution.Error = executionError(reconciled.Result)
	if err := loop.commitEvidence(
		ctx, &execution, *checkpoint, *response,
		pending.DispatchedAt,
	); err != nil {
		return err
	}
	response.ToolEvents = append(response.ToolEvents, execution)
	checkpoint.ToolEvents = append(
		checkpoint.ToolEvents, encodeToolExecution(execution),
	)
	checkpoint.Messages = append(
		checkpoint.Messages,
		protocol.Message{
			Role:    protocol.RoleTool,
			Content: string(reconciled.Result.Content),
			Name:    call.Name, ToolCallID: call.ID,
		},
	)
	checkpoint.PendingCall = nil
	checkpoint.Step++
	if err := loop.save(ctx, checkpoint, *state); err != nil {
		return err
	}
	if loop.recorder != nil {
		loop.recorder.RecordTool(
			checkpoint.Messages[len(checkpoint.Messages)-1],
		)
	}
	return nil
}

func structuredToolFailure(
	layer string,
	retryable bool,
	effectStatus string,
	evidence string,
	recovery string,
) ToolResult {
	normalizedCause := strings.Join(strings.Fields(evidence), " ")
	content, err := json.Marshal(map[string]any{
		"outcome": "error", "failure_layer": layer,
		"retryable": retryable, "effect_status": effectStatus,
		"evidence": []map[string]any{{
			"identity": "tool-failure", "kind": "inline",
			"content": map[string]string{"message": evidence},
		}},
		"normalized_cause":    normalizedCause,
		"suggested_recovery":  recovery,
		"artifact_references": []string{},
	})
	if err != nil {
		content = json.RawMessage(`{"outcome":"error","failure_layer":"runtime"}`)
	}
	return ToolResult{
		Content: content, IsError: true, FailureClass: layer,
		Retryable: retryable, FailureMessage: evidence,
	}
}

func structuredToolSuccess(evidence json.RawMessage) ToolResult {
	var decoded any
	if err := json.Unmarshal(evidence, &decoded); err != nil {
		decoded = map[string]string{"content": string(evidence)}
	}
	artifactReferences := []string{}
	if object, ok := decoded.(map[string]any); ok {
		if artifactID, ok := object["artifact_id"].(string); ok && strings.TrimSpace(artifactID) != "" {
			artifactReferences = append(artifactReferences, artifactID)
		}
	}
	content, err := json.Marshal(map[string]any{
		"outcome": "success", "failure_layer": "none",
		"retryable": false, "effect_status": "completed",
		"evidence": []map[string]any{{
			"identity": "tool-result", "kind": "inline", "content": decoded,
		}},
		"normalized_cause": "", "suggested_recovery": "",
		"artifact_references": artifactReferences,
	})
	if err != nil {
		content = json.RawMessage(`{"outcome":"error","failure_layer":"runtime","retryable":false,"effect_status":"failed"}`)
	}
	return ToolResult{Content: content}
}

func (loop *Loop) deliverAccepted(
	ctx context.Context,
	userContent string,
	checkpoint turnstate.Checkpoint,
	response Response,
	state cursor,
	generation protocol.NormalizedGeneration,
	streamed bool,
) (Response, error) {
	checkpoint.Messages = append(
		checkpoint.Messages,
		protocol.Message{
			Role:      protocol.RoleAssistant,
			Content:   generation.Content,
			Reasoning: generation.Reasoning,
		},
	)
	checkpoint.Step++
	response.Content = generation.Content
	response.Generation = generation
	response.ContentStreamed = streamed && generation.Content != ""
	response.ReasoningStreamed = streamed && generation.Reasoning != ""
	response.Checkpoint = &checkpoint
	// Acceptance is the durable boundary: persist the answer and AnswerReady
	// before stream bookkeeping or any reporter can acknowledge delivery.
	answerStore, hasAnswerStore := loop.store.(AnswerStateStore)
	answerRecord := records.AnswerRecord{}
	if hasAnswerStore {
		evidenceIDs := make([]string, 0, len(response.ToolEvents))
		for _, event := range response.ToolEvents {
			if event.IdempotencyKey != "" {
				evidenceIDs = append(evidenceIDs, event.IdempotencyKey+":result")
			}
		}
		streamState := records.StreamCommitted
		if streamed && loop.observer != nil {
			streamState = records.StreamProvisional
		}
		answerRecord = records.AnswerRecord{
			GeneratedAnswer:       generation.Content,
			SupportingEvidenceIDs: evidenceIDs,
			CompletionAssessment:  records.CompletionAssessment{Ready: true},
			StreamCommitState:     streamState,
		}
		if err := answerStore.SaveAnswerRecord(
			context.WithoutCancel(ctx), loop.config.TurnID, "accepted",
			answerRecord,
		); err != nil {
			return response, fmt.Errorf("runtime loop: persist accepted answer: %w", err)
		}
	}
	if terminalStore, ok := loop.store.(DeliveryStateStore); ok {
		if err := terminalStore.MarkAnswerReady(context.WithoutCancel(ctx), loop.config.TurnID, "accepted"); err != nil {
			return response, fmt.Errorf("runtime loop: mark answer ready: %w", err)
		}
	}
	// Checkpoint and stream bookkeeping are independent and cannot erase the
	// now-durable accepted answer.
	_ = loop.save(context.WithoutCancel(ctx), &checkpoint, state)
	if loop.observer != nil {
		if err := loop.commitAttempt(context.WithoutCancel(ctx)); err != nil {
			return response, fmt.Errorf("runtime loop: commit accepted stream: %w", err)
		}
	}
	if hasAnswerStore && answerRecord.StreamCommitState == records.StreamProvisional {
		answerRecord.StreamCommitState = records.StreamCommitted
		_ = answerStore.SaveAnswerRecord(
			context.WithoutCancel(ctx), loop.config.TurnID, "accepted", answerRecord,
		)
	}
	delivered, deliveryErr := loop.attemptDelivery(
		ctx, userContent, checkpoint, generation.Content,
		response.HonestPartial, &state,
	)
	if delivered.Content != "" {
		response.Content = delivered.Content
		response.Generation.Content = delivered.Content
	}
	if deliveryErr != nil {
		return response, deliveryErr
	}
	// Delivery is a distinct terminal state. Later status bookkeeping is
	// best-effort and can never replace a valid durable answer.
	_ = loop.store.SetTurnStatus(
		context.WithoutCancel(ctx), loop.config.TurnID, turnstate.StatusCompleted,
	)
	return response, nil
}

func (loop *Loop) attemptDelivery(
	ctx context.Context,
	userContent string,
	checkpoint turnstate.Checkpoint,
	content string,
	honestPartial bool,
	state *cursor,
) (DeliveryResult, error) {
	terminalStore, durable := loop.store.(DeliveryStateStore)
	deliveryRecord := records.DeliveryRecord{Channel: "chat", Attempts: []records.DeliveryAttempt{}}
	if durable {
		loaded, err := terminalStore.LoadDeliveryRecord(ctx, loop.config.TurnID, "primary")
		if err == nil {
			deliveryRecord = loaded
		} else if !errors.Is(err, sql.ErrNoRows) {
			return DeliveryResult{}, err
		}
	}
	for {
		if durable {
			if err := terminalStore.MarkDelivering(context.WithoutCancel(ctx), loop.config.TurnID, "primary"); err != nil {
				return DeliveryResult{}, err
			}
		}
		delivered := DeliveryResult{Content: content, Acknowledged: true}
		if loop.delivery != nil {
			delivered = loop.delivery.Deliver(ctx, userContent, checkpoint, content, honestPartial)
		}
		attempt := records.DeliveryAttempt{Number: uint64(len(deliveryRecord.Attempts) + 1), Error: delivered.Error}
		deliveryRecord.Attempts = append(deliveryRecord.Attempts, attempt)
		if delivered.Acknowledged {
			deliveryRecord.Acknowledgement = "acknowledged"
			deliveryRecord.LastDeliveryError = ""
		} else {
			deliveryRecord.LastDeliveryError = delivered.Error
		}
		if durable {
			if err := terminalStore.SaveDeliveryRecord(context.WithoutCancel(ctx), loop.config.TurnID, "primary", deliveryRecord); err != nil {
				return delivered, err
			}
		}
		if delivered.Acknowledged {
			if durable {
				if err := terminalStore.MarkDelivered(context.WithoutCancel(ctx), loop.config.TurnID); err != nil {
					return delivered, err
				}
			}
			return delivered, nil
		}
		if durable {
			if err := terminalStore.MarkDeliveryRetry(context.WithoutCancel(ctx), loop.config.TurnID); err != nil {
				return delivered, err
			}
		}
		decision := state.decide(converge.FailureDelivery, converge.Fingerprint{
			Operation: "delivery.chat", Phase: "delivery",
			FailureLayer: "delivery", NormalizedCause: delivered.Error,
			EffectStatus: "completed",
		})
		_ = loop.save(context.WithoutCancel(ctx), &checkpoint, *state)
		if !decision.Retry {
			return delivered, &DeliveryRetry{
				Answer: content, Attempts: attempt.Number, Cause: delivered.Error,
			}
		}
	}
}

// RetryDelivery resumes only the durable delivery state. It never composes
// context, calls a provider, reasons, or dispatches tools.
func (loop *Loop) RetryDelivery(ctx context.Context) (Response, error) {
	terminalStore, ok := loop.store.(DeliveryStateStore)
	if !ok {
		return Response{}, fmt.Errorf("runtime loop: durable delivery store unavailable")
	}
	stateStore, ok := loop.store.(interface {
		LoadTurnState(context.Context, string) (turnstate.TurnState, error)
	})
	if !ok {
		return Response{}, fmt.Errorf("runtime loop: turn snapshot unavailable")
	}
	turn, err := stateStore.LoadTurnState(ctx, loop.config.TurnID)
	if err != nil {
		return Response{}, err
	}
	answer, err := terminalStore.LoadAnswerRecord(ctx, loop.config.TurnID, "accepted")
	if err != nil {
		return Response{}, err
	}
	checkpoint := turnstate.Checkpoint{}
	if turn.Checkpoint != nil {
		checkpoint = *turn.Checkpoint
	}
	var state cursor
	if len(checkpoint.Runtime) > 0 {
		_ = json.Unmarshal(checkpoint.Runtime, &state)
	}
	delivered, err := loop.attemptDelivery(
		ctx, firstUserContent(checkpoint.Messages), checkpoint,
		answer.GeneratedAnswer, false, &state,
	)
	response := Response{Content: answer.GeneratedAnswer, Checkpoint: &checkpoint}
	if delivered.Content != "" {
		response.Content = delivered.Content
	}
	if err == nil {
		_ = loop.store.SetTurnStatus(context.WithoutCancel(ctx), loop.config.TurnID, turnstate.StatusCompleted)
	}
	return response, err
}

func (loop *Loop) deliverHonestPartial(
	ctx context.Context,
	userContent string,
	checkpoint turnstate.Checkpoint,
	response Response,
	state cursor,
	boundary string,
) (Response, error) {
	response.Repairs = state.repairDiagnostics()
	content := finalAnswerHonestFallback(response.ToolEvents)
	if boundary = strings.TrimSpace(boundary); boundary != "" {
		content = boundary + " " + content
	}
	generation := protocol.NormalizedGeneration{
		Content: content, FinishReason: protocol.FinishStop,
		Usage: loop.usage.Snapshot().Usage,
	}
	response.HonestPartial = true
	response.ContentStreamed = false
	response.ReasoningStreamed = false
	return loop.deliverAccepted(
		ctx, userContent, checkpoint, response, state, generation, false,
	)
}

func (loop *Loop) save(
	ctx context.Context,
	checkpoint *turnstate.Checkpoint,
	state cursor,
) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("runtime loop: encode checkpoint cursor: %w", err)
	}
	checkpoint.Runtime = encoded
	checkpoint.SavedAt = time.Now().UTC()
	if err := loop.store.SaveTurnCheckpoint(
		ctx, loop.config.TurnID, *checkpoint,
	); err != nil {
		return fmt.Errorf("runtime loop: save checkpoint: %w", err)
	}
	if store, ok := loop.store.(ConvergenceStore); ok {
		if err := store.SaveConvergenceRecord(ctx, loop.config.TurnID, loop.convergenceRecord(*checkpoint, state)); err != nil {
			return fmt.Errorf("runtime loop: save convergence record: %w", err)
		}
	}
	return nil
}

func (loop *Loop) convergenceRecord(checkpoint turnstate.Checkpoint, state cursor) records.ConvergenceRecord {
	posture := state.Posture
	if posture == "" {
		posture = converge.Execution
		if loop.config.MaxToolCalls <= 4 {
			posture = converge.Conversation
		} else if loop.config.MaxToolCalls <= 20 {
			posture = converge.Exploration
		}
	}
	limits := converge.ForPosture(posture)
	record := records.ConvergenceRecord{
		CumulativeInputTokens:  uint64(max(state.InputTokens, 0)),
		CumulativeOutputTokens: uint64(max(state.OutputTokens, 0)),
		ToolCalls:              uint64(max(state.ToolCalls, 0)),
		ProviderUsage:          []records.ProviderUsage{{Provider: "configured", Model: loop.config.Model, RequestCount: uint64(max(checkpoint.ProviderAttempts, 0)), InputTokens: uint64(max(state.InputTokens, 0)), OutputTokens: uint64(max(state.OutputTokens, 0))}},
		RepairCounters: map[string]uint64{
			"textual": uint64(max(state.TextualRepairs, 0)), "expectation": uint64(max(state.ExpectationRepairs, 0)),
			"empty": uint64(max(state.EmptyRepairs, 0)), "final_answer": uint64(max(state.FinalRepairs, 0)),
			"completion_deferrals": uint64(max(state.CompletionDefers, 0)),
		},
		Tuning: map[string]uint64{
			"provider_calls": limits.ProviderCalls, "tool_calls": limits.ToolCalls,
			"cumulative_input_tokens": limits.CumulativeInputTokens,
			"preferred_input_tokens":  limits.PreferredInputTokens, "hard_input_tokens": limits.HardInputTokens,
			"response_reserve_tokens": limits.ResponseReserveTokens, "synthesis_reserve": limits.SynthesisReserve,
			"identical_failure_repeats": limits.IdenticalFailureRepeats,
		},
	}
	attemptKeys := make([]string, 0, len(state.Controller.Attempts))
	for key := range state.Controller.Attempts {
		attemptKeys = append(attemptKeys, key)
	}
	sort.Strings(attemptKeys)
	for _, key := range attemptKeys {
		attempt := state.Controller.Attempts[key]
		fingerprint := records.FailureFingerprint{
			Operation:           attempt.Failure.Fingerprint.Operation,
			NormalizedArguments: append(json.RawMessage(nil), attempt.Failure.Fingerprint.NormalizedArguments...),
			Phase:               attempt.Failure.Fingerprint.Phase,
			FailureLayer:        records.FailureLayer(attempt.Failure.Fingerprint.FailureLayer),
			NormalizedCause:     attempt.Failure.Fingerprint.NormalizedCause,
			EffectStatus:        records.EffectStatus(attempt.Failure.Fingerprint.EffectStatus),
		}
		record.FailureFingerprints = append(record.FailureFingerprints, fingerprint)
		record.AttemptCounts = append(record.AttemptCounts, records.AttemptCount{Fingerprint: fingerprint, Count: attempt.Count})
	}
	record.StrategyChanges = append([]string(nil), state.Controller.StrategyChanges...)
	return record
}

func isResearchReadOperation(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"search", "fetch", "news", "quote", "series", "fundamentals", "earnings", "read_text", "read_multiple", "git_log", "git_show", "service_logs"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func (loop *Loop) incomplete(
	ctx context.Context,
	phase string,
	checkpoint turnstate.Checkpoint,
	response Response,
	started time.Time,
	advice string,
	cause error,
) *Incomplete {
	if started.IsZero() {
		started = time.Now().UTC()
	}
	var evidence json.RawMessage
	if len(response.ToolEvents) > 0 {
		evidence = encodeToolExecution(
			response.ToolEvents[len(response.ToolEvents)-1],
		)
	}
	var state cursor
	if len(checkpoint.Runtime) > 0 {
		_ = json.Unmarshal(checkpoint.Runtime, &state)
	}
	causeDetail := ""
	if cause != nil {
		causeDetail = strings.TrimSpace(cause.Error())
	}
	providerError := ""
	if phase == "provider" {
		providerError = causeDetail
	}
	incomplete := &Incomplete{
		Phase: phase, LastToolEvidence: evidence,
		StartedAt: started, AttemptCount: checkpoint.ProviderAttempts,
		RecoveryAdvice: advice, Repairs: state.repairDiagnostics(),
		CauseDetail: causeDetail, ProviderError: providerError,
		Checkpoint: checkpoint, Cause: cause,
	}
	if incomplete.AttemptCount < 1 {
		incomplete.AttemptCount = 1
	}
	_ = loop.store.SetTurnRecovery(
		ctx, loop.config.TurnID, turnstate.StatusIncomplete,
		turnstate.Recovery{
			Phase: phase, LastToolEvidence: evidence,
			StartedAt:    started,
			AttemptCount: incomplete.AttemptCount,
			Advice:       advice,
		},
	)
	if loop.incompletes != nil {
		loop.incompletes.RecordIncomplete(ctx, IncompleteRecord{
			TurnID:         loop.config.TurnID,
			ConversationID: loop.config.ConversationID,
			Phase:          phase,
			LastToolEvidence: append(
				json.RawMessage(nil), evidence...,
			),
			StartedAt:      started,
			AttemptCount:   incomplete.AttemptCount,
			RecoveryAdvice: advice,
			Repairs:        incomplete.Repairs,
			CauseDetail:    incomplete.CauseDetail,
			ProviderError:  incomplete.ProviderError,
		})
	}
	if loop.delivery != nil {
		loop.terminalOnce.Do(func() {
			loop.delivery.FinalizeIncomplete(ctx, checkpoint, incomplete)
		})
	}
	return incomplete
}

func (loop *Loop) initializeCodingCheckpoint(checkpoint *turnstate.Checkpoint, requirement string) {
	root := strings.TrimSpace(loop.config.ProjectRoot)
	if checkpoint == nil || root == "" {
		return
	}
	checkpoint.Coding = &turnstate.CodingCheckpoint{
		ProjectRoot:  root,
		Requirements: []string{strings.TrimSpace(requirement)},
		NextAction:   "Inspect the project and begin the next requirement.",
		UpdatedAt:    time.Now().UTC(),
	}
}

func (loop *Loop) updateCodingCheckpoint(checkpoint *turnstate.Checkpoint, call protocol.NormalizedToolCall, result ToolResult) {
	if checkpoint == nil || checkpoint.Coding == nil {
		return
	}
	coding := checkpoint.Coding
	base := strings.ToLower(call.Name)
	if index := strings.LastIndex(base, "__"); index >= 0 {
		base = base[index+2:]
	}
	var args map[string]interface{}
	_ = json.Unmarshal(call.Arguments, &args)
	var envelope map[string]interface{}
	_ = json.Unmarshal(result.Content, &envelope)
	mutation := false
	switch base {
	case "write_file", "edit_file", "create_directory":
		mutation = true
		addCodingFile(coding, stringValue(args["path"]))
	case "move_file":
		mutation = true
		addCodingFile(coding, stringValue(args["destination"]))
	case "patch_files":
		mutation = true
		if files, ok := envelope["files"].([]interface{}); ok {
			for _, raw := range files {
				if file, ok := raw.(map[string]interface{}); ok {
					addCodingFile(coding, stringValue(file["path"]))
				}
			}
		}
	}
	if base == "build_project" {
		coding.NextAction = "End this turn and await the durable Build outcome."
	} else if isVerificationCommand(base, args) {
		exit, _ := numberAsInt(envelope["exit_code"])
		timedOut, _ := envelope["timed_out"].(bool)
		succeeded := !result.IsError && exit == 0 && !timedOut
		coding.LastVerification = &turnstate.CodingVerification{
			Command: stringValue(args["command"]), CWD: stringValue(envelope["cwd"]),
			ExitCode: exit, Succeeded: succeeded, TimedOut: timedOut,
			VerifiedAt: time.Now().UTC(),
		}
		if succeeded {
			coding.CurrentFailures = nil
			coding.NextAction = "Review the acceptance criteria and deliver the verified result."
		} else {
			coding.CurrentFailures = codingFailures(result, envelope)
			coding.NextAction = "Correct the reported verification failures."
		}
	} else if result.IsError || shellEnvelopeFailed(envelope) {
		coding.CurrentFailures = codingFailures(result, envelope)
		coding.NextAction = "Correct the failed operation before continuing."
	} else if mutation {
		coding.NextAction = "Run the relevant project verification."
	}
	coding.UpdatedAt = time.Now().UTC()
}

func addCodingFile(checkpoint *turnstate.CodingCheckpoint, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	for _, existing := range checkpoint.FilesChanged {
		if existing == path {
			return
		}
	}
	if len(checkpoint.FilesChanged) < 256 {
		checkpoint.FilesChanged = append(checkpoint.FilesChanged, path)
	}
}

func stringValue(value interface{}) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func isVerificationCommand(base string, args map[string]interface{}) bool {
	if base != "shell" && base != "run" && !strings.Contains(base, "exec") && !strings.Contains(base, "command") {
		return false
	}
	command := strings.ToLower(stringValue(args["command"]))
	for _, marker := range []string{"go test", "go vet", "go build", " test", " lint", "typecheck", " check", "cargo test", "pytest"} {
		if strings.Contains(" "+command, marker) {
			return true
		}
	}
	return false
}

func shellEnvelopeFailed(envelope map[string]interface{}) bool {
	if timedOut, _ := envelope["timed_out"].(bool); timedOut {
		return true
	}
	exit, exists := numberAsInt(envelope["exit_code"])
	return exists && exit != 0
}

func codingFailures(result ToolResult, envelope map[string]interface{}) []string {
	values := []string{strings.TrimSpace(result.FailureMessage)}
	for _, stream := range []string{"stderr", "stdout"} {
		if structured, ok := envelope[stream].(map[string]interface{}); ok {
			values = append(values, stringValue(structured["text"]))
		} else {
			values = append(values, stringValue(envelope[stream]))
		}
	}
	values = append(values, stringValue(envelope["error"]))
	out := make([]string, 0, 3)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 1200 {
			value = value[:1200]
		}
		out = append(out, value)
		if len(out) == 3 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, "The operation returned a failure without diagnostic text.")
	}
	return out
}

func appendCodingCheckpoint(messages []protocol.Message, checkpoint *turnstate.CodingCheckpoint) []protocol.Message {
	if checkpoint == nil {
		return messages
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return messages
	}
	message := protocol.Message{
		Role:    protocol.RoleSystem,
		Content: "Structured coding checkpoint (runtime-owned; resume from this state without narrating recovery):\n" + string(encoded),
	}
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == protocol.RoleSystem {
		insertAt++
	}
	result := make([]protocol.Message, 0, len(messages)+1)
	result = append(result, cloneMessages(messages[:insertAt])...)
	result = append(result, message)
	result = append(result, cloneMessages(messages[insertAt:])...)
	return result
}

func requestMessages(
	systemPrompt string,
	durable []protocol.Message,
	repair string,
	activation string,
) []protocol.Message {
	messages, _ := composeRequestMessages("", systemPrompt, durable, repair, activation)
	return messages
}

func composeRequestMessages(
	conversationID string,
	systemPrompt string,
	durable []protocol.Message,
	repair string,
	activation string,
) ([]protocol.Message, records.ContextManifest) {
	items := make([]runtimecompose.Item, 0, len(durable)+3)
	if prompt := strings.TrimSpace(systemPrompt); prompt != "" {
		items = append(items, runtimecompose.Item{SourceNamespace: "stable", SourceID: "charter", ConversationID: conversationID, SemanticKind: "stable_identity", RevisionIdentity: "1", Content: prompt, Sector: runtimecompose.SectorStableIdentity, NeverTrim: true})
	}
	latestUser := -1
	for index, message := range durable {
		if message.Role == protocol.RoleUser {
			latestUser = index
		}
	}
	for index, message := range durable {
		kind := "transcript_" + string(message.Role)
		sector := runtimecompose.SectorRecentTranscript
		neverTrim := index == latestUser
		if neverTrim {
			sector = runtimecompose.SectorLatestMessage
		}
		if message.Role == protocol.RoleTool {
			kind = "tool_result"
			if index > latestUser {
				sector = runtimecompose.SectorUnconsumedToolBatch
				neverTrim = true
			}
		}
		items = append(items, runtimecompose.Item{SourceNamespace: "transcript", SourceID: fmt.Sprintf("%d", index), ConversationID: conversationID, SemanticKind: kind, Content: message.Content, Sector: sector, NeverTrim: neverTrim})
	}
	if tail := strings.TrimSpace(activation); tail != "" {
		items = append(items, runtimecompose.Item{SourceNamespace: "memory", SourceID: "activation", ConversationID: conversationID, SemanticKind: "memory_activation", Content: tail, Sector: runtimecompose.SectorLongTermMemory})
	}
	if prompt := strings.TrimSpace(repair); prompt != "" {
		items = append(items, runtimecompose.Item{SourceNamespace: "controller", SourceID: "repair", ConversationID: conversationID, SemanticKind: "controller_repair", Content: prompt, Sector: runtimecompose.SectorWorkingState})
	}
	included, manifest, diagnostics := runtimecompose.Compose(items, runtimecompose.DefaultSectorPolicy(160_000, 8_192))
	accepted := make(map[string]bool, len(included))
	for _, item := range included {
		accepted[item.SourceNamespace+"\x00"+item.SourceID] = true
	}
	result := make([]protocol.Message, 0, len(durable)+3)
	if prompt := strings.TrimSpace(systemPrompt); prompt != "" && accepted["stable\x00charter"] {
		result = append(result, protocol.Message{
			Role: protocol.RoleSystem, Content: prompt,
		})
	}
	// Memory and controller repair are reference context. They must precede
	// the durable conversation so the newest user message remains the final,
	// authoritative live request on the initial generation.
	if tail := strings.TrimSpace(activation); tail != "" && accepted["memory\x00activation"] {
		result = append(result, protocol.Message{
			Role: protocol.RoleSystem, Content: tail,
		})
	}
	if prompt := strings.TrimSpace(repair); prompt != "" && accepted["controller\x00repair"] {
		result = append(result, protocol.Message{
			Role: protocol.RoleSystem, Content: prompt,
		})
	}
	if len(diagnostics) > 0 {
		result = append(result, protocol.Message{Role: protocol.RoleSystem, Content: "[context_diagnostics]\n" + strings.Join(diagnostics, "\n")})
	}
	for index, message := range durable {
		if accepted["transcript\x00"+fmt.Sprintf("%d", index)] {
			result = append(result, message)
		}
	}
	return result, manifest
}

func appendGuidance(
	messages []protocol.Message,
	guidance []string,
) []protocol.Message {
	var accepted []string
	for _, instruction := range guidance {
		if instruction = strings.TrimSpace(instruction); instruction != "" {
			accepted = append(accepted, instruction)
		}
	}
	if len(accepted) == 0 {
		return messages
	}
	guidanceMessage := protocol.Message{
		Role: protocol.RoleSystem,
		Content: "Live supervisor guidance:\n" +
			strings.Join(accepted, "\n"),
	}
	// Keep guidance in the system prefix. Appending it after the conversation
	// would make it newer than the user's request and recreate the authority
	// inversion that caused cross-thread context to win.
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == protocol.RoleSystem {
		insertAt++
	}
	result := make([]protocol.Message, 0, len(messages)+1)
	result = append(result, cloneMessages(messages[:insertAt])...)
	result = append(result, guidanceMessage)
	result = append(result, cloneMessages(messages[insertAt:])...)
	return result
}

func completionContinuationPrompt(decision CompletionDecision) string {
	return "Durable completion gate: the task is not complete from server-verified evidence. " +
		"Do not give a final answer yet. Continue with the next dependency-ready work item. " +
		"Current gap: " + strings.TrimSpace(decision.Reason) +
		". Next action: " + strings.TrimSpace(decision.NextAction)
}

func extractExpectations(
	calls []protocol.NormalizedToolCall,
) ([]string, []protocol.NormalizedToolCall, error) {
	expectations := make([]string, len(calls))
	result := cloneCalls(calls)
	for index := range result {
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal(
			result[index].Arguments, &arguments,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"runtime loop: invalid tool arguments: %w", err,
			)
		}
		raw, exists := arguments["expect"]
		delete(arguments, "expect")
		if exists {
			_ = json.Unmarshal(raw, &expectations[index])
			expectations[index] = strings.TrimSpace(expectations[index])
		}
		if runtimeUncertainProbe(result[index].Name) && expectations[index] == "" {
			return nil, nil, fmt.Errorf(
				"runtime loop: tool call %s is missing expect",
				result[index].ID,
			)
		}
		stripped, err := json.Marshal(arguments)
		if err != nil {
			return nil, nil, err
		}
		result[index].Arguments = stripped
	}
	return expectations, result, nil
}

func runtimeUncertainProbe(name string) bool {
	base := strings.ToLower(strings.TrimSpace(name))
	if index := strings.LastIndex(base, "__"); index >= 0 {
		base = base[index+2:]
	}
	if base == "search_files" {
		return false
	}
	return strings.Contains(base, "web_search") ||
		strings.Contains(base, "web_news") ||
		strings.Contains(base, "exa_search") ||
		base == "search" ||
		strings.Contains(base, "fetch") ||
		strings.Contains(base, "http") ||
		strings.Contains(base, "request") ||
		strings.Contains(base, "navigate") ||
		strings.Contains(base, "download") ||
		base == "web_read"
}

func makeIdempotencyKey(
	turnID string,
	call protocol.NormalizedToolCall,
) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(turnID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(call.ID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(call.Name))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(call.Arguments)
	return hex.EncodeToString(digest.Sum(nil))
}

func responseFromCheckpoint(
	checkpoint turnstate.Checkpoint,
) (Response, error) {
	response := Response{ProviderCalls: checkpoint.ProviderAttempts}
	for _, raw := range checkpoint.ToolEvents {
		var event ToolExecution
		if err := json.Unmarshal(raw, &event); err != nil {
			return Response{}, fmt.Errorf(
				"runtime loop: decode tool event: %w", err,
			)
		}
		response.ToolEvents = append(response.ToolEvents, event)
	}
	return response, nil
}

func validateCheckpoint(
	userContent string,
	checkpoint turnstate.Checkpoint,
) error {
	if strings.TrimSpace(userContent) == "" || len(checkpoint.Messages) == 0 {
		return fmt.Errorf("runtime loop: invalid resume checkpoint")
	}
	first := checkpoint.Messages[0]
	if first.Role != protocol.RoleUser ||
		first.Content != userContent {
		return fmt.Errorf(
			"runtime loop: checkpoint does not match the active user turn",
		)
	}
	return checkpoint.ValidateForResume()
}

func cloneMessages(messages []protocol.Message) []protocol.Message {
	result := make([]protocol.Message, 0, len(messages))
	for _, message := range messages {
		message.ToolCalls = cloneCalls(message.ToolCalls)
		result = append(result, message)
	}
	return result
}

func cloneCalls(
	calls []protocol.NormalizedToolCall,
) []protocol.NormalizedToolCall {
	result := make([]protocol.NormalizedToolCall, 0, len(calls))
	for _, call := range calls {
		call.Arguments = append(json.RawMessage(nil), call.Arguments...)
		result = append(result, call)
	}
	return result
}

func encodeToolExecution(event ToolExecution) json.RawMessage {
	encoded, err := json.Marshal(event)
	if err != nil {
		return json.RawMessage(`{"error":"tool event encoding failed"}`)
	}
	return encoded
}
