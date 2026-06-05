// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import "testing"

// TestFrameConfidence covers the intent_frame@1 confidence parser used to
// trigger compiler escalation. Absent/unparseable confidence defaults to
// 1.0 (no spurious escalation); an explicit value is returned verbatim.
func TestFrameConfidence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"empty defaults high", "", 1.0},
		{"absent field defaults high", `{"verb":"build","objects":[]}`, 1.0},
		{"unparseable defaults high", `{not json`, 1.0},
		{"explicit low", `{"verb":"build","confidence":0.4}`, 0.4},
		{"explicit high", `{"verb":"build","confidence":0.95}`, 0.95},
		{"explicit zero", `{"verb":"build","confidence":0}`, 0.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := frameConfidence(c.in); got != c.want {
				t.Fatalf("frameConfidence(%q)=%v want %v", c.in, got, c.want)
			}
		})
	}
}

// TestFrameVerb verifies the frame verb extractor falls back to the
// caller-supplied verb when the frame omits or malforms it.
func TestFrameVerb(t *testing.T) {
	if got := frameVerb(`{"verb":"acquire"}`, "build"); got != "acquire" {
		t.Fatalf("got %q want acquire", got)
	}
	if got := frameVerb(`{"objects":[]}`, "build"); got != "build" {
		t.Fatalf("missing verb should fall back: got %q", got)
	}
	if got := frameVerb("", "build"); got != "build" {
		t.Fatalf("empty frame should fall back: got %q", got)
	}
	if got := frameVerb(`{bad`, "build"); got != "build" {
		t.Fatalf("unparseable frame should fall back: got %q", got)
	}
}

// TestEscalateReason maps the escalation trigger to its audit tag: an
// out-of-vocab (or empty) verb is a hard miss; otherwise low confidence.
func TestEscalateReason(t *testing.T) {
	if r := escalateReason("build"); r != "low_confidence" {
		t.Fatalf("valid verb -> low_confidence, got %q", r)
	}
	if r := escalateReason("frobnicate"); r != "invalid_verb" {
		t.Fatalf("invalid verb -> invalid_verb, got %q", r)
	}
	if r := escalateReason(""); r != "invalid_verb" {
		t.Fatalf("empty verb -> invalid_verb, got %q", r)
	}
}

// TestSynthModFallback locks the planner-model knob precedence: a
// dedicated plannerModel wins, else it falls back to the executorModel
// knob (single-knob back-compat), else empty (synthesize falls through to
// llm.DefaultPlannerModel()).
func TestSynthModFallback(t *testing.T) {
	d := &daemonState{plannerModel: "planner/x", executorModel: "exec/y"}
	if got := d.synthMod(); got != "planner/x" {
		t.Fatalf("plannerModel should win: got %q", got)
	}
	d = &daemonState{executorModel: "exec/y"}
	if got := d.synthMod(); got != "exec/y" {
		t.Fatalf("should fall back to executorModel: got %q", got)
	}
	d = &daemonState{}
	if got := d.synthMod(); got != "" {
		t.Fatalf("empty knobs -> empty: got %q", got)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
