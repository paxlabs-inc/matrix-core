// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/executor/tool"
)

func TestNewInfraOmitsUnavailableMCPAdapterWithoutFailingBoot(t *testing.T) {
	manifest := tool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/degraded-boot-test",
		Servers: []tool.ServerEntry{{
			Alias:         "unavailable",
			Transport:     "stdio",
			Command:       filepath.Join(t.TempDir(), "missing-mcp-server"),
			PackageDigest: "sha256:" + strings.Repeat("a", 64),
			Version:       "1.0.0",
			Tools: []tool.ToolEntry{{
				Name:            "probe",
				SideEffectClass: tool.SideEffectRead,
			}},
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	transcript, events := newCapturingTranscript()
	var stderr bytes.Buffer
	in, err := newInfra(context.Background(), infraOpts{
		ManifestPath: manifestPath,
		SpawnTimeout: time.Second,
		StderrSink:   &stderr,
	}, transcript)
	if err != nil {
		t.Fatalf("newInfra must boot without an optional adapter: %v", err)
	}
	t.Cleanup(func() { _ = in.Close() })

	if got := len(in.manifest.Servers); got != 0 {
		t.Fatalf("live manifest servers = %d, want 0", got)
	}
	if got := len(in.registry.List()); got != 0 {
		t.Fatalf("live registry tools = %d, want 0", got)
	}

	captured := decodeEvents(t, events)
	want := map[string]bool{"mcp.spawn.error": false, "mcp.degraded": false, "registry.built": false}
	for _, event := range captured {
		if _, ok := want[event.Type]; ok {
			want[event.Type] = true
		}
	}
	for event, found := range want {
		if !found {
			t.Fatalf("missing transcript event %q in %#v", event, captured)
		}
	}
}
