// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"errors"
	"strings"
	"testing"
)

func TestExtractText(t *testing.T) {
	r := &Result{Content: []Content{
		{Type: ContentTypeText, Text: "hello"},
		{Type: ContentTypeImage, Data: "abc"},
		{Type: ContentTypeText, Text: "world"},
	}}
	got := ExtractText(r)
	want := "hello\nworld"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := ExtractText(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}

func TestSideEffectClassValidation(t *testing.T) {
	for _, c := range []string{SideEffectRead, SideEffectWrite, SideEffectNetwork, SideEffectShell, SideEffectChain} {
		if err := validateSideEffect(c); err != nil {
			t.Fatalf("%q rejected: %v", c, err)
		}
	}
	err := validateSideEffect("filesystem")
	if !errors.Is(err, ErrInvalidSideEffect) {
		t.Fatalf("expected ErrInvalidSideEffect, got %v", err)
	}
}

func TestCapabilityGates(t *testing.T) {
	if !AllowAllSideEffects(SideEffectRead) {
		t.Fatal("AllowAllSideEffects denied read")
	}
	if AllowAllSideEffects("bogus") {
		t.Fatal("AllowAllSideEffects accepted bogus")
	}
	gate := AllowOnlySideEffects(SideEffectRead, SideEffectNetwork)
	if !gate(SideEffectRead) || !gate(SideEffectNetwork) {
		t.Fatal("narrow gate denied allowed class")
	}
	if gate(SideEffectShell) {
		t.Fatal("narrow gate accepted disallowed class")
	}
}

func TestParseToolURIMCP(t *testing.T) {
	u, err := ParseToolURI("matrix://tool/mcp/fs/read_text_file@2024.11.1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !u.IsMCP() {
		t.Fatal("expected MCP form")
	}
	if u.Server != "fs" || u.Name != "read_text_file" || u.Version != "2024.11.1" {
		t.Fatalf("unexpected: %+v", u)
	}
}

func TestParseToolURINative(t *testing.T) {
	u, err := ParseToolURI("matrix://tool/argus/place_order@v0.1.0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.IsMCP() || !u.IsNative() {
		t.Fatalf("expected native: %+v", u)
	}
	if u.Provider != "argus" || u.Name != "place_order" || u.Version != "v0.1.0" {
		t.Fatalf("unexpected: %+v", u)
	}
}

func TestParseToolURIRejectsBare(t *testing.T) {
	_, err := ParseToolURI("matrix://tool/mcp/fs/read_text_file")
	if !errors.Is(err, ErrUnpinnedTool) {
		t.Fatalf("expected ErrUnpinnedTool, got %v", err)
	}
}

func TestParseToolURIRejectsBadScheme(t *testing.T) {
	_, err := ParseToolURI("https://example.com/tool@v1")
	if !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("expected ErrInvalidURI, got %v", err)
	}
}

func TestParseToolURIRejectsBadAlias(t *testing.T) {
	_, err := ParseToolURI("matrix://tool/mcp/Bad-Alias!/x@1")
	if !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("expected ErrInvalidURI, got %v", err)
	}
}

func TestParseToolURIRejectsUnknownNativeNamespace(t *testing.T) {
	_, err := ParseToolURI("matrix://tool/notreal/x@1")
	if !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("expected ErrInvalidURI, got %v", err)
	}
}

func TestParseToolURIRoundTrip(t *testing.T) {
	for _, s := range []string{
		"matrix://tool/mcp/fs/read_text_file@2024.11.1",
		"matrix://tool/argus/place_order@v0.1.0",
		"matrix://tool/mcp/git/commit@1.0",
	} {
		u, err := ParseToolURI(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if u.String() != s {
			t.Fatalf("round-trip: got %q want %q", u.String(), s)
		}
	}
}

func TestValidDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	if !validDigest(good) {
		t.Fatal("good digest rejected")
	}
	for _, bad := range []string{
		"",
		"sha256:abc",
		"md5:" + strings.Repeat("a", 32),
		"sha256:" + strings.Repeat("g", 64), // non-hex
	} {
		if validDigest(bad) {
			t.Fatalf("bad digest %q accepted", bad)
		}
	}
}

func TestResolveEnv(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "OK" {
			return "yes", true
		}
		return "", false
	}
	v, ok := ResolveEnv("$env:OK", lookup)
	if !ok || v != "yes" {
		t.Fatalf("got %q,%v", v, ok)
	}
	v, ok = ResolveEnv("$env:MISSING", lookup)
	if ok {
		t.Fatalf("expected missing, got %q", v)
	}
	v, ok = ResolveEnv("literal", lookup)
	if !ok || v != "literal" {
		t.Fatalf("literal: %q,%v", v, ok)
	}
}

func TestResolveEnvList(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "TOKEN" {
			return "abc123", true
		}
		return "", false
	}
	out, _, err := ResolveEnvList([]string{"GITHUB_TOKEN=$env:TOKEN", "FOO=bar"}, lookup)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"GITHUB_TOKEN=abc123", "FOO=bar"}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("out[%d]=%q want %q", i, out[i], w)
		}
	}

	_, missing, err := ResolveEnvList([]string{"FOO=$env:NOT_THERE"}, lookup)
	if err == nil {
		t.Fatal("expected error for missing env")
	}
	if missing != "$env:NOT_THERE" {
		t.Fatalf("missing=%q", missing)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
