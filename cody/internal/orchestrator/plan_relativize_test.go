// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"path/filepath"
	"reflect"
	"testing"

	"matrix/cody/internal/contract"
)

// TestPlanRelativize proves the deterministic backstop for req 3.3 (Y1
// upstream): whatever shape the planner emits, an absolute in-root path in a
// task's goal, grounding files, verify commands, do-not-touch, or the plan
// goal is normalized to the identical workspace-relative shape the worker and
// gate use — while an already-relative path is unchanged and a path that
// escapes the root is left untouched.
func TestPlanRelativize(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	abs := func(rel string) string { return filepath.Join(root, rel) }

	tests := []struct {
		name string
		in   *Task
		want *Task
	}{
		{
			name: "absolute in-root paths become relative across every field",
			in: &Task{
				ID:    "t1",
				Title: "build the app",
				Goal:  "edit " + abs("src/App.tsx") + " to render the header",
				Grounding: contract.Grounding{
					Files: []string{abs("src/App.tsx"), abs("package.json")},
					Notes: "the entry point is " + abs("src/main.tsx"),
				},
				Verify:      []string{"cd " + abs("web") + " && npm test", "node " + abs("scripts/check.js")},
				Deliverable: contract.Deliverable{Shape: "a page", DoNotTouch: []string{abs("node_modules"), abs("package-lock.json")}},
			},
			want: &Task{
				ID:    "t1",
				Title: "build the app",
				Goal:  "edit src/App.tsx to render the header",
				Grounding: contract.Grounding{
					Files: []string{"src/App.tsx", "package.json"},
					Notes: "the entry point is src/main.tsx",
				},
				Verify:      []string{"cd web && npm test", "node scripts/check.js"},
				Deliverable: contract.Deliverable{Shape: "a page", DoNotTouch: []string{"node_modules", "package-lock.json"}},
			},
		},
		{
			name: "already-relative paths are unchanged (idempotent shape)",
			in: &Task{
				ID:          "t2",
				Title:       "tests",
				Goal:        "add a test in src/App.test.tsx",
				Grounding:   contract.Grounding{Files: []string{"src/App.tsx", "src/App.test.tsx"}},
				Verify:      []string{"npm test"},
				Deliverable: contract.Deliverable{Shape: "a test", DoNotTouch: []string{"dist/**"}},
			},
			want: &Task{
				ID:          "t2",
				Title:       "tests",
				Goal:        "add a test in src/App.test.tsx",
				Grounding:   contract.Grounding{Files: []string{"src/App.tsx", "src/App.test.tsx"}},
				Verify:      []string{"npm test"},
				Deliverable: contract.Deliverable{Shape: "a test", DoNotTouch: []string{"dist/**"}},
			},
		},
		{
			name: "a path escaping the root is left untouched",
			in: &Task{
				ID:          "t3",
				Title:       "escape",
				Goal:        "do a thing",
				Grounding:   contract.Grounding{Files: []string{"/etc/passwd", "src/ok.tsx"}},
				Verify:      []string{"true"},
				Deliverable: contract.Deliverable{Shape: "x", DoNotTouch: []string{"../outside.txt"}},
			},
			want: &Task{
				ID:          "t3",
				Title:       "escape",
				Goal:        "do a thing",
				Grounding:   contract.Grounding{Files: []string{"/etc/passwd", "src/ok.tsx"}},
				Verify:      []string{"true"},
				Deliverable: contract.Deliverable{Shape: "x", DoNotTouch: []string{"../outside.txt"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{Goal: "ship it", Tasks: []*Task{tc.in}}
			p.Relativize(root)
			got := p.Tasks[0]
			if got.Goal != tc.want.Goal {
				t.Errorf("goal:\n got  %q\n want %q", got.Goal, tc.want.Goal)
			}
			if got.Grounding.Notes != tc.want.Grounding.Notes {
				t.Errorf("notes:\n got  %q\n want %q", got.Grounding.Notes, tc.want.Grounding.Notes)
			}
			if !reflect.DeepEqual(got.Grounding.Files, tc.want.Grounding.Files) {
				t.Errorf("grounding files:\n got  %v\n want %v", got.Grounding.Files, tc.want.Grounding.Files)
			}
			if !reflect.DeepEqual(got.Verify, tc.want.Verify) {
				t.Errorf("verify:\n got  %v\n want %v", got.Verify, tc.want.Verify)
			}
			if !reflect.DeepEqual(got.Deliverable.DoNotTouch, tc.want.Deliverable.DoNotTouch) {
				t.Errorf("do_not_touch:\n got  %v\n want %v", got.Deliverable.DoNotTouch, tc.want.Deliverable.DoNotTouch)
			}
		})
	}
}

// TestPlanRelativizeIsIdempotent proves running the normalization twice yields
// the same plan — so a resumed/already-normalized plan is never corrupted.
func TestPlanRelativizeIsIdempotent(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	p := &Plan{
		Goal: "work on " + filepath.Join(root, "src"),
		Tasks: []*Task{{
			ID:          "t1",
			Title:       "t",
			Goal:        "edit " + filepath.Join(root, "src/App.tsx"),
			Grounding:   contract.Grounding{Files: []string{filepath.Join(root, "src/App.tsx")}},
			Verify:      []string{"go build ./..."},
			Deliverable: contract.Deliverable{Shape: "x", DoNotTouch: []string{filepath.Join(root, "vendor")}},
		}},
	}
	p.Relativize(root)
	once := &Plan{Goal: p.Goal, Tasks: []*Task{{
		ID: p.Tasks[0].ID, Title: p.Tasks[0].Title, Goal: p.Tasks[0].Goal,
		Grounding:   contract.Grounding{Files: append([]string{}, p.Tasks[0].Grounding.Files...)},
		Verify:      append([]string{}, p.Tasks[0].Verify...),
		Deliverable: contract.Deliverable{Shape: p.Tasks[0].Deliverable.Shape, DoNotTouch: append([]string{}, p.Tasks[0].Deliverable.DoNotTouch...)},
	}}}
	p.Relativize(root)
	if p.Goal != once.Goal {
		t.Errorf("plan goal not idempotent: got %q want %q", p.Goal, once.Goal)
	}
	if got, want := p.Tasks[0].Goal, once.Tasks[0].Goal; got != want {
		t.Errorf("task goal not idempotent: got %q want %q", got, want)
	}
	if !reflect.DeepEqual(p.Tasks[0].Grounding.Files, once.Tasks[0].Grounding.Files) {
		t.Errorf("grounding files not idempotent: got %v want %v", p.Tasks[0].Grounding.Files, once.Tasks[0].Grounding.Files)
	}
	if !reflect.DeepEqual(p.Tasks[0].Deliverable.DoNotTouch, once.Tasks[0].Deliverable.DoNotTouch) {
		t.Errorf("do_not_touch not idempotent: got %v want %v", p.Tasks[0].Deliverable.DoNotTouch, once.Tasks[0].Deliverable.DoNotTouch)
	}
}
