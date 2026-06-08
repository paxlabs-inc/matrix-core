// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"strings"
	"testing"
)

func TestNormalizeStatement(t *testing.T) {
	if got := normalizeStatement("  Hello   World  "); got != "hello world" {
		t.Errorf("normalize = %q", got)
	}
	if got := normalizeStatement(""); got != "" {
		t.Errorf("empty normalize = %q", got)
	}
}

func TestClampUnit(t *testing.T) {
	if clampUnit(1.5) != 1 {
		t.Error("clamp high")
	}
	if clampUnit(-0.5) != 0 {
		t.Error("clamp low")
	}
	if clampUnit(0.5) != 0.5 {
		t.Error("clamp passthrough")
	}
}

func TestMergeUnique(t *testing.T) {
	got := mergeUnique([]string{"a", "b"}, []string{"b", "c", ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("mergeUnique = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mergeUnique[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("empty -> 0 tokens")
	}
	if EstimateTokens("abcd") < 1 {
		t.Error("non-empty -> >= 1 token")
	}
	if EstimateTokens(strings.Repeat("x", 400)) <= EstimateTokens("x") {
		t.Error("longer string -> more tokens")
	}
}

func TestTruncateTokens(t *testing.T) {
	s := strings.Repeat("word ", 200)
	if truncateTokens(s, 0) != s {
		t.Error("non-positive budget must return the string unchanged")
	}
	out := truncateTokens(s, 2)
	if len(out) >= len(s) || !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncated marker: len=%d", len(out))
	}
}
