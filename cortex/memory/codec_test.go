// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"bytes"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTripAllTypes(t *testing.T) {
	cases := []TypedData{
		IdentityData{SchemaVersion: 1, Name: "Andrew", DID: "did:pax:owner"},
		FactData{SchemaVersion: 1, Statement: "PAX block time ~277ms", Subject: "matrix://chain/pax", Predicate: "block_time_ms"},
		PreferenceData{SchemaVersion: 1, Topic: "inference_precision", Polarity: PolarityPrefer, StrengthVal: 0.9},
		BeliefData{SchemaVersion: 1, Statement: "Pebble fits cortex", Subject: "matrix://decision/D17", Stance: StanceBelieve},
		EventData{SchemaVersion: 1, Kind: EventIntentCompleted, OutcomeVal: OutcomeSuccess, Summary: "boot"},
		GoalData{SchemaVersion: 1, Statement: "Ship Phase 2", Status: GoalActive},
		ConstraintData{SchemaVersion: 1, Statement: "no purple gradients", Polarity: PolarityDont, StrengthVal: StrengthHard, Source: ConstraintSourceUserDeclared},
		CapabilityData{SchemaVersion: 1, Subject: "matrix://agent/foo", Capability: "compile-intent", Verified: true, LastObserved: time.Unix(1, 0).UTC()},
		PatternData{SchemaVersion: 1, Statement: "Andrew prefers terse", Strength: 0.8, Coverage: 17},
	}

	for _, src := range cases {
		t.Run(src.memoryType().String(), func(t *testing.T) {
			enc, err := EncodeData(src)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := DecodeData(src.memoryType(), enc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.memoryType() != src.memoryType() {
				t.Fatalf("type mismatch: %v vs %v", got.memoryType(), src.memoryType())
			}
			// re-encode must be byte-identical (canonical form)
			enc2, err := EncodeData(got)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(enc, enc2) {
				t.Fatalf("non-canonical re-encode:\n a=%x\n b=%x", enc, enc2)
			}
		})
	}
}

func TestHashVersionStableAndDoesNotIncludeHashField(t *testing.T) {
	d := PreferenceData{SchemaVersion: 1, Topic: "tone", Polarity: PolarityPrefer, StrengthVal: 0.7}
	enc, err := EncodeData(d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	v := &Version{
		ID:         NewID(),
		Version:    1,
		Type:       TypePreference,
		Data:       enc,
		CreatedAt:  time.Unix(1700000000, 0).UTC(),
		CreatedBy:  "andrew",
		Confidence: 1.0,
		Provenance: Provenance{Source: SourceUserInput},
		Forms:      Forms{Short: "prefers tone=terse"},
	}
	h1, err := HashVersion(v)
	if err != nil {
		t.Fatalf("hash1: %v", err)
	}
	// HashVersion must restore the original Hash field after encoding.
	if v.Hash != ([32]byte{}) {
		t.Fatalf("HashVersion mutated v.Hash before set: %x", v.Hash)
	}
	v.Hash = h1
	// computing again with Hash already set must yield the same value
	h2, err := HashVersion(v)
	if err != nil {
		t.Fatalf("hash2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable: %x vs %x", h1, h2)
	}
}

func TestDecodeDataRejectsUnknownType(t *testing.T) {
	if _, err := DecodeData(Type(0xFF), []byte{0x40}); err == nil {
		t.Fatalf("expected ErrUnknownDataKind for unknown type")
	}
}

// TestEncodeEdgeRoundTrip: Phase 6 EdgeRecord canonical-CBOR round-trip
// preserves every field including the Tombstoned audit triple.
func TestEncodeEdgeRoundTrip(t *testing.T) {
	src := NewID()
	dst := NewID()
	at := time.Unix(1700000000, 0).UTC()
	rec := &EdgeRecord{
		Type:             EdgeContradicts,
		Src:              src,
		Dst:              dst,
		CreatedAt:        time.Unix(1690000000, 0).UTC(),
		CreatedBy:        "andrew",
		Weight:           0.42,
		Tombstoned:       true,
		TombstonedAt:     &at,
		TombstonedReason: "obsolete",
		TombstonedBy:     "system",
		Data:             []byte{0xDE, 0xAD},
	}
	enc, err := EncodeEdge(rec)
	if err != nil {
		t.Fatalf("EncodeEdge: %v", err)
	}
	var got EdgeRecord
	if err := DecodeEdge(enc, &got); err != nil {
		t.Fatalf("DecodeEdge: %v", err)
	}
	if got.Type != rec.Type || got.Src != rec.Src || got.Dst != rec.Dst {
		t.Fatalf("type/src/dst mismatch: %+v", got)
	}
	if got.CreatedBy != rec.CreatedBy || got.Weight != rec.Weight {
		t.Fatalf("createdBy/weight: %+v", got)
	}
	if !got.Tombstoned || got.TombstonedReason != "obsolete" || got.TombstonedBy != "system" {
		t.Fatalf("tombstone fields: %+v", got)
	}
	if got.TombstonedAt == nil || !got.TombstonedAt.Equal(at) {
		t.Fatalf("tombstoned-at: %+v", got.TombstonedAt)
	}
	if !bytes.Equal(got.Data, rec.Data) {
		t.Fatalf("data: %x vs %x", got.Data, rec.Data)
	}
	// Canonical re-encode is byte-identical.
	enc2, err := EncodeEdge(&got)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("non-canonical edge re-encode")
	}
}

func TestEdgeTypeNames(t *testing.T) {
	pairs := []struct {
		t EdgeType
		s string
	}{
		{EdgeDerivedFrom, "derived_from"},
		{EdgeReferences, "references"},
		{EdgeObservedBy, "observed_by"},
	}
	for _, p := range pairs {
		if p.t.String() != p.s {
			t.Fatalf("name(%d) = %q want %q", p.t, p.t.String(), p.s)
		}
		got, ok := ParseEdgeType(p.s)
		if !ok || got != p.t {
			t.Fatalf("ParseEdgeType(%q)=%v ok=%v", p.s, got, ok)
		}
	}
	if _, ok := ParseEdgeType("nope"); ok {
		t.Fatalf("ParseEdgeType accepted unknown")
	}
	if EdgeType(0).Valid() || EdgeType(0xFF).Valid() {
		t.Fatalf("Valid accepted out-of-range edge type")
	}
}

// TestGoalData_BackwardCompatDecode encodes a pre-sess#32 GoalData (only
// SchemaVersion / Statement / Status set) and verifies it decodes into the
// extended struct with all sess#32 fields zero-default. This proves old
// encoded Goals on disk decode forward without any migration.
func TestGoalData_BackwardCompatDecode(t *testing.T) {
	old := GoalData{SchemaVersion: 1, Statement: "Ship Phase 2", Status: GoalActive}
	enc, err := EncodeData(old)
	if err != nil {
		t.Fatalf("encode old GoalData: %v", err)
	}
	dec, err := DecodeData(TypeGoal, enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := dec.(GoalData)
	if !ok {
		t.Fatalf("decoded type %T, want GoalData", dec)
	}
	if got.SchemaVersion != 1 || got.Statement != "Ship Phase 2" || got.Status != GoalActive {
		t.Fatalf("legacy fields: %+v", got)
	}
	if got.VerbHint != "" || got.Objects != nil || got.Constraints != nil ||
		got.Budget != nil || got.Persistent || got.CreatedBy != "" {
		t.Fatalf("expected zero-default sess#32 fields, got %+v", got)
	}
}

// TestGoalData_NewFieldsRoundTrip covers all sess#32 ambient fields. The
// canonical re-encode must be byte-identical to the original encode.
func TestGoalData_NewFieldsRoundTrip(t *testing.T) {
	src := GoalData{
		SchemaVersion: 1,
		Statement:     "Build deployment pipeline",
		Status:        GoalActive,
		VerbHint:      "build",
		Objects: []GoalObjRef{
			{Kind: "repo", Ref: "matrix://chain/repo/main"},
			{Kind: "skill", Ref: "matrix://skill/deploy"},
		},
		Constraints: []GoalConstraint{
			{Type: "budget", Hard: true, Statement: "≤ 5 PAX/day"},
			{Type: "deadline", Hard: false, Statement: "by Friday"},
		},
		Budget: &GoalBudget{
			DailyPaxMax:      "5.0",
			TotalPaxMax:      "50.0",
			MaxIntentsPerDay: 20,
			MaxConcurrent:    2,
		},
		Persistent: true,
		CreatedBy:  "did:pax:owner",
	}
	enc, err := EncodeData(src)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := DecodeData(TypeGoal, enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := dec.(GoalData)
	if got.VerbHint != "build" || !got.Persistent || got.CreatedBy != "did:pax:owner" {
		t.Fatalf("scalar fields: %+v", got)
	}
	if len(got.Objects) != 2 || got.Objects[0].Ref != "matrix://chain/repo/main" {
		t.Fatalf("objects: %+v", got.Objects)
	}
	if len(got.Constraints) != 2 || !got.Constraints[0].Hard || got.Constraints[1].Hard {
		t.Fatalf("constraints: %+v", got.Constraints)
	}
	if got.Budget == nil || got.Budget.DailyPaxMax != "5.0" || got.Budget.MaxIntentsPerDay != 20 {
		t.Fatalf("budget: %+v", got.Budget)
	}
	enc2, err := EncodeData(got)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("non-canonical re-encode:\n a=%x\n b=%x", enc, enc2)
	}
}

func TestEncodeHeadVersionRoundTrip(t *testing.T) {
	id := NewID()
	h := &Head{
		ID:                 id,
		Type:               TypePreference,
		CurrentVersion:     1,
		ActorScope:         "andrew",
		Visibility:         VisPrivate,
		DeclaredImportance: 5,
		Tags:               []Tag{"prefs", "tone"},
		LastUpdatedAt:      time.Unix(1, 0).UTC(),
	}
	enc, err := EncodeHead(h)
	if err != nil {
		t.Fatalf("EncodeHead: %v", err)
	}
	var got Head
	if err := DecodeHead(enc, &got); err != nil {
		t.Fatalf("DecodeHead: %v", err)
	}
	if got.ID != h.ID || got.Type != h.Type || got.CurrentVersion != h.CurrentVersion ||
		got.Visibility != h.Visibility || got.DeclaredImportance != h.DeclaredImportance ||
		len(got.Tags) != len(h.Tags) {
		t.Fatalf("Head mismatch: %+v vs %+v", got, h)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
