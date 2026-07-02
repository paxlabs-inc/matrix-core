// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
)

// TestArchitectSpecFilesPersistedInWorkspace proves the Architect persistence:
// with SpecFiles enabled the plan is kept in sync as durable in-workspace
// spec files (EARS requirements + waved tasks with live statuses).
func TestArchitectSpecFilesPersistedInWorkspace(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t), SpecFiles: true,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			file, content := "greet.txt", "hello cody\n"
			if sheet.TaskID == "t2" {
				file, content = "reply.txt", "hello back\n"
			}
			if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "created " + file,
				Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0}},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	reqs, err := os.ReadFile(filepath.Join(root, ".cody", "spec", "requirements.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reqs), "SHALL satisfy: greet.txt contains hello cody") {
		t.Fatalf("requirements.md missing EARS acceptance:\n%s", reqs)
	}
	tasks, err := os.ReadFile(filepath.Join(root, ".cody", "spec", "tasks.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Wave 1", "## Wave 2", "- [x] t1", "- [x] t2", "verify: `grep -q \"hello cody\" greet.txt`"} {
		if !strings.Contains(string(tasks), want) {
			t.Fatalf("tasks.md missing %q:\n%s", want, tasks)
		}
	}
}
