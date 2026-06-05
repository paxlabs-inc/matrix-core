// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package forms generates the three render granularities (short, medium,
// full) for typed memories per research/04-cortex.md §9.
//
// Auto-generation is deterministic: Render(head, data) → Forms is a pure
// function of its inputs (no clocks, no PRNG, no locale). Same input bytes
// produce identical output bytes across runs and across hosts. This is
// load-bearing for cortex_snapshot_hash stability (D11) — forms ride along
// in the Head and Version records that hash into the namespace Merkle.
//
// Per-type templates live in render*ForType functions. The shapes mirror
// the §9.1 spec sketches but use ISO-8601 dates rather than relative times
// so the output stays time-invariant once persisted.
//
// Token-budget enforcement: the auto-renderer truncates short to
// memory.MaxShortTokens and medium to memory.MaxMediumTokens via the bytes/4
// heuristic shared with memory.CountTokens. Skill-supplied overrides that
// exceed the budget are rejected at write time by memory.ValidateMemory
// (returns memory.ErrFormTooLong); this package never silently truncates
// override input.
package forms

import (
	"fmt"
	"strings"
	"time"

	"matrix/cortex/memory"
)

// Render returns auto-generated Short and Medium for the (head, data) pair.
// Full is computed separately via RenderFull because it is never persisted
// in the Forms struct and is only materialised on demand (e.g. when a Find
// caller selects FormFull).
//
// Returned Forms always satisfy memory.CountTokens(short) ≤ MaxShortTokens
// and likewise for medium — by construction, via truncate.
func Render(h *memory.Head, data memory.TypedData) memory.Forms {
	if h == nil || data == nil {
		return memory.Forms{}
	}
	short, medium := renderForType(h, data)
	return memory.Forms{
		Short:  TruncateToTokens(short, memory.MaxShortTokens),
		Medium: TruncateToTokens(medium, memory.MaxMediumTokens),
	}
}

// RenderFull returns the canonical full-form rendering of typed data.
// Unbounded by design (§9: "full = canonical render of typed Data"), so no
// truncation. Mostly a debug/audit surface — production paths usually walk
// the typed Data struct directly rather than parsing this string.
func RenderFull(h *memory.Head, data memory.TypedData) string {
	if h == nil || data == nil {
		return ""
	}
	return renderFullForType(h, data)
}

// renderForType returns (short, medium) before truncation. Templates are
// per-type and field-presence aware: optional fields collapse cleanly when
// empty so the renders stay legible at the budgeted size.
func renderForType(h *memory.Head, data memory.TypedData) (string, string) {
	switch d := data.(type) {
	case memory.IdentityData:
		return renderIdentity(d)
	case memory.FactData:
		return renderFact(d)
	case memory.PreferenceData:
		return renderPreference(d)
	case memory.BeliefData:
		return renderBelief(d)
	case memory.EventData:
		return renderEvent(d)
	case memory.GoalData:
		return renderGoal(d)
	case memory.ConstraintData:
		return renderConstraint(d)
	case memory.CapabilityData:
		return renderCapability(d)
	case memory.PatternData:
		return renderPattern(d)
	}
	// Defensive: unknown TypedData. memory.ValidateMemory rejects this case
	// upstream, but if a caller bypasses validation we still want a
	// deterministic placeholder rather than a panic.
	return "<unknown>", "<unknown>"
}

func renderFullForType(h *memory.Head, data memory.TypedData) string {
	// Full is the same shape as medium but without the per-field truncation
	// that medium would impose at the byte/4 budget. We delegate to the
	// per-type renderers and use the medium template (which already lists
	// all fields). Future work: pretty-printed multi-line forms keyed off
	// type. Phase 4 keeps it minimal because no consumer renders full yet.
	_, medium := renderForType(h, data)
	return medium
}

// --- per-type renderers ---------------------------------------------------

func renderIdentity(d memory.IdentityData) (string, string) {
	short := d.Name
	if d.DID != "" {
		short = fmt.Sprintf("%s (%s)", d.Name, d.DID)
	}
	parts := []string{}
	if len(d.Wallets) > 0 {
		parts = append(parts, fmt.Sprintf("wallets=%d", len(d.Wallets)))
	}
	if len(d.Roles) > 0 {
		parts = append(parts, fmt.Sprintf("roles=%s", strings.Join(d.Roles, ",")))
	}
	if len(d.PublicKeys) > 0 {
		parts = append(parts, fmt.Sprintf("keys=%d", len(d.PublicKeys)))
	}
	medium := short
	if len(parts) > 0 {
		medium = short + " — " + strings.Join(parts, ", ")
	}
	return short, medium
}

func renderFact(d memory.FactData) (string, string) {
	// Predicate is a bounded vocab (e.g. "knows", "owns") so this reads
	// cleanly: predicate(subject)=statement.
	short := fmt.Sprintf("%s(%s)=%s", d.Predicate, d.Subject, d.Statement)
	parts := []string{}
	if d.AsOf != nil {
		parts = append(parts, "as_of="+d.AsOf.UTC().Format(time.RFC3339))
	}
	if d.Source != "" {
		parts = append(parts, "source="+d.Source)
	}
	medium := short
	if len(parts) > 0 {
		medium = short + " — " + strings.Join(parts, ", ")
	}
	return short, medium
}

func renderPreference(d memory.PreferenceData) (string, string) {
	short := fmt.Sprintf("prefers %s (%s, strength=%.2f)", d.Topic, d.Polarity, d.StrengthVal)
	medium := short
	if d.Rationale != "" {
		medium = short + " — " + d.Rationale
	}
	return short, medium
}

func renderBelief(d memory.BeliefData) (string, string) {
	short := fmt.Sprintf("%s %s", d.Stance, d.Statement)
	medium := fmt.Sprintf("%s — %d for / %d against", short, len(d.EvidenceFor), len(d.EvidenceAgainst))
	return short, medium
}

func renderEvent(d memory.EventData) (string, string) {
	short := fmt.Sprintf("%s %s", d.OutcomeVal, d.Kind)
	if d.Counterparty != "" {
		short += " with " + d.Counterparty
	}
	if d.Cost != nil {
		short += fmt.Sprintf(" cost=%s%s", d.Cost.Amount, d.Cost.Asset)
	}
	parts := []string{}
	if d.Duration != nil {
		parts = append(parts, "duration="+d.Duration.String())
	}
	if len(d.Artifacts) > 0 {
		parts = append(parts, fmt.Sprintf("artifacts=%d", len(d.Artifacts)))
	}
	if d.IntentRef != "" {
		parts = append(parts, "intent="+d.IntentRef)
	}
	if d.Summary != "" {
		parts = append(parts, d.Summary)
	}
	medium := short
	if len(parts) > 0 {
		medium = short + " — " + strings.Join(parts, ", ")
	}
	return short, medium
}

func renderGoal(d memory.GoalData) (string, string) {
	short := fmt.Sprintf("[%s] %s", d.Status, d.Statement)
	parts := []string{}
	if d.HorizonEnd != nil {
		parts = append(parts, "by="+d.HorizonEnd.UTC().Format(time.RFC3339))
	}
	if len(d.SuccessCriteria) > 0 {
		parts = append(parts, fmt.Sprintf("criteria=%d", len(d.SuccessCriteria)))
	}
	if len(d.Subgoals) > 0 {
		parts = append(parts, fmt.Sprintf("subgoals=%d", len(d.Subgoals)))
	}
	medium := short
	if len(parts) > 0 {
		medium = short + " — " + strings.Join(parts, ", ")
	}
	return short, medium
}

func renderConstraint(d memory.ConstraintData) (string, string) {
	short := fmt.Sprintf("[%s] %s %s", d.StrengthVal, d.Polarity, d.Statement)
	parts := []string{"source=" + string(d.Source)}
	if d.Trigger != "" {
		parts = append(parts, "trigger="+d.Trigger)
	}
	medium := short + " — " + strings.Join(parts, ", ")
	return short, medium
}

func renderCapability(d memory.CapabilityData) (string, string) {
	verified := "unverified"
	if d.Verified {
		verified = "verified"
	}
	short := fmt.Sprintf("%s can %s (%s)", d.Subject, d.Capability, verified)
	medium := fmt.Sprintf("%s — last_observed=%s", short, d.LastObserved.UTC().Format(time.RFC3339))
	return short, medium
}

func renderPattern(d memory.PatternData) (string, string) {
	short := fmt.Sprintf("%s (strength=%.2f, coverage=%d)", d.Statement, d.Strength, d.Coverage)
	medium := short
	if len(d.DerivedFrom) > 0 {
		medium = fmt.Sprintf("%s — derived from %d", short, len(d.DerivedFrom))
	}
	return short, medium
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
