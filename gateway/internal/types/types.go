// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package types holds the shared header names, request/response wire
// shapes, and small value structs used across the gateway internals.
//
// Single source of truth: every other internal package consumes header
// names from here so a rename never produces a silent header-mismatch
// regression. The daemon-side counterpart (MCL/llm + the executor
// daemon) hard-codes the same strings; if these change, both ends MUST
// update together.
package types

// HTTP header names — gateway wire contract. All headers are
// case-insensitive per RFC 7230, but we standardise on canonical
// casing here to keep grep-ability + Prometheus labels stable.
const (
	// HeaderAuthorization carries the Bearer ${MATRIX_GATEWAY_TOKEN}
	// shared secret. Required on every gateway request.
	HeaderAuthorization = "Authorization"

	// HeaderActorDID identifies the wallet/DID making the request.
	// Required. Used as the credit_ledger.actor_did key for rate-limit
	// + budget bookkeeping.
	HeaderActorDID = "X-Matrix-Actor-DID"

	// HeaderIntentID is the Matrix Intent.ID of the call site.
	// Required when present; used for cost telemetry per-intent
	// aggregation.
	HeaderIntentID = "X-Matrix-Intent-ID"

	// HeaderGoalID is the standing Goal memory ID this intent rolls
	// up under (sess#32). Optional.
	HeaderGoalID = "X-Matrix-Goal-ID"

	// HeaderSlot identifies the model slot the daemon resolved for
	// this call: compiler|planner|executor|liaison|neo. Drives the
	// free-tier whitelist in routing.go.
	HeaderSlot = "X-Matrix-Slot"

	// HeaderKindRoute identifies the executor sub-route (reason|code|
	// summarize|write|transform|classify|hard_reason). Optional;
	// only meaningful when Slot=executor.
	HeaderKindRoute = "X-Matrix-Kind-Route"

	// HeaderBYOAPIKey is "true" when the caller wants to bypass
	// metering using their own provider API key. The actual key is
	// passed in HeaderUserAPIKey.
	HeaderBYOAPIKey = "X-Matrix-BYO-API-Key"

	// HeaderUserAPIKey carries the caller's BYO Fireworks/Together
	// API key when HeaderBYOAPIKey == "true". Never logged.
	HeaderUserAPIKey = "X-Matrix-User-API-Key"

	// HeaderCostPax is a response header carrying the PAX cost of
	// this single LLM call (string-formatted decimal, base unit PAX).
	HeaderCostPax = "X-Matrix-Cost-Pax"

	// HeaderDailySpentPax is a response header carrying the actor's
	// running daily spend after this call (string-formatted decimal).
	HeaderDailySpentPax = "X-Matrix-Daily-Spent-Pax"

	// HeaderDailyRemainingPax is a response header carrying the
	// actor's remaining daily budget (string-formatted decimal,
	// negative numbers clamped to "0").
	HeaderDailyRemainingPax = "X-Matrix-Daily-Remaining-Pax"

	// HeaderRateTableVersion is a response header echoing the rate
	// table version that priced this call (integer string).
	HeaderRateTableVersion = "X-Matrix-Rate-Table-Version"
)

// Slot values. Mirror MCL/llm.ModelSlot string forms; duplicated as
// constants here so the gateway has no Go-import dependency on MCL.
const (
	SlotCompiler = "compiler"
	SlotPlanner  = "planner"
	SlotExecutor = "executor"
	// SlotLiaison is the pipeline NARRATOR — a passive, read-only
	// observability side-channel that humanizes plan/walk events for the
	// user. It does not drive work. Reuses already-priced models.
	SlotLiaison = "liaison"
	// SlotNeo is the Neo default conversational AGENT (matrix/neo): the
	// first-class function-calling agent that fronts the per-user runtime,
	// runs its own tools, and delegates rigorous/money tasks to MCL. Neo is
	// NOT the Liaison (which only narrates) — it gets its OWN slot so its
	// LLM spend is metered under its own identity. See rates.FreeTierWhitelist.
	SlotNeo = "neo"
)

// BudgetExhaustedResponse is the JSON body returned with 429 when
// the caller's daily PAX cap is reached.
type BudgetExhaustedResponse struct {
	Error    string `json:"error"`
	SpentPax string `json:"spent_pax"`
	LimitPax string `json:"limit_pax"`
}

// ChatCompletionRequest is the minimal subset of the OpenAI-compat
// chat completion body the gateway needs to introspect (model + stream
// flag). Other fields are forwarded verbatim via the request body
// passthrough; we only Unmarshal a copy for routing decisions.
type ChatCompletionRequest struct {
	Model  string `json:"model,omitempty"`
	Stream bool   `json:"stream,omitempty"`
}

// UpstreamUsage mirrors the OpenAI-compat usage trailer present in
// non-streaming responses and the FINAL chunk of streaming responses
// (Fireworks + Together both emit it). Decoded for cost computation.
type UpstreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// UpstreamResponseEnvelope is the minimum response shape the gateway
// needs to extract `usage` from. Choices/error/etc. are forwarded
// verbatim to the client.
type UpstreamResponseEnvelope struct {
	Usage *UpstreamUsage `json:"usage,omitempty"`
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
