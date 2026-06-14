// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package config holds Neo's runtime configuration.
//
// The locked operational contract — context-budget thresholds, loop
// discipline, and the execution surface (which actions stay "Natural" vs.
// escalate to MCL) — comes from the frozen design spec at neo/neo.frozen.kvx
// and is encoded as the Default() values here. Deployment wiring (models,
// cortex location, the daemon URL used for core_execute delegation) is
// overlaid from an optional runtime .kvx file and then from environment
// variables, so a fresh checkout runs with zero config.
//
// Precedence (lowest → highest): Default() < runtime .kvx < environment.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config is Neo's fully-resolved runtime configuration.
type Config struct {
	// --- identity / runtime wiring ---
	AgentName    string // human label, "Neo"
	CortexRoot   string // root dir of the cortex brain (per-actor stores live under it)
	CortexActor  string // actor scope for Neo's memory store
	DaemonURL    string // base URL of the MCL daemon for core_execute delegation
	ManifestPath string // agent manifest declaring MCP servers (agents/default.json)
	SkillsRoot   string // skills corpus root (procedural-pattern promotion target)

	// --- models (provider-qualified ids; see matrix/mcl/llm DetectProvider) ---
	MainModel  string // the conversational tool-calling loop
	CheapModel string // background write-back, compaction + summary validation
	EmbedModel string // semantic page-fault embeddings (gateway /v1/embeddings or direct provider)

	// --- memory budget (context window = RAM; cortex = disk) ---
	ContextWindowTokens   int // total model context window, for budget math
	SoftPct               int // cooperative compaction threshold (finish atomic step, then compact)
	HardPct               int // forced compaction threshold (runaway backstop)
	RetrievalTopK         int // page-fault: top-K cortex records per retrieval
	RetrievalBudgetTokens int // token ceiling for retrieved records
	PinnedBudgetTokens    int // token ceiling for the always-injected pinned block
	RecallTopK            int // conversational recall: top-K relevant past turns per turn
	RecallBudgetTokens    int // token ceiling for the recalled past-turns block

	// --- loop discipline ---
	StepBudget        int // max tool-call iterations per turn (anti-infinite)
	NoProgressStall   int // identical-failing-call / no-state-change count that trips a stall
	MaxRetriesPerTool int // recovery ladder rung 1: bounded retries for transient failures
	MaxAdaptAttempts  int // recovery ladder rung 2: bounded approach revisions

	// --- procedural memory guards ---
	MinPatternSuccesses int // successes required before a candidate pattern is injected

	// --- execution surface ---
	NaturalAllow    []string // reversible actions Neo performs directly (no wallet signature)
	EscalateActions []string // actions that cross into MCL (require a user wallet signature)

	// --- LLM transport ---
	GatewayURL string // optional metered-LLM gateway (empty = direct provider)
	ActorDID   string // actor DID stamped on gateway calls
}

// Default returns Neo's defaults, encoding the frozen spec's locked
// operational contract (neo/neo.frozen.kvx).
func Default() Config {
	return Config{
		AgentName:    "Neo",
		CortexRoot:   "/root/.cortex",
		CortexActor:  "neo",
		DaemonURL:    "http://127.0.0.1:8080",
		ManifestPath: "agents/default.json",
		SkillsRoot:   "skills",

		MainModel:  "accounts/fireworks/models/kimi-k2p7-code",
		CheapModel: "accounts/fireworks/routers/glm-5p1-fast",
		EmbedModel: "nomic-ai/nomic-embed-text-v1.5",

		ContextWindowTokens:   256000,
		SoftPct:               80,
		HardPct:               92,
		RetrievalTopK:         8,
		RetrievalBudgetTokens: 6000,
		PinnedBudgetTokens:    2000,
		RecallTopK:            6,
		RecallBudgetTokens:    2500,

		StepBudget:        50,
		NoProgressStall:   4,
		MaxRetriesPerTool: 3,
		MaxAdaptAttempts:  2,

		MinPatternSuccesses: 3,

		NaturalAllow: []string{
			"web_search", "git", "fetch_data", "write_code", "write_docs",
			"image_video_generation", "non_monetary_workflows",
			"scheduled_tasks", "onchain_reads", "shell", "long_lived_processes",
		},
		EscalateActions: []string{
			"send_value", "swap", "token_approve", "contract_deploy_gas",
			"fund_payment_stream", "fund_channel", "settle",
		},
	}
}

// Load returns Default(), overlaid with an optional runtime .kvx file (path
// may be empty or point at a missing file — both are non-fatal), then
// overlaid with environment variables.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		doc, ok, err := parseKVXFile(path)
		if err != nil {
			return c, err
		}
		if ok {
			c.applyDoc(doc)
		}
	}
	c.applyEnv()
	return c, nil
}

// applyDoc overlays a parsed runtime .kvx document onto c. Absent keys keep
// their current (default) value.
func (c *Config) applyDoc(d *kvxDoc) {
	if d.has("runtime") {
		c.AgentName = d.strOr("runtime", "agent_name", c.AgentName)
		c.CortexRoot = d.strOr("runtime", "cortex_root", c.CortexRoot)
		c.CortexActor = d.strOr("runtime", "cortex_actor", c.CortexActor)
		c.DaemonURL = d.strOr("runtime", "daemon_url", c.DaemonURL)
		c.ManifestPath = d.strOr("runtime", "manifest_path", c.ManifestPath)
		c.SkillsRoot = d.strOr("runtime", "skills_root", c.SkillsRoot)
		c.GatewayURL = d.strOr("runtime", "gateway_url", c.GatewayURL)
		c.ActorDID = d.strOr("runtime", "actor_did", c.ActorDID)
	}
	if d.has("models") {
		c.MainModel = d.strOr("models", "main", c.MainModel)
		c.CheapModel = d.strOr("models", "cheap", c.CheapModel)
		c.EmbedModel = d.strOr("models", "embed", c.EmbedModel)
	}
	if d.has("memory") {
		c.ContextWindowTokens = d.intOr("memory", "context_window_tokens", c.ContextWindowTokens)
		c.SoftPct = d.intOr("memory", "soft_pct", c.SoftPct)
		c.HardPct = d.intOr("memory", "hard_pct", c.HardPct)
		c.RetrievalTopK = d.intOr("memory", "retrieval_top_k", c.RetrievalTopK)
		c.RetrievalBudgetTokens = d.intOr("memory", "retrieval_budget_tokens", c.RetrievalBudgetTokens)
		c.PinnedBudgetTokens = d.intOr("memory", "pinned_budget_tokens", c.PinnedBudgetTokens)
		c.RecallTopK = d.intOr("memory", "recall_top_k", c.RecallTopK)
		c.RecallBudgetTokens = d.intOr("memory", "recall_budget_tokens", c.RecallBudgetTokens)
	}
	if d.has("loop") {
		c.StepBudget = d.intOr("loop", "step_budget", c.StepBudget)
		c.NoProgressStall = d.intOr("loop", "no_progress_stall", c.NoProgressStall)
		c.MaxRetriesPerTool = d.intOr("loop", "max_retries_per_tool", c.MaxRetriesPerTool)
		c.MaxAdaptAttempts = d.intOr("loop", "max_adapt_attempts", c.MaxAdaptAttempts)
	}
	if d.has("procedural") {
		c.MinPatternSuccesses = d.intOr("procedural", "min_pattern_successes", c.MinPatternSuccesses)
	}
	if d.has("execution") {
		if v := d.list("execution", "natural_allow"); v != nil {
			c.NaturalAllow = v
		}
		if v := d.list("execution", "escalate_actions"); v != nil {
			c.EscalateActions = v
		}
	}
}

// applyEnv overlays environment variables (highest precedence).
func (c *Config) applyEnv() {
	c.MainModel = envOr("NEO_MAIN_MODEL", c.MainModel)
	c.CheapModel = envOr("NEO_CHEAP_MODEL", c.CheapModel)
	c.EmbedModel = envOr("NEO_EMBED_MODEL", c.EmbedModel)
	c.CortexRoot = envOr("NEO_CORTEX_ROOT", c.CortexRoot)
	c.CortexActor = envOr("NEO_CORTEX_ACTOR", c.CortexActor)
	c.DaemonURL = envOr("NEO_DAEMON_URL", c.DaemonURL)
	c.ManifestPath = envOr("NEO_MANIFEST", c.ManifestPath)
	c.SkillsRoot = envOr("NEO_SKILLS_ROOT", c.SkillsRoot)
	c.ActorDID = envOr("NEO_ACTOR_DID", c.ActorDID)
	// MATRIX_GATEWAY_URL matches the daemon/router env key (router MachineEnv).
	c.GatewayURL = envOr("MATRIX_GATEWAY_URL", envOr("NEO_GATEWAY_URL", c.GatewayURL))

	if v := os.Getenv("NEO_CONTEXT_WINDOW_TOKENS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			c.ContextWindowTokens = n
		}
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// IsEscalateAction reports whether the named action crosses the wall into
// MCL (requires a user wallet signature) per the execution surface.
func (c Config) IsEscalateAction(action string) bool {
	for _, a := range c.EscalateActions {
		if a == action {
			return true
		}
	}
	return false
}

// SoftBudgetTokens / HardBudgetTokens convert the % thresholds into absolute
// token counts against the configured context window.
func (c Config) SoftBudgetTokens() int { return c.ContextWindowTokens * c.SoftPct / 100 }
func (c Config) HardBudgetTokens() int { return c.ContextWindowTokens * c.HardPct / 100 }
