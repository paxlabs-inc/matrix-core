// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cassandra"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/llmtest"
)

func seedWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func sheetFor(t *testing.T) *contract.TaskSheet {
	t.Helper()
	return &contract.TaskSheet{
		TaskID: "t1", Title: "demo", Goal: "make it work",
		Acceptance:  []string{"it works"},
		Verify:      contract.Verify{Commands: []string{"true"}, MustBeGreen: true},
		Deliverable: contract.Deliverable{Shape: "code"},
	}
}

const goTest = "package demo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {}\n"

func TestScreenPassesHonestWork(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{
		TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "demo.go", Kind: "create", Why: "the deliverable"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}},
	}
	if v := Screen(root, baseline, sheetFor(t), report); v != "" {
		t.Fatalf("honest work rejected: %q", v)
	}
}

func TestScreenRejectsDeletedTestFile(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "demo_test.go")); err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, baseline, sheetFor(t), report)
	if !strings.Contains(v, "deleted") {
		t.Fatalf("deleted test file not rejected: %q", v)
	}
}

func TestScreenRejectsRemovedTestCases(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	weakened := "package demo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(root, "demo_test.go"), []byte(weakened), 0o644); err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, baseline, sheetFor(t), report)
	if !strings.Contains(v, "lost test cases") {
		t.Fatalf("removed test cases not rejected: %q", v)
	}
}

func TestScreenRejectsAddedSkips(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	skipped := strings.Replace(goTest, "func TestB(t *testing.T) {}", "func TestB(t *testing.T) { t.Skip(\"later\") }", 1)
	if err := os.WriteFile(filepath.Join(root, "demo_test.go"), []byte(skipped), 0o644); err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, baseline, sheetFor(t), report)
	if !strings.Contains(v, "skip/disable") {
		t.Fatalf("added skip not rejected: %q", v)
	}
}

func TestScreenRejectsDoNotTouch(t *testing.T) {
	root := seedWorkspace(t, nil)
	sheet := sheetFor(t)
	sheet.Deliverable.DoNotTouch = []string{"vendor/"}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "vendor/lib.go", Kind: "edit", Why: "should not happen"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, TestBaseline{}, sheet, report)
	if !strings.Contains(v, "do-not-touch") {
		t.Fatalf("do-not-touch change not rejected: %q", v)
	}
}

// scriptedAdjudicator builds a REAL cassandra.Adjudicator whose decoder is the
// real llm.Client over a real SSE endpoint; only the verdict text is scripted.
func scriptedAdjudicator(t *testing.T, verdict string) *cassandra.Adjudicator {
	t.Helper()
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		return llmtest.Say(verdict)
	})
	t.Cleanup(srv.Close)
	return &cassandra.Adjudicator{Primary: NewLLMDecoder(llmtest.NewClient(t, srv))}
}

func doneReport() *contract.TurnInReport {
	return &contract.TurnInReport{
		TaskID: "t1", Status: contract.StatusDone, Summary: "did the work",
		Changes:      []contract.Change{{Path: "demo.go", Kind: "create", Why: "the deliverable"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0, OutputExcerpt: "ok"}},
	}
}

func TestAdjudicateAcceptsGroundedFull(t *testing.T) {
	adj := scriptedAdjudicator(t, `{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.9, "rationale": "the evidence satisfies the goal"}`)
	if v := Adjudicate(context.Background(), adj, sheetFor(t), doneReport(), "[GREEN exit 0] true\n"); v != "" {
		t.Fatalf("grounded full verdict rejected: %q", v)
	}
}

func TestAdjudicateRejectsUngroundedClaim(t *testing.T) {
	adj := scriptedAdjudicator(t, `{"grounded": false, "coverage": "full", "unverified_claims": ["claims real integration but wired a stubbed fake client"], "certainty": 0.8, "rationale": "the verification passes against a fake"}`)
	v := Adjudicate(context.Background(), adj, sheetFor(t), doneReport(), "[GREEN exit 0] true\n")
	if !strings.Contains(v, "unverified claim") || !strings.Contains(v, "fake") {
		t.Fatalf("ungrounded claim not rejected: %q", v)
	}
}

func TestAdjudicateRejectsPartialCoverage(t *testing.T) {
	adj := scriptedAdjudicator(t, `{"grounded": true, "coverage": "partial", "missing": ["acceptance criterion 2 was never exercised"], "certainty": 0.7}`)
	v := Adjudicate(context.Background(), adj, sheetFor(t), doneReport(), "[GREEN exit 0] true\n")
	if !strings.Contains(v, "missing") {
		t.Fatalf("partial coverage not rejected: %q", v)
	}
}

func TestAdjudicateFailsOpenOnMalformedVerdict(t *testing.T) {
	adj := scriptedAdjudicator(t, "I cannot decide right now, sorry — no JSON here.")
	if v := Adjudicate(context.Background(), adj, sheetFor(t), doneReport(), "[GREEN exit 0] true\n"); v != "" {
		t.Fatalf("adjudicator hiccup wedged the gate: %q", v)
	}
}

func TestAdjudicateNilAdjudicatorAccepts(t *testing.T) {
	if v := Adjudicate(context.Background(), nil, sheetFor(t), doneReport(), ""); v != "" {
		t.Fatalf("nil adjudicator rejected: %q", v)
	}
}
