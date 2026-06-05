// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// PlanTree IR — the typed plan produced by skills running under the
// executor model. The compiler picks WHICH skill writes the plan; the
// plan itself is executor work (research/06-agents.md §5.2).
//
// Spec: research/05-skills-and-tools.md §3.2 (SkillOutput "plan.proposed"),
// research/02-protocol.md §5 (plan.proposed message kind), §11.1 (sub-dispatch
// crosses an Intent boundary), research/06-agents.md §5.2 (plan tree generation
// lives inside skills which run under the executor).
//
// Encoding: canonical JSON with sorted keys (mirrors Intent encoding in
// encode.go) for D11 hashing. On-wire CBOR is layered in MCL/envelope
// when the plan ships inside a plan.proposed envelope.
//
// Design locks (Session 21 Q1-Q2 in matrix.kvx phase21_locked_design):
//
//	Q2 PlanTree lives in MCL/ir/ alongside Intent — shared type between
//	   producer (skill running under executor) and consumer (executor
//	   walker + auditor + replay tooling). NOT executor-private.
//
//	PlanNode is a discriminated union by Kind; each kind populates a
//	different subset of typed fields. Same posture as Constraint /
//	Predicate / Unknown in intent.go — keeps codec deterministic.
package ir

// PlanTree is the typed plan structure attached to a plan.proposed
// envelope body, OR carried inline in Intent.frame.objects as a
// PlanDraft slot value when a skill emits one at compile-time refinement.
//
// A PlanTree is a directed acyclic graph (in practice a tree with
// occasional shared leaves for citations) rooted at a single Root node.
// Walks are depth-first by default; parallel branches are explicit
// (PlanNode.Kind == NodeParallel).
type PlanTree struct {
	// ID is a ULID identifying this plan. Used by intent.correct to
	// reference specific plan-tree revisions.
	ID string `json:"id"`

	// Version pins the PlanTree schema. v1 = "mcl/0.1".
	Version string `json:"v"`

	// IntentID back-references the Intent this plan executes.
	IntentID string `json:"intent_id"`

	// CreatedAt is the wall-clock at plan emission. ISO-8601.
	CreatedAt string `json:"created_at"`

	// CreatedBy is the agent that produced the plan (matrix://agent/<did>).
	CreatedBy string `json:"created_by"`

	// SkillRef is the version-pinned skill URI that authored this plan
	// (matrix://skill/<name>@<version>). Required so the executor can
	// validate the skill manifest matches at execution time.
	SkillRef string `json:"skill_ref"`

	// ModelDigest is the executor model's digest at plan-emission time.
	// Recorded for audit (not for D11 — executor-side determinism is
	// best-effort per A9).
	ModelDigest string `json:"model_digest,omitempty"`

	// Root is the entry point of the plan walk.
	Root PlanNode `json:"root"`

	// Budget caps execution resources (mirrors Intent.Budget but may
	// narrow it; the executor enforces the tighter of the two).
	Budget *Budget `json:"budget,omitempty"`

	// Hash is the sha256 self-hash of canonical JSON encoding with
	// this field cleared. Content-addresses the plan.
	Hash string `json:"hash"`
}

// PlanNode is a single node in the plan tree. Kind discriminates which
// of the typed payload fields are populated. Unpopulated fields are
// omitted from canonical encoding.
type PlanNode struct {
	// ID is a stable identifier within the PlanTree (e.g. "n1", "n2").
	// Referenced by intent.correct patches and intent.progress events.
	ID string `json:"id"`

	// Kind discriminates the node payload.
	Kind string `json:"kind"`

	// Description is a short human-readable label (for UI / journal).
	// Not authoritative; the typed payload fields are the source of truth.
	Description string `json:"description,omitempty"`

	// Children is the ordered list of child nodes. For Kind=NodeSequential
	// children run in order; for Kind=NodeParallel they run concurrently;
	// for terminal kinds (NodeStep, NodeToolCall, NodeSubDispatch, NodeGate)
	// Children is empty.
	Children []PlanNode `json:"children,omitempty"`

	// --- terminal-kind payloads (exactly one is populated based on Kind) ---

	// Step is populated when Kind==NodeStep. An in-skill LLM prompt:
	// the executor LLM is invoked with the skill's prompt template and
	// the cortex bundle, then advances skill state.
	Step *StepPayload `json:"step,omitempty"`

	// ToolCall is populated when Kind==NodeToolCall.
	ToolCall *ToolCallPayload `json:"tool_call,omitempty"`

	// SubDispatch is populated when Kind==NodeSubDispatch.
	SubDispatch *SubDispatchPayload `json:"sub_dispatch,omitempty"`

	// Gate is populated when Kind==NodeGate (human-in-loop policy gate
	// per research/02-protocol.md §14).
	Gate *GatePayload `json:"gate,omitempty"`

	// ResultText is a transient, runtime-only record of this node's
	// produced output text — the tool result text for NodeToolCall, or
	// the LLM step text for NodeStep. The walker populates it during
	// execution so a later NodeStep can resolve ${<nodeID>.output}
	// references in its Step.Inputs against the actual upstream output
	// (e.g. a report step that consumes a prior fleet_summary tool call).
	//
	// Excluded from canonical JSON (json:"-") so it never enters the D11
	// content hash, the signed plan envelope, or the byte-identical
	// replay invariant — it is pure in-memory walk state.
	ResultText string `json:"-"`
}

// StepPayload carries the parameters for an in-skill LLM prompt step.
type StepPayload struct {
	// PromptName names the prompt block inside the skill that this step
	// invokes (skills with multiple prompt blocks disambiguate by name).
	// Empty = the skill's default prompt for this verb branch.
	PromptName string `json:"prompt_name,omitempty"`

	// Inputs is a map of slot-name → slot-value bindings injected into
	// the prompt's interpolation context. Values are matrix:// URIs
	// (post D13) or literal scalars.
	Inputs map[string]string `json:"inputs,omitempty"`

	// ExpectedOutputs lists the slot names this step is expected to
	// populate in the skill's §OUTPUTS section. Used by the executor
	// to validate the LLM produced what was asked for.
	ExpectedOutputs []string `json:"expected_outputs,omitempty"`

	// Kind is the cognitive-shape hint that drives executor-tier model
	// routing (Session 31b · matrix.kvx sess#31a model router). Closed
	// enum ValidStepKinds; empty defaults to "reason" at routing time
	// (preserves backwards-compat for plans authored before P2). Skill
	// authors annotate per-on-block via a `kind = "<value>"` KV inside
	// the on-block body; the planner LLM is system-prompted to emit
	// the same value here so the executor StepHandler can resolve the
	// right model from the llm.ModelRegistry without a second lookup.
	//
	// Canonical JSON: omitempty preserves the byte-identical replay
	// invariant for the 159 bulk-converted SKILL.mtx fixtures + every
	// existing plan tree that never set Kind.
	Kind string `json:"kind,omitempty"`
}

// ToolCallPayload carries the parameters for a single tool invocation.
type ToolCallPayload struct {
	// ToolRef is the version-pinned tool URI
	// (matrix://tool/mcp/<server>/<name>@<version> for MCP-backed,
	// matrix://tool/<ns>/<name>@<version> for native chain tools).
	// Bare-head refs are rejected at PlanTree validation (S4 hard rule).
	ToolRef string `json:"tool_ref"`

	// Args is the typed argument map for the tool. Schema-validated
	// against the tool's manifest at invocation time. Sensitive values
	// (credentials, tokens) MUST be passed via env-var refs rather than
	// inline so they don't leak into the plan tree's content address.
	Args map[string]string `json:"args,omitempty"`

	// Timeout is the per-call timeout in milliseconds. Zero defers to
	// the tool's manifest default.
	TimeoutMs int `json:"timeout_ms,omitempty"`

	// SideEffectClass declares what kind of side-effect this call has.
	// Closed enum: "read", "write", "network", "shell", "chain".
	// Cross-checked against the agent's allowed side-effect set at the
	// executor's capability gate before dispatch.
	SideEffectClass string `json:"side_effect_class,omitempty"`
}

// SubDispatchPayload carries the parameters for a sub-skill or sub-agent
// dispatch (research/02-protocol.md §11, research/05-skills-and-tools.md §5.1).
type SubDispatchPayload struct {
	// SkillRef is the version-pinned sub-skill URI. The parent skill
	// must declare this in its §SUB_SKILLS section (S6 hard rule).
	SkillRef string `json:"skill_ref"`

	// AgentRef is the target agent for cross-agent dispatch. Empty =
	// in-process dispatch under the same agent (v1 default per Q6 lock).
	AgentRef string `json:"agent_ref,omitempty"`

	// SubIntent is the Frame for the sub-intent. Typed surface only;
	// the executor wraps this in a full Intent with derived metadata
	// (id, parent, actor, snapshot_hash) at dispatch time.
	SubIntent *Frame `json:"sub_intent,omitempty"`

	// ScopeURI references the CortexScope grant for cross-agent reads
	// (matrix://scope/<id>). Empty for in-process dispatch.
	ScopeURI string `json:"scope_uri,omitempty"`
}

// GatePayload carries the parameters for a human-in-loop policy gate.
type GatePayload struct {
	// RuleRef identifies the rule that triggered the gate
	// (matrix://rule/<id>). Used for journal traceability.
	RuleRef string `json:"rule_ref,omitempty"`

	// Question is the human-readable prompt shown at the gate.
	Question string `json:"question"`

	// Options are the allowed answers. Empty = open text answer.
	Options []string `json:"options,omitempty"`

	// TimeoutMs is how long the executor waits for policy.gate.resolve
	// before treating the gate as denied. Zero = no timeout (block forever).
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// Plan node kinds (closed enum).
const (
	// NodeSequential runs Children in order; any child failure halts the
	// sequence and propagates failure to the parent (default semantics).
	NodeSequential = "sequential"

	// NodeParallel runs Children concurrently; all children must succeed
	// for the parent to succeed. First failure cancels siblings.
	NodeParallel = "parallel"

	// NodeStep is an in-skill LLM prompt step. Populates Step.
	NodeStep = "step"

	// NodeToolCall is a single tool invocation. Populates ToolCall.
	NodeToolCall = "tool_call"

	// NodeSubDispatch is a sub-skill or sub-agent dispatch. Populates SubDispatch.
	NodeSubDispatch = "sub_dispatch"

	// NodeGate is a human-in-loop policy gate. Populates Gate.
	NodeGate = "gate"
)

// ValidNodeKinds is the canonical closed set of node kinds.
var ValidNodeKinds = map[string]bool{
	NodeSequential:  true,
	NodeParallel:    true,
	NodeStep:        true,
	NodeToolCall:    true,
	NodeSubDispatch: true,
	NodeGate:        true,
}

// ValidSideEffectClasses is the closed enum for ToolCallPayload.SideEffectClass.
// Closed set keeps the executor's capability gate exhaustive.
var ValidSideEffectClasses = map[string]bool{
	"read":    true, // pure read: cortex query, fs read, http GET
	"write":   true, // local mutation: fs write, cortex write
	"network": true, // outbound network: http POST, fetch
	"shell":   true, // process execution: shell tool, git, build
	"chain":   true, // chain interaction: Paxeer tx (v1.1)
}

// StepKindNames is the canonical ordered list of valid StepPayload.Kind
// values (Session 31b). Mirrors llm.AllStepKindNames; the executor's
// step_handler_test guards against drift between the two definitions.
// IR owns the wire-form list because the kind ships in canonical JSON
// (replay invariant) before any llm-package code sees it.
var StepKindNames = []string{
	"reason",      // default agentic step (GLM-5.1 in DefaultRegistry)
	"code",        // code generation specialist
	"summarize",   // long-context summarization specialist
	"write",       // free-form prose specialist
	"transform",   // structured-in, structured-out, deterministic
	"classify",    // pick-from-list with grammar
	"hard_reason", // opt-in frontier reasoning (expensive)
}

// ValidStepKinds is the closed enum for StepPayload.Kind. Empty Kind
// is allowed (and treated as "reason" at routing time) — only an
// explicit unknown value is rejected by ValidatePlan.
var ValidStepKinds = func() map[string]bool {
	m := make(map[string]bool, len(StepKindNames))
	for _, k := range StepKindNames {
		m[k] = true
	}
	return m
}()

// Copyright © 2026 Paxlabs Inc. All rights reserved.
