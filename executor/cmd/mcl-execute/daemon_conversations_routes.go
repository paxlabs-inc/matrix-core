// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_conversations_routes.go — HTTP surface for the durable Liaison
// conversation memory, so the client can browse and reopen past chat
// threads instead of relying on ephemeral client-side storage.
//
// Routes (authAny — router-header trust OR legacy bearer):
//
//	GET /conversations        list every thread, newest-first (summaries)
//	GET /conversations/<id>   full (bounded) turn log for one thread
//
// Like the conversationStore it serves, this is a pure side-channel: it
// only reads the durable turn log persisted on the volume keyed by
// conversation_id. It NEVER touches cortex, signs envelopes, or perturbs
// the plan/walk, so it cannot affect the D11 replay byte-identity
// invariant. The daemon serves exactly one user, so every persisted
// conversation belongs to the authenticated caller.

import (
	"net/http"
	"strings"
)

// handleConversationsList serves GET /conversations.
func (d *daemonState) handleConversationsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if !d.convStore.enabled() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": []conversationSummary{}})
		return
	}
	items := d.convStore.List()
	if items == nil {
		items = []conversationSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

// handleConversationGet serves GET /conversations/<id>.
func (d *daemonState) handleConversationGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/conversations/")
	id = strings.Trim(id, "/")
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	rec := d.convStore.Get(id)
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleConversationsRouter dispatches /conversations and /conversations/<id>.
func (d *daemonState) handleConversationsRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/conversations")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		d.handleConversationsList(w, r)
		return
	}
	d.handleConversationGet(w, r)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
