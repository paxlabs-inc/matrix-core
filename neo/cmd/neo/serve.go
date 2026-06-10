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
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/server"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/writeback"
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
	)
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	if *manifest != "" {
		cfg.ManifestPath = *manifest
	}
	if *cortexRoot != "" {
		cfg.CortexRoot = *cortexRoot
	}
	if *actor != "" {
		cfg.CortexActor = *actor
	}
	backendURL := strings.TrimSpace(*backend)
	if backendURL == "" {
		backendURL = cfg.DaemonURL
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- models ---
	main, err := newClient(cfg.MainModel, 0.4, 4096, cfg)
	if err != nil {
		fatal("cannot start main model %q: %v\n      set FIREWORKS_API_KEY (or MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN) and retry.", cfg.MainModel, err)
	}
	cheap, err := newClient(cfg.CheapModel, 0.2, 1024, cfg)
	if err != nil {
		cheap = nil
	}

	// --- memory (own cortex actor; separate Pebble DB under the shared root) ---
	pager, err := memory.Open(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo: memory unavailable (%v) — continuing without persistent recall\n", err)
		pager = nil
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
	}

	// --- background write-back consolidation ---
	var cons agent.Consolidator
	var wc *writeback.Consolidator
	if pager != nil {
		cm := cheap
		if cm == nil {
			cm = main
		}
		wc = writeback.New(cm, pager, cfg)
		wc.Start()
		cons = wc
	}

	// Durable conversation history: an explicit NEO_CONVERSATIONS_DIR wins,
	// else it derives from the cortex root's parent — the SAME dir the MCL
	// daemon uses (/data/conversations in prod), so Neo + daemon threads list
	// as one unified history and survive reload / suspend / redeploy.
	convDir := conversation.Dir(os.Getenv("NEO_CONVERSATIONS_DIR"), cfg.CortexRoot)

	engine := server.NewEngine(server.EngineOptions{
		Config:          cfg,
		Main:            main,
		Cheap:           cheap,
		Tools:           tm,
		Pager:           pager,
		Consolidator:    cons,
		ConversationDir: convDir,
		BackendURL:      backendURL,
		BackendToken:    os.Getenv("NEO_DAEMON_TOKEN"),
	})
	if convDir != "" {
		fmt.Printf("  history: %s\n", convDir)
	}

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

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "neo: shutting down…")

	// Graceful, ordered shutdown so Neo's cortex actor flushes before exit
	// (the daemon snapshots the shared /data tree on ITS shutdown).
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
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

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "neo: "+format+"\n", a...)
	os.Exit(1)
}
