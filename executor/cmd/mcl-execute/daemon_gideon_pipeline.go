// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_gideon_pipeline.go — compiler-bypass pipeline for Gideon
// (Gideon Phase 1, plan todo: engine).
//
// runMessageDirect is a sibling to runMessage (daemon_pipeline.go) that
// DROPS the compile phase + clarify. Gideon's operator (Andrew) speaks
// straight typed intent, so there is no NL→IR compiler LLM, no slot
// filling, and no 422 clarify round-trip. The signed Intent envelope is
// built directly from {verb, prose, scope}, then the pipeline proceeds
// exactly as runMessage does: synthesize → DrivePlanProposed → walk →
// terminal attest/fail.
//
// Everything downstream of the intent build (lifecycle driver, envelope
// stream, walker, cortex root capture, cost accumulator, transcript) is
// reused verbatim so the audit/replay spine is identical and the replay
// byte-identity invariant (cortex §13.4) is preserved: the directly-
// built Intent is a pure deterministic function of {verb, prose, scope,
// actor, snapshot} with no LLM nondeterminism.
//
// The two Gideon hard guardrails (gideon_ops_policy.go) are enforced
// here as a plan pre-pass that runs after synthesis and before the
// walk: every NodeToolCall is evaluated; validator hard-denies fail the
// intent before any side effect, and chain-state-loss risks force a
// mandatory human gate through the existing httpGateHandler/gateBroker.

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"matrix/executor/runtime"
	"matrix/executor/tool"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
)

// dispatchMessage routes a message to the correct pipeline. In Gideon
// mode the compiler-bypass runMessageDirect is used; otherwise the
// legacy compile→walk runMessage path is preserved unchanged. Called by
// the sync (/messages) and async (/messages/async) HTTP handlers so the
// routing decision lives in one place.
func (d *daemonState) dispatchMessage(ctx context.Context, req messageRequest) (*messageResult, error) {
	if d.gideonMode {
		return runMessageDirect(ctx, d, req)
	}
	return runMessage(ctx, d, req)
}

// runMessageDirect executes one message through the compiler-bypass
// pipeline. Return contract mirrors runMessage:
//
//   - (*messageResult, nil) — success (status = completed | failed)
//   - (nil, error)          — fatal pipeline error (LLM down, MCP died, …)
//
// It never returns *clarifyRequiredError: there is no compile/clarify
// phase. On terminal completion (success OR walker/guardrail failure)
// the function always emits a signed terminal envelope.
func runMessageDirect(ctx context.Context, d *daemonState, req messageRequest) (*messageResult, error) {
	start := time.Now()
	if req.Prose == "" {
		return nil, fmt.Errorf("gideon: prose is required")
	}
	if req.SkillURI == "" {
		req.SkillURI = d.defaultSkillURI
	}
	if req.SkillURI == "" {
		return nil, fmt.Errorf("gideon: skill URI is required (no default configured)")
	}
	intentID := req.IntentID
	if intentID == "" {
		intentID = synthIntentID(req.Prose, req.Verb)
	}

	// Per-message transcript + shared broker tap + metrics — identical
	// to runMessage so live SSE clients + /metrics see Gideon runs the
	// same way as compiled runs.
	tPath := filepath.Join(d.transcriptsDir, intentID+".jsonl")
	t, err := openTranscript(tPath)
	if err != nil {
		return nil, fmt.Errorf("gideon: open transcript: %w", err)
	}
	defer t.Close()
	t.AttachBroker(d.broker)
	t.AttachMetrics(d.metrics)

	t.Event("message.start", "boot", map[string]interface{}{
		"intent_id": intentID,
		"prose":     req.Prose,
		"verb":      req.Verb,
		"skill":     req.SkillURI,
		"actor":     d.actor.UserURI,
		"agent":     d.actor.AgentURI,
		"goal_id":   req.GoalID(),
		"mode":      "gideon_direct",
	})

	costAcc := newIntentCostAccumulator(intentID, req.GoalID())

	// Phase 1 — load skill (local FS only).
	loader := runtime.NewSkillLoader(d.skillsRoot)
	skill, err := loader.Load(req.SkillURI)
	if err != nil {
		return nil, fmt.Errorf("gideon: skill load: %w", err)
	}
	t.Event("skill.loaded", "boot", map[string]interface{}{
		"uri":   skill.URI,
		"hash":  skill.CanonicalHash,
		"verbs": skill.MclVerbs,
	})

	// Phase 2 — envelope stream + lifecycle driver (per-intent).
	stream, err := newEnvelopeStream(d.journalDir, intentID, d.actor, t)
	if err != nil {
		return nil, fmt.Errorf("gideon: envelope stream: %w", err)
	}
	drv, err := newLifecycleDriver(intentID, stream, t)
	if err != nil {
		return nil, fmt.Errorf("gideon: lifecycle: %w", err)
	}

	// Phase 3 (REPLACED) — build the signed Intent envelope directly
	// from {verb, prose, scope}. No compiler LLM, no slot-filling, no
	// clarify. snapHash anchors the intent to the cortex root for D11
	// determinism exactly as the compiler would.
	snapHash, err := computeCortexSnapHash(d.infra.cortex)
	if err != nil {
		return nil, fmt.Errorf("gideon: cortex snapshot hash: %w", err)
	}
	intent, intentJSON, intentHash, err := buildDirectIntent(d, req, intentID, snapHash)
	if err != nil {
		return nil, fmt.Errorf("gideon: build intent: %w", err)
	}
	t.Event("intent.direct.built", "compile", map[string]interface{}{
		"intent_id":   intent.ID,
		"intent_hash": intentHash,
		"verb":        intent.Frame.Verb,
		"objects":     len(intent.Frame.Objects),
	})

	// Phase 4 — drafting → proposed (intent.compiled carries the
	// directly-built IR; latency 0 — no compiler ran).
	if _, err := drv.DriveCompiled(intentJSON, 0); err != nil {
		return nil, fmt.Errorf("gideon: drive compiled: %w", err)
	}
	// Phase 5 — proposed → accepted.
	if _, err := drv.DriveAccept(intentHash, req.Anchor); err != nil {
		return nil, fmt.Errorf("gideon: drive accept: %w", err)
	}

	// Phase 6 — synthesize plan (planner LLM, unchanged from runMessage).
	gwSynth := d.llmConfigFor(llm.SlotPlanner.String(), "", intentID, req.GoalID(), t, costAcc)
	// Gideon shares the Forge opencode.ai/zen brain: synthesize the plan
	// with ForgeRegistry (claude-opus planner via OPENCODE_API_KEY) so the
	// compiler-bypass path does not require a Fireworks key.
	forgeMode := d.forgeFS != nil || d.gideonMode
	synthRes, err := synthesize(ctx, synthesizeOpts{
		Skill:         skill,
		Intent:        intent,
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
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, intent, nil, nil,
			"correction_invalid", fmt.Sprintf("plan synthesis failed: %v", err), t)
		return makeFailedResult(intentID, intentHash, drv, nil, nil, "synthesize failed", err.Error(), start), nil
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
		return nil, fmt.Errorf("gideon: drive plan: %w", err)
	}

	// Phase 7.5 (GIDEON) — guardrail pre-pass. Evaluate every tool call
	// against GideonOpsPolicy BEFORE any dispatch. A validator hard-deny
	// fails the intent here (no side effect); a chain-state-loss risk
	// forces a mandatory human gate through the existing httpGateHandler.
	// The pre-pass performs no cortex writes, so it does not perturb the
	// replay byte-identity invariant.
	if blocked, gerr := d.enforceGideonGuardrails(ctx, intentID, req.Prose, synthRes.Plan, t); gerr != nil {
		return nil, fmt.Errorf("gideon: guardrail evaluation: %w", gerr)
	} else if blocked != nil {
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, intent, synthRes.Plan, nil,
			"policy_denied", blocked.Reason, t)
		return makeFailedResult(intentID, intentHash, drv, nil, synthRes.Plan, "policy_denied", blocked.Reason, start), nil
	}

	// Phase 8 — pre-walk cortex root.
	preRoot := captureRoot(d.infra.cortex)
	t.Event("walk.cortex.pre", "walk", map[string]interface{}{
		"overall_root": preRoot,
	})

	// Phase 9 — build walker (reused verbatim).
	walker, err := buildDaemonWalker(d, drv, stream, skill, t, intentID, req.GoalID(), costAcc)
	if err != nil {
		return nil, fmt.Errorf("gideon: build walker: %w", err)
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
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, intent, synthRes.Plan,
			walkRes, reason, werr.Error(), t)
		return makeFailedResult(intentID, intentHash, drv, walkRes, synthRes.Plan, reason, werr.Error(), start), nil
	}
	if hasIsErrors(walkRes) {
		_, _ = signTerminalFail(ctx, drv, d.infra.cortex, intent, synthRes.Plan,
			walkRes, "tool_error", "tool reported in-band failure", t)
		return makeFailedResult(intentID, intentHash, drv, walkRes, synthRes.Plan, "tool_error", "tool reported in-band failure", start), nil
	}
	if _, err := signTerminalAttest(ctx, drv, d.infra.cortex, intent, synthRes.Plan, walkRes, t); err != nil {
		return nil, fmt.Errorf("gideon: drive attest: %w", err)
	}

	res := &messageResult{
		IntentID:       intentID,
		IntentHash:     intentHash,
		Status:         "completed",
		LifecyclePath:  drv.Summary(),
		PreReplayRoot:  preRoot,
		PostReplayRoot: postRoot,
		WalkErrors:     0,
		NodeCount:      nodesIn(walkRes),
		EventCount:     eventsIn(walkRes),
		DurationMS:     time.Since(start).Milliseconds(),
		Answer:         collectPlanAnswer(synthRes.Plan),
	}
	t.Event("message.complete", "summary", map[string]interface{}{
		"intent_id":   intentID,
		"status":      res.Status,
		"lifecycle":   res.LifecyclePath,
		"node_count":  res.NodeCount,
		"duration_ms": res.DurationMS,
		"mode":        "gideon_direct",
	})
	costAcc.EmitTerminal(t)
	if m := t.Metrics(); m != nil {
		m.Flush(t)
	}
	return res, nil
}

// buildDirectIntent assembles a signed, content-addressed *ir.Intent
// directly from the request, bypassing the compiler. Scope (the typed
// objects the intent operates on) is sourced from req.SlotValues — each
// key/value becomes a Frame object so an operator (or the scheduler)
// can pin hosts/services without a compile round-trip.
//
// Verb resolution: req.Verb when a valid D7 verb; otherwise the sensible
// ops default "monitor". CompileMetadata is fully populated (with a
// deterministic seed) so the intent is replay-verifiable exactly like a
// compiled one — the only difference is there was no compiler LLM call.
func buildDirectIntent(d *daemonState, req messageRequest, intentID, snapHash string) (*ir.Intent, []byte, string, error) {
	verb := req.Verb
	if !ir.ValidVerb(verb) {
		verb = ir.VerbMonitor
	}

	objects := make([]ir.SlotEntry, 0, len(req.SlotValues))
	for name, val := range req.SlotValues {
		se := ir.SlotEntry{Name: name, Value: val}
		if len(val) >= len("matrix://") && val[:len("matrix://")] == "matrix://" {
			se.URI = val
		}
		objects = append(objects, se)
	}

	model := d.compilerModel
	if model == "" {
		model = "gideon-direct-passthrough"
	}

	intent := &ir.Intent{
		ID:         intentID,
		Version:    "mcl/0.1",
		Actor:      d.actor.UserURI,
		Agent:      d.actor.AgentURI,
		Prose:      req.Prose,
		Frame:      ir.Frame{Verb: verb, Objects: objects},
		State:      ir.StateProposed,
		Confidence: 1.0,
		CreatedAt:  nowRFC3339(),
		GoalID:     req.GoalID(),
		SignedBy:   d.actor.UserURI,
		CompileMetadata: &ir.CompileMetadata{
			Seed:               compilerSeed(intentID, d.actor.UserURI, snapHash, skillDigestOrEmpty(d, req), model),
			MtxDigest:          skillDigestOrEmpty(d, req),
			ModelDigest:        sha256Hex(model),
			ModelVersion:       model,
			Temperature:        0,
			Grammar:            "intent_direct@1",
			CortexSnapshotHash: snapHash,
		},
	}

	canon, err := ir.CanonicalJSON(intent)
	if err != nil {
		return nil, nil, "", fmt.Errorf("canonical json: %w", err)
	}
	hash, err := ir.Hash(intent)
	if err != nil {
		return nil, nil, "", fmt.Errorf("hash intent: %w", err)
	}
	intent.Hash = hash
	canon, err = ir.CanonicalJSON(intent)
	if err != nil {
		return nil, nil, "", fmt.Errorf("canonical json (final): %w", err)
	}
	return intent, canon, hash, nil
}

// skillDigestOrEmpty resolves the request skill's canonical hash for the
// CompileMetadata MtxDigest field. Returns "" on load failure (the skill
// is re-loaded in runMessageDirect, so a transient miss here is benign —
// it only affects the audit digest, never execution).
func skillDigestOrEmpty(d *daemonState, req messageRequest) string {
	uri := req.SkillURI
	if uri == "" {
		uri = d.defaultSkillURI
	}
	if uri == "" {
		return ""
	}
	loader := runtime.NewSkillLoader(d.skillsRoot)
	sk, err := loader.Load(uri)
	if err != nil || sk == nil {
		return ""
	}
	return sk.CanonicalHash
}

// enforceGideonGuardrails walks the synthesized plan, evaluates every
// NodeToolCall against d.gideonPolicy, and:
//
//   - OpsAllow → permits the call (the common, autonomous path).
//   - OpsDeny  → returns the blocking evaluation so the caller fails the
//     intent before any side effect (validator hard-deny).
//   - OpsGate  → opens a mandatory human gate via the existing
//     httpGateHandler/gateBroker and BLOCKS until answered. A non-
//     approval (deny, timeout, or absent broker) is treated as a block.
//
// Returns (blocked != nil) for the first denial/denied-gate encountered;
// (nil, nil) when every tool call is cleared. No cortex writes occur, so
// the replay byte-identity invariant is untouched.
func (d *daemonState) enforceGideonGuardrails(ctx context.Context, intentID, prose string, plan *ir.PlanTree, t *transcript) (*OpsEvaluation, error) {
	if d.gideonPolicy == nil || plan == nil {
		return nil, nil
	}
	var calls []*ir.PlanNode
	collectToolCalls(&plan.Root, &calls)

	for _, node := range calls {
		tc := node.ToolCall
		toolName := toolNameFromRef(tc.ToolRef)
		host := tc.Args["host"]
		command := tc.Args["command"]
		service := tc.Args["service"]

		ev := d.gideonPolicy.Evaluate(toolName, host, command, service, prose)
		t.Event("gideon.policy.eval", "walk", map[string]interface{}{
			"intent_id": intentID,
			"node_id":   node.ID,
			"tool":      toolName,
			"host":      host,
			"decision":  ev.Decision.String(),
			"rule":      ev.Rule,
			"pattern":   ev.Pattern,
		})

		switch ev.Decision {
		case OpsAllow:
			continue
		case OpsDeny:
			t.Event("gideon.policy.deny", "walk", map[string]interface{}{
				"intent_id": intentID,
				"node_id":   node.ID,
				"rule":      ev.Rule,
				"reason":    ev.Reason,
			})
			return &ev, nil
		case OpsGate:
			approved, gerr := d.runGideonGate(ctx, intentID, node.ID, ev, t)
			if gerr != nil {
				return nil, gerr
			}
			if !approved {
				// A denied / timed-out mandatory gate blocks the intent.
				blocked := ev
				blocked.Reason = "chain-state-loss gate not approved: " + ev.Reason
				return &blocked, nil
			}
			// Approved → fall through; this risky call is now cleared.
		}
	}
	return nil, nil
}

// runGideonGate opens a forced approval gate for a chain-state-loss risk
// through the same gateBroker/httpGateHandler used for synthesized gate
// nodes, so a channel (Telegram) or the panel can answer it via POST
// /intents/<id>/gates/<nid>/answer. Returns whether it was approved.
//
// When no gateBroker is wired the gate cannot be answered, so it
// fail-closed denies — a chain-state-loss action is NEVER auto-approved.
func (d *daemonState) runGideonGate(ctx context.Context, intentID, nodeID string, ev OpsEvaluation, t *transcript) (bool, error) {
	if d.gateBroker == nil {
		t.Event("gideon.gate.unavailable", "walk", map[string]interface{}{
			"intent_id": intentID,
			"node_id":   nodeID,
			"reason":    "no gate broker; chain-state-loss action fail-closed denied",
		})
		return false, nil
	}
	gateNodeID := nodeID + "-gideon-csl-gate"
	gateNode := &ir.PlanNode{
		ID:   gateNodeID,
		Kind: ir.NodeGate,
		Gate: &ir.GatePayload{
			RuleRef: "matrix://rule/gideon/chain-state-loss",
			Question: fmt.Sprintf(
				"CHAIN-STATE-LOSS RISK — host=%q tool=%q command=%q (pattern %q). "+
					"This can destroy chain state and is NOT reversible. Approve? Requires explicit YES.",
				ev.Host, ev.Tool, ev.Command, ev.Pattern),
			Options: []string{"yes", "no"},
		},
	}
	handler := newHTTPGateHandler(d.gateBroker, intentID, d.actor.UserURI, t, d.gateTimeout)
	decision, err := handler.HandleGate(ctx, gateNode)
	if err != nil {
		return false, err
	}
	t.Event("gideon.gate.decided", "walk", map[string]interface{}{
		"intent_id": intentID,
		"node_id":   gateNodeID,
		"approved":  decision.Approved,
		"answer":    decision.Answer,
	})
	return decision.Approved, nil
}

// collectToolCalls appends every NodeToolCall (depth-first) into out.
func collectToolCalls(n *ir.PlanNode, out *[]*ir.PlanNode) {
	if n == nil {
		return
	}
	if n.Kind == ir.NodeToolCall && n.ToolCall != nil {
		*out = append(*out, n)
	}
	for i := range n.Children {
		collectToolCalls(&n.Children[i], out)
	}
}

// toolNameFromRef extracts the bare tool name (e.g. "ssh_exec") from a
// version-pinned tool URI. Falls back to the raw ref on parse failure so
// the policy still receives a stable, matchable token.
func toolNameFromRef(toolRef string) string {
	parsed, err := tool.ParseToolURI(toolRef)
	if err != nil {
		return toolRef
	}
	return parsed.Name
}

// compile-time assertion that runtime is referenced (kept parallel to
// daemon_pipeline.go's usage pattern).
var _ = runtime.NewWalker

// Copyright © 2026 Paxlabs Inc. All rights reserved.
