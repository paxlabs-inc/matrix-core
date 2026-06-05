// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package envelope

// Message kind constants — the closed set of 15 from
// research/02-protocol.md §5 + matrix.kvx MCL_MESSAGE_KINDS.
//
// NO chat.message kind by design (research/02 §1 thesis: prose-in /
// prose-out is the bug). All user input is intent.draft (new) or
// intent.answer / intent.correct (continuing).
const (
	// User → agent: initial NL goal + any pre-filled slots.
	KindIntentDraft = "intent.draft"

	// Agent → user: typed Intent IR for review.
	KindIntentCompiled = "intent.compiled"

	// Agent → user: structured questions for unknowns.
	KindIntentClarify = "intent.clarify"

	// User → agent: answers to clarify questions (slot patches).
	KindIntentAnswer = "intent.answer"

	// User → agent: signed sign-off on the IR. Lifecycle transition
	// proposed → accepted.
	KindIntentAccept = "intent.accept"

	// Agent → user: decomposition into steps before execution.
	KindPlanProposed = "plan.proposed"

	// Agent → agent/tool: execution of a single step (executor-internal).
	KindPlanStep = "plan.step"

	// Agent → user: streaming intermediate output (the only streaming kind).
	KindPlanOutput = "plan.output"

	// User → agent: patch an Intent or plan mid-flight.
	KindIntentCorrect = "intent.correct"

	// Agent → agent: sub-intent to a delegated agent.
	KindIntentDispatch = "intent.dispatch"

	// Agent → user/chain: signed completion receipt.
	KindIntentAttest = "intent.attest"

	// Agent → user: typed failure.
	KindIntentFail = "intent.fail"

	// User → agent: revoke before completion.
	KindIntentCancel = "intent.cancel"

	// Agent → user: human-in-loop checkpoint (rule-triggered).
	KindPolicyGate = "policy.gate"

	// User → agent: approve/deny gate.
	KindPolicyGateResolve = "policy.gate.resolve"
)

// AllKinds is the canonical ordered list of 15 kinds.
// Order matches research/02-protocol.md §5 table for documentation parity.
var AllKinds = []string{
	KindIntentDraft,
	KindIntentCompiled,
	KindIntentClarify,
	KindIntentAnswer,
	KindIntentAccept,
	KindPlanProposed,
	KindPlanStep,
	KindPlanOutput,
	KindIntentCorrect,
	KindIntentDispatch,
	KindIntentAttest,
	KindIntentFail,
	KindIntentCancel,
	KindPolicyGate,
	KindPolicyGateResolve,
}

// validKinds is the lookup set; built once at init.
var validKinds = func() map[string]bool {
	m := make(map[string]bool, len(AllKinds))
	for _, k := range AllKinds {
		m[k] = true
	}
	return m
}()

// ValidKind reports whether s is one of the 15 closed message kinds.
func ValidKind(s string) bool {
	return validKinds[s]
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
