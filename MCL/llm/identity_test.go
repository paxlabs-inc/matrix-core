// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"strings"
	"testing"

	"matrix/mcl/mtx/interpreter"
)

// TestIdentityPreamble_Locked verifies the preamble text matches the
// matrix.kvx sess#34 lock verbatim. Edits MUST bump IdentityVersion
// concurrently; this test fails until both are updated.
func TestIdentityPreamble_Locked(t *testing.T) {
	const wantPrefix = "You are Matrix"
	const wantPath = "/root/matrix"
	const wantTail = "improving Matrix itself."

	if !strings.HasPrefix(IdentityPreamble, wantPrefix) {
		t.Errorf("IdentityPreamble must start with %q; got %q", wantPrefix, truncate(IdentityPreamble, 60))
	}
	if !strings.Contains(IdentityPreamble, wantPath) {
		t.Errorf("IdentityPreamble must reference %q", wantPath)
	}
	if !strings.HasSuffix(IdentityPreamble, wantTail) {
		t.Errorf("IdentityPreamble must end with %q; got %q", wantTail, IdentityPreamble[max(0, len(IdentityPreamble)-len(wantTail)-10):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestIdentityVersion_Format guards the matrix-identity-v<N> format.
// Compile-cache (sess#31d) composes this into model_digest; a regex-
// unfriendly value would break audit consumers downstream.
func TestIdentityVersion_Format(t *testing.T) {
	if !strings.HasPrefix(IdentityVersion, "matrix-identity-v") {
		t.Errorf("IdentityVersion must start with 'matrix-identity-v'; got %q", IdentityVersion)
	}
}

func TestInjectIdentity_PrependsFirst(t *testing.T) {
	in := []interpreter.Message{
		{Role: "user", Content: "build a thing"},
	}
	out := InjectIdentity(in)

	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].Role != "system" {
		t.Errorf("out[0].Role = %q, want system", out[0].Role)
	}
	if out[0].Content != IdentityPreamble {
		t.Errorf("out[0].Content mismatch with IdentityPreamble")
	}
	if out[1].Content != "build a thing" {
		t.Errorf("user message displaced; out[1] = %+v", out[1])
	}
}

func TestInjectIdentity_DoesNotMutateInput(t *testing.T) {
	in := []interpreter.Message{
		{Role: "user", Content: "test"},
	}
	_ = InjectIdentity(in)
	if len(in) != 1 || in[0].Content != "test" {
		t.Errorf("input mutated: %+v", in)
	}
}

func TestInjectIdentity_PreservesExistingSystemMessages(t *testing.T) {
	// Skill-supplied system prompts must survive intact at index 1+.
	in := []interpreter.Message{
		{Role: "system", Content: "Original skill system prompt"},
		{Role: "user", Content: "the prose"},
	}
	out := InjectIdentity(in)

	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	if out[0].Content != IdentityPreamble {
		t.Errorf("preamble must be first system message")
	}
	if out[1].Role != "system" || out[1].Content != "Original skill system prompt" {
		t.Errorf("existing system prompt lost; out[1] = %+v", out[1])
	}
	if out[2].Role != "user" || out[2].Content != "the prose" {
		t.Errorf("user message lost; out[2] = %+v", out[2])
	}
}

func TestMaybeInjectIdentity_GatedByConfig(t *testing.T) {
	in := []interpreter.Message{
		{Role: "user", Content: "x"},
	}

	// InjectIdentity=false → unchanged (legacy byte-identity invariant)
	off := maybeInjectIdentity(Config{InjectIdentity: false}, in)
	if len(off) != 1 || off[0].Content != "x" {
		t.Errorf("InjectIdentity=false must be byte-identity; got %+v", off)
	}

	// InjectIdentity=true → preamble prepended
	on := maybeInjectIdentity(Config{InjectIdentity: true}, in)
	if len(on) != 2 || on[0].Content != IdentityPreamble {
		t.Errorf("InjectIdentity=true must prepend preamble; got %+v", on)
	}
}

func TestIdentityModelDigestSuffix_Gated(t *testing.T) {
	if got := IdentityModelDigestSuffix(Config{InjectIdentity: false}); got != "" {
		t.Errorf("InjectIdentity=false suffix = %q, want empty", got)
	}
	got := IdentityModelDigestSuffix(Config{InjectIdentity: true})
	want := "+identity=" + IdentityVersion
	if got != want {
		t.Errorf("InjectIdentity=true suffix = %q, want %q", got, want)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
