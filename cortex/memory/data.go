// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Per-type Data schemas. Spec: research/04-cortex.md §4.2.
//
// Each Data type carries `SchemaVersion` so future migrations (§14.2) can
// transform on read. Phase 2 ships v1 for every type.
//
// Encoding rule: every Data struct is encoded to canonical deterministic
// CBOR via memory.EncodeData; the resulting bytes go into Version.Data.

package memory

import (
	"errors"
	"time"
)

// Stance for Beliefs (§4.2).
type Stance string

const (
	StanceBelieve Stance = "believe"
	StanceDoubt   Stance = "doubt"
	StanceSuspect Stance = "suspect"
)

func (s Stance) Valid() bool {
	switch s {
	case StanceBelieve, StanceDoubt, StanceSuspect:
		return true
	}
	return false
}

// Polarity for Preferences and Constraints (§4.2).
type Polarity string

const (
	PolarityPrefer  Polarity = "prefer"
	PolarityAvoid   Polarity = "avoid"
	PolarityNeutral Polarity = "neutral"
	PolarityDo      Polarity = "do"
	PolarityDont    Polarity = "dont"
)

// Strength for Constraints.
type Strength string

const (
	StrengthSoft Strength = "soft"
	StrengthFirm Strength = "firm"
	StrengthHard Strength = "hard"
)

// ConstraintSource for Constraints.
type ConstraintSource string

const (
	ConstraintSourceUserDeclared ConstraintSource = "user_declared"
	ConstraintSourceLearned      ConstraintSource = "learned"
	ConstraintSourceInferred     ConstraintSource = "inferred"
)

// GoalStatus for Goals.
type GoalStatus string

const (
	GoalActive    GoalStatus = "active"
	GoalPaused    GoalStatus = "paused"
	GoalCompleted GoalStatus = "completed"
	GoalAbandoned GoalStatus = "abandoned"
)

// EventKind enumerates the event subtypes (§4.2). Bounded vocabulary.
type EventKind string

const (
	EventIntentCompleted EventKind = "intent_completed"
	EventIntentFailed    EventKind = "intent_failed"
	EventPayment         EventKind = "payment"
	EventDispatch        EventKind = "dispatch"
	EventObservation     EventKind = "observation"
	EventInteraction     EventKind = "interaction"
)

// Outcome is the success/failure tag on Events.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomePartial Outcome = "partial"
)

// AssetAmount is a typed currency amount; format kept light for Phase 2.
type AssetAmount struct {
	Asset  string `cbor:"1,keyasint"`
	Amount string `cbor:"2,keyasint"` // decimal string to preserve precision
}

// PublicKey is opaque bytes (DID-bound identity lands in Phase 6).
type PublicKey []byte

// IdentityData (§4.2). Actor's self-knowledge.
type IdentityData struct {
	SchemaVersion int         `cbor:"0,keyasint"`
	Name          string      `cbor:"1,keyasint"`
	DID           string      `cbor:"2,keyasint,omitempty"`
	Wallets       []string    `cbor:"3,keyasint,omitempty"` // 0x... or pax... bech32 addrs
	Roles         []string    `cbor:"4,keyasint,omitempty"`
	PublicKeys    []PublicKey `cbor:"5,keyasint,omitempty"`
}

// FactData (§4.2). Objective claim.
type FactData struct {
	SchemaVersion int        `cbor:"0,keyasint"`
	Statement     string     `cbor:"1,keyasint"`
	Subject       string     `cbor:"2,keyasint"`           // URI
	Predicate     string     `cbor:"3,keyasint"`           // bounded vocab
	Object        []byte     `cbor:"4,keyasint,omitempty"` // canonical CBOR of the object value
	Source        string     `cbor:"5,keyasint,omitempty"` // optional URI
	AsOf          *time.Time `cbor:"6,keyasint,omitempty"`
}

// PreferenceData (§4.2).
type PreferenceData struct {
	SchemaVersion int      `cbor:"0,keyasint"`
	Topic         string   `cbor:"1,keyasint"`
	Value         []byte   `cbor:"2,keyasint"` // canonical CBOR of value
	Polarity      Polarity `cbor:"3,keyasint"`
	StrengthVal   float32  `cbor:"4,keyasint"`
	Rationale     string   `cbor:"5,keyasint,omitempty"`
}

// BeliefData (§4.2).
type BeliefData struct {
	SchemaVersion   int      `cbor:"0,keyasint"`
	Statement       string   `cbor:"1,keyasint"`
	Subject         string   `cbor:"2,keyasint"`
	Stance          Stance   `cbor:"3,keyasint"`
	EvidenceFor     []string `cbor:"4,keyasint,omitempty"`
	EvidenceAgainst []string `cbor:"5,keyasint,omitempty"`
}

// EventData (§4.2). Largest volume in production.
type EventData struct {
	SchemaVersion int            `cbor:"0,keyasint"`
	Kind          EventKind      `cbor:"1,keyasint"`
	IntentRef     string         `cbor:"2,keyasint,omitempty"`
	Counterparty  string         `cbor:"3,keyasint,omitempty"`
	OutcomeVal    Outcome        `cbor:"4,keyasint"`
	Cost          *AssetAmount   `cbor:"5,keyasint,omitempty"`
	Duration      *time.Duration `cbor:"6,keyasint,omitempty"`
	Artifacts     []string       `cbor:"7,keyasint,omitempty"` // ArtifactRef URIs
	Summary       string         `cbor:"8,keyasint,omitempty"` // 1-line human summary
}

// GoalData (§4.2).
//
// sess#32 ambient extension: CBOR keys 6–11 are optional and zero-default
// when missing, so old encoded GoalData decodes forward without migration
// (the canonical-deterministic CBOR encoder simply omits them when zero).
// New ambient writers populate them for the scheduler/architect surface.
type GoalData struct {
	SchemaVersion   int        `cbor:"0,keyasint"`
	Statement       string     `cbor:"1,keyasint"`
	HorizonEnd      *time.Time `cbor:"2,keyasint,omitempty"`
	SuccessCriteria []string   `cbor:"3,keyasint,omitempty"` // predicate exprs as strings (typed AST in Phase 3+)
	Status          GoalStatus `cbor:"4,keyasint"`
	Subgoals        []string   `cbor:"5,keyasint,omitempty"`

	// sess#32 ambient — CBOR-optional, backward-compat with pre-sess#32 Goals.
	VerbHint    string           `cbor:"6,keyasint,omitempty"`  // D7 closed verb hint for scheduler
	Objects     []GoalObjRef     `cbor:"7,keyasint,omitempty"`  // typed referents
	Constraints []GoalConstraint `cbor:"8,keyasint,omitempty"`  // hard/soft, embedded
	Budget      *GoalBudget      `cbor:"9,keyasint,omitempty"`  // per-goal caps
	Persistent  bool             `cbor:"10,keyasint,omitempty"` // false=one-shot, true=standing
	CreatedBy   string           `cbor:"11,keyasint,omitempty"` // wallet pubkey / DID of architect
}

// GoalObjRef is a typed referent embedded in a Goal — closed v1 ObjKind
// names (matrix:// URI or NL referent in Ref; optional inline value).
type GoalObjRef struct {
	Kind  string `cbor:"1,keyasint"`
	Ref   string `cbor:"2,keyasint"`
	Value []byte `cbor:"3,keyasint,omitempty"`
}

// GoalConstraint is a typed rail attached to a Goal at lock time. Distinct
// from cortex Constraint memories (those are per-actor standing rails);
// these are per-goal, embedded in the GoalData itself.
type GoalConstraint struct {
	Type      string `cbor:"1,keyasint"` // budget|deadline|jurisdiction|quality|rule|policy|x:*
	Hard      bool   `cbor:"2,keyasint"`
	Statement string `cbor:"3,keyasint"`           // human-readable
	Data      []byte `cbor:"4,keyasint,omitempty"` // canonical CBOR of type-specific payload
}

// GoalBudget caps spend and intent volume for a single Goal. All numeric
// fields are decimal strings to match AssetAmount.Amount semantics.
type GoalBudget struct {
	DailyPaxMax      string `cbor:"1,keyasint,omitempty"`
	TotalPaxMax      string `cbor:"2,keyasint,omitempty"`
	MaxIntentsPerDay int    `cbor:"3,keyasint,omitempty"`
	MaxConcurrent    int    `cbor:"4,keyasint,omitempty"`
}

// ConstraintData (§4.2). Distinct from formal rules/.
type ConstraintData struct {
	SchemaVersion int              `cbor:"0,keyasint"`
	Statement     string           `cbor:"1,keyasint"`
	Polarity      Polarity         `cbor:"2,keyasint"`
	Trigger       string           `cbor:"3,keyasint,omitempty"` // predicate expr
	StrengthVal   Strength         `cbor:"4,keyasint"`
	Source        ConstraintSource `cbor:"5,keyasint"`
}

// CapabilityData (§4.2).
type CapabilityData struct {
	SchemaVersion int       `cbor:"0,keyasint"`
	Subject       string    `cbor:"1,keyasint"`
	Capability    string    `cbor:"2,keyasint"`
	Parameters    []byte    `cbor:"3,keyasint,omitempty"`
	Verified      bool      `cbor:"4,keyasint"`
	LastObserved  time.Time `cbor:"5,keyasint"`
}

// PatternData (§4.2). Usually skill-authored.
type PatternData struct {
	SchemaVersion int      `cbor:"0,keyasint"`
	Statement     string   `cbor:"1,keyasint"`
	DerivedFrom   []string `cbor:"2,keyasint,omitempty"`
	Strength      float32  `cbor:"3,keyasint"`
	Coverage      int      `cbor:"4,keyasint"`
}

// TypedData is the marker interface implemented by every Data struct above.
// Used for compile-time dispatch in EncodeData / NewVersionFor.
type TypedData interface {
	memoryType() Type
}

func (IdentityData) memoryType() Type   { return TypeIdentity }
func (FactData) memoryType() Type       { return TypeFact }
func (PreferenceData) memoryType() Type { return TypePreference }
func (BeliefData) memoryType() Type     { return TypeBelief }
func (EventData) memoryType() Type      { return TypeEvent }
func (GoalData) memoryType() Type       { return TypeGoal }
func (ConstraintData) memoryType() Type { return TypeConstraint }
func (CapabilityData) memoryType() Type { return TypeCapability }
func (PatternData) memoryType() Type    { return TypePattern }

// TypeOf returns the canonical Type for d. Returns 0 if d is nil.
func TypeOf(d TypedData) Type {
	if d == nil {
		return 0
	}
	return d.memoryType()
}

// ErrUnknownDataKind is returned by DecodeData when t is not one of the
// nine canonical types.
var ErrUnknownDataKind = errors.New("memory: unknown data kind")

// Copyright © 2026 Paxlabs Inc. All rights reserved.
