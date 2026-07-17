// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	neoconfig "matrix/neo/internal/config"
	neomemory "matrix/neo/internal/memory"
)

func newPrivacyServer(t *testing.T) (*Server, http.Handler, *neomemory.Pager) {
	t.Helper()
	cfg := neoconfig.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "privacy-route-test"
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	s := &Server{engine: &Engine{pager: pager}}
	return s, s.Handler(), pager
}

// GET /memory/consent defaults off and carries the plain-language notice; PUT
// enables it and the change is durable and reflected on the next GET.
func TestMemoryConsentRoute(t *testing.T) {
	_, handler, _ := newPrivacyServer(t)

	getRes := httptest.NewRecorder()
	handler.ServeHTTP(getRes, httptest.NewRequest(http.MethodGet, "/memory/consent", nil))
	if getRes.Code != http.StatusOK {
		t.Fatalf("GET /memory/consent status = %d, body = %s", getRes.Code, getRes.Body.String())
	}
	var got struct {
		Consent neomemory.MemoryConsentState `json:"consent"`
		Notice  string                       `json:"notice"`
	}
	if err := json.Unmarshal(getRes.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode consent: %v", err)
	}
	if got.Consent.Enabled {
		t.Fatal("default consent must be off")
	}
	if strings.TrimSpace(got.Notice) == "" {
		t.Fatal("consent GET must include a plain-language notice before opt-in")
	}

	putRes := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/memory/consent", strings.NewReader(`{"enabled":true}`))
	putReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(putRes, putReq)
	if putRes.Code != http.StatusOK {
		t.Fatalf("PUT /memory/consent status = %d, body = %s", putRes.Code, putRes.Body.String())
	}

	get2 := httptest.NewRecorder()
	handler.ServeHTTP(get2, httptest.NewRequest(http.MethodGet, "/memory/consent", nil))
	if !strings.Contains(get2.Body.String(), `"enabled":true`) {
		t.Fatalf("consent not enabled after PUT: %s", get2.Body.String())
	}
}

// GET /memory/export returns current user-facing memory and never the internal
// consent carrier.
func TestMemoryExportRoute(t *testing.T) {
	_, handler, pager := newPrivacyServer(t)
	ctx := context.Background()
	if _, err := pager.SetMemoryConsent(ctx, true, "test user"); err != nil {
		t.Fatalf("SetMemoryConsent: %v", err)
	}
	if _, err := pager.RememberFact(ctx, "The user is building the Matrix Timeline."); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/memory/export", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("GET /memory/export status = %d, body = %s", res.Code, res.Body.String())
	}
	var export neomemory.MemoryExport
	if err := json.Unmarshal(res.Body.Bytes(), &export); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(export.Items) != 1 || export.Items[0].Type != "Fact" {
		t.Fatalf("export items = %#v, want one Fact", export.Items)
	}
	if strings.Contains(res.Body.String(), "durable personalization") {
		t.Fatalf("export leaked the consent carrier: %s", res.Body.String())
	}
}

// DELETE /memory/delete-all fails closed with an explicit failed-dependency
// response naming ORACLE task 4.1 (no partial tombstone-all).
func TestMemoryDeleteAllFailsClosed(t *testing.T) {
	_, handler, _ := newPrivacyServer(t)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodDelete, "/memory/delete-all", nil))
	if res.Code != http.StatusFailedDependency {
		t.Fatalf("DELETE /memory/delete-all status = %d, want 424; body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "ORACLE task 4.1") {
		t.Fatalf("delete-all response must name the ORACLE task 4.1 prerequisite: %s", res.Body.String())
	}
}

// PUT /personalization saves the single profile on Neo's own actor, enables
// consent, and GET reads it back; DELETE removes it from every surface.
func TestPersonalizationRouteRoundTripAndDelete(t *testing.T) {
	_, handler, pager := newPrivacyServer(t)

	putRes := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/personalization", strings.NewReader(`{"interests":["quantum computing"],"adventurousness":"balanced"}`))
	putReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(putRes, putReq)
	if putRes.Code != http.StatusOK {
		t.Fatalf("PUT /personalization status = %d, body = %s", putRes.Code, putRes.Body.String())
	}
	if !strings.Contains(putRes.Body.String(), "quantum computing") {
		t.Fatalf("PUT did not return the saved profile: %s", putRes.Body.String())
	}
	// Saving durable personal data is an affirmative opt-in.
	if !pager.MemoryConsentEnabled() {
		t.Fatal("saving a personalization profile must enable memory consent")
	}

	getRes := httptest.NewRecorder()
	handler.ServeHTTP(getRes, httptest.NewRequest(http.MethodGet, "/personalization", nil))
	if getRes.Code != http.StatusOK || !strings.Contains(getRes.Body.String(), "quantum computing") {
		t.Fatalf("GET /personalization = %d, body = %s", getRes.Code, getRes.Body.String())
	}

	// The profile is owned by Neo's actor — it is reachable through this route.
	prof, _, ok, err := pager.PersonalizationProfile(context.Background())
	if err != nil || !ok || len(prof.Interests) != 1 {
		t.Fatalf("profile not owned/readable via Neo pager: ok=%v err=%v prof=%+v", ok, err, prof)
	}

	expRes := httptest.NewRecorder()
	handler.ServeHTTP(expRes, httptest.NewRequest(http.MethodGet, "/personalization/export", nil))
	if expRes.Code != http.StatusOK || !strings.Contains(expRes.Body.String(), `"exists":true`) {
		t.Fatalf("GET /personalization/export = %d, body = %s", expRes.Code, expRes.Body.String())
	}

	delRes := httptest.NewRecorder()
	handler.ServeHTTP(delRes, httptest.NewRequest(http.MethodDelete, "/personalization", nil))
	if delRes.Code != http.StatusOK || !strings.Contains(delRes.Body.String(), `"deleted":true`) {
		t.Fatalf("DELETE /personalization = %d, body = %s", delRes.Code, delRes.Body.String())
	}

	get2 := httptest.NewRecorder()
	handler.ServeHTTP(get2, httptest.NewRequest(http.MethodGet, "/personalization", nil))
	if strings.Contains(get2.Body.String(), "quantum computing") {
		t.Fatalf("deleted profile still present: %s", get2.Body.String())
	}
}
