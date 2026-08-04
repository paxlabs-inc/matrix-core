// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"strings"
	"testing"

	"matrix/neo/internal/config"
	neomemory "matrix/neo/internal/memory"
)

func TestMemoryMutationToolUsesTypedPathAndBlocksShellMutation(t *testing.T) {
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "memory-mutation-tool-test"
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()
	ctx := context.Background()
	oldURI, err := pager.RememberUserFact(ctx, "The user's favorite editor is Vim.")
	if err != nil {
		t.Fatalf("write old fact: %v", err)
	}

	m := &Manager{
		mutation: pager.Mutate,
		byFunc: map[string]*boundTool{
			"exec__shell": {funcName: "exec__shell", name: "shell", surface: Natural},
		},
	}
	content, isErr, err := m.Dispatch(ctx, MemoryMutateTool, map[string]interface{}{
		"items": []interface{}{map[string]interface{}{
			"operation": "supersede",
			"target":    map[string]interface{}{"uri": oldURI},
			"value":     map[string]interface{}{"content": "The user's favorite editor is Helix."},
		}},
	})
	if err != nil || isErr {
		t.Fatalf("typed mutation: content=%q isErr=%v err=%v", content, isErr, err)
	}
	if !strings.Contains(content, "Helix") || strings.Contains(content, "matrix://Neocortex/") {
		t.Fatalf("plain confirmation mismatch: %q", content)
	}

	content, isErr, err = m.Dispatch(ctx, "exec__shell", map[string]interface{}{
		"command": "curl -X POST http://127.0.0.1:8080/memory/mutate -d '{}';",
	})
	if err != nil || !isErr || !strings.Contains(content, MemoryMutateTool) {
		t.Fatalf("shell mutation guard: content=%q isErr=%v err=%v", content, isErr, err)
	}
}
