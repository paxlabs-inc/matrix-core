// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package query

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"matrix/cortex/memory"
)

// ErrFieldUnknown is returned when a FieldRef does not resolve for the
// memory's type. It is non-fatal at the predicate level (Eq against an
// unknown field returns false), but Matches/Gt/etc. surface it.
var ErrFieldUnknown = errors.New("query: field unknown for this memory type")

// ErrFieldNotComparable is returned when an ordered comparison is attempted
// against a non-ordered field (e.g. Gt on a slice or bool).
var ErrFieldNotComparable = errors.New("query: field is not orderable")

// ErrTypeMismatch is returned when a predicate's literal Value cannot be
// compared with the resolved field value.
var ErrTypeMismatch = errors.New("query: predicate value type mismatch with field")

// resolveField returns the field's value for the given memory + decoded
// data, plus a kind indicating how to compare it. (nil, false, nil) means
// the field is unknown for this type — the caller decides whether that's
// fatal (Matches/Gt) or false-y (Eq).
func resolveField(ref FieldRef, m *memory.Memory, data memory.TypedData) (any, bool, error) {
	parts := strings.SplitN(string(ref), ".", 2)
	if len(parts) != 2 {
		return nil, false, fmt.Errorf("query: malformed field ref %q", ref)
	}
	switch parts[0] {
	case "head":
		v, ok := headField(parts[1], &m.Head)
		return v, ok, nil
	case "version":
		v, ok := versionField(parts[1], &m.Version)
		return v, ok, nil
	case "data":
		v, ok := dataField(parts[1], data)
		return v, ok, nil
	}
	return nil, false, fmt.Errorf("query: unknown namespace in field ref %q", ref)
}

// headField returns scalar fields of memory.Head by canonical lower_snake
// name. Unknown name → (nil, false).
func headField(name string, h *memory.Head) (any, bool) {
	switch name {
	case "id":
		return h.ID.String(), true
	case "type":
		return h.Type.String(), true
	case "current_version":
		return h.CurrentVersion, true
	case "actor_scope":
		return h.ActorScope, true
	case "visibility":
		switch h.Visibility {
		case memory.VisPrivate:
			return "private", true
		case memory.VisScoped:
			return "scoped", true
		case memory.VisActorPublic:
			return "actor_public", true
		}
		return "", true
	case "declared_importance":
		return uint64(h.DeclaredImportance), true
	case "tombstoned":
		return h.Tombstoned != nil, true
	case "last_updated_at":
		return h.LastUpdatedAt, true
	}
	return nil, false
}

// versionField returns scalar fields of memory.Version. Data is exposed
// only via the data.<X> namespace; this resolver ignores Data.
func versionField(name string, v *memory.Version) (any, bool) {
	switch name {
	case "version":
		return v.Version, true
	case "created_at":
		return v.CreatedAt, true
	case "created_by":
		return v.CreatedBy, true
	case "expires_at":
		if v.ExpiresAt == nil {
			return time.Time{}, true
		}
		return *v.ExpiresAt, true
	case "confidence":
		return float64(v.Confidence), true
	case "provenance.source":
		return string(v.Provenance.Source), true
	case "provenance.signed_by_present":
		return len(v.Provenance.SignedBy) > 0, true
	}
	return nil, false
}

// dataField returns scalar Data-struct fields per the §4.2 schemas. New
// fields require a one-line addition here AND a matching test.
func dataField(name string, d memory.TypedData) (any, bool) {
	switch x := d.(type) {
	case memory.IdentityData:
		switch name {
		case "name":
			return x.Name, true
		case "did":
			return x.DID, true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	case memory.FactData:
		switch name {
		case "statement":
			return x.Statement, true
		case "subject":
			return x.Subject, true
		case "predicate":
			return x.Predicate, true
		case "source":
			return x.Source, true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		case "as_of":
			if x.AsOf == nil {
				return time.Time{}, true
			}
			return *x.AsOf, true
		}
	case memory.PreferenceData:
		switch name {
		case "topic":
			return x.Topic, true
		case "polarity":
			return string(x.Polarity), true
		case "strength_val":
			return float64(x.StrengthVal), true
		case "rationale":
			return x.Rationale, true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	case memory.BeliefData:
		switch name {
		case "statement":
			return x.Statement, true
		case "subject":
			return x.Subject, true
		case "stance":
			return string(x.Stance), true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	case memory.EventData:
		switch name {
		case "kind":
			return string(x.Kind), true
		case "intent_ref":
			return x.IntentRef, true
		case "counterparty":
			return x.Counterparty, true
		case "outcome":
			return string(x.OutcomeVal), true
		case "summary":
			return x.Summary, true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	case memory.GoalData:
		switch name {
		case "statement":
			return x.Statement, true
		case "status":
			return string(x.Status), true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		case "horizon_end":
			if x.HorizonEnd == nil {
				return time.Time{}, true
			}
			return *x.HorizonEnd, true
		}
	case memory.ConstraintData:
		switch name {
		case "statement":
			return x.Statement, true
		case "polarity":
			return string(x.Polarity), true
		case "trigger":
			return x.Trigger, true
		case "strength":
			return string(x.StrengthVal), true
		case "source":
			return string(x.Source), true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	case memory.CapabilityData:
		switch name {
		case "subject":
			return x.Subject, true
		case "capability":
			return x.Capability, true
		case "verified":
			return x.Verified, true
		case "last_observed":
			return x.LastObserved, true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	case memory.PatternData:
		switch name {
		case "statement":
			return x.Statement, true
		case "strength":
			return float64(x.Strength), true
		case "coverage":
			return uint64(x.Coverage), true
		case "schema_version":
			return uint64(x.SchemaVersion), true
		}
	}
	return nil, false
}

// evaluator carries pre-compiled state for one Find call (regexp cache so
// each Matches predicate compiles once, not once per memory).
type evaluator struct {
	regex map[string]*regexp.Regexp
}

func newEvaluator() *evaluator {
	return &evaluator{regex: map[string]*regexp.Regexp{}}
}

// eval recursively evaluates a Predicate against (m, data). Returns false
// (no error) for an unknown field on Eq/Ne/In/HasTag — those are "this
// memory simply doesn't satisfy the predicate" signals. Gt/Gte/Lt/Lte/
// Matches return ErrFieldUnknown to surface schema typos.
func (ev *evaluator) eval(p Predicate, m *memory.Memory, data memory.TypedData) (bool, error) {
	if p == nil {
		return true, nil
	}
	switch x := p.(type) {
	case Eq:
		v, ok, err := resolveField(x.Field, m, data)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		return scalarEq(v, x.Value)
	case Ne:
		v, ok, err := resolveField(x.Field, m, data)
		if err != nil {
			return false, err
		}
		if !ok {
			return true, nil
		}
		eq, err := scalarEq(v, x.Value)
		if err != nil {
			return false, err
		}
		return !eq, nil
	case Gt:
		return ev.cmp(x.Field, x.Value, m, data, func(c int) bool { return c > 0 })
	case Gte:
		return ev.cmp(x.Field, x.Value, m, data, func(c int) bool { return c >= 0 })
	case Lt:
		return ev.cmp(x.Field, x.Value, m, data, func(c int) bool { return c < 0 })
	case Lte:
		return ev.cmp(x.Field, x.Value, m, data, func(c int) bool { return c <= 0 })
	case In:
		v, ok, err := resolveField(x.Field, m, data)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		for _, candidate := range x.Values {
			eq, err := scalarEq(v, candidate)
			if err != nil {
				return false, err
			}
			if eq {
				return true, nil
			}
		}
		return false, nil
	case HasTag:
		for _, tag := range m.Head.Tags {
			if string(tag) == x.Tag {
				return true, nil
			}
		}
		return false, nil
	case Matches:
		return ev.matches(x, m, data)
	case And:
		for _, c := range x.Children {
			ok, err := ev.eval(c, m, data)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	case Or:
		for _, c := range x.Children {
			ok, err := ev.eval(c, m, data)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case Not:
		ok, err := ev.eval(x.Inner, m, data)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}
	return false, fmt.Errorf("query: unknown predicate kind %T", p)
}

func (ev *evaluator) cmp(ref FieldRef, lit any, m *memory.Memory, data memory.TypedData, accept func(int) bool) (bool, error) {
	v, ok, err := resolveField(ref, m, data)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrFieldUnknown, ref)
	}
	c, err := scalarCmp(v, lit)
	if err != nil {
		return false, err
	}
	return accept(c), nil
}

func (ev *evaluator) matches(x Matches, m *memory.Memory, data memory.TypedData) (bool, error) {
	v, ok, err := resolveField(x.Field, m, data)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrFieldUnknown, x.Field)
	}
	s, ok := v.(string)
	if !ok {
		return false, fmt.Errorf("%w: %s (Matches needs string)", ErrFieldNotComparable, x.Field)
	}
	re, ok := ev.regex[x.Pattern]
	if !ok {
		var err error
		re, err = regexp.Compile(x.Pattern)
		if err != nil {
			return false, fmt.Errorf("query: bad regexp %q: %w", x.Pattern, err)
		}
		ev.regex[x.Pattern] = re
	}
	return re.MatchString(s), nil
}

// scalarEq compares two scalars. Numeric kinds are coerced through float64
// to allow Eq{declared_importance, 5} where 5 is an int literal but the
// resolved field is uint64. Time.Time uses Equal. Booleans, strings, and
// enum-strings (Polarity, Stance, ...) compare directly after converting
// the typed enum to string in resolveField.
func scalarEq(field, lit any) (bool, error) {
	if field == nil || lit == nil {
		return field == lit, nil
	}
	if t1, ok := field.(time.Time); ok {
		t2, ok := lit.(time.Time)
		if !ok {
			return false, fmt.Errorf("%w: time vs %T", ErrTypeMismatch, lit)
		}
		return t1.Equal(t2), nil
	}
	// Numeric coerce
	if f1, ok1 := numericTo64(field); ok1 {
		f2, ok2 := numericTo64(lit)
		if !ok2 {
			return false, fmt.Errorf("%w: numeric field vs %T", ErrTypeMismatch, lit)
		}
		return f1 == f2, nil
	}
	if s1, ok := field.(string); ok {
		if s2, ok := lit.(string); ok {
			return s1 == s2, nil
		}
		return false, fmt.Errorf("%w: string field vs %T", ErrTypeMismatch, lit)
	}
	if b1, ok := field.(bool); ok {
		if b2, ok := lit.(bool); ok {
			return b1 == b2, nil
		}
		return false, fmt.Errorf("%w: bool field vs %T", ErrTypeMismatch, lit)
	}
	return false, fmt.Errorf("%w: unsupported field kind %T", ErrTypeMismatch, field)
}

// scalarCmp returns -1 / 0 / +1 for field<lit / == / >. Same coercion
// rules as scalarEq but additionally requires ordered types.
func scalarCmp(field, lit any) (int, error) {
	if t1, ok := field.(time.Time); ok {
		t2, ok := lit.(time.Time)
		if !ok {
			return 0, fmt.Errorf("%w: time vs %T", ErrTypeMismatch, lit)
		}
		switch {
		case t1.Before(t2):
			return -1, nil
		case t1.After(t2):
			return 1, nil
		}
		return 0, nil
	}
	if f1, ok1 := numericTo64(field); ok1 {
		f2, ok2 := numericTo64(lit)
		if !ok2 {
			return 0, fmt.Errorf("%w: numeric field vs %T", ErrTypeMismatch, lit)
		}
		switch {
		case f1 < f2:
			return -1, nil
		case f1 > f2:
			return 1, nil
		}
		return 0, nil
	}
	if s1, ok := field.(string); ok {
		s2, ok := lit.(string)
		if !ok {
			return 0, fmt.Errorf("%w: string field vs %T", ErrTypeMismatch, lit)
		}
		return strings.Compare(s1, s2), nil
	}
	return 0, fmt.Errorf("%w: %T", ErrFieldNotComparable, field)
}

// numericTo64 coerces any signed/unsigned int or float to float64 for
// comparison. Returns (0, false) for non-numeric types.
func numericTo64(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
