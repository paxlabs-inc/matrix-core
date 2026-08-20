// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
		{"ContextWindowTokens", c.ContextWindowTokens, 1000000},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %d, want %d", ch.name, ch.got, ch.want)
		}
	}
	// MiMo direct-provider migration: every Neo lane pins the native
	// mimo-v2.5-pro id (the xiaomimimo/ Novita fleet form is a retired alias
	// the client still accepts — MCL llm.IsXiaomiModel).
	if c.MainModel != "mimo-v2.5-pro" {
		t.Errorf("MainModel = %q", c.MainModel)
	}
	if c.CheapModel != "mimo-v2.5-pro" {
		t.Errorf("CheapModel = %q", c.CheapModel)
	}
	if c.ConsolidationModel != "mimo-v2.5-pro" {
		t.Errorf("ConsolidationModel = %q", c.ConsolidationModel)
	}
	if c.CassandraModel != "grok-4.20-0309-non-reasoning" {
		t.Errorf("CassandraModel = %q", c.CassandraModel)
	}
	if c.CassandraEscalateModel != "grok-4.3" {
		t.Errorf("CassandraEscalateModel = %q", c.CassandraEscalateModel)
	}
	if len(c.NaturalAllow) == 0 || len(c.EscalateActions) == 0 {
		t.Fatalf("execution surface lists must be non-empty")
	}
}

func TestBudgetTokenMath(t *testing.T) {
	// 1M default: percentage headroom (200K soft / 80K hard) is absurd at this
	// scale, so the absolute caps bind — budgets sit just under the window and
	// trimming fires only near true pressure (req 1.1/1.2).
	c := Default()
	if got, want := c.SoftBudgetTokens(), 1000000-softHeadroomCapTokens; got != want {
		t.Errorf("SoftBudgetTokens = %d, want %d", got, want)
	}
	if got, want := c.HardBudgetTokens(), 1000000-hardHeadroomCapTokens; got != want {
		t.Errorf("HardBudgetTokens = %d, want %d", got, want)
	}

	// Small-window override (32K local models via NEO_CONTEXT_WINDOW_TOKENS):
	// the caps never bind, so the percentage-derived behavior is unchanged.
	c.ContextWindowTokens = 32768
	if got, want := c.SoftBudgetTokens(), 32768*80/100; got != want {
		t.Errorf("32K SoftBudgetTokens = %d, want %d", got, want)
	}
	if got, want := c.HardBudgetTokens(), 32768*92/100; got != want {
		t.Errorf("32K HardBudgetTokens = %d, want %d", got, want)
	}
}

func TestWindowOverridesAuthoritative(t *testing.T) {
	t.Setenv("NEO_CONTEXT_WINDOW_TOKENS", "32768")
	c := Default()
	c.applyEnv()
	if c.ContextWindowTokens != 32768 {
		t.Fatalf("env override ContextWindowTokens = %d, want 32768", c.ContextWindowTokens)
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

func TestNeocortexEnvWiring(t *testing.T) {
	t.Setenv("NEO_NEOCORTEX_SOCKET", "/tmp/neocortex-config-test.sock")
	t.Setenv("NEO_NEOCORTEX_TOKEN", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("NEO_NEOCORTEX_ACTOR", "neo-test")
	t.Setenv("NEO_DATA_ROOT", "/tmp/neo-data")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.NeocortexSocket != "/tmp/neocortex-config-test.sock" {
		t.Fatalf("NeocortexSocket = %q", c.NeocortexSocket)
	}
	if c.NeocortexToken != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("NeocortexToken = %q", c.NeocortexToken)
	}
	if c.NeocortexActor != "neo-test" {
		t.Fatalf("NeocortexActor = %q", c.NeocortexActor)
	}
	if c.DataRoot != "/tmp/neo-data" {
		t.Fatalf("DataRoot = %q", c.DataRoot)
	}
}

func TestLoadKVXOverlayThenEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "neo.kvx")
	doc := `
[runtime]
agent_name = "Trinity"
neocortex_actor = "neo-test"

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

func TestRuntimeProviderConfigIsSingleGatewayLane(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "neo.kvx")
	doc := `
[runtime]
actor_did = "did:matrix:test"

[runtime.provider]
gateway_url = "https://gateway.example"
bearer_env = "TEST_GATEWAY_TOKEN"
max_attempts = 4
backoff_initial_ms = 20
backoff_max_ms = 80
idle_timeout_seconds = 9
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := config.RuntimeProvider
	if provider.GatewayURL != "https://gateway.example" ||
		provider.BearerEnv != "TEST_GATEWAY_TOKEN" ||
		provider.MaxAttempts != 4 ||
		provider.BackoffInitial != 20*time.Millisecond ||
		provider.BackoffMax != 80*time.Millisecond ||
		provider.IdleTimeout != 9*time.Second {
		t.Fatalf("RuntimeProvider = %+v", provider)
	}

	t.Setenv("MATRIX_GATEWAY_URL", "https://gateway.env")
	t.Setenv("NEO_RUNTIME_PROVIDER_MAX_ATTEMPTS", "2")
	t.Setenv("NEO_RUNTIME", "resurrection")
	fromEnv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.RuntimeProvider.GatewayURL != "https://gateway.env" ||
		fromEnv.RuntimeProvider.MaxAttempts != 2 ||
		fromEnv.AgentRuntime != "resurrection" {
		t.Fatalf("env RuntimeProvider = %+v", fromEnv.RuntimeProvider)
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

// --- P2-4: Local sandbox preset ---

func TestSandboxPreset_UsesStubEmbedder(t *testing.T) {
	// A sandbox profile MUST select the Hash embedder stub. The existing
	// pickEmbedder() (neo/internal/memory/embedder.go) selects HashEmbedder
	// when GatewayURL is empty and no provider API key env is set — so the
	// preset wires EmbedModel to the hash-stub sentinel and blanks the
	// gateway/actor fields that would route to a metered API embedder.
	c := Sandbox()
	if c.EmbedModel != "hash-stub" {
		t.Errorf("sandbox EmbedModel = %q, want \"hash-stub\" (forces the Hash embedder stub)", c.EmbedModel)
	}
	if c.GatewayURL != "" {
		t.Errorf("sandbox GatewayURL = %q, want empty (no metered embedder route)", c.GatewayURL)
	}
	if c.ActorDID != "" {
		t.Errorf("sandbox ActorDID = %q, want empty (no gateway attribution)", c.ActorDID)
	}
}

func TestSandboxPresetUsesTempDataRoot(t *testing.T) {
	// A sandbox profile MUST use a throwaway runtime root so no production
	// state is touched. The path is under the OS temp dir and namespaced
	// "matrix-sandbox" so it is identifiable and cleanable.
	c := Sandbox()
	if c.DataRoot == "" {
		t.Fatal("sandbox DataRoot must not be empty")
	}
	if c.DataRoot == Default().DataRoot {
		t.Errorf("sandbox DataRoot = production default %q — must be a temp dir", c.DataRoot)
	}
	if c.NeocortexActor != "sandbox" {
		t.Errorf("sandbox NeocortexActor = %q, want \"sandbox\"", c.NeocortexActor)
	}
}

func TestSandboxPreset_NoLiveChainCalls(t *testing.T) {
	// A sandbox profile MUST make zero live chain (RPC) calls. DaemonURL
	// is the chain-core_execute delegation endpoint in the neo config;
	// empty disables it. GatewayURL is the metered-LLM gateway (also the
	// chain-adjacent spend surface); empty disables it.
	c := Sandbox()
	if c.DaemonURL != "" {
		t.Errorf("sandbox DaemonURL = %q, want empty (no live chain/core_execute delegation)", c.DaemonURL)
	}
	if c.GatewayURL != "" {
		t.Errorf("sandbox GatewayURL = %q, want empty (no metered gateway / chain-adjacent spend)", c.GatewayURL)
	}
}

func TestSandboxPreset_DefaultProfileUnaffected(t *testing.T) {
	// Calling Sandbox() must NOT mutate the package-level Default() values.
	// Default() is a constructor (returns a fresh Config by value), so this
	// is a structural guarantee: Sandbox() composes Default() and then
	// overrides fields on its own copy, leaving Default() byte-identical.
	d := Default()
	_ = Sandbox()
	d2 := Default()
	// Config contains slices ([]string), so a direct != comparison is not
	// valid. Instead verify the key fields the preset overrides are still
	// the production defaults.
	if d.DataRoot != d2.DataRoot {
		t.Errorf("Default DataRoot mutated: %q vs %q", d.DataRoot, d2.DataRoot)
	}
	if d.DaemonURL != d2.DaemonURL {
		t.Errorf("Default DaemonURL mutated: %q vs %q", d.DaemonURL, d2.DaemonURL)
	}
	if d.GatewayURL != d2.GatewayURL {
		t.Errorf("Default GatewayURL mutated: %q vs %q", d.GatewayURL, d2.GatewayURL)
	}
	if d.EmbedModel != d2.EmbedModel {
		t.Errorf("Default EmbedModel mutated: %q vs %q", d.EmbedModel, d2.EmbedModel)
	}
	// The sandbox profile must NOT share the production runtime root.
	s := Sandbox()
	if s.DataRoot == d.DataRoot {
		t.Error("sandbox DataRoot must differ from the production default")
	}
}

// --- P2-7: Adaptive step budget config keys ---

func TestStepBudgetAdaptiveDefaults(t *testing.T) {
	// By default adaptation is disabled: min=0, max=50 (== StepBudget).
	// This ensures today's fixed-budget behavior is the zero-config default.
	c := Default()
	if c.StepBudgetMin != 0 {
		t.Errorf("StepBudgetMin default = %d, want 0 (adaptation disabled)", c.StepBudgetMin)
	}
	if c.StepBudgetMax != 50 {
		t.Errorf("StepBudgetMax default = %d, want 50 (== StepBudget)", c.StepBudgetMax)
	}
}

func TestStepBudgetAdaptiveEnvOverrides(t *testing.T) {
	t.Setenv("NEO_STEP_BUDGET_MIN", "20")
	t.Setenv("NEO_STEP_BUDGET_MAX", "80")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StepBudgetMin != 20 {
		t.Errorf("StepBudgetMin = %d, want 20 (env override)", c.StepBudgetMin)
	}
	if c.StepBudgetMax != 80 {
		t.Errorf("StepBudgetMax = %d, want 80 (env override)", c.StepBudgetMax)
	}
}

func TestStepBudgetAdaptiveKVXOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "neo.kvx")
	doc := `
[loop]
step_budget_min = 15
step_budget_max = 65
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StepBudgetMin != 15 {
		t.Errorf("StepBudgetMin = %d, want 15 (kvx overlay)", c.StepBudgetMin)
	}
	if c.StepBudgetMax != 65 {
		t.Errorf("StepBudgetMax = %d, want 65 (kvx overlay)", c.StepBudgetMax)
	}
}

func TestActivationBudgetDerivation(t *testing.T) {
	cases := []struct {
		window, explicit, want int
	}{
		{1000000, 0, 20000},     // 1M default: 2% of window
		{32768, 0, 3000},        // small local-model override keeps the historical floor
		{150000, 0, 3000},       // at/below the floor knee
		{256000, 0, 5120},       // proportional in between
		{4000000, 0, 24000},     // capped under Neocortex.MaxActivateBudgetTokens
		{1000000, 12345, 12345}, // explicit knob wins verbatim
	}
	for _, tc := range cases {
		c := Default()
		c.ContextWindowTokens = tc.window
		c.ActivationBudgetTokens = tc.explicit
		if got := c.ActivationBudget(); got != tc.want {
			t.Errorf("ActivationBudget(window=%d explicit=%d) = %d, want %d", tc.window, tc.explicit, got, tc.want)
		}
	}
}

func TestActivationBudgetEnvOverride(t *testing.T) {
	t.Setenv("NEO_ACTIVATION_BUDGET_TOKENS", "9000")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ActivationBudget() != 9000 {
		t.Errorf("ActivationBudget = %d, want 9000 (env override)", c.ActivationBudget())
	}
}
