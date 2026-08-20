// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEpistemicConfigSurface proves req.8.1: the epistemic knobs exist in
// env + kvx [epistemic] forms with execution-safe defaults (off/off/3/4),
// kvx overlays defaults, and env beats kvx.
func TestEpistemicConfigSurface(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EpistemicPremises || c.EpistemicPredictions {
		t.Fatal("heuristic epistemic mechanisms must be opt-in, not execution gates")
	}
	if c.EpistemicMismatchLimit != 3 || c.EpistemicConvergenceWindow != 4 {
		t.Fatalf("bounds default = %d/%d, want 3/4", c.EpistemicMismatchLimit, c.EpistemicConvergenceWindow)
	}

	doc := `[epistemic]
	premises = true
	predictions = true
mismatch_limit = 5
convergence_window = 7
`
	path := filepath.Join(t.TempDir(), "neo.kvx")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load kvx: %v", err)
	}
	if !c.EpistemicPremises || !c.EpistemicPredictions {
		t.Fatal("kvx [epistemic] must explicitly enable the mechanisms")
	}
	if c.EpistemicMismatchLimit != 5 || c.EpistemicConvergenceWindow != 7 {
		t.Fatalf("kvx bounds = %d/%d, want 5/7", c.EpistemicMismatchLimit, c.EpistemicConvergenceWindow)
	}

	t.Setenv("NEO_EPISTEMIC_PREMISES", "false")
	t.Setenv("NEO_EPISTEMIC_MISMATCH_LIMIT", "2")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if c.EpistemicPremises {
		t.Fatal("env must beat kvx for NEO_EPISTEMIC_PREMISES")
	}
	if c.EpistemicMismatchLimit != 2 {
		t.Fatalf("env mismatch limit = %d, want 2", c.EpistemicMismatchLimit)
	}
	if !c.EpistemicPredictions {
		t.Fatal("untouched kvx value must survive the env overlay")
	}
}
