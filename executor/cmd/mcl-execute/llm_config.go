// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// llm_config.go — daemon-side helpers that thread sess#32 ambient-
// architect gateway routing into the per-call llm.Config produced by
// compile.go / synthesize.go / step_handler.go.
//
// Plan §5.16 reference. All four call sites (compile, synth, step,
// future-classifier) receive the same five gateway fields plus a
// shared cost-capture hook so /metrics and the Inbox UI see uniform
// X-Matrix-Cost-Pax telemetry regardless of which routed LLM the
// daemon invoked.
//
// Concurrency: the helper produces a fresh value per call; the
// returned closure (CostHook) is safe for concurrent invocation.

import (
	"net/http"
)

// llmGatewayBundle is the small sub-struct each per-call helper
// (compile / synth / step_handler) consumes. Empty fields preserve
// legacy direct-provider behaviour (the per-call helper checks
// GatewayURL == "" and skips the gateway branch).
type llmGatewayBundle struct {
	GatewayURL string
	ActorDID   string
	IntentID   string
	GoalID     string
	CostHook   func(http.Header)
}

// llmConfigFor returns the gateway bundle the daemon injects into a
// single LLM call. slot is "compiler" / "planner" / "executor"; kind
// is the executor sub-route (empty for non-executor slots). intentID
// + goalID flow through to the gateway as X-Matrix-{Intent,Goal}-ID
// headers AND into the cost-capture hook so Prometheus +
// transcript.intent.cost rollups carry the right tags.
//
// t is the per-message transcript (so the captured intent.cost event
// rides the right SSE channel). acc is optional: when non-nil the
// per-intent cost accumulator receives every captured cost so the
// lifecycle-driver terminal emits a clean intent.cost.summary event.
func (d *daemonState) llmConfigFor(slot, kind, intentID, goalID string, t *transcript, acc *intentCostAccumulator) llmGatewayBundle {
	if d == nil || d.gatewayURL == "" {
		// Legacy posture — leave every field zero; per-call helpers
		// short-circuit on GatewayURL == "" and use the existing
		// direct-provider path verbatim.
		return llmGatewayBundle{}
	}
	meta := intentCostMeta{
		IntentID:  intentID,
		GoalID:    goalID,
		Slot:      slot,
		KindRoute: kind,
	}
	return llmGatewayBundle{
		GatewayURL: d.gatewayURL,
		ActorDID:   d.actorDID,
		IntentID:   intentID,
		GoalID:     goalID,
		CostHook:   makeCostHook(t, d.metrics, acc, meta),
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
