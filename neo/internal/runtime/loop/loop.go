// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/cortex"
	"matrix/neo/internal/runtime/liveness"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
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
	textualToolRepairPrompt = "The previous provider response used invalid textual tool-call markup. Do not repeat or quote that markup. Either emit a native structured tool call that satisfies the active JSON Schema, including the required expect prediction, or return a normal final answer with no tool-call tags."
	textualToolFinalPrompt  = "Tool use is now disabled because repeated responses used unsafe tool-call markup. Give the user an honest ordinary-text answer from the available conversation and evidence. State any action you could not complete. Do not emit or quote tool names, tool-call tags, function tags, parameter tags, XML, JSON, or code fences."
	expectationRepairPrompt = "The previous structured tool call was not executed because at least one call omitted its required non-empty expect prediction. Resubmit the intended native structured tool call, adding a one-line expect prediction to every call, or answer normally without tools."
	expectationFinalPrompt  = "Tool use is now disabled because repeated structured calls omitted the required expect prediction. Give the user an honest ordinary-text answer from the available conversation and evidence, and state any action you could not complete. Do not call tools."
	emptyFinalRepairPrompt  = "The previous response ended without a user-facing answer. Answer the original user request now as ordinary text using the available conversation and tool results. Do not call tools, emit tool-call markup, or return an empty response."
)

const (
	explorationExhaustedResult = `{"error":"exploration budget exhausted by the enforced liveness decision policy"}`
	parallelismDeferredResult  = `{"error":"dispatch deferred: enforced liveness parallelism limit"}`
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

type Config struct {
	TurnID                 string
	ConversationID         string
	Model                  string
	SystemPrompt           string
	MaxOutputTokens        int
	MaxToolCalls           int
	IdleTimeout            time.Duration
	TextualRepairLimit     int
	ExpectationRepairLimit int
	EmptyRepairLimit       int
	FinalAnswerRepairLimit int
	CompletionDeferrals    int
	MaxTurnTokens          int
	Breaker                liveness.BreakerConfig
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
	Call           protocol.NormalizedToolCall `json:"call"`
	Result         json.RawMessage             `json:"result"`
	Error          string                      `json:"error,omitempty"`
	Expect         string                      `json:"expect,omitempty"`
	IdempotencyKey string                      `json:"idempotency_key"`
	MatchVerdict   string                      `json:"match_verdict"`
	SubgoalID      string                      `json:"subgoal_id"`
	Citation       *cortex.ToolEventCitation   `json:"citation,omitempty"`
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
	Checkpoint       turnstate.Checkpoint `json:"checkpoint"`
	Cause            error                `json:"-"`
}

type IncompleteRecord struct {
	TurnID           string          `json:"turn_id"`
	ConversationID   string          `json:"conversation_id,omitempty"`
	Phase            string          `json:"phase"`
	LastToolEvidence json.RawMessage `json:"last_tool_evidence,omitempty"`
	StartedAt        time.Time       `json:"started_at"`
	AttemptCount     int             `json:"attempt_count"`
	RecoveryAdvice   string          `json:"recovery_advice"`
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
	ToolCalls          int                   `json:"tool_calls"`
	TextualRepairs     int                   `json:"textual_repairs"`
	ExpectationRepairs int                   `json:"expectation_repairs"`
	EmptyRepairs       int                   `json:"empty_repairs"`
	FinalRepairs       int                   `json:"final_repairs"`
	CompletionDefers   int                   `json:"completion_defers"`
	ExplorationCalls   int                   `json:"exploration_calls,omitempty"`
	LastSignature      string                `json:"last_signature,omitempty"`
	RepeatedCalls      int                   `json:"repeated_calls,omitempty"`
	Circuit            liveness.BreakerState `json:"circuit,omitempty"`
	TokensSpent        int                   `json:"tokens_spent,omitempty"`
}

func (state cursor) repaired() bool {
	return state.TextualRepairs > 0 || state.ExpectationRepairs > 0 ||
		state.EmptyRepairs > 0 || state.FinalRepairs > 0
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
	return &Loop{
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
	}, nil
}

func (loop *Loop) Turn(
	ctx context.Context,
	userContent string,
) (Response, error) {
	userContent = strings.TrimSpace(userContent)
	if userContent == "" {
		return Response{}, fmt.Errorf("runtime loop: user message is required")
	}
	checkpoint := turnstate.Checkpoint{
		Messages: []protocol.Message{{
			Role: protocol.RoleUser, Content: userContent,
		}},
	}
	if err := loop.save(ctx, &checkpoint, cursor{}); err != nil {
		return Response{}, err
	}
	if loop.recorder != nil {
		loop.recorder.RecordUser(userContent)
	}
	return loop.runTurn(ctx, userContent, checkpoint, cursor{})
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
	toolCallLimit := loop.config.MaxToolCalls
	if policy.ToolCallBudget < toolCallLimit {
		toolCallLimit = policy.ToolCallBudget
	}
	breaker := loop.config.Breaker
	if err := state.Circuit.Enforce("turn"); err != nil {
		return response, loop.incomplete(
			ctx, boundCircuitBreaker, checkpoint, response,
			time.Now().UTC(), "resume_with_a_different_strategy", err,
		)
	}
	tools := loop.tools.Surface(ctx)
	if checkpoint.PendingCall != nil {
		if err := loop.reconcilePending(
			ctx, &checkpoint, &response, state,
		); err != nil {
			return response, err
		}
	}
	var nextRepair string
	var pendingDoubt string
	toolsDisabled := false
	revisionActive := false
	for {
		if err := ctx.Err(); err != nil {
			return response, loop.incomplete(
				context.WithoutCancel(ctx), "turn", checkpoint, response,
				time.Now().UTC(), "resume_from_checkpoint", err,
			)
		}
		if err := state.Circuit.Enforce("provider"); err != nil {
			return response, loop.incomplete(
				ctx, boundCircuitBreaker, checkpoint, response,
				time.Now().UTC(), "resume_with_a_different_strategy", err,
			)
		}
		// The cumulative token budget is checked HERE and nowhere else: at the
		// top of an iteration, before the next provider call, after every path
		// that could have delivered a finished answer has already returned. A
		// budget bound must never be able to eat a completed answer — that is
		// the o1-budget-kill class — so exhaustion only ever prevents MORE
		// generation, and it routes to the honest partial rather than a death.
		if loop.config.MaxTurnTokens > 0 &&
			state.TokensSpent >= loop.config.MaxTurnTokens {
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
				return response, loop.incomplete(
					ctx, "memory_activation", checkpoint, response,
					time.Now().UTC(), "resume_memory_activation", err,
				)
			}
		}
		request := protocol.GenerationRequest{
			Model: loop.config.Model,
			Messages: requestMessages(
				loop.config.SystemPrompt,
				checkpoint.Messages,
				nextRepair,
				activation,
			),
			Tools:           tools,
			MaxOutputTokens: loop.config.MaxOutputTokens,
			Stream:          loop.observer != nil,
		}
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
		// checkpoint.Messages, so the durable window and the cortex
		// transcript stay free of controller guidance.
		if pendingDoubt != "" {
			request.Messages = foldDoubt(request.Messages, pendingDoubt)
			request.Tools = nil
			pendingDoubt = ""
			revisionActive = true
		}
		started := time.Now().UTC()
		generation, streamed, generateErr := loop.generate(ctx, request)
		checkpoint.ProviderAttempts++
		response.ProviderCalls = checkpoint.ProviderAttempts
		response.Usage = loop.usage.Snapshot().Usage
		state.TokensSpent = carriedTokens + response.Usage.TotalTokens
		response.Liveness.TokensSpent = state.TokensSpent
		if generateErr != nil {
			revisionActive = false
			if loop.observer != nil && streamed {
				if resetErr := loop.observer.Reset(ctx); resetErr != nil {
					generateErr = errors.Join(generateErr, resetErr)
				}
			}
			if state.TextualRepairs < loop.config.TextualRepairLimit &&
				(errors.Is(generateErr, provider.ErrToolProtocol) ||
					errors.Is(generateErr, ErrTextualToolMarkup)) {
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
				if resetErr := loop.observer.Reset(ctx); resetErr != nil {
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
				_ = loop.observer.Reset(ctx)
			}
			if state.TextualRepairs < loop.config.TextualRepairLimit {
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
		expectations, calls, expectationErr :=
			extractExpectations(generation.ToolCalls)
		if expectationErr != nil {
			if loop.observer != nil && streamed {
				_ = loop.observer.Reset(ctx)
			}
			if state.ExpectationRepairs <
				loop.config.ExpectationRepairLimit {
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
					_ = loop.observer.Reset(ctx)
				}
				if state.EmptyRepairs < loop.config.EmptyRepairLimit {
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
			if accepted, reason := finalAnswerAddressesRequest(
				userContent, content, response.ToolEvents,
			); !accepted {
				if loop.observer != nil && streamed {
					_ = loop.observer.Reset(ctx)
				}
				if state.FinalRepairs <
					loop.config.FinalAnswerRepairLimit {
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
						_ = loop.observer.Reset(ctx)
					}
					if state.CompletionDefers >=
						loop.config.CompletionDeferrals {
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
			if err := loop.observer.CommitAttempt(ctx); err != nil {
				return response, loop.incomplete(
					ctx, "observer_commit", checkpoint, response,
					started, "resume_from_checkpoint", err,
				)
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
				return response, loop.incomplete(
					ctx, boundToolBudget, checkpoint, response, started,
					"resume_with_a_different_strategy", nil,
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
				return response, loop.incomplete(
					ctx, boundCircuitBreaker, checkpoint, response, started,
					"resume_with_a_different_strategy", err,
				)
			}
			signature := liveness.CanonicalToolSignature(
				call.Name, call.Arguments,
			)
			if signature == state.LastSignature &&
				state.RepeatedCalls >= policy.SameStrategyRetries+1 {
				response.Liveness.Enforcements = append(
					response.Liveness.Enforcements,
					LivenessEnforcement{
						Bound: boundSameStrategy, Tool: call.Name,
						CallID: call.ID, Observed: state.RepeatedCalls,
						Limit: policy.SameStrategyRetries,
					},
				)
				return response, loop.incomplete(
					ctx, "tool_loop", checkpoint, response, started,
					"change_strategy_the_repeated_call_produced_no_new_evidence",
					nil,
				)
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
			pending := &turnstate.PendingCall{
				CallID: call.ID, IdempotencyKey: idempotencyKey,
				ToolName: call.Name, Arguments: call.Arguments,
				Expect:       expectations[index],
				DispatchedAt: time.Now().UTC(),
			}
			checkpoint.PendingCall = pending
			if err := loop.save(ctx, &checkpoint, state); err != nil {
				return response, err
			}
			result, executeErr := loop.execute(
				ctx, call, idempotencyKey, started,
			)
			if executeErr != nil {
				return response, loop.incomplete(
					context.WithoutCancel(ctx), "tool", checkpoint,
					response, started,
					"reconcile_effect_by_idempotency_key",
					executeErr,
				)
			}
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
		MatchVerdict: cortex.ToolMatchUnknown,
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
		if err != nil {
			return generation, false, err
		}
		streamed := false
		if generation.Reasoning != "" {
			streamed = true
			if err := loop.observer.ReasoningDelta(
				providerCtx, generation.Reasoning,
			); err != nil {
				return protocol.NormalizedGeneration{}, streamed, err
			}
		}
		if generation.Content != "" {
			streamed = true
			if err := loop.observer.ContentDelta(
				providerCtx, generation.Content,
			); err != nil {
				return protocol.NormalizedGeneration{}, streamed, err
			}
		}
		return generation, streamed, nil
	}
	streamed := false
	generation, err := streamer.GenerateStream(
		providerCtx, request, &loop.usage,
		func(chunk protocol.StreamChunk) error {
			if chunk.ReasoningDelta != "" {
				streamed = true
				if err := loop.observer.ReasoningDelta(
					providerCtx, chunk.ReasoningDelta,
				); err != nil {
					return err
				}
			}
			if chunk.ContentDelta != "" {
				streamed = true
				return loop.observer.ContentDelta(
					providerCtx, chunk.ContentDelta,
				)
			}
			return nil
		},
	)
	return generation, streamed, err
}

func (loop *Loop) execute(
	ctx context.Context,
	call protocol.NormalizedToolCall,
	idempotencyKey string,
	started time.Time,
) (ToolResult, error) {
	toolCtx, cancel := context.WithTimeout(ctx, loop.config.IdleTimeout)
	defer cancel()
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
	state cursor,
) error {
	pending := checkpoint.PendingCall
	reconciled, err := loop.tools.Reconcile(
		ctx, pending.IdempotencyKey,
	)
	if err != nil || reconciled.Status == ReconcileUnknown {
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
	if reconciled.Status == ReconcileRetrySafe {
		reconciled.Result, err = loop.execute(
			ctx, call, pending.IdempotencyKey, pending.DispatchedAt,
		)
		if err != nil {
			return loop.incomplete(
				ctx, "effect_reconciliation", *checkpoint, *response,
				pending.DispatchedAt,
				"reconcile_effect_by_idempotency_key", err,
			)
		}
	}
	execution := ToolExecution{
		Call: call, Result: reconciled.Result.Content,
		Expect:         pending.Expect,
		IdempotencyKey: pending.IdempotencyKey,
		MatchVerdict: matchToolExpectation(
			pending.Expect, reconciled.Result,
		),
		SubgoalID: loop.subgoalFor(call),
	}
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
	if err := loop.save(ctx, checkpoint, state); err != nil {
		return err
	}
	if loop.recorder != nil {
		loop.recorder.RecordTool(
			checkpoint.Messages[len(checkpoint.Messages)-1],
		)
	}
	return nil
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
	if err := loop.save(ctx, &checkpoint, state); err != nil {
		return response, err
	}
	if loop.observer != nil {
		if err := loop.observer.CommitAttempt(ctx); err != nil {
			return response, loop.incomplete(
				ctx, "observer_commit", checkpoint, response,
				time.Now().UTC(), "resume_delivery", err,
			)
		}
	}
	response.Content = generation.Content
	response.Generation = generation
	response.ContentStreamed = streamed && generation.Content != ""
	response.ReasoningStreamed = streamed && generation.Reasoning != ""
	response.Checkpoint = &checkpoint
	if err := loop.store.SetTurnStatus(
		ctx, loop.config.TurnID, turnstate.StatusCompleted,
	); err != nil {
		return response, loop.incomplete(
			ctx, "completion_commit", checkpoint, response,
			time.Now().UTC(), "resume_delivery", err,
		)
	}
	if loop.delivery != nil {
		var delivered DeliveryResult
		loop.terminalOnce.Do(func() {
			delivered = loop.delivery.Deliver(
				ctx, userContent, checkpoint, generation.Content,
				response.HonestPartial,
			)
		})
		if delivered.Content != "" {
			response.Content = delivered.Content
			response.Generation.Content = delivered.Content
		}
	}
	return response, nil
}

func (loop *Loop) deliverHonestPartial(
	ctx context.Context,
	userContent string,
	checkpoint turnstate.Checkpoint,
	response Response,
	state cursor,
	boundary string,
) (Response, error) {
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
	return nil
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
	incomplete := &Incomplete{
		Phase: phase, LastToolEvidence: evidence,
		StartedAt: started, AttemptCount: checkpoint.ProviderAttempts,
		RecoveryAdvice: advice, Checkpoint: checkpoint, Cause: cause,
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
		})
	}
	if loop.delivery != nil {
		loop.terminalOnce.Do(func() {
			loop.delivery.FinalizeIncomplete(ctx, checkpoint, incomplete)
		})
	}
	return incomplete
}

func requestMessages(
	systemPrompt string,
	durable []protocol.Message,
	repair string,
	activation string,
) []protocol.Message {
	result := make([]protocol.Message, 0, len(durable)+3)
	if prompt := strings.TrimSpace(systemPrompt); prompt != "" {
		result = append(result, protocol.Message{
			Role: protocol.RoleSystem, Content: prompt,
		})
	}
	result = append(result, cloneMessages(durable)...)
	if prompt := strings.TrimSpace(repair); prompt != "" {
		result = append(result, protocol.Message{
			Role: protocol.RoleSystem, Content: prompt,
		})
	}
	// Retrieved memory rides the SYSTEM role, never the user role. It is the
	// last message in the clone and is rebuilt every step, so in the user role
	// it would occupy the most recent user turn for the whole turn — recalled
	// text from an unrelated session then reads as the live request and the
	// model attributes it to the user.
	if tail := strings.TrimSpace(activation); tail != "" {
		result = append(result, protocol.Message{
			Role: protocol.RoleSystem, Content: tail,
		})
	}
	return result
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
	result := cloneMessages(messages)
	result = append(result, protocol.Message{
		Role: protocol.RoleSystem,
		Content: "Live supervisor guidance:\n" +
			strings.Join(accepted, "\n"),
	})
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
		if !exists ||
			json.Unmarshal(raw, &expectations[index]) != nil ||
			strings.TrimSpace(expectations[index]) == "" {
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
