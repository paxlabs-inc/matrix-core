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

	engine, err := server.NewEngine(server.EngineOptions{
		WorkspaceRoot:     *workspaceRoot,
		DataDir:           *dataDir,
		Cortex:            cortex.New(st),
		GatewayURL:        os.Getenv("MATRIX_GATEWAY_URL"),
		ActorDID:          os.Getenv("CODY_ACTOR_DID"),
		DefaultMode:       m,
		OrchestratorModel: os.Getenv("CODY_ORCHESTRATOR_MODEL"),
		WorkerModel:       os.Getenv("CODY_WORKER_MODEL"),
		RulesDir:          *rulesDir,
		SkillsDir:         *skillsDir,
		Logf:              log.Printf,
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
