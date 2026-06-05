// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package query

import (
	"errors"
	"strings"
	"testing"
	"time"

	"matrix/cortex/memory"
)

// helper: wrap a Head + decoded Data into the pair the evaluator expects.
func wrapPref(t *testing.T, topic string, importance uint8, tags ...memory.Tag) (*memory.Memory, memory.TypedData) {
	t.Helper()
	d := memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         topic,
		Polarity:      memory.PolarityPrefer,
		StrengthVal:   0.7,
	}
	m := &memory.Memory{
		Head: memory.Head{
			ID:                 memory.NewID(),
			Type:               memory.TypePreference,
			CurrentVersion:     1,
			ActorScope:         "andrew",
			Visibility:         memory.VisPrivate,
			DeclaredImportance: importance,
			Tags:               tags,
			LastUpdatedAt:      time.Now().UTC(),
		},
		Version: memory.Version{
			ID:         memory.NewID(),
			Version:    1,
			Type:       memory.TypePreference,
			CreatedAt:  time.Now().UTC(),
			Confidence: 1.0,
		},
	}
	return m, d
}

func TestEvalEqOnDataField(t *testing.T) {
	m, d := wrapPref(t, "tone", 5)
	ev := newEvaluator()
	got, err := ev.eval(Eq{Field: "data.topic", Value: "tone"}, m, d)
	if err != nil || !got {
		t.Fatalf("Eq topic=tone: got=%v err=%v", got, err)
	}
	got, err = ev.eval(Eq{Field: "data.topic", Value: "tempo"}, m, d)
	if err != nil || got {
		t.Fatalf("Eq topic=tempo: got=%v err=%v", got, err)
	}
}

func TestEvalEqUnknownFieldIsFalsy(t *testing.T) {
	m, d := wrapPref(t, "tone", 5)
	ev := newEvaluator()
	got, err := ev.eval(Eq{Field: "data.bogus", Value: "x"}, m, d)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got {
		t.Fatalf("Eq on unknown field should be false")
	}
	// Ne on unknown field should be true (vacuously: nothing equals it).
	got, err = ev.eval(Ne{Field: "data.bogus", Value: "x"}, m, d)
	if err != nil || !got {
		t.Fatalf("Ne unknown: got=%v err=%v", got, err)
	}
}

func TestEvalGtErrorsOnUnknownField(t *testing.T) {
	m, d := wrapPref(t, "tone", 5)
	ev := newEvaluator()
	_, err := ev.eval(Gt{Field: "data.bogus", Value: 1}, m, d)
	if !errors.Is(err, ErrFieldUnknown) {
		t.Fatalf("expected ErrFieldUnknown, got %v", err)
	}
}

func TestEvalGtNumeric(t *testing.T) {
	m, d := wrapPref(t, "tone", 7)
	ev := newEvaluator()
	got, err := ev.eval(Gt{Field: "head.declared_importance", Value: 5}, m, d)
	if err != nil || !got {
		t.Fatalf("7>5: %v %v", got, err)
	}
	got, err = ev.eval(Gt{Field: "head.declared_importance", Value: 9}, m, d)
	if err != nil || got {
		t.Fatalf("7>9: %v %v", got, err)
	}
}

func TestEvalInString(t *testing.T) {
	m, d := wrapPref(t, "tone", 1)
	ev := newEvaluator()
	got, err := ev.eval(In{Field: "data.topic", Values: []any{"tempo", "tone", "tune"}}, m, d)
	if err != nil || !got {
		t.Fatalf("topic in {tempo,tone,tune}: %v %v", got, err)
	}
}

func TestEvalHasTag(t *testing.T) {
	m, d := wrapPref(t, "tone", 1, "personal", "voice")
	ev := newEvaluator()
	got, err := ev.eval(HasTag{Tag: "voice"}, m, d)
	if err != nil || !got {
		t.Fatalf("HasTag voice: %v %v", got, err)
	}
	got, err = ev.eval(HasTag{Tag: "missing"}, m, d)
	if err != nil || got {
		t.Fatalf("HasTag missing: %v %v", got, err)
	}
}

func TestEvalMatches(t *testing.T) {
	m, d := wrapPref(t, "tone-of-voice", 1)
	ev := newEvaluator()
	got, err := ev.eval(Matches{Field: "data.topic", Pattern: "^tone-"}, m, d)
	if err != nil || !got {
		t.Fatalf("matches ^tone-: %v %v", got, err)
	}
	got, err = ev.eval(Matches{Field: "data.topic", Pattern: "^xyz$"}, m, d)
	if err != nil || got {
		t.Fatalf("matches ^xyz$: %v %v", got, err)
	}
}

func TestEvalAndOrNot(t *testing.T) {
	m, d := wrapPref(t, "tone", 5, "personal")
	ev := newEvaluator()
	p := And{Children: []Predicate{
		Eq{Field: "data.topic", Value: "tone"},
		Or{Children: []Predicate{
			HasTag{Tag: "personal"},
			HasTag{Tag: "team"},
		}},
		Not{Inner: Gt{Field: "head.declared_importance", Value: 9}},
	}}
	got, err := ev.eval(p, m, d)
	if err != nil || !got {
		t.Fatalf("nested predicate failed: got=%v err=%v", got, err)
	}
}

func TestEvalTypeMismatchSurfaces(t *testing.T) {
	m, d := wrapPref(t, "tone", 5)
	ev := newEvaluator()
	_, err := ev.eval(Eq{Field: "data.topic", Value: 5}, m, d)
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
}

func TestFieldRefValidate(t *testing.T) {
	good := []FieldRef{"head.id", "version.created_at", "data.topic"}
	for _, f := range good {
		if err := f.Validate(); err != nil {
			t.Fatalf("%q: unexpected err %v", f, err)
		}
	}
	bad := []FieldRef{"", "head", "head.", ".id", "frame.x", "head.id.x"}
	for _, f := range bad {
		// "head.id.x" is technically OK shape-wise — namespace=head, field=id.x;
		// the resolver returns unknown. Skip from this set.
		if f == "head.id.x" {
			continue
		}
		if err := f.Validate(); err == nil {
			t.Fatalf("%q: expected validation error", f)
		}
	}
}

func TestPredicateStringStable(t *testing.T) {
	p := And{Children: []Predicate{
		Eq{Field: "data.topic", Value: "tone"},
		HasTag{Tag: "personal"},
	}}
	s := p.String()
	if !strings.Contains(s, "data.topic = tone") || !strings.Contains(s, "has_tag \"personal\"") {
		t.Fatalf("unexpected string form: %q", s)
	}
}

func TestCollectHasTags(t *testing.T) {
	p := And{Children: []Predicate{
		HasTag{Tag: "a"},
		And{Children: []Predicate{HasTag{Tag: "b"}}},
		Or{Children: []Predicate{HasTag{Tag: "c"}}}, // not collected (under Or)
		Not{Inner: HasTag{Tag: "d"}},                // not collected (under Not)
	}}
	got := collectHasTags(p)
	want := map[string]bool{"a": true, "b": true}
	if len(got) != len(want) {
		t.Fatalf("collected %v want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("collected %q not expected", g)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
