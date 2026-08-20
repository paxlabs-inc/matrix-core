// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

// daemon_personalization_routes.go implements the personalization profile
// surface on the per-user daemon (ORACLE req 13): GET/PUT/DELETE /personalization
// and GET /personalization/export.
//
// The confirmed personalization profile (interests, day-to-day goals, media
// tastes with dislikes per category, recommendation adventurousness, and brief
// content preferences) is stored as ONE versioned, tagged, pinned cortex
// record — SEPARATE from the onboarding profile and NEVER overloading its
// expertise_domains (req 13.1).
//
// Carrier mapping (mirrors the Automatrix Opportunity → Goal mapping so cortex's
// closed 9-type taxonomy stays untouched): the record is a TypeGoal tagged
// "personalization-profile" with Status active (so it lands in the pinned tier),
// and the dedicated schema rides as a canonical-JSON sidecar in
// GoalData.SuccessCriteria[0]. forms.Render only emits the criteria COUNT for a
// Goal, so the sidecar never pollutes forms or embeddings.
//
// Absent profile → GET returns an empty (schema-versioned) profile; mutations
// only ever originate from confirmed interview output, explicit Settings edits,
// or explicit recommendation feedback (req 13.3 — enforced by the fact that
// this is the sole write surface and it carries no inferred-trait fields).

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"centra/core/cortex"
	"centra/core/cortex/memory"
)

// personalizationTag marks the single personalization-profile cortex record.
// Goal{Active} records are pinned by tier, so the profile is never evicted.
const personalizationTag memory.Tag = "personalization-profile"

// personalizationStatement is the fixed Goal.Statement for the carrier record.
// It carries no user content (that lives in the sidecar), so it is safe to
// render into forms.
const personalizationStatement = "personalization profile"

// personalizationSidecarPrefix namespaces the canonical-JSON payload stored in
// SuccessCriteria[0], versioning the sidecar schema independently of the record.
const personalizationSidecarPrefix = "neo.personalization.profile.v1:"

// personalizationSchemaVersion is the current profile schema version.
const personalizationSchemaVersion = 1

// mediaTaste is a liked/disliked pair for one media category (req 13.1).
type mediaTaste struct {
	Liked    []string `json:"liked,omitempty"`
	Disliked []string `json:"disliked,omitempty"`
}

// mediaTastes groups the per-category likes/dislikes captured by the interview.
type mediaTastes struct {
	Music    mediaTaste `json:"music,omitempty"`
	Films    mediaTaste `json:"films,omitempty"`
	Shows    mediaTaste `json:"shows,omitempty"`
	Books    mediaTaste `json:"books,omitempty"`
	Games    mediaTaste `json:"games,omitempty"`
	Creators mediaTaste `json:"creators,omitempty"`
}

// briefPreferences is the profile's slice of brief CONTENT preferences. The
// operational SCHEDULE (timezone, delivery time, days, alarm id, paused,
// last-delivered) lives in the separate briefsettings sidecar (req 14.1); only
// the durable content shape (length + selected sections) belongs to the profile.
type briefPreferences struct {
	Length   string   `json:"length,omitempty"`   // short|standard|deep
	Sections []string `json:"sections,omitempty"` // closed set — see validPersonalizationSections
}

// personalizationProfile is the dedicated, inspectable schema (req 13.1). It
// contains NO inferred sensitive-trait fields (req 13.3).
type personalizationProfile struct {
	SchemaVersion    int              `json:"schema_version"`
	Interests        []string         `json:"interests,omitempty"`
	DayToDayGoals    []string         `json:"day_to_day_goals,omitempty"`
	Media            mediaTastes      `json:"media,omitempty"`
	Adventurousness  string           `json:"adventurousness,omitempty"` // familiar|balanced|surprising
	BriefPreferences briefPreferences `json:"brief_preferences,omitempty"`
}

// personalizationResponse is GET /personalization: the profile plus its cortex
// URI (empty when no record exists yet).
type personalizationResponse struct {
	Profile personalizationProfile `json:"profile"`
	URI     string                 `json:"uri,omitempty"`
}

// personalizationExport is GET /personalization/export: the profile with its
// provenance/version lineage for user data export (req 13.2).
type personalizationExport struct {
	Tag           string                 `json:"tag"`
	SchemaVersion int                    `json:"schema_version"`
	Profile       personalizationProfile `json:"profile"`
	URI           string                 `json:"uri,omitempty"`
	Version       uint64                 `json:"version,omitempty"`
	UpdatedAt     string                 `json:"updated_at,omitempty"`
	CreatedBy     string                 `json:"created_by,omitempty"`
	Exists        bool                   `json:"exists"`
}

// handlePersonalization serves GET/PUT/DELETE /personalization.
func (d *daemonState) handlePersonalization(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		d.getPersonalization(w, r)
	case http.MethodPut:
		d.putPersonalization(w, r)
	case http.MethodDelete:
		d.deletePersonalization(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handlePersonalizationExport serves GET /personalization/export.
func (d *daemonState) handlePersonalizationExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusOK, personalizationExport{Tag: string(personalizationTag), SchemaVersion: personalizationSchemaVersion, Exists: false})
		return
	}
	mem, err := d.findPersonalizationMemory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization lookup: " + err.Error()})
		return
	}
	out := personalizationExport{Tag: string(personalizationTag), SchemaVersion: personalizationSchemaVersion, Exists: mem != nil}
	if mem == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	prof, derr := decodePersonalization(mem)
	if derr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization decode: " + derr.Error()})
		return
	}
	out.Profile = prof
	out.SchemaVersion = prof.SchemaVersion
	out.URI = string(cortex.BuildURI(memory.TypeGoal, mem.Head.ID, mem.Head.CurrentVersion))
	out.Version = mem.Head.CurrentVersion
	out.CreatedBy = mem.Version.CreatedBy
	if !mem.Version.CreatedAt.IsZero() {
		out.UpdatedAt = mem.Version.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *daemonState) getPersonalization(w http.ResponseWriter, r *http.Request) {
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusOK, personalizationResponse{Profile: emptyPersonalization()})
		return
	}
	mem, err := d.findPersonalizationMemory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization lookup: " + err.Error()})
		return
	}
	if mem == nil {
		writeJSON(w, http.StatusOK, personalizationResponse{Profile: emptyPersonalization()})
		return
	}
	prof, derr := decodePersonalization(mem)
	if derr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization decode: " + derr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, personalizationResponse{
		Profile: prof,
		URI:     string(cortex.BuildURI(memory.TypeGoal, mem.Head.ID, mem.Head.CurrentVersion)),
	})
}

func (d *daemonState) putPersonalization(w http.ResponseWriter, r *http.Request) {
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}

	var req personalizationProfile
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	norm, verr := validatePersonalization(req)
	if verr != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": verr})
		return
	}

	sidecar, err := encodePersonalizationSidecar(norm)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization encode: " + err.Error()})
		return
	}

	createdBy := "matrix://personalization"
	actorScope := ""
	if d.actor != nil && d.actor.UserURI != "" {
		createdBy = d.actor.UserURI
		actorScope = d.actor.UserURI
	}
	data := memory.GoalData{
		SchemaVersion:   1,
		Statement:       personalizationStatement,
		Status:          memory.GoalActive,
		SuccessCriteria: []string{sidecar},
	}
	meta := cortex.WriteMeta{
		CreatedBy:  createdBy,
		Confidence: 1.0,
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	}

	existing, err := d.findPersonalizationMemory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization lookup: " + err.Error()})
		return
	}
	if existing != nil {
		uri := cortex.BuildURI(memory.TypeGoal, existing.Head.ID, existing.Head.CurrentVersion)
		newURI, uerr := d.infra.cortex.Update(uri, data, meta)
		if uerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization update: " + uerr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, personalizationResponse{Profile: norm, URI: string(newURI)})
		return
	}

	head := memory.Head{
		ActorScope:         actorScope,
		Visibility:         memory.VisPrivate,
		DeclaredImportance: 10,
		Tags:               []memory.Tag{personalizationTag},
	}
	uri, werr := d.infra.cortex.Write(head, data, meta)
	if werr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization write: " + werr.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, personalizationResponse{Profile: norm, URI: string(uri)})
}

func (d *daemonState) deletePersonalization(w http.ResponseWriter, r *http.Request) {
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	mem, err := d.findPersonalizationMemory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization lookup: " + err.Error()})
		return
	}
	if mem == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": false})
		return
	}
	by := "matrix://personalization"
	if d.actor != nil && d.actor.UserURI != "" {
		by = d.actor.UserURI
	}
	uri := cortex.BuildURI(memory.TypeGoal, mem.Head.ID, mem.Head.CurrentVersion)
	if terr := d.infra.cortex.Tombstone(uri, "user deleted personalization profile", by); terr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "personalization delete: " + terr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// findPersonalizationMemory scans Goal-type memories for the one tagged
// "personalization-profile". Returns nil (no error) when none exists.
func (d *daemonState) findPersonalizationMemory() (*memory.Memory, error) {
	if d.infra == nil || d.infra.cortex == nil {
		return nil, nil
	}
	ids, err := d.infra.cortex.ListByType(memory.TypeGoal, 0)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		mem, rerr := d.infra.cortex.ResolveLatest(id)
		if rerr != nil {
			if errors.Is(rerr, memory.ErrNotFound) {
				continue
			}
			return nil, rerr
		}
		if mem.Head.Tombstoned != nil {
			continue
		}
		for _, t := range mem.Head.Tags {
			if t == personalizationTag {
				return mem, nil
			}
		}
	}
	return nil, nil
}

// decodePersonalization extracts the profile sidecar from a carrier Goal
// memory. A malformed/absent sidecar yields an empty (schema-versioned)
// profile rather than an error, so a corrupted record still reads cleanly.
func decodePersonalization(mem *memory.Memory) (personalizationProfile, error) {
	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return personalizationProfile{}, err
	}
	gd, ok := data.(memory.GoalData)
	if !ok || len(gd.SuccessCriteria) == 0 {
		return emptyPersonalization(), nil
	}
	raw := gd.SuccessCriteria[0]
	if !strings.HasPrefix(raw, personalizationSidecarPrefix) {
		return emptyPersonalization(), nil
	}
	var prof personalizationProfile
	if uerr := json.Unmarshal([]byte(strings.TrimPrefix(raw, personalizationSidecarPrefix)), &prof); uerr != nil {
		return emptyPersonalization(), nil
	}
	if prof.SchemaVersion == 0 {
		prof.SchemaVersion = personalizationSchemaVersion
	}
	return prof, nil
}

// encodePersonalizationSidecar renders the profile as the prefixed canonical
// JSON string carried in SuccessCriteria[0].
func encodePersonalizationSidecar(p personalizationProfile) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return personalizationSidecarPrefix + string(b), nil
}

// emptyPersonalization is the zero profile returned when no record exists.
func emptyPersonalization() personalizationProfile {
	return personalizationProfile{SchemaVersion: personalizationSchemaVersion}
}

// Personalization input bounds — the record rides the pinned tier, so keep it
// small and well-formed.
const (
	maxInterests           = 30
	maxDayToDayGoals       = 20
	maxMediaItemsPerList   = 30
	maxPersonalizationItem = 120
	maxBriefSections       = 6
)

// validPersonalizationAdventure is the closed adventurousness vocabulary.
var validPersonalizationAdventure = map[string]struct{}{
	"familiar": {}, "balanced": {}, "surprising": {},
}

// validPersonalizationLength is the closed brief-length vocabulary.
var validPersonalizationLength = map[string]struct{}{
	"short": {}, "standard": {}, "deep": {},
}

// validPersonalizationSections is the closed brief-section vocabulary (req 11.2).
var validPersonalizationSections = map[string]struct{}{
	"news": {}, "music": {}, "movies_tv": {}, "books": {}, "games": {}, "daily_assistance": {},
}

// validatePersonalization enforces the input bounds and closed vocabularies and
// returns the normalized profile (trimmed, empties dropped, deduped). A
// non-empty second return value is a plain-language validation error.
func validatePersonalization(p personalizationProfile) (personalizationProfile, string) {
	out := personalizationProfile{SchemaVersion: personalizationSchemaVersion}

	var verr string
	if out.Interests, verr = cleanStrList(p.Interests, maxInterests, "interests"); verr != "" {
		return personalizationProfile{}, verr
	}
	if out.DayToDayGoals, verr = cleanStrList(p.DayToDayGoals, maxDayToDayGoals, "day_to_day_goals"); verr != "" {
		return personalizationProfile{}, verr
	}
	if out.Media, verr = cleanMedia(p.Media); verr != "" {
		return personalizationProfile{}, verr
	}

	adv := strings.ToLower(strings.TrimSpace(p.Adventurousness))
	if adv != "" {
		if _, ok := validPersonalizationAdventure[adv]; !ok {
			return personalizationProfile{}, fmt.Sprintf("adventurousness must be one of familiar|balanced|surprising (got %q)", p.Adventurousness)
		}
		out.Adventurousness = adv
	}

	length := strings.ToLower(strings.TrimSpace(p.BriefPreferences.Length))
	if length != "" {
		if _, ok := validPersonalizationLength[length]; !ok {
			return personalizationProfile{}, fmt.Sprintf("brief length must be one of short|standard|deep (got %q)", p.BriefPreferences.Length)
		}
		out.BriefPreferences.Length = length
	}
	sections := make([]string, 0, len(p.BriefPreferences.Sections))
	seen := make(map[string]struct{}, len(p.BriefPreferences.Sections))
	for _, s := range p.BriefPreferences.Sections {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, ok := validPersonalizationSections[s]; !ok {
			return personalizationProfile{}, fmt.Sprintf("unknown brief section %q", s)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		sections = append(sections, s)
		if len(sections) > maxBriefSections {
			return personalizationProfile{}, fmt.Sprintf("too many brief sections (max %d)", maxBriefSections)
		}
	}
	if len(sections) > 0 {
		out.BriefPreferences.Sections = sections
	}
	return out, ""
}

// cleanStrList trims, drops empties, case-insensitively dedups (order-
// preserving), and bounds one string list.
func cleanStrList(in []string, max int, field string) ([]string, string) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len([]rune(s)) > maxPersonalizationItem {
			return nil, fmt.Sprintf("%s item too long (max %d characters): %q", field, maxPersonalizationItem, s)
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
		if len(out) > max {
			return nil, fmt.Sprintf("too many %s (max %d)", field, max)
		}
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// cleanMedia normalizes every per-category liked/disliked list.
func cleanMedia(m mediaTastes) (mediaTastes, string) {
	var out mediaTastes
	cats := []struct {
		name string
		src  mediaTaste
		dst  *mediaTaste
	}{
		{"music", m.Music, &out.Music},
		{"films", m.Films, &out.Films},
		{"shows", m.Shows, &out.Shows},
		{"books", m.Books, &out.Books},
		{"games", m.Games, &out.Games},
		{"creators", m.Creators, &out.Creators},
	}
	for _, c := range cats {
		liked, verr := cleanStrList(c.src.Liked, maxMediaItemsPerList, c.name+".liked")
		if verr != "" {
			return mediaTastes{}, verr
		}
		disliked, verr := cleanStrList(c.src.Disliked, maxMediaItemsPerList, c.name+".disliked")
		if verr != "" {
			return mediaTastes{}, verr
		}
		c.dst.Liked = liked
		c.dst.Disliked = disliked
	}
	return out, ""
}
