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
	"time"
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
	CheapModel string // compaction + summary validation + cheap relation-classify
	// ConsolidationModel extracts durable learnings from each turn's transcript
	// into cortex (memory write-back). A stronger model than CheapModel because
	// extraction quality directly sets memory quality; still cheap-tier (it runs
	// once per turn in the background). Must be on the gateway neo-slot whitelist.
	ConsolidationModel string
	EmbedModel string // semantic page-fault embeddings (gateway /v1/embeddings or direct provider)
	// CassandraModel is the cheap/fast Cassandra completeness auditor (the
	// completion-gate adjudicator, metered on the dedicated "cassandra" slot);
	// CassandraEscalateModel is the stronger second-opinion model consulted on
	// low-certainty high-stakes audits ("" disables escalation). Both must be on
	// the gateway's cassandra-slot whitelist.
	CassandraModel         string
	CassandraEscalateModel string

	// --- memory budget (context window = RAM; cortex = disk) ---
	ContextWindowTokens   int // total model context window, for budget math
	SoftPct               int // cooperative compaction threshold (finish atomic step, then compact)
	HardPct               int // forced compaction threshold (runaway backstop)
	RetrievalTopK         int // page-fault: top-K cortex records per retrieval
	RetrievalBudgetTokens int // token ceiling for retrieved records
	// AmbientRetrievalTopK caps the ambient (push) memory seed injected into
	// the system block each turn (v3 #1: reasoning-time retrieval). 0 = fully
	// tool-driven (no forced seed and no mid-turn refault — the model pulls
	// with memory_recall); N = a thin top-N seed plus the pinned tier. The
	// pinned tier (identity, hard rules, learned guidance, active goal, user
	// profile) is ALWAYS injected and is unaffected by this knob.
	AmbientRetrievalTopK int
	PinnedBudgetTokens   int // token ceiling for the always-injected pinned block
	RecallTopK           int // conversational recall: top-K relevant past turns per turn
	RecallBudgetTokens   int // token ceiling for the recalled past-turns block

	// --- loop discipline ---
	StepBudget        int // max tool-call iterations per turn (anti-infinite)
	NoProgressStall   int // identical-failing-call / no-state-change count that trips a stall
	MaxRetriesPerTool int // recovery ladder rung 1: bounded retries for transient failures
	MaxAdaptAttempts  int // recovery ladder rung 2: bounded approach revisions

	// --- sub-agent swarm (task-scoped concurrent helpers; see [swarm]) ---
	MaxSubagents           int // hard cap on sub-agents spawned in one spawn_subagents call
	MaxConcurrentSubagents int // semaphore: how many sub-agents run at once (the rest queue)
	SubagentStepBudget     int // per-sub-agent tool-call iteration budget (smaller than the parent's)

	// --- task supervisor (durability: a dispatched task runs to completion
	// across model errors, tool failures, early loop-ends, user disconnects,
	// and even daemon restart/suspend — at least one agent stays on it until
	// the objective is met to standard; see the Task Durability Rule) ---
	SuperviseTasks     bool          // master switch: wrap each turn in the persistent supervisor
	GateAllWork        bool          // route substantial reversible deliverables through the completion gate too (not just money/chain turns)
	TaskMaxWall        time.Duration // hard wall-clock ceiling for one supervised task (generous; anti-runaway)
	TaskAttemptTimeout time.Duration // ceiling for a single supervised attempt (one agent run)
	TaskMaxRespawns    int           // max fresh-agent respawns before delivering an honest partial

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

		MainModel:          "deepseek-ai/DeepSeek-V4-Pro",
		CheapModel:          "accounts/fireworks/routers/glm-5p1-fast",
		ConsolidationModel:  "accounts/fireworks/models/deepseek-v4-flash",
		EmbedModel: "nomic-ai/nomic-embed-text-v1.5",
		// Cassandra completeness auditor: a cheap/fast primary + a stronger
		// escalation model, both on the gateway cassandra-slot whitelist
		// (gateway rates.FreeTierWhitelist "cassandra").
		CassandraModel:         "accounts/fireworks/models/deepseek-v4-flash",
		CassandraEscalateModel: "accounts/fireworks/models/deepseek-v4-pro",

		ContextWindowTokens:   256000,
		SoftPct:               80,
		HardPct:               92,
		RetrievalTopK:         8,
		RetrievalBudgetTokens: 6000,
		AmbientRetrievalTopK:  5,
		PinnedBudgetTokens:    2000,
		RecallTopK:            6,
		RecallBudgetTokens:    2500,

		StepBudget:        50,
		NoProgressStall:   4,
		MaxRetriesPerTool: 3,
		MaxAdaptAttempts:  2,

		MaxSubagents:           8,
		MaxConcurrentSubagents: 4,
		SubagentStepBudget:     40,

		// Task durability: ON by default. The ceilings are generous-but-finite
		// (the user chose "max persistence" — effectively no practical limit —
		// but a hard backstop must exist so a wedged task can't burn metered
		// spend forever). GateAllWork makes every substantial deliverable prove
		// completeness (not just money/chain turns) — "highest standard".
		SuperviseTasks:     true,
		GateAllWork:        true,
		TaskMaxWall:        6 * time.Hour,
		TaskAttemptTimeout: 20 * time.Minute,
		TaskMaxRespawns:    50,

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
		c.ConsolidationModel = d.strOr("models", "consolidation", c.ConsolidationModel)
		c.EmbedModel = d.strOr("models", "embed", c.EmbedModel)
		c.CassandraModel = d.strOr("models", "cassandra", c.CassandraModel)
		c.CassandraEscalateModel = d.strOr("models", "cassandra_escalate", c.CassandraEscalateModel)
	}
	if d.has("memory") {
		c.ContextWindowTokens = d.intOr("memory", "context_window_tokens", c.ContextWindowTokens)
		c.SoftPct = d.intOr("memory", "soft_pct", c.SoftPct)
		c.HardPct = d.intOr("memory", "hard_pct", c.HardPct)
		c.RetrievalTopK = d.intOr("memory", "retrieval_top_k", c.RetrievalTopK)
		c.RetrievalBudgetTokens = d.intOr("memory", "retrieval_budget_tokens", c.RetrievalBudgetTokens)
		c.AmbientRetrievalTopK = d.intOr("memory", "ambient_retrieval_top_k", c.AmbientRetrievalTopK)
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
	if d.has("swarm") {
		c.MaxSubagents = d.intOr("swarm", "max_subagents", c.MaxSubagents)
		c.MaxConcurrentSubagents = d.intOr("swarm", "max_concurrent_subagents", c.MaxConcurrentSubagents)
		c.SubagentStepBudget = d.intOr("swarm", "subagent_step_budget", c.SubagentStepBudget)
	}
	if d.has("supervisor") {
		c.SuperviseTasks = d.boolOr("supervisor", "supervise_tasks", c.SuperviseTasks)
		c.GateAllWork = d.boolOr("supervisor", "gate_all_work", c.GateAllWork)
		if m := d.intOr("supervisor", "task_max_wall_minutes", 0); m > 0 {
			c.TaskMaxWall = time.Duration(m) * time.Minute
		}
		if m := d.intOr("supervisor", "task_attempt_timeout_minutes", 0); m > 0 {
			c.TaskAttemptTimeout = time.Duration(m) * time.Minute
		}
		c.TaskMaxRespawns = d.intOr("supervisor", "task_max_respawns", c.TaskMaxRespawns)
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
	c.ConsolidationModel = envOr("NEO_CONSOLIDATION_MODEL", c.ConsolidationModel)
	c.EmbedModel = envOr("NEO_EMBED_MODEL", c.EmbedModel)
	c.CassandraModel = envOr("NEO_CASSANDRA_MODEL", c.CassandraModel)
	c.CassandraEscalateModel = envOr("NEO_CASSANDRA_ESCALATE_MODEL", c.CassandraEscalateModel)
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
	c.MaxSubagents = envInt("NEO_MAX_SUBAGENTS", c.MaxSubagents)
	c.MaxConcurrentSubagents = envInt("NEO_MAX_CONCURRENT_SUBAGENTS", c.MaxConcurrentSubagents)
	c.SubagentStepBudget = envInt("NEO_SUBAGENT_STEP_BUDGET", c.SubagentStepBudget)

	// Task supervisor (durability).
	c.SuperviseTasks = envBool("NEO_SUPERVISE_TASKS", c.SuperviseTasks)
	c.GateAllWork = envBool("NEO_GATE_ALL_WORK", c.GateAllWork)
	if m := envInt("NEO_TASK_MAX_WALL_MINUTES", 0); m > 0 {
		c.TaskMaxWall = time.Duration(m) * time.Minute
	}
	if m := envInt("NEO_TASK_ATTEMPT_TIMEOUT_MINUTES", 0); m > 0 {
		c.TaskAttemptTimeout = time.Duration(m) * time.Minute
	}
	c.TaskMaxRespawns = envInt("NEO_TASK_MAX_RESPAWNS", c.TaskMaxRespawns)
	// AmbientRetrievalTopK accepts 0 (fully tool-driven), so it uses the
	// non-negative variant rather than envInt (which rejects 0).
	c.AmbientRetrievalTopK = envIntNonNeg("NEO_AMBIENT_RETRIEVAL_TOP_K", c.AmbientRetrievalTopK)
}

// envInt overlays a positive integer from the environment, keeping the
// fallback when the var is absent or not a positive integer.
func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// envIntNonNeg overlays a non-negative integer from the environment, keeping
// the fallback when the var is absent or not a non-negative integer. Unlike
// envInt it accepts 0 — used for knobs where zero is a meaningful setting
// (e.g. AmbientRetrievalTopK=0 = fully tool-driven retrieval).
func envIntNonNeg(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envBool overlays a boolean from the environment, keeping the fallback when
// the var is absent or unrecognized. Truthy: 1/true/yes/on; falsy:
// 0/false/no/off (case-insensitive).
func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
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
