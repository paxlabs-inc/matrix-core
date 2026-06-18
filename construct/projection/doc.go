// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package projection is the Construct projection engine: the step that flips
// "generate UI" into "project onto the vocabulary" (construct.frozen.kvx
// [deferred].layer_projection). It maps arbitrary agent world-state onto a
// composition of the 8 frozen primitives and hands the result to the transport
// layer to stream as construct.surface[.patch] events.
//
// It has two tiers, matching the hybrid Phase 4 design locked with Andrew
// (2026-06-18):
//
//   - PASSIVE (deterministic, no model): ProjectEvent maps known pipeline
//     events (a tool result, produced step text) onto surfaces with no model
//     call. This is the Go port of the client wrap-first adapter
//     (client/lib/construct/adapter.ts), but driven by the raw event stream
//     rather than an assembled task. It is the safety floor that fixes the
//     historical "200-char tool dump / empty narration on a tool-only plan"
//     for the MCL pipeline, for free.
//
//   - ACTIVE (agent-authored): RenderTools advertises the construct_render
//     tool the projecting agent (Neo) calls to deliberately render a surface,
//     and ParseRender maps a render tool-call back into a validated
//     schema.Surface. The agent fills a TRUSTED primitive and the validation
//     gate rejects anything malformed, so the agent can never emit arbitrary
//     UI (invariant i2) — expressiveness from the agent, safety from the fixed
//     contract.
//
// The package is pure and module-decoupled: it imports only the Construct
// schema, never the executor or Neo, so both can depend on it and it stays
// trivially testable.
package projection
