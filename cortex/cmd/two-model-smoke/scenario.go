// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Built-in scenarios. Each scenario seeds the two agents with role-
// specific system prompts and produces an opening user message that
// kicks off the conversation.
//
// Add new scenarios as cases in scenarioByName.

package main

import "fmt"

// Scenario describes a two-agent conversation seed.
type Scenario struct {
	Name        string
	Description string
	// SystemA / SystemB are the system prompts for agents A and B
	// respectively. They establish role + tool-use posture without
	// repeating the tool catalog (the LLM already sees that via the
	// `tools` request field).
	SystemA string
	SystemB string
	// Opening is the first user-role message both agents share.
	Opening string
}

// scenarioByName returns the Scenario for the given name, or an error.
func scenarioByName(name string) (Scenario, error) {
	switch name {
	case "default", "":
		return defaultScenario(), nil
	}
	return Scenario{}, fmt.Errorf("scenario: unknown name %q", name)
}

// defaultScenario sets up collaborative knowledge construction. Agent
// A is the "Researcher": writes typed memories about a topic. Agent B
// is the "Reviewer": queries A's memories and asks for refinement /
// challenges contradictions / proposes edges.
//
// Both agents share one cortex actor; their identities (and the
// CreatedBy on each write) are "alice" (A, Researcher) and "bob" (B,
// Reviewer). They see each other's writes via cortex_find /
// cortex_list and can build a connected memory graph.
func defaultScenario() Scenario {
	const sharedRules = `Hard rules for tool use:

- Read the cortex_write tool description CAREFULLY before calling it.
  It contains the EXACT schemas for all 9 typed-memory shapes,
  including which fields are required, which are CLOSED ENUMS with
  fixed valid values, which are integers vs floats, and minimal
  example payloads. Copy a working example and adapt it; do not
  guess field names.
- Field names are PascalCase (Statement, Subject, Stance, ...).
  encoding/json matches case-insensitively but does not translate
  underscores: "evidence_for" will be silently ignored, use
  "EvidenceFor".
- When a tool returns {"error":"..."} READ THE ERROR. It tells you
  exactly which field is wrong (e.g. "Belief.stance invalid" means
  you used a value outside {believe, doubt, suspect}). Fix that
  specific field on the next call; do not just retry the same shape.
- Do NOT include base64 binary fields (Fact.Object, Identity.PublicKeys,
  Preference.Value, Capability.Parameters). Omit them.
- Tag your writes with 1-3 short lowercase tags so the other agent
  can discover them via cortex_find.
- When you reference a memory, USE THE EXACT URI returned by
  cortex_write or cortex_find. Never invent URIs.
- After enough writing/reading, end your turn with a brief plain-text
  summary of what you accomplished. Do not call tools forever.`

	systemA := `You are AGENT-A ("alice"), the Researcher in a two-agent cortex
smoke test. You and AGENT-B share one cortex memory store and can see
each other's writes.

Your job: build up a small structured knowledge base about the topic
the user provides. Write Facts, Beliefs, Patterns, and at least one
Goal. Tag them so AGENT-B can find them. When AGENT-B asks for
clarification or challenges a memory, respond by writing a refined
memory (cortex_update) or a corroborating one (cortex_write +
cortex_add_edge with edge_type=corroborates or contradicts).

` + sharedRules

	systemB := `You are AGENT-B ("bob"), the Reviewer in a two-agent cortex
smoke test. You and AGENT-A share one cortex memory store and can see
each other's writes.

Your job: read AGENT-A's writes via cortex_find and cortex_list, then
challenge or extend them. Write Beliefs that disagree (with edges
contradicts), Patterns that generalize across A's Facts (with edges
derived_from), and at least one Constraint flagging an over-claim.
Use cortex_update_head to retag A's memories if their tags are off.

` + sharedRules

	opening := `Topic: deterministic state machines in distributed systems.
AGENT-A (alice) goes first: write 3-5 typed memories establishing the
core ideas, then summarize in plain text what you wrote.
AGENT-B (bob) goes next: query alice's writes, challenge or extend at
least two of them, and add at least one edge between memories.`

	return Scenario{
		Name:        "default",
		Description: "Collaborative knowledge construction; alice writes, bob reviews + extends",
		SystemA:     systemA,
		SystemB:     systemB,
		Opening:     opening,
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
