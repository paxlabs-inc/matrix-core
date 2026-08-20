// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"centra/core/cortex"
	"centra/core/cortex/store"
)

// newPersonalizationTestDaemon returns a daemonState wired to a real per-actor
// Pebble-backed cortex under a tempdir, with an actor identity. No fakes.
func newPersonalizationTestDaemon(t *testing.T) *daemonState {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cortex")
	s, err := store.Open(root, "test-actor", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cx := cortex.New(s)
	return &daemonState{
		infra: &infra{cortex: cx, store: s},
		actor: &actorIdentity{
			DID:      "did:matrix:test:abc",
			UserURI:  "matrix://user/did:matrix:test:abc",
			AgentURI: "matrix://agent/did:matrix:test:abc",
		},
	}
}

// callPersonalization drives the handler and returns status + raw body.
func callPersonalization(t *testing.T, d *daemonState, method, path string, body interface{}) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = http.NoBody
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	switch path {
	case "/personalization/export":
		d.handlePersonalizationExport(rec, req)
	default:
		d.handlePersonalization(rec, req)
	}
	return rec.Code, rec.Body.Bytes()
}

func sampleProfile() personalizationProfile {
	return personalizationProfile{
		Interests:     []string{"AI", "cycling", "AI"}, // dup dropped
		DayToDayGoals: []string{"stay on top of research"},
		Media: mediaTastes{
			Music: mediaTaste{Liked: []string{"jazz"}, Disliked: []string{"gabber"}},
			Films: mediaTaste{Liked: []string{"sci-fi"}},
		},
		Adventurousness: "Balanced", // normalized to lowercase
		BriefPreferences: briefPreferences{
			Length:   "Standard",
			Sections: []string{"news", "music", "news"}, // dup dropped
		},
	}
}

// TestPersonalization_EmptyGet: no record yields an empty schema-versioned profile.
func TestPersonalization_EmptyGet(t *testing.T) {
	d := newPersonalizationTestDaemon(t)
	code, body := callPersonalization(t, d, http.MethodGet, "/personalization", nil)
	if code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", code, body)
	}
	var resp personalizationResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.URI != "" {
		t.Errorf("empty GET uri = %q, want empty", resp.URI)
	}
	if resp.Profile.SchemaVersion != personalizationSchemaVersion {
		t.Errorf("schema_version = %d, want %d", resp.Profile.SchemaVersion, personalizationSchemaVersion)
	}
	if len(resp.Profile.Interests) != 0 {
		t.Errorf("empty profile has interests: %v", resp.Profile.Interests)
	}
}

// TestPersonalization_RoundTrip: PUT then GET returns the normalized profile.
func TestPersonalization_RoundTrip(t *testing.T) {
	d := newPersonalizationTestDaemon(t)
	code, body := callPersonalization(t, d, http.MethodPut, "/personalization", sampleProfile())
	if code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body = %s", code, body)
	}
	var put personalizationResponse
	if err := json.Unmarshal(body, &put); err != nil {
		t.Fatalf("unmarshal put: %v", err)
	}
	if put.URI == "" {
		t.Errorf("PUT returned empty uri")
	}
	// Normalization: dedup + lowercase.
	if got := put.Profile.Interests; len(got) != 2 {
		t.Errorf("interests = %v, want 2 (dedup)", got)
	}
	if put.Profile.Adventurousness != "balanced" {
		t.Errorf("adventurousness = %q, want balanced", put.Profile.Adventurousness)
	}
	if put.Profile.BriefPreferences.Length != "standard" {
		t.Errorf("length = %q, want standard", put.Profile.BriefPreferences.Length)
	}
	if len(put.Profile.BriefPreferences.Sections) != 2 {
		t.Errorf("sections = %v, want 2 (dedup)", put.Profile.BriefPreferences.Sections)
	}

	code, body = callPersonalization(t, d, http.MethodGet, "/personalization", nil)
	if code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", code, body)
	}
	var got personalizationResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	if got.Profile.Media.Music.Liked[0] != "jazz" {
		t.Errorf("music liked = %v", got.Profile.Media.Music.Liked)
	}
	if got.Profile.Media.Music.Disliked[0] != "gabber" {
		t.Errorf("music disliked = %v", got.Profile.Media.Music.Disliked)
	}
	if got.URI == "" {
		t.Errorf("GET returned empty uri after PUT")
	}
}

// TestPersonalization_VersionedEdit: a second PUT updates in place (single record).
func TestPersonalization_VersionedEdit(t *testing.T) {
	d := newPersonalizationTestDaemon(t)
	if code, body := callPersonalization(t, d, http.MethodPut, "/personalization", sampleProfile()); code != http.StatusCreated {
		t.Fatalf("first PUT status = %d, body = %s", code, body)
	}

	edited := sampleProfile()
	edited.Interests = []string{"gardening"}
	code, body := callPersonalization(t, d, http.MethodPut, "/personalization", edited)
	if code != http.StatusOK {
		t.Fatalf("second PUT status = %d, body = %s", code, body)
	}
	var upd personalizationResponse
	if err := json.Unmarshal(body, &upd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(upd.Profile.Interests) != 1 || upd.Profile.Interests[0] != "gardening" {
		t.Errorf("edited interests = %v, want [gardening]", upd.Profile.Interests)
	}

	// Exactly one authoritative record survives (single-record invariant).
	mem, err := d.findPersonalizationMemory()
	if err != nil || mem == nil {
		t.Fatalf("findPersonalizationMemory: mem=%v err=%v", mem, err)
	}
	if mem.Head.CurrentVersion < 2 {
		t.Errorf("version = %d, want >= 2 after edit", mem.Head.CurrentVersion)
	}
}

// TestPersonalization_Delete: DELETE removes the record from the authoritative surface.
func TestPersonalization_Delete(t *testing.T) {
	d := newPersonalizationTestDaemon(t)
	if code, body := callPersonalization(t, d, http.MethodPut, "/personalization", sampleProfile()); code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body = %s", code, body)
	}
	code, body := callPersonalization(t, d, http.MethodDelete, "/personalization", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", code, body)
	}
	var del map[string]bool
	if err := json.Unmarshal(body, &del); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !del["deleted"] {
		t.Errorf("delete = %v, want true", del)
	}
	// Gone from the authoritative surface.
	mem, err := d.findPersonalizationMemory()
	if err != nil {
		t.Fatalf("findPersonalizationMemory: %v", err)
	}
	if mem != nil {
		t.Errorf("record still resolvable after delete")
	}
	code, body = callPersonalization(t, d, http.MethodGet, "/personalization", nil)
	var resp personalizationResponse
	_ = json.Unmarshal(body, &resp)
	if code != http.StatusOK || resp.URI != "" {
		t.Errorf("GET after delete: code=%d uri=%q, want 200 + empty", code, resp.URI)
	}
}

// TestPersonalization_ExportShape: export carries lineage + the profile.
func TestPersonalization_ExportShape(t *testing.T) {
	d := newPersonalizationTestDaemon(t)

	// Before any record: exists=false.
	code, body := callPersonalization(t, d, http.MethodGet, "/personalization/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", code, body)
	}
	var pre personalizationExport
	if err := json.Unmarshal(body, &pre); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pre.Exists {
		t.Errorf("export exists=true before any record")
	}
	if pre.Tag != string(personalizationTag) {
		t.Errorf("export tag = %q", pre.Tag)
	}

	if code, body := callPersonalization(t, d, http.MethodPut, "/personalization", sampleProfile()); code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body = %s", code, body)
	}
	code, body = callPersonalization(t, d, http.MethodGet, "/personalization/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", code, body)
	}
	var exp personalizationExport
	if err := json.Unmarshal(body, &exp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !exp.Exists {
		t.Errorf("export exists=false after PUT")
	}
	if exp.URI == "" || exp.Version == 0 || exp.UpdatedAt == "" {
		t.Errorf("export lineage incomplete: uri=%q ver=%d updated=%q", exp.URI, exp.Version, exp.UpdatedAt)
	}
	if exp.CreatedBy != d.actor.UserURI {
		t.Errorf("export created_by = %q, want %q", exp.CreatedBy, d.actor.UserURI)
	}
	if len(exp.Profile.Interests) != 2 {
		t.Errorf("export profile interests = %v", exp.Profile.Interests)
	}
}

// TestPersonalization_RejectsBadVocab: closed vocabularies are enforced with 400.
func TestPersonalization_RejectsBadVocab(t *testing.T) {
	d := newPersonalizationTestDaemon(t)
	cases := []personalizationProfile{
		{Adventurousness: "wild"},
		{BriefPreferences: briefPreferences{Length: "epic"}},
		{BriefPreferences: briefPreferences{Sections: []string{"weather"}}},
	}
	for i, c := range cases {
		code, body := callPersonalization(t, d, http.MethodPut, "/personalization", c)
		if code != http.StatusBadRequest {
			t.Errorf("case %d: status = %d body=%s, want 400", i, code, body)
		}
	}
	// Nothing was written on rejection.
	if mem, _ := d.findPersonalizationMemory(); mem != nil {
		t.Errorf("a rejected PUT still wrote a record")
	}
}

// TestPersonalization_MethodNotAllowed guards the verb set.
func TestPersonalization_MethodNotAllowed(t *testing.T) {
	d := newPersonalizationTestDaemon(t)
	code, _ := callPersonalization(t, d, http.MethodPost, "/personalization", sampleProfile())
	if code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", code)
	}
}
