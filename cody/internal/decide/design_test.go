// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package decide

import (
	"strings"
	"testing"
)

// TestDesignRecordValidate proves a record missing a binding field cannot bind
// UI work — the fields a worker turn-in is later screened against.
func TestDesignRecordValidate(t *testing.T) {
	full := &DesignRecord{
		StyleDirection: "editorial swiss",
		Typography:     "GT Sectra + Söhne",
		Palette:        []string{"ink #14110f", "bone #f4f1ea"},
		ComponentIdiom: "flat cards with a single hairline rule",
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("full record rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(r *DesignRecord)
	}{
		{"no style", func(r *DesignRecord) { r.StyleDirection = "" }},
		{"no typography", func(r *DesignRecord) { r.Typography = "" }},
		{"no palette", func(r *DesignRecord) { r.Palette = nil }},
		{"no idiom", func(r *DesignRecord) { r.ComponentIdiom = "" }},
	} {
		rec := *full
		tc.mut(&rec)
		if err := rec.Validate(); err == nil {
			t.Fatalf("%s: expected rejection, got nil", tc.name)
		}
	}
}

// TestScreenDesignBannedDeterministic proves req 9.2's deterministic
// banned-defaults screen: a record leaning on any AI-tell is rejected, and the
// same record always screens the same (no model in the loop).
func TestScreenDesignBannedDeterministic(t *testing.T) {
	clean := &DesignRecord{
		StyleDirection: "dark luxury with disciplined contrast",
		Typography:     "Söhne + Tiempos pairing",
		Palette:        []string{"obsidian #0b0b0d", "brass #b8975a"},
		Spacing:        "8pt rhythm, generous editorial gutters",
		Motion:         "restrained ease-out on reveal",
		ComponentIdiom: "surfaces separated by tone, no border strokes",
		Rationale:      "a premium consumer product wants gravity, not template polish",
	}
	if v := ScreenDesignBanned(clean); v != "" {
		t.Fatalf("clean record rejected: %s", v)
	}

	banned := []struct {
		name string
		rec  *DesignRecord
	}{
		{"purple gradient hero", &DesignRecord{
			StyleDirection: "hero with a purple gradient background", Typography: "Inter",
			Palette: []string{"violet"}, ComponentIdiom: "cards"}},
		{"default framework blue", &DesignRecord{
			StyleDirection: "minimal", Typography: "Inter",
			Palette: []string{"#3b82f6", "white"}, ComponentIdiom: "cards"}},
		{"stock theme", &DesignRecord{
			StyleDirection: "shadcn default look", Typography: "Inter",
			Palette: []string{"zinc"}, ComponentIdiom: "cards"}},
		{"gradient blob", &DesignRecord{
			StyleDirection: "floating gradient blob accents", Typography: "Inter",
			Palette: []string{"teal"}, ComponentIdiom: "cards"}},
		{"emoji chrome", &DesignRecord{
			StyleDirection: "playful ⭐ sparkle nav", Typography: "Inter",
			Palette: []string{"pink"}, ComponentIdiom: "cards"}},
	}
	for _, tc := range banned {
		v := ScreenDesignBanned(tc.rec)
		if v == "" {
			t.Fatalf("%s: banned record passed the screen", tc.name)
		}
		// Determinism: a second run yields the identical verdict.
		if v2 := ScreenDesignBanned(tc.rec); v2 != v {
			t.Fatalf("%s: non-deterministic verdict %q != %q", tc.name, v2, v)
		}
	}
}

// TestBannedTellsInText proves the shared screen the acceptance gate reuses on
// changed UI files catches the same tells at implementation time (req 9.3).
func TestBannedTellsInText(t *testing.T) {
	css := ".hero{background:linear-gradient(90deg,#7c3aed,indigo);}" // purple gradient
	hits := BannedTellsInText(css)
	if len(hits) == 0 {
		t.Fatalf("purple gradient CSS produced no tells")
	}
	if !strings.Contains(strings.Join(hits, " "), "purple/indigo") {
		t.Fatalf("expected a purple/indigo tell, got %v", hits)
	}
	if hits := BannedTellsInText(".btn{color:#111;background:#b8975a}"); len(hits) != 0 {
		t.Fatalf("clean CSS produced tells: %v", hits)
	}
}
