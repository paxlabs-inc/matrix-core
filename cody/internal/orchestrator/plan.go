// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/edit"
)

// TaskStatus tracks a plan task through the loop.
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskDone       TaskStatus = "done"
	TaskFailed     TaskStatus = "failed"
)

// Task is one waved plan entry — the unit of delegation.
type Task struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Goal       string     `json:"goal"`
	Acceptance []string   `json:"acceptance"`
	Wave       int        `json:"wave"`
	Requires   []string   `json:"requires,omitempty"`
	Status     TaskStatus `json:"status"`
	// Grounding, Verify, and Deliverable feed the task sheet.
	Grounding   contract.Grounding   `json:"grounding"`
	Verify      []string             `json:"verify"`
	Deliverable contract.Deliverable `json:"deliverable"`
}

// Plan is the waved task list the orchestrator works through.
type Plan struct {
	Goal  string  `json:"goal"`
	Tasks []*Task `json:"tasks"`
}

// Validate rejects malformed plans before any dispatch.
func (p *Plan) Validate() error {
	if len(p.Tasks) == 0 {
		return fmt.Errorf("plan has no tasks")
	}
	byID := map[string]bool{}
	for _, t := range p.Tasks {
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("plan task with empty id")
		}
		if byID[t.ID] {
			return fmt.Errorf("duplicate plan task id %q", t.ID)
		}
		byID[t.ID] = true
		if t.Status == "" {
			t.Status = TaskPending
		}
	}
	for _, t := range p.Tasks {
		for _, req := range t.Requires {
			if !byID[req] {
				return fmt.Errorf("task %q requires unknown task %q", t.ID, req)
			}
		}
	}
	return nil
}

// Relativize rewrites absolute in-root paths across the plan into clean,
// workspace-relative form so the fresh-context worker and the acceptance gate
// are primed with the identical path shape (req 3.3, the Y1 upstream). This is
// the deterministic backstop to the planner's prompt: whatever shape the model
// emits, the plan that is persisted, published, and folded into every sheet
// carries relative paths. Path-list fields (grounding files, do-not-touch) are
// normalized element-wise through the single edit.Rel seam; free-form fields
// (plan/task goal, grounding notes, verify commands) have the absolute
// workspace-root prefix stripped so an in-root path becomes relative while
// verification still runs from the root (so a relativized command is
// unchanged in meaning). Paths that escape the root or are not in-root are
// left untouched. Idempotent.
func (p *Plan) Relativize(root string) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return
	}
	absRoot = filepath.Clean(absRoot)
	prefix := absRoot + string(filepath.Separator)
	relList := func(in []string) []string {
		for i, s := range in {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				continue
			}
			if rel, err := edit.Rel(absRoot, trimmed); err == nil {
				in[i] = rel
			}
		}
		return in
	}
	p.Goal = strings.ReplaceAll(p.Goal, prefix, "")
	for _, t := range p.Tasks {
		t.Goal = strings.ReplaceAll(t.Goal, prefix, "")
		t.Grounding.Notes = strings.ReplaceAll(t.Grounding.Notes, prefix, "")
		t.Grounding.Files = relList(t.Grounding.Files)
		t.Deliverable.DoNotTouch = relList(t.Deliverable.DoNotTouch)
		for i, cmd := range t.Verify {
			t.Verify[i] = strings.ReplaceAll(cmd, prefix, "")
		}
	}
}

// Get returns the task with the given id, or nil.
func (p *Plan) Get(id string) *Task {
	for _, t := range p.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// NextEligible selects the next task to dispatch: the lowest wave whose every
// required task is done and whose status is pending.
func (p *Plan) NextEligible() *Task {
	var best *Task
	for _, t := range p.Tasks {
		if t.Status != TaskPending {
			continue
		}
		ready := true
		for _, req := range t.Requires {
			if r := p.Get(req); r == nil || r.Status != TaskDone {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if best == nil || t.Wave < best.Wave {
			best = t
		}
	}
	return best
}

// Complete reports whether every task is done.
func (p *Plan) Complete() bool {
	for _, t := range p.Tasks {
		if t.Status != TaskDone {
			return false
		}
	}
	return true
}

// Render produces the compact plan digest held in the orchestrator's window.
func (p *Plan) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PLAN: %s\n", p.Goal)
	for _, t := range p.Tasks {
		reqs := ""
		if len(t.Requires) > 0 {
			reqs = " requires=" + strings.Join(t.Requires, ",")
		}
		fmt.Fprintf(&b, "- [%s] %s (wave %d%s): %s\n", t.Status, t.ID, t.Wave, reqs, t.Title)
	}
	return b.String()
}

const planFile = "plan.json"

// SavePlan durably persists the plan beside the sheets/reports (atomic).
func SavePlan(dir string, p *Plan) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, planFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadPlan reads the persisted plan; os.ErrNotExist when none was saved.
func LoadPlan(dir string) (*Plan, error) {
	data, err := os.ReadFile(filepath.Join(dir, planFile))
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("plan corrupt: %w", err)
	}
	return &p, nil
}
