// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

// The morning-brief control surface (ORACLE task 5.2, req 11.1/11.2/11.3/14).
// Neo-owned authenticated routes registered ahead of the daemon proxy:
//
//   GET  /brief/settings         read the schedule (enabled, tz, time, days,
//                                length, sections, paused, alarm live)
//   PUT  /brief/settings         edit the schedule and/or toggle enable/pause:
//                                  {enabled} ON  -> create the ONE MORNING_BRIEF alarm
//                                           OFF -> cancel it (req 11.3)
//                                  schedule fields -> persist + reschedule when live
//                                  {paused} -> skip without disabling (req 14.2)
//
// The toggle/edit goes through the production briefGovernor (wired in serve.go /
// NewEngine). Default off (req 11.1): a fresh user reads {enabled:false}.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"matrix/neo/internal/briefhistory"
	"matrix/neo/internal/briefsettings"
)

// briefControl is the slice of the production brief governor the control surface
// needs. The engine stores the concrete governor; the endpoints use this
// interface so a nil governor cleanly yields a 503 rather than a panic.
type briefControl interface {
	SetEnabled(ctx context.Context, enabled bool) error
	SetPaused(ctx context.Context, paused bool) error
	UpdateSchedule(ctx context.Context, sc briefsettings.Schedule) error
	SettingsView() briefsettings.State
}

// handleBrief is the prefix dispatcher for /brief/*.
func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/brief/settings":
		s.handleBriefSettings(w, r)
	case "/brief/feedback":
		s.handleBriefFeedback(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown brief route"})
	}
}

// handleBriefFeedback records the user's EXPLICIT reaction to a delivered brief
// item (task 5.5, req 17.2): the durable history takes it (so composition stops
// repeating it), and it flows into the profile ONLY as an explicit,
// user-attributed preference entry — never silent inference.
func (s *Server) handleBriefFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine == nil || s.engine.briefHistory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "brief feedback not available"})
		return
	}
	var req struct {
		Item    string `json:"item"`
		Verdict string `json:"verdict"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	req.Item = strings.TrimSpace(req.Item)
	req.Verdict = strings.ToLower(strings.TrimSpace(req.Verdict))
	if err := s.engine.briefHistory.AppendFeedback(req.Item, req.Verdict); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Explicit user-attributed preference entry (req 17.2). Best-effort: the
	// durable history is the authoritative feedback record; a preference-write
	// failure never blocks the ack.
	if s.engine.pager != nil {
		polarity, strength := "prefer", float32(0.7)
		switch req.Verdict {
		case briefhistory.VerdictNotInterested:
			polarity, strength = "avoid", 0.8
		case briefhistory.VerdictLessOfThis:
			polarity, strength = "avoid", 0.6
		}
		_, _ = s.engine.pager.RememberPreference(r.Context(), "morning-brief item: "+req.Item, polarity, strength,
			"the user explicitly said "+req.Verdict+" on this delivered brief item")
	}
	writeJSON(w, http.StatusOK, map[string]bool{"recorded": true})
}

func (s *Server) briefControlOrError(w http.ResponseWriter) (briefControl, bool) {
	if s.engine == nil || s.engine.briefGov == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "brief control not available"})
		return nil, false
	}
	var ctl briefControl = s.engine.briefGov
	return ctl, true
}

// briefSettingsResponse is the GET/PUT /brief/settings wire shape. No alarm id
// or protocol detail leaks — only whether an alarm is currently live.
type briefSettingsResponse struct {
	Enabled      bool     `json:"enabled"`
	Paused       bool     `json:"paused"`
	AlarmLive    bool     `json:"alarm_live"`
	Timezone     string   `json:"timezone,omitempty"`
	DeliveryTime string   `json:"delivery_time,omitempty"`
	Days         []int    `json:"days,omitempty"`
	Length       string   `json:"length,omitempty"`
	Sections     []string `json:"sections,omitempty"`
}

func briefViewToResponse(v briefsettings.State) briefSettingsResponse {
	return briefSettingsResponse{
		Enabled:      v.Enabled,
		Paused:       v.Paused,
		AlarmLive:    v.AlarmID != "",
		Timezone:     v.Timezone,
		DeliveryTime: v.DeliveryTime,
		Days:         v.Days,
		Length:       v.Length,
		Sections:     v.Sections,
	}
}

// briefSettingsRequest is the PUT body. Every field is optional; a nil pointer
// leaves that dimension unchanged, so the client can toggle enable without
// resending the whole schedule (and vice-versa).
type briefSettingsRequest struct {
	Enabled        *bool     `json:"enabled,omitempty"`
	Paused         *bool     `json:"paused,omitempty"`
	Timezone       *string   `json:"timezone,omitempty"`
	DeliveryTime   *string   `json:"delivery_time,omitempty"`
	Days           *[]int    `json:"days,omitempty"`
	Length         *string   `json:"length,omitempty"`
	Sections       *[]string `json:"sections,omitempty"`
	ConversationID *string   `json:"conversation_id,omitempty"`
}

func (s *Server) handleBriefSettings(w http.ResponseWriter, r *http.Request) {
	ctl, ok := s.briefControlOrError(w)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, briefViewToResponse(ctl.SettingsView()))
	case http.MethodPut:
		var body briefSettingsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}

		// 1) Schedule edit first (so a same-call enable/reschedule uses the new
		//    values). Only issued when at least one schedule field is present.
		if body.touchesSchedule() {
			cur := ctl.SettingsView()
			sc := briefsettings.Schedule{
				Timezone:       strOr(body.Timezone, cur.Timezone),
				DeliveryTime:   strOr(body.DeliveryTime, cur.DeliveryTime),
				Days:           intsOr(body.Days, cur.Days),
				Length:         strOr(body.Length, cur.Length),
				Sections:       stringsOr(body.Sections, cur.Sections),
				ConversationID: strOr(body.ConversationID, cur.ConversationID),
			}
			if verr := validateBriefSchedule(sc); verr != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": verr})
				return
			}
			if err := ctl.UpdateSchedule(r.Context(), sc); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not update the schedule: " + err.Error()})
				return
			}
		}

		// 2) Pause toggle.
		if body.Paused != nil {
			if err := ctl.SetPaused(r.Context(), *body.Paused); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not apply pause: " + err.Error()})
				return
			}
		}

		// 3) Enable toggle last (creates/cancels the alarm from the now-current
		//    schedule).
		if body.Enabled != nil {
			if err := ctl.SetEnabled(r.Context(), *body.Enabled); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not apply the setting: " + err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, briefViewToResponse(ctl.SettingsView()))
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (b briefSettingsRequest) touchesSchedule() bool {
	return b.Timezone != nil || b.DeliveryTime != nil || b.Days != nil ||
		b.Length != nil || b.Sections != nil || b.ConversationID != nil
}

// validBriefLength / validBriefSections are the closed vocabularies (req 11.2),
// mirroring the personalization profile's brief-preference vocab on the daemon.
var validBriefLength = map[string]struct{}{"short": {}, "standard": {}, "deep": {}}
var validBriefSections = map[string]struct{}{
	"news": {}, "music": {}, "movies_tv": {}, "books": {}, "games": {}, "daily_assistance": {},
}

// validateBriefSchedule enforces the delivery-time format, day range, and closed
// vocabularies. Returns a plain-language error (caller rejects with 400) or "".
func validateBriefSchedule(sc briefsettings.Schedule) string {
	if dt := strings.TrimSpace(sc.DeliveryTime); dt != "" {
		if _, err := buildBriefCron(dt, nil); err != nil {
			return "invalid delivery time (want HH:MM)"
		}
	}
	for _, d := range sc.Days {
		if d < 0 || d > 6 {
			return "days must be 0 (Sunday) through 6 (Saturday)"
		}
	}
	if l := strings.ToLower(strings.TrimSpace(sc.Length)); l != "" {
		if _, ok := validBriefLength[l]; !ok {
			return "brief length must be one of short|standard|deep"
		}
	}
	for _, s := range sc.Sections {
		if _, ok := validBriefSections[strings.ToLower(strings.TrimSpace(s))]; !ok {
			return "unknown brief section: " + s
		}
	}
	return ""
}

func strOr(p *string, def string) string {
	if p != nil {
		return strings.TrimSpace(*p)
	}
	return def
}

func intsOr(p *[]int, def []int) []int {
	if p != nil {
		return *p
	}
	return def
}

func stringsOr(p *[]string, def []string) []string {
	if p != nil {
		return *p
	}
	return def
}
