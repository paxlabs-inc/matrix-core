// Command chronosd is the centralized agent scheduler / wake control plane.
// One always-on service for the whole Matrix fleet: agents POST alarms (once /
// cron) with a contextful wake payload; chronosd durably stores them in
// Postgres, fires due alarms, and asks the router to wake the agent + deliver
// the resume turn.
//
//	chronosd                 # run the HTTP server + dispatch worker
//
// Config is env-first with an optional chronos.config.kvx overlay
// (internal/config). See chronos.frozen.kvx for the frozen architecture.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/paxlabs-inc/chronos/internal/auth"
	"github.com/paxlabs-inc/chronos/internal/config"
	"github.com/paxlabs-inc/chronos/internal/dispatch"
	"github.com/paxlabs-inc/chronos/internal/server"
	"github.com/paxlabs-inc/chronos/internal/store"
	"github.com/paxlabs-inc/chronos/internal/telemetry"
	"github.com/paxlabs-inc/chronos/internal/wake"
)

func main() {
	os.Exit(run())
}

func run() int {
	log := telemetry.NewLogger()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Error("config load failed", "error", err.Error())
		return 1
	}
	log.Info("chronosd config",
		"port", cfg.Port,
		"dev", cfg.Dev,
		"tick", cfg.Tick.String(),
		"router_wake_url", cfg.RouterWakeURL,
		"transport_auth", cfg.TransportToken != "",
	)

	st, err := store.New(ctx, cfg.PostgresURI)
	if err != nil {
		log.Error("postgres connect failed", "error", err.Error())
		return 1
	}
	defer st.Close()

	migDir := cfg.MigrationsDir
	if !filepath.IsAbs(migDir) {
		migDir = filepath.Join(moduleRoot(), migDir)
	}
	if err := st.Migrate(ctx, migDir); err != nil {
		log.Error("migrate failed", "dir", migDir, "error", err.Error())
		return 1
	}
	log.Info("migrations applied", "dir", migDir)

	challenges := auth.NewChallenges(cfg.ChallengeTTL)
	tokens := auth.NewTokens(cfg.AgentAuthSecret, cfg.TokenTTL)

	// Opportunistic challenge GC.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				challenges.Purge()
			}
		}
	}()

	// Dispatch worker: claims due alarms and delivers them via the router wake.
	waker := wake.New(cfg.RouterWakeURL, cfg.WakeToken)
	if cfg.WakeToken == "" {
		log.Warn("CHRONOS_WAKE_TOKEN unset: router /internal/wake will reject deliveries unless it too is tokenless (dev only)")
	}
	worker := dispatch.New(st, waker, log, dispatch.Config{
		Tick:        cfg.Tick,
		Lease:       cfg.ClaimLease,
		Batch:       cfg.ClaimBatch,
		MaxFailures: cfg.MaxFailures,
	})
	go worker.Run(ctx)

	srv := server.New(server.Deps{
		Store:          st,
		Challenges:     challenges,
		Tokens:         tokens,
		Log:            log,
		TransportToken: cfg.TransportToken,
		DefaultMaxFail: cfg.MaxFailures,
	})
	if cfg.TransportToken == "" {
		log.Warn("CHRONOS_TOKEN unset: transport auth disabled; bind to loopback/6PN only")
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("chronosd listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err.Error())
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info("chronosd shutdown complete")
	return 0
}

// moduleRoot resolves the migrations base dir when the path is relative. Honors
// CHRONOS_ROOT (set by the systemd unit), else the working directory.
func moduleRoot() string {
	if root := os.Getenv("CHRONOS_ROOT"); root != "" {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
