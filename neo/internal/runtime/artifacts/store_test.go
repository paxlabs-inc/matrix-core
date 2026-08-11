// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package artifacts

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"matrix/vault"
)

func artifactVault(t *testing.T, root string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: filepath.Join(root, "vault"),
		UserDID: "did:matrix:artifact-test", KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestFiveMegabyteArtifactProjectsAndRehydratesAfterRestart(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "artifacts")
	session := artifactVault(t, root)
	store, err := Open(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("ordinary real shell output 0123456789abcdef\n"), 128*1024)
	needle := []byte("EXACT-SUBSECTION deployment_id=dep-741 status=healthy\n")
	content := append(line, needle...)
	if len(content) < 5<<20 {
		t.Fatalf("fixture is %d bytes, want at least five MiB", len(content))
	}
	meta, projection, err := store.Put(context.Background(), Metadata{
		LogicalTurnID: "turn-1", CycleIdentity: "cycle-7", CallIdentity: "call-3",
		Tool: "shell_exec", NormalizedArgs: json.RawMessage(`{"command":"status"}`), MIME: "text/plain",
	}, content)
	if err != nil {
		t.Fatal(err)
	}
	encodedProjection, _ := json.Marshal(projection)
	if len(encodedProjection) > 16<<10 || !projection.Truncated || projection.ArtifactID != meta.ArtifactID || projection.ByteSize != int64(len(content)) {
		t.Fatalf("projection is not bounded/typed: bytes=%d %#v", len(encodedProjection), projection)
	}
	for _, suffix := range []string{".content.vault", ".metadata.vault"} {
		raw, err := os.ReadFile(filepath.Join(dir, meta.ArtifactID+suffix))
		if err != nil || !vault.IsVault(raw) || bytes.Contains(raw, needle) {
			t.Fatalf("artifact %s was not encrypted: err=%v", suffix, err)
		}
	}

	restarted, err := Open(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	rehydrated, err := restarted.Rehydrate(context.Background(), meta.ArtifactID, Selector{Search: "deployment_id=dep-741", Limit: 1})
	if err != nil || !bytes.Contains(rehydrated, bytes.TrimSpace(needle)) {
		t.Fatalf("search rehydration = %q err=%v", rehydrated, err)
	}
	offset := int64(bytes.Index(content, needle))
	exact, err := restarted.Rehydrate(context.Background(), meta.ArtifactID, Selector{ByteOffset: offset, ByteLength: int64(len(needle))})
	if err != nil || !bytes.Equal(exact, needle) {
		t.Fatalf("exact byte rehydration mismatch: len=%d err=%v", len(exact), err)
	}
}

func TestStructuredSelectors(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "artifacts"), artifactVault(t, root))
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err := store.Put(context.Background(), Metadata{
		LogicalTurnID: "turn-2", CallIdentity: "call-json", Tool: "api_fetch", MIME: "application/json",
	}, []byte(`{"name":"centra","nested":{"status":"green"},"rows":[{"id":1},{"id":2}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		selector Selector
		want     string
	}{
		{Selector{JSONPointer: "/nested/status"}, `"green"`},
		{Selector{Child: "/rows/1"}, `{"id":2}`},
		{Selector{Fields: []string{"name"}}, `{"name":"centra"}`},
	} {
		got, err := store.Rehydrate(context.Background(), meta.ArtifactID, test.selector)
		if err != nil || string(got) != test.want {
			t.Fatalf("selector %#v = %s err=%v", test.selector, got, err)
		}
	}
}

func TestLargeSearchProjectionIncludesBoundedEvidencePreview(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "artifacts"), artifactVault(t, root))
	if err != nil {
		t.Fatal(err)
	}
	results := make([]map[string]any, 20)
	for index := range results {
		results[index] = map[string]any{
			"title":     "Primary announcement",
			"url":       "https://example.com/source",
			"published": "2026-08-06",
			"snippet":   string(bytes.Repeat([]byte("evidence "), 200)),
		}
	}
	content, _ := json.Marshal(map[string]any{
		"provider": "exa", "query": "agent interoperability", "results": results,
	})
	_, projection, err := store.Put(context.Background(), Metadata{
		LogicalTurnID: "turn-search", CallIdentity: "call-search", Tool: "exa__exa_search",
		MIME: "application/json",
	}, content)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(projection)
	preview, ok := projection.ImportantFields["results_preview"].([]map[string]any)
	if !ok || len(preview) != 12 || preview[0]["url"] != "https://example.com/source" {
		t.Fatalf("search preview = %#v", projection.ImportantFields["results_preview"])
	}
	if len(encoded) > 16<<10 {
		t.Fatalf("projection exceeded bounded context size: %d bytes", len(encoded))
	}
}
