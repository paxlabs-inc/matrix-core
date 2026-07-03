// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"matrix/cody/internal/llm"
)

// DesignProfile is the product context the design language is chosen FOR — the
// facts that make one visual direction fit better than another. A direction
// that ignores this profile is exactly the AI-default the banned-tells screen
// and the anti-default lens reject.
type DesignProfile struct {
	ProductKind     string `json:"product_kind"`
	Audience        string `json:"audience"`
	BrandAdjectives string `json:"brand_adjectives"`
	Surface         string `json:"surface"`
}

// DesignRecord is the durable Design Language Record: the product profile plus
// the six binding decisions every subsequent UI task inherits — style
// direction, typography, palette, spacing/layout system, motion posture, and
// component idiom (req 9.1). Authored HOT (N divergent records generated, judged
// COLD) and then screened deterministically for banned AI-tells and by the
// anti-default lens before the human resolves it.
type DesignRecord struct {
	Profile        DesignProfile `json:"profile"`
	StyleDirection string        `json:"style_direction"`
	Typography     string        `json:"typography"`
	Palette        []string      `json:"palette"`
	Spacing        string        `json:"spacing"`
	Motion         string        `json:"motion"`
	ComponentIdiom string        `json:"component_idiom"`
	Rationale      string        `json:"rationale"`
}

// Validate rejects a record that could not bind real UI work: it needs a style
// direction, a typography choice, a palette, and a component idiom — the fields
// a worker's turn-in is screened against for drift.
func (r *DesignRecord) Validate() error {
	var missing []string
	if strings.TrimSpace(r.StyleDirection) == "" {
		missing = append(missing, "style_direction")
	}
	if strings.TrimSpace(r.Typography) == "" {
		missing = append(missing, "typography")
	}
	if len(r.Palette) == 0 {
		missing = append(missing, "palette")
	}
	if strings.TrimSpace(r.ComponentIdiom) == "" {
		missing = append(missing, "component_idiom")
	}
	if len(missing) > 0 {
		return fmt.Errorf("design record is missing binding fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Summary renders the DLR as the one-block constraint every UI task sheet
// carries so a fresh-context worker builds to the same design language (req 9.3).
func (r *DesignRecord) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Style direction: %s\n", strings.TrimSpace(r.StyleDirection))
	fmt.Fprintf(&b, "Typography: %s\n", strings.TrimSpace(r.Typography))
	fmt.Fprintf(&b, "Palette: %s\n", strings.Join(r.Palette, ", "))
	if s := strings.TrimSpace(r.Spacing); s != "" {
		fmt.Fprintf(&b, "Spacing / layout: %s\n", s)
	}
	if s := strings.TrimSpace(r.Motion); s != "" {
		fmt.Fprintf(&b, "Motion posture: %s\n", s)
	}
	fmt.Fprintf(&b, "Component idiom: %s", strings.TrimSpace(r.ComponentIdiom))
	return b.String()
}

// designSystem casts the model as the design-language author. The banned-tells
// list is stated up front so the generation itself resists the generic AI look;
// the design-quality doctrine is injected so the record can cite it.
func designSystem(doctrine string) string {
	var b strings.Builder
	b.WriteString(`You are Cody prime's design lead authoring a Design Language Record for a UI-bearing build. Choose a visual direction a senior product designer would for THIS product — the opposite of the generic AI-default template look.

Derive the product profile from the request, then decide: style direction, typography, palette (name concrete colors), spacing/layout system, motion posture, and component idiom. Every subsequent UI task inherits this record verbatim.

HARD BANS (a record that leans on any of these is rejected deterministically):
- untouched stock component themes (shadcn/Material/Bootstrap defaults passed off as finished)
- purple/indigo gradient hero sections
- glassmorphism blobs / gradient blobs
- emoji used as UI chrome
- default framework blue (Tailwind blue-500 #3b82f6, Bootstrap #007bff) as the accent

Output ONLY a JSON object, no prose, no code fences:
{
  "profile": {"product_kind": "...", "audience": "...", "brand_adjectives": "...", "surface": "..."},
  "style_direction": "a specific, opinionated direction (not 'clean minimal')",
  "typography": "concrete typeface pairing + why",
  "palette": ["concrete colors — names or hex"],
  "spacing": "the spacing/layout system",
  "motion": "the motion posture",
  "component_idiom": "how components look and behave",
  "rationale": "one or two sentences: why this direction for THIS product and not the default"
}

`)
	b.WriteString("DESIGN-QUALITY DOCTRINE (rules/web/design-quality.md — cite it):\n")
	b.WriteString(strings.TrimSpace(doctrine))
	return b.String()
}

// designCriteria is what the cold judge weighs candidate design records against.
const designCriteria = "The record whose direction best fits its own product profile (product kind, audience, brand, surface) as a senior designer would choose; reject a record that reads as the generic AI-default template look or whose direction would be identical regardless of the product."

// AuthorDesign authors a Design Language Record the decision way: generate
// `candidates` divergent records HOT, keep the ones that parse and validate,
// and pick the best-fitting one COLD (req 10.2). Returns the chosen record.
func AuthorDesign(ctx context.Context, hot, cold *llm.Client, brief, doctrine string, candidates int) (*DesignRecord, error) {
	if hot == nil {
		return nil, errors.New("decide: nil decision client")
	}
	if candidates < 1 {
		candidates = 1
	}
	system := designSystem(doctrine)
	cands, err := Generate(ctx, hot, system, brief, candidates)
	if err != nil {
		return nil, fmt.Errorf("decide: author design: %w", err)
	}
	var valid []*DesignRecord
	var judgeable []Candidate
	for _, c := range cands {
		rec, err := parseDesign(c.Text)
		if err != nil {
			continue
		}
		if err := rec.Validate(); err != nil {
			continue
		}
		judgeable = append(judgeable, Candidate{Index: len(valid), Text: c.Text})
		valid = append(valid, rec)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("decide: model produced no valid design record across %d candidate(s)", len(cands))
	}
	if len(valid) == 1 {
		return valid[0], nil
	}
	dec, err := Judge(ctx, cold, "UI-bearing build brief:\n"+brief, designCriteria, judgeable)
	if err != nil {
		return valid[0], nil // fail-safe to the first valid record
	}
	pick := dec.Pick
	if pick < 0 || pick >= len(valid) {
		pick = 0
	}
	return valid[pick], nil
}

// bannedTell is one deterministic AI-default screen: a human-readable name and
// the predicate that fires it over lowercased text.
type bannedTell struct {
	name  string
	fires func(low string) bool
}

func contains(low string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// bannedTells is the deterministic banned-defaults list (req 9.2): the AI-tells
// a Design Language Record may never lean on. Each is a pure text predicate so
// the screen is fully deterministic — the same record always screens the same.
var bannedTells = []bannedTell{
	{"purple/indigo gradient hero", func(low string) bool {
		return contains(low, "gradient") && contains(low, "purple", "indigo", "violet")
	}},
	{"gradient/glass blob", func(low string) bool {
		return contains(low, "blob") && contains(low, "gradient", "glass", "glassmorph")
	}},
	{"default framework blue accent", func(low string) bool {
		return contains(low, "#3b82f6", "#007bff", "#2563eb", "#1d4ed8") ||
			contains(low, "tailwind blue", "bootstrap blue", "default blue", "framework blue")
	}},
	{"untouched stock component theme", func(low string) bool {
		return contains(low, "shadcn default", "default shadcn", "stock theme", "default theme",
			"material default", "bootstrap default", "default tailwind", "out of the box", "out-of-the-box", "unmodified")
	}},
	{"emoji as UI chrome", func(low string) bool { return hasEmoji(low) }},
}

// BannedTellsInText returns the concrete banned AI-tells present in text, or
// nil. Shared by the DLR screen (over the record) and the acceptance gate (over
// changed UI file content) so the same doctrine screens the decision AND its
// downstream drift (req 9.2, 9.3).
func BannedTellsInText(text string) []string {
	low := strings.ToLower(text)
	var hits []string
	for _, t := range bannedTells {
		if t.fires(low) {
			hits = append(hits, t.name)
		}
	}
	return hits
}

// ScreenDesignBanned runs the deterministic banned-defaults screen over a
// record. It returns "" when the record passes, or a concrete rejection listing
// every banned tell it leans on.
func ScreenDesignBanned(rec *DesignRecord) string {
	if rec == nil {
		return ""
	}
	fields := []string{
		rec.StyleDirection, rec.Typography, strings.Join(rec.Palette, " "),
		rec.Spacing, rec.Motion, rec.ComponentIdiom, rec.Rationale,
	}
	hits := BannedTellsInText(strings.Join(fields, "\n"))
	if len(hits) == 0 {
		return ""
	}
	return "banned-defaults screen rejected the design record: leans on " + strings.Join(hits, "; ")
}

// antiDesignSystem casts the model as the anti-AI-default adjudicator: the gate
// lens whose job is to reject a design that is indistinguishable from the
// generic template look an AI reaches for by default (req 9.2).
const antiDesignSystem = `You are Cody prime's anti-AI-default design adjudicator. You are given a Design Language Record: a product profile and the chosen visual direction. Your ONE job is to reject a design that no one could tell apart from AI-default output.

Reject (accept=false) when: the direction is the generic template look (safe gray-on-white, one decorative accent, uniform cards, stock component themes) and does not turn on the product's specifics; or the direction would be identical regardless of the product. Accept (accept=true) only when the direction is defensibly specific to THIS product and reads as intentional senior design work.

Output ONLY a JSON object, no prose, no code fences:
{"accept": <true|false>, "reason": "one sentence: why this direction is or is not distinguishable from AI-default"}`

// ScreenDesignAntiDefault runs the anti-AI-default lens over a chosen record. It
// returns "" when the record passes, or a concrete rejection reason. A judge
// transport error fails OPEN (empty) — the human still resolves the record, so a
// hiccup never wedges the phase, but an explicit reject verdict is honored (the
// proven Cassandra posture).
func ScreenDesignAntiDefault(ctx context.Context, cold *llm.Client, rec *DesignRecord) string {
	if cold == nil || rec == nil {
		return ""
	}
	var u strings.Builder
	fmt.Fprintf(&u, "PRODUCT PROFILE:\nkind: %s\naudience: %s\nbrand: %s\nsurface: %s\n\n",
		rec.Profile.ProductKind, rec.Profile.Audience, rec.Profile.BrandAdjectives, rec.Profile.Surface)
	fmt.Fprintf(&u, "CHOSEN DESIGN LANGUAGE:\n%s\nWHY: %s\n", rec.Summary(), strings.TrimSpace(rec.Rationale))
	res, err := cold.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(antiDesignSystem),
		llm.UserMessage(u.String()),
	}})
	if err != nil {
		return "" // fail open
	}
	obj := firstJSONObject(res.Message.Content)
	if obj == "" {
		return ""
	}
	var v antiDefaultVerdict
	if json.Unmarshal([]byte(obj), &v) != nil {
		return ""
	}
	if v.Accept {
		return ""
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "the design is not distinguishable from AI-default"
	}
	return "anti-default lens rejected the design decision: " + reason
}

// parseDesign extracts the first balanced JSON object and unmarshals a record.
func parseDesign(raw string) (*DesignRecord, error) {
	obj := firstJSONObject(raw)
	if obj == "" {
		return nil, errors.New("decide: no JSON object in design output")
	}
	var rec DesignRecord
	if err := json.Unmarshal([]byte(obj), &rec); err != nil {
		return nil, fmt.Errorf("decide: parse design record: %w", err)
	}
	return &rec, nil
}

// hasEmoji reports whether s carries a pictographic emoji rune — the "emoji as
// UI chrome" tell. Covers the common emoji blocks (Misc Symbols, Dingbats,
// Supplemental Symbols, and the SMP pictograph planes) plus a couple of
// frequently-abused standalone marks.
func hasEmoji(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x1F000 && r <= 0x1FAFF: // SMP pictographs / emoji
			return true
		case r >= 0x2600 && r <= 0x27BF: // Misc symbols + dingbats
			return true
		case r == 0x2B50 || r == 0x2B55: // star, heavy circle
			return true
		case r >= 0xFE00 && r <= 0xFE0F: // variation selectors (emoji presentation)
			return true
		}
	}
	return false
}
