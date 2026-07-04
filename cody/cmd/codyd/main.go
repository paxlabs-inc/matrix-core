// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// codyd serves the Cody coding engine in the per-user environment, beside
// Neo, following the proven daemon shape: engine + session supervisor + SSE
// broker + durable trace behind the router's JWT. It holds no signing key and
// never routes through MCL. Boot resumes orphaned plans (Task Durability);
// shutdown is graceful and leaves everything resumable.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"matrix/cody/internal/mode"
	"matrix/cody/internal/sandbox"
	"matrix/cody/internal/server"
	cortex "matrix/cortex"
	"matrix/cortex/store"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", envOrDefault("CODY_ADDR", ":8090"), "listen address")
	workspaceRoot := flag.String("workspace", envOrDefault("CODY_WORKSPACE", "/workspace"), "workspace root")
	dataDir := flag.String("data", envOrDefault("CODY_DATA_DIR", "/data/cody"), "durable state dir")
	defaultMode := flag.String("mode", envOrDefault("CODY_DEFAULT_MODE", "engineer"), "default mode (prototype|engineer|architect)")
	rulesDir := flag.String("rules", os.Getenv("CODY_RULES_DIR"), "rules/ standards dir (optional)")
	skillsDir := flag.String("skills", os.Getenv("CODY_SKILLS_DIR"), "skills/ library dir (optional)")
	scaffoldDir := flag.String("scaffold", os.Getenv("CODY_SCAFFOLD_DIR"), "scaffolder suite dir (optional)")
	flag.Parse()

	m, err := mode.Parse(*defaultMode)
	if err != nil {
		log.Fatalf("codyd: %v", err)
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("codyd: data dir: %v", err)
	}

	cortexRoot := envOrDefault("CODY_CORTEX_ROOT", filepath.Join(*dataDir, "cortex"))
	st, err := store.Open(cortexRoot, "cody", nil)
	if err != nil {
		log.Fatalf("codyd: cortex store: %v", err)
	}
	defer st.Close()

	// Preview (req 7): a Railway sandbox client turns a completed plan into an
	// on-demand preview reverse-proxied by the router. Enabled only when the
	// Railway credentials AND the router coordinates + this user's id are all
	// present; otherwise codyd runs preview-less (the client shows "no preview
	// yet"). CODY_USER_ID is the supabase user id the router /preview proxy keys
	// on — it MUST equal the JWT subject the router authenticates.
	var sb sandbox.Client
	if tok := os.Getenv("RAILWAY_API_TOKEN"); tok != "" {
		sb = sandbox.New(sandbox.Config{
			Token:         tok,
			ProjectID:     os.Getenv("RAILWAY_PROJECT_ID"),
			EnvironmentID: os.Getenv("RAILWAY_ENVIRONMENT_ID"),
		})
	}
	previewTTL := time.Duration(0)
	if v := os.Getenv("CODY_PREVIEW_TTL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			previewTTL = d
		}
	}

	engine, err := server.NewEngine(server.EngineOptions{
		WorkspaceRoot:     *workspaceRoot,
		DataDir:           *dataDir,
		Cortex:            cortex.New(st),
		GatewayURL:        os.Getenv("MATRIX_GATEWAY_URL"),
		ActorDID:          os.Getenv("CODY_ACTOR_DID"),
		DefaultMode:       m,
		OrchestratorModel: os.Getenv("CODY_ORCHESTRATOR_MODEL"),
		WorkerModel:       os.Getenv("CODY_WORKER_MODEL"),
		// Conversation titles come from a small bounded LLM call (async;
		// fallback = the first message line). CODY_TITLE_MODEL overrides the
		// model; the call is best-effort and never blocks a dispatch.
		TitleModel:  envOrDefault("CODY_TITLE_MODEL", mode.FastModel),
		RulesDir:    *rulesDir,
		SkillsDir:   *skillsDir,
		ScaffoldDir: *scaffoldDir,
		// Shared tool services (req 13.1): the browser (screenshot evidence),
		// fetch, and web search. Boot-safe when unset — same env as the Neo
		// daemon's tool proxies so one environment configures both.
		BrowserURL:   os.Getenv("MATRIX_BROWSER_URL"),
		BrowserToken: os.Getenv("MATRIX_BROWSER_TOKEN"),
		SearxngURL:   os.Getenv("MATRIX_SEARXNG_URL"),
		SearxngToken: os.Getenv("MATRIX_SEARXNG_TOKEN"),
		// Preview wiring (req 7).
		Sandbox:            sb,
		PreviewUserID:      os.Getenv("CODY_USER_ID"),
		RouterInternalURL:  os.Getenv("ROUTER_INTERNAL_URL"),
		RouterPreviewToken: os.Getenv("ROUTER_PREVIEW_TOKEN"),
		PreviewPublicBase:  os.Getenv("CODY_PREVIEW_PUBLIC_BASE"),
		PreviewTTL:         previewTTL,
		PreviewImage:       os.Getenv("CODY_PREVIEW_IMAGE"),
		Logf:               log.Printf,
	})
	if err != nil {
		log.Fatalf("codyd: %v", err)
	}

	if n := engine.ResumeOrphanedPlans(); n > 0 {
		log.Printf("codyd: resumed %d orphaned plan(s)", n)
	}

	srv := &http.Server{Addr: *addr, Handler: server.New(engine).Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("codyd: listening on %s (workspace %s, mode %s)", *addr, *workspaceRoot, m)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("codyd: %v", err)
		}
	case s := <-sig:
		log.Printf("codyd: %s — shutting down", s)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		engine.Close()
	}
	fmt.Println("codyd: bye")
}
