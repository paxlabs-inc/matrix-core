// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Write-time validation per §4.3.
//
// Performs:
//  1. Type validity + Type/Data consistency.
//  2. Visibility validity.
//  3. Provenance source validity.
//  4. Tag bounds.
//  5. Forms token-budget enforcement.
//  6. Per-type required-field checks.
//
// Reference integrity (URIs in DerivedFrom/Attestations resolve to existing
// memories) is checked at write time inside cortex.Write where the store
// handle is available; this file stays storage-free.

package memory

import (
	"fmt"
	"strings"
)

// ValidateMemory checks h, v, and data together. data MAY be nil if v.Data
// is already populated; in that case the function decodes v.Data internally.
func ValidateMemory(h *Head, v *Version, data TypedData) error {
	if h == nil || v == nil {
		return fmt.Errorf("memory: ValidateMemory: nil head or version")
	}
	if !h.Type.Valid() {
		return ErrInvalidType
	}
	if h.Type != v.Type {
		return fmt.Errorf("memory: head.Type=%s != version.Type=%s", h.Type, v.Type)
	}
	if !h.Visibility.Valid() {
		return ErrInvalidVisibility
	}
	if h.DeclaredImportance > 10 {
		return fmt.Errorf("memory: declared_importance %d > 10", h.DeclaredImportance)
	}
	if len(h.Tags) > MaxTagsPerMemory {
		return ErrTooManyTags
	}
	for _, t := range h.Tags {
		if len(t) > MaxTagLen {
			return ErrTagTooLong
		}
	}
	// Frames (Phase 8): each FrameRef must pass its own Validate;
	// MaxFramesPerMemory caps fan-out per write so idx/frame +
	// idx/actor_obj key emission stays bounded. The cortex Write path
	// emits keys from this slice 1:1 (for idx/frame) and conditionally
	// (idx/actor_obj when h.Type==TypeEvent) — see cortex.go.
	if len(h.Frames) > MaxFramesPerMemory {
		return ErrTooManyFrames
	}
	for i := range h.Frames {
		if err := h.Frames[i].Validate(); err != nil {
			return err
		}
	}

	if !v.Provenance.Source.Valid() {
		return ErrInvalidSource
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		return fmt.Errorf("memory: confidence %f out of [0,1]", v.Confidence)
	}

	if err := validateForms(&v.Forms); err != nil {
		return err
	}

	// Resolve typed Data: prefer the supplied struct; else decode from v.Data.
	if data == nil {
		if len(v.Data) == 0 {
			return ErrEmptyData
		}
		decoded, err := DecodeData(v.Type, v.Data)
		if err != nil {
			return fmt.Errorf("memory: decode data: %w", err)
		}
		data = decoded
	}
	if data.memoryType() != v.Type {
		return ErrTypeDataMismatch
	}
	if err := validateTypedData(data); err != nil {
		return err
	}
	return nil
}

// validateForms enforces §9.3 token caps using the bytes/4 heuristic. See
// the BytesPerToken doc on types.go for the rationale and tradeoffs.
func validateForms(f *Forms) error {
	if n := CountTokens(f.Short); n > MaxShortTokens {
		return fmt.Errorf("%w: short=%d > %d", ErrFormTooLong, n, MaxShortTokens)
	}
	if n := CountTokens(f.Medium); n > MaxMediumTokens {
		return fmt.Errorf("%w: medium=%d > %d", ErrFormTooLong, n, MaxMediumTokens)
	}
	return nil
}

// CountTokens returns ceil(utf8_bytes(s) / BytesPerToken). Empty string → 0.
//
// Deterministic and zero-dependency. Public so the forms package and any
// budget-enforcing caller (Find render, Compact) agree byte-for-byte with
// write-time validation. Switching the heuristic is a snapshot-hash-affecting
// change.
func CountTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + BytesPerToken - 1) / BytesPerToken
}

// validateTypedData runs per-type required-field checks.
func validateTypedData(d TypedData) error {
	switch x := d.(type) {
	case IdentityData:
		if strings.TrimSpace(x.Name) == "" {
			return missingField(TypeIdentity, "name")
		}
	case FactData:
		if strings.TrimSpace(x.Statement) == "" {
			return missingField(TypeFact, "statement")
		}
		if strings.TrimSpace(x.Subject) == "" {
			return missingField(TypeFact, "subject")
		}
		if strings.TrimSpace(x.Predicate) == "" {
			return missingField(TypeFact, "predicate")
		}
	case PreferenceData:
		if strings.TrimSpace(x.Topic) == "" {
			return missingField(TypePreference, "topic")
		}
		if x.Polarity == "" {
			return missingField(TypePreference, "polarity")
		}
		if x.StrengthVal < 0 || x.StrengthVal > 1 {
			return fmt.Errorf("memory: Preference.strength %f out of [0,1]", x.StrengthVal)
		}
	case BeliefData:
		if strings.TrimSpace(x.Statement) == "" {
			return missingField(TypeBelief, "statement")
		}
		if !x.Stance.Valid() {
			return fmt.Errorf("memory: Belief.stance invalid: %q", x.Stance)
		}
	case EventData:
		if x.Kind == "" {
			return missingField(TypeEvent, "kind")
		}
		if x.OutcomeVal == "" {
			return missingField(TypeEvent, "outcome")
		}
	case GoalData:
		if strings.TrimSpace(x.Statement) == "" {
			return missingField(TypeGoal, "statement")
		}
		if x.Status == "" {
			return missingField(TypeGoal, "status")
		}
	case ConstraintData:
		if strings.TrimSpace(x.Statement) == "" {
			return missingField(TypeConstraint, "statement")
		}
		if x.StrengthVal == "" {
			return missingField(TypeConstraint, "strength")
		}
		if x.Source == "" {
			return missingField(TypeConstraint, "source")
		}
	case CapabilityData:
		if strings.TrimSpace(x.Subject) == "" {
			return missingField(TypeCapability, "subject")
		}
		if strings.TrimSpace(x.Capability) == "" {
			return missingField(TypeCapability, "capability")
		}
	case PatternData:
		if strings.TrimSpace(x.Statement) == "" {
			return missingField(TypePattern, "statement")
		}
		if x.Strength < 0 || x.Strength > 1 {
			return fmt.Errorf("memory: Pattern.strength %f out of [0,1]", x.Strength)
		}
	default:
		return ErrUnknownDataKind
	}
	return nil
}

func missingField(t Type, field string) error {
	return fmt.Errorf("memory: %s.%s required", t, field)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
