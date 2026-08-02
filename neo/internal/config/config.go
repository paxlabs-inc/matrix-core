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

	"matrix/vault"
)

// Config is Neo's fully-resolved runtime configuration.
type Config struct {
	// --- identity / runtime wiring ---
	AgentName      string // human label, "Morpheus" (the market identity; "Neo" stays a valid configured name)
	CortexRoot     string // root dir of the cortex brain (per-actor stores live under it)
	CortexActor    string // actor scope for Neo's memory store
	DaemonURL      string // base URL of the MCL daemon for core_execute delegation
	ManifestPath   string // agent manifest declaring MCP servers (agents/default.json)
	SkillsRoot     string // skills corpus root (procedural-pattern promotion target)
	SelfModelGraph string

	// --- models (provider-qualified ids; see matrix/mcl/llm DetectProvider) ---
	MainModel  string // the conversational tool-calling loop
	CheapModel string // compaction + summary validation + cheap relation-classify
	// ConsolidationModel extracts durable learnings from each turn's transcript
	// into cortex (memory write-back). A stronger model than CheapModel because
	// extraction quality directly sets memory quality; still cheap-tier (it runs
	// once per turn in the background). Must be on the gateway neo-slot whitelist.
	ConsolidationModel string
	EmbedModel         string // semantic page-fault embeddings (gateway /v1/embeddings or direct provider)
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
	// ActivationBudgetTokens caps the per-turn cortex activation bundle (the
	// continuous-memory trailing tail: pinned + timeline + recent + transcript
	// slice + story-so-far). 0 = derive from the window via ActivationBudget()
	// — window-proportional so a 1M window carries a rich bundle while a 32K
	// local-model override keeps the old 3K behavior byte-identical.
	ActivationBudgetTokens int

	// FirstTurnRelevancePush injects a bounded RELEVANCE retrieval into the
	// activation window on the FIRST turn of a conversation only. cortex.Activate's
	// pushed tiers (Pinned/Timeline/Recent) are recency-based and query-INDEPENDENT
	// (activate.go: the query arg is unused), so on the opening message the agent
	// carries recency but NOT relevance-to-the-ask unless it reactively calls
	// memory_recall. With this on, Neo runs pager.Retrieve(query) once on turn 1
	// and injects the hits, so the opening turn has relevance-matched memory with
	// no reactive tool call. After turn 1 the pull-not-push philosophy resumes.
	FirstTurnRelevancePush bool
	// WarmOnOpen makes the FIRST inbound HTTP request spin up the embedder + HNSW
	// semantic substrate in the background (one-shot). Activate + StorySoFar are
	// already cheap (materialized scans, deterministic, no LLM); the only real
	// cold-start is the embedder/HNSW that the relevance push depends on, so
	// warming it on app-open keeps the first message's recall off the cold path.
	WarmOnOpen bool

	// --- hello-world session substrate (all flags default off) ---
	// SessionCurrentIntent replaces the set-once standing objective with the
	// latest real user request plus explicit persistent goals and open loops.
	// When false, the legacy activeGoal path remains byte-identical.
	SessionCurrentIntent   bool
	SessionExactProjection bool
	InteractionPosture     bool

	// --- loop discipline ---
	StepBudget      int // max tool-call iterations per turn (anti-infinite); the configured ceiling
	StepBudgetMin   int // P2-7: adaptive floor. 0 = adaptation disabled (effective budget = StepBudgetMax, today's fixed behavior). When >0 the effective budget scales within [StepBudgetMin, StepBudgetMax] based on turn complexity.
	StepBudgetMax   int // P2-7: adaptive ceiling. Defaults to StepBudget (50) so behavior is unchanged when adaptation is off.
	NoProgressStall int // identical-failing-call / no-state-change count that trips a stall
	// SemanticStallSimilarityPct is the word-overlap threshold (0–100) at which
	// two consecutive tool-call batches at the SAME operation (same tool shape +
	// same target identifiers) are treated as a no-progress repeat even when
	// their argument WORDING differs — so a cosmetic reword cannot defeat the
	// stall guard. The repeat counter still trips at NoProgressStall. 0 disables
	// semantic detection (exact batch-signature matching only — legacy behavior).
	SemanticStallSimilarityPct int
	MaxRetriesPerTool          int // recovery ladder rung 1: bounded retries for transient failures
	MaxAdaptAttempts           int // recovery ladder rung 2: bounded approach revisions
	// MaxGuidanceNudges caps how many CONSECUTIVE system-guidance nudges the
	// loop may inject (completion-gate rejections, read-full steers) before it
	// escalates to an honest stop-and-ask instead of re-nudging indefinitely
	// (neo-smoothness req.1.5). The counter resets on genuine progress (a tool
	// dispatch / accepted completion). 0 disables the cap (unbounded nudging).
	MaxGuidanceNudges int

	// --- sub-agent swarm (task-scoped concurrent helpers; see [swarm]) ---
	MaxSubagents           int // hard cap on sub-agents spawned in one spawn_subagents call
	MaxConcurrentSubagents int // semaphore: how many sub-agents run at once (the rest queue)
	SubagentStepBudget     int // per-sub-agent tool-call iteration budget (smaller than the parent's)

	// --- browser filmstrip (BROWSER-FILMSTRIP; see [browser] / NEO_BROWSER_*) ---
	// BrowserAutoshot deterministically captures a viewport JPEG after each
	// view-changing browser action (navigate/back/click/form submit) so the
	// "Neo's Computer" browsing surface reads as a live filmstrip without
	// relying on the model choosing to screenshot. Default ON; a capture failure
	// never blocks the navigation or the turn (best-effort). BrowserAutoshotMax
	// caps captures per run so a long browsing turn can't grow media without
	// bound. MediaRetentionHours bounds screenshot/media growth on the volume:
	// the GC sweep deletes served media files older than this (0 disables).
	BrowserAutoshot     bool
	BrowserAutoshotMax  int
	MediaRetentionHours int

	// --- voice (VOICE; [voice] / NEO_VOICE_*) ---
	VoiceEnabled        bool
	VoiceMode           string
	VoiceASRDeadline    time.Duration
	VoiceTTSVoice       string
	VoiceTTSStyle       string
	VoiceTTSDeadline    time.Duration
	VoiceIdleDisconnect time.Duration

	// --- task supervisor (durability: a dispatched task runs to completion
	// across model errors, tool failures, early loop-ends, user disconnects,
	// and even daemon restart/suspend — at least one agent stays on it until
	// the objective is met to standard; see the Task Durability Rule) ---
	SuperviseTasks     bool          // master switch: wrap each turn in the persistent supervisor
	TaskMaxWall        time.Duration // hard wall-clock ceiling for one supervised task (generous; anti-runaway)
	TaskAttemptTimeout time.Duration // ceiling for a single supervised attempt (one agent run)
	TaskMaxRespawns    int           // max fresh-agent respawns before delivering an honest partial

	// DeathConsolidateEvery is the bounded cadence for the self-authoring
	// consolidation pass (self-model task 3.2, req.5.2): after this many recorded
	// loop deaths, the agent reads its death journal and writes/reinforces durable
	// how-I-fail failure-pattern memories, deduplicating recurrences of the same
	// mode into ONE reinforced belief. It runs on this cadence, NEVER every turn;
	// <= 0 disables the pass.
	DeathConsolidateEvery int

	// --- procedural memory guards ---
	MinPatternSuccesses int // successes required before a candidate pattern is injected

	// --- cassandra 2.0 (the silent-voice controller) ---
	// Cassandra 2.0 replaces the retired proof-of-work completion gate with a
	// silent, first-person feedback controller that edits the agent's OWN prior
	// assistant-message content in place (folding in Doubt / Questioning /
	// Curiosity / Assurance / Urge-to-verify) so the model reads it as its own
	// emerging thought. It is a two-sided damping loop and a self-healing loop
	// killer; it is SILENT on a clean run. These knobs gate it. Config:
	// [cassandra] block / NEO_CASSANDRA_* env.
	//
	// CassandraEnabled is the master switch: when false the controller is a
	// COMPLETE no-op and the window is byte-identical to the pre-feature loop.
	CassandraEnabled bool
	// CassandraMinStep is the first loop step at which a mod may fire — there is
	// no prior assistant turn to edit before step 2 (default 2).
	CassandraMinStep int
	// CassandraMaxModsPerTurn caps total modifications in one Chat turn (anti-nag;
	// default 3).
	CassandraMaxModsPerTurn int
	// CassandraLoopThreshold is the repeat count that arms the doubt side, chosen
	// to fire ONE step BEFORE the hard no-progress stall (NoProgressStall). 0 (the
	// default) derives it as NoProgressStall-1 so it tracks the stall bound when
	// unset; a positive value overrides.
	CassandraLoopThreshold int
	// CassandraCooldownSteps is the minimum number of steps between mods of the
	// same trigger class, so the controller never nags the same drift every step
	// (default 2).
	CassandraCooldownSteps int

	// --- epistemic core (EPISTEMIC-CORE; [epistemic] block / NEO_EPISTEMIC_*) ---
	// EpistemicPremises arms Mechanism 1 (premise ledger + check-before-act
	// gate): a plan's load-bearing premises are extracted with provenance and
	// no action dispatches on a cheaply-checkable unverified assumption.
	EpistemicPremises bool
	// EpistemicPredictions arms Mechanism 3 (prediction-carrying dispatch):
	// probe-class tool calls state an expectation; mismatch is a first-class
	// belief-update event.
	EpistemicPredictions bool
	// EpistemicMismatchLimit is the consecutive per-strategy mismatch count
	// that forces a tools-stripped revision step (default 3).
	EpistemicMismatchLimit int
	// EpistemicConvergenceWindow is how many consecutive actions without
	// evidence growth count as measured non-convergence (default 4).
	EpistemicConvergenceWindow int

	// O1ConstrainTools gates Architect O1's closed-world capability narrowing
	// (req.3 O1-CAP): when true, each turn advertises only the task-contract's
	// minimal selected tool subset (o1.CompileRuntimeCapabilities). When false
	// (the DEFAULT) Neo advertises its FULL bound tool surface every turn — the
	// O1 manifests/verifiers/ledger/preflight still compile over that full
	// surface, but no tool is hidden from the model. Default off because the
	// contract's request->capability mapping is a text heuristic that silently
	// strips essential tools (e.g. fetch) whenever the request does not name
	// the server alias verbatim, leaving the model unable to act. Config:
	// [o1] block / env NEO_O1_CONSTRAIN_TOOLS.
	O1ConstrainTools bool

	// --- heartbeat (P1-4: Chronos-driven proactive turn convention) ---
	// HeartbeatInterval is the recurring-alarm interval (minutes) for Neo's
	// proactive self-review of active goals/constraints. 0 = disabled (the
	// default: no wakeups occur). When >0, a Chronos recurring alarm fires at
	// this interval; its wake_message tells Neo to review active goals and
	// reply with the HEARTBEAT_OK sentinel when nothing needs attention (the
	// turn is then suppressed — no user-facing output). Config: [heartbeat]
	// section in neo.frozen.kvx / env NEO_HEARTBEAT_INTERVAL.
	HeartbeatInterval int

	// --- MCP tool result cache + parallel dispatch (P2-5) ---
	// MCPCacheTTL is the TTL for the idempotent-read result cache. Zero
	// disables caching (the default). When >0, successful results for
	// explicitly-idempotent read tools (web_search, web_news) are served
	// from cache within the TTL; side-effecting and money tools are never
	// cached. Config: env NEO_MCP_CACHE_TTL_SECONDS.
	MCPCacheTTL time.Duration
	// ToolDispatchConcurrency bounds how many INDEPENDENT tool calls in one
	// turn dispatch concurrently. Zero disables parallel dispatch (calls run
	// serially, the legacy behaviour). Results are always assembled in call
	// order regardless of completion order (determinism, i6). Config: env
	// NEO_TOOL_DISPATCH_CONCURRENCY.
	ToolDispatchConcurrency int

	// --- automatrix (proactive surprise tasks; see [automatrix] / NEO_AUTOMATRIX_*) ---
	// AutomatrixEnabled is the build-level master switch; a per-user opt-in
	// (stored in the daemon settings) is STILL required before Neo wakes to
	// work. AutomatrixBaseIntervalMinutes is the base wake cadence (0 =
	// disabled); AutomatrixJitterMinutes is the ± randomization window that
	// gives wakes their "random" feel; AutomatrixMaxTasksPerDay caps proactive
	// tasks per day (anti-spam); AutomatrixMinConfidence is the capture floor an
	// opportunity must clear. NtfyServer/NtfyTopic and AppriseURL configure the
	// completion-ping Notifier (a blank NtfyTopic is derived from ActorDID).
	AutomatrixEnabled             bool
	AutomatrixBaseIntervalMinutes int
	AutomatrixJitterMinutes       int
	AutomatrixMaxTasksPerDay      int
	AutomatrixMinConfidence       float32
	NtfyServer                    string
	NtfyTopic                     string
	AppriseURL                    string

	// --- execution surface ---
	NaturalAllow    []string // reversible actions Neo performs directly (no wallet signature)
	EscalateActions []string // actions that cross into MCL (require a user wallet signature)

	// --- LLM transport ---
	GatewayURL      string // optional metered-LLM gateway (empty = direct provider)
	ActorDID        string // actor DID stamped on gateway calls
	AgentRuntime    string
	RuntimeProvider RuntimeProviderConfig

	// --- data-at-rest vault (set programmatically by runServe after
	// vault.Boot, never from env; nil = legacy plaintext dev/CLI) ---
	Vault     *vault.Session // fail-closed sealing session for every store incl. cortex
	VaultUser string         // user DID bound into sealed objects' associated data
}

type RuntimeProviderConfig struct {
	GatewayURL     string
	BearerEnv      string
	MaxAttempts    int
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	IdleTimeout    time.Duration
}

// Default returns Neo's defaults, encoding the frozen spec's locked
// operational contract (neo/neo.frozen.kvx).
func Default() Config {
	return Config{
		AgentName:      "Morpheus",
		CortexRoot:     "/root/.cortex",
		CortexActor:    "neo",
		DaemonURL:      "http://127.0.0.1:8080",
		ManifestPath:   "agents/default.json",
		SkillsRoot:     "skills",
		SelfModelGraph: "graph/self-model",

		MainModel:          "mimo-v2.5-pro",
		CheapModel:         "mimo-v2.5-pro",
		ConsolidationModel: "mimo-v2.5-pro",
		EmbedModel:         "nomic-ai/nomic-embed-text-v1.5",
		// Cassandra completeness auditor: a cheap/fast primary + a stronger
		// escalation model, both on the gateway cassandra-slot whitelist
		// (gateway rates.FreeTierWhitelist "cassandra").
		CassandraModel:         "grok-4.20-0309-non-reasoning",
		CassandraEscalateModel: "grok-4.3",

		ContextWindowTokens:   1000000,
		SoftPct:               80,
		HardPct:               92,
		RetrievalTopK:         8,
		RetrievalBudgetTokens: 6000,
		AmbientRetrievalTopK:  5,
		PinnedBudgetTokens:    2000,
		RecallTopK:            6,
		RecallBudgetTokens:    2500,

		// Q2 (memory warm + relevance push). Both ON: the opening message
		// carries relevance-matched memory (not just recency) with no reactive
		// memory_recall, and the embedder/HNSW cold-start is paid on app-open.
		FirstTurnRelevancePush: true,
		WarmOnOpen:             true,
		SessionCurrentIntent:   false,
		SessionExactProjection: false,
		InteractionPosture:     false,

		StepBudget:                 50,
		StepBudgetMin:              0,  // P2-7: adaptation disabled by default (effective budget = 50, today's behavior)
		StepBudgetMax:              50, // P2-7: ceiling equals StepBudget so the band is [0,50] when disabled
		NoProgressStall:            4,
		SemanticStallSimilarityPct: 50, // catch reworded retries at the same operation
		MaxRetriesPerTool:          3,
		MaxAdaptAttempts:           2,
		MaxGuidanceNudges:          3, // consecutive guidance nudges before stop-and-ask (neo-smoothness req.1.5)

		MaxSubagents:           8,
		MaxConcurrentSubagents: 4,
		SubagentStepBudget:     40,

		// Browser filmstrip: auto-capture ON (a clean turn that never browses is
		// byte-identical to disabled). 40 stills per run is generous for a real
		// browsing session yet bounds media growth; the volume GC keeps 7 days of
		// stills (parallel to the trace retain window).
		BrowserAutoshot:     true,
		BrowserAutoshotMax:  40,
		MediaRetentionHours: 168,

		VoiceEnabled:        false,
		VoiceMode:           "native",
		VoiceASRDeadline:    30 * time.Second,
		VoiceTTSVoice:       "Mia",
		VoiceTTSStyle:       "Warm, direct, conversational delivery.",
		VoiceTTSDeadline:    30 * time.Second,
		VoiceIdleDisconnect: 120 * time.Second,

		// Task durability: ON by default. The ceilings are generous-but-finite
		// (the user chose "max persistence" — effectively no practical limit —
		// but a hard backstop must exist so a wedged task can't burn metered
		// spend forever).
		SuperviseTasks:        true,
		TaskMaxWall:           6 * time.Hour,
		TaskAttemptTimeout:    20 * time.Minute,
		TaskMaxRespawns:       50,
		DeathConsolidateEvery: 3, // self-author how-I-fail memories every 3rd loop death

		MinPatternSuccesses: 3,

		// Cassandra 2.0 silent-voice controller. ON by default (a clean run is
		// byte-identical to disabled, so it costs nothing when healthy). min_step
		// 2 (no prior turn to edit at step 1), max 3 mods/turn (anti-nag),
		// loop_threshold 0 = derive NoProgressStall-1 (fire one step before the
		// hard stall), cooldown 2 steps per trigger class.
		CassandraEnabled:        true,
		CassandraMinStep:        2,
		CassandraMaxModsPerTurn: 3,
		CassandraLoopThreshold:  0,
		CassandraCooldownSteps:  2,

		// Epistemic core: both mechanisms ON by default (a clean, grounded run
		// renders nothing and dispatches nothing differently); bounds per the
		// design table (mismatch 3, convergence window 4).
		EpistemicPremises:          true,
		EpistemicPredictions:       true,
		EpistemicMismatchLimit:     3,
		EpistemicConvergenceWindow: 4,

		// Automatrix (proactive surprise tasks). Default OFF; capture still runs
		// regardless of this switch so the opportunity queue is warm, but Neo
		// never wakes to work until both this and the per-user opt-in are on.
		// Cadence/jitter/cap/floor and the ntfy server default per the design
		// table; NtfyTopic stays blank (derived from ActorDID at notify time)
		// and AppriseURL is unset (optional fan-out).
		AutomatrixEnabled:             false,
		AutomatrixBaseIntervalMinutes: 45,
		AutomatrixJitterMinutes:       30,
		AutomatrixMaxTasksPerDay:      3,
		AutomatrixMinConfidence:       0.6,
		NtfyServer:                    "https://ntfy.sh",
		NtfyTopic:                     "",
		AppriseURL:                    "",

		// P2-5: parallel dispatch of independent tool calls in a turn.
		// Bounded so a batch of N calls doesn't fork-bomb MCP servers.
		// 0 would mean serial (legacy); 4 matches MaxConcurrentSubagents.
		ToolDispatchConcurrency: 4,

		RuntimeProvider: RuntimeProviderConfig{
			BearerEnv:      "MATRIX_GATEWAY_TOKEN",
			MaxAttempts:    3,
			BackoffInitial: 250 * time.Millisecond,
			BackoffMax:     2 * time.Second,
			IdleTimeout:    120 * time.Second,
		},
		AgentRuntime: "legacy",

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

// Sandbox returns a hermetic local-dev preset that wires a throwaway temp
// cortex, the existing Hash embedder stub, a no-op chain RPC (empty
// DaemonURL), and metering disabled (empty GatewayURL) — by composing the
// EXISTING override points (Config struct fields) only. No new runtime
// paths are introduced; every field set here is one a kvx profile or env
// var could already set.
//
// The preset composes Default() and overrides the hermetic-surface fields
// on the returned copy. Default() itself is never mutated.
//
// Launch command (one command, zero external deps):
//
//	neo serve -sandbox
//
// or for the MCL executor daemon:
//
//	mcl-execute daemon -sandbox
//
// The Hash embedder stub is selected because pickEmbedder() (see
// neo/internal/memory/embedder.go) falls through to embed.NewHashEmbedder()
// when GatewayURL is empty and no provider API key env is set — both
// guaranteed by this preset.
func Sandbox() Config {
	c := Default()
	c.AgentName = "Morpheus (sandbox)"
	// Throwaway cortex root under the OS temp dir so no production brain
	// is touched. os.MkdirAll is deferred to the caller (memory.Open /
	// newInfra) which already creates the path; here we only name it.
	c.CortexRoot = os.TempDir() + "/matrix-sandbox-cortex"
	c.CortexActor = "sandbox"
	// No live chain / core_execute delegation: blank the daemon URL so
	// delegate calls short-circuit rather than dialing 127.0.0.1:8080.
	c.DaemonURL = ""
	// Metering disabled: blank the gateway URL + actor DID. This also
	// forces pickEmbedder() to select the Hash stub (gateway branch
	// requires GatewayURL + token + ActorDID; the direct provider branch
	// requires BASETEN_API_KEY env — both absent in a sandbox run).
	c.GatewayURL = ""
	c.RuntimeProvider.GatewayURL = ""
	c.ActorDID = ""
	// Sentinel embed model so callers/tests can confirm the stub was
	// selected. pickEmbedder() maps this to the Hash embedder because the
	// gateway path is off (empty GatewayURL); the value is otherwise only
	// a label (the Hash embedder ignores it).
	c.EmbedModel = "hash-stub"
	return c
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
		c.SelfModelGraph = d.strOr("runtime", "self_model_graph", c.SelfModelGraph)
		c.GatewayURL = d.strOr("runtime", "gateway_url", c.GatewayURL)
		c.ActorDID = d.strOr("runtime", "actor_did", c.ActorDID)
	}
	if d.has("runtime.provider") {
		c.RuntimeProvider.GatewayURL = d.strOr("runtime.provider", "gateway_url", c.RuntimeProvider.GatewayURL)
		c.RuntimeProvider.BearerEnv = d.strOr("runtime.provider", "bearer_env", c.RuntimeProvider.BearerEnv)
		c.RuntimeProvider.MaxAttempts = d.intOr("runtime.provider", "max_attempts", c.RuntimeProvider.MaxAttempts)
		if milliseconds := d.intOr("runtime.provider", "backoff_initial_ms", 0); milliseconds > 0 {
			c.RuntimeProvider.BackoffInitial = time.Duration(milliseconds) * time.Millisecond
		}
		if milliseconds := d.intOr("runtime.provider", "backoff_max_ms", 0); milliseconds > 0 {
			c.RuntimeProvider.BackoffMax = time.Duration(milliseconds) * time.Millisecond
		}
		if seconds := d.intOr("runtime.provider", "idle_timeout_seconds", 0); seconds > 0 {
			c.RuntimeProvider.IdleTimeout = time.Duration(seconds) * time.Second
		}
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
		c.ActivationBudgetTokens = d.intOr("memory", "activation_budget_tokens", c.ActivationBudgetTokens)
		c.FirstTurnRelevancePush = d.boolOr("memory", "first_turn_relevance_push", c.FirstTurnRelevancePush)
		c.WarmOnOpen = d.boolOr("memory", "warm_on_open", c.WarmOnOpen)
	}
	if d.has("session") {
		c.SessionCurrentIntent = d.boolOr("session", "current_intent", c.SessionCurrentIntent)
		c.SessionExactProjection = d.boolOr("session", "exact_projection", c.SessionExactProjection)
		c.InteractionPosture = d.boolOr("session", "interaction_posture", c.InteractionPosture)
	}
	if d.has("loop") {
		c.StepBudget = d.intOr("loop", "step_budget", c.StepBudget)
		c.StepBudgetMin = d.intOr("loop", "step_budget_min", c.StepBudgetMin)
		c.StepBudgetMax = d.intOr("loop", "step_budget_max", c.StepBudgetMax)
		c.NoProgressStall = d.intOr("loop", "no_progress_stall", c.NoProgressStall)
		c.SemanticStallSimilarityPct = d.intOr("loop", "semantic_stall_similarity_pct", c.SemanticStallSimilarityPct)
		c.MaxRetriesPerTool = d.intOr("loop", "max_retries_per_tool", c.MaxRetriesPerTool)
		c.MaxAdaptAttempts = d.intOr("loop", "max_adapt_attempts", c.MaxAdaptAttempts)
		c.MaxGuidanceNudges = d.intOr("loop", "max_guidance_nudges", c.MaxGuidanceNudges)
	}
	if d.has("swarm") {
		c.MaxSubagents = d.intOr("swarm", "max_subagents", c.MaxSubagents)
		c.MaxConcurrentSubagents = d.intOr("swarm", "max_concurrent_subagents", c.MaxConcurrentSubagents)
		c.SubagentStepBudget = d.intOr("swarm", "subagent_step_budget", c.SubagentStepBudget)
	}
	if d.has("supervisor") {
		c.SuperviseTasks = d.boolOr("supervisor", "supervise_tasks", c.SuperviseTasks)
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
	if d.has("cassandra") {
		c.CassandraEnabled = d.boolOr("cassandra", "enabled", c.CassandraEnabled)
		c.CassandraMinStep = d.intOr("cassandra", "min_step", c.CassandraMinStep)
		c.CassandraMaxModsPerTurn = d.intOr("cassandra", "max_mods_per_turn", c.CassandraMaxModsPerTurn)
		c.CassandraLoopThreshold = d.intOr("cassandra", "loop_threshold", c.CassandraLoopThreshold)
		c.CassandraCooldownSteps = d.intOr("cassandra", "cooldown_steps", c.CassandraCooldownSteps)
	}
	if d.has("voice") {
		c.VoiceEnabled = d.boolOr("voice", "enabled", c.VoiceEnabled)
		c.VoiceMode = voiceModeOr(d.strOr("voice", "mode", c.VoiceMode), c.VoiceMode)
		if sec := d.intOr("voice", "asr_deadline_seconds", 0); sec > 0 {
			c.VoiceASRDeadline = time.Duration(sec) * time.Second
		}
		c.VoiceTTSVoice = d.strOr("voice", "tts_voice", c.VoiceTTSVoice)
		c.VoiceTTSStyle = d.strOr("voice", "tts_style", c.VoiceTTSStyle)
		if sec := d.intOr("voice", "tts_deadline_seconds", 0); sec > 0 {
			c.VoiceTTSDeadline = time.Duration(sec) * time.Second
		}
		if sec := d.intOr("voice", "idle_disconnect_seconds", 0); sec > 0 {
			c.VoiceIdleDisconnect = time.Duration(sec) * time.Second
		}
	}
	if d.has("epistemic") {
		c.EpistemicPremises = d.boolOr("epistemic", "premises", c.EpistemicPremises)
		c.EpistemicPredictions = d.boolOr("epistemic", "predictions", c.EpistemicPredictions)
		c.EpistemicMismatchLimit = d.intOr("epistemic", "mismatch_limit", c.EpistemicMismatchLimit)
		c.EpistemicConvergenceWindow = d.intOr("epistemic", "convergence_window", c.EpistemicConvergenceWindow)
	}
	if d.has("o1") {
		c.O1ConstrainTools = d.boolOr("o1", "constrain_tools", c.O1ConstrainTools)
	}
	if d.has("heartbeat") {
		c.HeartbeatInterval = d.intOr("heartbeat", "interval_minutes", c.HeartbeatInterval)
	}
	if d.has("automatrix") {
		c.AutomatrixEnabled = d.boolOr("automatrix", "enabled", c.AutomatrixEnabled)
		c.AutomatrixBaseIntervalMinutes = d.intOr("automatrix", "base_interval_minutes", c.AutomatrixBaseIntervalMinutes)
		c.AutomatrixJitterMinutes = d.intOr("automatrix", "jitter_minutes", c.AutomatrixJitterMinutes)
		c.AutomatrixMaxTasksPerDay = d.intOr("automatrix", "max_tasks_per_day", c.AutomatrixMaxTasksPerDay)
		c.AutomatrixMinConfidence = float32(d.floatOr("automatrix", "min_confidence", float64(c.AutomatrixMinConfidence)))
		c.NtfyServer = d.strOr("automatrix", "ntfy_server", c.NtfyServer)
		c.NtfyTopic = d.strOr("automatrix", "ntfy_topic", c.NtfyTopic)
		c.AppriseURL = d.strOr("automatrix", "apprise_url", c.AppriseURL)
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
	c.SelfModelGraph = envOr("NEO_SELF_MODEL_GRAPH", c.SelfModelGraph)
	c.ActorDID = envOr("NEO_ACTOR_DID", c.ActorDID)
	// MATRIX_GATEWAY_URL matches the daemon/router env key (router MachineEnv).
	c.GatewayURL = envOr("MATRIX_GATEWAY_URL", envOr("NEO_GATEWAY_URL", c.GatewayURL))
	c.AgentRuntime = strings.ToLower(
		strings.TrimSpace(envOr("NEO_RUNTIME", c.AgentRuntime)),
	)
	c.RuntimeProvider.GatewayURL = envOr("MATRIX_GATEWAY_URL", c.RuntimeProvider.GatewayURL)
	c.RuntimeProvider.BearerEnv = envOr("NEO_RUNTIME_PROVIDER_BEARER_ENV", c.RuntimeProvider.BearerEnv)
	c.RuntimeProvider.MaxAttempts = envInt("NEO_RUNTIME_PROVIDER_MAX_ATTEMPTS", c.RuntimeProvider.MaxAttempts)
	if milliseconds := envInt("NEO_RUNTIME_PROVIDER_BACKOFF_INITIAL_MS", 0); milliseconds > 0 {
		c.RuntimeProvider.BackoffInitial = time.Duration(milliseconds) * time.Millisecond
	}
	if milliseconds := envInt("NEO_RUNTIME_PROVIDER_BACKOFF_MAX_MS", 0); milliseconds > 0 {
		c.RuntimeProvider.BackoffMax = time.Duration(milliseconds) * time.Millisecond
	}
	if seconds := envInt("NEO_RUNTIME_PROVIDER_IDLE_TIMEOUT_SECONDS", 0); seconds > 0 {
		c.RuntimeProvider.IdleTimeout = time.Duration(seconds) * time.Second
	}

	if v := os.Getenv("NEO_CONTEXT_WINDOW_TOKENS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			c.ContextWindowTokens = n
		}
	}
	c.ActivationBudgetTokens = envInt("NEO_ACTIVATION_BUDGET_TOKENS", c.ActivationBudgetTokens)
	c.MaxSubagents = envInt("NEO_MAX_SUBAGENTS", c.MaxSubagents)
	c.MaxConcurrentSubagents = envInt("NEO_MAX_CONCURRENT_SUBAGENTS", c.MaxConcurrentSubagents)
	c.SubagentStepBudget = envInt("NEO_SUBAGENT_STEP_BUDGET", c.SubagentStepBudget)

	// Browser filmstrip (BROWSER-FILMSTRIP). Auto-capture defaults ON; the cap
	// and retention accept 0 (0 cap = no cap; 0 retention = GC disabled), so they
	// use the non-negative variant rather than envInt (which rejects 0).
	c.BrowserAutoshot = envBool("NEO_BROWSER_AUTOSHOT", c.BrowserAutoshot)
	c.BrowserAutoshotMax = envIntNonNeg("NEO_BROWSER_AUTOSHOT_MAX", c.BrowserAutoshotMax)
	c.MediaRetentionHours = envIntNonNeg("NEO_MEDIA_RETENTION_HOURS", c.MediaRetentionHours)
	c.VoiceEnabled = envBool("NEO_VOICE_ENABLED", c.VoiceEnabled)
	c.VoiceMode = voiceModeOr(os.Getenv("NEO_VOICE_MODE"), c.VoiceMode)
	if sec := envInt("NEO_VOICE_ASR_DEADLINE_SECONDS", 0); sec > 0 {
		c.VoiceASRDeadline = time.Duration(sec) * time.Second
	}
	c.VoiceTTSVoice = envOr("NEO_VOICE_TTS_VOICE", c.VoiceTTSVoice)
	c.VoiceTTSStyle = envOr("NEO_VOICE_TTS_STYLE", c.VoiceTTSStyle)
	if sec := envInt("NEO_VOICE_TTS_DEADLINE_SECONDS", 0); sec > 0 {
		c.VoiceTTSDeadline = time.Duration(sec) * time.Second
	}
	if sec := envInt("VOICE_IDLE_DISCONNECT_S", 0); sec > 0 {
		c.VoiceIdleDisconnect = time.Duration(sec) * time.Second
	}

	// P2-7: adaptive step budget. StepBudgetMin accepts 0 (adaptation
	// disabled — the default); StepBudgetMax accepts 0 only to mean "use
	// StepBudget as the ceiling" (so an unset env keeps today's behavior).
	c.StepBudgetMin = envIntNonNeg("NEO_STEP_BUDGET_MIN", c.StepBudgetMin)
	c.StepBudgetMax = envIntNonNeg("NEO_STEP_BUDGET_MAX", c.StepBudgetMax)
	// SemanticStallSimilarityPct accepts 0 (disabled), so it uses the
	// non-negative variant rather than envInt (which rejects 0).
	c.SemanticStallSimilarityPct = envIntNonNeg("NEO_SEMANTIC_STALL_SIMILARITY_PCT", c.SemanticStallSimilarityPct)
	// MaxGuidanceNudges accepts 0 (cap disabled — unbounded nudging), so it
	// uses the non-negative variant.
	c.MaxGuidanceNudges = envIntNonNeg("NEO_MAX_GUIDANCE_NUDGES", c.MaxGuidanceNudges)

	// Task supervisor (durability).
	c.SuperviseTasks = envBool("NEO_SUPERVISE_TASKS", c.SuperviseTasks)
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
	// HeartbeatInterval accepts 0 (disabled), so it uses the non-negative
	// variant (P1-4). No wakeups occur when 0.
	c.HeartbeatInterval = envIntNonNeg("NEO_HEARTBEAT_INTERVAL", c.HeartbeatInterval)
	// Q2 memory warm + relevance push (default on; set to false to disable).
	c.FirstTurnRelevancePush = envBool("NEO_FIRST_TURN_RELEVANCE_PUSH", c.FirstTurnRelevancePush)
	c.WarmOnOpen = envBool("NEO_WARM_ON_OPEN", c.WarmOnOpen)
	c.SessionCurrentIntent = envBool("NEO_SESSION_CURRENT_INTENT", c.SessionCurrentIntent)
	c.SessionExactProjection = envBool("NEO_SESSION_EXACT_PROJECTION", c.SessionExactProjection)
	c.InteractionPosture = envBool("NEO_INTERACTION_POSTURE", c.InteractionPosture)

	// Automatrix (proactive surprise tasks). Interval/jitter/cap accept 0
	// (interval 0 = disabled, jitter 0 = no randomization) so they use the
	// non-negative variant. NTFY_SERVER/NTFY_TOPIC/APPRISE_URL match the design
	// table's env keys (no NEO_ prefix — they are conventional ntfy/Apprise vars).
	c.AutomatrixEnabled = envBool("NEO_AUTOMATRIX_ENABLED", c.AutomatrixEnabled)
	c.AutomatrixBaseIntervalMinutes = envIntNonNeg("NEO_AUTOMATRIX_INTERVAL", c.AutomatrixBaseIntervalMinutes)
	c.AutomatrixJitterMinutes = envIntNonNeg("NEO_AUTOMATRIX_JITTER", c.AutomatrixJitterMinutes)
	c.AutomatrixMaxTasksPerDay = envIntNonNeg("NEO_AUTOMATRIX_MAX_PER_DAY", c.AutomatrixMaxTasksPerDay)
	c.AutomatrixMinConfidence = envFloat32("NEO_AUTOMATRIX_MIN_CONFIDENCE", c.AutomatrixMinConfidence)
	c.NtfyServer = envOr("NTFY_SERVER", c.NtfyServer)
	c.NtfyTopic = envOr("NTFY_TOPIC", c.NtfyTopic)
	c.AppriseURL = envOr("APPRISE_URL", c.AppriseURL)

	// P2-5: MCP result cache TTL (seconds). 0 = disabled.
	if v := envIntNonNeg("NEO_MCP_CACHE_TTL_SECONDS", 0); v > 0 {
		c.MCPCacheTTL = time.Duration(v) * time.Second
	}
	// P2-5: parallel dispatch of independent tool calls. 0 = serial.
	c.ToolDispatchConcurrency = envIntNonNeg("NEO_TOOL_DISPATCH_CONCURRENCY", c.ToolDispatchConcurrency)

	// Cassandra 2.0 silent-voice controller. enabled is a plain bool switch.
	// min_step / max_mods_per_turn / cooldown_steps are positive counts (envInt
	// rejects 0, keeping the default). loop_threshold accepts 0 (the "derive
	// NoProgressStall-1" sentinel), so it uses the non-negative variant.
	c.CassandraEnabled = envBool("NEO_CASSANDRA_ENABLED", c.CassandraEnabled)
	c.CassandraMinStep = envInt("NEO_CASSANDRA_MIN_STEP", c.CassandraMinStep)
	c.CassandraMaxModsPerTurn = envInt("NEO_CASSANDRA_MAX_MODS", c.CassandraMaxModsPerTurn)
	c.CassandraLoopThreshold = envIntNonNeg("NEO_CASSANDRA_LOOP_THRESHOLD", c.CassandraLoopThreshold)
	c.CassandraCooldownSteps = envInt("NEO_CASSANDRA_COOLDOWN", c.CassandraCooldownSteps)

	// Epistemic core (EPISTEMIC-CORE req.8.1): both mechanisms honestly
	// disableable; the bounds are positive counts (envInt rejects 0).
	c.EpistemicPremises = envBool("NEO_EPISTEMIC_PREMISES", c.EpistemicPremises)
	c.EpistemicPredictions = envBool("NEO_EPISTEMIC_PREDICTIONS", c.EpistemicPredictions)
	c.EpistemicMismatchLimit = envInt("NEO_EPISTEMIC_MISMATCH_LIMIT", c.EpistemicMismatchLimit)
	c.EpistemicConvergenceWindow = envInt("NEO_EPISTEMIC_CONVERGENCE_WINDOW", c.EpistemicConvergenceWindow)

	// Architect O1 closed-world tool narrowing is OFF by default (full surface);
	// NEO_O1_CONSTRAIN_TOOLS=true re-arms the minimal contract selection.
	c.O1ConstrainTools = envBool("NEO_O1_CONSTRAIN_TOOLS", c.O1ConstrainTools)
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

func voiceModeOr(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "native":
		return "native"
	case "asr_first":
		return "asr_first"
	default:
		return fallback
	}
}

// envFloat32 overlays a float32 from the environment, keeping the fallback when
// the var is absent or not a valid float.
func envFloat32(key string, fallback float32) float32 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			return float32(f)
		}
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

// Headroom caps for the budget derivation below: at large windows a percentage
// threshold reserves an absurd absolute slice (80% of 1M leaves 200K tokens
// idle), so the reserved headroom is the SMALLER of the percentage-derived
// slice and these absolute caps. Below ~320K windows the caps never bind, so
// small local-model configurations (32K via NEO_CONTEXT_WINDOW_TOKENS) keep
// their exact prior trim/compaction behavior.
const (
	softHeadroomCapTokens = 64000
	hardHeadroomCapTokens = 32000
)

// SoftBudgetTokens / HardBudgetTokens derive the compaction thresholds from
// the configured context window: the percentage-derived budget, raised to
// window-minus-capped-headroom when the percentage would reserve more than the
// absolute cap — so trimming fires only near true pressure at 1M scale while
// small windows keep the exact percentage behavior.
func (c Config) SoftBudgetTokens() int {
	return budgetTokens(c.ContextWindowTokens, c.SoftPct, softHeadroomCapTokens)
}

func (c Config) HardBudgetTokens() int {
	return budgetTokens(c.ContextWindowTokens, c.HardPct, hardHeadroomCapTokens)
}

// Bounds for the derived activation-bundle budget (ActivationBudget below).
// The floor is the historical cortex default (small windows keep their exact
// prior behavior); the ceiling stays under cortex.MaxActivateBudgetTokens.
const (
	activationBudgetFloorTokens = 3000
	activationBudgetCapTokens   = 24000
)

// ActivationBudget returns the per-turn cortex activation-bundle token budget.
// An explicit ActivationBudgetTokens wins verbatim; otherwise it derives
// window-proportionally (2% of the context window) clamped to
// [activationBudgetFloorTokens, activationBudgetCapTokens] — 1M → 20000,
// 256K → 5120, and any window ≤ 150K keeps the historical 3000.
func (c Config) ActivationBudget() int {
	if c.ActivationBudgetTokens > 0 {
		return c.ActivationBudgetTokens
	}
	b := c.ContextWindowTokens / 50
	if b < activationBudgetFloorTokens {
		b = activationBudgetFloorTokens
	}
	if b > activationBudgetCapTokens {
		b = activationBudgetCapTokens
	}
	return b
}

func budgetTokens(window, pct, headroomCap int) int {
	b := window * pct / 100
	if capped := window - headroomCap; capped > b {
		b = capped
	}
	return b
}
