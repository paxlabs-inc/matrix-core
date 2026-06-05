// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package ir defines the Go types for the Intent IR — the central type in Matrix.
//
// The Intent IR is what the user signs. It carries the typed source-of-truth
// (Frame) alongside structured gaps (Unknowns) and grounding references.
// Everything downstream operates on this type, never on raw prose.
//
// Schema source: research/02-protocol.md §6.
// Encoding: canonical JSON with sorted keys for deterministic hashing (D11).
// CBOR codec can be layered via encoding interface when cortex dep is wired.
package ir

// Intent is the central type in Matrix. Every interaction produces one.
type Intent struct {
	ID      string `json:"id"`               // ULID
	Version string `json:"v"`                // schema version, "mcl/0.1"
	Parent  string `json:"parent,omitempty"` // parent IntentRef for sub-intents
	Actor   string `json:"actor"`            // who wants this (UserRef or AgentRef)
	Agent   string `json:"agent"`            // who will execute (AgentRef)

	// Human surface
	Prose string `json:"prose"` // original NL goal (display only)

	// Typed surface — the source of truth
	Frame Frame `json:"frame"`

	// Gaps
	Unknowns []Unknown `json:"unknowns,omitempty"`

	// Grounding
	References []Reference `json:"references,omitempty"`

	// Lifecycle
	State      string  `json:"state"`      // IntentState
	Confidence float64 `json:"confidence"` // 0..1
	Budget     *Budget `json:"budget,omitempty"`
	Deadline   string  `json:"deadline,omitempty"` // ISO8601
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  string  `json:"expires_at,omitempty"`

	// sess#32 ambient — links a sub-intent to its parent standing Goal so
	// per-goal cost telemetry, the scheduler escalation gate, and the
	// Goals.vue per-goal child-intent feed can attribute work to the right
	// Goal memory. Empty for chat-style one-shot intents (no parent Goal).
	// Backward-compatible: omitempty keeps pre-sess#32 IR JSON byte-identical.
	GoalID string `json:"goal_id,omitempty"`

	// Provenance
	SignedBy string `json:"signed_by"` // actor's public key
	Hash     string `json:"hash"`      // sha256 self-hash for content addressing

	// Compilation trace (D11)
	CompileMetadata *CompileMetadata `json:"compile_metadata,omitempty"`
}

// Frame is the typed source of truth — what gets signed and executed.
type Frame struct {
	Verb            string       `json:"verb"`                       // D7 closed vocab
	Objects         []SlotEntry  `json:"objects"`                    // typed referents
	Constraints     []Constraint `json:"constraints,omitempty"`      // §6.2
	SuccessCriteria []Predicate  `json:"success_criteria,omitempty"` // §6.3
	Preferences     []Preference `json:"preferences,omitempty"`      // soft tie-breakers
}

// SlotEntry is a named typed referent in the Frame's objects list.
type SlotEntry struct {
	Name  string `json:"name"`           // slot name
	Value string `json:"value"`          // resolved value or NL text
	URI   string `json:"uri,omitempty"`  // resolved matrix:// URI (post D13)
	Type  string `json:"type,omitempty"` // type annotation
}

// Constraint is a typed predicate that must hold throughout execution.
// §6.2: budget, deadline, jurisdiction, quality, rule, policy, x:custom.
type Constraint struct {
	Type string `json:"type"` // "budget", "deadline", "jurisdiction", "quality", "rule", "policy", "x:*"
	Hard bool   `json:"hard"` // hard constraints fail the intent if violated

	// Type-specific fields (only the relevant ones are populated)
	Max    *AssetAmount `json:"max,omitempty"`    // budget
	By     string       `json:"by,omitempty"`     // deadline (ISO8601)
	Allow  []string     `json:"allow,omitempty"`  // jurisdiction
	Deny   []string     `json:"deny,omitempty"`   // jurisdiction
	Metric string       `json:"metric,omitempty"` // quality
	Min    float64      `json:"min,omitempty"`    // quality
	Rule   string       `json:"rule,omitempty"`   // rule (RuleRef)
	Policy string       `json:"policy,omitempty"` // policy (Argus)
	Schema string       `json:"schema,omitempty"` // x:custom
	Data   string       `json:"data,omitempty"`   // x:custom (opaque JSON)
}

// Predicate is a checkable criterion that determines completion (§6.3).
type Predicate struct {
	Type     string `json:"type"`               // "delivered", "signed_off", "external", "attestation", "x:*"
	Artifact string `json:"artifact,omitempty"` // delivered
	By       string `json:"by,omitempty"`       // signed_off (UserRef)
	URL      string `json:"url,omitempty"`      // external
	Check    string `json:"check,omitempty"`    // external
	Source   string `json:"source,omitempty"`   // attestation (AgentRef)
	Topic    string `json:"topic,omitempty"`    // attestation
	Schema   string `json:"schema,omitempty"`   // x:custom
	Data     string `json:"data,omitempty"`     // x:custom
}

// Preference is a soft tie-breaker that does not fail the intent if violated.
type Preference struct {
	Rank   string   `json:"rank"`             // preference dimension
	Prefer []string `json:"prefer,omitempty"` // ordered preferences
}

// Unknown is a typed gap blocking or delaying execution (§6.4).
type Unknown struct {
	ID         string   `json:"id"`                    // local id ("u1", "u2")
	Field      string   `json:"field"`                 // SlotPath ("frame.constraints[0].max")
	Type       string   `json:"type"`                  // expected type
	Severity   string   `json:"severity"`              // "blocking", "preferred", "optional"
	Rationale  string   `json:"rationale"`             // human-readable why
	Default    string   `json:"default,omitempty"`     // suggested fill
	Options    []string `json:"options,omitempty"`     // enum-like choices
	SourceHint string   `json:"source_hint,omitempty"` // cortex location that might fill this
}

// Reference is a grounding matrix:// URI the agent must respect (D13).
type Reference struct {
	URI     string `json:"uri"`               // matrix:// URI with version
	Type    string `json:"type,omitempty"`    // cortex memory type
	Role    string `json:"role,omitempty"`    // how this reference is used
	Summary string `json:"summary,omitempty"` // medium-form summary
}

// Budget is an optional cap on execution resources.
type Budget struct {
	MaxCost   *AssetAmount `json:"max_cost,omitempty"`
	MaxTime   string       `json:"max_time,omitempty"` // duration
	MaxCalls  int          `json:"max_calls,omitempty"`
	MaxAgents int          `json:"max_agents,omitempty"`
}

// AssetAmount is a typed amount of an asset.
type AssetAmount struct {
	Asset  string  `json:"asset"` // "PAX", "USD", etc.
	Amount float64 `json:"amount"`
}

// CompileMetadata records the compilation trace for D11 replay-verification.
type CompileMetadata struct {
	Seed               string  `json:"seed"`          // sha256 of (intent.id || actor || snapshot_hash || mtx_digest || model_digest)
	MtxDigest          string  `json:"mtx_digest"`    // sha256 of canonical SKILL.mtx + core/*.mtx ASTs
	ModelDigest        string  `json:"model_digest"`  // digest of the compiler model
	ModelVersion       string  `json:"model_version"` // model identifier
	Temperature        float64 `json:"temperature"`
	Grammar            string  `json:"grammar"`  // grammar constraint used (e.g. "intent_frame@1")
	SkillID            string  `json:"skill_id"` // which skill was selected
	SkillVersion       string  `json:"skill_version"`
	CortexSnapshotHash string  `json:"cortex_snapshot_hash"` // Merkle root at compile time
}

// IntentState constants matching the lifecycle state machine (§7).
const (
	StateDraft      = "draft"
	StateProposed   = "proposed"
	StateClarifying = "clarifying"
	StateAccepted   = "accepted"
	StateExecuting  = "executing"
	StateCompleted  = "completed"
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
)

// Severity constants for Unknown.
const (
	SeverityBlocking  = "blocking"
	SeverityPreferred = "preferred"
	SeverityOptional  = "optional"
)

// Verb constants (D7 closed vocab).
const (
	VerbFind      = "find"
	VerbAcquire   = "acquire"
	VerbBuild     = "build"
	VerbModify    = "modify"
	VerbDeliver   = "deliver"
	VerbAnalyze   = "analyze"
	VerbNegotiate = "negotiate"
	VerbSchedule  = "schedule"
	VerbMonitor   = "monitor"
	VerbDelegate  = "delegate"
)

// D7ClosedVerbs is the canonical set of 10 verbs.
var D7ClosedVerbs = map[string]bool{
	VerbFind: true, VerbAcquire: true, VerbBuild: true, VerbModify: true,
	VerbDeliver: true, VerbAnalyze: true, VerbNegotiate: true, VerbSchedule: true,
	VerbMonitor: true, VerbDelegate: true,
}

// ValidVerb returns true if v is a valid D7 verb or x: extension.
func ValidVerb(v string) bool {
	if D7ClosedVerbs[v] {
		return true
	}
	if len(v) > 2 && v[:2] == "x:" {
		return true
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
