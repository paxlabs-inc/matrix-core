// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// Unit tests for the Construct OS Shell rehydration read path
// (serveConstructState, the transport-agnostic core of GET /construct/state).
//
// Every test drives the REAL package-level core against a REAL
// surfacestore.Store backed by t.TempDir() — no stubs/mocks/fakes. Frames are
// recorded through the store's async path (then Flush'd) exactly as the broker
// tee will record them, then the assertions are made against the actual HTTP
// response the handler writes. This proves the catch-up cursor, the
// retained-frame cap, conversation scoping (the per-user isolation guard), the
// read-only invariant, and the disabled-store empty response on the wire, not
// just at the store boundary.
//
// Covers: R8.1 (since_seq catch-up), R15.3 (conversation scoping / never leak
// another conversation's frames), R16.3 (response bounded to the retained-frame
// cap).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"centra/packages/construct/schema"
	"centra/packages/construct/surfacestore"
	"centra/packages/construct/transport"
)

// recordSurface enqueues one real construct.surface frame for a conversation
// with a distinctive marker field so a test can prove which conversation a
// returned frame belongs to.
func recordSurface(st *surfacestore.Store, conversationID string, seq int, marker string) {
	st.Record(conversationID, schema.Frame{
		Seq:    seq,
		Ts:     "2026-01-01T00:00:0" + strconv.Itoa(seq%10) + "Z",
		Phase:  transport.Phase,
		Type:   transport.EventSurface,
		Fields: map[string]interface{}{"id": marker, "kind": "Stream", "marker": marker},
	})
}

// serveState drives the read-only core exactly as the auth/method wrapper does:
// builds a GET against /construct/state with the given raw query, runs
// serveConstructState with the supplied store, and decodes the response body.
func serveState(t *testing.T, store *surfacestore.Store, rawQuery string) (*httptest.ResponseRecorder, schema.StateResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/construct/state?"+rawQuery, http.NoBody)
	rec := httptest.NewRecorder()
	serveConstructState(rec, req, store)

	var resp schema.StateResponse
	// Only decode a JSON body when the handler returned OK; error paths carry a
	// {"error":...} object that is not a StateResponse.
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode StateResponse: %v (body=%q)", err, rec.Body.String())
		}
	}
	return rec, resp
}

// TestServeConstructState_SinceSeqCursor confirms since_seq is a strict
// catch-up cursor: only frames with seq > since_seq are returned, while
// last_seq always reports the newest seq across the full loaded set so a
// reconnecting client advances its live cursor even when the catch-up window is
// empty (R8.1).
func TestServeConstructState_SinceSeqCursor(t *testing.T) {
	st := surfacestore.Open(t.TempDir())
	defer st.Close()

	const convID = "conv_cursor"
	const total = 5
	for i := 1; i <= total; i++ {
		recordSurface(st, convID, i, "s"+strconv.Itoa(i))
	}
	st.Flush()

	tests := []struct {
		name     string
		query    string
		wantSeqs []int
	}{
		{name: "absent cursor returns all", query: "conversation_id=" + convID, wantSeqs: []int{1, 2, 3, 4, 5}},
		{name: "zero cursor returns all", query: "conversation_id=" + convID + "&since_seq=0", wantSeqs: []int{1, 2, 3, 4, 5}},
		{name: "mid cursor returns only newer", query: "conversation_id=" + convID + "&since_seq=3", wantSeqs: []int{4, 5}},
		{name: "newest cursor returns empty window", query: "conversation_id=" + convID + "&since_seq=5", wantSeqs: []int{}},
		{name: "beyond-newest cursor returns empty window", query: "conversation_id=" + convID + "&since_seq=99", wantSeqs: []int{}},
		{name: "negative cursor clamps to zero", query: "conversation_id=" + convID + "&since_seq=-4", wantSeqs: []int{1, 2, 3, 4, 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, resp := serveState(t, st, tc.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: want 200 got %d (body=%q)", rec.Code, rec.Body.String())
			}
			gotSeqs := make([]int, 0, len(resp.Frames))
			for _, f := range resp.Frames {
				gotSeqs = append(gotSeqs, f.Seq)
			}
			if !equalInts(gotSeqs, tc.wantSeqs) {
				t.Errorf("frames: want seqs %v got %v", tc.wantSeqs, gotSeqs)
			}
			// last_seq is the newest across the FULL set, independent of the
			// catch-up filter — so it stays at `total` even when the window is
			// empty.
			if resp.LastSeq != total {
				t.Errorf("last_seq: want %d (newest of full set) got %d", total, resp.LastSeq)
			}
			if resp.ConversationID != convID {
				t.Errorf("conversation_id: want %q got %q", convID, resp.ConversationID)
			}
			// Every returned frame must be strictly newer than the cursor.
			cursor := sinceFromQuery(tc.query)
			for _, f := range resp.Frames {
				if f.Seq <= cursor {
					t.Errorf("frame seq %d not strictly greater than since_seq %d", f.Seq, cursor)
				}
			}
		})
	}
}

// TestServeConstructState_CapBoundsResponse confirms the response is bounded to
// the retained-frame cap (R16.3): with a small retain, recording more than
// retain frames yields a response holding only the newest `retain` frames (the
// oldest are dropped), while last_seq still reports the true newest seq.
func TestServeConstructState_CapBoundsResponse(t *testing.T) {
	const retain = 4
	// Open reads the cap once via CONSTRUCT_SURFACE_RETAIN; t.Setenv scopes it
	// to this test, giving a real small-retain store through the public Open.
	t.Setenv("CONSTRUCT_SURFACE_RETAIN", strconv.Itoa(retain))
	st := surfacestore.Open(t.TempDir())
	defer st.Close()

	const convID = "conv_capped"
	const total = 10
	for i := 1; i <= total; i++ {
		recordSurface(st, convID, i, "s"+strconv.Itoa(i))
	}
	st.Flush()

	rec, resp := serveState(t, st, "conversation_id="+convID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", rec.Code)
	}
	if len(resp.Frames) != retain {
		t.Fatalf("cap not enforced: want %d frames got %d", retain, len(resp.Frames))
	}
	// Newest `retain` kept, oldest dropped: seqs 7,8,9,10 for total=10,retain=4.
	wantFirst := total - retain + 1
	if resp.Frames[0].Seq != wantFirst {
		t.Errorf("oldest retained frame: want seq %d got %d (newest must be kept)", wantFirst, resp.Frames[0].Seq)
	}
	if last := resp.Frames[len(resp.Frames)-1].Seq; last != total {
		t.Errorf("newest retained frame: want seq %d got %d", total, last)
	}
	// The dropped oldest frames must not appear in the response.
	for _, f := range resp.Frames {
		if f.Seq < wantFirst {
			t.Errorf("frame seq %d should have been dropped by the cap", f.Seq)
		}
	}
	if resp.LastSeq != total {
		t.Errorf("last_seq: want %d got %d", total, resp.LastSeq)
	}
}

// TestServeConstructState_ConversationScoping confirms strict per-conversation
// isolation (R15.3): a GET for conversation A never returns conversation B's
// frames, and a blank or path-separator conversation_id is rejected (400) with
// no leaked frames.
func TestServeConstructState_ConversationScoping(t *testing.T) {
	st := surfacestore.Open(t.TempDir())
	defer st.Close()

	const convA, convB = "conv_alpha", "conv_bravo"
	for i := 1; i <= 3; i++ {
		recordSurface(st, convA, i, "ALPHA-"+strconv.Itoa(i))
		recordSurface(st, convB, i, "BRAVO-"+strconv.Itoa(i))
	}
	st.Flush()

	t.Run("A never returns B frames", func(t *testing.T) {
		rec, resp := serveState(t, st, "conversation_id="+convA)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: want 200 got %d", rec.Code)
		}
		if len(resp.Frames) != 3 {
			t.Fatalf("want 3 frames for A, got %d", len(resp.Frames))
		}
		for _, f := range resp.Frames {
			if m, _ := f.Fields["marker"].(string); !strings.HasPrefix(m, "ALPHA-") {
				t.Errorf("conversation A leaked a non-A frame: marker=%q", m)
			}
		}
		// The other conversation's marker must appear nowhere in the raw body.
		if strings.Contains(rec.Body.String(), "BRAVO") {
			t.Errorf("response for A leaked conversation B content:\n%s", rec.Body.String())
		}
	})

	rejected := []struct {
		name  string
		query string
	}{
		{name: "blank conversation_id", query: "conversation_id="},
		{name: "absent conversation_id", query: ""},
		{name: "whitespace conversation_id", query: "conversation_id=%20%20"},
		{name: "posix path separator", query: "conversation_id=" + convA + "%2F.." + "%2F" + convB},
		{name: "windows path separator", query: "conversation_id=a%5Cb"},
		{name: "traversal id", query: "conversation_id=..%2Fescape"},
	}
	for _, tc := range rejected {
		t.Run("reject "+tc.name, func(t *testing.T) {
			rec, _ := serveState(t, st, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400 got %d (body=%q)", rec.Code, rec.Body.String())
			}
			// A rejected request must never leak any conversation's frames.
			body := rec.Body.String()
			if strings.Contains(body, "ALPHA") || strings.Contains(body, "BRAVO") {
				t.Errorf("rejected request leaked frame content:\n%s", body)
			}
		})
	}
}

// TestServeConstructState_ReadOnly confirms the read path never creates or
// modifies a stored frame: the store's Load count is identical before and after
// serving (the side-channel / D11 read-only invariant on the wire).
func TestServeConstructState_ReadOnly(t *testing.T) {
	st := surfacestore.Open(t.TempDir())
	defer st.Close()

	const convID = "conv_readonly"
	for i := 1; i <= 3; i++ {
		recordSurface(st, convID, i, "s"+strconv.Itoa(i))
	}
	st.Flush()

	before := len(st.Load(convID))
	if before != 3 {
		t.Fatalf("setup: want 3 stored frames got %d", before)
	}

	// Serve several times, including a catch-up request — none may mutate state.
	serveState(t, st, "conversation_id="+convID)
	serveState(t, st, "conversation_id="+convID+"&since_seq=2")
	serveState(t, st, "conversation_id="+convID+"&since_seq=99")
	st.Flush()

	after := len(st.Load(convID))
	if after != before {
		t.Errorf("read path mutated the store: Load count %d -> %d", before, after)
	}
}

// TestServeConstructState_DisabledStore confirms a nil or disabled store yields
// a well-formed empty response (HTTP 200, empty frames, last_seq 0) rather than
// an error — a cold open with persistence off shows an empty computer, never a
// failure. Frames must serialize as [] (not null) so the client reducer can
// fold it directly.
func TestServeConstructState_DisabledStore(t *testing.T) {
	cases := []struct {
		name  string
		store *surfacestore.Store
	}{
		{name: "nil store", store: nil},
		{name: "disabled (empty-dir) store", store: surfacestore.Open("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const convID = "conv_disabled"
			rec, resp := serveState(t, tc.store, "conversation_id="+convID)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: want 200 got %d", rec.Code)
			}
			if len(resp.Frames) != 0 {
				t.Errorf("want empty frames, got %d", len(resp.Frames))
			}
			if resp.LastSeq != 0 {
				t.Errorf("want last_seq 0, got %d", resp.LastSeq)
			}
			if resp.ConversationID != convID {
				t.Errorf("conversation_id: want %q got %q", convID, resp.ConversationID)
			}
			// Well-formed empty: frames is an empty JSON array, never null.
			if !strings.Contains(strings.ReplaceAll(rec.Body.String(), " ", ""), "\"frames\":[]") {
				t.Errorf("frames must serialize as [] not null:\n%s", rec.Body.String())
			}
		})
	}
}

// --- helpers --------------------------------------------------------

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sinceFromQuery extracts the effective since_seq cursor a request carries
// (clamped to 0 for absent/negative), mirroring serveConstructState, so a test
// can assert every returned frame is strictly newer than the cursor.
func sinceFromQuery(rawQuery string) int {
	for _, kv := range strings.Split(rawQuery, "&") {
		if strings.HasPrefix(kv, "since_seq=") {
			n, err := strconv.Atoi(strings.TrimPrefix(kv, "since_seq="))
			if err != nil || n < 0 {
				return 0
			}
			return n
		}
	}
	return 0
}
