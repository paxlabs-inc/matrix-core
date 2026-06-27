// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package schema

// Frame is one persisted Construct surface event — the durable unit the
// surfacestore tees and the rehydration read path returns. Its shape mirrors
// the daemon SSE Event ({seq,ts,phase,type,fields}) — and neo/internal/trace's
// Event — so a reopen replays the persisted stream through the client's
// existing reducer (transport.ApplyPatch mirror) byte-for-byte.
//
// It lives here in construct/schema (not in surfacestore) because it is a wire
// contract that reaches the client: the codegen mirrors it into the client's
// types.gen.ts, and surfacestore aliases this canonical type so the persisted
// shape can never drift from the generated client type.
//
// Phase is always the Construct transport phase ("construct"); Type is one of
// the Construct surface event types ("construct.surface" |
// "construct.surface.patch").
type Frame struct {
	// Seq orders frames within a conversation; mirrors the transcript event seq.
	Seq int `json:"seq"`
	// Ts is the RFC3339 emit timestamp of the frame.
	Ts string `json:"ts"`
	// Phase is the transport phase, always "construct".
	Phase string `json:"phase"`
	// Type is the event type ("construct.surface" | "construct.surface.patch").
	Type string `json:"type"`
	// Fields is the opaque event payload, mirroring the SSE Event fields.
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// StateResponse is the body of the rehydration read path
// (GET /construct/state?conversation_id=&since_seq=). It carries a
// conversation's durable surface frames, oldest-first, so a cold-opening shell
// replays them through the same reducer the live feed uses, plus the newest seq
// for the SSE catch-up cursor.
type StateResponse struct {
	// ConversationID scopes the response to a single conversation (per-user
	// isolation; never cross-conversation).
	ConversationID string `json:"conversation_id"`
	// Frames are the conversation's persisted surface frames, oldest-first,
	// capped to the retained-frame bound.
	Frames []Frame `json:"frames"`
	// LastSeq is the newest frame seq, used as the SSE "since" cursor for
	// live catch-up after rehydration.
	LastSeq int `json:"last_seq"`
}
