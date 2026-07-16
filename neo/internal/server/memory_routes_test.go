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

func TestMemoryRoutesReadNeoPagerMemories(t *testing.T) {
	cfg := neoconfig.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "timeline-route-test"
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })

	if _, err := pager.RememberFact(context.Background(), "The user is building the Matrix Timeline."); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	if _, err := pager.RememberPreference(context.Background(), "answers", "do", 0.9, "Keep them concise."); err != nil {
		t.Fatalf("RememberPreference: %v", err)
	}

	s := &Server{engine: &Engine{pager: pager}}
	handler := s.Handler()

	recentReq := httptest.NewRequest(http.MethodGet, "/memory/recent?limit=120", nil)
	recentRes := httptest.NewRecorder()
	handler.ServeHTTP(recentRes, recentReq)
	if recentRes.Code != http.StatusOK {
		t.Fatalf("GET /memory/recent status = %d, body = %s", recentRes.Code, recentRes.Body.String())
	}
	var recent struct {
		Items []neomemory.TimelineEntry `json:"items"`
	}
	if err := json.Unmarshal(recentRes.Body.Bytes(), &recent); err != nil {
		t.Fatalf("decode recent: %v", err)
	}
	if len(recent.Items) != 2 {
		t.Fatalf("recent items = %d, want 2: %s", len(recent.Items), recentRes.Body.String())
	}
	seen := map[string]bool{}
	for _, item := range recent.Items {
		seen[item.Type] = true
	}
	if !seen["Fact"] || !seen["Preference"] {
		t.Fatalf("recent types = %#v, want Fact and Preference", seen)
	}

	typesReq := httptest.NewRequest(http.MethodGet, "/memory/types", nil)
	typesRes := httptest.NewRecorder()
	handler.ServeHTTP(typesRes, typesReq)
	if typesRes.Code != http.StatusOK || !strings.Contains(typesRes.Body.String(), `"type":"Fact","count":1`) {
		t.Fatalf("GET /memory/types status = %d, body = %s", typesRes.Code, typesRes.Body.String())
	}

	searchReq := httptest.NewRequest(http.MethodPost, "/memory/search", strings.NewReader(`{"type":["Preference"],"limit":120,"form":"medium"}`))
	searchReq.Header.Set("Content-Type", "application/json")
	searchRes := httptest.NewRecorder()
	handler.ServeHTTP(searchRes, searchReq)
	if searchRes.Code != http.StatusOK {
		t.Fatalf("POST /memory/search status = %d, body = %s", searchRes.Code, searchRes.Body.String())
	}
	var searched struct {
		Items []neomemory.TimelineEntry `json:"items"`
	}
	if err := json.Unmarshal(searchRes.Body.Bytes(), &searched); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(searched.Items) != 1 || searched.Items[0].Type != "Preference" {
		t.Fatalf("search items = %#v, want one Preference", searched.Items)
	}
}
