// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsMatchFrozenSpec(t *testing.T) {
	c := Default()
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"StepBudget", c.StepBudget, 50},
		{"NoProgressStall", c.NoProgressStall, 4},
		{"MaxRetriesPerTool", c.MaxRetriesPerTool, 3},
		{"MaxAdaptAttempts", c.MaxAdaptAttempts, 2},
		{"SoftPct", c.SoftPct, 80},
		{"HardPct", c.HardPct, 92},
		{"MinPatternSuccesses", c.MinPatternSuccesses, 3},
		{"ContextWindowTokens", c.ContextWindowTokens, 256000},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %d, want %d", ch.name, ch.got, ch.want)
		}
	}
	if c.MainModel != "accounts/fireworks/routers/kimi-k2p6-fast" {
		t.Errorf("MainModel = %q", c.MainModel)
	}
	if c.CheapModel != "accounts/fireworks/routers/glm-5p1-fast" {
		t.Errorf("CheapModel = %q", c.CheapModel)
	}
	if len(c.NaturalAllow) == 0 || len(c.EscalateActions) == 0 {
		t.Fatalf("execution surface lists must be non-empty")
	}
}

func TestBudgetTokenMath(t *testing.T) {
	c := Default()
	if got, want := c.SoftBudgetTokens(), 256000*80/100; got != want {
		t.Errorf("SoftBudgetTokens = %d, want %d", got, want)
	}
	if got, want := c.HardBudgetTokens(), 256000*92/100; got != want {
		t.Errorf("HardBudgetTokens = %d, want %d", got, want)
	}
}

func TestIsEscalateAction(t *testing.T) {
	c := Default()
	if !c.IsEscalateAction("send_value") {
		t.Error("send_value should escalate")
	}
	if !c.IsEscalateAction("token_approve") {
		t.Error("token_approve should escalate")
	}
	if c.IsEscalateAction("web_search") {
		t.Error("web_search must NOT escalate")
	}
	if c.IsEscalateAction("not_a_real_action") {
		t.Error("unknown action must NOT escalate")
	}
}

func TestLoadEnvOverridesDefault(t *testing.T) {
	t.Setenv("NEO_MAIN_MODEL", "accounts/fireworks/routers/custom-model")
	t.Setenv("NEO_CONTEXT_WINDOW_TOKENS", "100000")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MainModel != "accounts/fireworks/routers/custom-model" {
		t.Errorf("env did not override MainModel: %q", c.MainModel)
	}
	if c.ContextWindowTokens != 100000 {
		t.Errorf("env did not override ContextWindowTokens: %d", c.ContextWindowTokens)
	}
}

func TestLoadKVXOverlayThenEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "neo.kvx")
	doc := `
[runtime]
agent_name = "Trinity"
cortex_actor = "neo-test"

[models]
main = "accounts/fireworks/routers/from-kvx"
cheap = "accounts/fireworks/routers/cheap-kvx"

[memory]
soft_pct = 70
hard_pct = 88

[loop]
step_budget = 25

[execution]
natural_allow = ["shell", "git"]
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	// kvx overlays defaults.
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AgentName != "Trinity" {
		t.Errorf("AgentName = %q, want Trinity", c.AgentName)
	}
	if c.MainModel != "accounts/fireworks/routers/from-kvx" {
		t.Errorf("MainModel from kvx = %q", c.MainModel)
	}
	if c.SoftPct != 70 || c.HardPct != 88 {
		t.Errorf("memory thresholds = %d/%d, want 70/88", c.SoftPct, c.HardPct)
	}
	if c.StepBudget != 25 {
		t.Errorf("StepBudget = %d, want 25", c.StepBudget)
	}
	if len(c.NaturalAllow) != 2 || c.NaturalAllow[0] != "shell" {
		t.Errorf("natural_allow not overlaid: %v", c.NaturalAllow)
	}

	// env beats kvx.
	t.Setenv("NEO_MAIN_MODEL", "accounts/fireworks/routers/from-env")
	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.MainModel != "accounts/fireworks/routers/from-env" {
		t.Errorf("env should beat kvx, got %q", c2.MainModel)
	}
}

func TestLoadMissingKVXIsNonFatal(t *testing.T) {
	c, err := Load("/nonexistent/neo.kvx")
	if err != nil {
		t.Fatalf("missing kvx must be non-fatal, got %v", err)
	}
	if c.StepBudget != 50 {
		t.Errorf("defaults lost on missing kvx: StepBudget=%d", c.StepBudget)
	}
}
