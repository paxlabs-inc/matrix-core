// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package contract defines the two records the Cody engine is built around:
// the TaskSheet (orchestrator -> worker delegation contract) and the
// TurnInReport (worker -> orchestrator evidence contract). A sheet is
// SELF-CONTAINED — a fresh-context worker executes it with zero access to the
// orchestrator's conversation history — and both records are durable JSON so
// a crashed worker is replaceable from the same sheet and the orchestrator's
// lean window is reconstructable after a restart.
package contract

import (
	"errors"
	"fmt"
	"strings"
)

// TaskSheet is the self-contained delegation contract for exactly one task.
// It carries everything a fresh-context worker needs: what done means, how
// done is proven, where to look, and what must not be touched.
type TaskSheet struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	// Goal states what done means in one or two sentences.
	Goal string `json:"goal"`
	// Acceptance lists the testable acceptance criteria the turn-in is
	// adjudicated against.
	Acceptance  []string    `json:"acceptance"`
	Grounding   Grounding   `json:"grounding"`
	Constraints Constraints `json:"constraints"`
	Verify      Verify      `json:"verify"`
	Deliverable Deliverable `json:"deliverable"`
	// Attempt is 1 for the first dispatch and increments on re-dispatch.
	Attempt int `json:"attempt,omitempty"`
	// Feedback carries the orchestrator's concrete rejection feedback on a
	// re-dispatch; empty on the first attempt.
	Feedback string `json:"feedback,omitempty"`
	// Steers carries mid-run human direction the orchestrator folded in at a
	// boundary — a stop-and-ask answer or a live steer. Every sheet authored
	// after the direction lands carries it so a fresh-context worker honors it
	// (req 12.2). Ordered oldest-first; empty when the run has no direction.
	Steers []string `json:"steers,omitempty"`
	// UITask marks a sheet whose deliverable is user-facing UI. The gate holds
	// UI turn-ins to a higher bar: they must carry a screenshot artifact
	// (req 13.2) and, when a Design Language Record is in force, must not drift
	// from it (req 9.3).
	UITask bool `json:"ui_task,omitempty"`
	// ScreenshotCapable is the engine's typed capability signal (req 4.1):
	// true only when a screenshot can actually be captured in this
	// environment. When false, the worker is told NOT to produce screenshots
	// and the gate's screenshot screen degrades to a non-blocking advisory
	// (req 4.3) — a demand nothing can satisfy is never issued.
	ScreenshotCapable bool `json:"screenshot_capable,omitempty"`
}

// Grounding pins the sheet to exact seams in the workspace: the files the
// worker must read before writing, line-level references, and free-form notes.
type Grounding struct {
	Files    []string `json:"files,omitempty"`
	LineRefs []string `json:"line_refs,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// Constraints binds the worker: the constitution clauses (always), the active
// mode policy rendered as prose, and the applicable rules/<language> files.
type Constraints struct {
	Constitution []string `json:"constitution,omitempty"`
	ModePolicy   string   `json:"mode_policy,omitempty"`
	RulesRefs    []string `json:"rules_refs,omitempty"`
	// DesignLanguage, when set (UI tasks under a resolved Design Language
	// Record), is the binding visual-language constraint the worker must build
	// to and the gate screens the turn-in against (req 9.3).
	DesignLanguage string `json:"design_language,omitempty"`
}

// Verify lists the verification commands that must be green for the task to
// be accepted. The orchestrator independently re-runs these on turn-in.
type Verify struct {
	Commands    []string `json:"commands"`
	MustBeGreen bool     `json:"must_be_green"`
	TimeoutSecs int      `json:"timeout_secs,omitempty"`
	WorkingDir  string   `json:"working_dir,omitempty"`
}

// Deliverable describes the expected shape of the work and the explicit
// do-not-touch set (path prefixes/globs the worker must never modify).
type Deliverable struct {
	Shape      string   `json:"shape"`
	DoNotTouch []string `json:"do_not_touch,omitempty"`
}

// Validate enforces the self-containment bar: a sheet missing its goal,
// acceptance criteria, or verification commands cannot be executed by a
// fresh-context worker and is rejected before dispatch.
func (s *TaskSheet) Validate() error {
	var problems []string
	if strings.TrimSpace(s.TaskID) == "" {
		problems = append(problems, "task_id is empty")
	}
	if err := validateID(s.TaskID); s.TaskID != "" && err != nil {
		problems = append(problems, err.Error())
	}
	if strings.TrimSpace(s.Title) == "" {
		problems = append(problems, "title is empty")
	}
	if strings.TrimSpace(s.Goal) == "" {
		problems = append(problems, "goal is empty (the sheet must state what done means)")
	}
	if len(s.Acceptance) == 0 {
		problems = append(problems, "acceptance criteria are empty (the turn-in cannot be adjudicated)")
	}
	if len(s.Verify.Commands) == 0 {
		problems = append(problems, "verify commands are empty (acceptance requires a green verification record)")
	}
	if strings.TrimSpace(s.Deliverable.Shape) == "" {
		problems = append(problems, "deliverable shape is empty")
	}
	if len(problems) > 0 {
		return fmt.Errorf("task sheet %q is not self-contained: %s", s.TaskID, strings.Join(problems, "; "))
	}
	return nil
}

// Status is the worker's honest self-assessment of the turn-in. It is an
// input to the acceptance gate, never a substitute for it.
type Status string

const (
	// StatusDone means the worker believes every acceptance criterion is met
	// and verification is green. The orchestrator still re-verifies.
	StatusDone Status = "done"
	// StatusPartial means real progress with honest gaps; partials surface as
	// partials, never dressed up as done.
	StatusPartial Status = "partial"
	// StatusBlocked means the worker could not proceed (missing precondition,
	// ambiguity, deterministic failure) and explains why in Gaps.
	StatusBlocked Status = "blocked"
)

func (s Status) valid() bool {
	switch s {
	case StatusDone, StatusPartial, StatusBlocked:
		return true
	}
	return false
}

// Usage is a worker's LLM token accounting for one task attempt — the run
// ledger accumulates it per run so the metered spend is visible on the
// deliverable, not just in the gateway's books.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// Add folds another accounting into this one.
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
}

// Change records one workspace mutation the worker made.
type Change struct {
	Path string `json:"path"`
	// Kind is one of create|edit|delete|rename.
	Kind string `json:"kind"`
	Why  string `json:"why"`
}

// Evidence is one real verification command run: the command, its exit code,
// and an excerpt of its output. Fabricating evidence is pointless — the
// orchestrator re-runs the commands itself before accepting.
type Evidence struct {
	Command       string `json:"command"`
	Exit          int    `json:"exit"`
	OutputExcerpt string `json:"output_excerpt,omitempty"`
	// OverflowPath points at the full output on disk when the excerpt was
	// truncated (read-full discipline: the remainder is always retrievable).
	OverflowPath string `json:"overflow_path,omitempty"`
	// Screenshot is the workspace-relative path to a screenshot artifact
	// captured for a UI task — the design-quality evidence the gate requires on
	// UI turn-ins (req 13.2). Empty on non-UI evidence.
	Screenshot string `json:"screenshot,omitempty"`
}

// Green reports whether the evidence records a passing run.
func (e Evidence) Green() bool { return e.Exit == 0 }

// TurnInReport is the worker's structured turn-in: status, what changed and
// why, real verification evidence, and honest gaps/assumptions. It is the ONLY
// artifact of the worker that survives acceptance — the transcript is
// discarded.
type TurnInReport struct {
	TaskID       string     `json:"task_id"`
	Status       Status     `json:"status"`
	Summary      string     `json:"summary,omitempty"`
	Changes      []Change   `json:"changes,omitempty"`
	Verification []Evidence `json:"verification,omitempty"`
	Gaps         []string   `json:"gaps,omitempty"`
	Assumptions  []string   `json:"assumptions,omitempty"`
	// Attempt echoes the sheet attempt this report answers.
	Attempt int `json:"attempt,omitempty"`
	// Usage is the worker's LLM token spend for this attempt (engine-recorded).
	Usage Usage `json:"usage,omitempty"`
}

// Validate enforces the report contract: a known status, a matching task id,
// and — for a done claim — verification evidence to adjudicate. A done claim
// with red evidence is rejected here before it ever reaches the gate.
func (r *TurnInReport) Validate() error {
	var problems []string
	if strings.TrimSpace(r.TaskID) == "" {
		problems = append(problems, "task_id is empty")
	}
	if !r.Status.valid() {
		problems = append(problems, fmt.Sprintf("status %q is not one of done|partial|blocked", r.Status))
	}
	if r.Status == StatusDone {
		if len(r.Verification) == 0 {
			problems = append(problems, "done claimed with no verification evidence")
		}
		for _, ev := range r.Verification {
			if !ev.Green() {
				problems = append(problems, fmt.Sprintf("done claimed but %q exited %d", ev.Command, ev.Exit))
			}
		}
	}
	if (r.Status == StatusPartial || r.Status == StatusBlocked) && len(r.Gaps) == 0 {
		problems = append(problems, fmt.Sprintf("status %q requires honest gaps explaining what is missing", r.Status))
	}
	if len(problems) > 0 {
		return fmt.Errorf("turn-in report %q is invalid: %s", r.TaskID, strings.Join(problems, "; "))
	}
	return nil
}

// AllGreen reports whether every verification evidence entry passed. An empty
// evidence set is NOT green — absence of failure is not success.
func (r *TurnInReport) AllGreen() bool {
	if len(r.Verification) == 0 {
		return false
	}
	for _, ev := range r.Verification {
		if !ev.Green() {
			return false
		}
	}
	return true
}

var errBadID = errors.New("task id may contain only letters, digits, '.', '-', '_'")

// validateID keeps task ids safe as file names for the durable store.
func validateID(id string) error {
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("%w (got %q)", errBadID, id)
		}
	}
	return nil
}
