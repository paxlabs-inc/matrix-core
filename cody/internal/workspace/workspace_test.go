// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedGoRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/demo\n\ngo 1.21\n")
	writeFile(t, root, "main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, root, "cmd/tool/main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, root, "internal/util/util.go", "package util\n")
	writeFile(t, root, "Makefile", "build:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n\n.PHONY: build test\n")
	writeFile(t, root, "README.md", "# demo\n")
	writeFile(t, root, ".git/config", "[core]\n")
	writeFile(t, root, "vendor/dep/dep.go", "package dep\n")
	return root
}

func seedNodeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "package.json", `{
  "name": "demo",
  "main": "src/index.ts",
  "scripts": {"build": "next build", "test": "vitest run", "lint": "eslint ."},
  "dependencies": {"next": "15.0.0", "react": "19.0.0"},
  "devDependencies": {"vitest": "2.0.0"}
}`)
	writeFile(t, root, "src/index.ts", "export {}\n")
	writeFile(t, root, "src/app/page.tsx", "export default function Page() { return null }\n")
	writeFile(t, root, "node_modules/react/index.js", "module.exports = {}\n")
	return root
}

func TestScanGoRepo(t *testing.T) {
	root := seedGoRepo(t)
	m, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Languages["go"] != 3 {
		t.Fatalf("go file count = %d, want 3 (vendor/.git excluded)", m.Languages["go"])
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, ".git") || strings.HasPrefix(f.Path, "vendor") {
			t.Fatalf("ignored dir leaked into index: %s", f.Path)
		}
	}
	if langs := m.PrimaryLanguages(); len(langs) == 0 || langs[0] != "go" {
		t.Fatalf("PrimaryLanguages() = %v, want go first", langs)
	}
	wantTargets := map[string]bool{"make:build": true, "make:test": true, "go:build": true, "go:test": true}
	got := map[string]bool{}
	for _, bt := range m.BuildTargets {
		got[bt] = true
	}
	for w := range wantTargets {
		if !got[w] {
			t.Fatalf("BuildTargets missing %s: %v", w, m.BuildTargets)
		}
	}
	if got["make:.PHONY"] {
		t.Fatalf(".PHONY leaked into targets: %v", m.BuildTargets)
	}
	entries := strings.Join(m.EntryPoints, " ")
	if !strings.Contains(entries, "main.go") || !strings.Contains(entries, "cmd/tool/main.go") {
		t.Fatalf("EntryPoints = %v", m.EntryPoints)
	}
	hasFw := false
	for _, fw := range m.Frameworks {
		if fw == "go-module" {
			hasFw = true
		}
	}
	if !hasFw {
		t.Fatalf("Frameworks = %v, want go-module", m.Frameworks)
	}
}

func TestScanNodeRepo(t *testing.T) {
	root := seedNodeRepo(t)
	m, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	fw := strings.Join(m.Frameworks, " ")
	for _, want := range []string{"next", "react", "vitest"} {
		if !strings.Contains(fw, want) {
			t.Fatalf("Frameworks = %v, want %s", m.Frameworks, want)
		}
	}
	targets := strings.Join(m.BuildTargets, " ")
	for _, want := range []string{"npm:build", "npm:test", "npm:lint"} {
		if !strings.Contains(targets, want) {
			t.Fatalf("BuildTargets = %v, want %s", m.BuildTargets, want)
		}
	}
	eps := strings.Join(m.EntryPoints, " ")
	if !strings.Contains(eps, "src/index.ts") || !strings.Contains(eps, "src/app/page.tsx") {
		t.Fatalf("EntryPoints = %v", m.EntryPoints)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "node_modules") {
			t.Fatalf("node_modules leaked into index: %s", f.Path)
		}
	}
}

func TestPersistAcrossSessions(t *testing.T) {
	root := seedGoRepo(t)
	m1, err := Refresh(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, StateDir, indexFile)); err != nil {
		t.Fatalf("index not persisted: %v", err)
	}
	// A fresh "session" loads the persisted index without rescanning.
	m2, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if m2.FileCount != m1.FileCount || m2.Languages["go"] != m1.Languages["go"] {
		t.Fatalf("persisted index mismatch: %+v vs %+v", m2.Languages, m1.Languages)
	}
	if len(m2.BuildTargets) != len(m1.BuildTargets) {
		t.Fatalf("build targets lost across sessions: %v vs %v", m2.BuildTargets, m1.BuildTargets)
	}
	// LoadOrScan must take the load path (same GeneratedAt).
	m3, err := LoadOrScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m3.GeneratedAt.Equal(m1.GeneratedAt) {
		t.Fatal("LoadOrScan rescanned despite a persisted index")
	}
}

func TestLoadOrScanFreshWorkspace(t *testing.T) {
	root := seedNodeRepo(t)
	m, err := LoadOrScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.FileCount == 0 {
		t.Fatal("fresh LoadOrScan produced an empty model")
	}
	if _, err := os.Stat(filepath.Join(root, StateDir, indexFile)); err != nil {
		t.Fatalf("fresh LoadOrScan did not persist: %v", err)
	}
}

func TestOwnStateDirExcluded(t *testing.T) {
	root := seedGoRepo(t)
	if _, err := Refresh(root); err != nil {
		t.Fatal(err)
	}
	m, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, StateDir) {
			t.Fatalf(".cody state dir leaked into the index: %s", f.Path)
		}
	}
	if !strings.Contains(m.Summary(), "languages: go") {
		t.Fatalf("Summary() = %q", m.Summary())
	}
}

func TestScanRejectsNonDir(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Scan accepted a missing root")
	}
}
