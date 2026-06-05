// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"matrix/executor/mcp"
	"matrix/executor/runtime"
	"matrix/executor/tool"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/interpreter"
	"matrix/mcl/mtx/parser"
)

// synthesizeOpts configures plan synthesis.
type synthesizeOpts struct {
	Skill    *runtime.LoadedSkill
	Intent   *ir.Intent
	Manifest *tool.AgentManifest
	Registry *tool.Registry
	// Manager (optional) supplies live tool InputSchemas via
	// Manager.Tools(alias). When set, the system prompt embeds the
	// JSON Schema for every allowlisted tool so the LLM emits valid
	// args. nil falls back to URI+description only (legacy walk_cmd
	// behaviour).
	Manager  *mcp.Manager
	Agent    string // matrix://agent/<did>
	Model    string // override DefaultPlannerModel.Model (sess#31a)
	BaseURL  string // optional override for the LLM endpoint (gateway / BYO swap)
	Seed     int64  // forwarded to the planner model for D11 determinism
	MaxRetry int    // retries on parse/validate failure (default 2)
	// WorkspaceRoot is the absolute filesystem path the agent's MCP
	// servers (notably fs/git) are scoped to. Embedded into the
	// system prompt so the executor LLM emits valid `path` /
	// `repo_path` args instead of hallucinating paths that the MCP
	// server will reject. Empty string omits the section.
	WorkspaceRoot string

	// --- sess#32 ambient-architect MatrixGateway routing (plan §5.16) ---
	//
	// Same posture as compileOpts: optional, opt-in via non-empty
	// GatewayURL. When set, the planner LLM call is metered against
	// the credit_ledger and CostHook receives the response headers
	// for daemon-side telemetry. ActorDID, IntentID, GoalID stamp the
	// gateway-side audit trail.
	GatewayURL string
	ActorDID   string
	IntentID   string
	GoalID     string
	CostHook   func(http.Header)

	// ForgeMode (sess#36 / Forge Phase 3) — when true, the planner
	// resolves from llm.ForgeRegistry (opencode.ai/zen + identity
	// preamble). Empty preserves legacy DefaultRegistry posture.
	ForgeMode bool
}

// synthesizeResult bundles the produced plan + audit metadata.
type synthesizeResult struct {
	Plan        *ir.PlanTree
	PlanJSON    []byte
	PlanHash    string
	ModelID     string
	ModelDigest string
	RawOutput   string // last raw LLM output (before JSON unmarshal)
	Rounds      int
	LatencyMs   int64
}

// synthesize calls the planner LLM (DefaultPlannerModel — sess#31a)
// to produce a typed *ir.PlanTree that consumes the supplied Intent.
//
// Production semantics:
//   - Planner LLM is REQUIRED. Returns error on llm.New failure.
//   - Uses plan_tree@1 grammar (S23Q13 lock in MCL/llm/model.go) which
//     the planner slot is pre-configured for (gpt-oss-120b on Fireworks,
//     recursive $defs/plan_node handling proven). Falls back to
//     unconstrained decode + retry-on-parse-failure when grammar is
//     declined by an override model.
//   - System prompt enumerates the agent's manifest tools (URIs +
//     descriptions) so the model can only emit valid ToolRefs.
//   - User prompt contains the Intent + skill description + retry hint
//     on subsequent rounds.
//   - On parse/validate failure, retries up to MaxRetry times feeding the
//     last error back to the model. Final failure surfaces with full
//     trace in transcript + returned error.
//
// Spec citations:
//
//	research/06-agents.md §5.2 (plan synthesis is its own tier per sess#31a)
//	matrix.kvx sess#31a (SlotPlanner introduced; was conflated under SlotExecutor)
//	MCL/ir/plan.go:3-8 (IR shared producer+consumer)
func synthesize(ctx context.Context, opts synthesizeOpts, t *transcript) (*synthesizeResult, error) {
	if opts.Skill == nil || opts.Intent == nil || opts.Manifest == nil || opts.Registry == nil {
		return nil, fmt.Errorf("synthesize: missing required input (skill/intent/manifest/registry)")
	}
	if opts.MaxRetry == 0 {
		opts.MaxRetry = 2
	}
	if opts.Seed == 0 {
		opts.Seed = 42
	}

	// Build planner LLM client. DefaultPlannerModel already pre-configures
	// GrammarJSONSchema + DefaultGrammars (plan_tree@1 with recursive $defs);
	// we re-set them defensively so any opts.Model override that targets a
	// non-grammar model still gets the grammar attempted (provider falls
	// back to free-form when it can't honour the schema).
	//
	// sess#36: ForgeMode swaps to llm.ForgePlannerModel (opencode.ai/zen +
	// claude-opus-4-7) for self-maintenance plan synthesis.
	var cfg llm.Config
	if opts.ForgeMode {
		cfg = llm.ForgePlannerModel()
	} else {
		cfg = llm.DefaultPlannerModel()
	}
	if opts.Model != "" {
		cfg.Model = opts.Model
	}
	if opts.BaseURL != "" {
		cfg.Endpoint = strings.TrimRight(opts.BaseURL, "/") + "/v1/chat/completions"
	}
	cfg.Seed = opts.Seed
	cfg.Grammars = llm.DefaultGrammars()
	cfg.GrammarMode = llm.GrammarJSONSchema
	intentID := opts.IntentID
	if intentID == "" && opts.Intent != nil {
		intentID = opts.Intent.ID
	}
	if opts.GatewayURL != "" {
		// Sess#32 ambient-architect MatrixGateway routing (plan §5.16).
		// Planner slot — the gateway whitelists which model is allowed
		// here; BYO keys bypass that check (handled gateway-side, not
		// the daemon's concern).
		cfg.GatewayURL = opts.GatewayURL
		cfg.ActorDID = opts.ActorDID
		cfg.IntentID = intentID
		cfg.GoalID = opts.GoalID
		cfg.SlotLabel = llm.SlotPlanner.String()
		cfg.OnResponseHeaders = opts.CostHook
	}
	client, err := llm.New(&cfg)
	if err != nil {
		return nil, fmt.Errorf("synthesize: llm.New (planner): %w", err)
	}

	t.Event("synth.start", "synth", map[string]interface{}{
		"model":           cfg.Model,
		"intent_id":       opts.Intent.ID,
		"verb":            opts.Intent.Frame.Verb,
		"skill_uri":       opts.Skill.URI,
		"manifest_tools":  countManifestTools(opts.Manifest),
		"declared_tools":  len(opts.Skill.DeclaredTools),
		"tools_none":      opts.Skill.ToolsNone,
		"declared_subs":   len(opts.Skill.DeclaredSubSkills),
		"sub_skills_none": opts.Skill.SubSkillsNone,
		"workspace_root":  opts.WorkspaceRoot,
		"max_retry":       opts.MaxRetry,
	})

	// Router decision audit (sess#31d P4). Synthesizer routes through
	// SlotPlanner; the resolved model id is what the planner LLM
	// actually called, after CLI/daemon overrides.
	recordRouterDecision(t, routerDecision{
		Slot:     llm.SlotPlanner.String(),
		Model:    cfg.Model,
		IntentID: opts.Intent.ID,
		Reason:   "planner.slot.resolve",
	})

	systemMsg := buildSystemPrompt(opts.Skill, opts.Manifest, opts.Manager, opts.WorkspaceRoot)
	userBase := buildUserPrompt(opts.Intent, opts.Skill)

	var (
		lastErr    error
		lastOutput string
		totalMS    int64
		planResult *ir.PlanTree
		planJSON   []byte
		planHash   string
	)

	for round := 1; round <= opts.MaxRetry+1; round++ {
		userMsg := userBase
		if lastErr != nil {
			userMsg = userBase + "\n\nPrevious attempt failed validation:\n" +
				lastErr.Error() + "\n\nRaw output was:\n" + truncate(lastOutput, 2000) +
				"\n\nProduce a corrected plan_tree@1 JSON document."
		}

		messages := []interpreter.Message{
			{Role: "system", Content: systemMsg},
			{Role: "user", Content: userMsg},
		}

		t0 := time.Now()
		// Reasoning-aware decode: frontier planner models surface their
		// chain-of-thought in a sibling reasoning_content field
		// (DecodeWithReasoning, chat-completions only). Taking only the
		// answer channel keeps that prose out of the JSON we unmarshal.
		// Models that don't separate it fall back to plain Decode and are
		// handled by extractPlanJSON below.
		var (
			raw  string
			derr error
		)
		if rd, ok := client.(reasoningDecoder); ok {
			raw, _, derr = rd.DecodeWithReasoning(ctx, messages, "plan_tree@1")
		} else {
			raw, derr = client.Decode(ctx, messages, "plan_tree@1")
		}
		dur := time.Since(t0)
		totalMS += dur.Milliseconds()
		t.Event("synth.llm.decode", "synth", map[string]interface{}{
			"round": round,
			"ms":    dur.Milliseconds(),
			"bytes": len(raw),
			"model": cfg.Model,
			"error": errStr(derr),
		})
		if m := t.Metrics(); m != nil {
			m.Observe(routeMetricKey{
				Slot:  llm.SlotPlanner.String(),
				Model: cfg.Model,
			}, dur.Milliseconds(), derr)
		}
		if derr != nil {
			lastErr = derr
			continue
		}
		lastOutput = raw

		// Extract the plan_tree@1 JSON object: strip inline reasoning
		// blocks + markdown fences and pull the first balanced {...} so a
		// reasoning model's chain-of-thought prose around the JSON never
		// trips json.Unmarshal (the 'invalid character' / 'unexpected end
		// of JSON input' synth.parse.error class).
		clean := extractPlanJSON(raw)

		// Decode into ir.PlanTree.
		var plan ir.PlanTree
		if uerr := json.Unmarshal([]byte(clean), &plan); uerr != nil {
			lastErr = fmt.Errorf("unmarshal: %w", uerr)
			t.Event("synth.parse.error", "synth", map[string]interface{}{
				"round": round,
				"error": uerr.Error(),
			})
			continue
		}

		// Stamp missing audit fields the LLM may not have produced.
		if plan.IntentID == "" {
			plan.IntentID = opts.Intent.ID
		}
		if plan.Version == "" {
			plan.Version = "plan_tree@1"
		}
		if plan.CreatedAt == "" {
			plan.CreatedAt = nowRFC3339()
		}
		if plan.CreatedBy == "" {
			plan.CreatedBy = opts.Agent
		}
		if plan.SkillRef == "" {
			plan.SkillRef = opts.Skill.URI
		}
		if plan.ModelDigest == "" {
			plan.ModelDigest = sha256Hex(cfg.Model)
		}
		if plan.ID == "" {
			plan.ID = newULIDLike()
		}

		// Compute the hash on canonical form.
		plan.Hash = ""
		ph, herr := ir.HashPlan(&plan)
		if herr != nil {
			lastErr = fmt.Errorf("hash plan: %w", herr)
			continue
		}
		plan.Hash = ph

		if verr := ir.ValidatePlan(&plan); verr != nil {
			lastErr = fmt.Errorf("validate plan: %w", verr)
			t.Event("synth.validate.error", "synth", map[string]interface{}{
				"round": round,
				"error": verr.Error(),
			})
			continue
		}

		// Verify every ToolRef in the plan is resolvable through the
		// agent's registry. Surfaces hallucinated tools BEFORE the walk.
		if verr := verifyToolRefs(&plan, opts.Registry); verr != nil {
			lastErr = verr
			t.Event("synth.tool.error", "synth", map[string]interface{}{
				"round": round,
				"error": verr.Error(),
			})
			continue
		}

		// Enforce the skill's §TOOLS / §SUB_SKILLS allow-lists. The
		// LLM gets shown only the allowed tools in the system prompt,
		// but a defensive check prevents prompt regressions or model
		// drift from leaking through.
		if verr := verifySkillAllowList(&plan, opts.Skill); verr != nil {
			lastErr = verr
			t.Event("synth.allowlist.error", "synth", map[string]interface{}{
				"round": round,
				"error": verr.Error(),
			})
			continue
		}

		canon, cerr := ir.CanonicalJSONPlan(&plan)
		if cerr != nil {
			lastErr = fmt.Errorf("canonical json plan: %w", cerr)
			continue
		}

		planResult = &plan
		planJSON = canon
		planHash = ph
		t.Event("synth.plan.ok", "synth", map[string]interface{}{
			"round":     round,
			"plan_id":   plan.ID,
			"plan_hash": planHash,
			"nodes":     countPlanNodes(&plan.Root),
		})
		break
	}

	if planResult == nil {
		return nil, fmt.Errorf("synthesize: exhausted %d retries; last error: %w",
			opts.MaxRetry+1, lastErr)
	}

	return &synthesizeResult{
		Plan:        planResult,
		PlanJSON:    planJSON,
		PlanHash:    planHash,
		ModelID:     cfg.Model,
		ModelDigest: sha256Hex(cfg.Model),
		RawOutput:   lastOutput,
		Rounds:      countRounds(opts.MaxRetry, lastErr, planResult),
		LatencyMs:   totalMS,
	}, nil
}

// buildSystemPrompt assembles the role + capabilities section. The
// agent's manifest tool URIs are filtered through the skill's
// §TOOLS allow-list so the executor LLM only sees what the skill
// author sanctioned. When mgr is non-nil, each tool's live
// InputSchema (from MCP tools/list) is embedded so the model emits
// valid args. workspaceRoot is announced so path-taking tools (fs,
// git) get sane absolute paths instead of hallucinated ones.
func buildSystemPrompt(skill *runtime.LoadedSkill, manifest *tool.AgentManifest, mgr *mcp.Manager, workspaceRoot string) string {
	var sb strings.Builder
	sb.WriteString("You are the Matrix executor planner.\n\n")
	sb.WriteString("Your job: given an Intent IR and a list of available tools, ")
	sb.WriteString("emit a single JSON document conforming to the plan_tree@1 schema. ")
	sb.WriteString("Plan tree generation lives under the executor per research/06 §5.2.\n\n")

	sb.WriteString("== Skill ==\n")
	sb.WriteString("URI: " + skill.URI + "\n")
	if skill.Display != "" {
		sb.WriteString("Name: " + skill.Display + "\n")
	}
	if skill.Description != "" {
		sb.WriteString("Description: " + skill.Description + "\n")
	}
	if len(skill.MclVerbs) > 0 {
		sb.WriteString("Handles verbs: " + strings.Join(skill.MclVerbs, ", ") + "\n")
	}
	if len(skill.MdBytes) > 0 {
		sb.WriteString("\n== Skill body (SKILL.md, executor-LLM consumer per matrix.kvx invariant) ==\n")
		sb.Write(skill.MdBytes)
		sb.WriteString("\n")
	}

	if workspaceRoot != "" {
		sb.WriteString("\n== Workspace ==\n")
		sb.WriteString("Workspace root (absolute path): " + workspaceRoot + "\n")
		sb.WriteString("All filesystem and git tools are scoped to this directory. ")
		sb.WriteString("Use this exact path (or a sub-path under it) for any tool ")
		sb.WriteString("argument that names a file/directory/repo location. ")
		sb.WriteString("Paths outside this root are denied at runtime.\n")
	}

	writeStepKindHintSection(&sb, skill)
	writeToolSection(&sb, skill, manifest, mgr)
	writeSubSkillSection(&sb, skill)

	if len(manifest.AllowedSideEffects) > 0 {
		sb.WriteString("\nAllowed side-effect classes: " +
			strings.Join(manifest.AllowedSideEffects, ", ") + "\n")
	}

	sb.WriteString("\n== Plan tree schema (plan_tree@1) ==\n")
	sb.WriteString(`Top-level shape:
  {
    "id": "<ULID-like 26-char string>",
    "v":  "plan_tree@1",
    "intent_id": "<intent.id>",
    "skill_ref": "<the skill URI above>",
    "root": <PlanNode>
  }

PlanNode shape (discriminated by "kind"):
  - {"id": "...", "kind": "sequential",   "children": [PlanNode, ...]}
  - {"id": "...", "kind": "parallel",     "children": [PlanNode, ...]}
  - {"id": "...", "kind": "step",          "step": {"prompt_name": "...", "inputs": {...}, "kind": "<reason|code|summarize|write|transform|classify|hard_reason>"}}
  - {"id": "...", "kind": "tool_call",     "tool_call": {"tool_ref": "<matrix://tool/...>", "args": {"k":"v"}, "side_effect_class": "read|write|network|shell|chain"}}
  - {"id": "...", "kind": "sub_dispatch",  "sub_dispatch": {"skill_ref": "matrix://skill/<slug>@<ver>"}}
  - {"id": "...", "kind": "gate",          "gate": {"question": "...", "options": ["yes","no"]}}

Constraints:
  - Every node MUST have a unique "id" (e.g. n01, n02, n02a, ...).
  - tool_call.args VALUES MUST be strings (JSON-string-only). Bools/ints are
    coerced server-side; emit strings like "true" or "42".
  - Plan must include AT LEAST one terminal action (tool_call/step/gate).
  - Use "sequential" for ordered dependencies; "parallel" only when truly
    independent (e.g. fan-out reads).
  - PREFER "parallel" aggressively for independent steps. Two steps are
    independent when neither reads an output the other produces and
    neither shares state with the other (e.g. "summarize doc A" and
    "summarize doc B" → parallel). Sequential is the right default
    only when step N+1 consumes step N's output. Brainstorming N
    independent ideas, drafting K alternative options, fan-out reads
    across cortex, and exploring sibling design directions are all
    parallel use-cases — emit them as children of a single
    {"kind":"parallel"} node so the walker fans them out as goroutines
    (matrix.kvx S23Q3) and the wall-clock cost collapses to max(...)
    instead of sum(...).
  - Gate options MUST be fully self-describing strings the user can pick
    from in isolation. NEVER emit opaque placeholders like "Idea 1",
    "Option A", "Approach 2" — bake the actual concept text into each
    option (e.g. "Yield aggregator that auto-routes stablecoin deposits
    across Aave/Compound" instead of "Idea 1"). If you don't have the
    concrete content yet, do NOT emit the gate — emit a step first to
    generate the content, then a follow-up replanning point, but never
    a gate with stub labels.
  - Gate questions MUST stand alone with full context. Do not assume the
    user remembers prior steps. Include in the question text any
    information the user needs to choose an option meaningfully.
  - step.kind is OPTIONAL and routes the step to a specialist executor
    model (Session 31b model router). Closed enum: reason (default),
    code, summarize, write, transform, classify, hard_reason. If the
    "Step routing hints" section above maps this Intent's verb to a
    specific kind, emit that exact value as step.kind for every
    NodeStep child under that verb's plan branch. Omit the field
    when no hint applies (the executor will route to the default
    reason-kind model).
  - REASON-ONLY PLANS ARE POLICY-FORBIDDEN when ANY of the following
    is true: (a) the "Available tools" section above lists ≥1 tool,
    (b) the Intent prose references a filesystem path or directory,
    (c) the Intent prose contains a persistence verb (write, save,
    store, persist, commit, remember, record, log, attest, snapshot),
    (d) the Intent prose asks the system to read, analyze, map, or
    inspect concrete artifacts (files, packages, code, repos). A
    plan that consists ENTIRELY of {"kind":"step", "step":{"kind":"reason"}}
    nodes under these conditions is a contract violation — the whole
    point of the MCL compile→plan→execute pipeline (matrix.kvx D18)
    is to translate human prose into typed PRECISE tool dispatches,
    not into LLM monologue. If the skill declares §TOOLS=none but the
    agent manifest above lists tools, the manifest's tools are still
    available; use them. If the Intent demands persistence but no
    write-class tool is listed, emit a tool_call to
    forge-bridge.shell_exec whose "command" arg runs a curl POST to
    ${MATRIX_DAEMON}/memory with the Authorization Bearer header and
    a JSON body so cortex.Write happens through the daemon's HTTP
    surface. Pure reason is the right plan shape ONLY when the prose
    is a self-contained reasoning problem with no external state
    (math, classification, summarization of a string the user
    provided inline) — never for "analyze the repo at <path>".

Output ONLY the JSON. Do not wrap in markdown fences. Do not narrate.
`)
	return sb.String()
}

// buildUserPrompt renders the intent + a minimal directive.
func buildUserPrompt(intent *ir.Intent, skill *runtime.LoadedSkill) string {
	var sb strings.Builder
	sb.WriteString("== Intent ==\n")
	sb.WriteString(fmt.Sprintf("ID:   %s\n", intent.ID))
	sb.WriteString(fmt.Sprintf("Verb: %s\n", intent.Frame.Verb))
	if intent.Prose != "" {
		sb.WriteString(fmt.Sprintf("Prose: %s\n", intent.Prose))
	}
	if len(intent.Frame.Objects) > 0 {
		sb.WriteString("Objects:\n")
		for _, o := range intent.Frame.Objects {
			ref := o.Value
			if o.URI != "" {
				ref = o.URI
			}
			sb.WriteString(fmt.Sprintf("  - %s [%s] = %s\n", o.Name, o.Type, ref))
		}
	}
	if len(intent.Frame.Constraints) > 0 {
		sb.WriteString("Constraints:\n")
		for _, c := range intent.Frame.Constraints {
			sb.WriteString(fmt.Sprintf("  - %s (hard=%v)\n", c.Type, c.Hard))
		}
	}
	if len(intent.Frame.SuccessCriteria) > 0 {
		sb.WriteString("Success criteria:\n")
		for _, p := range intent.Frame.SuccessCriteria {
			sb.WriteString(fmt.Sprintf("  - %s\n", p.Type))
		}
	}
	if len(intent.References) > 0 {
		sb.WriteString("Cortex references (grounded at compile time):\n")
		for _, r := range intent.References {
			sb.WriteString(fmt.Sprintf("  - %s [%s]: %s\n", r.URI, r.Type, r.Summary))
		}
	}
	sb.WriteString("\nProduce a plan_tree@1 JSON document that fulfills this Intent ")
	sb.WriteString("using the available tools.\n")
	return sb.String()
}

// verifyToolRefs walks the plan and checks every ToolCall resolves
// against the registry. Returns the first failure.
func verifyToolRefs(plan *ir.PlanTree, reg *tool.Registry) error {
	var bad []string
	walkPlanRec(&plan.Root, func(n *ir.PlanNode) {
		if n.Kind != ir.NodeToolCall || n.ToolCall == nil {
			return
		}
		if _, err := reg.Get(n.ToolCall.ToolRef); err != nil {
			bad = append(bad, fmt.Sprintf("node=%s tool_ref=%s err=%v",
				n.ID, n.ToolCall.ToolRef, err))
		}
	})
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("synthesize: %d unresolved tool ref(s): %s",
		len(bad), strings.Join(bad, "; "))
}

// verifySkillAllowList enforces §TOOLS / §SUB_SKILLS allow-lists on
// the synthesized plan. Returns nil when the plan respects the
// declared scope. The check is the second line of defense after the
// system prompt; without it, prompt regressions or model drift could
// silently leak unauthorized capabilities through.
//
// Semantics:
//   - skill.ToolsNone=true → ANY tool_call node is rejected.
//   - skill.DeclaredTools non-empty → tool_call.tool_ref must be a
//     member.
//   - skill.DeclaredTools nil and ToolsNone false → no enforcement
//     (covers non-skill files / future opt-out manifests).
//   - skill.SubSkillsNone=true → ANY sub_dispatch node is rejected.
//   - skill.DeclaredSubSkills non-empty → sub_dispatch.skill_ref must
//     be a member.
func verifySkillAllowList(plan *ir.PlanTree, skill *runtime.LoadedSkill) error {
	if skill == nil {
		return nil
	}
	toolSet := stringSet(skill.DeclaredTools)
	subSet := stringSet(skill.DeclaredSubSkills)

	var bad []string
	walkPlanRec(&plan.Root, func(n *ir.PlanNode) {
		switch n.Kind {
		case ir.NodeToolCall:
			if n.ToolCall == nil {
				return
			}
			if skill.ToolsNone {
				bad = append(bad, fmt.Sprintf("node=%s tool_call=%s rejected: skill declares §TOOLS none",
					n.ID, n.ToolCall.ToolRef))
				return
			}
			if len(toolSet) == 0 {
				return // unconstrained
			}
			if _, ok := toolSet[n.ToolCall.ToolRef]; !ok {
				bad = append(bad, fmt.Sprintf("node=%s tool_call=%s not in skill §TOOLS allow-list",
					n.ID, n.ToolCall.ToolRef))
			}
		case ir.NodeSubDispatch:
			if n.SubDispatch == nil {
				return
			}
			if skill.SubSkillsNone {
				bad = append(bad, fmt.Sprintf("node=%s sub_dispatch=%s rejected: skill declares §SUB_SKILLS none",
					n.ID, n.SubDispatch.SkillRef))
				return
			}
			if len(subSet) == 0 {
				return
			}
			if _, ok := subSet[n.SubDispatch.SkillRef]; !ok {
				bad = append(bad, fmt.Sprintf("node=%s sub_dispatch=%s not in skill §SUB_SKILLS allow-list",
					n.ID, n.SubDispatch.SkillRef))
			}
		}
	})
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("synthesize: skill allow-list violations (%d): %s",
		len(bad), strings.Join(bad, "; "))
}

func stringSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

// writeToolSection renders the "== Available tools ==" block,
// filtered by the skill's §TOOLS allow-list. When mgr is non-nil and
// has live schemas for the relevant alias, each tool entry includes
// its JSON-Schema InputSchema so the LLM emits valid args.
func writeToolSection(sb *strings.Builder, skill *runtime.LoadedSkill, manifest *tool.AgentManifest, mgr *mcp.Manager) {
	sb.WriteString("\n== Available tools ==\n")

	if skill.ToolsNone {
		sb.WriteString("This skill declares §TOOLS = none. ")
		sb.WriteString("DO NOT emit any tool_call nodes. ")
		sb.WriteString("Build the plan from step (LLM prompts), gate (user questions), ")
		sb.WriteString("sequential, and parallel nodes only.\n")
		return
	}

	allow := stringSet(skill.DeclaredTools)
	if len(allow) == 0 && !skill.ToolsNone {
		// Skill section absent (non-skill caller); fall back to the
		// full agent-manifest enumeration. Keeps walk_cmd's
		// historical behaviour intact for ad-hoc CLI plans.
		writeToolListing(sb, manifest, mgr, nil)
		return
	}
	writeToolListing(sb, manifest, mgr, allow)
}

// writeToolListing renders the manifest tools, optionally filtered
// by allow. When allow is nil, every tool is listed (legacy mode).
func writeToolListing(sb *strings.Builder, manifest *tool.AgentManifest, mgr *mcp.Manager, allow map[string]struct{}) {
	sb.WriteString("Use ONLY these matrix://tool URIs in NodeToolCall.tool_ref. ")
	sb.WriteString("Any other URI will be rejected at plan validation.\n\n")

	count := 0
	for _, srv := range manifest.Servers {
		// Pre-fetch live schemas for this server (one map per
		// alias; constant-time lookup per tool).
		schemas := liveSchemaMap(mgr, srv.Alias)
		for _, te := range srv.Tools {
			uri := fmt.Sprintf("matrix://tool/mcp/%s/%s@%s", srv.Alias, te.Name, srv.Version)
			if allow != nil {
				if _, ok := allow[uri]; !ok {
					continue
				}
			}
			count++
			sb.WriteString(fmt.Sprintf("  - %s\n", uri))
			sb.WriteString(fmt.Sprintf("      side_effect_class: %s\n", te.SideEffectClass))
			if te.Description != "" {
				sb.WriteString(fmt.Sprintf("      description: %s\n", te.Description))
			}
			if schemaJSON, ok := schemas[te.Name]; ok {
				sb.WriteString("      input_schema: ")
				sb.WriteString(compactJSON(schemaJSON))
				sb.WriteString("\n")
			}
		}
	}
	if count == 0 {
		sb.WriteString("  (no tools allowed by this skill)\n")
	}
}

// liveSchemaMap returns name→inputSchema for the given alias, or an
// empty map when the manager is nil or the alias has no cached
// tools/list response.
func liveSchemaMap(mgr *mcp.Manager, alias string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if mgr == nil {
		return out
	}
	for _, t := range mgr.Tools(alias) {
		if len(t.InputSchema) > 0 {
			out[t.Name] = t.InputSchema
		}
	}
	return out
}

// compactJSON returns a single-line representation of a JSON-Schema
// blob suitable for embedding in a system prompt. Falls back to the
// raw bytes when re-encoding fails (e.g. malformed schema).
func compactJSON(raw json.RawMessage) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(out)
}

// writeSubSkillSection renders the "== Available sub-skills ==" block.
// none → strong negative; an allow-list → enumeration; absence/empty
// → no section emitted (sub_dispatch nodes are independently gated by
// the daemonState.allowSubDispatch flag).
func writeSubSkillSection(sb *strings.Builder, skill *runtime.LoadedSkill) {
	if skill.SubSkillsNone {
		sb.WriteString("\n== Available sub-skills ==\n")
		sb.WriteString("This skill declares §SUB_SKILLS = none. ")
		sb.WriteString("DO NOT emit any sub_dispatch nodes.\n")
		return
	}
	if len(skill.DeclaredSubSkills) == 0 {
		return
	}
	sb.WriteString("\n== Available sub-skills ==\n")
	sb.WriteString("Use ONLY these matrix://skill URIs in NodeSubDispatch.skill_ref:\n")
	subs := append([]string(nil), skill.DeclaredSubSkills...)
	sort.Strings(subs)
	for _, uri := range subs {
		sb.WriteString("  - " + uri + "\n")
	}
}

func walkPlanRec(n *ir.PlanNode, fn func(*ir.PlanNode)) {
	if n == nil {
		return
	}
	fn(n)
	for i := range n.Children {
		walkPlanRec(&n.Children[i], fn)
	}
}

func countPlanNodes(n *ir.PlanNode) int {
	if n == nil {
		return 0
	}
	c := 1
	for i := range n.Children {
		c += countPlanNodes(&n.Children[i])
	}
	return c
}

func countManifestTools(m *tool.AgentManifest) int {
	n := 0
	for _, s := range m.Servers {
		n += len(s.Tools)
	}
	return n
}

func countRounds(maxRetry int, lastErr error, plan *ir.PlanTree) int {
	if plan == nil {
		return maxRetry + 1
	}
	// Successful synthesis: rounds = (failures observed) + 1.
	return 1 // not perfectly tracked; informational only
}

// extractPlanJSON pulls the plan_tree@1 JSON object out of a raw planner
// response. Frontier/reasoning planner models often wrap the JSON in
// chain-of-thought prose, <think> blocks, or markdown fences, so a naive
// json.Unmarshal of the whole string fails on the first prose character
// (the 'invalid character' synth.parse.error class). This drops inline
// reasoning tag blocks, strips fences, then returns the first balanced
// top-level {...} object (brace-counted, string/escape aware). When no
// balanced object is found it returns the input from the first '{' (or the
// cleaned string) so the caller's unmarshal still surfaces a useful error.
func extractPlanJSON(raw string) string {
	clean, _ := splitReasoning(raw)
	clean = strings.TrimSpace(stripCodeFences(clean))
	start := strings.IndexByte(clean, '{')
	if start < 0 {
		return clean
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(clean); i++ {
		c := clean[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return clean[start : i+1]
			}
		}
	}
	// Unbalanced (e.g. truncated output): return from the first brace so
	// the unmarshal error reflects the JSON, not the leading prose.
	return clean[start:]
}

// stripCodeFences trims ```json ... ``` wrappers the model may add.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop opening fence (may have "json" hint).
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return s
	}
	s = s[i+1:]
	// Drop closing fence.
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// extractStepKindHints walks the SKILL.mtx AST and returns a map
// verb -> step.kind for every top-level on-block in §PROCEDURE that
// declares `kind = "<value>"`. Used by the planner system prompt
// (Session 31b model router · P2b) so the LLM emits the correct
// step.kind in plan_tree@1 without having to scan the whole .mtx
// source. Returns nil when the skill has no .mtx bytes (defensive)
// or no kind annotations.
//
// Best-effort: parser errors are silently ignored — the synthesizer
// also receives the full SKILL.md body as fallback context, and the
// planner can still emit a valid plan without explicit hints.
func extractStepKindHints(mtxBytes []byte) map[string]string {
	hints, _ := extractOnBlockHints(mtxBytes)
	return hints
}

// extractOutputCardinalityHints returns a map verb -> N (positive int)
// for every top-level on-block in §PROCEDURE that declares an
// `output_cardinality = <int>` KV. nil when no annotations exist.
// Session 31c · P3c.
func extractOutputCardinalityHints(mtxBytes []byte) map[string]int {
	_, cards := extractOnBlockHints(mtxBytes)
	return cards
}

// extractOnBlockHints walks the SKILL.mtx AST once and harvests both
// flavours of on-block hint metadata in a single pass. Cheaper than
// two separate walks; the synthesizer always wants both (and any
// future hint kinds will fold in here).
func extractOnBlockHints(mtxBytes []byte) (map[string]string, map[string]int) {
	if len(mtxBytes) == 0 {
		return nil, nil
	}
	file, _ := parser.New(mtxBytes).Parse()
	if file == nil {
		return nil, nil
	}
	var proc *ast.Section
	for _, sec := range file.Sections {
		if sec.Name == "PROCEDURE" {
			proc = sec
			break
		}
	}
	if proc == nil {
		return nil, nil
	}
	kinds := map[string]string{}
	cards := map[string]int{}
	for _, entry := range proc.Entries {
		ob, ok := entry.(*ast.OnBlock)
		if !ok {
			continue
		}
		vc, ok := ob.Condition.(*ast.VerbCondition)
		if !ok {
			continue
		}
		for _, sub := range ob.Entries {
			kv, ok := sub.(*ast.KVPair)
			if !ok || len(kv.Key) != 1 {
				continue
			}
			switch kv.Key[0] {
			case "kind":
				if v := interpreter.ExtractKindValue(kv.Value); v != "" {
					kinds[vc.Verb] = v
				}
			case "output_cardinality":
				if n, ok := interpreter.ExtractPositiveIntValue(kv.Value); ok {
					cards[vc.Verb] = n
				}
			}
		}
	}
	if len(kinds) == 0 {
		kinds = nil
	}
	if len(cards) == 0 {
		cards = nil
	}
	return kinds, cards
}

// writeStepKindHintSection emits the "== Step routing hints ==" block
// in the planner system prompt when the skill carries on-block kind
// annotations. The output is deterministic (sorted by verb) so the
// prompt hashes stably under D11.
func writeStepKindHintSection(sb *strings.Builder, skill *runtime.LoadedSkill) {
	kinds, cards := extractOnBlockHints(skill.MtxBytes)
	if len(kinds) == 0 && len(cards) == 0 {
		return
	}
	sb.WriteString("\n== Step routing hints (from SKILL.mtx on-block metadata) ==\n")

	if len(kinds) > 0 {
		sb.WriteString("When emitting NodeStep entries that belong to one of the verb branches\n")
		sb.WriteString("below, set step.kind to the listed value verbatim:\n")
		verbs := make([]string, 0, len(kinds))
		for v := range kinds {
			verbs = append(verbs, v)
		}
		sort.Strings(verbs)
		for _, v := range verbs {
			sb.WriteString(fmt.Sprintf("  - verb=%s -> step.kind=%q\n", v, kinds[v]))
		}
	}

	if len(cards) > 0 {
		sb.WriteString("\nOutput-cardinality hints (skill declares N independent items per\n")
		sb.WriteString("invocation; emit a single multi-output NodeStep that produces all N\n")
		sb.WriteString("items in one decode, OR a NodeParallel with N child NodeSteps when\n")
		sb.WriteString("the items must be generated independently — do NOT emit N separate\n")
		sb.WriteString("sequential NodeSteps):\n")
		verbs := make([]string, 0, len(cards))
		for v := range cards {
			verbs = append(verbs, v)
		}
		sort.Strings(verbs)
		for _, v := range verbs {
			sb.WriteString(fmt.Sprintf("  - verb=%s -> output_cardinality=%d\n", v, cards[v]))
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
