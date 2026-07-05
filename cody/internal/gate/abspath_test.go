// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package gate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cassandra"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/llmtest"
)

const absMutexSrc = "package rl\n\nimport \"sync\"\n\ntype Limiter struct {\n\tmu       sync.Mutex\n\tcapacity float64\n}\n"

// TestRenderSourceIncludesAbsoluteInRootPath is the Y1 regression guard: an
// absolute in-root change path was previously skipped by renderSource
// (filepath.IsAbs -> continue), so the adjudicator never saw the source and a
// structurally-correct change looped to failure. After the fix, the absolute
// form is normalized through the single seam and its source appears in the
// evidence — identically to the relative form.
func TestRenderSourceIncludesAbsoluteInRootPath(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"ratelimiter.go": absMutexSrc})
	abs := filepath.Join(root, "ratelimiter.go")

	absReport := &contract.TurnInReport{
		TaskID: "t1", Status: contract.StatusDone, Summary: "added the mutex",
		Changes: []contract.Change{{Path: abs, Kind: "create", Why: "the deliverable"}},
	}
	relReport := &contract.TurnInReport{
		TaskID: "t1", Status: contract.StatusDone, Summary: "added the mutex",
		Changes: []contract.Change{{Path: "ratelimiter.go", Kind: "create", Why: "the deliverable"}},
	}

	absEv := BuildEvidence(root, absReport, "[GREEN exit 0] go build\n")
	relEv := BuildEvidence(root, relReport, "[GREEN exit 0] go build\n")

	if !strings.Contains(absEv, "mu       sync.Mutex") {
		t.Fatalf("absolute in-root change source missing from evidence:\n%s", absEv)
	}
	if !strings.Contains(absEv, "CHANGED FILES") {
		t.Fatalf("absolute-path evidence lacks the source section:\n%s", absEv)
	}
	// The relative form already worked; the two must carry the same source.
	if !strings.Contains(relEv, "mu       sync.Mutex") {
		t.Fatalf("relative change source missing from evidence:\n%s", relEv)
	}
}

// TestScreenScreensAbsolutePathDoNotTouch proves the do-not-touch screen
// compares normalized paths: an absolute in-root change to a do-not-touch file
// is rejected identically to its relative form (before the fix, the absolute
// path never matched the relative pattern and slipped through).
func TestScreenScreensAbsolutePathDoNotTouch(t *testing.T) {
	root := seedWorkspace(t, nil)
	sheet := sheetFor(t)
	sheet.Deliverable.DoNotTouch = []string{"vendor/"}
	abs := filepath.Join(root, "vendor", "lib.go")

	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: abs, Kind: "edit", Why: "should not happen"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, TestBaseline{}, sheet, report)
	if !strings.Contains(v, "do-not-touch") {
		t.Fatalf("absolute-path do-not-touch change not rejected: %q", v)
	}
}

// sourceSensingAdjudicator builds a REAL cassandra.Adjudicator over a real llm
// client and a real SSE endpoint whose only scripting is: ground the turn-in
// IFF the needle (a source line) actually reached the adjudicator in the
// evidence. This makes acceptance a true function of the evidence contents.
func sourceSensingAdjudicator(t *testing.T, needle string) *cassandra.Adjudicator {
	t.Helper()
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, needle) {
				return llmtest.Say(`{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.9}`)
			}
		}
		return llmtest.Say(`{"grounded": false, "coverage": "partial", "missing": ["no source in evidence to confirm the sync.Mutex field"], "certainty": 0.6}`)
	})
	t.Cleanup(srv.Close)
	return &cassandra.Adjudicator{Primary: NewLLMDecoder(llmtest.NewClient(t, srv))}
}

// TestAdjudicateGroundsOnAbsolutePathSource is the ac_4 end-to-end proof: a
// worker writes an absolute-path file; the gate's evidence carries that file's
// source; the REAL adjudicator grounds on it. The absolute and relative forms
// must both be accepted (the source reaches the adjudicator either way).
func TestAdjudicateGroundsOnAbsolutePathSource(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"ratelimiter.go": absMutexSrc})
	abs := filepath.Join(root, "ratelimiter.go")
	adj := sourceSensingAdjudicator(t, "sync.Mutex")

	for _, tc := range []struct {
		name string
		path string
	}{
		{"absolute-in-root", abs},
		{"relative", "ratelimiter.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &contract.TurnInReport{
				TaskID: "t1", Status: contract.StatusDone, Summary: "added the mutex field",
				Changes:      []contract.Change{{Path: tc.path, Kind: "create", Why: "the deliverable"}},
				Verification: []contract.Evidence{{Command: "true", Exit: 0}},
			}
			if v := Adjudicate(context.Background(), adj, root, sheetFor(t), report, "[GREEN exit 0] true\n"); v != "" {
				t.Fatalf("structurally-correct work rejected (adjudicator got no source): %q", v)
			}
		})
	}
}
