// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package writeback

import (
	"testing"

	"matrix/neo/internal/memory"
)

func TestParseLooseJSON(t *testing.T) {
	cases := []string{
		`{"facts":["a"],"patterns":[],"outcome":null}`,
		"```json\n{\"facts\":[\"a\"],\"patterns\":[],\"outcome\":null}\n```",
		"Sure! Here is the result:\n{\"facts\":[\"a\"],\"patterns\":[],\"outcome\":null}\nHope that helps.",
	}
	for i, in := range cases {
		var out extract
		if err := parseLooseJSON(in, &out); err != nil {
			t.Fatalf("case %d parse err: %v", i, err)
		}
		if len(out.Facts) != 1 || out.Facts[0] != "a" {
			t.Errorf("case %d: facts not extracted: %+v", i, out)
		}
	}

	var out extract
	if err := parseLooseJSON("no json here at all", &out); err == nil {
		t.Error("expected an error when there is no JSON object")
	}
}

func TestParseLooseJSONStructuredPattern(t *testing.T) {
	in := `{"facts":[],"patterns":[{"name":"deploy","trigger":"launch token","steps":["compile","deploy"],"gotchas":["MCOPY"],"success_criteria":["status=1"]}],"outcome":{"summary":"shipped","status":"success"}}`
	var out extract
	if err := parseLooseJSON(in, &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Patterns) != 1 {
		t.Fatalf("want 1 pattern, got %d", len(out.Patterns))
	}
	pj := out.Patterns[0]
	if pj.Name != "deploy" || len(pj.Steps) != 2 || pj.Steps[1] != "deploy" {
		t.Errorf("structured pattern not decoded: %+v", pj)
	}
	// the extracted pattern must map cleanly into the memory schema.
	spec := memory.PatternSpec{Name: pj.Name, Trigger: pj.Trigger, Steps: pj.Steps, Gotchas: pj.Gotchas, SuccessCriteria: pj.SuccessCriteria}
	if spec.IsEmpty() {
		t.Error("mapped PatternSpec should not be empty")
	}
	if out.Outcome == nil || out.Outcome.Status != "success" {
		t.Errorf("outcome not decoded: %+v", out.Outcome)
	}
}

func TestMapOutcome(t *testing.T) {
	if mapOutcome("success") != memory.OutcomeSuccess {
		t.Error("success")
	}
	if mapOutcome("failure") != memory.OutcomeFailure {
		t.Error("failure")
	}
	if mapOutcome("anything else") != memory.OutcomePartial {
		t.Error("default should be partial")
	}
}
