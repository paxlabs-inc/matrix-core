// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import "testing"

func TestParseVerdict_FullSchema(t *testing.T) {
	raw := `{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": [], "assumptions": ["used default chain 125"], "open_unknowns": [], "certainty": 0.9, "rationale": "all deliverables produced"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Grounded || v.Coverage != CoverageFull || !v.CoverageComplete() || !v.Sound() {
		t.Fatalf("unexpected verdict: %#v", v)
	}
	if len(v.Assumptions) != 1 || v.Assumptions[0] != "used default chain 125" {
		t.Fatalf("assumptions not parsed: %#v", v.Assumptions)
	}
}

func TestParseVerdict_LegacyCriticShape(t *testing.T) {
	// The old criticVerdict {complete, missing, rationale} must still parse:
	// complete=false maps to coverage=partial.
	raw := `{"complete": false, "missing": ["deploy token"], "rationale": "only compiled"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Coverage != CoveragePartial {
		t.Fatalf("legacy complete=false should map to partial, got %q", v.Coverage)
	}
	if v.CoverageComplete() {
		t.Fatal("legacy incomplete verdict must not report CoverageComplete")
	}
	if len(v.Missing) != 1 || v.Missing[0] != "deploy token" {
		t.Fatalf("missing not parsed: %#v", v.Missing)
	}
}

func TestParseVerdict_LegacyCompleteTrue(t *testing.T) {
	v, err := ParseVerdict(`{"complete": true, "missing": []}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.CoverageComplete() {
		t.Fatalf("legacy complete=true should be CoverageComplete, got %#v", v)
	}
}

func TestParseVerdict_CodeFencesAndReasoning(t *testing.T) {
	raw := "Here is my audit.\n```json\n{\"coverage\": \"partial\", \"missing\": [\"mint the token\"]}\n```\nDone."
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse with fences: %v", err)
	}
	if v.Coverage != CoveragePartial || len(v.Missing) != 1 {
		t.Fatalf("fenced verdict not parsed: %#v", v)
	}
}

func TestParseVerdict_NestedBracesInStrings(t *testing.T) {
	raw := `{"coverage":"full","missing":[],"rationale":"result was {\"ok\": true} as expected"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse nested braces: %v", err)
	}
	if v.Coverage != CoverageFull {
		t.Fatalf("nested-brace verdict not parsed: %#v", v)
	}
}

func TestParseVerdict_CoherenceGuardsAppliedOnParse(t *testing.T) {
	// grounded=true with unverified claims -> g2 forces grounded false at parse.
	raw := `{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": ["the price is $5"], "rationale": "x"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Grounded {
		t.Fatal("expected g2 to force grounded false during parse")
	}
}

func TestParseVerdict_StrayBraceDoesNotShadowVerdict(t *testing.T) {
	// A reasoning model emits prose containing a stray brace group before the
	// real verdict. The scanner must skip the unrecognized object and pick the
	// verdict.
	raw := `Let me think about the schema {note: drop this}. Final verdict:
{"coverage": "partial", "missing": ["mint the token"], "rationale": "compiled only"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse with stray brace: %v", err)
	}
	if v.Coverage != CoveragePartial || len(v.Missing) != 1 || v.Missing[0] != "mint the token" {
		t.Fatalf("stray brace shadowed the real verdict: %#v", v)
	}
}

func TestParseVerdict_TrailingNoteAfterVerdict(t *testing.T) {
	raw := `{"coverage":"full","missing":[]}

Note: I am confident in this. {ignored: true}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse with trailing note: %v", err)
	}
	if !v.CoverageComplete() {
		t.Fatalf("trailing note must not change the verdict: %#v", v)
	}
}

func TestParseVerdict_MissingCoverageSignalFailsTowardRefusal(t *testing.T) {
	// req 7.1 (C2): a verdict that carries a grounding surface but OMITS the
	// coverage signal entirely (no coverage field, no legacy complete flag)
	// must NOT be read as full — the absent completeness signal fails toward
	// refusal (g4).
	raw := `{"grounded": true, "unverified_claims": [], "rationale": "looks fine"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Coverage != CoveragePartial {
		t.Fatalf("omitted coverage must fail toward refusal (partial), got %q", v.Coverage)
	}
	if v.CoverageComplete() || v.Sound() {
		t.Fatal("a verdict missing its coverage signal must not be accepted as complete")
	}
}

func TestParseVerdict_MissingGroundedSignalFailsTowardRefusal(t *testing.T) {
	// req 7.1 (C2): coverage is explicitly full and no unverified claims are
	// listed, but the grounded field is OMITTED. The absent grounding signal
	// fails toward refusal (g5): grounded=false, so the verdict is NOT Sound —
	// grounding is never inferred true from the mere absence of a listed
	// hallucination surface.
	raw := `{"coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.9}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Grounded {
		t.Fatal("omitted grounded field must fail toward refusal (grounded=false)")
	}
	if v.Sound() {
		t.Fatal("a verdict missing its grounded signal must not be Sound")
	}
	// The coverage half is still honoured — the MCL gate (CoverageComplete)
	// remains byte-identical for an explicit coverage=full.
	if !v.CoverageComplete() {
		t.Fatal("explicit coverage=full with no missing must still be CoverageComplete")
	}
}

func TestParseVerdict_EmptyObjectFailsOpen(t *testing.T) {
	// A content-free "{}" carries no verdict signal; ParseVerdict errors so the
	// caller fails OPEN (a critic hiccup never converts a clean run to failure).
	if _, err := ParseVerdict("{}"); err == nil {
		t.Fatal("expected error for a content-free object so the caller fails open")
	}
}

func TestParseVerdict_NoJSON(t *testing.T) {
	if _, err := ParseVerdict("I cannot produce JSON, sorry."); err == nil {
		t.Fatal("expected error when no JSON object is present")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
