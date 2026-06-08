// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"strings"
	"testing"
)

func TestParseKVXSectionsAndTypes(t *testing.T) {
	src := `
# a comment line
[runtime]
agent_name = "Neo"   # trailing comment
empty_ok =

[memory]
soft_pct = 80
bad_int = notanumber

[execution]
natural_allow = ["shell", "git", "web_search"]
single = "lonely"
`
	doc, err := parseKVX(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parseKVX: %v", err)
	}
	if !doc.has("runtime") || !doc.has("memory") || !doc.has("execution") {
		t.Fatal("missing sections")
	}
	if got := doc.str("runtime", "agent_name"); got != "Neo" {
		t.Errorf("agent_name = %q, want Neo (comment must be stripped)", got)
	}
	if got := doc.intOr("memory", "soft_pct", -1); got != 80 {
		t.Errorf("soft_pct = %d, want 80", got)
	}
	if got := doc.intOr("memory", "bad_int", 42); got != 42 {
		t.Errorf("bad int should fall back to 42, got %d", got)
	}
	if got := doc.intOr("memory", "absent", 7); got != 7 {
		t.Errorf("absent key should fall back to 7, got %d", got)
	}
	list := doc.list("execution", "natural_allow")
	if len(list) != 3 || list[0] != "shell" || list[2] != "web_search" {
		t.Errorf("list parse wrong: %v", list)
	}
	if single := doc.list("execution", "single"); len(single) != 1 || single[0] != "lonely" {
		t.Errorf("bare value should become single-element list: %v", single)
	}
	if got := doc.strOr("runtime", "missing", "fallback"); got != "fallback" {
		t.Errorf("strOr fallback failed: %q", got)
	}
}

func TestStripCommentRespectsQuotes(t *testing.T) {
	if got := stripComment(`key = "a # b"`); got != `key = "a # b"` {
		t.Errorf("quoted # was stripped: %q", got)
	}
	if got := stripComment(`key = value # trailing`); got != "key = value" {
		t.Errorf("trailing comment not stripped: %q", got)
	}
}

func TestInterpolateEnv(t *testing.T) {
	t.Setenv("NEO_TEST_INTERP", "expanded")
	src := `
[runtime]
val = "${NEO_TEST_INTERP}/suffix"
`
	doc, err := parseKVX(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.str("runtime", "val"); got != "expanded/suffix" {
		t.Errorf("interpolation failed: %q", got)
	}
}

func TestUnterminatedSectionErrors(t *testing.T) {
	if _, err := parseKVX(strings.NewReader("[oops\n")); err == nil {
		t.Error("expected error on unterminated section header")
	}
}

func TestUnquoteAndSplitList(t *testing.T) {
	if unquote(`"hi"`) != "hi" {
		t.Error("unquote failed")
	}
	if unquote(`bare`) != "bare" {
		t.Error("unquote of bare value should be identity")
	}
	parts := splitList(`"a, b", "c"`)
	if len(parts) != 2 {
		t.Errorf("splitList should not split inside quotes: %v", parts)
	}
}
