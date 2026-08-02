// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// `neo serve` runs Neo as the production conversational service. It is the
// default agent behind POST /chat, streams its work (including live web-search
// snippets and source cards) over SSE, and reverse-proxies every other route
// to the co-located MCL daemon — which it also reaches for core_execute
// (rigorous / money-moving tasks) over HTTP. Deploy posture: Neo on :8080 in
// front, the daemon on :8081 behind it, both in the per-user Machine.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/automatrixlog"
	"matrix/neo/internal/automatrixsettings"
	"matrix/neo/internal/briefhistory"
	"matrix/neo/internal/briefsettings"
	"matrix/neo/internal/config"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/dojo"
	"matrix/neo/internal/machinemailsettings"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/preview"
	"matrix/neo/internal/runrecord"
	sbx "matrix/neo/internal/sandbox"
	"matrix/neo/internal/server"
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/task"
	"matrix/neo/internal/telegramsettings"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/trace"
	"matrix/neo/internal/writeback"
	"matrix/vault"
)

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		configPath = fs.String("config", "", "runtime neo.kvx config (optional)")
		manifest   = fs.String("manifest", "", "agent manifest with MCP servers (overrides config)")
		cortexRoot = fs.String("cortex-root", "", "cortex brain root dir (overrides config)")
		actor      = fs.String("actor", "", "cortex actor name (overrides config; default neo)")
		addr       = fs.String("addr", envOrDefault("NEO_ADDR", ":8080"), "listen address")
		backend    = fs.String("backend", "", "co-located MCL daemon base URL for core_execute + proxy (overrides NEO_DAEMON_URL/config)")
		noTools    = fs.Bool("no-tools", false, "skip spawning MCP servers (chat-only)")
		// P2-4: one-command hermetic local-dev preset. Composes Default()
		// with a temp cortex, the Hash embedder stub, no-op chain RPC,
		// and metering disabled. Zero external deps. When set, -config,
		// -cortex-root, -actor, and -backend are ignored (the preset is
		// fully hermetic). Explicit -manifest still wins so a sandbox
		// run can point at a test manifest.
		sandbox = fs.Bool("sandbox", false, "hermetic local-dev preset: temp cortex, hash embedder stub, mock/no-op chain RPC, metering off (zero external deps). See config.Sandbox().")
	)
	_ = fs.Parse(args)

	var cfg config.Config
	var err error
	if *sandbox {
		cfg = config.Sandbox()
		fmt.Fprintf(os.Stdout, "neo: sandbox preset active — temp cortex %s, hash embedder stub, no chain RPC, metering off\n", cfg.CortexRoot)
	} else {
		cfg, err = config.Load(*configPath)
		if err != nil {
			fatal("load config: %v", err)
		}
	}
	if *manifest != "" {
		cfg.ManifestPath = *manifest
	}
	if !*sandbox {
		if *cortexRoot != "" {
			cfg.CortexRoot = *cortexRoot
		}
		if *actor != "" {
			cfg.CortexActor = *actor
		}
	}
	backendURL := strings.TrimSpace(*backend)
	if backendURL == "" {
		backendURL = cfg.DaemonURL
	}

	// Fail-closed data-at-rest vault. Production (router injects VAULT_REQUIRED
	// =true) MUST bring up a usable per-user key or the daemon refuses to boot
	// into plaintext — mirroring the main-model fail-closed posture, never the
	// best-effort degrade path. The wrapped keyfile lives beside /data (the
	// cortex root's parent). The hermetic sandbox preset runs plaintext.
	vaultDataDir := filepath.Dir(cfg.CortexRoot)
	vaultUser := cfg.ActorDID
	if vaultUser == "" {
		vaultUser = os.Getenv("MATRIX_USER_ID")
	}
	if keyfile, err := os.ReadFile(vault.KeyfilePath(vaultDataDir)); err == nil {
		if existing, err := vault.ParseKeyfile(keyfile); err == nil && strings.TrimSpace(existing.User) != "" {
			vaultUser = existing.User
		}
	}
	vcfg := vault.ConfigFromEnv(vaultDataDir, vaultUser)
	if *sandbox {
		vcfg.Required = false
	}
	vaultSess, verr := vault.Boot(context.Background(), vcfg)
	if verr != nil {
		fatal("vault required but unavailable: %v — set VAULT_KEK (64-hex-char key) or VAULT_KEK_FILE and NEO_ACTOR_DID, or set VAULT_REQUIRED=false for plaintext dev/CLI", verr)
	}
	if vaultSess.Encrypting() {
		fmt.Fprintf(os.Stdout, "neo: data-at-rest vault active for %s\n", cfg.ActorDID)
	}
	// Thread the session into cfg so memory.Open seals the cortex store from
	// its first write (encryption below the hash boundary; vaultseam.go).
	cfg.Vault = vaultSess
	cfg.VaultUser = vaultUser

	// Media plane: generated + uploaded media live on the machine volume,
	// derived from the cortex root's parent (e.g. /data/media) unless overridden
	// by NEO_MEDIA_DIR. Export it (and the URL base) into the environment BEFORE
	// spawning MCP servers so the co-located media bridge (tools/media) writes
	// its outputs into the exact directory Neo serves from GET /media/.
	mediaPath := server.MediaDir(os.Getenv("NEO_MEDIA_DIR"), cfg.CortexRoot)
	if mediaPath != "" {
		_ = os.Setenv("MATRIX_MEDIA_DIR", mediaPath)
		if os.Getenv("MATRIX_MEDIA_BASE") == "" {
			_ = os.Setenv("MATRIX_MEDIA_BASE", "/media")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- models ---
	main, err := newClient(cfg.MainModel, 0.4, 8192, true, cfg)
	if err != nil {
		fatal("cannot start main model %q: %v\n      set XAI_API_KEY (or MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN) and retry.", cfg.MainModel, err)
	}
	cheap, err := newClient(cfg.CheapModel, 0.2, 1024, false, cfg)
	if err != nil {
		cheap = nil
	}
	// Background sub-agents (spawn_subagents) run on the main-capability model
	// but with EXTENDED REASONING OFF: only the user-facing Neo loop and the
	// core MCL pipeline think. nil falls back to the main client in the swarm.
	subMain, err := newClient(cfg.MainModel, 0.4, 8192, false, cfg)
	if err != nil {
		subMain = nil
	}
	// Stateless MiMo v2.5 omni vision grounder for DOJO desktop_look — invoked
	// ONLY inside the tool, never the session model. Generous max_tokens so a
	// reasoning-heavy grounding pass still leaves room for the JSON answer
	// (wave-1 empty-content gotcha); thinking OFF. nil disables desktop_look.
	omni, oerr := newOmniClient(envOrDefault("NEO_OMNI_MODEL", "mimo-v2.5"))
	if oerr != nil {
		omni = nil
	}

	// --- memory (own cortex actor; separate Pebble DB under the shared root) ---
	pager, err := memory.Open(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo: memory unavailable (%v) — continuing without persistent recall\n", err)
		pager = nil
	}

	// --- disposable-desktop bridge env (DOJO wave 2) ---
	// The desktop bridge (tools/desktop/desktop.mjs) posts bytebotd actions to
	// the daemon's own loopback /dojo/computer-use proxy. Wire its endpoint +
	// shared token into the environment BEFORE spawning tools, so the bridge
	// subprocess inherits them; the same token gates the proxy. Only when the
	// dojo lane is actually configured (Railway credential present).
	var dojoBridgeToken string
	if os.Getenv("RAILWAY_API_TOKEN") != "" {
		dojoBridgeToken = os.Getenv("MATRIX_DOJO_TOKEN")
		if dojoBridgeToken == "" {
			dojoBridgeToken = randomToken()
			_ = os.Setenv("MATRIX_DOJO_TOKEN", dojoBridgeToken)
		}
		if os.Getenv("MATRIX_DOJO_URL") == "" {
			host := *addr
			if strings.HasPrefix(host, ":") {
				host = "127.0.0.1" + host
			}
			_ = os.Setenv("MATRIX_DOJO_URL", "http://"+host+"/dojo")
		}
	}

	// --- tools (Neo's natural surface; escalate-class reachable only via core_execute) ---
	var tm *tools.Manager
	if !*noTools {
		tm, err = tools.Spawn(ctx, tools.Options{ManifestPath: cfg.ManifestPath, StderrSink: os.Stderr})
		if err != nil {
			fmt.Fprintf(os.Stderr, "neo: tools unavailable (%v) — continuing chat-only\n", err)
			tm = nil
		}
	}
	// Explicit memory lookup: "what do you remember?" must be an action the
	// model can take, not an apology about missing tools.
	if tm != nil && pager != nil {
		tm.SetRecall(pager.Recall)
		tm.SetMemoryMutation(pager.Mutate)
	}

	// --- background write-back consolidation ---
	var cons agent.Consolidator
	var wc *writeback.Consolidator
	if pager != nil {
		// Memory write-back: a stronger extractor (its quality sets memory
		// quality) + the cheap model for the rare relation-classify path.
		extract, eerr := newClient(cfg.ConsolidationModel, 0.2, 2048, false, cfg)
		if eerr != nil || extract == nil {
			extract = main // fall back to the main model if the extractor won't start
		}
		wc = writeback.New(extract, cheap, pager, cfg)
		wc.Start()
		cons = wc
	}

	// Durable conversation history: an explicit NEO_CONVERSATIONS_DIR wins,
	// else it derives from the cortex root's parent — the SAME dir the MCL
	// daemon uses (/data/conversations in prod), so Neo + daemon threads list
	// as one unified history and survive reload / suspend / redeploy.
	convDir := conversation.Dir(os.Getenv("NEO_CONVERSATIONS_DIR"), cfg.CortexRoot)
	// Durable task ledger: lives beside history on the machine volume so an
	// in-flight task survives a restart / Fly suspend and the boot reaper can
	// resume it (the Task Durability Rule).
	taskDir := task.Dir(os.Getenv("NEO_TASKS_DIR"), cfg.CortexRoot)
	runDir := runrecord.Dir(os.Getenv("NEO_RUNS_DIR"), cfg.CortexRoot)
	journalPath := sessionjournal.Dir(os.Getenv("NEO_SESSION_JOURNAL_PATH"), cfg.CortexRoot)
	journal, err := sessionjournal.Open(ctx, journalPath, vaultSess)
	if err != nil {
		fatal("cannot start session journal: %v", err)
	}
	// Durable workspace trace: "Neo's Computer" (tool steps / search cards /
	// media / surfaces / swarm windows) persisted per run beside history on the
	// machine volume, so reopening a thread rebuilds the workspace instead of
	// showing an empty computer (F3). Derives /data/trace; NEO_TRACE_DIR wins.
	traceDir := trace.Dir(os.Getenv("NEO_TRACE_DIR"), cfg.CortexRoot)
	// Durable Automatrix completion inbox: the in-app "Neo finished something
	// for you" surprise results, persisted beside history on the machine volume
	// so they survive reload / suspend / redeploy and are discoverable on next
	// open. Derives /data/automatrix; NEO_AUTOMATRIX_DIR wins.
	automatrixDir := automatrixlog.Dir(os.Getenv("NEO_AUTOMATRIX_DIR"), cfg.CortexRoot)
	// Coding workbench workspace: the same persisted root the fs/exec tools
	// mutate (/workspace in prod). NEO_WORKSPACE_DIR wins; absent any root the
	// workbench surface stays disabled.
	workspaceDir := server.WorkspaceRoot(os.Getenv("NEO_WORKSPACE_DIR"))
	// On-demand Railway sandbox previews (dev servers never run on this VM).
	// Enabled only when the Railway credentials AND the router coordinates +
	// this user's id are all present; otherwise the workbench shows the honest
	// "no preview" state. NEO_USER_ID falls back to CODY_USER_ID so existing
	// per-user deploy env keeps working.
	var sb sbx.Client
	if tok := os.Getenv("RAILWAY_API_TOKEN"); tok != "" {
		sb = sbx.New(sbx.Config{
			Token:         tok,
			ProjectID:     os.Getenv("RAILWAY_PROJECT_ID"),
			EnvironmentID: os.Getenv("RAILWAY_ENVIRONMENT_ID"),
		})
	}
	previewUserID := os.Getenv("NEO_USER_ID")
	if previewUserID == "" {
		previewUserID = os.Getenv("CODY_USER_ID")
	}
	previewTTL := time.Duration(0)
	if v := os.Getenv("NEO_PREVIEW_TTL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			previewTTL = d
		}
	}
	// Disposable dojo desktops (DOJO req 1): the lane exists only while a
	// session is alive — no standing per-user desktop service. Credentials
	// enter through the same Railway environment references as previews; the
	// caps are env-configured and enforced server-side (req 1.4).
	var dojoRW dojo.RailwayClient
	if tok := os.Getenv("RAILWAY_API_TOKEN"); tok != "" {
		dojoRW = dojo.NewRailway(dojo.RailwayConfig{
			Token:         tok,
			EnvironmentID: os.Getenv("RAILWAY_ENVIRONMENT_ID"),
		})
	}
	dojoCfg := dojo.Config{Image: os.Getenv("NEO_DOJO_IMAGE")}
	dojoCfg.Logf = func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, "neo: "+format+"\n", args...)
	}
	if v := os.Getenv("NEO_DOJO_IDLE_TIMEOUT"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			dojoCfg.IdleTimeout = d
		}
	}
	if v := os.Getenv("NEO_DOJO_MAX_LIFETIME"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			dojoCfg.MaxLifetime = d
		}
	}
	if v := os.Getenv("NEO_DOJO_PROVISION_TIMEOUT"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			dojoCfg.ProvisionTimeout = d
		}
	}
	if v := os.Getenv("NEO_DOJO_MAX_ACTIVE"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			dojoCfg.MaxActive = n
		}
	}

	var resurrection *agent.ResurrectionRuntime
	switch cfg.AgentRuntime {
	case "", "legacy":
		cfg.AgentRuntime = "legacy"
	case "resurrection":
		statePath := strings.TrimSpace(os.Getenv("NEO_TURNSTATE_PATH"))
		if statePath == "" {
			statePath = filepath.Join(vaultDataDir, "neo-turnstate.db")
		}
		resurrection, err = agent.OpenResurrectionRuntime(
			ctx, cfg, tm, pager, statePath,
		)
		if err != nil {
			fatal("cannot start resurrection runtime: %v", err)
		}
		fmt.Printf("  runtime: resurrection (%s)\n", statePath)
	default:
		fatal("unsupported NEO_RUNTIME %q (expected legacy or resurrection)", cfg.AgentRuntime)
	}

	engine := server.NewEngine(server.EngineOptions{
		Config:                 cfg,
		Main:                   main,
		Cheap:                  cheap,
		SubMain:                subMain,
		Tools:                  tm,
		Pager:                  pager,
		Runtime:                resurrection,
		Consolidator:           cons,
		ConversationDir:        convDir,
		TaskDir:                taskDir,
		RunDir:                 runDir,
		Journal:                journal,
		TraceDir:               traceDir,
		AutomatrixDir:          automatrixDir,
		AutomatrixSettingsDir:  automatrixsettings.Dir(os.Getenv("NEO_AUTOMATRIX_DIR"), cfg.CortexRoot),
		BriefSettingsDir:       briefsettings.Dir(os.Getenv("NEO_BRIEF_DIR"), cfg.CortexRoot),
		BriefHistoryDir:        briefhistory.Dir(os.Getenv("NEO_BRIEF_DIR"), cfg.CortexRoot),
		TelegramSettingsDir:    telegramsettings.Dir(os.Getenv("NEO_TELEGRAM_DIR"), cfg.CortexRoot),
		MachineMailSettingsDir: machinemailsettings.Dir(os.Getenv("NEO_MACHINEMAIL_DIR"), cfg.CortexRoot),
		MediaDir:               mediaPath,
		NovitaAPIKey:           os.Getenv("NOVITA_API_KEY"),
		VoiceASRURL:            envOrDefault("MATRIX_MIMO_ASR_URL", "https://api.xiaomimimo.com/v1/chat/completions"),
		VoiceASRKey:            os.Getenv("MIMO_API_KEY"),
		VoiceControllerURL:     envOrDefault("VOICE_CONTROLLER_URL", "http://127.0.0.1:8791"),
		WorkspaceDir:           workspaceDir,
		Sandbox:                sb,
		PreviewCfg: preview.Config{
			UserID:            previewUserID,
			RouterInternalURL: os.Getenv("ROUTER_INTERNAL_URL"),
			RouterToken:       os.Getenv("ROUTER_PREVIEW_TOKEN"),
			TTL:               previewTTL,
			Image:             os.Getenv("NEO_PREVIEW_IMAGE"),
		},
		DojoRailway:     dojoRW,
		DojoCfg:         dojoCfg,
		Omni:            omni,
		DojoBridgeToken: dojoBridgeToken,
		BackendURL:      backendURL,
		BackendToken:    os.Getenv("NEO_DAEMON_TOKEN"),
		Vault:           vaultSess,
	})
	if convDir != "" {
		fmt.Printf("  history: %s\n", convDir)
	}
	if taskDir != "" {
		fmt.Printf("  tasks: %s\n", taskDir)
	}
	if traceDir != "" {
		fmt.Printf("  trace: %s (workspace survives reload)\n", traceDir)
	}
	if journalPath != "" {
		fmt.Printf("  session journal: %s\n", journalPath)
	}
	if automatrixDir != "" {
		fmt.Printf("  automatrix: %s (completion inbox survives reload)\n", automatrixDir)
	}
	if mediaPath != "" {
		fmt.Printf("  media: %s (served at /media)\n", mediaPath)
	}
	if workspaceDir != "" {
		fmt.Printf("  workspace: %s (served at /workspace)\n", workspaceDir)
	}

	// Autonomous Automatrix execution (task 4.1): the wake handler hands a
	// picked non-financial opportunity to this runner, which marks it
	// in_progress and drives a supervised run on the RESTRICTED (no-money) tool
	// surface, resuming into the origin conversation and held to the same
	// completion gate as any state-touching turn. (The per-user opt-in + Chronos
	// alarm lifecycle — the governor — is wired by task 6.1.)
	engine.SetAutomatrixRunner(engine.RunAutomatrixOpportunity)

	// Autonomous morning-brief execution (ORACLE task 5.4): a fired-and-eligible
	// MORNING_BRIEF wake hands off to this runner, which drives a supervised,
	// restart-durable run on the TIGHT read-only brief surface and delivers the
	// composed brief out of band (durable conversation turn + inbox record, then
	// a best-effort ping). The per-user opt-in + MORNING_BRIEF alarm lifecycle
	// (the governor) is wired inside NewEngine when BriefSettingsDir is set.
	engine.SetBriefRunner(engine.RunMorningBrief)

	// Idle sandbox previews are torn down on a TTL (req 7.4).
	engine.StartPreviewReaper(ctx)
	// Idle / over-lifetime dojo desktops are torn down server-side (DOJO req 1.4).
	engine.StartDojoReaper(ctx)
	engine.StartTelegram(ctx)

	srv, err := server.New(engine, backendURL)
	if err != nil {
		fatal("build server: %v", err)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}

	fmt.Printf("%s serving on %s — default agent; backend daemon %s\n", cfg.AgentName, *addr, backendURL)
	if tm != nil {
		fmt.Printf("  tools: %d natural", len(tm.NaturalToolNames()))
		if esc := tm.EscalateToolNames(); len(esc) > 0 {
			fmt.Printf(" (+%d via core_execute)", len(esc))
		}
		fmt.Println()
		for _, wn := range tm.Warnings() {
			fmt.Printf("  ! %s\n", wn)
		}
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("listen: %v", err)
		}
	}()

	// Boot reaper (Task Durability Rule): pick up any task left unfinished by a
	// previous process (crash / Fly suspend mid-work) and drive it to
	// completion — at least one agent always finishes the job.
	if n := engine.ResumeOrphanedTasks(); n > 0 {
		fmt.Printf("  resuming %d unfinished task(s) from the durable ledger\n", n)
	}
	// Task Durability Rule for proactive work: pick up any Automatrix
	// opportunity left in_progress by a previous process (crash / suspend
	// mid-run) and drive it to completion on the restricted surface (task 4.1).
	if n := engine.ResumeInProgressAutomatrix(); n > 0 {
		fmt.Printf("  resuming %d in-progress automatrix opportunity(ies)\n", n)
	}
	// Task Durability Rule for the morning brief (ORACLE task 5.4): pick up any
	// brief left mid-compose by a previous process (crash / suspend) and resume
	// it on the RESTRICTED brief surface — never the money surface (the normal
	// reaper above already excludes KindBrief tasks).
	if n := engine.ResumeInProgressBriefs(); n > 0 {
		fmt.Printf("  resuming %d in-progress morning brief(s)\n", n)
	}

	<-ctx.Done()
	fmt.Fprintln(os.Stdout, "neo: shutting down…")

	// Graceful, ordered shutdown so Neo's cortex actor flushes before exit
	// (the daemon snapshots the shared /data tree on ITS shutdown).
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	// Flush the durable workspace trace writer before exit so a reopen after a
	// graceful restart still sees the last events (F3).
	engine.Close()
	if wc != nil {
		wc.Stop()
	}
	if tm != nil {
		_ = tm.Close()
	}
	if pager != nil {
		_ = pager.Close()
	}
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// randomToken mints a 32-hex shared secret for the loopback desktop-bridge
// proxy when the deployment did not pin MATRIX_DOJO_TOKEN.
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dojo-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "neo: "+format+"\n", a...)
	os.Exit(1)
}
