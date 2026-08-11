// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"strings"
	"testing"

	"matrix/neo/internal/memory"
)

// captureWriteSkill is a test WriteSkillFunc that records the last spec it
// received and returns a fake Neocortex URI. It stands in for the pager-backed
// implementation wired in production.
type captureWriteSkill struct {
	called   bool
	lastSpec memory.PatternSpec
	lastURI  string
}

func (c *captureWriteSkill) writeSkill(ctx context.Context, spec memory.PatternSpec) (string, error) {
	c.called = true
	c.lastSpec = spec
	c.lastURI = "matrix://Neocortex/Pattern/test#1"
	return c.lastURI, nil
}

// TestWriteSkill_PersistsValidPattern verifies that the write_skill synthetic
// tool (P2-2) parses its arguments into a validated PatternSpec and dispatches
// it through the injected WriteSkillFunc, which persists it as a Neocortex
// Pattern. The spec must survive a full round-trip: the name, trigger,
// preconditions, steps, gotchas, and success_criteria the model sent must be
// the spec the write-back function receives.
func TestWriteSkill_PersistsValidPattern(t *testing.T) {
	cap := &captureWriteSkill{}
	m := &Manager{writeSkill: cap.writeSkill}

	args := map[string]interface{}{
		"name":             "deploy ERC-20 on Paxeer",
		"trigger":          "user asks to launch a token",
		"preconditions":    []interface{}{"FIREWORKS_API_KEY set", "agent wallet funded"},
		"steps":            []interface{}{"tachyon_compile", "tachyon_test", "tachyon_deploy"},
		"gotchas":          []interface{}{"pre-Cancun chain -> pin evm_version=shanghai"},
		"success_criteria": []interface{}{"receipt status=1", "balanceOf>0"},
	}

	out, isErr, err := m.Dispatch(context.Background(), WriteSkillTool, args)
	if err != nil {
		t.Fatalf("Dispatch returned transport error: %v", err)
	}
	if isErr {
		t.Fatalf("Dispatch returned in-band error for a valid spec: %q", out)
	}
	if !cap.called {
		t.Fatal("write_skill did not call the injected WriteSkillFunc")
	}

	// The spec must carry every field the model sent, verbatim.
	spec := cap.lastSpec
	if spec.Name != "deploy ERC-20 on Paxeer" {
		t.Errorf("name = %q", spec.Name)
	}
	if spec.Trigger != "user asks to launch a token" {
		t.Errorf("trigger = %q", spec.Trigger)
	}
	if len(spec.Preconditions) != 2 || spec.Preconditions[0] != "FIREWORKS_API_KEY set" {
		t.Errorf("preconditions = %v", spec.Preconditions)
	}
	if len(spec.Steps) != 3 || spec.Steps[2] != "tachyon_deploy" {
		t.Errorf("steps = %v", spec.Steps)
	}
	if len(spec.Gotchas) != 1 || spec.Gotchas[0] != "pre-Cancun chain -> pin evm_version=shanghai" {
		t.Errorf("gotchas = %v", spec.Gotchas)
	}
	if len(spec.SuccessCriteria) != 2 || spec.SuccessCriteria[1] != "balanceOf>0" {
		t.Errorf("success_criteria = %v", spec.SuccessCriteria)
	}

	// A spec with at least a name is non-empty (valid against PatternSpec).
	if spec.IsEmpty() {
		t.Error("a spec with a name must not be IsEmpty")
	}

	// The result must surface the URI so the agent can cite it.
	if !strings.Contains(out, cap.lastURI) {
		t.Errorf("write_skill result should contain the Neocortex URI %q; got %q", cap.lastURI, out)
	}
}

// TestWriteSkill_RejectsEmptySpec verifies the guard: a write_skill call with
// no usable content (empty name, trigger, and steps) is rejected in-band so the
// model reads the error and corrects rather than the harness retrying.
func TestWriteSkill_RejectsEmptySpec(t *testing.T) {
	cap := &captureWriteSkill{}
	m := &Manager{writeSkill: cap.writeSkill}

	out, isErr, err := m.Dispatch(context.Background(), WriteSkillTool, map[string]interface{}{})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !isErr {
		t.Fatalf("empty spec should be rejected in-band; got %q", out)
	}
	if cap.called {
		t.Error("WriteSkillFunc must NOT be called for an empty spec")
	}
	if !strings.Contains(strings.ToLower(out), "name") {
		t.Errorf("rejection message should hint that a name/steps is required; got %q", out)
	}
}

// TestWriteSkill_UnwiredReportsUnavailable verifies the nil-func path: when no
// write_skill function is wired, the tool reports it is unavailable in-band
// (matching the memory_recall / spawn_subagents convention).
func TestWriteSkill_UnwiredReportsUnavailable(t *testing.T) {
	m := &Manager{} // writeSkill == nil
	out, isErr, err := m.Dispatch(context.Background(), WriteSkillTool, map[string]interface{}{"name": "x"})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !isErr {
		t.Fatal("unwired write_skill should report unavailable in-band")
	}
	if !strings.Contains(strings.ToLower(out), "not available") && !strings.Contains(strings.ToLower(out), "unavailable") {
		t.Errorf("unwired message should say unavailable; got %q", out)
	}
}
