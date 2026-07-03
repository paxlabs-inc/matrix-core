// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStackDoctrineReadsRulesDir proves the SDR cites the authored
// rules/stack-selection decision tables when RulesDir is mounted (req 16.1) and
// falls open to the compact default when it is not — the wiring task 4.2 owed.
func TestStackDoctrineReadsRulesDir(t *testing.T) {
	rulesDir := t.TempDir()
	sel := filepath.Join(rulesDir, "stack-selection")
	if err := os.MkdirAll(sel, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two real markdown files; concatMarkdown reads them in filename order.
	const frontMarker = "AUTHORED-FRONTEND-DOCTRINE-MARKER"
	const backMarker = "AUTHORED-BACKEND-DOCTRINE-MARKER"
	if err := os.WriteFile(filepath.Join(sel, "frontend.md"), []byte("# Frontend\n"+frontMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sel, "backend.md"), []byte("# Backend\n"+backMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-markdown file must be ignored.
	if err := os.WriteFile(filepath.Join(sel, "INDEX.json"), []byte(`{"ignored":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{opts: EngineOptions{RulesDir: rulesDir}}
	got := e.stackDoctrine()
	if !strings.Contains(got, frontMarker) || !strings.Contains(got, backMarker) {
		t.Fatalf("stackDoctrine did not read the authored tables: %q", got)
	}
	if strings.Contains(got, "ignored") {
		t.Fatalf("stackDoctrine picked up a non-markdown file: %q", got)
	}
	// backend.md sorts before frontend.md, so backend content leads.
	if strings.Index(got, backMarker) > strings.Index(got, frontMarker) {
		t.Fatalf("stackDoctrine did not concatenate in filename order: %q", got)
	}

	// No RulesDir → the compact default floor.
	bare := &Engine{opts: EngineOptions{}}
	if bare.stackDoctrine() != defaultStackDoctrine {
		t.Fatalf("empty RulesDir should fall open to defaultStackDoctrine")
	}
	// RulesDir set but no stack-selection dir → default floor (fail-open).
	empty := &Engine{opts: EngineOptions{RulesDir: t.TempDir()}}
	if empty.stackDoctrine() != defaultStackDoctrine {
		t.Fatalf("missing stack-selection dir should fall open to defaultStackDoctrine")
	}
}

// TestDesignDoctrineReadsRulesDir proves the DLR cites the authored
// rules/web/design-quality.md when mounted and falls open otherwise.
func TestDesignDoctrineReadsRulesDir(t *testing.T) {
	rulesDir := t.TempDir()
	web := filepath.Join(rulesDir, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "AUTHORED-DESIGN-QUALITY-MARKER"
	if err := os.WriteFile(filepath.Join(web, "design-quality.md"), []byte("# Design quality\n"+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{opts: EngineOptions{RulesDir: rulesDir}}
	if got := e.designDoctrine(); !strings.Contains(got, marker) {
		t.Fatalf("designDoctrine did not read the authored file: %q", got)
	}

	bare := &Engine{opts: EngineOptions{}}
	if bare.designDoctrine() != defaultDesignDoctrine {
		t.Fatalf("empty RulesDir should fall open to defaultDesignDoctrine")
	}
	// RulesDir set but file absent → default floor.
	empty := &Engine{opts: EngineOptions{RulesDir: t.TempDir()}}
	if empty.designDoctrine() != defaultDesignDoctrine {
		t.Fatalf("missing design-quality.md should fall open to defaultDesignDoctrine")
	}
}
