// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Tests for the automatrix.complete seam at the engine level (req 6.1 / 6.5):
// emitAutomatrixComplete writes the durable in-app record AND publishes the
// broker event, carrying the RESULT not the protocol. Drives the REAL Engine +
// broker + durable store (no fakes).

import (
	"testing"
)

// TestEmitAutomatrixComplete_RecordsAndPublishes proves a genuine completion is
// (1) written durably to the inbox as unread and (2) announced on the broker as
// an automatrix.complete event with the result-not-protocol fields.
func TestEmitAutomatrixComplete_RecordsAndPublishes(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		AutomatrixDir:   t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	if !e.automatrix.Enabled() {
		t.Fatal("automatrix inbox must be enabled when AutomatrixDir is set")
	}

	const convID = "conv_surprise"
	rec := e.emitAutomatrixComplete(convID,
		"Draft the quarterly update doc you mentioned",
		"Drafted a 1-page quarterly update in the thread")

	// (1) Durable record: written, unread, with the result intact.
	inbox := e.AutomatrixInbox().List()
	if len(inbox) != 1 {
		t.Fatalf("want 1 durable completion record, got %d", len(inbox))
	}
	if inbox[0].ID != rec.ID {
		t.Errorf("returned record id %q must match the stored one %q", rec.ID, inbox[0].ID)
	}
	if inbox[0].Read {
		t.Error("a fresh completion record must be unread")
	}
	if e.AutomatrixInbox().Unread() != 1 {
		t.Errorf("want 1 unread, got %d", e.AutomatrixInbox().Unread())
	}
	if inbox[0].ResultSummary != "Drafted a 1-page quarterly update in the thread" {
		t.Errorf("result summary lost: %+v", inbox[0])
	}

	// (2) Broker event: published on the conversation topic with the
	// result-not-protocol fields.
	replay, _, cancel := e.broker.subscribe(convID, 0)
	defer cancel()
	var found *Event
	for i := range replay {
		if replay[i].Type == automatrixCompleteEvent {
			found = &replay[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no automatrix.complete event published on topic %q (got %d events)", convID, len(replay))
	}
	if found.Fields["conversation_id"] != convID {
		t.Errorf("event conversation_id = %v, want %q", found.Fields["conversation_id"], convID)
	}
	if found.Fields["result_summary"] != "Drafted a 1-page quarterly update in the thread" {
		t.Errorf("event result_summary lost: %+v", found.Fields)
	}
	if found.Fields["opportunity_summary"] != "Draft the quarterly update doc you mentioned" {
		t.Errorf("event opportunity_summary lost: %+v", found.Fields)
	}
	if found.Fields["id"] != rec.ID {
		t.Errorf("event id = %v, want %q", found.Fields["id"], rec.ID)
	}
}
