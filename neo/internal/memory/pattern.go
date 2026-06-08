// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PatternSpec is the structured procedural-memory schema from the frozen spec
// ([procedural.pattern_schema]): a reusable how-to recipe — name, trigger,
// preconditions (checked BEFORE applying), steps, gotchas (learned failure
// modes), and success_criteria (verified AFTER). cortex's PatternData stores a
// single flat Statement, so Neo maps this richer schema ONTO that field by
// encoding it as canonical JSON. The frozen spec explicitly permits "define the
// cortex Pattern type (or map onto existing types)"; mapping keeps cortex — the
// shared, replay-critical, tamper-evident store — untouched.
type PatternSpec struct {
	Name            string   `json:"name,omitempty"`
	Trigger         string   `json:"trigger,omitempty"`
	Preconditions   []string `json:"preconditions,omitempty"`
	Steps           []string `json:"steps,omitempty"`
	Gotchas         []string `json:"gotchas,omitempty"`
	SuccessCriteria []string `json:"success_criteria,omitempty"`
}

// patternEncPrefix tags an encoded PatternSpec so a legacy/plain Statement
// (written before the structured schema, or by hand) is distinguishable and
// still decodes gracefully as a freeform recipe.
const patternEncPrefix = "neo.pattern.v1:"

// Encode renders the spec to the flat string stored in PatternData.Statement.
func (s PatternSpec) Encode() string {
	b, err := json.Marshal(s)
	if err != nil {
		return strings.TrimSpace(s.Name + " " + strings.Join(s.Steps, "; "))
	}
	return patternEncPrefix + string(b)
}

// DecodePatternSpec parses a stored Statement back into a PatternSpec. A plain
// (non-encoded) statement is treated as a single freeform step so legacy flat
// patterns continue to render.
func DecodePatternSpec(statement string) PatternSpec {
	s := strings.TrimSpace(statement)
	if rest, ok := strings.CutPrefix(s, patternEncPrefix); ok {
		var spec PatternSpec
		if err := json.Unmarshal([]byte(rest), &spec); err == nil {
			return spec
		}
	}
	if s == "" {
		return PatternSpec{}
	}
	return PatternSpec{Steps: []string{s}}
}

// dedupKey is the normalized identity used to reinforce an existing pattern
// rather than writing a duplicate: the name when present, else the trigger,
// else the joined steps.
func (s PatternSpec) dedupKey() string {
	switch {
	case strings.TrimSpace(s.Name) != "":
		return normalizeStatement(s.Name)
	case strings.TrimSpace(s.Trigger) != "":
		return normalizeStatement(s.Trigger)
	default:
		return normalizeStatement(strings.Join(s.Steps, " "))
	}
}

// IsEmpty reports whether the spec carries no usable content.
func (s PatternSpec) IsEmpty() bool { return s.dedupKey() == "" }

// Pattern is a retrieved procedural pattern (proven how-to) ready for injection
// into the system block.
type Pattern struct {
	Spec       PatternSpec
	Confidence float32
	Coverage   int
	URI        string
}

// Render produces the one-line guidance injected into the window
// ([procedural.retrieval]): the proven path plus the preconditions to check
// first, the gotchas to avoid, and the success criteria to verify after.
func (p Pattern) Render() string {
	var b strings.Builder
	name := strings.TrimSpace(p.Spec.Name)
	if name == "" {
		name = "(unnamed)"
	}
	b.WriteString(name)
	if t := strings.TrimSpace(p.Spec.Trigger); t != "" {
		fmt.Fprintf(&b, " [when: %s]", t)
	}
	if len(p.Spec.Preconditions) > 0 {
		fmt.Fprintf(&b, " · preconditions: %s", strings.Join(p.Spec.Preconditions, "; "))
	}
	if len(p.Spec.Steps) > 0 {
		fmt.Fprintf(&b, " · steps: %s", strings.Join(p.Spec.Steps, " → "))
	}
	if len(p.Spec.Gotchas) > 0 {
		fmt.Fprintf(&b, " · gotchas: %s", strings.Join(p.Spec.Gotchas, "; "))
	}
	if len(p.Spec.SuccessCriteria) > 0 {
		fmt.Fprintf(&b, " · success: %s", strings.Join(p.Spec.SuccessCriteria, "; "))
	}
	if p.Coverage > 0 {
		fmt.Fprintf(&b, " (%d× proven)", p.Coverage)
	}
	return b.String()
}
