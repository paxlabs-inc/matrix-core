// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package schema

import (
	"reflect"
	"testing"

	"centra/packages/construct/schema/primitives"
)

// oneOfEach returns one valid surface per frozen kind, exercising every
// builder + payload.
func oneOfEach() []*Surface {
	confirmed := true
	return []*Surface{
		NewNarration("n1", &primitives.Narration{Text: "thinking", Role: primitives.NarrationThinking}),
		NewMetric("m1", &primitives.Metric{Label: "Block height", Value: "1234567", Unit: "block", Magnitude: 1234567, Trend: primitives.TrendUp}),
		NewEntity("e1", &primitives.Entity{
			Type:        "tx",
			Identity:    "0xabc",
			Fields:      []primitives.EntityField{{Key: "from", Value: "0x01"}},
			Affordances: []primitives.Affordance{{ID: "sign", Label: "Sign", Kind: primitives.AffordanceAsk, AskRef: "ask1"}},
		}),
		NewStructure("s1", &primitives.Structure{
			Shape:   primitives.ShapeTable,
			Columns: []string{"name", "size"},
			Records: []primitives.StructureNode{{Cells: map[string]string{"name": "a.txt", "size": "10"}}},
		}),
		NewStream("st1", &primitives.Stream{Source: "terminal", Chunks: []primitives.StreamChunk{{Seq: 1, Text: "hello"}}}),
		NewTimeline("t1", &primitives.Timeline{Steps: []primitives.TimelineStep{{ID: "a", Label: "compile", Status: primitives.StepDone}}}),
		NewCanvas("c1", &primitives.Canvas{Media: primitives.CanvasMedia{Kind: primitives.MediaImage, URL: "https://x/y.png"}}),
		NewAsk("a1", &primitives.Ask{AskKind: primitives.AskConfirm, Prompt: "Proceed?", Required: true, Response: &primitives.AskResponse{Confirmed: &confirmed}}),
	}
}

func TestEachKindValidAndRoundTrips(t *testing.T) {
	for _, s := range oneOfEach() {
		if err := s.Validate(); err != nil {
			t.Fatalf("kind %s: unexpected invalid: %v", s.Kind, err)
		}
		b, err := s.Marshal()
		if err != nil {
			t.Fatalf("kind %s: marshal: %v", s.Kind, err)
		}
		got, err := ParseValid(b)
		if err != nil {
			t.Fatalf("kind %s: parse: %v", s.Kind, err)
		}
		if !reflect.DeepEqual(s, got) {
			t.Fatalf("kind %s: round-trip mismatch\n have %+v\n want %+v", s.Kind, got, s)
		}
	}
}

func TestKindsCoversFrozenEight(t *testing.T) {
	if len(Kinds) != 8 {
		t.Fatalf("expected 8 frozen kinds, got %d", len(Kinds))
	}
	want := map[Kind]bool{
		KindNarration: true, KindMetric: true, KindEntity: true, KindStructure: true,
		KindStream: true, KindTimeline: true, KindCanvas: true, KindAsk: true,
	}
	for _, k := range Kinds {
		if !want[k] {
			t.Fatalf("unexpected kind %q in Kinds", k)
		}
		if !ValidKind(k) {
			t.Fatalf("ValidKind(%q) = false", k)
		}
	}
}

func TestValidateRejectsUnknownKind(t *testing.T) {
	s := &Surface{Kind: "hologram", ID: "x"}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestValidateRejectsMissingID(t *testing.T) {
	s := NewNarration("", &primitives.Narration{Text: "hi"})
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestValidateRejectsKindPayloadMismatch(t *testing.T) {
	// Kind says metric but the payload is a narration.
	s := &Surface{Kind: KindMetric, ID: "x", Narration: &primitives.Narration{Text: "hi"}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for kind/payload mismatch")
	}
}

func TestValidateRejectsMultiplePayloads(t *testing.T) {
	s := &Surface{
		Kind:      KindMetric,
		ID:        "x",
		Metric:    &primitives.Metric{Label: "a", Value: "1"},
		Narration: &primitives.Narration{Text: "hi"},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for multiple payloads")
	}
}

func TestAttributesDecorateAnyKind(t *testing.T) {
	conf := 0.5
	for _, s := range oneOfEach() {
		s.WithRef("other").WithSeq(7).WithAttributes(&Attributes{
			Stakes:      StakesIrreversible,
			Confidence:  &conf,
			Cost:        &Cost{Amount: 0.17, Unit: "PAX"},
			Temporality: TemporalityPoint,
		})
		if err := s.Validate(); err != nil {
			t.Fatalf("kind %s: attributes should decorate any kind: %v", s.Kind, err)
		}
		b, err := s.Marshal()
		if err != nil {
			t.Fatalf("kind %s: marshal: %v", s.Kind, err)
		}
		got, err := ParseValid(b)
		if err != nil {
			t.Fatalf("kind %s: parse: %v", s.Kind, err)
		}
		if got.Ref != "other" || got.Seq != 7 || got.Attributes == nil {
			t.Fatalf("kind %s: decoration lost on round-trip: %+v", s.Kind, got)
		}
	}
}

func TestAttributesRejectsOutOfRangeConfidence(t *testing.T) {
	bad := 1.5
	a := &Attributes{Confidence: &bad}
	if a.Valid() {
		t.Fatal("expected confidence > 1 to be invalid")
	}
}

func TestValidateRejectsBadStructureShape(t *testing.T) {
	s := NewStructure("s", &primitives.Structure{Shape: "matrix", Records: nil})
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for unknown structure shape")
	}
}

func TestValidateRejectsBadAskKind(t *testing.T) {
	s := NewAsk("a", &primitives.Ask{AskKind: "telepathy", Prompt: "?"})
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for unknown ask kind")
	}
}
