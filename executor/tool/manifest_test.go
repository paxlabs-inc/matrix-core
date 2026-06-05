// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"errors"
	"strings"
	"testing"
)

func validManifestJSON(t *testing.T) []byte {
	t.Helper()
	return []byte(`{
		"schema_version": 1,
		"agent": "matrix://agent/test",
		"description": "test agent",
		"servers": [
			{
				"alias": "fs",
				"transport": "stdio",
				"command": "npx",
				"args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"],
				"env": ["DEBUG=1"],
				"package_digest": "sha256:` + strings.Repeat("a", 64) + `",
				"version": "2024.11.1",
				"tools": [
					{"name": "read_text_file", "description": "read text", "side_effect_class": "read"},
					{"name": "write_text_file", "side_effect_class": "write"}
				]
			}
		],
		"allowed_side_effects": ["read", "write"]
	}`)
}

func TestParseValidManifest(t *testing.T) {
	m, err := ParseAgentManifest(validManifestJSON(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Agent != "matrix://agent/test" {
		t.Fatalf("agent: %q", m.Agent)
	}
	if len(m.Servers) != 1 || m.Servers[0].Alias != "fs" {
		t.Fatalf("servers: %+v", m.Servers)
	}
}

func TestManifestRejectsSchemaMismatch(t *testing.T) {
	bad := []byte(`{"schema_version": 99, "agent": "x"}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("got %v", err)
	}
}

func TestManifestRequiresAgent(t *testing.T) {
	bad := []byte(`{"schema_version": 1}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "missing agent") {
		t.Fatalf("got %v", err)
	}
}

func TestManifestRejectsBadSideEffect(t *testing.T) {
	bad := []byte(`{"schema_version":1,"agent":"x","allowed_side_effects":["filesystem"]}`)
	_, err := ParseAgentManifest(bad)
	if !errors.Is(err, ErrInvalidSideEffect) {
		t.Fatalf("expected ErrInvalidSideEffect, got %v", err)
	}
}

func TestManifestRejectsDuplicateAlias(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	bad := []byte(`{
		"schema_version": 1,
		"agent": "x",
		"servers": [
			{"alias":"fs","transport":"stdio","command":"x","package_digest":"` + dig + `","version":"1","tools":[{"name":"a","side_effect_class":"read"}]},
			{"alias":"fs","transport":"stdio","command":"y","package_digest":"` + dig + `","version":"1","tools":[{"name":"b","side_effect_class":"read"}]}
		]
	}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "duplicate server alias") {
		t.Fatalf("got %v", err)
	}
}

func TestManifestRejectsBadDigest(t *testing.T) {
	bad := []byte(`{
		"schema_version": 1,
		"agent": "x",
		"servers": [{"alias":"fs","transport":"stdio","command":"x","package_digest":"deadbeef","version":"1","tools":[{"name":"a","side_effect_class":"read"}]}]
	}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "package_digest") {
		t.Fatalf("got %v", err)
	}
}

func TestManifestRejectsServerNoTools(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	bad := []byte(`{
		"schema_version": 1,
		"agent": "x",
		"servers": [{"alias":"fs","transport":"stdio","command":"x","package_digest":"` + dig + `","version":"1","tools":[]}]
	}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "declares no tools") {
		t.Fatalf("got %v", err)
	}
}

func TestManifestRejectsHTTPMissingEndpoint(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	bad := []byte(`{
		"schema_version": 1,
		"agent": "x",
		"servers": [{"alias":"fs","transport":"http","package_digest":"` + dig + `","version":"1","tools":[{"name":"a","side_effect_class":"read"}]}]
	}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("got %v", err)
	}
}

func TestManifestRejectsUnknownTransport(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	bad := []byte(`{
		"schema_version": 1,
		"agent": "x",
		"servers": [{"alias":"fs","transport":"ssh","command":"x","package_digest":"` + dig + `","version":"1","tools":[{"name":"a","side_effect_class":"read"}]}]
	}`)
	_, err := ParseAgentManifest(bad)
	if err == nil || !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("got %v", err)
	}
}

func TestNativeToolEntryValidation(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	good := &NativeToolEntry{
		Namespace:       "argus",
		Name:            "place_order",
		Version:         "v1",
		Digest:          dig,
		SideEffectClass: "chain",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good native: %v", err)
	}
	bad := *good
	bad.Namespace = "notreal"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected unknown namespace rejection")
	}
}

func TestManifestAcceptsAllNativeNamespaces(t *testing.T) {
	dig := "sha256:" + strings.Repeat("a", 64)
	for _, ns := range []string{"argus", "orob", "plv", "pofq", "registry", "payments", "attest", "chain"} {
		nt := &NativeToolEntry{
			Namespace:       ns,
			Name:            "x",
			Version:         "v1",
			Digest:          dig,
			SideEffectClass: "chain",
		}
		if err := nt.Validate(); err != nil {
			t.Errorf("namespace %q rejected: %v", ns, err)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
