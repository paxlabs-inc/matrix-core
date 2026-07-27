// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"

	neoidentity "matrix/neo/internal/identity"
)

// auditEventIdentityLeak is emitted on the audit side-channel when the model
// broke character and self-identified as its underlying LLM. It is a
// COMPLIANCE canary, not merely a brand check: Neo is an agent (a charter
// imposed on some foundation model), and a model that will not hold the
// simplest system rule — its own name — is signalling it may disregard the
// harder ones (money, safety, do-not-touch). Surfacing the breach keeps us from
// being blind to that; scrubbing (below) contains it; the guidance re-anchor
// makes the model self-correct in-loop.
const auditEventIdentityLeak = "identity.leak"

// agentName is the configured agent identity with the canonical "Neo"
// fallback — the single source the identity guardrails resolve the name from.
func (a *Agent) agentName() string {
	if n := strings.TrimSpace(a.cfg.AgentName); n != "" {
		return n
	}
	return "Neo"
}

// cleanContent applies the identity scrub to a user-facing content surface,
// discarding the leaked flag (the caller only wants the safe text). Used at the
// narration/answer surfaces where detection is handled separately.
func (a *Agent) cleanContent(text string) string {
	out, _ := scrubIdentity(a.agentName(), text)
	return out
}

// scrubIdentity rewrites any self-identification as an underlying model to the
// agent's own name and reports whether it changed anything (a detected leak).
// It is deterministic and display-safe: applied to reasoning glimpses, streamed
// deltas, and the delivered answer so a breach never reaches the user even when
// the prompt-level guardrail and the in-loop re-anchor both miss.
func scrubIdentity(name, text string) (string, bool) {
	return neoidentity.Scrub(name, text)
}

// identityReanchorNudge is the private guidance-channel steer used to re-anchor
// a model that broke character. It corrects the CURRENT turn and reasserts the
// charter's authority rather than only hiding the symptom.
func identityReanchorNudge(name string) string {
	return "You just referred to yourself as the underlying language model. You are " + name +
		" — that is your only identity, in your reply and in your reasoning. The model beneath you is infrastructure, not who you are. Re-anchor and answer as " + name +
		", and hold every other system rule just as firmly."
}
