// Command layerxd is the centralized agent-native settlement fabric: one
// always-on sequencer for the whole Matrix fleet. Agents deposit (USDL direct or
// PAX/USDC/USDT swapped to USDL at the vault) to mint a USD-denominated balance
// (USDX), pay each other instantly + gaslessly, get Merkle-provable signed
// receipts, and have the flow net-settled to Paxeer on a tiered schedule.
//
//	layerxd                 # run the HTTP server + settlement worker
//
// Config is env-first with an optional layerx.config.kvx overlay
// (internal/config). See layerx.frozen.kvx for the frozen architecture.
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

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/config"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/server"
	"github.com/paxlabs-inc/layerx/internal/settle"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/internal/telemetry"
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
	log.Info("layerxd config",
		"port", cfg.Port,
		"dev", cfg.Dev,
		"window", cfg.Window.String(),
		"micro_threshold_micro_usdx", cfg.MicroThresholdUSDX,
		"reserve_asset", cfg.ReserveAsset(),
		"transport_auth", cfg.TransportToken != "",
		"chain_rpc", cfg.ChainRPC != "",
		"vault", cfg.VaultAddr != "",
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

	// Sequencer receipt-signing key (ed25519). Empty seed -> ephemeral (dev).
	signer, ephemeral, err := sig.New(cfg.SequencerSeedHex)
	if err != nil {
		log.Error("sequencer key init failed", "error", err.Error())
		return 1
	}
	if ephemeral {
		log.Warn("LAYERX_SEQUENCER_KEY unset: using an EPHEMERAL receipt-signing key (receipts will not verify across restarts; dev only)")
	}
	log.Info("sequencer signer ready", "pubkey", signer.PublicHex())

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

	// Settlement: DevSettler stands in until the on-chain Paxeer settler is
	// wired (layerx.frozen.kvx [deferred]).
	settler := chain.NewDevSettler()
	if cfg.ChainRPC == "" || cfg.AnchorAddr == "" {
		log.Warn("no chain configured (LAYERX_CHAIN_RPC / LAYERX_ANCHOR_ADDR unset): using DevSettler — batches anchor with a pseudo tx hash, NOT on Paxeer")
	}
	worker := settle.New(st, settler, log, cfg.Window)
	go worker.Run(ctx)

	led := ledger.New(st, signer, cfg.MicroThresholdUSDX)

	srv := server.New(server.Deps{
		Store:          st,
		Ledger:         led,
		Settler:        worker,
		Challenges:     challenges,
		Tokens:         tokens,
		Log:            log,
		TransportToken: cfg.TransportToken,
		VaultAddr:      cfg.VaultAddr,
		ReserveAsset:   cfg.ReserveAsset(),
	})
	if cfg.TransportToken == "" {
		log.Warn("LAYERX_TOKEN unset: transport auth disabled; bind to loopback/6PN only")
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("layerxd listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err.Error())
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info("layerxd shutdown complete")
	return 0
}

// moduleRoot resolves the migrations base dir when the path is relative. Honors
// LAYERX_ROOT (set by the systemd unit / container), else the working directory.
func moduleRoot() string {
	if root := os.Getenv("LAYERX_ROOT"); root != "" {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
