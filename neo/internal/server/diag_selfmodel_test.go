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

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// TestDiagSelfModelReturnsRealResidentSummaryAndFailurePatterns proves req.13.1
// + 13.3: the read-only inspection surface returns the REAL resident
// self-summary and the REAL active failure-pattern memories for a seeded agent —
// resolved through the shared cortex self-model, no fakes.
func TestDiagSelfModelReturnsRealResidentSummaryAndFailurePatterns(t *testing.T) {
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-diag-selfmodel"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()
	ctx := context.Background()

	const summary = "Neo assembles system, transcript, then a trailing memory tail; coding and execution are faculties; core_execute is the value-transfer wall."
	const pattern = "I loop when a trailing memory tail is treated as the live request"
	if _, err := pager.WriteStructuralSelf(ctx, memory.StructuralSelf{Summary: summary, Scope: []string{"neo", "cortex"}, ContextLimit: 131072}); err != nil {
		t.Fatalf("WriteStructuralSelf: %v", err)
	}
	if _, err := pager.WriteFailurePattern(ctx, pattern, []string{"matrix://cortex/Event/death#1"}); err != nil {
		t.Fatalf("WriteFailurePattern: %v", err)
	}

	s := &Server{engine: &Engine{pager: pager}}

	get := func() selfModelDiag {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleDiagSelfModel(rec, httptest.NewRequest(http.MethodGet, "/diag/self-model", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /diag/self-model = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var got selfModelDiag
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		return got
	}

	got := get()
	if got.ResidentSummary != summary {
		t.Errorf("resident summary = %q, want the real seeded summary", got.ResidentSummary)
	}
	if got.ContextLimit != 131072 {
		t.Errorf("context limit = %d, want the real seeded 131072", got.ContextLimit)
	}
	foundPattern := false
	for _, fp := range got.FailurePatterns {
		if strings.Contains(fp.Statement, "trailing memory tail is treated as the live request") {
			foundPattern = true
		}
	}
	if !foundPattern {
		t.Errorf("inspection surface did not return the real failure pattern; got %+v", got.FailurePatterns)
	}

	// Side-effect free (req.13.2): a second inspection returns the identical
	// model — reading it never mutates it.
	again := get()
	if again.ResidentSummary != got.ResidentSummary || len(again.FailurePatterns) != len(got.FailurePatterns) {
		t.Error("the inspection surface mutated the self-model between reads; it must be side-effect free")
	}
}

// TestDiagSelfModelIsReadOnlyMethod proves the surface is a pure read: a
// non-GET is rejected, and a missing pager degrades cleanly rather than panicking.
func TestDiagSelfModelIsReadOnlyMethod(t *testing.T) {
	s := &Server{engine: &Engine{}}
	rec := httptest.NewRecorder()
	s.handleDiagSelfModel(rec, httptest.NewRequest(http.MethodPost, "/diag/self-model", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST must be rejected; got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleDiagSelfModel(rec, httptest.NewRequest(http.MethodGet, "/diag/self-model", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a nil pager must degrade to 503, not panic; got %d", rec.Code)
	}
}
