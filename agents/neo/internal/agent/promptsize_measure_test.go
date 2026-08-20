// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"centra/packages/construct/schema"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/memory"
	"centra/agents/neo/internal/tools"
)

// TestMeasurePromptSize is a measurement harness, not an assertion: it renders
// the REAL current stable prefix (charter + capability surface + ground truth)
// and the real pinned tier through the production assembly code and dumps each
// section to PROMPT_MEASURE_DIR for external byte/token accounting. Skipped
// unless PROMPT_MEASURE_DIR is set.
func TestMeasurePromptSize(t *testing.T) {
	dir := os.Getenv("PROMPT_MEASURE_DIR")
	if dir == "" {
		t.Skip("set PROMPT_MEASURE_DIR to run the measurement harness")
	}

	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-measure"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	tm := &tools.Manager{}
	tm.SetRecall(func(ctx context.Context, query string, types []string, k int, asOf *time.Time) (string, error) {
		return "", nil
	})
	tm.SetTodo(func(ctx context.Context, items []tools.TodoItem) error { return nil })
	tm.SetSurfaceEmitter(func(ctx context.Context, s *schema.Surface) error { return nil })
	tm.SetPreview(func(ctx context.Context) (string, error) { return "", nil })

	a := New(Options{Config: cfg, Tools: tm, Pager: pager, ConvID: "conv-measure",
		Capability: &CapabilitySurface{}})
	a.SetWorkspace("/data/workspace", "default", "", "")

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("charter.txt", a.systemPrompt())
	write("capability_scaffold.txt", a.renderCapabilitySurface())
	write("stable_prefix.txt", a.stableSystem())
	write("pinned.txt", pager.Pinned(t.Context(), ""))
}
