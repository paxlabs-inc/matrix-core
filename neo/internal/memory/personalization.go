// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
)

// The ORACLE personalization profile (task 5.3, req 12.3/13.1/13.3) — Neo's
// write surface for the SAME single record the daemon exposes on
// GET/PUT/DELETE /personalization: ONE versioned, tagged, pinned cortex record,
// separate from the onboarding profile.
//
// Carrier mapping (byte-identical to the daemon's, mirroring the Automatrix
// Opportunity → Goal mapping so cortex's closed 9-type taxonomy stays
// untouched): a TypeGoal tagged "personalization-profile" with Status active;
// the dedicated schema rides as a prefixed canonical-JSON sidecar in
// GoalData.SuccessCriteria[0] (forms.Render only emits the criteria COUNT for a
// Goal, so the sidecar never pollutes forms or embeddings).
//
// Mutations only ever originate from confirmed interview output, explicit
// Settings edits, or explicit recommendation feedback (req 13.3): this surface
// is reached solely through the confirmation-gated save_personalization_profile
// tool (advertised only to interview agents) and carries no inferred-trait
// fields.

const (
	// personalizationTag marks the single profile record. MUST stay
	// byte-identical to the daemon's constant (daemon_personalization_routes.go)
	// — both surfaces address the same record.
	personalizationTag memory.Tag = "personalization-profile"

	// personalizationStatement is the fixed carrier Goal.Statement. No user
	// content (that lives in the sidecar), so it is safe to render into forms.
	personalizationStatement = "personalization profile"

	// personalizationSidecarPrefix namespaces the canonical-JSON payload in
	// SuccessCriteria[0]. MUST stay byte-identical to the daemon's constant.
	personalizationSidecarPrefix = "neo.personalization.profile.v1:"

	// personalizationSchemaVersion is the current profile schema version.
	personalizationSchemaVersion = 1

	// personalizationImportance pins the record into the durable tier.
	personalizationImportance = 10
)

// MediaTaste is a liked/disliked pair for one media category (req 13.1).
type MediaTaste struct {
	Liked    []string `json:"liked,omitempty"`
	Disliked []string `json:"disliked,omitempty"`
}

// MediaTastes groups the per-category likes/dislikes captured by the interview.
type MediaTastes struct {
	Music    MediaTaste `json:"music,omitempty"`
	Films    MediaTaste `json:"films,omitempty"`
	Shows    MediaTaste `json:"shows,omitempty"`
	Books    MediaTaste `json:"books,omitempty"`
	Games    MediaTaste `json:"games,omitempty"`
	Creators MediaTaste `json:"creators,omitempty"`
}

// BriefPreferences is the profile's slice of brief CONTENT preferences. The
// operational schedule (timezone, delivery time, days, alarm id) lives in the
// separate briefsettings sidecar (req 14.1).
type BriefPreferences struct {
	Length   string   `json:"length,omitempty"`   // short|standard|deep
	Sections []string `json:"sections,omitempty"` // closed set — news|music|movies_tv|books|games|daily_assistance
}

// PersonalizationProfile is the dedicated, inspectable schema (req 13.1). It
// contains NO inferred sensitive-trait fields (req 13.3) — a skipped interview
// group simply leaves its fields empty (req 12.2).
type PersonalizationProfile struct {
	SchemaVersion    int              `json:"schema_version"`
	Interests        []string         `json:"interests,omitempty"`
	DayToDayGoals    []string         `json:"day_to_day_goals,omitempty"`
	Media            MediaTastes      `json:"media,omitempty"`
	Adventurousness  string           `json:"adventurousness,omitempty"` // familiar|balanced|surprising
	BriefPreferences BriefPreferences `json:"brief_preferences,omitempty"`
}

// Empty reports whether no interview group produced any content.
func (p PersonalizationProfile) Empty() bool {
	return len(p.Interests) == 0 && len(p.DayToDayGoals) == 0 &&
		p.Adventurousness == "" && p.BriefPreferences.Length == "" &&
		len(p.BriefPreferences.Sections) == 0 && mediaEmpty(p.Media)
}

func mediaEmpty(m MediaTastes) bool {
	for _, t := range []MediaTaste{m.Music, m.Films, m.Shows, m.Books, m.Games, m.Creators} {
		if len(t.Liked) > 0 || len(t.Disliked) > 0 {
			return false
		}
	}
	return true
}

// RenderForInterview renders the saved profile as the plain-language existing-
// answers block a REPEAT interview re-enters with (req 12.1 "repeat/edit
// re-enters with existing answers"). Empty profile renders "".
func (p PersonalizationProfile) RenderForInterview() string {
	if p.Empty() {
		return ""
	}
	var b strings.Builder
	line := func(label string, vals []string) {
		if len(vals) > 0 {
			fmt.Fprintf(&b, "- %s: %s\n", label, strings.Join(vals, ", "))
		}
	}
	line("Interests", p.Interests)
	line("Day-to-day help", p.DayToDayGoals)
	media := func(cat string, t MediaTaste) {
		if len(t.Liked) > 0 {
			fmt.Fprintf(&b, "- %s they like: %s\n", cat, strings.Join(t.Liked, ", "))
		}
		if len(t.Disliked) > 0 {
			fmt.Fprintf(&b, "- %s they dislike: %s\n", cat, strings.Join(t.Disliked, ", "))
		}
	}
	media("Music", p.Media.Music)
	media("Films", p.Media.Films)
	media("Shows", p.Media.Shows)
	media("Books", p.Media.Books)
	media("Games", p.Media.Games)
	media("Creators", p.Media.Creators)
	if p.Adventurousness != "" {
		fmt.Fprintf(&b, "- Recommendation adventurousness: %s\n", p.Adventurousness)
	}
	if p.BriefPreferences.Length != "" {
		fmt.Fprintf(&b, "- Brief length: %s\n", p.BriefPreferences.Length)
	}
	if len(p.BriefPreferences.Sections) > 0 {
		fmt.Fprintf(&b, "- Brief sections: %s\n", strings.Join(p.BriefPreferences.Sections, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// PersonalizationProfile returns the single saved profile (zero-value profile +
// ok=false when none exists). Tombstoned records read as absent.
func (p *Pager) PersonalizationProfile(ctx context.Context) (PersonalizationProfile, string, bool, error) {
	_ = ctx
	mem, err := p.findPersonalizationMemory()
	if err != nil {
		return PersonalizationProfile{}, "", false, err
	}
	if mem == nil {
		return PersonalizationProfile{SchemaVersion: personalizationSchemaVersion}, "", false, nil
	}
	prof := decodePersonalizationGoal(mem)
	uri := string(cortex.BuildURI(memory.TypeGoal, mem.Head.ID, mem.Head.CurrentVersion))
	return prof, uri, true, nil
}

// PersonalizationRecord is the profile plus its provenance/version lineage for
// user data export (req 8.3/13.2).
type PersonalizationRecord struct {
	Profile   PersonalizationProfile `json:"profile"`
	URI       string                 `json:"uri,omitempty"`
	Version   uint64                 `json:"version,omitempty"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
	CreatedBy string                 `json:"created_by,omitempty"`
	Exists    bool                   `json:"exists"`
}

// PersonalizationExport returns the single saved profile with its version
// lineage. A missing record yields Exists=false with an empty profile.
func (p *Pager) PersonalizationExport(ctx context.Context) (PersonalizationRecord, error) {
	_ = ctx
	out := PersonalizationRecord{Profile: PersonalizationProfile{SchemaVersion: personalizationSchemaVersion}}
	mem, err := p.findPersonalizationMemory()
	if err != nil {
		return PersonalizationRecord{}, err
	}
	if mem == nil {
		return out, nil
	}
	out.Exists = true
	out.Profile = decodePersonalizationGoal(mem)
	out.URI = string(cortex.BuildURI(memory.TypeGoal, mem.Head.ID, mem.Head.CurrentVersion))
	out.Version = mem.Head.CurrentVersion
	out.CreatedBy = mem.Version.CreatedBy
	if !mem.Version.CreatedAt.IsZero() {
		out.UpdatedAt = mem.Version.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	return out, nil
}

// SavePersonalizationProfile normalizes, validates, and persists the profile as
// the single authoritative record: a versioned UPDATE when one exists, a fresh
// pinned record otherwise (req 13.1 — never a second record, never a fragment).
// Returns the record URI.
func (p *Pager) SavePersonalizationProfile(ctx context.Context, prof PersonalizationProfile) (string, error) {
	_ = ctx
	norm, verr := normalizePersonalization(prof)
	if verr != "" {
		return "", errors.New(verr)
	}
	sidecar, err := json.Marshal(norm)
	if err != nil {
		return "", fmt.Errorf("neo/memory: personalization encode: %w", err)
	}
	data := memory.GoalData{
		SchemaVersion:   1,
		Statement:       personalizationStatement,
		Status:          memory.GoalActive,
		SuccessCriteria: []string{personalizationSidecarPrefix + string(sidecar)},
	}
	meta := cortex.WriteMeta{
		CreatedBy:  p.cfg.CortexActor,
		Confidence: 1.0,
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	}

	existing, err := p.findPersonalizationMemory()
	if err != nil {
		return "", err
	}
	if existing != nil {
		uri := cortex.BuildURI(memory.TypeGoal, existing.Head.ID, existing.Head.CurrentVersion)
		newURI, uerr := p.cortex.Update(uri, data, meta)
		if uerr != nil {
			return "", fmt.Errorf("neo/memory: personalization update: %w", uerr)
		}
		return string(newURI), nil
	}

	head := memory.Head{
		ActorScope:         p.cfg.CortexActor,
		Visibility:         memory.VisPrivate,
		DeclaredImportance: personalizationImportance,
		Tags:               []memory.Tag{personalizationTag},
	}
	uri, werr := p.cortex.Write(head, data, meta)
	if werr != nil {
		return "", fmt.Errorf("neo/memory: personalization write: %w", werr)
	}
	return string(uri), nil
}

// DeletePersonalization tombstones the single personalization profile record
// through the existing Cortex tombstone path (the same primitive typed-memory
// delete uses), removing it from current retrieval, indexes, and caches. It
// reports whether a record was present; a missing record is a no-op success.
// The async embedder is drained around the versioned tombstone so a queued
// embed of the prior version cannot land after the delete.
func (p *Pager) DeletePersonalization(ctx context.Context) (bool, error) {
	if p == nil || p.cortex == nil {
		return false, errors.New("neo/memory: pager unavailable")
	}
	if p.hasEmbedder {
		if err := p.cortex.DrainEmbedder(ctx); err != nil {
			return false, fmt.Errorf("prepare personalization indexes: %w", err)
		}
		defer func() { _ = p.cortex.DrainEmbedder(ctx) }()
	}
	mem, err := p.findPersonalizationMemory()
	if err != nil {
		return false, err
	}
	if mem == nil {
		return false, nil
	}
	uri := cortex.BuildURI(memory.TypeGoal, mem.Head.ID, mem.Head.CurrentVersion)
	if err := p.cortex.Tombstone(uri, "personalization profile deleted by user", p.cfg.CortexActor); err != nil {
		return false, fmt.Errorf("neo/memory: personalization delete: %w", err)
	}
	return true, nil
}

// findPersonalizationMemory scans Goal-type memories for the one tagged
// personalization-profile — the same lookup the daemon route uses, so both
// surfaces resolve the same record. Returns nil (no error) when none exists.
func (p *Pager) findPersonalizationMemory() (*memory.Memory, error) {
	ids, err := p.cortex.ListByType(memory.TypeGoal, 0)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		mem, rerr := p.cortex.ResolveLatest(id)
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

// decodePersonalizationGoal extracts the profile sidecar from the carrier Goal.
// A malformed/absent sidecar yields an empty (schema-versioned) profile rather
// than an error, so a corrupted record still reads cleanly.
func decodePersonalizationGoal(mem *memory.Memory) PersonalizationProfile {
	empty := PersonalizationProfile{SchemaVersion: personalizationSchemaVersion}
	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return empty
	}
	gd, ok := asGoalData(data)
	if !ok || len(gd.SuccessCriteria) == 0 {
		return empty
	}
	raw := gd.SuccessCriteria[0]
	if !strings.HasPrefix(raw, personalizationSidecarPrefix) {
		return empty
	}
	var prof PersonalizationProfile
	if uerr := json.Unmarshal([]byte(strings.TrimPrefix(raw, personalizationSidecarPrefix)), &prof); uerr != nil {
		return empty
	}
	if prof.SchemaVersion == 0 {
		prof.SchemaVersion = personalizationSchemaVersion
	}
	return prof
}

// Personalization input bounds and closed vocabularies — byte-identical to the
// daemon's validation (daemon_personalization_routes.go) so the two write
// surfaces accept the same shape.
const (
	maxPersonalizationInterests = 30
	maxPersonalizationGoals     = 20
	maxPersonalizationMedia     = 30
	maxPersonalizationItemLen   = 120
	maxPersonalizationSections  = 6
)

var validPersonalizationAdventure = map[string]struct{}{
	"familiar": {}, "balanced": {}, "surprising": {},
}

var validPersonalizationLength = map[string]struct{}{
	"short": {}, "standard": {}, "deep": {},
}

var validPersonalizationSections = map[string]struct{}{
	"news": {}, "music": {}, "movies_tv": {}, "books": {}, "games": {}, "daily_assistance": {},
}

// normalizePersonalization enforces the bounds and closed vocabularies and
// returns the normalized profile (trimmed, empties dropped, deduped). A
// non-empty second return is a plain-language validation error.
func normalizePersonalization(p PersonalizationProfile) (PersonalizationProfile, string) {
	out := PersonalizationProfile{SchemaVersion: personalizationSchemaVersion}

	var verr string
	if out.Interests, verr = cleanPersonalizationList(p.Interests, maxPersonalizationInterests, "interests"); verr != "" {
		return PersonalizationProfile{}, verr
	}
	if out.DayToDayGoals, verr = cleanPersonalizationList(p.DayToDayGoals, maxPersonalizationGoals, "day_to_day_goals"); verr != "" {
		return PersonalizationProfile{}, verr
	}
	cats := []struct {
		name string
		src  MediaTaste
		dst  *MediaTaste
	}{
		{"music", p.Media.Music, &out.Media.Music},
		{"films", p.Media.Films, &out.Media.Films},
		{"shows", p.Media.Shows, &out.Media.Shows},
		{"books", p.Media.Books, &out.Media.Books},
		{"games", p.Media.Games, &out.Media.Games},
		{"creators", p.Media.Creators, &out.Media.Creators},
	}
	for _, c := range cats {
		if c.dst.Liked, verr = cleanPersonalizationList(c.src.Liked, maxPersonalizationMedia, c.name+".liked"); verr != "" {
			return PersonalizationProfile{}, verr
		}
		if c.dst.Disliked, verr = cleanPersonalizationList(c.src.Disliked, maxPersonalizationMedia, c.name+".disliked"); verr != "" {
			return PersonalizationProfile{}, verr
		}
	}

	adv := strings.ToLower(strings.TrimSpace(p.Adventurousness))
	if adv != "" {
		if _, ok := validPersonalizationAdventure[adv]; !ok {
			return PersonalizationProfile{}, fmt.Sprintf("adventurousness must be one of familiar|balanced|surprising (got %q)", p.Adventurousness)
		}
		out.Adventurousness = adv
	}

	length := strings.ToLower(strings.TrimSpace(p.BriefPreferences.Length))
	if length != "" {
		if _, ok := validPersonalizationLength[length]; !ok {
			return PersonalizationProfile{}, fmt.Sprintf("brief length must be one of short|standard|deep (got %q)", p.BriefPreferences.Length)
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
			return PersonalizationProfile{}, fmt.Sprintf("unknown brief section %q", s)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		sections = append(sections, s)
		if len(sections) > maxPersonalizationSections {
			return PersonalizationProfile{}, fmt.Sprintf("too many brief sections (max %d)", maxPersonalizationSections)
		}
	}
	if len(sections) > 0 {
		out.BriefPreferences.Sections = sections
	}
	return out, ""
}

// cleanPersonalizationList trims, drops empties, case-insensitively dedups
// (order-preserving), and bounds one string list.
func cleanPersonalizationList(in []string, max int, field string) ([]string, string) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len([]rune(s)) > maxPersonalizationItemLen {
			return nil, fmt.Sprintf("%s item too long (max %d characters): %q", field, maxPersonalizationItemLen, s)
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
