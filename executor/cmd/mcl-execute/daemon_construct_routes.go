// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_construct_routes.go — the Construct OS Shell rehydration read path.
//
// Route (authAny — router-header trust OR legacy bearer):
//
//	GET /construct/state?conversation_id=<id>&since_seq=<n>
//	    The thin, read-only rehydration endpoint a cold-opening shell hits to
//	    rebuild "the computer as the user left it". It returns the
//	    conversation's durable Construct surface frames (oldest-first, already
//	    capped to the retained-frame bound by surfacestore.Load) plus the newest
//	    seq, so the client can replay them through the same reducer the live
//	    feed uses and then subscribe to the live stream from last_seq.
//
// READ-ONLY / SIDE-CHANNEL (load-bearing, D11): this handler ONLY reads the
// durable surface timeline. It NEVER writes a frame, signs an envelope, writes
// cortex, or touches the plan/walk — so it cannot perturb the D11 replay
// byte-identity invariant. It is the pure VIEW read sibling of the broker tee
// that populates the store.
//
// PER-USER ISOLATION (R15.3): the daemon serves exactly one user, so every
// persisted conversation belongs to the authenticated caller; the handler
// additionally scopes strictly by conversation_id and rejects a blank id or one
// containing a path separator (also a path-traversal guard, mirroring
// surfacestore.Load's own validation) so it can never read across conversations.

import (
	"net/http"
	"strings"

	"matrix/construct/schema"
	"matrix/construct/surfacestore"
)

// handleConstructState serves GET /construct/state, the read-only rehydration
// endpoint. It enforces auth + method, then delegates to the testable
// serveConstructState core with the daemon's durable surface store (wired in
// task 3.1; nil/disabled until then, in which case rehydration returns an empty
// workspace rather than erroring).
func (d *daemonState) handleConstructState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	serveConstructState(w, r, d.surfaceStore)
}

// serveConstructState is the transport-agnostic core of the rehydration read
// path, taking the surface store explicitly so it is unit-testable without a
// full daemonState (task 2.3). It is strictly read-only.
//
// Behavior:
//   - conversation_id is required and must contain no path separator; a blank
//     or unsafe id is a 400 (and would never resolve in the store anyway).
//   - since_seq (optional, default 0; negatives clamped to 0) is a catch-up
//     cursor: only frames with seq strictly greater than since_seq are
//     returned, so a reconnecting client backfills only what it is missing.
//   - last_seq is the newest frame seq across the FULL loaded set (before the
//     since_seq filter), so the client always advances its live "since" cursor
//     even when the catch-up window is empty.
//   - A nil/disabled store yields an empty (but well-formed) response.
func serveConstructState(w http.ResponseWriter, r *http.Request, store *surfacestore.Store) {
	conversationID := strings.TrimSpace(queryString(r, "conversation_id", ""))
	if conversationID == "" || strings.ContainsAny(conversationID, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	sinceSeq, _ := queryInt(r, "since_seq", 0)
	if sinceSeq < 0 {
		sinceSeq = 0
	}

	// Load is itself conversation-scoped and bounded to the retained-frame cap,
	// and returns nil for a disabled store — so a nil store guard plus this
	// call is the whole of the read.
	var loaded []schema.Frame
	if store != nil {
		loaded = store.Load(conversationID)
	}

	// last_seq is the newest seq across the full loaded set (frames are
	// oldest-first, so it is the max; computed defensively rather than assuming
	// strict monotonicity). The catch-up filter keeps only frames newer than
	// the client's cursor.
	lastSeq := 0
	frames := make([]schema.Frame, 0, len(loaded))
	for _, f := range loaded {
		if f.Seq > lastSeq {
			lastSeq = f.Seq
		}
		if f.Seq > sinceSeq {
			frames = append(frames, f)
		}
	}

	writeJSON(w, http.StatusOK, schema.StateResponse{
		ConversationID: conversationID,
		Frames:         frames,
		LastSeq:        lastSeq,
	})
}
