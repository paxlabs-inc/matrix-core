// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/codegraph/selfmodel"
	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// TestCapabilitySurfaceDerivedAndPersisted proves epistemic-core req.2.1/2.2 on
// the real seams: a REAL self-model artifact on disk is loaded by a real pager,
// the server derives the capability surface from its OWN route table (the same
// table Handler registers), persists it into the durable structural self, and
// the engine resolves it — carrying the TRUE external surface (POST /chat +
// SSE /events; no OpenAI-compatible endpoint), never hand-written prompt prose.
func TestCapabilitySurfaceDerivedAndPersisted(t *testing.T) {
	ctx := context.Background()

	// A real self-model artifact on disk, as codegraph writes it.
	graph := t.TempDir()
	artifact := selfmodel.Artifact{
		Summary: "structural-summary-marker: conversation loop, window assembly, safety wall.",
		Merkle:  "merkle-test",
		Scope:   []string{"matrix/neo"},
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graph, "self-model.json"), append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-capability-test"
	cfg.SelfModelGraph = graph
	pager, err := memory.Open(cfg) // LoadStructuralSelf runs against the real artifact
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	e := NewEngine(EngineOptions{
		Config:     cfg,
		Tools:      &tools.Manager{},
		Pager:      pager,
		BackendURL: "http://127.0.0.1:1",
	})
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})

	srv, err := New(e, "http://127.0.0.1:1") // recordSurface persists at boot
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// Derived facts carry the true external surface, from the route table.
	facts := srv.SurfaceFacts()
	joined := strings.Join(facts.API, "\n")
	if !strings.Contains(joined, "POST /chat") {
		t.Fatalf("derived API surface must carry POST /chat; got:\n%s", joined)
	}
	if !strings.Contains(joined, "GET /events") || !strings.Contains(joined, "SSE") {
		t.Fatalf("derived API surface must carry the SSE GET /events stream; got:\n%s", joined)
	}
	if !strings.Contains(strings.Join(facts.IsNot, "\n"), "OpenAI-compatible") {
		t.Fatalf("is-not facts must refute the OpenAI-compatible endpoint; got:\n%s", strings.Join(facts.IsNot, "\n"))
	}

	// Persisted: the durable self-model now carries the surface facet WITHOUT
	// losing the artifact-derived structural summary.
	model, err := pager.SelfModel(ctx)
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	if model.Structural.Surface == nil {
		t.Fatal("structural self must carry the derived surface facet after boot")
	}
	if !strings.Contains(strings.Join(model.Structural.Surface.API, "\n"), "POST /chat") {
		t.Fatal("persisted surface facet lost the POST /chat fact")
	}
	if !strings.Contains(model.Structural.Summary, "structural-summary-marker") {
		t.Fatalf("surface write must preserve the artifact-derived summary; got %q", model.Structural.Summary)
	}

	// Boot-reload preservation: a fresh artifact reload (next process boot)
	// must not drop the surface facet.
	if _, err := pager.LoadStructuralSelf(ctx); err != nil {
		t.Fatalf("LoadStructuralSelf reload: %v", err)
	}
	model, err = pager.SelfModel(ctx)
	if err != nil {
		t.Fatalf("SelfModel after reload: %v", err)
	}
	if model.Structural.Surface == nil || len(model.Structural.Surface.IsNot) == 0 {
		t.Fatal("artifact reload dropped the surface facet")
	}

	// The engine resolves the render-ready material from the durable model.
	cs := e.capabilitySurface(ctx)
	if cs == nil {
		t.Fatal("capabilitySurface must resolve when a pager is wired")
	}
	if !strings.Contains(strings.Join(cs.IsNot, "\n"), "OpenAI-compatible") {
		t.Fatal("resolved capability surface lost the OpenAI-compatible refutation")
	}
	if !strings.Contains(cs.StructuralSummary, "structural-summary-marker") {
		t.Fatal("resolved capability surface lost the structural summary")
	}
}
