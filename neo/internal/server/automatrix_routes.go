// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// The Automatrix control surface (task 6.1, req 7.1/7.3/7.5/7.4). These are
// Neo-owned authenticated routes (served behind the same authenticated front as
// /chat — they are registered on Neo's mux ahead of the daemon proxy, so they
// ride the per-user daemon's authenticated surface):
//
//   GET  /automatrix/settings        read the per-user opt-in
//   PUT  /automatrix/settings        toggle the opt-in {enabled:bool}
//                                     ON  -> create the single AUTOMATRIX alarm
//                                     OFF -> cancel it (req 4.1 / 7.5)
//   GET  /automatrix/queue           the management queue of PENDING opportunities
//                                     (eligible + financial; financial ones are
//                                     surfaced for explicit approval, req 7.3)
//   POST /automatrix/queue/dismiss   {uri} -> status=dismissed
//   POST /automatrix/queue/approve   {uri} -> dispatch the item as a NORMAL,
//                                     gated, user-initiated task (the money path
//                                     stays the existing core_execute-gated run,
//                                     NOT the restricted autonomous surface)
//   GET  /automatrix/inbox           the completion inbox + unread badge (req 7.4)
//   POST /automatrix/inbox/read      {id} -> mark one completion read
//
// The opt-in toggle and read go through the production AutomatrixGovernor (the
// concrete type wired in serve.go); the queue reads/writes go through the Neocortex
// pager; the inbox through the durable automatrixlog store. No secrets are read
// or returned — only the result-not-protocol surfaces.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"matrix/neo/internal/automatrixsettings"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/task"
)

// automatrixControl is the slice of the production governor the control surface
// needs: toggle the opt-in (creating/cancelling the alarm) and read it back.
// The engine stores the governor behind the AutomatrixGovernor seam; the
// endpoints type-assert to this richer interface, so a test/seam governor that
// does not implement it simply yields a 503 (the toggle is unavailable) rather
// than a panic.
type automatrixControl interface {
	SetEnabled(ctx context.Context, enabled bool) error
	SetTimezone(timezone string) error
	SettingsView() automatrixsettings.State
}

// handleAutomatrix is the prefix dispatcher for /automatrix/*. Registered ahead
// of the daemon proxy so these Neo-owned routes are served locally.
func (s *Server) handleAutomatrix(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/automatrix/settings":
		s.handleAutomatrixSettings(w, r)
	case r.URL.Path == "/automatrix/queue":
		s.handleAutomatrixQueue(w, r)
	case r.URL.Path == "/automatrix/queue/dismiss":
		s.handleAutomatrixDismiss(w, r)
	case r.URL.Path == "/automatrix/queue/approve":
		s.handleAutomatrixApprove(w, r)
	case r.URL.Path == "/automatrix/inbox":
		s.handleAutomatrixInbox(w, r)
	case r.URL.Path == "/automatrix/inbox/read":
		s.handleAutomatrixInboxRead(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown automatrix route"})
	}
}

// automatrixControlOrError resolves the production governor or writes a 503.
func (s *Server) automatrixControlOrError(w http.ResponseWriter) (automatrixControl, bool) {
	ctl, ok := s.engine.automatrixGov.(automatrixControl)
	if !ok || ctl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "automatrix control not available"})
		return nil, false
	}
	return ctl, true
}

// settingsResponse is the GET/PUT /automatrix/settings wire shape — the opt-in
// and whether an alarm is currently live. No alarm id / protocol detail leaks.
type settingsResponse struct {
	Enabled    bool   `json:"enabled"`
	AlarmLive  bool   `json:"alarm_live"`
	TasksToday int    `json:"tasks_today"`
	Timezone   string `json:"timezone"`
}

func viewToResponse(v automatrixsettings.State) settingsResponse {
	return settingsResponse{
		Enabled:    v.Enabled,
		AlarmLive:  v.AlarmID != "",
		TasksToday: v.TasksToday,
		Timezone:   v.Timezone,
	}
}

func (s *Server) handleAutomatrixSettings(w http.ResponseWriter, r *http.Request) {
	ctl, ok := s.automatrixControlOrError(w)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, viewToResponse(ctl.SettingsView()))
	case http.MethodPut:
		var body struct {
			Enabled  bool    `json:"enabled"`
			Timezone *string `json:"timezone,omitempty"`
		}
		if body.Timezone != nil {
			if err := ctl.SetTimezone(*body.Timezone); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		if err := ctl.SetEnabled(r.Context(), body.Enabled); err != nil {
			logAutomatrixGovernorErr("toggle opt-in", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not apply the setting: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, viewToResponse(ctl.SettingsView()))
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// queueItem is one management-queue row (req 7.3). Financial = !eligible: the
// client renders an approve action for financial items and dismiss for all.
type queueItem struct {
	URI        string  `json:"uri"`
	Summary    string  `json:"summary"`
	Rationale  string  `json:"rationale"`
	Financial  bool    `json:"financial"`
	Confidence float32 `json:"confidence"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at,omitempty"`
}

func (s *Server) handleAutomatrixQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine.pager == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": []queueItem{}})
		return
	}
	opps, err := s.engine.pager.QueuedOpportunities(r.Context(), 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read queue: " + err.Error()})
		return
	}
	items := make([]queueItem, 0, len(opps))
	for _, o := range opps {
		row := queueItem{
			URI:        o.URI,
			Summary:    o.Summary,
			Rationale:  o.Rationale,
			Financial:  !o.EligibleAutonomous,
			Confidence: o.Confidence,
			Status:     string(o.Status),
		}
		if !o.CreatedAt.IsZero() {
			row.CreatedAt = o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		items = append(items, row)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

// uriBody is the shared {uri} request body for dismiss/approve.
type uriBody struct {
	URI string `json:"uri"`
}

func (s *Server) handleAutomatrixDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	var body uriBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	uri := strings.TrimSpace(body.URI)
	if uri == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uri is required"})
		return
	}
	if err := s.engine.pager.SetOpportunityStatus(r.Context(), uri, memory.OpportunityDismissed); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dismiss: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "uri": uri, "status": string(memory.OpportunityDismissed)})
}

// handleAutomatrixApprove approves a (typically financial) opportunity for a
// NORMAL, gated, user-initiated run. It dispatches the item exactly like a user
// message through the standard session path — so a financial item runs as an
// ordinary core_execute-gated task (the user approves any value-moving step
// inline), NEVER on the restricted autonomous surface and NEVER auto-run. The
// opportunity is then marked scheduled so it leaves the pending management queue.
func (s *Server) handleAutomatrixApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	var body uriBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	uri := strings.TrimSpace(body.URI)
	if uri == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uri is required"})
		return
	}
	// Resolve the opportunity from the pending queue so the dispatched objective
	// is built from its real summary + rationale (and resumes into its origin
	// conversation for context fidelity).
	opp, ok := s.findQueuedOpportunity(r.Context(), uri)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending opportunity for that uri"})
		return
	}
	objective := buildApprovalObjective(opp)
	convID := strings.TrimSpace(opp.OriginConversationID)
	if convID == "" {
		convID = synthConvID(objective)
	}
	// Dispatch exactly like POST /chat: mint/resume the session, submit the
	// objective as a normal (gated) user turn, and persist the user turn stamped
	// with the run id. This is the existing task-dispatch path — no new money
	// path is invented; a value-moving step still crosses core_execute under the
	// user's inline approval. The terminal observer settles the opportunity when
	// the run finishes: a genuine completion marks it done and writes the
	// durable completion record (so it shows in the Done inbox, req 7.4); a
	// failure re-pends it; a deliberate user stop returns it to the queue
	// untouched. Without this the approved item was parked at "scheduled"
	// forever and the finished task never appeared anywhere.
	sess := s.engine.sessions.get(convID)
	engine, oppCopy := s.engine, opp
	runID, fresh := sess.submitWith(objective, func(status task.Status) {
		engine.settleApprovedOpportunity(oppCopy, convID, status)
	})
	s.engine.conv.AppendUser(convID, runID, objective)
	// The item is now being handled by a user-initiated run; take it out of the
	// pending management queue.
	if err := s.engine.pager.SetOpportunityStatus(r.Context(), uri, memory.OpportunityScheduled); err != nil {
		logAutomatrixGovernorErr("approve set status", err)
	}
	if !fresh {
		// The objective was queued into an already-live run: no terminal
		// observer rides it, so settle bookkeeping can't run. Log so a stuck
		// "scheduled" item is diagnosable rather than silent.
		logAutomatrixGovernorErr("approve", fmt.Errorf("objective queued into live run %s; opportunity %s will not auto-settle", runID, uri))
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"ok":              true,
		"uri":             uri,
		"conversation_id": convID,
		"intent_id":       runID,
		"events_url":      "/events?intent_id=" + runID,
		"poll_url":        "/messages/async/" + runID,
	})
}

// findQueuedOpportunity returns the pending opportunity with the given uri.
func (s *Server) findQueuedOpportunity(ctx context.Context, uri string) (memory.OpportunitySpec, bool) {
	opps, err := s.engine.pager.QueuedOpportunities(ctx, 0)
	if err != nil {
		return memory.OpportunitySpec{}, false
	}
	for _, o := range opps {
		if o.URI == uri {
			return o, true
		}
	}
	return memory.OpportunitySpec{}, false
}

// buildApprovalObjective composes the user-facing task objective for an
// approved opportunity from its summary + grounding rationale. The framing
// tells the agent the user approved this from Settings and is likely away —
// the runs Andrew reviewed ended with "Want me to…?" questions and "on your
// screen" narration nobody was there to see.
func buildApprovalObjective(o memory.OpportunitySpec) string {
	obj := strings.TrimSpace(o.Summary)
	if r := strings.TrimSpace(o.Rationale); r != "" {
		obj += "\n\n(Context: " + r + ")"
	}
	obj += "\n\nThe user approved this task from their Settings queue — it is background work, and they are probably not watching it run. Complete it end-to-end and deliver ONE self-contained written result they can read later: lead with the outcome, include the concrete artifacts inline, do not present anything as being live on their screen, and do not end with questions or offers — nobody is there to answer them."
	return obj
}

// settleApprovedOpportunity records the terminal outcome of an approved
// (user-initiated, gated) opportunity run — the sibling of
// settleAutomatrixOpportunity for the approve path. A genuine completion marks
// the opportunity done and writes the durable completion record + ping (the
// Done inbox); an interrupted run returns it to pending untouched (the user
// stopped it on purpose — no attempts penalty); any other terminal re-pends it
// with attempts++ (bounded, dismissed at the ceiling).
func (e *Engine) settleApprovedOpportunity(opp memory.OpportunitySpec, convID string, status task.Status) {
	if e.pager == nil || strings.TrimSpace(opp.URI) == "" {
		return
	}
	ctx := context.Background()
	switch status {
	case task.StatusDone:
		if err := e.pager.SetOpportunityStatus(ctx, opp.URI, memory.OpportunityDone); err != nil {
			logAutomatrixGovernorErr("approve settle done", err)
		}
		e.announceAutomatrixCompletion(convID, opp.Summary, e.lastAssistantText(convID))
	case task.StatusInterrupted:
		if err := e.pager.SetOpportunityStatus(ctx, opp.URI, memory.OpportunityPending); err != nil {
			logAutomatrixGovernorErr("approve settle interrupted", err)
		}
	default:
		if _, err := e.pager.FailOpportunityAttempt(ctx, opp.URI, automatrixMaxAttempts); err != nil {
			logAutomatrixGovernorErr("approve settle failed attempt", err)
		}
	}
}

// inboxResponse is the GET /automatrix/inbox wire shape — the completion inbox
// (result-not-protocol) plus the unread badge count (req 7.4).
func (s *Server) handleAutomatrixInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	inbox := s.engine.AutomatrixInbox()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":  inbox.List(),
		"unread": inbox.Unread(),
	})
}

func (s *Server) handleAutomatrixInboxRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	changed, err := s.engine.AutomatrixInbox().MarkRead(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "mark read: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id, "changed": changed})
}
