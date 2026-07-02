// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSheet() *TaskSheet {
	return &TaskSheet{
		TaskID:     "task-1.1",
		Title:      "Add greeting helper",
		Goal:       "greet.go exposes Greet(name) returning a friendly greeting",
		Acceptance: []string{"Greet(\"cody\") returns a non-empty string containing the name"},
		Grounding: Grounding{
			Files:    []string{"greet.go"},
			LineRefs: []string{"greet.go:1"},
			Notes:    "new file; module already initialized",
		},
		Constraints: Constraints{
			Constitution: []string{"no fakes", "verify before done"},
			ModePolicy:   "engineer",
			RulesRefs:    []string{"rules/golang/coding-style.md"},
		},
		Verify:      Verify{Commands: []string{"go test ./..."}, MustBeGreen: true},
		Deliverable: Deliverable{Shape: "greet.go + greet_test.go", DoNotTouch: []string{"go.mod"}},
		Attempt:     1,
	}
}

func TestTaskSheetValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*TaskSheet)
		wantErr string
	}{
		{"valid", func(s *TaskSheet) {}, ""},
		{"missing goal", func(s *TaskSheet) { s.Goal = " " }, "goal is empty"},
		{"missing acceptance", func(s *TaskSheet) { s.Acceptance = nil }, "acceptance criteria are empty"},
		{"missing verify", func(s *TaskSheet) { s.Verify.Commands = nil }, "verify commands are empty"},
		{"missing title", func(s *TaskSheet) { s.Title = "" }, "title is empty"},
		{"missing shape", func(s *TaskSheet) { s.Deliverable.Shape = "" }, "deliverable shape is empty"},
		{"missing id", func(s *TaskSheet) { s.TaskID = "" }, "task_id is empty"},
		{"unsafe id", func(s *TaskSheet) { s.TaskID = "../escape" }, "task id may contain only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSheet()
			tc.mutate(s)
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestTurnInReportValidate(t *testing.T) {
	green := Evidence{Command: "go test ./...", Exit: 0, OutputExcerpt: "ok"}
	red := Evidence{Command: "go test ./...", Exit: 1, OutputExcerpt: "FAIL"}
	cases := []struct {
		name    string
		report  TurnInReport
		wantErr string
	}{
		{
			"done with green evidence",
			TurnInReport{TaskID: "t1", Status: StatusDone, Verification: []Evidence{green}, Attempt: 1},
			"",
		},
		{
			"done with no evidence",
			TurnInReport{TaskID: "t1", Status: StatusDone, Attempt: 1},
			"done claimed with no verification evidence",
		},
		{
			"done with red evidence",
			TurnInReport{TaskID: "t1", Status: StatusDone, Verification: []Evidence{red}, Attempt: 1},
			"exited 1",
		},
		{
			"partial needs gaps",
			TurnInReport{TaskID: "t1", Status: StatusPartial, Attempt: 1},
			"requires honest gaps",
		},
		{
			"partial with gaps",
			TurnInReport{TaskID: "t1", Status: StatusPartial, Gaps: []string{"tests missing for edge case"}, Attempt: 1},
			"",
		},
		{
			"blocked with gaps",
			TurnInReport{TaskID: "t1", Status: StatusBlocked, Gaps: []string{"module does not build"}, Attempt: 1},
			"",
		},
		{
			"unknown status",
			TurnInReport{TaskID: "t1", Status: "victory", Attempt: 1},
			"not one of done|partial|blocked",
		},
		{
			"missing task id",
			TurnInReport{Status: StatusDone, Verification: []Evidence{green}},
			"task_id is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.report.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAllGreen(t *testing.T) {
	r := TurnInReport{TaskID: "t1", Status: StatusDone}
	if r.AllGreen() {
		t.Fatal("AllGreen() with no evidence must be false — absence of failure is not success")
	}
	r.Verification = []Evidence{{Command: "go build ./...", Exit: 0}, {Command: "go test ./...", Exit: 0}}
	if !r.AllGreen() {
		t.Fatal("AllGreen() = false with all-zero exits")
	}
	r.Verification = append(r.Verification, Evidence{Command: "go vet ./...", Exit: 2})
	if r.AllGreen() {
		t.Fatal("AllGreen() = true with a red entry")
	}
}

func TestStoreSheetRoundTrip(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sheet := validSheet()
	if err := st.SaveSheet(sheet); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadSheet(sheet.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != sheet.Goal || got.TaskID != sheet.TaskID || len(got.Acceptance) != 1 {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
	if got.Verify.Commands[0] != "go test ./..." || got.Deliverable.DoNotTouch[0] != "go.mod" {
		t.Fatalf("nested fields lost: %+v", got)
	}
	ids, err := st.ListSheets()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != sheet.TaskID {
		t.Fatalf("ListSheets() = %v", ids)
	}
}

func TestStoreRejectsInvalidSheet(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bad := validSheet()
	bad.Goal = ""
	if err := st.SaveSheet(bad); err == nil {
		t.Fatal("SaveSheet accepted a non-self-contained sheet")
	}
	if _, err := st.LoadSheet("../../etc/passwd"); err == nil {
		t.Fatal("LoadSheet accepted a path-traversal id")
	}
}

func TestStoreReportsPerAttempt(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r1 := &TurnInReport{TaskID: "t1", Status: StatusPartial, Gaps: []string{"lint red"}, Attempt: 1}
	r2 := &TurnInReport{TaskID: "t1", Status: StatusDone, Verification: []Evidence{{Command: "go test ./...", Exit: 0}}, Attempt: 2}
	other := &TurnInReport{TaskID: "t2", Status: StatusBlocked, Gaps: []string{"missing dep"}, Attempt: 1}
	for _, r := range []*TurnInReport{r2, r1, other} {
		if err := st.SaveReport(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.LoadReports("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Attempt != 1 || got[1].Attempt != 2 {
		t.Fatalf("LoadReports order wrong: %+v", got)
	}
	if got[0].Status != StatusPartial || got[1].Status != StatusDone {
		t.Fatalf("history mismatch: %+v", got)
	}
}

func TestStoreDurableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSheet(validSheet()); err != nil {
		t.Fatal(err)
	}
	// A fresh store over the same dir (the crashed-worker / restart path).
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st2.LoadSheet("task-1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal == "" {
		t.Fatal("sheet lost across reopen")
	}
	// No torn tmp files left behind.
	entries, err := os.ReadDir(filepath.Join(dir, "sheets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("torn tmp file left: %s", e.Name())
		}
	}
}
