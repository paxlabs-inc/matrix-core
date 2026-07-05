// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"matrix/cody/internal/decide"
	"matrix/cody/internal/llm"
)

// plannerSystem casts the model as the planning half of Cody prime. Depth
// guidance arrives per mode via the rendered policy; the JSON contract is
// fixed so the engine, not the model, owns the loop.
const plannerSystem = `You are Cody prime's planner. Decompose the user's coding request into a dependency-waved task list for ONE workspace.

Output ONLY a JSON object, no prose, no code fences:
{
  "goal": "one-line restatement of the request",
  "tasks": [
    {
      "id": "short-kebab-id",
      "title": "imperative title",
      "goal": "what done means for this task, one or two sentences",
      "acceptance": ["testable criterion", ...],
      "wave": 1,
      "requires": ["ids of tasks this depends on"],
      "grounding": {"files": ["files to read before writing"], "notes": "seam notes"},
      "verify": ["shell commands that must exit 0 for this task to be done"],
      "deliverable": {"shape": "what gets produced", "do_not_touch": ["paths the worker must not modify"]}
    }
  ]
}

Rules: every task must be independently executable by a fresh-context worker from its sheet alone; verify commands are the project's own (prefer the detected ones in the workspace context); waves order dependencies (same wave = independent); keep the plan as small as the request allows. Every path you emit — in goal, grounding.files, verify commands, and deliverable — MUST be workspace-relative (relative to the workspace root), never an absolute filesystem path; verification runs from the workspace root.

PROVABILITY (hard rule): every acceptance criterion MUST be demonstrable by the output of that task's verify commands — the acceptance gate grounds its judgment ONLY in what those commands print when re-run. Never author a criterion no listed command can prove (a claim like "the dev server starts" needs a bounded command that exercises it, e.g. a timeout-wrapped start + curl probe; "package X is installed" needs a command that greps the manifest or resolves the module). If a criterion cannot be checked by a bounded shell command, rewrite it as one that can, or drop it — an unprovable criterion wedges the task in rejection forever. Verify commands must be BOUNDED: never a bare long-running process (dev server, watcher) that hangs; wrap with timeout and background + probe + kill instead.`

// plannerCriteria is what the judge weighs candidate plans against when more
// than one survives parsing.
const plannerCriteria = "The plan that best decomposes THIS request into independently-verifiable, dependency-waved tasks each executable by a fresh-context worker from its sheet alone; prefer the smallest plan that still fully covers the request; reject a decomposition that ignores the request's specifics."

// plannerUser renders the planner's user prompt for a request.
func plannerUser(request, grounding, modePolicy string) string {
	var user strings.Builder
	fmt.Fprintf(&user, "REQUEST:\n%s\n", request)
	if modePolicy != "" {
		fmt.Fprintf(&user, "\nMODE POLICY (dial the plan depth accordingly):\n%s\n", modePolicy)
	}
	if grounding != "" {
		fmt.Fprintf(&user, "\nWORKSPACE CONTEXT:\n%s\n", grounding)
	}
	return user.String()
}

// PlanFromModel asks the model to author the waved task list for a request,
// grounded in the workspace summary, and validates the result. The engine
// owns everything else — the model only proposes the decomposition. This is
// the single-candidate path; PlanFromModelDivergent is the decision-phase path
// the engine uses (N divergent candidates judged).
func PlanFromModel(ctx context.Context, client *llm.Client, request, grounding, modePolicy string) (*Plan, error) {
	res, err := client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(plannerSystem),
		llm.UserMessage(plannerUser(request, grounding, modePolicy)),
	}})
	if err != nil {
		return nil, fmt.Errorf("planner: %w", err)
	}
	plan, err := parsePlan(res.Message.Content)
	if err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("planner: model produced an invalid plan: %w", err)
	}
	return plan, nil
}

// PlanFromModelDivergent authors the plan the decision way: generate
// `candidates` divergent plans HOT, keep the ones that parse and validate, and
// pick the best-fitting one COLD (req 10.2 — plan-shape authoring runs at
// decision temperature with N divergent candidates judged; workers stay cold).
// hot is the decision-temperature client; cold is the adjudication client. It
// falls back cleanly to the single-candidate contract when candidates <= 1.
func PlanFromModelDivergent(ctx context.Context, hot, cold *llm.Client, request, grounding, modePolicy string, candidates int) (*Plan, error) {
	if candidates <= 1 {
		return PlanFromModel(ctx, hot, request, grounding, modePolicy)
	}
	user := plannerUser(request, grounding, modePolicy)
	cands, err := decide.Generate(ctx, hot, plannerSystem, user, candidates)
	if err != nil {
		return nil, fmt.Errorf("planner: %w", err)
	}
	// Keep only candidates that parse into a valid plan.
	var valid []*Plan
	var judgeable []decide.Candidate
	for _, c := range cands {
		p, err := parsePlan(c.Text)
		if err != nil {
			continue
		}
		if err := p.Validate(); err != nil {
			continue
		}
		judgeable = append(judgeable, decide.Candidate{Index: len(valid), Text: c.Text})
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("planner: model produced no valid plan across %d candidate(s)", len(cands))
	}
	if len(valid) == 1 {
		return valid[0], nil
	}
	dec, err := decide.Judge(ctx, cold, "Coding request to plan:\n"+request, plannerCriteria, judgeable)
	if err != nil {
		return valid[0], nil // fail-safe to the first valid plan
	}
	pick := dec.Pick
	if pick < 0 || pick >= len(valid) {
		pick = 0
	}
	return valid[pick], nil
}

// adoptSystem casts the model as the planning half of Cody prime ADOPTING an
// existing specification document as the plan — not decomposing a prose request
// from scratch. The document is the source of truth: its requirements map onto
// task acceptance criteria and its own structure (sections/phases/waves) drives
// the decomposition where present (req 11.1). The JSON contract is identical to
// the from-scratch planner so the engine owns the loop unchanged.
const adoptSystem = `You are Cody prime's planner ADOPTING an existing SPECIFICATION DOCUMENT as the plan. The document is the source of truth — do NOT invent a different plan. Map the document's requirements/acceptance criteria onto task acceptance criteria, preserving its wording where stated. Derive the tasks and their dependency waves from the document's OWN structure (its sections, phases, or explicit task/wave list) where present; only decompose further where the document leaves a step implicit.

Output ONLY a JSON object, no prose, no code fences:
{
  "goal": "the document's overall objective, one line",
  "tasks": [
    {
      "id": "short-kebab-id (prefer the document's own task ids where it has them)",
      "title": "imperative title",
      "goal": "what done means for this task, from the document",
      "acceptance": ["the document's acceptance criteria for this task"],
      "wave": 1,
      "requires": ["ids of tasks this depends on"],
      "grounding": {"files": ["files to read before writing"], "notes": "seam notes from the document"},
      "verify": ["shell commands that must exit 0 for this task to be done"],
      "deliverable": {"shape": "what gets produced", "do_not_touch": ["paths the worker must not modify"]}
    }
  ]
}

Rules: every task must be independently executable by a fresh-context worker from its sheet alone; preserve the document's requirement wording in acceptance criteria; preserve the document's waves/phases as the dependency waves where it defines them; verify commands are the project's own (prefer detected ones); stay faithful to the document — never drop or invent scope. Every path you emit — in goal, grounding.files, verify commands, and deliverable — MUST be workspace-relative (relative to the workspace root), never an absolute filesystem path; verification runs from the workspace root.

PROVABILITY (hard rule): every acceptance criterion MUST be demonstrable by the output of that task's verify commands — the acceptance gate grounds its judgment ONLY in what those commands print when re-run. Author verify commands that exercise each criterion (a "server starts" criterion needs a timeout-wrapped start + curl probe; "package X present" needs a manifest grep). Verify commands must be BOUNDED: never a bare long-running process (dev server, watcher) that hangs; wrap with timeout and background + probe + kill instead.`

// adoptCriteria is what the judge weighs candidate adoptions against.
const adoptCriteria = "The plan that most faithfully adopts THIS specification document — every stated requirement mapped to a task's acceptance criteria, the document's own waves/phases preserved, nothing dropped or invented; reject a plan that paraphrases loosely or reshapes the document's structure without cause."

// adoptUser renders the adoption prompt: the spec document is the primary
// content, with the workspace context and mode policy as secondary framing.
func adoptUser(specDoc, grounding, modePolicy string) string {
	var user strings.Builder
	fmt.Fprintf(&user, "SPECIFICATION DOCUMENT TO ADOPT:\n%s\n", strings.TrimSpace(specDoc))
	if modePolicy != "" {
		fmt.Fprintf(&user, "\nMODE POLICY (dial the plan depth accordingly):\n%s\n", modePolicy)
	}
	if grounding != "" {
		fmt.Fprintf(&user, "\nWORKSPACE CONTEXT:\n%s\n", grounding)
	}
	return user.String()
}

// AdoptSpecDivergent adopts an existing spec document as the plan the decision
// way: generate `candidates` faithful adoptions HOT, keep the ones that parse
// and validate, and pick the most faithful COLD (req 10.2, 11.1). Falls back to
// a single adoption when candidates <= 1.
func AdoptSpecDivergent(ctx context.Context, hot, cold *llm.Client, specDoc, grounding, modePolicy string, candidates int) (*Plan, error) {
	user := adoptUser(specDoc, grounding, modePolicy)
	if candidates <= 1 {
		res, err := hot.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
			llm.SystemMessage(adoptSystem),
			llm.UserMessage(user),
		}})
		if err != nil {
			return nil, fmt.Errorf("planner: adopt spec: %w", err)
		}
		plan, err := parsePlan(res.Message.Content)
		if err != nil {
			return nil, err
		}
		if err := plan.Validate(); err != nil {
			return nil, fmt.Errorf("planner: adopted an invalid plan: %w", err)
		}
		return plan, nil
	}
	cands, err := decide.Generate(ctx, hot, adoptSystem, user, candidates)
	if err != nil {
		return nil, fmt.Errorf("planner: adopt spec: %w", err)
	}
	var valid []*Plan
	var judgeable []decide.Candidate
	for _, c := range cands {
		p, err := parsePlan(c.Text)
		if err != nil {
			continue
		}
		if err := p.Validate(); err != nil {
			continue
		}
		judgeable = append(judgeable, decide.Candidate{Index: len(valid), Text: c.Text})
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("planner: model produced no valid adoption across %d candidate(s)", len(cands))
	}
	if len(valid) == 1 {
		return valid[0], nil
	}
	dec, err := decide.Judge(ctx, cold, "Specification document being adopted (excerpt):\n"+excerpt(specDoc, 2000), adoptCriteria, judgeable)
	if err != nil {
		return valid[0], nil
	}
	pick := dec.Pick
	if pick < 0 || pick >= len(valid) {
		pick = 0
	}
	return valid[pick], nil
}

// excerpt returns at most n runes of s, for keeping a large spec document from
// dominating the judge's window.
func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n… (truncated)"
}

// parsePlan extracts the first balanced JSON object from the model output
// (tolerating fences and surrounding prose) and unmarshals the plan.
func parsePlan(raw string) (*Plan, error) {
	obj := firstJSONObject(raw)
	if obj == "" {
		return nil, fmt.Errorf("planner: no JSON object in model output")
	}
	var p Plan
	if err := json.Unmarshal([]byte(obj), &p); err != nil {
		return nil, fmt.Errorf("planner: parse plan: %w", err)
	}
	return &p, nil
}

// firstJSONObject returns the first balanced {...} span in s, string- and
// escape-aware, or "".
func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
