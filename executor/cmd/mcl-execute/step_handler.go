// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// step_handler.go houses the three handler implementations the production
// walker (executor/runtime/walker.go) plugs in via WalkerParams:
//
//   - llmStepHandler        implements runtime.StepHandler
//   - inProcessSubDispatch  implements runtime.SubDispatchHandler
//   - stdinGateHandler      implements runtime.GateHandler
//
// Interface shapes are pinned to walker.go:67-114 verbatim:
//
//   StepHandler.HandleStep        (ctx, plan, node) (*StepResult, error)
//   StepResult                    {Outputs, Text, LatencyMs}
//   SubDispatchHandler.HandleSubDispatch(ctx, parent, node) (*SubDispatchResult, error)
//   SubDispatchResult             {SubIntentID, Outcome, CitedURIs}
//   GateHandler.HandleGate        (ctx, node) (*GateDecision, error)
//   GateDecision                  {Approved, Answer}

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/executor/runtime"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/interpreter"
)

// ---------------------------------------------------------------------
// llmStepHandler — runtime.StepHandler
// ---------------------------------------------------------------------

// llmStepHandler implements runtime.StepHandler by calling the executor
// LLM with a step-specific prompt. SKILL.md body is folded into the
// system message so the executor model sees the per-skill rubric on
// every step dispatch (matrix.kvx invariant 2026-05-24 sess#22c line 828:
// "SKILL.md = prose body (executor-LLM consumer)").
//
// Session 31b (model router · P2) upgrade: the handler now resolves a
// per-step llm.Client from llm.ModelRegistry using node.Step.Kind as
// the routing dimension. Clients are lazy-cached per llm.RouteKey so
// the brainstorming hot path (8 sequential reason-kind decodes) only
// builds one client. Override fields preserve the prior CLI semantics:
// --model on mcl-execute pins every step to that model, --base-url
// targets a single proxy endpoint, --seed pins the seed across kinds.
type llmStepHandler struct {
	registry        *llm.ModelRegistry
	overrideModel   string
	overrideBaseURL string
	overrideSeed    int64

	// --- sess#32 ambient-architect MatrixGateway routing (plan §5.16) ---
	// Empty gatewayURL preserves the legacy direct-provider posture.
	gatewayURL string
	actorDID   string
	intentID   string
	goalID     string
	costHook   func(http.Header)

	clientsMu sync.Mutex
	clients   map[llm.RouteKey]interpreter.LLM

	skillURI string
	skillMD  []byte
	t        *transcript
}

// stepHandlerOpts bundles the gateway-aware fields (sess#32) so
// callers don't need to keep the constructor signature growing. All
// fields are optional; zero-value preserves legacy posture.
type stepHandlerOpts struct {
	Model      string
	BaseURL    string
	Seed       int64
	SkillURI   string
	SkillMD    []byte
	GatewayURL string
	ActorDID   string
	IntentID   string
	GoalID     string
	CostHook   func(http.Header)

	// ForgeMode (sess#36 / Forge Phase 3) — when true, the executor
	// step handler binds llm.ForgeRegistry instead of DefaultRegistry
	// so every step decode runs through opencode.ai/zen (Claude Opus
	// 4.7 + GPT 5.5) with the Matrix identity preamble injected.
	ForgeMode bool
}

func newLLMStepHandler(model, baseURL string, seed int64, skillURI string, skillMD []byte, t *transcript) (*llmStepHandler, error) {
	return newLLMStepHandlerOpts(stepHandlerOpts{
		Model:    model,
		BaseURL:  baseURL,
		Seed:     seed,
		SkillURI: skillURI,
		SkillMD:  skillMD,
	}, t)
}

// newLLMStepHandlerOpts is the gateway-aware constructor used by the
// daemon (sess#32). The legacy newLLMStepHandler delegates to this
// with empty gateway fields so existing call sites + tests keep
// working without churn.
func newLLMStepHandlerOpts(opts stepHandlerOpts, t *transcript) (*llmStepHandler, error) {
	// Eagerly resolve the reason-kind client so config errors (bad endpoint,
	// missing API key) surface at handler construction time rather than
	// at first step decode. The other kind clients are built lazily.
	registry := llm.DefaultRegistry()
	if opts.ForgeMode {
		registry = llm.ForgeRegistry()
	}
	h := &llmStepHandler{
		registry:        registry,
		overrideModel:   opts.Model,
		overrideBaseURL: opts.BaseURL,
		overrideSeed:    opts.Seed,
		gatewayURL:      opts.GatewayURL,
		actorDID:        opts.ActorDID,
		intentID:        opts.IntentID,
		goalID:          opts.GoalID,
		costHook:        opts.CostHook,
		clients:         make(map[llm.RouteKey]interpreter.LLM),
		skillURI:        opts.SkillURI,
		skillMD:         opts.SkillMD,
		t:               t,
	}
	if _, _, err := h.clientFor(llm.RouteKey{Slot: llm.SlotExecutor, Kind: llm.KindReason}); err != nil {
		return nil, fmt.Errorf("step_handler: llm.New (reason kind): %w", err)
	}
	return h, nil
}

// cfgFor returns the effective Config for a route key after applying
// the CLI overrides. Pure (no I/O); separated from clientFor for tests.
func (h *llmStepHandler) cfgFor(key llm.RouteKey) llm.Config {
	cfg := h.registry.Resolve(key)
	// Step prompts are free-form prose; do NOT constrain with grammar.
	// (KindClassify carries grammar in the registry config but the
	// in-skill prompt step uses raw text; classify kinds that need
	// grammar go through a different IR path.)
	cfg.GrammarMode = llm.GrammarNone
	if h.overrideModel != "" {
		cfg.Model = h.overrideModel
	}
	if h.overrideBaseURL != "" {
		cfg.Endpoint = strings.TrimRight(h.overrideBaseURL, "/") + "/v1/chat/completions"
	}
	if h.overrideSeed != 0 {
		cfg.Seed = h.overrideSeed
	}
	if h.gatewayURL != "" {
		// Sess#32 ambient-architect MatrixGateway routing (plan §5.16).
		// Executor slot — kind label rides X-Matrix-Kind-Route so the
		// gateway audit trail can split reason / code / classify
		// per-call. cfg.Model stays the registry-resolved model so
		// gateway-side whitelist enforcement sees the actual decision.
		cfg.GatewayURL = h.gatewayURL
		cfg.ActorDID = h.actorDID
		cfg.IntentID = h.intentID
		cfg.GoalID = h.goalID
		cfg.SlotLabel = llm.SlotExecutor.String()
		cfg.KindRoute = key.Kind.String()
		cfg.OnResponseHeaders = h.costHook
	}
	return cfg
}

// clientFor returns a lazy-cached interpreter.LLM client for the route
// key, building it on first use. The cache key normalizes via the same
// rules as llm.ModelRegistry.Resolve (Kind ignored when Slot is not
// executor; KindUnspecified collapses to KindReason) so semantically-
// equivalent calls hit the same client.
func (h *llmStepHandler) clientFor(key llm.RouteKey) (interpreter.LLM, llm.Config, error) {
	// Normalize key for cache lookup so empty/unspecified Kind collapses
	// to KindReason (mirrors registry.normalizeKind without depending on
	// the unexported helper).
	cacheKey := key
	if cacheKey.Slot != llm.SlotExecutor {
		cacheKey.Kind = llm.KindUnspecified
	} else if cacheKey.Kind == llm.KindUnspecified {
		cacheKey.Kind = llm.KindReason
	}

	h.clientsMu.Lock()
	defer h.clientsMu.Unlock()

	cfg := h.cfgFor(cacheKey)
	if c, ok := h.clients[cacheKey]; ok {
		return c, cfg, nil
	}
	c, err := llm.New(&cfg)
	if err != nil {
		return nil, cfg, fmt.Errorf("step_handler: llm.New (%s): %w", cacheKey.Kind, err)
	}
	h.clients[cacheKey] = c
	return c, cfg, nil
}

// HandleStep implements runtime.StepHandler.
func (h *llmStepHandler) HandleStep(ctx context.Context, plan *ir.PlanTree, node *ir.PlanNode) (*runtime.StepResult, error) {
	if node == nil || node.Step == nil {
		return nil, fmt.Errorf("step_handler: nil step body for node %q", planNodeID(node))
	}

	// Route on the step's declared kind. Empty / unknown → KindReason.
	// (Validator at ir.ValidatePlan already rejects unknown kinds, so
	// reaching ParseStepKind with a malformed value would be a planner
	// bug that bypassed validation — defensive fallback to KindReason.)
	key := llm.RouteKey{
		Slot: llm.SlotExecutor,
		Kind: llm.ParseStepKind(node.Step.Kind),
	}
	client, cfg, err := h.clientFor(key)
	if err != nil {
		return nil, err
	}

	system := h.buildSystem()
	user := h.buildUser(plan, node)
	msgs := []interpreter.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	kindStr := key.Kind.String()

	// Router decision audit (sess#31d P4) — emitted BEFORE the decode
	// so even decode errors still leave a "which route was selected"
	// trace in the transcript.
	intentID := ""
	if plan != nil {
		intentID = plan.IntentID
	}
	streamUsedAtDecision := false
	if _, ok := client.(interpreter.StreamingLLM); ok {
		streamUsedAtDecision = true
	}
	recordRouterDecision(h.t, routerDecision{
		Slot:     llm.SlotExecutor.String(),
		Kind:     kindStr,
		Model:    cfg.Model,
		IntentID: intentID,
		NodeID:   node.ID,
		Streamed: streamUsedAtDecision,
		Reason:   "step.kind.resolve",
	})

	t0 := time.Now()
	var (
		text       string
		decodeErr  error
		streamUsed bool
	)
	// Streaming capability detection (Session 31c · P3a). When the
	// client implements interpreter.StreamingLLM, prefer the live
	// path so step.text.delta SSE events ship as the model decodes.
	// Falls back transparently for clients that don't (e.g. injected
	// fakes in tests, future non-OpenAI providers without SSE).
	if streamer, ok := client.(interpreter.StreamingLLM); ok {
		streamUsed = true
		text, decodeErr = h.streamStep(ctx, streamer, msgs, node, kindStr)
	} else {
		text, decodeErr = client.Decode(ctx, msgs, "")
	}
	dur := time.Since(t0)

	// Audit event carries the resolved kind + model + delivery mode so
	// per-route latency histograms can split streaming vs non-streaming
	// (canvas Section 8 / P4 deliverable).
	h.t.Event("step.llm.decode", "walk", map[string]interface{}{
		"node_id":     node.ID,
		"prompt_name": node.Step.PromptName,
		"kind":        kindStr,
		"model":       cfg.Model,
		"streamed":    streamUsed,
		"ms":          dur.Milliseconds(),
		"bytes":       len(text),
		"error":       errStr(decodeErr),
	})
	// Histogram accumulation (sess#31d P4). When the transcript has
	// a routerMetrics attached (daemon mode), record the latency
	// observation so /metrics + the periodic router.histogram flush
	// can surface p50/p99 + error rate per route.
	if m := h.t.Metrics(); m != nil {
		m.Observe(routeMetricKey{
			Slot:     llm.SlotExecutor.String(),
			Kind:     kindStr,
			Model:    cfg.Model,
			Streamed: streamUsed,
		}, dur.Milliseconds(), decodeErr)
	}
	// Surface the step's actual text to the SSE consumer so the UI can
	// render context (e.g. brainstormed alternatives) BEFORE the next
	// gate fires. Without this, gates that reference prior-step output
	// (like "Which idea?" with options "Idea 1/2/3") arrive without the
	// content the user needs to choose meaningfully.
	//
	// step.text is emitted in BOTH delivery modes (stream + non-stream)
	// so transcript replays + clients that haven't yet consumed
	// step.text.delta still get the final text in one shot.
	if decodeErr == nil && len(text) > 0 {
		preview := text
		const maxPreview = 8000
		if len(preview) > maxPreview {
			preview = preview[:maxPreview]
		}
		h.t.Event("step.text", "walk", map[string]interface{}{
			"node_id":     node.ID,
			"prompt_name": node.Step.PromptName,
			"kind":        kindStr,
			"text":        preview,
			"truncated":   len(text) > maxPreview,
			"streamed":    streamUsed,
			"ms":          dur.Milliseconds(),
		})
	}
	if decodeErr != nil {
		// The walker treats StepHandler errors as recoverable: it
		// records the err in WalkResult.Errors but continues. We
		// still surface a partial StepResult so the transcript has
		// LatencyMs even on failure.
		return &runtime.StepResult{LatencyMs: dur.Milliseconds()}, decodeErr
	}
	return &runtime.StepResult{
		Outputs:   map[string]string{},
		Text:      text,
		LatencyMs: dur.Milliseconds(),
	}, nil
}

// deltaFlushBytes is the byte-count threshold that triggers an
// immediate step.text.delta emission. Tuned so a typical 800-token
// step (~3.2 KB) yields ~16 events instead of one-per-token, which
// keeps the SSE broker's per-subscriber 256-event buffer comfortably
// under capacity for the brainstorming hot path (8 steps × 16 deltas
// + per-step text/decode + ~50 walker bookkeeping events ~= 200).
const deltaFlushBytes = 200

// streamStep drives a streaming Decode through the step.text.delta
// SSE channel, coalescing tokens to keep the broker buffer healthy.
//
// Coalescing rules (any one triggers a flush):
//   - Pending bytes >= deltaFlushBytes
//   - Incoming chunk contains a newline (preserves paragraph cadence
//     for narrative steps; brainstorm questions land cleanly when the
//     model emits one per line).
//
// On Stream completion (success or error) the residual pending buffer
// is flushed so the client never miss the trailing tail. Returns the
// fully accumulated text plus whatever error Stream surfaced — both
// are needed by HandleStep to emit step.text + step.llm.decode.
func (h *llmStepHandler) streamStep(
	ctx context.Context,
	streamer interpreter.StreamingLLM,
	msgs []interpreter.Message,
	node *ir.PlanNode,
	kindStr string,
) (string, error) {
	var (
		pending strings.Builder
		seq     int
		total   int
	)

	flush := func(reason string) {
		if pending.Len() == 0 {
			return
		}
		seq++
		delta := pending.String()
		pending.Reset()
		h.t.Event("step.text.delta", "walk", map[string]interface{}{
			"node_id":     node.ID,
			"prompt_name": node.Step.PromptName,
			"kind":        kindStr,
			"seq":         seq,
			"delta":       delta,
			"total_bytes": total,
			"reason":      reason, // "size" | "newline" | "final"
		})
	}

	text, err := streamer.Stream(ctx, msgs, "", func(d string) {
		if d == "" {
			return
		}
		pending.WriteString(d)
		total += len(d)
		if pending.Len() >= deltaFlushBytes {
			flush("size")
			return
		}
		if strings.ContainsRune(d, '\n') {
			flush("newline")
		}
	})
	flush("final")
	return text, err
}

func (h *llmStepHandler) buildSystem() string {
	var sb strings.Builder
	sb.WriteString("You are the Matrix executor running a single Step inside a plan walk.\n\n")
	sb.WriteString("Skill: " + h.skillURI + "\n")
	if len(h.skillMD) > 0 {
		sb.WriteString("\n== Skill body (SKILL.md) ==\n")
		sb.Write(h.skillMD)
		sb.WriteString("\n")
	}
	sb.WriteString("\nFollow the skill's intent. Be terse and decisive. ")
	sb.WriteString("If the step produces a structured output, format it as ")
	sb.WriteString("compact JSON for downstream parsing.\n")
	return sb.String()
}

func (h *llmStepHandler) buildUser(plan *ir.PlanTree, node *ir.PlanNode) string {
	var sb strings.Builder
	sb.WriteString("== Step ==\n")
	sb.WriteString("ID:          " + node.ID + "\n")
	sb.WriteString("Prompt name: " + node.Step.PromptName + "\n")
	if len(node.Step.Inputs) > 0 {
		// Resolve ${<nodeID>.output} / ${<nodeID>} references against the
		// real upstream node outputs the walker recorded on the plan tree.
		// Without this, the literal placeholder string reaches the model
		// and it confabulates (the false fleet "all-clear" root cause):
		// the report step never actually saw the fleet_summary result.
		outputs := collectNodeOutputs(plan)
		sb.WriteString("Inputs:\n")
		keys := make([]string, 0, len(node.Step.Inputs))
		for k := range node.Step.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  %s: %s\n", k, resolveOutputRefs(node.Step.Inputs[k], outputs)))
		}
	}
	if len(node.Step.ExpectedOutputs) > 0 {
		sb.WriteString("Expected outputs: " + strings.Join(node.Step.ExpectedOutputs, ", ") + "\n")
	}
	sb.WriteString("\nPlan context: this step belongs to plan " + plan.ID +
		" implementing intent " + plan.IntentID + ".\n")
	return sb.String()
}

// outputRefPattern matches plan-step input references of the form
// ${<nodeID>.output}, ${<nodeID>.text}, or bare ${<nodeID>}. The planner
// emits these in Step.Inputs to wire a prior node's output into a later
// step (e.g. "fleet_summary": "${n02.output}"). nodeID is the loose set
// of chars used for plan node ids (alphanumerics, _, -).
var outputRefPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_-]+)(?:\.(?:output|text))?\}`)

// collectNodeOutputs walks the plan tree and returns nodeID → recorded
// runtime output text (ResultText). Populated by the walker as tool
// calls and steps complete; consumed here to resolve ${...} input refs.
func collectNodeOutputs(plan *ir.PlanTree) map[string]string {
	out := map[string]string{}
	if plan == nil {
		return out
	}
	var walk func(n *ir.PlanNode)
	walk = func(n *ir.PlanNode) {
		if n == nil {
			return
		}
		if n.ResultText != "" {
			out[n.ID] = n.ResultText
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(&plan.Root)
	return out
}

// resolveOutputRefs replaces every ${<nodeID>.output} reference in val
// with the upstream node's actual recorded output. An unresolved
// reference (node missing or produced no output) is replaced with an
// explicit marker rather than left as a literal placeholder, so the
// model is told the data is unavailable instead of silently inventing it.
func resolveOutputRefs(val string, outputs map[string]string) string {
	if !strings.Contains(val, "${") {
		return val
	}
	return outputRefPattern.ReplaceAllStringFunc(val, func(match string) string {
		m := outputRefPattern.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		if resolved, ok := outputs[m[1]]; ok && resolved != "" {
			return resolved
		}
		return fmt.Sprintf("<unresolved upstream output %q — no data was produced by that node; do NOT fabricate a value>", m[1])
	})
}

// planNodeID is local to this file (avoids shadowing the ir.PlanNode.ID
// field name `nodeID` used elsewhere). Returns a printable identifier
// for transcript and error messages.
func planNodeID(n *ir.PlanNode) string {
	if n == nil {
		return "<nil>"
	}
	return n.ID
}

// ---------------------------------------------------------------------
// inProcessSubDispatch — runtime.SubDispatchHandler
// ---------------------------------------------------------------------

// inProcessSubDispatch implements runtime.SubDispatchHandler by recursively
// invoking another walker against the resolved sub-skill in the SAME
// agent process. This is the Q6 v1 carve-out (in-process under same agent
// only; cross-agent CortexScope handoff is v1.1).
//
// Construction is deferred to a maker closure so walk_cmd.go can pass in
// the parent walker's tool.Registry / cortex / envelope sink without
// pulling those types into this file's imports.
type inProcessSubDispatch struct {
	loader      *runtime.SkillLoader
	makeWalker  func(skill *runtime.LoadedSkill) (*runtime.Walker, error)
	synthesizer func(ctx context.Context, skill *runtime.LoadedSkill, parent *ir.PlanTree, node *ir.PlanNode) (*ir.PlanTree, error)
	t           *transcript
}

// HandleSubDispatch implements runtime.SubDispatchHandler.
func (s *inProcessSubDispatch) HandleSubDispatch(ctx context.Context, parent *ir.PlanTree, node *ir.PlanNode) (*runtime.SubDispatchResult, error) {
	if node == nil || node.SubDispatch == nil {
		return nil, fmt.Errorf("sub_dispatch: nil body on node %q", planNodeID(node))
	}
	skill, err := s.loader.Load(node.SubDispatch.SkillRef)
	if err != nil {
		return nil, fmt.Errorf("sub_dispatch: load %q: %w", node.SubDispatch.SkillRef, err)
	}
	s.t.Event("sub.skill.loaded", "walk", map[string]interface{}{
		"node_id": node.ID,
		"skill":   skill.URI,
		"verbs":   skill.MclVerbs,
	})

	subPlan, err := s.synthesizer(ctx, skill, parent, node)
	if err != nil {
		s.t.Event("sub.synthesize.error", "walk", map[string]interface{}{
			"node_id": node.ID,
			"skill":   skill.URI,
			"error":   err.Error(),
		})
		return nil, fmt.Errorf("sub_dispatch: synthesize: %w", err)
	}

	subWalker, err := s.makeWalker(skill)
	if err != nil {
		return nil, fmt.Errorf("sub_dispatch: build walker: %w", err)
	}

	t0 := time.Now()
	subResult, werr := subWalker.Run(ctx, subPlan)
	dur := time.Since(t0)
	s.t.Event("sub.walk.complete", "walk", map[string]interface{}{
		"node_id":   node.ID,
		"sub_plan":  subPlan.ID,
		"sub_nodes": countPlanNodes(&subPlan.Root),
		"ms":        dur.Milliseconds(),
		"error":     errStr(werr),
	})

	outcome := "success"
	if werr != nil || (subResult != nil && len(subResult.Errors) > 0) {
		outcome = "failure"
	}

	cited := make([]string, 0)
	if subResult != nil {
		for _, u := range subResult.EventURIs {
			cited = append(cited, string(u))
		}
	}

	return &runtime.SubDispatchResult{
		SubIntentID: subPlan.IntentID,
		Outcome:     outcome,
		CitedURIs:   cited,
	}, werr
}

// buildSubIntent assembles the synthetic *ir.Intent that v1 in-process
// sub-dispatch feeds to synthesize() for the sub-skill (Q6 carve-out).
//
// Why this helper exists (sess#37c):
//
//	synthesize() guards its inputs with a nil-check that rejects
//	Intent==nil with "synthesize: missing required input (skill/intent/
//	manifest/registry)". The pre-sess#37c synthesizer closure passed
//	Intent: nil with the comment "v1: sub-plan derived from sub-skill
//	manifest only" — but that contract was never honoured on the
//	consumer side, so every sub_dispatch hit the guard immediately
//	after sub.skill.loaded and the walker bubbled subagent_failed.
//
// v1 sub-dispatch skips the compile phase against the sub-skill (no
// clarify, no slot resolve, no LLM compile round-trip). The sub-Intent
// is derived structurally from the parent walk context + sub-skill
// metadata so synthesize() has the minimum it needs to render the
// planner prompt: a non-nil Intent with ID + Frame.Verb + Prose.
//
// Verb resolution (highest-priority wins):
//  1. node.SubDispatch.SubIntent.Verb when the parent planner
//     populated NodeSubDispatch.sub_intent (future-proofing; the v1
//     planner system prompt only emits {skill_ref}).
//  2. The sub-skill's first §SKILL.mcl.verbs entry. Sub-skills declare
//     the verb their §PROCEDURE on-block handles; this matches.
//  3. "analyze" — D7 read-only fallback when the sub-skill has no
//     declared verbs (defensive; the loader rejects skills without
//     §SKILL.mcl.verbs but the runtime never trusts that invariant).
//
// Other fields (Frame.Objects, Frame.Constraints, etc.) inherit from
// node.SubDispatch.SubIntent when present, else zero-value. Parent
// linkage uses ir.Intent.Parent so the cortex audit chain ties the
// sub-Intent back to the parent.
func buildSubIntent(sk *runtime.LoadedSkill, parent *ir.PlanTree, node *ir.PlanNode, actor *actorIdentity) *ir.Intent {
	var frame ir.Frame
	if node != nil && node.SubDispatch != nil && node.SubDispatch.SubIntent != nil {
		frame = *node.SubDispatch.SubIntent
	}
	if frame.Verb == "" {
		switch {
		case sk != nil && len(sk.MclVerbs) > 0:
			frame.Verb = sk.MclVerbs[0]
		default:
			frame.Verb = ir.VerbAnalyze
		}
	}

	parentRef, parentID, nodeID := "", "", ""
	if parent != nil {
		parentID = parent.IntentID
		if parentID != "" {
			parentRef = "matrix://intent/" + parentID
		}
	}
	if node != nil {
		nodeID = node.ID
	}

	skURI := ""
	if sk != nil {
		skURI = sk.URI
	}

	actorURI, agentURI := "", ""
	if actor != nil {
		actorURI = actor.UserURI
		agentURI = actor.AgentURI
	}

	prose := fmt.Sprintf(
		"In-process sub-dispatch from parent intent %s, plan node %s, "+
			"invoking skill %s with verb=%s. Synthesize a plan_tree@1 "+
			"that consumes this sub-skill's declared §TOOLS and §SUB_SKILLS "+
			"to fulfil the sub-skill's §PROCEDURE for this verb.",
		parentID, nodeID, skURI, frame.Verb)

	return &ir.Intent{
		ID:        newULIDLike(),
		Version:   "mcl/0.1",
		Parent:    parentRef,
		Actor:     actorURI,
		Agent:     agentURI,
		Prose:     prose,
		Frame:     frame,
		State:     ir.StateExecuting,
		CreatedAt: nowRFC3339(),
	}
}

// ---------------------------------------------------------------------
// stdinGateHandler — runtime.GateHandler
// ---------------------------------------------------------------------

// gatePolicy controls stdinGateHandler.HandleGate behaviour.
type gatePolicy string

const (
	gatePolicyApprove gatePolicy = "approve"
	gatePolicyDeny    gatePolicy = "deny"
	gatePolicyPrompt  gatePolicy = "prompt"
)

// stdinGateHandler implements runtime.GateHandler with three modes:
//
//   - "approve" → always approve with the first option (or "yes")
//   - "deny"    → always deny with the last option (or "no")
//   - "prompt"  → read a line from stdin
//
// Spec: research/02-protocol.md §13 PolicyGate.
type stdinGateHandler struct {
	policy gatePolicy
	actor  string
	reader io.Reader
	writer io.Writer
	t      *transcript
}

func newStdinGateHandler(policy, actor string, t *transcript) *stdinGateHandler {
	p := gatePolicy(policy)
	switch p {
	case gatePolicyApprove, gatePolicyDeny, gatePolicyPrompt:
		// ok
	default:
		p = gatePolicyPrompt
	}
	return &stdinGateHandler{
		policy: p,
		actor:  actor,
		reader: os.Stdin,
		writer: os.Stderr,
		t:      t,
	}
}

// HandleGate implements runtime.GateHandler.
func (g *stdinGateHandler) HandleGate(ctx context.Context, node *ir.PlanNode) (*runtime.GateDecision, error) {
	if node == nil || node.Gate == nil {
		return nil, fmt.Errorf("gate: nil gate body on node %q", planNodeID(node))
	}
	g.t.Event("gate.invoked", "walk", map[string]interface{}{
		"node_id":  node.ID,
		"question": node.Gate.Question,
		"options":  node.Gate.Options,
		"rule":     node.Gate.RuleRef,
		"policy":   string(g.policy),
		"actor":    g.actor,
	})

	approved := true
	answer := ""
	switch g.policy {
	case gatePolicyApprove:
		approved = true
		answer = firstOption(node.Gate.Options, "yes")
	case gatePolicyDeny:
		approved = false
		answer = lastOption(node.Gate.Options, "no")
	default:
		fmt.Fprintln(g.writer, "\n── Gate ──")
		if node.Gate.RuleRef != "" {
			fmt.Fprintf(g.writer, "Rule: %s\n", node.Gate.RuleRef)
		}
		fmt.Fprintf(g.writer, "Question: %s\n", node.Gate.Question)
		if len(node.Gate.Options) > 0 {
			fmt.Fprintf(g.writer, "Options: %s\n", strings.Join(node.Gate.Options, ", "))
		}
		fmt.Fprint(g.writer, "> ")
		line, rerr := bufio.NewReader(g.reader).ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			return nil, fmt.Errorf("gate: stdin read: %w", rerr)
		}
		answer = strings.TrimSpace(line)
		if answer == "" {
			answer = firstOption(node.Gate.Options, "no")
		}
		// Approval semantics: explicit "no"/"deny"/empty → deny.
		switch strings.ToLower(answer) {
		case "no", "n", "deny", "denied", "reject":
			approved = false
		default:
			approved = true
		}
	}

	g.t.Event("gate.decided", "walk", map[string]interface{}{
		"node_id":  node.ID,
		"approved": approved,
		"answer":   answer,
		"actor":    g.actor,
	})
	return &runtime.GateDecision{
		Approved: approved,
		Answer:   answer,
	}, nil
}

func firstOption(opts []string, fallback string) string {
	if len(opts) > 0 {
		return opts[0]
	}
	return fallback
}

func lastOption(opts []string, fallback string) string {
	if len(opts) > 0 {
		return opts[len(opts)-1]
	}
	return fallback
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
