// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package transport streams Construct surfaces over the EXISTING daemon SSE
// pipe (executor/cmd/mcl-execute/{transcript.go,daemon_sse.go}). It adds two
// event types alongside chat.*/step.*:
//
//	construct.surface        a full surface (one of the 8 frozen primitives)
//	construct.surface.patch  a progressive update to an existing surface id
//
// SIDE-CHANNEL INVARIANT (load-bearing): these are TRANSCRIPT SSE events only —
// exactly like plan.tool.result and the Liaison's chat.* turns. Emitting them
// NEVER signs an envelope, writes cortex, or touches plan/walk, so it cannot
// perturb the D11 replay byte-identity invariant. The emit helpers therefore
// take only a minimal EventSink (which the daemon transcript already
// satisfies) and never reach into the pipeline.
package transport

import (
	"encoding/json"
	"fmt"

	"centra/packages/construct/schema"
)

// Event types + phase, carried in the daemon sseEvent{Type,Phase,Fields}.
const (
	// Phase tags every Construct event so the existing per-phase SSE filter
	// (and the Liaison's loop-guard) can include/exclude the channel. Mirrors
	// the Liaison's "liaison" phase.
	Phase = "construct"

	// EventSurface carries a full surface.
	EventSurface = "construct.surface"
	// EventSurfacePatch carries a progressive update to an existing surface id.
	EventSurfacePatch = "construct.surface.patch"
)

// Field keys in the event Fields payload.
const (
	// FieldSurface holds the full schema.Surface (construct.surface) — or, on a
	// patch, the increment surface (construct.surface.patch).
	FieldSurface = "surface"
	// FieldID is the target surface id for a patch.
	FieldID = "id"
	// FieldIntentID/FieldConversationID thread the existing per-subscriber SSE
	// filtering + /events/replay/:id (unchanged).
	FieldIntentID       = "intent_id"
	FieldConversationID = "conversation_id"
)

// EventSink is the minimal contract the emit helpers need. The daemon's
// *transcript already satisfies it (Event(eventType, phase, fields)), so a
// caller passes the transcript directly. Keeping it an interface decouples
// this package from the executor module entirely.
type EventSink interface {
	Event(eventType, phase string, fields map[string]interface{})
}

// EmitSurface validates and emits a full surface as a construct.surface event.
// intentID/conversationID thread the existing SSE filtering; intentID is also
// what the daemon transcript auto-stamps, but we set it explicitly so the
// helper is correct against any sink. Returns an error only if the surface is
// invalid (a caller never emits an unvalidated surface — invariant i2).
func EmitSurface(sink EventSink, intentID, conversationID string, s *schema.Surface) error {
	if s == nil {
		return fmt.Errorf("construct/transport: nil surface")
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("construct/transport: %w", err)
	}
	sink.Event(EventSurface, Phase, surfaceFields(intentID, conversationID, s, ""))
	return nil
}

// PatchSurface emits a progressive update to an existing surface id. delta is a
// surface of the SAME kind carrying the increment (appended Stream chunks,
// upserted Timeline steps, or a replacement payload for the other kinds — see
// ApplyPatch). The id is stamped onto the delta and surfaced as FieldID so the
// client can target its store.
func PatchSurface(sink EventSink, intentID, conversationID, id string, delta *schema.Surface) error {
	if delta == nil {
		return fmt.Errorf("construct/transport: nil patch")
	}
	if id == "" {
		return fmt.Errorf("construct/transport: patch requires a target id")
	}
	delta.ID = id
	if err := delta.Validate(); err != nil {
		return fmt.Errorf("construct/transport: %w", err)
	}
	sink.Event(EventSurfacePatch, Phase, surfaceFields(intentID, conversationID, delta, id))
	return nil
}

// surfaceFields builds the event Fields payload. The surface rides as a nested
// object so the JSONL/SSE encoder serialises it verbatim; on replay the same
// bytes reproduce the same surface (idempotent).
func surfaceFields(intentID, conversationID string, s *schema.Surface, patchID string) map[string]interface{} {
	f := map[string]interface{}{FieldSurface: s}
	if patchID != "" {
		f[FieldID] = patchID
	}
	if intentID != "" {
		f[FieldIntentID] = intentID
	}
	if conversationID != "" {
		f[FieldConversationID] = conversationID
	}
	return f
}

// SurfaceFromEvent extracts a schema.Surface from an event's Fields payload. It
// tolerates both an in-process *schema.Surface (same map read back) and a
// JSON-decoded map[string]interface{} (the SSE/JSONL wire), so it works
// identically on the live tap and on replay. It does NOT validate; callers at
// trust boundaries call Validate.
func SurfaceFromEvent(fields map[string]interface{}) (*schema.Surface, error) {
	raw, ok := fields[FieldSurface]
	if !ok {
		return nil, fmt.Errorf("construct/transport: event has no %q field", FieldSurface)
	}
	if s, ok := raw.(*schema.Surface); ok {
		return s, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("construct/transport: re-marshal surface: %w", err)
	}
	return schema.Unmarshal(b)
}
