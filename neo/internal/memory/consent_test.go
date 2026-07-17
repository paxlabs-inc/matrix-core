// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"

	"matrix/cortex"
)

// A fresh actor has NO durable-memory consent: it reads explicitly OFF and not
// yet explicit (PRIV-01 req 8.1 — default off for new users).
func TestMemoryConsentDefaultOff(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	state, err := p.MemoryConsent(ctx)
	if err != nil {
		t.Fatalf("MemoryConsent: %v", err)
	}
	if state.Enabled {
		t.Error("default consent must be disabled")
	}
	if state.Explicit {
		t.Error("default consent must not be marked explicit")
	}
	if p.MemoryConsentEnabled() {
		t.Error("MemoryConsentEnabled must be false by default")
	}
}

// Opt-in and opt-out are persisted, auditable (updated_at/by), and VERSIONED on
// the SAME single record rather than accumulating new records (req 8.1/8.2).
func TestMemoryConsentOptInOutVersionedAndAuditable(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	on, err := p.SetMemoryConsent(ctx, true, "test user")
	if err != nil {
		t.Fatalf("SetMemoryConsent(true): %v", err)
	}
	if !on.Enabled || !on.Explicit {
		t.Fatalf("opt-in state = %+v, want enabled+explicit", on)
	}
	if on.UpdatedAt == "" || on.UpdatedBy != "test user" {
		t.Fatalf("opt-in must be audited (updated_at/by): %+v", on)
	}
	if !p.MemoryConsentEnabled() {
		t.Fatal("MemoryConsentEnabled must be true after opt-in")
	}

	off, err := p.SetMemoryConsent(ctx, false, "test user")
	if err != nil {
		t.Fatalf("SetMemoryConsent(false): %v", err)
	}
	if off.Enabled {
		t.Fatal("opt-out state must be disabled")
	}
	if !off.Explicit {
		t.Fatal("opt-out is still an explicit, recorded choice")
	}
	if p.MemoryConsentEnabled() {
		t.Fatal("MemoryConsentEnabled must be false after opt-out")
	}

	// One write + one update = a single record at version 2 (no duplicate
	// records — auditable version lineage).
	mem, err := p.findMemoryConsent()
	if err != nil {
		t.Fatalf("findMemoryConsent: %v", err)
	}
	if mem == nil {
		t.Fatal("consent record missing after opt-out")
	}
	if mem.Head.CurrentVersion != 2 {
		t.Fatalf("consent record version = %d, want 2 (1 write + 1 update)", mem.Head.CurrentVersion)
	}
}

// The internal consent carrier is a tagged Preference; it must NEVER surface in
// any user-facing or model-facing retrieval lane, or leak its rationale JSON
// into learned guidance (PRIV-01 immediate item 1).
func TestConsentCarrierHiddenFromEverySurface(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.SetMemoryConsent(ctx, true, "test user"); err != nil {
		t.Fatalf("SetMemoryConsent: %v", err)
	}
	// A real user-facing preference and fact to prove the surfaces still work.
	if _, err := p.RememberPreference(ctx, "terse, information-dense replies", "prefer", 0.9, "user dislikes filler"); err != nil {
		t.Fatalf("RememberPreference: %v", err)
	}
	if _, err := p.RememberFact(ctx, "the dev box repo is at /root/matrix"); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	drain(t, p)

	consent, err := p.findMemoryConsent()
	if err != nil || consent == nil {
		t.Fatalf("findMemoryConsent: %v (nil=%v)", err, consent == nil)
	}
	consentURI := string(cortex.BuildURI(consent.Head.Type, consent.Head.ID, consent.Head.CurrentVersion))

	// Timeline: no consent entry, no consent tag, total excludes it.
	items, total, err := p.Timeline(TimelineQuery{Limit: 200})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	for _, it := range items {
		if it.URI == consentURI {
			t.Error("Timeline surfaced the consent carrier record")
		}
		for _, tag := range it.Tags {
			if tag == string(memoryConsentTag) {
				t.Error("Timeline surfaced a consent-tagged record")
			}
		}
	}
	if total != len(items) {
		t.Errorf("Timeline total %d != returned %d (consent not excluded from total)", total, len(items))
	}

	// TimelineTypes: the Preference bucket excludes the consent carrier — it
	// should count exactly the one real preference.
	counts, err := p.TimelineTypes()
	if err != nil {
		t.Fatalf("TimelineTypes: %v", err)
	}
	for _, c := range counts {
		if c.Type == "Preference" && c.Count != 1 {
			t.Errorf("Preference count = %d, want 1 (consent carrier excluded)", c.Count)
		}
	}

	// LearnedGuidance: never carries the consent rationale JSON.
	for _, line := range p.LearnedGuidance(ctx) {
		if strings.Contains(line, memoryConsentSidecarPrefix) || strings.Contains(line, "durable personalization") {
			t.Errorf("LearnedGuidance leaked the consent carrier: %q", line)
		}
	}

	// Retrieve: no snippet is the consent record or its rationale JSON.
	snips, err := p.Retrieve(ctx, "durable personalization consent")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, s := range snips {
		if s.URI == consentURI || strings.Contains(s.Text, memoryConsentSidecarPrefix) {
			t.Errorf("Retrieve surfaced the consent carrier: %+v", s)
		}
	}

	// RecallHits: same.
	hits, err := p.RecallHits(ctx, "durable personalization consent", nil, 20, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	for _, h := range hits {
		if h.URI == consentURI || strings.Contains(h.Text, memoryConsentSidecarPrefix) {
			t.Errorf("RecallHits surfaced the consent carrier: %+v", h)
		}
	}
}
