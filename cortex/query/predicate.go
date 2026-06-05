// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package query implements the typed Find query engine described in
// research/04-cortex.md §12.
//
// Phase 3 scope (read side, no graph, no vector):
//   - Predicate AST: Eq, Ne, Gt, Gte, Lt, Lte, In, HasTag, Matches, And, Or, Not
//   - Field references: head.<X> | version.<X> | data.<X> (per-type names)
//   - Candidate planning: idx/tag scans (when HasTag) → idx/type scans (else)
//   - Salience-cold ordering by default; OrderBy clauses override
//   - Tombstone filter (default: exclude)
//   - Late-binding audit hook (D13): journal a KindFind entry when the call
//     opts in via Query.LateBinding=true
//
// Deferred to later phases:
//   - From / Follow (graph traversal)        — Phase 6 (edges)
//   - Near / NearURI (vector recall)         — Phase 5 (embeddings)
//   - BudgetTokens-driven form rendering     — Phase 4 (forms generator)
//   - Scope (CortexScope sub-agent gating)   — Phase 10 (Merkle proofs)
//
// The package depends on store, keys, memory, salience. It does NOT depend on
// the top-level cortex package; the cortex.Find facade is a thin wrapper that
// calls query.Run.

package query

import (
	"fmt"
	"strings"
)

// Predicate is the closed AST of typed where-clauses. The eval method is
// package-private so callers cannot synthesise predicate types outside this
// package; only the constructors below are valid.
type Predicate interface {
	predicate()
	// String returns a stable canonical rendering for journaling and debug.
	// Round-tripping the string back to a Predicate is NOT guaranteed; this
	// is for audit only.
	String() string
}

// Eq compares Field for equality with Value. Numeric, bool, string,
// time.Time, and memory enum types (Type, Visibility, Polarity, Stance,
// EventKind, Outcome, ConstraintSource, Strength, GoalStatus, SourceKind)
// are supported. Slices and maps are rejected at eval time.
type Eq struct {
	Field FieldRef
	Value any
}

// Ne is the inverse of Eq.
type Ne struct {
	Field FieldRef
	Value any
}

// Gt / Gte / Lt / Lte support ordered scalars: numeric, time.Time, string
// (lexicographic). All other types yield ErrFieldNotComparable.
type Gt struct {
	Field FieldRef
	Value any
}
type Gte struct {
	Field FieldRef
	Value any
}
type Lt struct {
	Field FieldRef
	Value any
}
type Lte struct {
	Field FieldRef
	Value any
}

// In returns true if Field equals any of Values. Values must all be of the
// same kind as Field.
type In struct {
	Field  FieldRef
	Values []any
}

// HasTag is short for Eq{tags, contains tag}. Special-cased so the planner
// can short-circuit candidate selection via idx/tag.
type HasTag struct {
	Tag string
}

// Matches applies a Go regexp to a string-typed field. Pre-compile cost is
// paid once per query (in Run).
type Matches struct {
	Field   FieldRef
	Pattern string
}

// And evaluates to true iff every child evaluates to true. An empty And is
// vacuously true.
type And struct {
	Children []Predicate
}

// Or evaluates to true iff any child evaluates to true. An empty Or is
// vacuously false.
type Or struct {
	Children []Predicate
}

// Not is logical negation.
type Not struct {
	Inner Predicate
}

func (Eq) predicate()      {}
func (Ne) predicate()      {}
func (Gt) predicate()      {}
func (Gte) predicate()     {}
func (Lt) predicate()      {}
func (Lte) predicate()     {}
func (In) predicate()      {}
func (HasTag) predicate()  {}
func (Matches) predicate() {}
func (And) predicate()     {}
func (Or) predicate()      {}
func (Not) predicate()     {}

// FieldRef is a dotted-path string with two valid prefixes: "head.<name>",
// "version.<name>", or "data.<name>". The data.<name> path resolves against
// the type-specific Data struct's exported snake_case field names; see
// fields.go for the per-type whitelist.
type FieldRef string

// Validate returns an error if ref is not parseable. Does not check that
// the field exists for a given memory type — that's eval-time.
func (ref FieldRef) Validate() error {
	parts := strings.SplitN(string(ref), ".", 2)
	if len(parts) != 2 || parts[1] == "" {
		return fmt.Errorf("query: malformed field ref %q (want head.X | version.X | data.X)", ref)
	}
	switch parts[0] {
	case "head", "version", "data":
		return nil
	}
	return fmt.Errorf("query: unknown field-ref namespace %q", parts[0])
}

// String renderings (canonical, used by audit and debug).
func (e Eq) String() string  { return fmt.Sprintf("(%s = %v)", e.Field, e.Value) }
func (e Ne) String() string  { return fmt.Sprintf("(%s != %v)", e.Field, e.Value) }
func (e Gt) String() string  { return fmt.Sprintf("(%s > %v)", e.Field, e.Value) }
func (e Gte) String() string { return fmt.Sprintf("(%s >= %v)", e.Field, e.Value) }
func (e Lt) String() string  { return fmt.Sprintf("(%s < %v)", e.Field, e.Value) }
func (e Lte) String() string { return fmt.Sprintf("(%s <= %v)", e.Field, e.Value) }
func (e In) String() string  { return fmt.Sprintf("(%s IN %v)", e.Field, e.Values) }
func (e HasTag) String() string {
	return fmt.Sprintf("(has_tag %q)", e.Tag)
}
func (e Matches) String() string { return fmt.Sprintf("(%s ~ /%s/)", e.Field, e.Pattern) }
func (e And) String() string {
	if len(e.Children) == 0 {
		return "(and)"
	}
	parts := make([]string, len(e.Children))
	for i, c := range e.Children {
		parts[i] = c.String()
	}
	return "(and " + strings.Join(parts, " ") + ")"
}
func (e Or) String() string {
	if len(e.Children) == 0 {
		return "(or)"
	}
	parts := make([]string, len(e.Children))
	for i, c := range e.Children {
		parts[i] = c.String()
	}
	return "(or " + strings.Join(parts, " ") + ")"
}
func (e Not) String() string {
	if e.Inner == nil {
		return "(not nil)"
	}
	return "(not " + e.Inner.String() + ")"
}

// collectHasTags walks the predicate looking for top-level HasTag predicates
// that the planner can use to drive candidate selection through idx/tag.
//
// Only HasTags reachable through pure And-conjunction (no Or, no Not) are
// returned, because those are the only ones the planner can safely use as
// a *narrowing* candidate filter. A HasTag inside Or or Not constrains the
// final result set without constraining the candidate set.
func collectHasTags(p Predicate) []string {
	if p == nil {
		return nil
	}
	switch x := p.(type) {
	case HasTag:
		return []string{x.Tag}
	case And:
		var out []string
		for _, c := range x.Children {
			out = append(out, collectHasTags(c)...)
		}
		return out
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
