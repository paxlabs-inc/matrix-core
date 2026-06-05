// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package ir

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidVerb(t *testing.T) {
	for _, v := range []string{"find", "acquire", "build", "modify", "deliver", "analyze", "negotiate", "schedule", "monitor", "delegate"} {
		if !ValidVerb(v) {
			t.Errorf("ValidVerb(%q) = false, want true", v)
		}
	}
	if !ValidVerb("x:brainstorm") {
		t.Error("ValidVerb(x:brainstorm) = false, want true")
	}
	if ValidVerb("brainstorm") {
		t.Error("ValidVerb(brainstorm) = true, want false")
	}
	if ValidVerb("") {
		t.Error("ValidVerb('') = true, want false")
	}
}

func TestIntentMarshalRoundTrip(t *testing.T) {
	intent := &Intent{
		ID:      "01HQ000000000000000000000",
		Version: "mcl/0.1",
		Actor:   "matrix://user/alice",
		Agent:   "matrix://agent/did:pax:0x123",
		Prose:   "find me a reliable GPU host",
		Frame: Frame{
			Verb: VerbAcquire,
			Objects: []SlotEntry{
				{Name: "service", Value: "gpu_inference", Type: "string"},
			},
			Constraints: []Constraint{
				{Type: "budget", Hard: true, Max: &AssetAmount{Asset: "PAX", Amount: 100}},
			},
			SuccessCriteria: []Predicate{
				{Type: "delivered", Artifact: "running inference endpoint"},
			},
			Preferences: []Preference{
				{Rank: "jurisdiction", Prefer: []string{"us-west", "eu-west"}},
			},
		},
		Unknowns: []Unknown{
			{
				ID:        "u1",
				Field:     "frame.objects.precision",
				Type:      "enum<fp8|bf16|fp16>",
				Severity:  SeverityPreferred,
				Rationale: "different precisions have different price/quality",
				Options:   []string{"fp8", "bf16", "fp16"},
			},
		},
		References: []Reference{
			{URI: "matrix://cortex/Fact/gpu-pricing#3", Type: "Fact", Summary: "GPU pricing data"},
		},
		State:      StateProposed,
		Confidence: 0.82,
		CreatedAt:  "2026-05-24T01:00:00Z",
		SignedBy:   "ed25519:abc123",
		Hash:       "deadbeef",
	}

	// Marshal
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}

	// Unmarshal
	var got Intent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if got.ID != intent.ID {
		t.Errorf("ID=%q, want %q", got.ID, intent.ID)
	}
	if got.Frame.Verb != VerbAcquire {
		t.Errorf("Verb=%q", got.Frame.Verb)
	}
	if len(got.Frame.Objects) != 1 {
		t.Errorf("Objects=%d", len(got.Frame.Objects))
	}
	if len(got.Unknowns) != 1 || got.Unknowns[0].ID != "u1" {
		t.Errorf("Unknowns mismatch")
	}
	if got.Confidence != 0.82 {
		t.Errorf("Confidence=%f", got.Confidence)
	}
}

func TestCanonicalJSONDeterministic(t *testing.T) {
	intent := &Intent{
		ID:      "01HQ000000000000000000000",
		Version: "mcl/0.1",
		Actor:   "matrix://user/alice",
		Agent:   "matrix://agent/did:pax:0x123",
		Prose:   "build a plan",
		Frame: Frame{
			Verb: VerbBuild,
			Objects: []SlotEntry{
				{Name: "target", Value: "database"},
			},
		},
		State:      StateProposed,
		Confidence: 0.90,
		CreatedAt:  "2026-05-24T01:00:00Z",
		SignedBy:   "ed25519:abc",
		Hash:       "",
	}

	// Encode twice — must be byte-identical
	b1, err := CanonicalJSON(intent)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := CanonicalJSON(intent)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("canonical JSON not deterministic:\n%s\nvs\n%s", b1, b2)
	}

	// Verify keys are sorted
	s := string(b1)
	actorIdx := strings.Index(s, `"actor"`)
	agentIdx := strings.Index(s, `"agent"`)
	frameIdx := strings.Index(s, `"frame"`)
	if actorIdx > agentIdx || agentIdx > frameIdx {
		t.Errorf("keys not sorted: actor@%d agent@%d frame@%d", actorIdx, agentIdx, frameIdx)
	}
}

func TestCanonicalJSONOmitsEmptyFields(t *testing.T) {
	intent := &Intent{
		ID:         "01HQ000000000000000000000",
		Version:    "mcl/0.1",
		Actor:      "matrix://user/alice",
		Agent:      "matrix://agent/did:pax:0x123",
		Prose:      "test",
		Frame:      Frame{Verb: VerbFind, Objects: []SlotEntry{{Name: "x", Value: "y"}}},
		State:      StateDraft,
		Confidence: 0.5,
		CreatedAt:  "2026-05-24T00:00:00Z",
		SignedBy:   "ed25519:xyz",
	}

	canonical, err := CanonicalJSON(intent)
	if err != nil {
		t.Fatal(err)
	}

	s := string(canonical)
	// Empty fields should be omitted
	if strings.Contains(s, `"parent"`) {
		t.Error("canonical JSON should omit empty parent")
	}
	if strings.Contains(s, `"unknowns"`) {
		t.Error("canonical JSON should omit empty unknowns")
	}
	if strings.Contains(s, `"budget"`) {
		t.Error("canonical JSON should omit nil budget")
	}
	if strings.Contains(s, `"deadline"`) {
		t.Error("canonical JSON should omit empty deadline")
	}
	if strings.Contains(s, `"goal_id"`) {
		t.Error("canonical JSON should omit empty goal_id (sess#32 backward-compat)")
	}
}

// TestIntent_GoalIDBackwardCompat covers the sess#32 ambient field. Empty
// GoalID must produce byte-identical canonical JSON / hash as a pre-sess#32
// Intent (omitempty), and a populated GoalID must round-trip and appear in
// the canonical bytes.
func TestIntent_GoalIDBackwardCompat(t *testing.T) {
	base := &Intent{
		ID:         "01HQ000000000000000000000",
		Version:    "mcl/0.1",
		Actor:      "matrix://user/alice",
		Agent:      "matrix://agent/did:pax:0x123",
		Prose:      "test",
		Frame:      Frame{Verb: VerbFind, Objects: []SlotEntry{{Name: "x", Value: "y"}}},
		State:      StateDraft,
		Confidence: 0.5,
		CreatedAt:  "2026-05-24T00:00:00Z",
		SignedBy:   "ed25519:xyz",
	}
	hashEmpty, err := Hash(base)
	if err != nil {
		t.Fatalf("hash empty: %v", err)
	}
	base.GoalID = ""
	hashStillEmpty, err := Hash(base)
	if err != nil {
		t.Fatalf("hash still-empty: %v", err)
	}
	if hashEmpty != hashStillEmpty {
		t.Fatalf("setting GoalID=\"\" must not shift hash:\n empty       = %s\n still-empty = %s", hashEmpty, hashStillEmpty)
	}
	base.GoalID = "01HQ000000000000000000G0AL"
	hashSet, err := Hash(base)
	if err != nil {
		t.Fatalf("hash set: %v", err)
	}
	if hashSet == hashEmpty {
		t.Fatalf("populated GoalID must change the hash")
	}
	canonical, err := CanonicalJSON(base)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if !strings.Contains(string(canonical), `"goal_id":"01HQ000000000000000000G0AL"`) {
		t.Fatalf("canonical lacks populated goal_id: %s", canonical)
	}
}

func TestHash(t *testing.T) {
	intent := &Intent{
		ID:         "01HQ000000000000000000000",
		Version:    "mcl/0.1",
		Actor:      "matrix://user/alice",
		Agent:      "matrix://agent/did:pax:0x123",
		Prose:      "build a plan",
		Frame:      Frame{Verb: VerbBuild, Objects: []SlotEntry{{Name: "x", Value: "y"}}},
		State:      StateProposed,
		Confidence: 0.90,
		CreatedAt:  "2026-05-24T01:00:00Z",
		SignedBy:   "ed25519:abc",
		Hash:       "will-be-cleared",
	}

	h1, err := Hash(intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 64 { // sha256 hex
		t.Fatalf("hash length=%d, want 64", len(h1))
	}

	// Hash should be stable
	h2, err := Hash(intent)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}

	// Original Hash field should be preserved
	if intent.Hash != "will-be-cleared" {
		t.Fatalf("Hash field mutated to %q", intent.Hash)
	}

	// Different intent should produce different hash
	intent2 := *intent
	intent2.Prose = "different prose"
	h3, err := Hash(&intent2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h3 {
		t.Fatal("different intents produced same hash")
	}
}

func TestD7ClosedVerbsComplete(t *testing.T) {
	expected := []string{"find", "acquire", "build", "modify", "deliver", "analyze", "negotiate", "schedule", "monitor", "delegate"}
	if len(D7ClosedVerbs) != 10 {
		t.Fatalf("D7ClosedVerbs has %d entries, want 10", len(D7ClosedVerbs))
	}
	for _, v := range expected {
		if !D7ClosedVerbs[v] {
			t.Errorf("missing verb %q", v)
		}
	}
}

func TestIntentStates(t *testing.T) {
	states := []string{StateDraft, StateProposed, StateClarifying, StateAccepted, StateExecuting, StateCompleted, StateFailed, StateCancelled}
	if len(states) != 8 {
		t.Fatalf("expected 8 states, got %d", len(states))
	}
	for _, s := range states {
		if s == "" {
			t.Error("empty state constant")
		}
	}
}

func TestConstraintTypes(t *testing.T) {
	c := Constraint{
		Type: "budget",
		Hard: true,
		Max:  &AssetAmount{Asset: "PAX", Amount: 50},
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"budget"`) {
		t.Errorf("type not serialized: %s", data)
	}
	if !strings.Contains(string(data), `"PAX"`) {
		t.Errorf("asset not serialized: %s", data)
	}
}

func TestCompileMetadata(t *testing.T) {
	intent := &Intent{
		ID:         "test",
		Version:    "mcl/0.1",
		Actor:      "a",
		Agent:      "b",
		Prose:      "test",
		Frame:      Frame{Verb: VerbBuild, Objects: []SlotEntry{{Name: "x", Value: "y"}}},
		State:      StateProposed,
		Confidence: 0.9,
		CreatedAt:  "2026-01-01T00:00:00Z",
		SignedBy:   "k",
		CompileMetadata: &CompileMetadata{
			Seed:               "abc123",
			MtxDigest:          "4ffdcc2f",
			ModelDigest:        "model-sha256",
			ModelVersion:       "gpt-oss-120b",
			Temperature:        0.0,
			Grammar:            "intent_frame@1",
			SkillID:            "writing-plans",
			SkillVersion:       "1.0.0",
			CortexSnapshotHash: "deadbeef",
		},
	}

	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "intent_frame@1") {
		t.Error("compile_metadata.grammar not serialized")
	}
	if !strings.Contains(string(data), "writing-plans") {
		t.Error("compile_metadata.skill_id not serialized")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
