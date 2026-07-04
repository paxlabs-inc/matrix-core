// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/workspace"
)

func seed(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedRulesRoot mirrors the real rules/ layout: common/ + per-language dirs.
func seedRulesRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	seed(t, root, "common/coding-style.md", "# Coding Style\n")
	seed(t, root, "common/testing.md", "# Testing\n")
	seed(t, root, "golang/testing.md", "# Go Testing\n")
	seed(t, root, "golang/patterns.md", "# Go Patterns\n")
	seed(t, root, "typescript/coding-style.md", "# TS Style\n")
	seed(t, root, "python/security.md", "# Py Security\n")
	seed(t, root, "golang/notes.txt", "not markdown\n")
	return root
}

func modelFor(t *testing.T, langs map[string]int) *workspace.Model {
	t.Helper()
	root := t.TempDir()
	seed(t, root, "placeholder.txt", "x\n")
	m, err := workspace.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Languages = langs
	return m
}

func TestLoadRulesDetectedStack(t *testing.T) {
	rulesRoot := seedRulesRoot(t)
	m := modelFor(t, map[string]int{"go": 10, "typescript": 3})
	r, err := LoadRules(rulesRoot, m)
	if err != nil {
		t.Fatal(err)
	}
	refs := r.Refs()
	joined := strings.Join(refs, " ")
	for _, want := range []string{
		"common/coding-style.md", "common/testing.md",
		"golang/patterns.md", "golang/testing.md",
		"typescript/coding-style.md",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Refs() = %v, want %s", refs, want)
		}
	}
	if strings.Contains(joined, "python/") {
		t.Fatalf("undetected stack leaked into refs: %v", refs)
	}
	if strings.Contains(joined, "notes.txt") {
		t.Fatalf("non-markdown file leaked into refs: %v", refs)
	}
	// Common rules come first (they are the base the language files extend).
	if !strings.HasPrefix(refs[0], "common/") {
		t.Fatalf("common rules not first: %v", refs)
	}
}

func TestLoadRulesJavascriptMapsToTypescript(t *testing.T) {
	rulesRoot := seedRulesRoot(t)
	m := modelFor(t, map[string]int{"javascript": 5})
	r, err := LoadRules(rulesRoot, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Language["typescript"]) != 1 {
		t.Fatalf("javascript did not map to typescript rules: %+v", r.Language)
	}
}

func TestLoadRulesUnknownLanguageSkipped(t *testing.T) {
	rulesRoot := seedRulesRoot(t)
	m := modelFor(t, map[string]int{"shell": 4, "sql": 2})
	r, err := LoadRules(rulesRoot, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Language) != 0 {
		t.Fatalf("languages without rule dirs must be skipped: %+v", r.Language)
	}
	if len(r.Common) != 2 {
		t.Fatalf("common rules always apply: %v", r.Common)
	}
}

func TestRenderWorkingPolicy(t *testing.T) {
	rulesRoot := seedRulesRoot(t)
	m := modelFor(t, map[string]int{"go": 1})
	r, err := LoadRules(rulesRoot, m)
	if err != nil {
		t.Fatal(err)
	}
	rendered := r.Render()
	for _, want := range []string{"standards in force", "common/coding-style.md", "golang (detected stack)", "golang/testing.md"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("Render() missing %q:\n%s", want, rendered)
		}
	}
	abs := r.AbsRefs()
	if len(abs) != len(r.Refs()) || !filepath.IsAbs(abs[0]) {
		t.Fatalf("AbsRefs() = %v", abs)
	}
	if _, err := os.Stat(abs[0]); err != nil {
		t.Fatalf("AbsRefs()[0] does not exist: %v", err)
	}
}

func TestSkillsIndexAndLoad(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "web-perf/SKILL.md", "---\nname: web-perf\n---\n# Web Perf\nMeasure LCP first.\n")
	seed(t, root, "sql-tuning/SKILL.md", "---\nname: sql-tuning\n---\n# SQL Tuning\n")
	seed(t, root, "broken-skill/README.md", "no SKILL.md here\n")
	seed(t, root, "INDEX.json", "{}\n")

	s, err := LoadSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Names) != 2 || s.Names[0] != "sql-tuning" || s.Names[1] != "web-perf" {
		t.Fatalf("Names = %v", s.Names)
	}
	if !s.Has("web-perf") || s.Has("broken-skill") {
		t.Fatalf("Has() wrong: %v", s.Names)
	}
	body, err := s.Load("web-perf")
	if err != nil || !strings.Contains(body, "Measure LCP first.") {
		t.Fatalf("Load() = %q, %v", body, err)
	}
	if _, err := s.Load("../../../etc/passwd"); err == nil {
		t.Fatal("Load accepted a path-traversal name")
	}
	if _, err := s.Load("no-such-skill"); err == nil {
		t.Fatal("Load accepted a missing skill")
	}
	idx := s.RenderIndex()
	if !strings.Contains(idx, "- sql-tuning") || !strings.Contains(idx, "- web-perf") {
		t.Fatalf("RenderIndex() = %q", idx)
	}
}

func TestSkillsDescriptions(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "api-design/SKILL.md",
		"---\nname: api-design\ndescription: REST API design patterns for production APIs.\n---\n# API Design\n")
	seed(t, root, "no-desc/SKILL.md", "# No frontmatter here\nJust a body.\n")

	s, err := LoadSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Descriptions["api-design"]; got != "REST API design patterns for production APIs." {
		t.Fatalf("description = %q", got)
	}
	if _, ok := s.Descriptions["no-desc"]; ok {
		t.Fatal("no-desc should carry no description")
	}
	idx := s.RenderIndex()
	if !strings.Contains(idx, "- api-design — REST API design patterns for production APIs.") {
		t.Fatalf("RenderIndex() = %q", idx)
	}
	if !strings.Contains(idx, "- no-desc\n") {
		t.Fatalf("RenderIndex() missing bare entry: %q", idx)
	}
}

// TestAgainstRealRepoRules proves the mapping holds on the actual deployed
// rules/ + skills/ trees when running inside the Matrix repo.
func TestAgainstRealRepoRules(t *testing.T) {
	rulesRoot := "../../../rules"
	if _, err := os.Stat(rulesRoot); err != nil {
		t.Skip("real rules/ not present")
	}
	m := modelFor(t, map[string]int{"go": 100, "typescript": 50})
	r, err := LoadRules(rulesRoot, m)
	if err != nil {
		t.Fatal(err)
	}
	refs := strings.Join(r.Refs(), " ")
	for _, want := range []string{"common/testing.md", "golang/testing.md", "typescript/"} {
		if !strings.Contains(refs, want) {
			t.Fatalf("real rules refs missing %s: %v", want, r.Refs())
		}
	}
	skillsRoot := "../../../skills"
	if _, err := os.Stat(skillsRoot); err != nil {
		t.Skip("real skills/ not present")
	}
	s, err := LoadSkills(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Names) < 100 {
		t.Fatalf("real skills index suspiciously small: %d", len(s.Names))
	}
	body, err := s.Load(s.Names[0])
	if err != nil || body == "" {
		t.Fatalf("real skill body: %v", err)
	}
}
