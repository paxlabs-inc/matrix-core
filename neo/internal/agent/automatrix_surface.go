// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import "matrix/neo/internal/llm"

// NewAutomatrix builds an Agent for an autonomous Automatrix run on the
// RESTRICTED (no-money) tool surface. It is a thin wrapper over New that FORCES
// RestrictTools — the existing sub-agent restricted-surface mechanism — so the
// money/chain core_execute delegate is structurally absent from the advertised
// tool schema set (req.3.2).
//
// Constructing the run through this helper means the restriction is a property
// of the constructor, not a caller responsibility: the engine cannot
// accidentally dispatch a proactive run on the full, money-capable surface.
func NewAutomatrix(o Options) *Agent {
	o.RestrictTools = true
	o.Automatrix = true
	return New(o)
}

// AdvertisedTools returns a copy of the exact tool schema set this agent
// advertises to the model (the slice passed as ChatRequest.Tools). It exists so
// the Automatrix structural guarantee can be asserted against the REAL surface
// the model would see, not a reconstruction of it.
func (a *Agent) AdvertisedTools() []llm.Tool {
	out := make([]llm.Tool, len(a.schemas))
	copy(out, a.schemas)
	return out
}
