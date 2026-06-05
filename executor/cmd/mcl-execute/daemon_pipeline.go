// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_pipeline.go — non-interactive walk pipeline for daemon mode.
//
// Mirrors walk_cmd.go's 12-phase pipeline but:
//
//   - Reuses one *infra (cortex + MCP + registry) across many messages
//     instead of building/tearing-down per message.
//   - Returns errors instead of os.Exit / fatalf so the daemon stays up
//     across LLM hiccups.
//   - Non-interactive: blocking-clarify questions surface to the HTTP
//     caller as a structured 422 response (clarifyRequiredError) rather
//     than reading stdin.
//   - Gate policy is forced to "approve" (Q1 lock policy is server-
//     mediated; manual stdin gating is not a daemon affordance).
//   - Per-message transcript writes to its own JSONL under
//     <transcripts>/<intent_id>.jsonl AND mirrors to the shared SSE
//     broker so live web clients see every event.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"matrix/executor/runtime"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/interpreter"
)

// Compile-time imports kept to satisfy unused-warnings; cortex + ir are
// used transitively via compile/synthesize signatures so we don't need
// to import them here.
var _ = runtime.NewWalker

// messageRequest captures the body of POST /messages.
type messageRequest struct {
	Prose      string            `json:"prose"`
	Verb       string            `json:"verb,omitempty"`        // optional override
	SkillURI   string            `json:"skill,omitempty"`       // matrix://skill/<slug>@<v>; defaults to daemon flag
	IntentID   string            `json:"intent_id,omitempty"`   // optional; default = synthIntentID
	SlotValues map[string]string `json:"slot_values,omitempty"` // pre-filled clarify answers
	Anchor     bool              `json:"anchor,omitempty"`      // request chain anchor (D10)
	// GoalIDField is the optional cortex Goal this message rolls up
	// under (sess#32 ambient-architect plan §5.11). Stamped on every
	// gateway-routed LLM call as X-Matrix-Goal-ID so the
	// matrix_daemon_cost_pax_total counter aggregates per goal.
	// Empty leaves the goal label unbound.
	GoalIDField string `json:"goal_id,omitempty"`
	// ConversationID threads the Liaison's chat.* turns across a /chat
	// conversation (front door). Stamped on the narrator's chat events so
	// the client can group them. Empty for legacy /messages callers.
	ConversationID string `json:"conversation_id,omitempty"`
	// UserName is the signed-in user's friendly display label (OAuth
	// profile name or email), forwarded by the client so the Liaison can
	// address them by name. Sanitized to a first name before use; empty
	// for legacy callers or when no human-friendly name is known.
	UserName string `json:"user_name,omitempty"`
}

// GoalID returns the request's goal identifier (empty when unset).
func (r messageRequest) GoalID() string { return r.GoalIDField }

// messageResult is the success body of /messages.
type messageResult struct {
	IntentID       string `json:"intent_id"`
	IntentHash     string `json:"intent_hash"`
	Status         string `json:"status"` // completed | failed
	LifecyclePath  string `json:"lifecycle"`
	PreReplayRoot  string `json:"pre_replay_root,omitempty"`
	PostReplayRoot string `json:"post_replay_root,omitempty"`
	WalkErrors     int    `json:"walk_errors"`
	NodeCount      int    `json:"node_count"`
	EventCount     int    `json:"event_count"`
	DurationMS     int64  `json:"duration_ms"`
	Error          string `json:"error,omitempty"`
	// Answer is the deterministic, ground-truth result of the run,
	// composed directly from the executed plan's node outputs (tool
	// results + executor step text) — NOT from a re-read of the SSE
	// event stream and NOT synthesized by the Liaison. This is the
	// authoritative deliverable: persisted with the job and pull-
	// retrievable, so the true outcome is always knowable regardless
	// of the live stream or the conversational-phrasing LLM.
	Answer string `json:"answer,omitempty"`
}

// collectPlanAnswer walks the executed plan tree in dispatch order and
// joins every node's ResultText into the deterministic answer body. The
// walker records the FULL tool output (up to 64K) and executor step text
// on each node's ResultText (runtime/walker.go), so this is the real,
// untruncated ground truth — the source the closing turn is composed
// from, and the raw fallback if conversational phrasing fails.
func collectPlanAnswer(plan *ir.PlanTree) string {
	if plan == nil {
		return ""
	}
	var parts []string
	walkPlanRec(&plan.Root, func(n *ir.PlanNode) {
		if n == nil {
			return
		}
		if txt := strings.TrimSpace(n.ResultText); txt != "" {
			parts = append(parts, txt)
		}
	})
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// clarifyRequiredError is returned when the compiler produced blocking
// unknowns that the client must resolve. Mapped to HTTP 422 by the
// server with a structured body containing the questions.
type clarifyRequiredError struct {
	IntentID  string               `json:"intent_id"`
	Questions []clarifyQuestionDTO `json:"questions"`
	Round     int                  `json:"round"`
	frame     string               // internal — for transcript only
	rawErr    error                // wrapped errClarifyRequired
}

type clarifyQuestionDTO struct {
	SlotName string `json:"slot_name"`
	TypeName string `json:"type_name"`
	Required bool   `json:"required"`
	Prompt   string `json:"prompt"`
}

func (c *clarifyRequiredError) Error() string {
	return fmt.Sprintf("clarify required: %d question(s) on intent %s",
		len(c.Questions), c.IntentID)
}

// runMessage executes a single message end-to-end through the existing
// 12-phase pipeline. Returns:
//
//   - (*messageResult, nil)               — success (status = completed | failed)
//   - (nil, *clarifyRequiredError)        — compile produced blocking unknowns
//   - (nil, error)                        — fatal pipeline error (LLM down, MCP died, …)
//
// On terminal completion (success OR walker failure) the function
// always emits a signed envelope (intent.attest or intent.fail), so the
// envelope chain is durable regardless of which terminal branch fires.
func runMessage(
	ctx context.Context,
	d *daemonState,
	req messageRequest,
) (result *messageResult, retErr error) {
	start := time.Now()
	if req.Prose == "" {
		return nil, fmt.Errorf("daemon: prose is required")
	}
	if req.SkillURI == "" {
		req.SkillURI = d.defaultSkillURI
	}
	if req.SkillURI == "" {
		return nil, fmt.Errorf("daemon: skill URI is required (no default configured)")
	}
	intentID := req.IntentID
	if intentID == "" {
		intentID = synthIntentID(req.Prose, req.Verb)
	}
	// Pin the resolved intent id back onto req so the deferred
	// emitFinalTurn (Leg C) stamps the closing chat turn with the same
	// id the client is subscribed on.
	req.IntentID = intentID

	// Per-message transcript file: one JSONL per intent under
	// <transcripts-dir>/<intent_id>.jsonl. The shared broker tap is
	// installed so live SSE clients see every event. The router
	// metrics accumulator (sess#31d P4) is also re-attached so per-
	// message decodes feed the daemon-wide histogram surfaced via
	// /metrics.
	tPath := filepath.Join(d.transcriptsDir, intentID+".jsonl")
	t, err := openTranscript(tPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: open transcript: %w", err)
	}
	defer t.Close()
	// Bind this transcript to the per-message intent_id so every
	// downstream Event() auto-stamps fields["intent_id"] when the call
	// site did not include it. Without this stamp the broker's
	// per-subscriber sseFilter (daemon_sse.go:54) drops the entire
	// walk/lifecycle/step/gate/envelope stream for every browser
	// listening on /events?intent_id=<id>, which is what produced the
	// "Connected — waiting for activity" empty-state on the client even
	// after the run had completed. Must be set BEFORE AttachBroker so
	// the very first Event ("message.start" below) is also stamped.
	t.SetIntentID(intentID)
	t.AttachBroker(d.broker)
	t.AttachMetrics(d.metrics)

	// Liaison narrator (side-channel): when enabled, narrate this run to
	// the human as chat.* turns. Subscribed BEFORE message.start so it
	// sees the whole stream; shutdown() is deferred AFTER t.Close() is
	// deferred, so (LIFO) the closing chat turn is written before the
	// transcript file closes.
	if d.liaisonEnabled() {
		narrator := d.startLiaisonNarrator(ctx, t, intentID, req.ConversationID, req.Prose, req.UserName)
		defer narrator.shutdown()
		// Registered AFTER shutdown so (LIFO) the guaranteed closing turn
		// is emitted BEFORE the narrator stops and BEFORE t.Close(). This
		// is the authoritative answer path (Leg C): the closing turn is
		// composed from the deterministic ground-truth result for EVERY
		// terminal branch (success, task-failure, clarify, or fatal
		// system error), so the user always gets a real, final answer.
		defer func() { d.emitFinalTurn(t, narrator, req, result, retErr) }()
	}

	t.Event("message.start", "boot", map[string]interface{}{
		"intent_id": intentID,
		"prose":     req.Prose,
		"verb":      req.Verb,
		"skill":     req.SkillURI,
		"actor":     d.actor.UserURI,
		"agent":     d.actor.AgentURI,
		"goal_id":   req.GoalID(),
	})

	// Sess#32 ambient-architect cost telemetry (plan §5.11). One
	// accumulator per intent: every gateway-routed LLM call folds its
	// X-Matrix-Cost-Pax into the running total; the lifecycle terminal
	// emits a single intent.cost.summary so /metrics + the Inbox UI
	// can render the per-intent spend without re-walking the
	// transcript. Empty totalPax (no gateway routing) makes the emit a
	// no-op so legacy CLI runs stay clean.
	costAcc := newIntentCostAccumulator(intentID, req.GoalID())

	// Phase 1 — load skill (cheap; OK to do per-message since the
	// SkillLoader is local FS only).
	loader := runtime.NewSkillLoader(d.skillsRoot)
	skill, err := loader.Load(req.SkillURI)
	if err != nil {
		return nil, fmt.Errorf("daemon: skill load: %w", err)
	}
	t.Event("skill.loaded", "boot", map[string]interface{}{
		"uri":   skill.URI,
		"hash":  skill.CanonicalHash,
		"verbs": skill.MclVerbs,
	})

	// Phase 2 — envelope stream + lifecycle driver (per-intent).
	stream, err := newEnvelopeStream(d.journalDir, intentID, d.actor, t)
	if err != nil {
		return nil, fmt.Errorf("daemon: envelope stream: %w", err)
	}
	drv, err := newLifecycleDriver(intentID, stream, t)
	if err != nil {
		return nil, fmt.Errorf("daemon: lifecycle: %w", err)
	}

	// Phase 3 — compile (NON-interactive — clarify surfaces as 422).
	gwCompile := d.llmConfigFor(llm.SlotCompiler.String(), "", intentID, req.GoalID(), t, costAcc)
	forgeMode := d.forgeFS != nil
	compRes, err := compile(ctx, d.infra.cortex, compileOpts{
		Skill:               skill,
		Prose:               req.Prose,
		Verb:                req.Verb,
		Actor:               d.actor.UserURI,
		Agent:               d.actor.AgentURI,
		IntentID:            intentID,
		Model:               d.compilerModel,
		EscalationModel:     d.compilerEscalateModel,
		ConfidenceThreshold: d.compileConfidenceThreshold,
		BaseURL:             d.llmBaseURL,
		Seed:                d.seed,
		SlotValues:          req.SlotValues,
		Interactive:         false,
		MaxClarify:          1, // single-pass; clients re-POST with slot_values
		GatewayURL:          gwCompile.GatewayURL,
		ActorDID:            gwCompile.ActorDID,
		GoalID:              gwCompile.GoalID,
		CostHook:            gwCompile.CostHook,
		ForgeMode:           forgeMode,
	}, t)
	if err != nil {
		// Surface clarify-required as a structured error.
		var cr *errClarifyRequired
		if errors.As(err, &cr) {
			cre := &clarifyRequiredError{
				IntentID:  intentID,
				Questions: dtoFromQuestions(cr.Questions),
				Round:     compRes.Rounds,
				frame:     compRes.FrameJSON,
				rawErr:    cr,
			}
			t.Event("compile.clarify.required", "compile", map[string]interface{}{
				"intent_id": intentID,
				"questions": len(cr.Questions),
				"prompts":   clarifyPrompts(cr.Questions),
			})
			return nil, cre
		}
		return nil, fmt.Errorf("daemon: compile: %w", err)
	}

	// Phase 4 — drafting → proposed.
	if _, err := drv.DriveCompiled(compRes.IntentJSON, compRes.LatencyMs); err != nil {
		return nil, fmt.Errorf("daemon: drive compiled: %w", err)
	}
	// Phase 5 — proposed → accepted.
	if _, err := drv.DriveAccept(compRes.IntentHash, req.Anchor); err != nil {
		return nil, fmt.Errorf("daemon: drive accept: %w", err)
	}

	// Phase 6 — synthesize plan.
	gwSynth := d.llmConfigFor(llm.SlotPlanner.String(), "", intentID, req.GoalID(), t, costAcc)
	synthRes, err := synthesize(ctx, synthesizeOpts{
		Skill:         skill,
		Intent:        compRes.Intent,
		Manifest:      d.infra.manifest,
		Registry:      d.infra.registry,
		Manager:       d.infra.manager,
		Agent:         d.actor.AgentURI,
		Model:         d.synthMod(),
		BaseURL:       d.llmBaseURL,
		Seed:          d.seed,
		MaxRetry:      d.maxRetry,
		WorkspaceRoot: d.workspaceRoot,
		GatewayURL:    gwSynth.GatewayURL,
		ActorDID:      gwSynth.ActorDID,
		IntentID:      intentID,
		GoalID:        gwSynth.GoalID,
		CostHook:      gwSynth.CostHook,
		ForgeMode:     forgeMode,
	}, t)
	if err != nil {
		// Synthesis fatal: drive lifecycle to failed for audit chain.
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, compRes.Intent, nil, nil,
			"correction_invalid", fmt.Sprintf("plan synthesis failed: %v", err), t)
		return makeFailedResult(intentID, compRes.IntentHash, drv, nil, nil, "synthesize failed", err.Error(), start), nil
	}
	t.Event("synth.done", "synth", map[string]interface{}{
		"plan_id":   synthRes.Plan.ID,
		"plan_hash": synthRes.PlanHash,
		"nodes":     countPlanNodes(&synthRes.Plan.Root),
		"rounds":    synthRes.Rounds,
		"ms":        synthRes.LatencyMs,
	})

	// Phase 7 — accepted → executing.
	if _, err := drv.DrivePlanProposed(synthRes.PlanJSON); err != nil {
		return nil, fmt.Errorf("daemon: drive plan: %w", err)
	}

	// Phase 7.5 (PUBLIC) — PaxeerSpendPolicy plan-time gate. Walks
	// every paxeer-net write tool call, parses the value-bearing arg,
	// and gates per-call against the per-call cap + aggregate cap
	// BEFORE any side effect. A malformed value-arg fails the intent
	// here (no dispatch); a per-call or aggregate gate forces a
	// mandatory human approval through the existing gateBroker. The
	// pre-pass performs no cortex writes, so it does not perturb the
	// replay byte-identity invariant.
	if blocked, gerr := d.enforcePaxeerSpend(ctx, intentID, synthRes.Plan, t); gerr != nil {
		return nil, fmt.Errorf("daemon: paxeer spend evaluation: %w", gerr)
	} else if blocked != nil {
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, compRes.Intent, synthRes.Plan, nil,
			"policy_denied", blocked.Reason, t)
		return makeFailedResult(intentID, compRes.IntentHash, drv, nil, synthRes.Plan, "policy_denied", blocked.Reason, start), nil
	}

	// Phase 8 — pre-walk cortex root.
	preRoot := captureRoot(d.infra.cortex)
	t.Event("walk.cortex.pre", "walk", map[string]interface{}{
		"overall_root": preRoot,
	})

	// Phase 9 — build walker (gate=httpGateHandler when broker wired,
	// no sub-dispatch by default). intentID passed through so the
	// HTTP gate handler can register pending gates under the right
	// scope for POST /intents/<id>/gates/<nid>/answer. goalID +
	// costAcc thread sess#32 ambient-architect cost telemetry into
	// every step decode the walker fires.
	walker, err := buildDaemonWalker(d, drv, stream, skill, t, intentID, req.GoalID(), costAcc)
	if err != nil {
		return nil, fmt.Errorf("daemon: build walker: %w", err)
	}

	// Phase 10 — run walk.
	walkRes, werr := walker.Run(ctx, synthRes.Plan)
	postRoot := captureRoot(d.infra.cortex)
	t.Event("walk.cortex.post", "walk", map[string]interface{}{
		"overall_root": postRoot,
		"changed":      preRoot != postRoot,
	})

	// Phase 11 — terminal envelope.
	if werr != nil {
		reason := classifyWalkError(werr)
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, compRes.Intent, synthRes.Plan,
			walkRes, reason, werr.Error(), t)
		return makeFailedResult(intentID, compRes.IntentHash, drv, walkRes, synthRes.Plan, reason, werr.Error(), start), nil
	}
	if hasIsErrors(walkRes) {
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, compRes.Intent, synthRes.Plan,
			walkRes, "tool_error", "tool reported in-band failure", t)
		return makeFailedResult(intentID, compRes.IntentHash, drv, walkRes, synthRes.Plan, "tool_error", "tool reported in-band failure", start), nil
	}
	// A step's LLM decode (or transport) errored — e.g. a budget_exhausted
	// 429. The walker records these in WalkResult.Errors and continues, so
	// without this check a plan whose content step failed would attest as
	// "completed" and the user would be falsely told it succeeded. Promote
	// it to a terminal failure so the true outcome is always surfaced; any
	// partial node output is still folded into the answer by makeFailedResult.
	if nodeID, errMsg, ok := firstWalkError(walkRes); ok {
		reason := classifyStepError(errMsg)
		detail := fmt.Sprintf("step %s: %s", nodeID, errMsg)
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, compRes.Intent, synthRes.Plan,
			walkRes, reason, detail, t)
		return makeFailedResult(intentID, compRes.IntentHash, drv, walkRes, synthRes.Plan, reason, detail, start), nil
	}
	if _, err := signTerminalAttest(ctx, drv, d.infra.cortex, compRes.Intent, synthRes.Plan, walkRes, t); err != nil {
		return nil, fmt.Errorf("daemon: drive attest: %w", err)
	}

	res := &messageResult{
		IntentID:       intentID,
		IntentHash:     compRes.IntentHash,
		Status:         "completed",
		LifecyclePath:  drv.Summary(),
		PreReplayRoot:  preRoot,
		PostReplayRoot: postRoot,
		WalkErrors:     0,
		NodeCount:      nodesIn(walkRes),
		EventCount:     eventsIn(walkRes),
		DurationMS:     time.Since(start).Milliseconds(),
		// Deterministic ground-truth deliverable (Leg B): the real
		// executed-plan output, captured from node ResultText, not the
		// truncated SSE previews and not the Liaison's prose.
		Answer: collectPlanAnswer(synthRes.Plan),
	}
	t.Event("message.complete", "summary", map[string]interface{}{
		"intent_id":   intentID,
		"status":      res.Status,
		"lifecycle":   res.LifecyclePath,
		"node_count":  res.NodeCount,
		"duration_ms": res.DurationMS,
	})
	// Per-intent cost summary (sess#32 ambient-architect plan §5.11).
	// Emits an intent.cost.summary transcript event with the
	// cumulative PAX spend for this intent. No-op when no cost
	// headers were ever observed (legacy direct-provider posture).
	costAcc.EmitTerminal(t)
	// Per-message router histogram flush (sess#31d P4). Snapshots
	// the daemon-wide accumulator so anyone tailing this intent's
	// transcript can see per-route latency p50/p99 + cache hit
	// rate without scraping /metrics. Counters are NOT reset
	// (Prometheus convention).
	if m := t.Metrics(); m != nil {
		m.Flush(t)
	}
	return res, nil
}

// buildDaemonWalker mirrors walk_cmd.go:buildWalker but selects the
// gate handler dynamically: when d.gateBroker is wired (sess#27+),
// gates block on POST /intents/<id>/gates/<nid>/answer; otherwise
// the legacy stdin-policy=approve handler auto-approves so sync-only
// CLI flows from sess#26 still pass.
//
// Sub-dispatch is enabled iff daemonState.allowSubDispatch is true
// (Q6 v1 carve-out: same agent, in-process only; cross-agent +
// CortexScope Merkle proof handoff is v1.1).
//
// goalID + costAcc are sess#32 ambient-architect plumbing (plan §5.11):
// goalID stamps every gateway-routed step decode as X-Matrix-Goal-ID;
// costAcc receives the per-call X-Matrix-Cost-Pax for the intent.
func buildDaemonWalker(d *daemonState, drv *lifecycleDriver, stream *envelopeStream, skill *runtime.LoadedSkill, t *transcript, intentID, goalID string, costAcc *intentCostAccumulator) (*runtime.Walker, error) {
	gwStep := d.llmConfigFor(llm.SlotExecutor.String(), "", intentID, goalID, t, costAcc)
	// Gideon is the Forge spinoff and shares the opencode.ai/zen brain:
	// bind ForgeRegistry (Claude/GPT via OPENCODE_API_KEY) in gideon mode
	// too, not just when the Forge FS surface is mounted.
	forgeMode := d.forgeFS != nil || d.gideonMode
	stepH, err := newLLMStepHandlerOpts(stepHandlerOpts{
		Model:      d.executorModel,
		BaseURL:    d.llmBaseURL,
		Seed:       d.seed,
		SkillURI:   skill.URI,
		SkillMD:    skill.MdBytes,
		GatewayURL: gwStep.GatewayURL,
		ActorDID:   gwStep.ActorDID,
		IntentID:   intentID,
		GoalID:     goalID,
		CostHook:   gwStep.CostHook,
		ForgeMode:  forgeMode,
	}, t)
	if err != nil {
		return nil, fmt.Errorf("daemon: step handler: %w", err)
	}
	var gateH runtime.GateHandler
	if d.gateBroker != nil && intentID != "" {
		gateH = newHTTPGateHandler(d.gateBroker, intentID, d.actor.UserURI, t, d.gateTimeout)
	} else {
		gateH = newStdinGateHandler(string(gatePolicyApprove), d.actor.UserURI, t)
	}

	params := runtime.WalkerParams{
		Registry: d.infra.registry,
		Cortex:   d.infra.cortex,
		Envelope: stream,
		Events:   t,
		Step:     stepH,
		Gate:     gateH,
		ActorURI: d.actor.UserURI,
	}

	if d.allowSubDispatch {
		// In-process sub-dispatch: synthesize + walk the resolved
		// sub-skill in the SAME process. Reuse the parent's
		// envelopeStream + lifecycleDriver so sub-walk envelopes ride
		// the parent's intent chain (sub-intent fan-out is v1.1).
		// makeWalker recurses with allowSubDispatch already on, so any
		// depth of nested sub-dispatch resolves; cycle/depth bounds are
		// enforced by runtime.Walker (see runtime/walker.go).
		loader := runtime.NewSkillLoader(d.skillsRoot)
		gwSub := d.llmConfigFor(llm.SlotPlanner.String(), "", intentID, goalID, t, costAcc)
		sub := &inProcessSubDispatch{
			loader: loader,
			t:      t,
			synthesizer: func(ctx context.Context, sk *runtime.LoadedSkill, parent *ir.PlanTree, node *ir.PlanNode) (*ir.PlanTree, error) {
				// sess#37c — Build the synthetic sub-Intent from the
				// parent walk context + sub-skill metadata. synthesize()
				// nil-checks Intent and bubbles "missing required input"
				// without it; the pre-sess#37c closure passed Intent: nil
				// and short-circuited every sub_dispatch attempt.
				subIntent := buildSubIntent(sk, parent, node, d.actor)
				subRes, serr := synthesize(ctx, synthesizeOpts{
					Skill:         sk,
					Intent:        subIntent,
					Manifest:      d.infra.manifest,
					Registry:      d.infra.registry,
					Manager:       d.infra.manager,
					Agent:         d.actor.AgentURI,
					Model:         d.synthMod(),
					BaseURL:       d.llmBaseURL,
					Seed:          d.seed,
					MaxRetry:      1,
					WorkspaceRoot: d.workspaceRoot,
					GatewayURL:    gwSub.GatewayURL,
					ActorDID:      gwSub.ActorDID,
					IntentID:      subIntent.ID,
					GoalID:        gwSub.GoalID,
					CostHook:      gwSub.CostHook,
					ForgeMode:     forgeMode,
				}, t)
				if serr != nil {
					return nil, serr
				}
				return subRes.Plan, nil
			},
			makeWalker: func(sk *runtime.LoadedSkill) (*runtime.Walker, error) {
				return buildDaemonWalker(d, drv, stream, sk, t, intentID, goalID, costAcc)
			},
		}
		params.Sub = sub
	}

	return runtime.NewWalker(params)
}

// makeFailedResult assembles a messageResult for non-fatal terminal
// failures (synth fail, walk error, in-band tool error). Fatal errors
// (broken cortex, signal abort) bubble up to the HTTP layer instead.
//
// plan is the executed plan (nil when failure occurred before/at
// synthesis); any partial node output it captured is folded into the
// deterministic Answer alongside the failure reason, so even a failed
// run carries real, ground-truth context rather than a bare code.
func makeFailedResult(intentID, intentHash string, drv *lifecycleDriver, walk *runtime.WalkResult, plan *ir.PlanTree, reason, msg string, start time.Time) *messageResult {
	// User-facing answer carries the CLEAN, jargon-free failure line; the
	// raw reason+msg is preserved on Error for diagnosis / the durable job.
	failLine := friendlyFailLine(reason)
	answer := failLine
	if partial := collectPlanAnswer(plan); partial != "" {
		answer = partial + "\n\n" + failLine
	}
	return &messageResult{
		IntentID:      intentID,
		IntentHash:    intentHash,
		Status:        "failed",
		LifecyclePath: drv.Summary(),
		WalkErrors:    countWalkErrors(walk),
		NodeCount:     nodesIn(walk),
		EventCount:    eventsIn(walk),
		DurationMS:    time.Since(start).Milliseconds(),
		Error:         fmt.Sprintf("%s: %s", reason, msg),
		Answer:        answer,
	}
}

// friendlyFailLine maps a terminal reason code to a plain, jargon-free
// sentence for the user. The raw error (with the underlying provider
// message) stays on messageResult.Error for operators; the user never
// sees a 429 body or an internal reason token.
func friendlyFailLine(reason string) string {
	switch reason {
	case "budget_exhausted":
		return "I couldn't finish this because the usage budget for this account has been reached. Once it resets (or the limit is raised) I can pick this right back up."
	case "timeout":
		return "I couldn't finish this in time — a step timed out before it produced a result. Please try again in a moment."
	case "tool_error":
		return "I couldn't finish this because one of the tools I used reported an error. Please try again, or rephrase the request."
	default:
		return "I couldn't complete this task. Please try again, or rephrase what you'd like."
	}
}

func countWalkErrors(wr *runtime.WalkResult) int {
	if wr == nil {
		return 0
	}
	n := 0
	for _, b := range wr.IsErrors {
		if b {
			n++
		}
	}
	if n == 0 && len(wr.Errors) > 0 {
		n = len(wr.Errors)
	}
	return n
}

// dtoFromQuestions converts interpreter.ClarifyQuestion -> wire DTO.
func dtoFromQuestions(qs []*interpreter.ClarifyQuestion) []clarifyQuestionDTO {
	out := make([]clarifyQuestionDTO, 0, len(qs))
	for _, q := range qs {
		out = append(out, clarifyQuestionDTO{
			SlotName: q.SlotName,
			TypeName: q.TypeName,
			Required: q.Required,
			Prompt:   q.Prompt,
		})
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
