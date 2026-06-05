// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_chat.go — POST /chat: the Liaison front door.
//
// This is the human entry point for the conversational surface. Instead
// of the client deciding whether a message is a task (and POSTing
// /messages/async) vs chit-chat, the Liaison triages every message:
//
//   - reply    → answered directly (greetings, status, capability Qs);
//                the text is returned synchronously.
//   - dispatch → starts the normal async pipeline (same path as
//                /messages/async) and the per-run narrator (spawned in
//                runMessage) streams chat.assistant turns over
//                /events?intent_id=<id> until the closing answer.
//
// Clarify re-entry: when the client passes the original intent_id plus
// slot_values (answers to a prior clarify turn), triage is skipped and
// the run is re-dispatched directly with the answers.
//
// The endpoint is metered like any other LLM surface (the triage call
// routes through SlotLiaison on the gateway). It returns 503 when the
// Liaison is disabled (-liaison-disable).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// chatRequest is the body of POST /chat.
type chatRequest struct {
	// Message is the user's natural-language turn. Required.
	Message string `json:"message"`
	// ConversationID threads turns together. Optional; the daemon mints
	// one when absent and echoes it so the client can reuse it.
	ConversationID string `json:"conversation_id,omitempty"`
	// IntentID + SlotValues drive clarify re-entry: answers to a prior
	// clarify turn for an in-flight intent. When both are set, triage is
	// skipped and the run is re-dispatched with the answers.
	IntentID   string            `json:"intent_id,omitempty"`
	SlotValues map[string]string `json:"slot_values,omitempty"`
	// GoalID rolls the dispatched run up under a cortex Goal (optional).
	GoalID string `json:"goal_id,omitempty"`
	// Skill overrides the daemon's default skill for the dispatched run.
	Skill string `json:"skill,omitempty"`
	// UserName is the signed-in user's friendly display label (OAuth
	// profile name or email), so the Liaison can address them by name.
	// Sanitized daemon-side before use; optional.
	UserName string `json:"user_name,omitempty"`
}

// handleChat serves POST /chat. The transcript t is the daemon-level
// boot transcript; it is used only for the triage call's cost-capture
// hook (the per-run narration uses its own per-intent transcript).
func (d *daemonState) handleChat(t *transcript) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		ctx, ok := d.requireAuthPolicy(w, r, authAny)
		if !ok {
			return
		}
		if !d.liaisonEnabled() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "liaison disabled; restart daemon without -liaison-disable",
			})
			return
		}
		if d.asyncReg == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "async registry not enabled",
			})
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
			return
		}

		conversationID := req.ConversationID
		if conversationID == "" {
			conversationID = synthConversationID(req.Message)
		}
		skill := req.Skill
		if skill == "" {
			skill = d.defaultSkillURI
		}
		userID := userIDFromContext(ctx)

		// dispatch kicks off the normal async pipeline. The narrator
		// (spawned in runMessage when liaison is enabled) narrates it as
		// chat.assistant turns stamped with conversationID.
		dispatch := func(prose string) {
			if skill == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "skill URI required (no daemon default configured)",
				})
				return
			}
			intentID := req.IntentID
			if intentID == "" {
				intentID = synthIntentID(prose, "chat")
			}
			mreq := messageRequest{
				Prose:          prose,
				SkillURI:       skill,
				IntentID:       intentID,
				SlotValues:     req.SlotValues,
				GoalIDField:    req.GoalID,
				ConversationID: conversationID,
				UserName:       req.UserName,
			}
			if _, err := d.asyncReg.CreateQueued(intentID, userID, mreq); err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			go d.runAsyncMessage(intentID, mreq, t)
			writeJSON(w, http.StatusAccepted, map[string]interface{}{
				"conversation_id": conversationID,
				"intent_id":       intentID,
				"kind":            "dispatch",
				"events_url":      "/events?intent_id=" + intentID,
				"poll_url":        "/messages/async/" + intentID,
			})
		}

		// Clarify re-entry: a prior run asked for input; the client sends
		// the original intent + the answers. Skip triage, re-dispatch.
		if req.IntentID != "" && len(req.SlotValues) > 0 {
			d.convStore.AppendUser(conversationID, req.Message)
			dispatch(req.Message)
			return
		}

		// Front door: let the Liaison decide reply vs dispatch — with the
		// recent conversation as context so follow-ups ("maybe try
		// paxscan", "do the same for X") resolve against prior turns
		// instead of being read as a brand-new, context-free request.
		history := d.convStore.Recent(conversationID, convRecallTurns)
		triageIntent := synthIntentID(req.Message, "triage")
		dec := d.triageMessage(ctx, triageIntent, req.Message, req.UserName, history, t)
		// Record the user's turn AFTER triage (so triage saw only prior
		// turns) but BEFORE responding, so the thread memory is durable
		// regardless of what happens next.
		d.convStore.AppendUser(conversationID, req.Message)
		if dec.Action == "reply" {
			// A direct reply is itself a conversation turn — persist it
			// so the next message recalls it.
			d.convStore.AppendAssistant(conversationID, "", dec.Reply)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"conversation_id": conversationID,
				"kind":            "reply",
				"text":            dec.Reply,
			})
			return
		}
		dispatch(dec.Prose)
	}
}

// synthConversationID mints a unique conversation id. Includes the wall
// clock so two identical opening messages start distinct threads.
func synthConversationID(seed string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("conv|%d|%s", time.Now().UnixNano(), seed)))
	return "conv_" + hex.EncodeToString(h[:10])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
