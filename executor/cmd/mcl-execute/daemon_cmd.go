// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_cmd.go — `mcl-execute daemon` subcommand entry point.
//
// Boots one *infra (manifest → MCP → registry → cortex + embedder),
// loads the actor identity once, starts the HTTP+SSE server, and
// installs a graceful-shutdown handler so SIGINT/SIGTERM drains
// in-flight requests, flushes Pebble, stops MCP servers, and exits 0.
//
// Strategy A (matrix.kvx sess#24 lock): one daemon = one user.
// Single-flight invariant: at most one in-flight intent at a time
// (cortex single-writer). Concurrent /messages calls receive 409 Busy.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"matrix/executor/internal/snapshot"
)

// daemonState owns every long-lived dependency the daemon shares
// across messages. One instance per process.
type daemonState struct {
	infra           *infra
	actor           *actorIdentity
	skillsRoot      string
	journalDir      string
	transcriptsDir  string
	defaultSkillURI string
	compilerModel   string
	executorModel   string
	// plannerModel decouples plan synthesis from the executor knob (v1
	// launch 2026-06-01). Empty -> synthMod() falls back to executorModel,
	// so single-knob deployments are unchanged. Set via -planner-model /
	// MATRIX_PLANNER_MODEL.
	plannerModel string
	// compilerEscalateModel is the stronger compiler model compile()
	// re-invokes on a low-confidence (or invalid-verb) frame. Empty
	// disables escalation. Must be on the gateway compiler-slot whitelist.
	// Set via -compiler-escalate-model / MATRIX_COMPILER_ESCALATE_MODEL.
	compilerEscalateModel string
	// compileConfidenceThreshold is the frame-confidence floor below which
	// compile escalates; 0 falls back to compile.go's default (0.75).
	compileConfidenceThreshold float64
	seed                       int64
	maxRetry                   int
	// criticEnabled turns on the completeness critic + re-plan gate
	// (Phase 10.5): after a clean walk, an LLM auditor checks whether the
	// executed work satisfied every requested deliverable; unmet items
	// trigger a bounded re-plan (DriveCorrectMaterial → re-synthesize →
	// re-walk) and a still-incomplete run NEVER attests as success. Set via
	// -completeness-critic / MATRIX_COMPLETENESS_CRITIC (default on; "0"
	// disables). Off (struct zero) for direct-constructed test daemons, so
	// existing pipeline tests are unaffected.
	criticEnabled bool
	// criticModel overrides the auditor LLM (else criticMod() falls back to
	// the planner/executor model). Set via -critic-model / MATRIX_CRITIC_MODEL.
	criticModel string
	// maxReplan bounds critic-driven re-plan rounds (default 2).
	maxReplan int
	broker    *sseBroker
	startedAt time.Time

	// allowSubDispatch enables in-process sub-dispatch (Q6 v1
	// carve-out). When false, the walker uses
	// runtime.NotImplementedSubDispatch and any sub_dispatch node
	// in a synthesized plan terminates the walk with
	// ErrSubDispatchNotImplemented. When true, the walker recursively
	// synthesizes + walks the resolved sub-skill in the SAME process
	// using the same intent envelope chain (cross-agent / CortexScope
	// proof handoff is v1.1).
	allowSubDispatch bool

	// workspaceRoot is the absolute path the agent's MCP fs/git
	// servers are scoped to (e.g. /data/workspace in the daemon
	// container). Threaded into the synthesizer's system prompt so
	// the executor LLM emits valid `path` / `repo_path` args instead
	// of hallucinating paths the MCP servers will reject.
	workspaceRoot string

	// authToken is the bearer token required on every privileged
	// endpoint. Empty disables auth (local-dev only).
	authToken string

	// busy enforces single-flight on /messages so cortex stays
	// single-writer. Use TryLock for non-blocking 409 semantics.
	busy sync.Mutex

	// --- sess#27 additions: feature-rich route surface ---

	// jwtSecret is the Supabase HS256 shared secret for daemon-side
	// JWT signature re-verification (Lock #B defense in depth).
	// Populated from MATRIX_SUPABASE_JWT_SECRET env var when the
	// daemon boots; nil when local-dev mode (verify-on-presence,
	// no-op when X-Matrix-JWT header absent).
	jwtSecret []byte

	// boundUserID is the supabase user id this daemon is dedicated
	// to (one daemon = one user invariant). When set, every
	// X-Matrix-User header MUST match this value. Empty disables
	// the check (CLI / multi-user smoke).
	boundUserID string

	// asyncReg tracks every /messages/async run so /intents/:id/
	// summary, /intents/:id/cancel, and /messages/async/:id can
	// surface lifecycle without re-walking the journal each time.
	asyncReg *asyncRegistry

	// convStore is the durable per-conversation turn log (user message
	// + the agent's closing answer), persisted on the volume keyed by
	// conversation_id. It gives the Liaison front door real multi-turn
	// memory: triage and the closing answer recall recent turns, so a
	// follow-up like "maybe try paxscan" is understood in the context
	// of the prior request instead of being treated as turn #1. It is a
	// side-channel store — like the Liaison itself it NEVER writes
	// cortex, so it cannot perturb the D11 replay byte-identity
	// invariant. nil disables conversation memory.
	convStore *conversationStore

	// asyncCurrentIntent stores the intent_id of the goroutine that
	// currently holds d.busy (atomic.Value of string). Empty when no
	// async request is in-flight. Read by daemon_pipeline.go to
	// decide whether to use the httpGateHandler vs the legacy stdin
	// gate handler.
	asyncCurrentIntent atomic.Value

	// gateBroker registers walker-blocked gate prompts so POST
	// /intents/:id/gates/:nid/answer can resolve them.
	gateBroker *gateBroker

	// gateTimeout caps how long a httpGateHandler waits for a user
	// answer before auto-denying. Default 24h.
	gateTimeout time.Duration

	// indexCache holds the journal-walk-derived intent summaries.
	indexCache   *indexCache
	skillCatalog *skillCatalog
	snapMgr      *snapshot.Manager

	// sess#29: rate limiter for POST /memory. Lazy-initialised on first
	// access via memoryWriteLimiter() so legacy code paths and tests
	// don't have to thread an init step.
	memWriteLimMu sync.Mutex
	memWriteLim   *memoryWriteLimiter

	// sess#29: LLM endpoint override. Empty falls back to per-provider
	// default endpoint inside MCL/llm. Set by the Tauri shell after the
	// wizard, OR by CLI -llm-base-url.
	llmBaseURL string

	// sess#32 ambient-architect MatrixGateway routing (plan §5.16).
	// gatewayURL is the host portion of the gateway (e.g.
	// "https://matrix.paxeer.app/gw"); empty disables gateway
	// routing and the daemon falls back to the legacy direct-
	// provider posture (llmBaseURL or per-provider default).
	// actorDID is the matrix://user/<did> the daemon represents,
	// stamped on every routed LLM call as X-Matrix-Actor-DID.
	gatewayURL string
	actorDID   string

	// sess#31d (P4): per-route latency histograms + audit counters.
	// Shared across all per-message transcripts so /metrics serves
	// daemon-wide aggregates. nil before AttachMetrics; never
	// repopulated after boot.
	metrics *routerMetrics

	// liaison, when non-nil, enables the Liaison conversational agent
	// (daemon_liaison.go): per-run natural-language narration + the
	// /chat front door. Set at boot unless -liaison-disable. nil leaves
	// the pipeline silent (legacy posture): the narrator never spawns
	// and /chat returns 503. Pure side-channel — never touches cortex,
	// envelopes, or the plan/walk, so it cannot perturb D11 replay.
	liaison *liaisonState

	// sess#34 / Forge Phase 1: filesystem allow/deny policy for the
	// Forge HTTP surface (GET /fs/tree, GET /fs/read, POST /fs/write).
	// nil when the daemon is NOT running in Forge mode — the routes
	// 404 unmounted. Set by runDaemon when -forge-mode is true (or
	// when a Forge agent manifest is loaded; deferred to sess#35).
	forgeFS *ForgeFSPolicy

	// sess#36 / Forge Phase 3: git operation allow/deny policy for the
	// Forge HTTP surface (/git/status, /git/diff, /git/branch,
	// /git/merge). nil when not in Forge mode — the routes 404 unmounted
	// alongside the /fs/* surface. Set by runDaemon when -forge-mode is
	// true.
	gitOps *GitOpsPolicy

	// sess#36 / Forge Phase 3: PTY shell config for the Forge WebSocket
	// surface (WS /shell/exec). nil when not in Forge mode — the routes
	// 404 unmounted. Set by runDaemon when -forge-mode is true.
	shellCfg *ForgeShellConfig

	// Gideon (plan todos: engine / ops-tools / scheduler). When
	// gideonMode is true, /messages + /messages/async dispatch to the
	// compiler-bypass runMessageDirect (daemon_gideon_pipeline.go), the
	// GideonOpsPolicy hard guardrails gate/deny risky tool calls, and
	// the reasoning scheduler (gideon_scheduler.go) runs. Additive:
	// every existing path (including -forge-mode) is unchanged when
	// gideonMode is false. gideonPolicy is immutable post-boot so it is
	// safe to share across the scheduler + HTTP goroutines.
	gideonMode          bool
	gideonPolicy        *GideonOpsPolicy
	gideonSweepInterval time.Duration

	// Paxeer chain-spend gating (public-user runtime). Evaluated on the
	// synthesized plan BEFORE any tool dispatch (daemon_pipeline.go
	// Phase 7.5) so any paxeer-net write whose value-arg exceeds the
	// per-call cap, OR whose aggregate plan spend exceeds the
	// aggregate cap, gates through the existing gateBroker. nil
	// disables the policy entirely (still safe — bridge-side
	// PAXEER_MAX_SPEND_WEI + custody API enforce their own caps). Set
	// unconditionally at boot so every per-user daemon picks it up.
	paxeerSpend *PaxeerSpendPolicy
}

// memoryWriteLimiter returns the lazy-initialised limiter for POST /memory.
func (d *daemonState) memoryWriteLimiter() *memoryWriteLimiter {
	d.memWriteLimMu.Lock()
	defer d.memWriteLimMu.Unlock()
	if d.memWriteLim == nil {
		d.memWriteLim = newMemoryWriteLimiter(memoryWriteRateLimit, memoryWriteRateWindow)
	}
	return d.memWriteLim
}

// synthMod returns the model the planner (plan synthesis) slot should
// use: the dedicated plannerModel when set, else the executorModel knob
// (v1 launch decoupling with single-knob back-compat). An empty result
// lets synthesize() fall through to llm.DefaultPlannerModel().
func (d *daemonState) synthMod() string {
	if d.plannerModel != "" {
		return d.plannerModel
	}
	return d.executorModel
}

// runDaemon parses flags, boots dependencies, launches the HTTP server,
// and blocks until SIGINT/SIGTERM. Exits non-zero on boot failure;
// graceful shutdown returns 0.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	var (
		addr                  = fs.String("addr", ":8080", "HTTP listen address")
		manifestPath          = fs.String("manifest", "/root/matrix/agents/default.json", "agent manifest path")
		skillsRoot            = fs.String("skills-root", "/root/matrix/skills", "skill repository root")
		cortexRoot            = fs.String("cortex-root", "", "cortex storage root (REQUIRED)")
		cortexActor           = fs.String("cortex-actor", "executor", "cortex actor namespace")
		journalDir            = fs.String("journal-dir", "/root/matrix/journal/logs", "envelope journal directory")
		transcriptsDir        = fs.String("transcripts-dir", "/root/matrix/journal/logs/_transcripts", "per-message JSONL transcript directory")
		keyfile               = fs.String("keyfile", "/root/matrix/.matrix/executor.key", "ed25519 seed path (created if absent)")
		didLabel              = fs.String("did", "executor", "DID label suffix")
		defaultSkill          = fs.String("skill-default", "", "default matrix://skill/<slug>@<v> when /messages omits skill")
		compilerModel         = fs.String("compiler-model", "", "override compiler LLM model")
		executorModel         = fs.String("executor-model", "", "override executor LLM model")
		plannerModel          = fs.String("planner-model", os.Getenv("MATRIX_PLANNER_MODEL"), "override planner (plan-synthesis) LLM model, decoupled from -executor-model (v1 launch). Empty falls back to -executor-model so single-knob deployments are unchanged. Defaults to env MATRIX_PLANNER_MODEL.")
		compilerEscalateModel = fs.String("compiler-escalate-model", os.Getenv("MATRIX_COMPILER_ESCALATE_MODEL"), "stronger compiler model re-invoked when a compile self-reports low confidence or an invalid verb. Empty disables escalation. Must be on the gateway compiler-slot whitelist. Defaults to env MATRIX_COMPILER_ESCALATE_MODEL.")
		compileConfThreshold  = fs.Float64("compile-confidence-threshold", 0, "frame-confidence floor (0..1) below which the compiler escalates; 0 uses the built-in default (0.75)")
		liaisonModel          = fs.String("liaison-model", os.Getenv("MATRIX_LIAISON_MODEL"), "override the Liaison (user-facing conversational narrator) LLM model. Empty uses the SlotLiaison registry default (deepseek-v4-flash). Must be on the gateway liaison-slot whitelist. Defaults to env MATRIX_LIAISON_MODEL.")
		liaisonDisable        = fs.Bool("liaison-disable", os.Getenv("MATRIX_LIAISON_DISABLE") == "1", "disable the Liaison conversational agent: no per-run narration and POST /chat returns 503. Defaults to env MATRIX_LIAISON_DISABLE==1.")
		llmBaseURL            = fs.String("llm-base-url", "", "override LLM endpoint base URL (e.g. https://matrix.paxeer.app/gw for MatrixGateway, or https://api.fireworks.ai/inference for BYO). Empty falls back to MCL/llm's per-provider default. Path '/v1/chat/completions' is appended by the client.")
		gatewayURL            = fs.String("gateway-url", os.Getenv("MATRIX_GATEWAY_URL"), "MatrixGateway base URL (host portion, no /v1/...). When set, EVERY routed LLM call (compile, planner, executor) is proxied through ${gateway-url}/v1/chat/completions with X-Matrix-Actor-DID + X-Matrix-Intent-ID + X-Matrix-Slot headers and is metered against the credit_ledger. Empty (default) preserves the legacy direct-provider posture. Sess#32 ambient-architect plan §5.16. Defaults to env MATRIX_GATEWAY_URL.")
		seed                  = fs.Int64("seed", 42, "compiler seed (D11)")
		maxRetry              = fs.Int("max-retry", 2, "max plan-synthesis retry rounds")
		criticEnabled         = fs.Bool("completeness-critic", os.Getenv("MATRIX_COMPLETENESS_CRITIC") != "0",
			"enable the completeness critic + re-plan gate (Phase 10.5): after a clean walk, an LLM auditor verifies every requested deliverable was actually produced; unmet items trigger a bounded re-plan and a still-incomplete run is reported as failed, never falsely 'completed'. Default on; set MATRIX_COMPLETENESS_CRITIC=0 to disable.")
		criticModel = fs.String("critic-model", os.Getenv("MATRIX_CRITIC_MODEL"),
			"override the completeness-critic auditor LLM. Empty falls back to the planner/executor model. Routes on the gateway planner slot, so an override must be planner-slot whitelisted.")
		maxReplan = fs.Int("max-replan", 2, "max critic-driven re-plan rounds before a still-incomplete run is failed honestly")
		allowSubDisp          = fs.Bool("allow-sub-dispatch", false, "enable in-process sub-dispatch (Q6 v1 carve-out: same agent, in-process only)")
		workspaceRoot         = fs.String("workspace-root", "", "absolute path agents' MCP fs/git servers are scoped to (announced to the synthesizer prompt; empty = section omitted)")
		withEmbedder          = fs.Bool("with-embedder", false, "start cortex hash embedder (in-process)")
		withFireworks         = fs.Bool("with-fireworks-embedder", false, "start cortex Fireworks embedder (REAL)")
		bootTimeout           = fs.Duration("boot-timeout", 2*time.Minute, "infra boot timeout")
		shutdownTimeout       = fs.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown drain budget")
		bufferSize            = fs.Int("sse-buffer", 256, "per-subscriber SSE buffer size")
		// sess#34 / Forge Phase 1: enables GET /fs/tree, GET /fs/read,
		// POST /fs/write with the Q3=c allowlist (full RW under
		// /root/matrix EXCEPT cortex/store + knowledge + journal).
		// Defaults to env MATRIX_FORGE_MODE=1.
		forgeMode = fs.Bool("forge-mode", os.Getenv("MATRIX_FORGE_MODE") == "1",
			"enable Forge HTTP filesystem routes (/fs/tree, /fs/read, /fs/write) under the default Forge allow/deny policy (matrix.kvx sess#34 Q3=c)")
		// Gideon ops-agent mode (plan todos: engine / ops-tools /
		// scheduler). Flips the compiler-bypass pipeline, the
		// GideonOpsPolicy guardrails, and the reasoning scheduler.
		// Defaults to env MATRIX_GIDEON_MODE==1. Mutually exclusive with
		// -forge-mode in practice; if both are set, gideon wins.
		gideonMode = fs.Bool("gideon-mode", os.Getenv("MATRIX_GIDEON_MODE") == "1",
			"enable Gideon ops-agent mode: compiler-bypass pipeline (runMessageDirect), GideonOpsPolicy hard guardrails, and the reasoning scheduler. Default manifest becomes agents/gideon.json. Gideon wins if -forge-mode is also set. Defaults to env MATRIX_GIDEON_MODE==1.")
		gideonSweep = fs.Duration("gideon-sweep-interval", 5*time.Minute,
			"Gideon scheduler sweep interval; <=0 disables the reasoning loop (gideon-mode only)")
		// Snapshot S3 backup wiring (matrix.kvx S25Q6). All flags are
		// optional; when -snapshot-endpoint is empty the daemon runs
		// without snapshot pull/push (local-dev posture).
		snapDataDir      = fs.String("snapshot-data-dir", "", "snapshot tarball root (default: parent of -cortex-root)")
		snapEndpoint     = fs.String("snapshot-endpoint", "", "S3-compatible endpoint (e.g. http://[fdaa:75:8960:...]:9000)")
		snapBucket       = fs.String("snapshot-bucket", "matrix-state", "S3 bucket name")
		snapUserID       = fs.String("snapshot-user-id", "", "snapshot key namespace under users/<id>/ (default: -cortex-actor)")
		snapInterval     = fs.Duration("snapshot-interval", snapshot.DefaultPushInterval, "periodic push interval; <0 disables ticker (boot+shutdown only)")
		snapKeyEnv       = fs.String("snapshot-access-key-env", "MATRIX_S3_KEY", "env var holding the S3 access key")
		snapSecretEnv    = fs.String("snapshot-secret-key-env", "MATRIX_S3_SECRET", "env var holding the S3 secret key")
		snapBootDeadline = fs.Duration("snapshot-boot-timeout", 60*time.Second, "boot-pull deadline before falling back to fresh-start")
		// sess#27 additions for the feature-rich frontend route surface.
		boundUser    = fs.String("bound-user", "", "supabase user id this daemon is bound to (X-Matrix-User must match)")
		jwtSecretEnv = fs.String("jwt-secret-env", "MATRIX_SUPABASE_JWT_SECRET", "env var holding the Supabase HS256 secret for daemon-side jwt verify")
		gateTimeout  = fs.Duration("gate-timeout", 24*time.Hour, "default deadline for human-in-loop gate prompts (overridable per gate)")
		asyncMaxJobs = fs.Int("async-max-jobs", defaultMaxAsyncJobs, "max retained async-job entries in registry (oldest terminal evicted on overflow)")
		// Paxeer chain-spend gating (public-user runtime). Plan-time
		// belt-and-suspenders gate over the bridge-side
		// PAXEER_MAX_SPEND_WEI + the custody API's own caps. Caps in
		// wei (decimal). Empty / 0 disables that layer of the policy.
		// Negative ("-1") disables the policy entirely. Defaults to
		// 1 PAX per call, 5 PAX per plan; envs PAXEER_SPEND_CAP_WEI +
		// PAXEER_AGG_CAP_WEI override.
		paxeerCapEnv  = os.Getenv("PAXEER_SPEND_CAP_WEI")
		paxeerAggEnv  = os.Getenv("PAXEER_AGG_CAP_WEI")
		paxeerCapStr  = fs.String("paxeer-cap-wei", paxeerCapEnv, "per-call paxeer-net write spend cap in wei (decimal). Empty = default (1 PAX). '-1' disables the policy entirely. Env: PAXEER_SPEND_CAP_WEI.")
		paxeerAggStr  = fs.String("paxeer-aggregate-cap-wei", paxeerAggEnv, "aggregate plan spend cap across paxeer-net writes in wei (decimal). Empty = default (5 PAX). '0' disables the aggregate gate while keeping per-call. Env: PAXEER_AGG_CAP_WEI.")
		paxeerDisable = fs.Bool("paxeer-spend-policy-disable", os.Getenv("PAXEER_SPEND_POLICY_DISABLE") == "1", "fully disable PaxeerSpendPolicy plan-time gating (bridge-side + custody-side caps remain in force). Env: PAXEER_SPEND_POLICY_DISABLE=1.")
	)
	fs.Parse(args)

	if *cortexRoot == "" {
		fatalf("daemon: -cortex-root is required")
	}

	// Gideon mode wins over forge mode if both are set (don't crash),
	// and swaps the default manifest to agents/gideon.json when the
	// operator didn't pin one explicitly. Done BEFORE infra boot so
	// newInfra loads the right manifest.
	if *gideonMode {
		*forgeMode = false
		if *manifestPath == "/root/matrix/agents/default.json" {
			*manifestPath = "/root/matrix/agents/gideon.json"
		}
	}

	if err := os.MkdirAll(*transcriptsDir, 0o755); err != nil {
		fatalf("daemon: mkdir transcripts-dir: %v", err)
	}

	// Daemon-level transcript: stderr-only mirror; per-message
	// transcripts open under transcripts-dir/<intent_id>.jsonl.
	t, err := openTranscript("")
	if err != nil {
		fatalf("daemon: open transcript: %v", err)
	}
	defer t.Close()

	bootCtx, cancelBoot := context.WithTimeout(context.Background(), *bootTimeout)
	defer cancelBoot()

	t.Event("daemon.boot.start", "boot", map[string]interface{}{
		"addr":         *addr,
		"cortex_root":  *cortexRoot,
		"cortex_actor": *cortexActor,
		"manifest":     *manifestPath,
		"skill_dft":    *defaultSkill,
	})

	// Construct snapshot manager (no network IO yet) and run boot-pull
	// BEFORE infra opens the cortex Pebble DB, so a freshly-attached
	// Volume restores its previous tree before any reader sees it.
	//
	// Defaults: DataDir = parent(cortexRoot); UserID = cortexActor.
	var snapMgr *snapshot.Manager
	if *snapEndpoint != "" {
		dataDir := *snapDataDir
		if dataDir == "" {
			dataDir = filepath.Dir(*cortexRoot)
		}
		uid := *snapUserID
		if uid == "" {
			uid = *cortexActor
		}
		snapCfg := snapshot.Config{
			DataDir:      dataDir,
			Endpoint:     *snapEndpoint,
			Bucket:       *snapBucket,
			AccessKey:    os.Getenv(*snapKeyEnv),
			SecretKey:    os.Getenv(*snapSecretEnv),
			UserID:       uid,
			PushInterval: *snapInterval,
			Logf: func(event string, fields map[string]interface{}) {
				t.Event(event, "snapshot", fields)
			},
		}
		mgr, sErr := snapshot.New(snapCfg)
		if sErr != nil {
			fatalf("daemon: snapshot.New: %v", sErr)
		}
		snapMgr = mgr

		pullCtx, cancelPull := context.WithTimeout(bootCtx, *snapBootDeadline)
		pulled, pErr := snapMgr.BootPull(pullCtx)
		cancelPull()
		switch {
		case pErr == nil:
			t.Event("snapshot.boot.done", "boot", map[string]interface{}{
				"pulled": pulled,
			})
		case errors.Is(pErr, snapshot.ErrNoSnapshot):
			t.Event("snapshot.boot.fresh", "boot", nil)
		default:
			// Fail-closed: BootPull only reaches this branch when the volume
			// is UNSEEDED and the pull genuinely errored. Continuing would
			// boot on empty state and mint a fresh identity key
			// (identity.go loadOrCreateIdentity), silently forking the user's
			// DID and losing their cortex. Refuse to start; Fly's
			// restart-on-failure policy retries the pull on the next boot.
			// (A new user with no prior snapshot takes the ErrNoSnapshot
			// branch above, not this one.)
			t.Event("snapshot.boot.error", "boot", map[string]interface{}{
				"error": pErr.Error(),
			})
			fatalf("daemon: snapshot boot-pull failed on unseeded volume; refusing to start to avoid identity/data fork: %v", pErr)
		}
	} else {
		t.Event("snapshot.disabled", "boot", map[string]interface{}{
			"reason": "no_snapshot_endpoint_flag",
		})
	}

	in, err := newInfra(bootCtx, infraOpts{
		ManifestPath:       *manifestPath,
		CortexRoot:         *cortexRoot,
		CortexActor:        *cortexActor,
		WithEmbedder:       *withEmbedder,
		WithFireworksEmbed: *withFireworks,
		StderrSink:         os.Stderr,
	}, t)
	if err != nil {
		fatalf("daemon: infra: %v", err)
	}
	defer in.Close()

	actor, err := loadOrCreateIdentity(*keyfile, *didLabel)
	if err != nil {
		fatalf("daemon: identity: %v", err)
	}
	t.Event("identity.loaded", "boot", map[string]interface{}{
		"did":  actor.DID,
		"user": actor.UserURI,
	})

	broker := newSSEBroker(*bufferSize)
	t.AttachBroker(broker)

	// Router metrics accumulator (Session 31d · P4). Attached to the
	// daemon-level transcript so every routed-LLM call site
	// downstream (compile, synth, step_handler) records latency
	// observations into the same shared histogram. The /metrics
	// endpoint reads from it directly via t.Metrics().
	metrics := newRouterMetrics()
	t.AttachMetrics(metrics)
	t.Event("router.metrics.attached", "boot", map[string]interface{}{
		"buckets_ms": histogramBucketsMs,
	})

	state := &daemonState{
		infra:                      in,
		actor:                      actor,
		skillsRoot:                 *skillsRoot,
		journalDir:                 *journalDir,
		transcriptsDir:             *transcriptsDir,
		defaultSkillURI:            *defaultSkill,
		compilerModel:              *compilerModel,
		executorModel:              *executorModel,
		plannerModel:               *plannerModel,
		compilerEscalateModel:      *compilerEscalateModel,
		compileConfidenceThreshold: *compileConfThreshold,
		llmBaseURL:                 *llmBaseURL,
		gatewayURL:                 *gatewayURL,
		// X-Matrix-Actor-DID must be a BARE did: (gateway auth.looksLikeDID
		// rejects the matrix://user/<did> URI form). actor.DID is
		// did:matrix:<user-id>:<key16> — stable per user across restarts
		// (key persists on the snapshotted volume), so it is the correct
		// per-actor credit_ledger key for budget metering.
		actorDID:         actor.DID,
		seed:             *seed,
		maxRetry:         *maxRetry,
		criticEnabled:    *criticEnabled,
		criticModel:      *criticModel,
		maxReplan:        *maxReplan,
		broker:           broker,
		startedAt:        time.Now().UTC(),
		authToken:        os.Getenv("MATRIX_DAEMON_TOKEN"),
		allowSubDispatch: *allowSubDisp,
		workspaceRoot:    *workspaceRoot,
		boundUserID:      *boundUser,
		jwtSecret:        []byte(os.Getenv(*jwtSecretEnv)),
		gateTimeout:      *gateTimeout,
		asyncReg:         newAsyncRegistry(*asyncMaxJobs, asyncJobDir(*cortexRoot, *transcriptsDir)),
		convStore:        newConversationStore(conversationDir(*cortexRoot, *transcriptsDir)),
		gateBroker:       newGateBroker(),
		indexCache:       newIndexCache(256),
		skillCatalog:     newSkillCatalog(*skillsRoot),
		snapMgr:          snapMgr,
		metrics:          metrics,
	}
	if *forgeMode {
		state.forgeFS = DefaultForgeFSPolicy()
		state.gitOps = DefaultGitOpsPolicy()
		state.shellCfg = DefaultForgeShellConfig()
	}
	if *gideonMode {
		state.gideonMode = true
		state.gideonPolicy = DefaultGideonOpsPolicy()
		state.gideonSweepInterval = *gideonSweep
	}
	// Liaison conversational agent: wired unless explicitly disabled, so
	// every public runtime narrates its runs and serves the /chat front
	// door out of the box. Pure side-channel; see daemon_liaison.go.
	if !*liaisonDisable {
		state.liaison = &liaisonState{model: *liaisonModel}
	}
	// PaxeerSpendPolicy is wired unconditionally on the public per-
	// user daemon: paxeer-net is folded into agents/default.json so
	// every public runtime can spend, and every public runtime gets
	// the plan-time spend gate. Disable explicitly with
	// -paxeer-spend-policy-disable when running an internal/dev
	// daemon that should not gate writes.
	if !*paxeerDisable {
		policy := DefaultPaxeerSpendPolicy()
		if v, ok := parsePaxeerCapFlag(*paxeerCapStr); ok {
			policy.PerCallCapWei = v
		}
		if v, ok := parsePaxeerCapFlag(*paxeerAggStr); ok {
			policy.AggregateCapWei = v
		}
		state.paxeerSpend = policy
	}
	state.asyncCurrentIntent.Store("")
	t.Event("daemon.config", "boot", map[string]interface{}{
		"allow_sub_dispatch": state.allowSubDispatch,
		"workspace_root":     state.workspaceRoot,
		"bound_user":         state.boundUserID,
		"jwt_verify_enabled": len(state.jwtSecret) > 0,
		"gate_timeout":       state.gateTimeout.String(),
		"async_registry":     true,
		"snapshot_enabled":   state.snapMgr != nil,
		"surface_v":          daemonAPIVersion,
		"gateway_routing":    state.gatewayURL != "",
		"actor_did":          state.actorDID,
		"planner_model":      state.plannerModel,
		"compiler_escalate":  state.compilerEscalateModel,
		"liaison_enabled":    state.liaison != nil,
		"liaison_model":      state.liaisonMod(),
		"forge_mode":         state.forgeFS != nil,
		"forge_git":          state.gitOps != nil,
		"forge_shell":        state.shellCfg != nil,
		"gideon_mode":        state.gideonMode,
		"gideon_sweep":       state.gideonSweepInterval.String(),
		"paxeer_spend":       state.paxeerSpend != nil,
		"paxeer_cap_wei":     paxeerCapForLog(state.paxeerSpend, true),
		"paxeer_agg_cap_wei": paxeerCapForLog(state.paxeerSpend, false),
	})

	mux := newDaemonMux(state, t)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// SSE responses can be long-lived; do NOT set WriteTimeout.
	}

	// Wire /shutdown → graceful close. The handler triggers this fn
	// in a goroutine after replying 202; we drain via srv.Shutdown
	// so in-flight messages finish.
	shutdownReq := make(chan struct{}, 1)
	attachShutdown(func() {
		select {
		case shutdownReq <- struct{}{}:
		default:
		}
	})

	// Snapshot ticker: launch AFTER server is configured but BEFORE
	// signal-multiplex so the first tick rides the same parent context
	// (cancelling on shutdown). When snapMgr is nil, this is a no-op.
	tickerCtx, cancelTicker := context.WithCancel(context.Background())
	defer cancelTicker()
	if snapMgr != nil {
		snapMgr.Start(tickerCtx)
	}

	// Gideon reasoning scheduler (Phase 3). Launched only in gideon mode
	// with a positive sweep interval; stops on gideonCtx cancellation,
	// which fires when the daemon begins draining (below).
	gideonCtx, cancelGideon := context.WithCancel(context.Background())
	defer cancelGideon()
	if state.gideonMode && state.gideonSweepInterval > 0 {
		go state.runGideonScheduler(gideonCtx, t)
	}

	// Run server in goroutine so we can multiplex with signal handling.
	serverErr := make(chan error, 1)
	go func() {
		t.Event("daemon.listen", "boot", map[string]interface{}{"addr": *addr})
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	// Durable inbox (Leg A): resume any async jobs persisted before the
	// last stop — queued jobs are (re)dispatched so an accepted message
	// is never dropped, and jobs interrupted mid-flight surface a
	// deterministic outcome. Runs in the background so boot isn't blocked
	// while resumed jobs serialise on d.busy.
	go state.resumeAsyncJobs(t)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		t.Event("daemon.signal", "shutdown", map[string]interface{}{"signal": sig.String()})
	case <-shutdownReq:
		t.Event("daemon.shutdown.dispatch", "shutdown", nil)
	case err := <-serverErr:
		if err != nil {
			t.Event("daemon.listen.error", "shutdown", map[string]interface{}{"error": err.Error()})
			fmt.Fprintf(os.Stderr, "daemon: listen: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Graceful drain: stop accepting, allow in-flight to finish,
	// close SSE subscribers, then close infra.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()

	// Stop the Gideon scheduler before draining so no new sweep tick
	// acquires d.busy mid-shutdown.
	cancelGideon()

	t.Event("daemon.drain.start", "shutdown", nil)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Event("daemon.drain.timeout", "shutdown", map[string]interface{}{"error": err.Error()})
	}
	broker.CloseAll()
	t.Event("daemon.drain.done", "shutdown", nil)

	// Snapshot final push: server has drained, cortex is quiescent.
	// Run BEFORE the deferred in.Close() so Pebble is still flushable
	// from the same shutdown context. Errors are logged but never
	// fatal — losing a final snapshot is preferable to leaking a
	// half-closed Pebble DB if Push hangs.
	if snapMgr != nil {
		cancelTicker() // halt any in-flight tick before final Push
		stopCtx, cancelStop := context.WithTimeout(context.Background(), *shutdownTimeout)
		if err := snapMgr.Stop(stopCtx); err != nil {
			t.Event("snapshot.shutdown.error", "shutdown", map[string]interface{}{
				"error": err.Error(),
			})
		} else {
			t.Event("snapshot.shutdown.done", "shutdown", nil)
		}
		cancelStop()
	}
	// in.Close() runs via deferred call above.
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
