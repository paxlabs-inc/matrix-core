// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

// MCL/llm/model.go — Matrix Model Router (Session 31a, 2026-05-27).
//
// Replaces the original 2-slot {Compiler, Executor} design with a 3-tier
// router plus step-kind sub-routing for the executor. Architecture and
// rationale: canvases/model-router-architecture.canvas.tsx Section 2+3.
//
// Tiers:
//   SlotCompiler  — verb classify + frame extract (small, grammar, seeded)
//   SlotPlanner   — plan_tree@1 synthesis      (medium, grammar, recursive $defs)
//   SlotExecutor  — step decode + gate gen     (agentic, kind-routed)
//
// Executor sub-routes (StepKind):
//   reason       — GLM-5.1     (default; long-horizon agentic loops)
//   code         — Qwen3-Coder-480B
//   summarize    — DeepSeek-V4-Flash (1M context)
//   write        — Kimi K2.6
//   transform    — gpt-oss-20b
//   classify     — gpt-oss-20b (with grammar)
//   hard_reason  — DeepSeek-V4-Pro (opt-in)
//
// Backwards-compat: DefaultCompilerModel + DefaultExecutorModel are preserved
// as thin shims over DefaultRegistry().Resolve(...) so legacy call sites at
// MCL/cmd/mclc, bridge/cmd/mclc-cortex, executor/cmd/mcl-execute,
// executor/cmd/mcl-e2e keep working without churn.

import "strings"

// ----------------------------------------------------------------------------
// ModelSlot — coarse routing tier
// ----------------------------------------------------------------------------

// ModelSlot identifies which routing tier a call belongs to.
type ModelSlot int

const (
	// SlotCompiler is the small/seedable/grammar-constrained model.
	// Used for verb classification + intent frame extraction. Sub-second
	// target. Determinism (D11) required.
	SlotCompiler ModelSlot = iota

	// SlotPlanner is the medium/grammar-aware model used for plan_tree@1
	// synthesis. Recursive $defs/plan_node schema; needs reliable JSON
	// shape output. Was previously folded under SlotExecutor.
	SlotPlanner

	// SlotExecutor is the agentic model for step decode + gate generation.
	// Free-form output by default; step-kind sub-routing (see StepKind)
	// selects a specialist per call.
	SlotExecutor

	// SlotLiaison is the user-facing conversational agent ("the Liaison").
	// It narrates the compiler/planner/executor pipeline to the human in
	// natural language, fields chat replies + clarifications, and composes
	// the final answer. It is a pure observability SIDE-CHANNEL: it never
	// writes cortex, signs envelopes, or touches the plan/walk, so it
	// cannot perturb the D11 replay byte-identity invariant. Free-form
	// prose output (GrammarNone); warmth over determinism.
	SlotLiaison
)

func (s ModelSlot) String() string {
	switch s {
	case SlotCompiler:
		return "compiler"
	case SlotPlanner:
		return "planner"
	case SlotExecutor:
		return "executor"
	case SlotLiaison:
		return "liaison"
	}
	return "unknown"
}

// ----------------------------------------------------------------------------
// StepKind — executor sub-routing
// ----------------------------------------------------------------------------

// StepKind identifies the cognitive shape of an executor step. Only
// meaningful when ModelSlot == SlotExecutor; ignored otherwise.
type StepKind int

const (
	// KindUnspecified is the zero value. Treated as KindReason during
	// routing. Used by older plans + bulk-converted skills that don't
	// declare a kind explicitly.
	KindUnspecified StepKind = iota

	// KindReason is the default agentic step decode. GLM-5.1 default.
	KindReason

	// KindCode signals code generation. Qwen3-Coder-480B specialist.
	KindCode

	// KindSummarize signals input > 50k tokens or summarization/extraction.
	// DeepSeek-V4-Flash (1M context) specialist.
	KindSummarize

	// KindWrite signals free-form prose / creative copy. Kimi K2.6 specialist.
	KindWrite

	// KindTransform signals structured input -> structured output, no
	// creative leap. gpt-oss-20b specialist.
	KindTransform

	// KindClassify signals pick-from-list or boolean output with grammar.
	// gpt-oss-20b + grammar specialist.
	KindClassify

	// KindHardReason is opt-in only via SKILL.mtx flag. Reserved for
	// hardest reasoning tasks (formal proofs, adversarial debugging).
	// DeepSeek-V4-Pro specialist. Expensive + slow.
	KindHardReason
)

// AllStepKinds is the canonical ordered list of executor step kinds.
// Used by grammar enums + audit. Excludes KindUnspecified (synthetic).
var AllStepKinds = []StepKind{
	KindReason, KindCode, KindSummarize, KindWrite,
	KindTransform, KindClassify, KindHardReason,
}

// AllStepKindNames is the wire-form ordered list for plan_tree@1
// grammar enum + audit. Excludes "unspecified" (synthetic).
var AllStepKindNames = []string{
	"reason", "code", "summarize", "write",
	"transform", "classify", "hard_reason",
}

// String returns the wire-form name for a StepKind. KindUnspecified
// returns "reason" because routing normalizes Unspecified -> Reason.
func (k StepKind) String() string {
	switch k {
	case KindReason, KindUnspecified:
		return "reason"
	case KindCode:
		return "code"
	case KindSummarize:
		return "summarize"
	case KindWrite:
		return "write"
	case KindTransform:
		return "transform"
	case KindClassify:
		return "classify"
	case KindHardReason:
		return "hard_reason"
	}
	return "reason"
}

// ParseStepKind converts a wire-form string to StepKind. Empty + unknown
// values both map to KindUnspecified (which routes as KindReason). Case-
// insensitive; trims whitespace.
func ParseStepKind(s string) StepKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "reason":
		return KindReason
	case "code":
		return KindCode
	case "summarize":
		return KindSummarize
	case "write":
		return KindWrite
	case "transform":
		return KindTransform
	case "classify":
		return KindClassify
	case "hard_reason":
		return KindHardReason
	}
	return KindUnspecified
}

// ValidStepKindName reports whether s is one of the closed wire-form
// step-kind names. Used by plan validators + SKILL.mtx validators to
// reject typos at the boundary instead of silently dropping to default.
func ValidStepKindName(s string) bool {
	if s == "" {
		return true // empty defaults to "reason" silently (backwards compat)
	}
	for _, k := range AllStepKindNames {
		if s == k {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// RouteKey + ModelRegistry — runtime policy
// ----------------------------------------------------------------------------

// RouteKey identifies which model should serve a particular call.
//
//   - Slot is required.
//   - Kind is ignored when Slot != SlotExecutor.
//   - LongCtx is an estimation hint (input > ~100k tokens). When true,
//     Resolve prefers the long-context variant of the route if one is
//     registered, falling back to the primary otherwise.
type RouteKey struct {
	Slot    ModelSlot
	Kind    StepKind
	LongCtx bool
}

// ModelRegistry resolves (Slot, Kind, LongCtx) -> Config. Runtime policy
// only — never persisted in cortex OverallRoot. Mirrors the sidecar
// posture of meta/salience_weights (Phase 12) and the rate-limit buckets
// (Phase 14).
type ModelRegistry struct {
	// routes maps (Slot, Kind) -> primary Config.
	routes map[RouteKey]Config

	// longCtxRoutes maps (Slot, Kind) -> LongCtx Config overrides.
	// Tracked separately so a normal lookup doesn't accidentally pick
	// the long-context variant.
	longCtxRoutes map[RouteKey]Config

	// fallback is the last-resort Config used when no route matches.
	// Always populated; ensures Resolve never returns a zero-value Config
	// that would fail at llm.New time.
	fallback Config
}

// NewModelRegistry returns an empty registry with the given fallback.
// Use Register to populate routes.
func NewModelRegistry(fallback Config) *ModelRegistry {
	return &ModelRegistry{
		routes:        make(map[RouteKey]Config),
		longCtxRoutes: make(map[RouteKey]Config),
		fallback:      fallback,
	}
}

// Register sets the Config for a route key. LongCtx routes are stored
// in a separate map so non-LongCtx lookups don't see them.
//
// Returns the registry for fluent chaining:
//
//	reg := NewModelRegistry(fallback).
//	    Register(RouteKey{Slot: SlotCompiler}, compilerCfg).
//	    Register(RouteKey{Slot: SlotPlanner},  plannerCfg)
func (r *ModelRegistry) Register(k RouteKey, cfg Config) *ModelRegistry {
	if r == nil {
		return r
	}
	bare := RouteKey{Slot: k.Slot, Kind: normalizeKind(k.Slot, k.Kind)}
	if k.LongCtx {
		r.longCtxRoutes[bare] = cfg
	} else {
		r.routes[bare] = cfg
	}
	return r
}

// Resolve returns the Config for a route key. Normalization rules:
//
//   - Kind is ignored (zeroed) when Slot != SlotExecutor.
//   - KindUnspecified on SlotExecutor normalizes to KindReason.
//   - LongCtx=true prefers the LongCtx variant; falls through to primary
//     when no LongCtx variant is registered.
//   - Unknown KindCode/Summarize/Write/etc. on SlotExecutor fall back to
//     KindReason (the executor default) before hitting the global fallback.
func (r *ModelRegistry) Resolve(k RouteKey) Config {
	if r == nil {
		return Config{}
	}
	bare := RouteKey{Slot: k.Slot, Kind: normalizeKind(k.Slot, k.Kind)}

	if k.LongCtx {
		if cfg, ok := r.longCtxRoutes[bare]; ok {
			return cfg
		}
		// Fall through to primary if no LongCtx variant is registered.
	}
	if cfg, ok := r.routes[bare]; ok {
		return cfg
	}

	// Executor fallback: unknown kind -> reason kind.
	if k.Slot == SlotExecutor && bare.Kind != KindReason {
		if cfg, ok := r.routes[RouteKey{Slot: SlotExecutor, Kind: KindReason}]; ok {
			return cfg
		}
	}
	return r.fallback
}

// Routes returns a snapshot of all registered routes (primary + LongCtx)
// for inspection + audit. The returned map's LongCtx keys carry
// LongCtx=true; primary keys carry LongCtx=false.
func (r *ModelRegistry) Routes() map[RouteKey]Config {
	if r == nil {
		return nil
	}
	out := make(map[RouteKey]Config, len(r.routes)+len(r.longCtxRoutes))
	for k, v := range r.routes {
		out[k] = v
	}
	for k, v := range r.longCtxRoutes {
		long := k
		long.LongCtx = true
		out[long] = v
	}
	return out
}

// Fallback returns the registry's fallback Config (read-only accessor).
func (r *ModelRegistry) Fallback() Config {
	if r == nil {
		return Config{}
	}
	return r.fallback
}

// normalizeKind enforces the rule that Kind is only meaningful for
// SlotExecutor. Other slots get Kind=0 (KindUnspecified). Executor +
// KindUnspecified normalizes to KindReason.
func normalizeKind(slot ModelSlot, kind StepKind) StepKind {
	if slot != SlotExecutor {
		return 0
	}
	if kind == KindUnspecified {
		return KindReason
	}
	return kind
}

// ----------------------------------------------------------------------------
// DefaultRegistry — v1 router populated per canvas Section 2 + 3
// ----------------------------------------------------------------------------

// DefaultRegistry returns the v1 router populated per the matrix.kvx
// Session 31a model router design. Models pinned to Fireworks for the
// primary path (matches existing infra + MCL/llm provider detection);
// Together is the alt path that the gateway selects when Fireworks 5xxes
// or exceeds per-tier latency budget.
//
// Provider-specific model ID conventions:
//
//	Fireworks: "accounts/fireworks/models/<name>"
//	Together:  "<vendor>/<name>"
func DefaultRegistry() *ModelRegistry {
	grammars := DefaultGrammars()

	// --- SlotCompiler --------------------------------------------------------
	compiler := Config{
		Model:       "accounts/fireworks/models/gpt-oss-20b",
		Temperature: 0,
		Seed:        42,
		MaxTokens:   512,
		GrammarMode: GrammarJSONSchema,
		Grammars:    grammars,
	}
	// LongCtx: when cortex bundle exceeds ~100k tokens, DeepSeek-V4-Flash's
	// 1M context is the safe escalation.
	compilerLongCtx := Config{
		Model:       "accounts/fireworks/models/deepseek-v4-flash",
		Temperature: 0,
		Seed:        42,
		MaxTokens:   1024,
		GrammarMode: GrammarJSONSchema,
		Grammars:    grammars,
	}

	// --- SlotPlanner ---------------------------------------------------------
	// MaxTokens is generous (8192) because the planner emits a full
	// plan_tree@1 JSON document and a frontier model may spend a large
	// share of the budget on reasoning before the JSON; a tight cap
	// truncates the plan mid-object (the 'unexpected end of JSON input'
	// synth.parse.error class). Plans rarely approach this ceiling.
	planner := Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Temperature: 0.2,
		Seed:        42,
		MaxTokens:   8192,
		GrammarMode: GrammarJSONSchema,
		Grammars:    grammars,
	}
	// LongCtx: GLM-5.1's 202k window for plans with extensive context.
	plannerLongCtx := Config{
		Model:       "accounts/fireworks/models/glm-5.1",
		Temperature: 0.2,
		Seed:        42,
		MaxTokens:   3072,
		GrammarMode: GrammarJSONSchema,
		Grammars:    grammars,
	}

	// --- SlotExecutor: KindReason (default agentic) --------------------------
	executorReason := Config{
		Model:       "accounts/fireworks/models/glm-5.1",
		Temperature: 0.4,
		MaxTokens:   1536,
		GrammarMode: GrammarNone,
	}

	// --- SlotExecutor: KindCode (code-gen specialist) ------------------------
	executorCode := Config{
		Model:       "Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8",
		Temperature: 0.2,
		MaxTokens:   4096,
		GrammarMode: GrammarNone,
	}

	// --- SlotExecutor: KindSummarize (long-context specialist) ---------------
	executorSummarize := Config{
		Model:       "accounts/fireworks/models/deepseek-v4-flash",
		Temperature: 0.2,
		MaxTokens:   2048,
		GrammarMode: GrammarNone,
	}

	// --- SlotExecutor: KindWrite (prose specialist) --------------------------
	executorWrite := Config{
		Model:       "accounts/fireworks/models/kimi-k2.6",
		Temperature: 0.6,
		MaxTokens:   2048,
		GrammarMode: GrammarNone,
	}

	// --- SlotExecutor: KindTransform (structured shape, no creativity) -------
	executorTransform := Config{
		Model:       "accounts/fireworks/models/gpt-oss-20b",
		Temperature: 0,
		Seed:        42,
		MaxTokens:   1024,
		GrammarMode: GrammarNone,
	}

	// --- SlotExecutor: KindClassify (pick-from-list with grammar) ------------
	executorClassify := Config{
		Model:       "accounts/fireworks/models/gpt-oss-20b",
		Temperature: 0,
		Seed:        42,
		MaxTokens:   64,
		GrammarMode: GrammarJSONSchema,
		Grammars:    grammars,
	}

	// --- SlotExecutor: KindHardReason (opt-in frontier) ----------------------
	executorHardReason := Config{
		Model:       "accounts/fireworks/models/deepseek-v4-pro",
		Temperature: 0.2,
		MaxTokens:   4096,
		GrammarMode: GrammarNone,
	}

	// --- SlotLiaison (user-facing conversational narrator) -------------------
	// DeepSeek-V4-Flash: fast + cheap + 1M context so it can fold a large
	// batch of technical pipeline events into one natural-language chat turn
	// without latency spikes. Free-form prose; no seed (natural variation is
	// fine — the Liaison is a side-channel and is never replayed).
	liaison := Config{
		Model:       "accounts/fireworks/models/deepseek-v4-flash",
		Temperature: 0.5,
		MaxTokens:   1024,
		GrammarMode: GrammarNone,
	}

	reg := NewModelRegistry(executorReason).
		Register(RouteKey{Slot: SlotCompiler}, compiler).
		Register(RouteKey{Slot: SlotCompiler, LongCtx: true}, compilerLongCtx).
		Register(RouteKey{Slot: SlotPlanner}, planner).
		Register(RouteKey{Slot: SlotPlanner, LongCtx: true}, plannerLongCtx).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindReason}, executorReason).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindCode}, executorCode).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindSummarize}, executorSummarize).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindWrite}, executorWrite).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindTransform}, executorTransform).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindClassify}, executorClassify).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindHardReason}, executorHardReason).
		Register(RouteKey{Slot: SlotLiaison}, liaison)

	return reg
}

// ----------------------------------------------------------------------------
// ForgeRegistry — local self-maintenance routing (Session 34 / Forge Phase 1)
// ----------------------------------------------------------------------------

// Opencode endpoint constants for the zen proxy that fronts Anthropic
// Messages + OpenAI Responses APIs behind a single OPENCODE_API_KEY. Source:
// matrix/temp/model.opencode catalog 2026-05-27.
const (
	OpencodeMessagesEndpoint  = "https://opencode.ai/zen/v1/messages"
	OpencodeResponsesEndpoint = "https://opencode.ai/zen/v1/responses"
	OpencodeChatEndpoint      = "https://opencode.ai/zen/v1/chat/completions"
)

// Forge model id constants. Edit these constants (not the routing logic) to
// upgrade or downgrade individual slots; the wiring stays valid.
const (
	ForgeModelClaudeOpus47 = "claude-opus-4-7"
	ForgeModelClaudeOpus46 = "claude-opus-4-6"
	ForgeModelClaudeOpus45 = "claude-opus-4-5"
	ForgeModelGPT55        = "gpt-5.5"
	ForgeModelGPT55Pro     = "gpt-5.5-pro"
	ForgeModelGPT54Mini    = "gpt-5.4-mini"
	ForgeModelGPT53Codex   = "gpt-5.3-codex"
	ForgeModelKimiK26      = "kimi-k2.6"
	ForgeModelGLM51        = "glm-5.1"
)

// ForgeRegistry returns the v1 Forge model router populated per matrix.kvx
// sess#34. Powers the local self-maintenance Matrix instance whose sole
// purpose is to optimize its own codebase at /root/matrix.
//
// Routing strategy:
//
//	SlotCompiler              gpt-5.5             (fast structured-output for verb classify + frame extract)
//	SlotPlanner               claude-opus-4-7     (strongest reasoning for plan_tree@1 synthesis)
//	SlotExecutor/reason       claude-opus-4-7     (agentic loops, code-context reasoning)
//	SlotExecutor/code         claude-opus-4-7     (code-gen at frontier quality)
//	SlotExecutor/summarize    gpt-5.5             (long-context summarisation)
//	SlotExecutor/write        claude-opus-4-7     (prose / docs)
//	SlotExecutor/transform    gpt-5.5             (structured shape, no creativity)
//	SlotExecutor/classify     gpt-5.5             (pick-from-list, fast)
//	SlotExecutor/hard_reason  claude-opus-4-7     (formal proofs, adversarial debugging)
//
// Every route has InjectIdentity=true so the Matrix-maintaining-Matrix
// identity preamble (llm.IdentityPreamble) ships on every routed call.
// ProviderSet=true so DetectProvider is skipped (bare opencode model ids
// like "claude-opus-4-7" and "gpt-5.5" don't fit the "<vendor>/<model>"
// shape).
//
// Caller pins OPENCODE_API_KEY in the daemon environment; New() reads it
// via envKey(ProviderOpencode).
func ForgeRegistry() *ModelRegistry {
	// Anthropic-shape model (claude-opus-4-7) → /v1/messages endpoint.
	//
	// Sess#36 fix (2026-05-28): claude-opus-4-7 deprecates the
	// `temperature` parameter — sending it returns
	//   `400 BAD_REQUEST: temperature is deprecated for this model.`
	// The model has its own internal sampling. We accept the temp arg
	// for API compatibility with the gpt55() builder below but DROP it
	// before constructing the Config so messages_api.go's `omitempty`
	// JSON tag elides the field on the wire.
	opus := func(_ float64, maxTok int) Config {
		return Config{
			Model:       ForgeModelClaudeOpus47,
			Endpoint:    OpencodeMessagesEndpoint,
			Provider:    ProviderOpencode,
			ProviderSet: true,
			Shape:       ShapeMessages,
			// Temperature intentionally omitted — opus 4.7 rejects it.
			MaxTokens:      maxTok,
			GrammarMode:    GrammarNone,
			InjectIdentity: true,
		}
	}

	// OpenAI-Responses-shape model (gpt-5.5) → /v1/responses endpoint
	gpt55 := func(temp float64, maxTok int) Config {
		return Config{
			Model:          ForgeModelGPT55,
			Endpoint:       OpencodeResponsesEndpoint,
			Provider:       ProviderOpencode,
			ProviderSet:    true,
			Shape:          ShapeResponses,
			Temperature:    temp,
			MaxTokens:      maxTok,
			GrammarMode:    GrammarNone,
			InjectIdentity: true,
		}
	}

	compiler := gpt55(0, 512)
	compiler.Seed = 42

	planner := opus(0.2, 4096)

	executorReason := opus(0.4, 4096)
	executorCode := opus(0.2, 8192)
	executorSummarize := gpt55(0.2, 4096)
	executorWrite := opus(0.6, 4096)
	executorTransform := gpt55(0, 1024)
	executorTransform.Seed = 42
	executorClassify := gpt55(0, 128)
	executorClassify.Seed = 42
	executorHardReason := opus(0.2, 8192)

	// SlotLiaison — conversational narrator on the Forge brain. gpt-5.5 is
	// fast + fluent for the chat-turn cadence; warmth over determinism.
	liaison := gpt55(0.5, 1024)

	reg := NewModelRegistry(executorReason).
		Register(RouteKey{Slot: SlotCompiler}, compiler).
		Register(RouteKey{Slot: SlotPlanner}, planner).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindReason}, executorReason).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindCode}, executorCode).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindSummarize}, executorSummarize).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindWrite}, executorWrite).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindTransform}, executorTransform).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindClassify}, executorClassify).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindHardReason}, executorHardReason).
		Register(RouteKey{Slot: SlotLiaison}, liaison)

	return reg
}

// ForgeCompilerModel returns the SlotCompiler Config from ForgeRegistry.
// Convenience shim for callers that don't need full router resolution.
func ForgeCompilerModel() Config {
	return ForgeRegistry().Resolve(RouteKey{Slot: SlotCompiler})
}

// ForgePlannerModel returns the SlotPlanner Config from ForgeRegistry.
func ForgePlannerModel() Config {
	return ForgeRegistry().Resolve(RouteKey{Slot: SlotPlanner})
}

// ForgeExecutorModel returns the SlotExecutor+KindReason Config from
// ForgeRegistry (the default executor route).
func ForgeExecutorModel() Config {
	return ForgeRegistry().Resolve(RouteKey{Slot: SlotExecutor, Kind: KindReason})
}

// ----------------------------------------------------------------------------
// Backwards-compat shims
// ----------------------------------------------------------------------------

// DefaultCompilerModel returns the Config for SlotCompiler from the
// default registry. Backwards-compatible shim. New call sites should
// use DefaultRegistry().Resolve(...) directly so per-call kind/LongCtx
// hints propagate.
func DefaultCompilerModel() Config {
	return DefaultRegistry().Resolve(RouteKey{Slot: SlotCompiler})
}

// DefaultPlannerModel returns the Config for SlotPlanner from the
// default registry. New in P1; previously plan_tree@1 synthesis ran
// against DefaultExecutorModel which conflated two semantically
// different tiers.
func DefaultPlannerModel() Config {
	return DefaultRegistry().Resolve(RouteKey{Slot: SlotPlanner})
}

// DefaultExecutorModel returns the Config for SlotExecutor + KindReason
// from the default registry. Backwards-compatible shim. New call sites
// that know their step kind (set by SKILL.mtx + propagated through
// plan_tree@1 in P2) should use DefaultRegistry().Resolve(...) directly.
func DefaultExecutorModel() Config {
	return DefaultRegistry().Resolve(RouteKey{Slot: SlotExecutor, Kind: KindReason})
}

// ----------------------------------------------------------------------------
// DefaultGrammars — built-in grammar definitions
// ----------------------------------------------------------------------------

// DefaultGrammars returns the built-in grammar definitions for the compiler
// and planner slots. The intent_frame@1 + verb_vocab@1 grammars enforce
// the D7 closed verb vocab + v1 closed obj_kind enum. plan_tree@1 mirrors
// MCL/ir/plan.go with recursive $defs/plan_node.
func DefaultGrammars() map[string]*GrammarDef {
	return map[string]*GrammarDef{
		"intent_frame@1": {
			Name: "intent_frame",
			JSONSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"verb": map[string]interface{}{
						"type": "string",
						"enum": []string{
							"find", "acquire", "build", "modify", "deliver",
							"analyze", "negotiate", "schedule", "monitor", "delegate",
						},
					},
					// confidence — the compiler's self-assessed certainty in this
					// frame extraction (0..1). REQUIRED so the grammar-constrained
					// decode always emits it; the executor reads it to escalate a
					// low-confidence compile to a stronger model (compile.go). Not
					// persisted on the Intent (buildIntent reads verb+objects only).
					"confidence": map[string]interface{}{
						"type":    "number",
						"minimum": 0,
						"maximum": 1,
					},
					"objects": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"kind": map[string]interface{}{
									"type": "string",
									"enum": []string{
										"service", "model", "agent", "knowledge",
										"intent", "asset", "plan", "capability",
									},
								},
								"ref":         map[string]interface{}{"type": "string"},
								"description": map[string]interface{}{"type": "string"},
							},
							"required": []string{"kind", "ref"},
						},
					},
					"constraints": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"type":      map[string]interface{}{"type": "string"},
								"statement": map[string]interface{}{"type": "string"},
								"strength":  map[string]interface{}{"type": "string", "enum": []string{"soft", "firm", "hard"}},
							},
							"required": []string{"statement", "strength"},
						},
					},
					"success_criteria": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"criterion":  map[string]interface{}{"type": "string"},
								"measurable": map[string]interface{}{"type": "boolean"},
							},
							"required": []string{"criterion"},
						},
					},
					"preferences": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"topic":    map[string]interface{}{"type": "string"},
								"polarity": map[string]interface{}{"type": "string", "enum": []string{"prefer", "avoid", "neutral"}},
								"strength": map[string]interface{}{"type": "number"},
							},
							"required": []string{"topic", "polarity"},
						},
					},
				},
				"required": []string{"verb", "objects", "confidence"},
			},
		},
		"verb_vocab@1": {
			Name: "verb_classifier",
			JSONSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"verb": map[string]interface{}{
						"type": "string",
						"enum": []string{
							"find", "acquire", "build", "modify", "deliver",
							"analyze", "negotiate", "schedule", "monitor", "delegate",
						},
					},
					"confidence": map[string]interface{}{
						"type":    "number",
						"minimum": 0,
						"maximum": 1,
					},
				},
				"required": []string{"verb", "confidence"},
			},
		},
		// plan_tree@1 — Session 23 S23Q13 lock + Session 31a kind extension.
		// Mirrors MCL/ir/plan.go: PlanTree + PlanNode with ValidNodeKinds +
		// ValidSideEffectClasses closed enums. Recursive via $defs/plan_node
		// (PlanNode.Children is itself an array of PlanNode). Providers
		// supporting OpenAI-compat JSON Schema with $defs (Fireworks +
		// Together both do as of 2026-05) honor this directly; callers
		// may opt out via GrammarNone if their provider chokes on recursion.
		//
		// P1 extension: step.kind enum added; defaults to "reason" when
		// omitted by the planner. Bulk-converted SKILL.mtx fixtures don't
		// emit it; new authoring annotates per on-block.
		"plan_tree@1": {
			Name: "plan_tree",
			JSONSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":           map[string]interface{}{"type": "string"},
					"v":            map[string]interface{}{"type": "string"},
					"intent_id":    map[string]interface{}{"type": "string"},
					"created_at":   map[string]interface{}{"type": "string"},
					"created_by":   map[string]interface{}{"type": "string"},
					"skill_ref":    map[string]interface{}{"type": "string"},
					"model_digest": map[string]interface{}{"type": "string"},
					"root":         map[string]interface{}{"$ref": "#/$defs/plan_node"},
					"hash":         map[string]interface{}{"type": "string"},
				},
				"required": []string{"id", "v", "intent_id", "skill_ref", "root"},
				"$defs": map[string]interface{}{
					"plan_node": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id":          map[string]interface{}{"type": "string"},
							"kind":        map[string]interface{}{"type": "string", "enum": []string{"sequential", "parallel", "step", "tool_call", "sub_dispatch", "gate"}},
							"description": map[string]interface{}{"type": "string"},
							"children": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"$ref": "#/$defs/plan_node"},
							},
							"step": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"prompt_name":      map[string]interface{}{"type": "string"},
									"inputs":           map[string]interface{}{"type": "object"},
									"expected_outputs": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
									"kind": map[string]interface{}{
										"type": "string",
										"enum": []string{
											"reason", "code", "summarize", "write",
											"transform", "classify", "hard_reason",
										},
									},
								},
							},
							"tool_call": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"tool_ref":          map[string]interface{}{"type": "string"},
									"args":              map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
									"timeout_ms":        map[string]interface{}{"type": "integer"},
									"side_effect_class": map[string]interface{}{"type": "string", "enum": []string{"read", "write", "network", "shell", "chain"}},
								},
								"required": []string{"tool_ref"},
							},
							"sub_dispatch": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"skill_ref": map[string]interface{}{"type": "string"},
									"agent_ref": map[string]interface{}{"type": "string"},
									"scope_uri": map[string]interface{}{"type": "string"},
								},
								"required": []string{"skill_ref"},
							},
							"gate": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"rule_ref":   map[string]interface{}{"type": "string"},
									"question":   map[string]interface{}{"type": "string"},
									"options":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
									"timeout_ms": map[string]interface{}{"type": "integer"},
								},
								"required": []string{"question"},
							},
						},
						"required": []string{"id", "kind"},
					},
				},
			},
		},
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
