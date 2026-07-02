// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteSpecFiles renders the plan as durable in-workspace spec files —
// requirements.md (the goals and acceptance criteria in EARS shape) and
// tasks.md (the waved checklist with live statuses) under .cody/spec/. This
// is Architect mode's persistence: the spec survives the session and a
// resumed orchestrator (or the user) reads the same durable plan the engine
// executes. The files live under .cody, so writing them never violates the
// orchestrator's never-writes-code invariant over the user's tree.
func WriteSpecFiles(root string, p *Plan) error {
	dir := filepath.Join(root, ".cody", "spec")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "requirements.md"), renderRequirements(p)); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "tasks.md"), renderTasks(p))
}

func renderRequirements(p *Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Requirements: %s\n", p.Goal)
	for _, t := range p.Tasks {
		fmt.Fprintf(&b, "\n## %s — %s\n\n", t.ID, t.Title)
		fmt.Fprintf(&b, "**Goal:** %s\n\n**Acceptance criteria:**\n", t.Goal)
		for i, a := range t.Acceptance {
			fmt.Fprintf(&b, "%d. THE deliverable SHALL satisfy: %s\n", i+1, a)
		}
	}
	return b.String()
}

func renderTasks(p *Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Implementation Plan: %s\n", p.Goal)
	tasks := append([]*Task{}, p.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Wave < tasks[j].Wave })
	wave := -1
	for _, t := range tasks {
		if t.Wave != wave {
			wave = t.Wave
			fmt.Fprintf(&b, "\n## Wave %d\n\n", wave)
		}
		mark := " "
		if t.Status == TaskDone {
			mark = "x"
		}
		reqs := ""
		if len(t.Requires) > 0 {
			reqs = " (requires " + strings.Join(t.Requires, ", ") + ")"
		}
		fmt.Fprintf(&b, "- [%s] %s %s — %s%s\n", mark, t.ID, t.Title, t.Status, reqs)
		for _, v := range t.Verify {
			fmt.Fprintf(&b, "  - verify: `%s`\n", v)
		}
	}
	return b.String()
}

func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
