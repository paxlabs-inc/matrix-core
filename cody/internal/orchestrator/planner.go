// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

Rules: every task must be independently executable by a fresh-context worker from its sheet alone; verify commands are the project's own (prefer the detected ones in the workspace context); waves order dependencies (same wave = independent); keep the plan as small as the request allows.`

// PlanFromModel asks the model to author the waved task list for a request,
// grounded in the workspace summary, and validates the result. The engine
// owns everything else — the model only proposes the decomposition.
func PlanFromModel(ctx context.Context, client *llm.Client, request, grounding, modePolicy string) (*Plan, error) {
	var user strings.Builder
	user.WriteString("REQUEST:\n" + request + "\n")
	if modePolicy != "" {
		user.WriteString("\nMODE POLICY (dial the plan depth accordingly):\n" + modePolicy + "\n")
	}
	if grounding != "" {
		user.WriteString("\nWORKSPACE CONTEXT:\n" + grounding + "\n")
	}
	res, err := client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(plannerSystem),
		llm.UserMessage(user.String()),
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
