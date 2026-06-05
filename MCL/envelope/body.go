// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package envelope

import (
	"fmt"
	"reflect"
)

// Typed body structs for all 15 message kinds. Each carries the
// kind-specific payload; field tags are CBOR integer keyasint for
// canonical wire encoding.
//
// Spec sources:
//   - research/02-protocol.md §5 (message kind list)
//   - research/02-protocol.md §6 (Intent IR shape)
//   - research/02-protocol.md §9 (clarification protocol)
//   - research/02-protocol.md §10 (correction protocol)
//   - research/02-protocol.md §11 (sub-dispatch)
//   - research/02-protocol.md §12 (attestation)
//   - research/02-protocol.md §13 (typed failure)
//   - research/02-protocol.md §14 (policy gates)
//
// Each body type embeds the minimum fields needed for that kind. v1
// keeps every body compact and additive; additional fields land at
// higher unused integer tags without a SchemaVersion bump.

// IntentDraftBody — initial NL goal from user.
type IntentDraftBody struct {
	// Prose is the original natural-language goal.
	Prose string `cbor:"0,keyasint" json:"prose"`

	// SlotValues are pre-filled slot bindings (form-fill from the UI).
	// Keyed by slot name from the matched skill's §INPUTS section.
	SlotValues map[string]string `cbor:"1,keyasint,omitempty" json:"slot_values,omitempty"`

	// PreferredSkill is an optional matrix://skill/... ref hint from
	// the user (e.g. user explicitly invoked /writing-plans).
	PreferredSkill string `cbor:"2,keyasint,omitempty" json:"preferred_skill,omitempty"`
}

// IntentCompiledBody — agent's typed Intent IR for user review.
// The Intent itself is carried as canonical JSON bytes (deterministic
// hash per D11) inside IntentJSON. We do NOT re-encode it in CBOR
// because the canonical JSON IS the IR's content address; re-encoding
// would lose that property.
type IntentCompiledBody struct {
	// IntentJSON is the canonical JSON encoding of the matrix/mcl/ir.Intent.
	// Receivers decode via ir.CanonicalJSON-equivalent path.
	IntentJSON []byte `cbor:"0,keyasint" json:"intent_json"`

	// CompileLatencyMs is the wall-clock cost of compilation.
	CompileLatencyMs int64 `cbor:"1,keyasint,omitempty" json:"compile_latency_ms,omitempty"`
}

// IntentClarifyBody — structured questions for unknowns.
type IntentClarifyBody struct {
	// Questions one per unmet unknown.
	Questions []ClarifyQuestion `cbor:"0,keyasint" json:"questions"`
}

// ClarifyQuestion is a single question targeting one Unknown.
type ClarifyQuestion struct {
	// UnknownID matches Intent.Unknowns[].ID ("u1", "u2"...).
	UnknownID string `cbor:"0,keyasint" json:"unknown_id"`

	// Field is the SlotPath the answer will patch.
	Field string `cbor:"1,keyasint" json:"field"`

	// Prompt is the user-facing question text.
	Prompt string `cbor:"2,keyasint" json:"prompt"`

	// Type is the expected answer type.
	Type string `cbor:"3,keyasint,omitempty" json:"type,omitempty"`

	// Required: if true, intent.answer MUST include this question.
	Required bool `cbor:"4,keyasint,omitempty" json:"required,omitempty"`

	// Options for enum-like questions.
	Options []string `cbor:"5,keyasint,omitempty" json:"options,omitempty"`

	// Default suggested fill.
	Default string `cbor:"6,keyasint,omitempty" json:"default,omitempty"`
}

// IntentAnswerBody — slot patches answering clarify questions.
// Wire form is RFC 6902 JSON Patch per D8; the typed authoring surface
// (SlotPatch) compiles to RFC 6902 in MCL/patch (separate package).
type IntentAnswerBody struct {
	// Patches is RFC 6902 JSON Patch bytes (array of {op,path,value} ops)
	// applied against the Intent IR.
	Patches []byte `cbor:"0,keyasint" json:"patches"`

	// AnswerOf is the correlation ID of the intent.clarify being answered.
	AnswerOf string `cbor:"1,keyasint" json:"answer_of"`
}

// IntentAcceptBody — user's signed sign-off on the IR.
// The Envelope's outer Signature IS the acceptance signature; the body
// just pins the IR hash for replay-binding.
type IntentAcceptBody struct {
	// IntentHash is the sha256 of canonical-JSON Intent at acceptance time.
	// Receivers verify this matches the local IR before acting.
	IntentHash string `cbor:"0,keyasint" json:"intent_hash"`

	// AcceptedAt is wall-clock ISO-8601 (mirrors header At; explicit for audit).
	AcceptedAt string `cbor:"1,keyasint" json:"accepted_at"`

	// AnchorRequested: did the user opt into chain-anchoring? D10 trigger.
	AnchorRequested bool `cbor:"2,keyasint,omitempty" json:"anchor_requested,omitempty"`
}

// PlanProposedBody — agent's decomposition into steps before execution.
// Carries a PlanTree as canonical JSON bytes (mirrors IntentCompiledBody
// posture — the canonical JSON IS the content address).
type PlanProposedBody struct {
	// PlanJSON is the canonical JSON encoding of ir.PlanTree.
	PlanJSON []byte `cbor:"0,keyasint" json:"plan_json"`
}

// PlanStepBody — single step execution envelope (executor-internal).
// Used for inter-component messaging within an executor; rarely user-visible.
type PlanStepBody struct {
	// PlanID identifies the parent PlanTree.
	PlanID string `cbor:"0,keyasint" json:"plan_id"`

	// NodeID identifies the PlanNode being executed.
	NodeID string `cbor:"1,keyasint" json:"node_id"`

	// Status is the lifecycle of this step: "started", "completed",
	// "failed", "cancelled". Closed enum.
	Status string `cbor:"2,keyasint" json:"status"`

	// Result is opaque step output (tool result, sub-intent ref, etc.)
	// encoded as JSON bytes for human-readable journaling.
	Result []byte `cbor:"3,keyasint,omitempty" json:"result,omitempty"`

	// Error is populated when Status == "failed".
	Error string `cbor:"4,keyasint,omitempty" json:"error,omitempty"`

	// LatencyMs is the wall-clock duration of the step.
	LatencyMs int64 `cbor:"5,keyasint,omitempty" json:"latency_ms,omitempty"`
}

// PlanOutputBody — streaming intermediate output. Multiple of these
// may share an Intent + PlanID + NodeID, distinguished by Sequence.
type PlanOutputBody struct {
	// PlanID identifies the parent PlanTree.
	PlanID string `cbor:"0,keyasint" json:"plan_id"`

	// NodeID identifies the emitting PlanNode.
	NodeID string `cbor:"1,keyasint" json:"node_id"`

	// Sequence is a monotonic counter within a (PlanID, NodeID) stream.
	Sequence uint64 `cbor:"2,keyasint" json:"sequence"`

	// Chunk is opaque output bytes (stdout/stderr/tool result fragment).
	Chunk []byte `cbor:"3,keyasint" json:"chunk"`

	// Channel labels the stream: "stdout", "stderr", "result", "progress".
	Channel string `cbor:"4,keyasint,omitempty" json:"channel,omitempty"`

	// Final marks the last chunk in the stream.
	Final bool `cbor:"5,keyasint,omitempty" json:"final,omitempty"`
}

// IntentCorrectBody — user patches an Intent or plan mid-flight.
type IntentCorrectBody struct {
	// Target is "intent" or "plan" — what's being patched.
	Target string `cbor:"0,keyasint" json:"target"`

	// Patches is RFC 6902 JSON Patch bytes.
	Patches []byte `cbor:"1,keyasint" json:"patches"`

	// Reason is a structured reason code for the correction (audit).
	Reason string `cbor:"2,keyasint,omitempty" json:"reason,omitempty"`

	// RetryFrom names the PlanNode.ID to resume from after applying.
	// Empty = restart plan from root.
	RetryFrom string `cbor:"3,keyasint,omitempty" json:"retry_from,omitempty"`
}

// IntentDispatchBody — sub-intent to a delegated agent.
type IntentDispatchBody struct {
	// SubIntentJSON is the canonical JSON encoding of the child Intent.
	// Receivers decode via ir.CanonicalJSON-equivalent path.
	SubIntentJSON []byte `cbor:"0,keyasint" json:"sub_intent_json"`

	// ScopeURI references the CortexScope granted for child reads.
	// Empty for in-process dispatch under the same agent.
	ScopeURI string `cbor:"1,keyasint,omitempty" json:"scope_uri,omitempty"`

	// PaymentChannel references a tools/payments/stream if the dispatch
	// is external (third-party agent). Empty for in-process.
	PaymentChannel string `cbor:"2,keyasint,omitempty" json:"payment_channel,omitempty"`
}

// IntentAttestBody — signed completion receipt.
type IntentAttestBody struct {
	// Outcome is "success", "failure", or "partial".
	Outcome string `cbor:"0,keyasint" json:"outcome"`

	// CitedURIs are the matrix://cortex/... URIs that were load-bearing
	// during execution. These feed cortex.Attest for salience EMA.
	CitedURIs []string `cbor:"1,keyasint,omitempty" json:"cited_uris,omitempty"`

	// EvidenceJSON is opaque structured evidence (criteria checks,
	// artifact hashes, tool outputs). Free-form per intent type.
	EvidenceJSON []byte `cbor:"2,keyasint,omitempty" json:"evidence_json,omitempty"`

	// CompletedAt is wall-clock ISO-8601.
	CompletedAt string `cbor:"3,keyasint" json:"completed_at"`

	// AnchorTx is the chain transaction hash if anchored on Paxeer.
	// Empty when not anchored (v1: always empty, chain dropped).
	AnchorTx string `cbor:"4,keyasint,omitempty" json:"anchor_tx,omitempty"`
}

// IntentFailBody — typed failure.
type IntentFailBody struct {
	// Reason is one of the structured failure reasons from
	// research/02-protocol.md §13: "blocked_by_constraint",
	// "tool_error", "policy_denied", "deadline_exceeded",
	// "budget_exceeded", "subagent_failed", "ambiguous_after_clarify",
	// "correction_invalid", "x:custom".
	Reason string `cbor:"0,keyasint" json:"reason"`

	// Message is a human-readable elaboration.
	Message string `cbor:"1,keyasint,omitempty" json:"message,omitempty"`

	// EvidenceJSON is structured evidence (mirrors IntentAttestBody).
	EvidenceJSON []byte `cbor:"2,keyasint,omitempty" json:"evidence_json,omitempty"`

	// FailedAt is wall-clock ISO-8601.
	FailedAt string `cbor:"3,keyasint" json:"failed_at"`

	// PartialURIs are matrix://cortex/... URIs for work products that
	// did land before the failure (so the user can recover them).
	PartialURIs []string `cbor:"4,keyasint,omitempty" json:"partial_uris,omitempty"`
}

// IntentCancelBody — user revokes before completion.
type IntentCancelBody struct {
	// Reason is human-readable; no closed enum (user-driven action).
	Reason string `cbor:"0,keyasint,omitempty" json:"reason,omitempty"`

	// CancelledAt is wall-clock ISO-8601.
	CancelledAt string `cbor:"1,keyasint" json:"cancelled_at"`
}

// PolicyGateBody — human-in-loop checkpoint emitted by the executor
// when a rule fires with `gate` rather than `block`.
type PolicyGateBody struct {
	// RuleRef is the matrix://rule/<id> that triggered.
	RuleRef string `cbor:"0,keyasint" json:"rule_ref"`

	// PlanID + NodeID locate the gate within the executing plan.
	PlanID string `cbor:"1,keyasint,omitempty" json:"plan_id,omitempty"`
	NodeID string `cbor:"2,keyasint,omitempty" json:"node_id,omitempty"`

	// Question is shown to the user.
	Question string `cbor:"3,keyasint" json:"question"`

	// Options for the answer; empty = open text.
	Options []string `cbor:"4,keyasint,omitempty" json:"options,omitempty"`

	// ExpiresAt is wall-clock when the gate auto-denies (ISO-8601).
	ExpiresAt string `cbor:"5,keyasint,omitempty" json:"expires_at,omitempty"`
}

// PolicyGateResolveBody — user approves/denies a gate.
type PolicyGateResolveBody struct {
	// GateOf is the correlation_id of the policy.gate being resolved.
	GateOf string `cbor:"0,keyasint" json:"gate_of"`

	// Decision is "approve" or "deny".
	Decision string `cbor:"1,keyasint" json:"decision"`

	// Answer is the chosen option (when PolicyGateBody.Options was set)
	// or the free-text answer.
	Answer string `cbor:"2,keyasint,omitempty" json:"answer,omitempty"`

	// ResolvedAt is wall-clock ISO-8601.
	ResolvedAt string `cbor:"3,keyasint" json:"resolved_at"`
}

// kindBodyType maps each kind to its expected Go body type.
// Used by NewEnvelope and ValidateBody for strict kind↔type checking.
var kindBodyType = map[string]reflect.Type{
	KindIntentDraft:       reflect.TypeOf(IntentDraftBody{}),
	KindIntentCompiled:    reflect.TypeOf(IntentCompiledBody{}),
	KindIntentClarify:     reflect.TypeOf(IntentClarifyBody{}),
	KindIntentAnswer:      reflect.TypeOf(IntentAnswerBody{}),
	KindIntentAccept:      reflect.TypeOf(IntentAcceptBody{}),
	KindPlanProposed:      reflect.TypeOf(PlanProposedBody{}),
	KindPlanStep:          reflect.TypeOf(PlanStepBody{}),
	KindPlanOutput:        reflect.TypeOf(PlanOutputBody{}),
	KindIntentCorrect:     reflect.TypeOf(IntentCorrectBody{}),
	KindIntentDispatch:    reflect.TypeOf(IntentDispatchBody{}),
	KindIntentAttest:      reflect.TypeOf(IntentAttestBody{}),
	KindIntentFail:        reflect.TypeOf(IntentFailBody{}),
	KindIntentCancel:      reflect.TypeOf(IntentCancelBody{}),
	KindPolicyGate:        reflect.TypeOf(PolicyGateBody{}),
	KindPolicyGateResolve: reflect.TypeOf(PolicyGateResolveBody{}),
}

// checkBodyKind validates that body is of the expected Go type for kind.
// Accepts both value and pointer-to-value of the expected type.
func checkBodyKind(kind string, body interface{}) error {
	want, ok := kindBodyType[kind]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if body == nil {
		return fmt.Errorf("%w: kind=%s body=nil", ErrBodyTypeMismatch, kind)
	}
	got := reflect.TypeOf(body)
	if got.Kind() == reflect.Ptr {
		got = got.Elem()
	}
	if got != want {
		return fmt.Errorf("%w: kind=%s want=%s got=%s", ErrBodyTypeMismatch, kind, want, got)
	}
	return nil
}

// BodyTypeOf returns the reflect.Type expected for the given kind, or
// nil if kind is not a valid kind. Used by external decoders that need
// to allocate the right typed struct dynamically.
func BodyTypeOf(kind string) reflect.Type {
	return kindBodyType[kind]
}

// NewTypedBody allocates a fresh zero-valued body for kind as a pointer
// usable with env.DecodeBody. Returns nil for unknown kinds.
func NewTypedBody(kind string) interface{} {
	t := kindBodyType[kind]
	if t == nil {
		return nil
	}
	return reflect.New(t).Interface()
}

// ValidateBody decodes env.Body into the kind-matched typed struct and
// returns the typed value (as an interface{}). Use this when the caller
// wants strict kind↔type validation in one step. Returns ErrBodyTypeMismatch
// (via NewTypedBody) for unknown kinds.
func ValidateBody(env *Envelope) (interface{}, error) {
	if env == nil {
		return nil, fmt.Errorf("envelope: nil Envelope")
	}
	out := NewTypedBody(env.Kind)
	if out == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, env.Kind)
	}
	if err := env.DecodeBody(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
