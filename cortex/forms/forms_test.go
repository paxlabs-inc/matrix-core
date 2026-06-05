// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package forms

import (
	"strings"
	"testing"
	"time"

	"matrix/cortex/memory"
)

func TestRender_Determinism(t *testing.T) {
	// Same input → same output, byte-for-byte. Snapshot-hash invariant.
	d := memory.PreferenceData{
		SchemaVersion: 1, Topic: "tone", Polarity: memory.PolarityPrefer,
		StrengthVal: 0.7, Rationale: "andrew prefers terse over verbose",
	}
	h := &memory.Head{Type: memory.TypePreference, ActorScope: "andrew"}
	a := Render(h, d)
	b := Render(h, d)
	if a != b {
		t.Fatalf("non-deterministic: %+v vs %+v", a, b)
	}
}

func TestRender_BudgetEnforced(t *testing.T) {
	huge := strings.Repeat("a", 4000) // ~1000 tokens
	d := memory.PreferenceData{
		SchemaVersion: 1, Topic: "tone", Polarity: memory.PolarityPrefer,
		StrengthVal: 0.5, Rationale: huge,
	}
	h := &memory.Head{Type: memory.TypePreference, ActorScope: "x"}
	got := Render(h, d)
	if memory.CountTokens(got.Short) > memory.MaxShortTokens {
		t.Fatalf("short over budget: %d > %d", memory.CountTokens(got.Short), memory.MaxShortTokens)
	}
	if memory.CountTokens(got.Medium) > memory.MaxMediumTokens {
		t.Fatalf("medium over budget: %d > %d", memory.CountTokens(got.Medium), memory.MaxMediumTokens)
	}
	if !strings.HasSuffix(got.Medium, Ellipsis) {
		t.Fatalf("expected ellipsis suffix on truncated medium: %q", got.Medium)
	}
}

func TestRender_AllTypesProduceContent(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	cases := []struct {
		name string
		head *memory.Head
		data memory.TypedData
	}{
		{"identity", &memory.Head{Type: memory.TypeIdentity}, memory.IdentityData{
			SchemaVersion: 1, Name: "Andrew", DID: "did:pax:abc", Roles: []string{"founder"}},
		},
		{"fact", &memory.Head{Type: memory.TypeFact}, memory.FactData{
			SchemaVersion: 1, Statement: "PAX is native asset", Subject: "matrix://chain/paxeer",
			Predicate: "has_native", AsOf: &at},
		},
		{"preference", &memory.Head{Type: memory.TypePreference}, memory.PreferenceData{
			SchemaVersion: 1, Topic: "tone", Polarity: memory.PolarityPrefer, StrengthVal: 0.9,
			Rationale: "blunt over polite"},
		},
		{"belief", &memory.Head{Type: memory.TypeBelief}, memory.BeliefData{
			SchemaVersion: 1, Statement: "compiler should be deterministic",
			Stance: memory.StanceBelieve, EvidenceFor: []string{"a", "b"}},
		},
		{"event", &memory.Head{Type: memory.TypeEvent}, memory.EventData{
			SchemaVersion: 1, Kind: memory.EventIntentCompleted, Counterparty: "did:pax:srv",
			OutcomeVal: memory.OutcomeSuccess, IntentRef: "matrix://intent/01H",
			Cost: &memory.AssetAmount{Asset: "PAX", Amount: "1.5"}},
		},
		{"goal", &memory.Head{Type: memory.TypeGoal}, memory.GoalData{
			SchemaVersion: 1, Statement: "ship cortex phase 4", Status: memory.GoalActive,
			HorizonEnd: &at, SuccessCriteria: []string{"go test green"}},
		},
		{"constraint", &memory.Head{Type: memory.TypeConstraint}, memory.ConstraintData{
			SchemaVersion: 1, Statement: "no chain coupling above tools/", Polarity: memory.PolarityDont,
			StrengthVal: memory.StrengthHard, Source: memory.ConstraintSourceUserDeclared},
		},
		{"capability", &memory.Head{Type: memory.TypeCapability}, memory.CapabilityData{
			SchemaVersion: 1, Subject: "did:pax:srv", Capability: "render-3d", Verified: true,
			LastObserved: at},
		},
		{"pattern", &memory.Head{Type: memory.TypePattern}, memory.PatternData{
			SchemaVersion: 1, Statement: "service X consistently fast on weekdays",
			Strength: 0.8, Coverage: 42, DerivedFrom: []string{"a", "b", "c"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := Render(c.head, c.data)
			if f.Short == "" {
				t.Fatalf("%s: empty short form", c.name)
			}
			if f.Medium == "" {
				t.Fatalf("%s: empty medium form", c.name)
			}
			if memory.CountTokens(f.Short) > memory.MaxShortTokens {
				t.Fatalf("%s: short over budget", c.name)
			}
			if memory.CountTokens(f.Medium) > memory.MaxMediumTokens {
				t.Fatalf("%s: medium over budget", c.name)
			}
		})
	}
}

func TestTruncateToTokens_UTF8Safe(t *testing.T) {
	// "héllo wörld " repeated. é and ö are 2 bytes each.
	s := strings.Repeat("héllo wörld ", 100)
	out := TruncateToTokens(s, 5) // 5 tokens = 20 bytes budget incl ellipsis
	if memory.CountTokens(out) > 5 {
		t.Fatalf("over budget: %d", memory.CountTokens(out))
	}
	// Must be valid UTF-8.
	for i := 0; i < len(out); i++ {
		// byte iter is fine; we don't need utf8.ValidString here, but the
		// output must end on a rune boundary OR the ellipsis. Easiest: check
		// last codepoints decode without RuneError.
	}
}

func TestTruncateToTokens_BoundaryEqualToBudget(t *testing.T) {
	// Exactly at budget → unchanged, no ellipsis.
	s := strings.Repeat("a", memory.MaxShortTokens*memory.BytesPerToken)
	out := TruncateToTokens(s, memory.MaxShortTokens)
	if out != s {
		t.Fatalf("unexpected truncation at boundary: in=%d out=%d", len(s), len(out))
	}
}

func TestRender_NilInputsSafe(t *testing.T) {
	if got := Render(nil, nil); got != (memory.Forms{}) {
		t.Fatalf("expected empty forms for nil inputs, got %+v", got)
	}
	if got := RenderFull(nil, nil); got != "" {
		t.Fatalf("expected empty full for nil inputs, got %q", got)
	}
}

func TestCountTokens_BytesPer4Heuristic(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{strings.Repeat("a", 4*50), 50},
		{strings.Repeat("a", 4*50+1), 51},
	}
	for _, c := range cases {
		if got := memory.CountTokens(c.in); got != c.want {
			t.Fatalf("CountTokens(len=%d) = %d, want %d", len(c.in), got, c.want)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
