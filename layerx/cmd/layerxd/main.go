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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/config"
	"github.com/paxlabs-inc/layerx/internal/deposit"
	"github.com/paxlabs-inc/layerx/internal/events"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/engine"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	"github.com/paxlabs-inc/layerx/internal/perps/mode"
	"github.com/paxlabs-inc/layerx/internal/ratelimit"
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
		"require_transport", cfg.RequireTransport,
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

	// Pre-create the transfers seq-range partitions ahead of the cursor (migration
	// 005), and keep doing so periodically so inserts never spill into the DEFAULT
	// overflow partition. No-op when transfers is not partitioned.
	if err := st.EnsureTransferPartitions(ctx); err != nil {
		log.Warn("ensure transfer partitions failed (continuing; DEFAULT partition is the safety net)", "error", err.Error())
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := st.EnsureTransferPartitions(ctx); err != nil {
					log.Warn("periodic ensure transfer partitions failed", "error", err.Error())
				}
			}
		}
	}()

	// Live SSE event broker (GET /v1/stream): the settlement worker + the pay
	// handler publish onto it so explorer clients stream instead of polling.
	broker := events.NewBroker(256)

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

	// Settlement: select the real on-chain Paxeer settler when the chain is
	// fully configured (RPC + vault + anchor + operator key); otherwise fall back
	// to the DevSettler, which anchors with a deterministic pseudo tx hash and
	// performs no on-chain action.
	var settler chain.Settler = chain.NewDevSettler()
	var depositWorker *deposit.Worker
	var reserveReader server.ReserveReader
	chainConfigured := cfg.ChainRPC != "" && cfg.VaultAddr != "" && cfg.AnchorAddr != "" && cfg.OperatorKeyPresent
	if chainConfigured {
		client, err := chain.NewClient(ctx, cfg.ChainRPC, chain.DefaultChainID)
		if err != nil {
			log.Error("chain client init failed", "error", err.Error())
			return 1
		}
		defer client.Close()
		op, err := chain.LoadOperator(os.Getenv(cfg.OperatorKeyEnv))
		if err != nil {
			log.Error("operator key load failed", "error", err.Error())
			return 1
		}
		var anchorHistory *chain.BlockscoutAnchorHistory
		if cfg.AnchorHistoryURL != "" {
			anchorHistory, err = chain.NewBlockscoutAnchorHistory(cfg.AnchorHistoryURL, nil)
			if err != nil {
				log.Error("anchor history init failed", "error", err.Error())
				return 1
			}
		}
		ps, err := chain.NewPaxeerSettler(client, op, cfg.VaultAddr, cfg.AnchorAddr, anchorHistory)
		if err != nil {
			log.Error("paxeer settler init failed", "error", err.Error())
			return 1
		}
		settler = ps
		log.Info("on-chain settler ready", "chain_id", chain.DefaultChainID, "operator", op.Address().Hex(), "vault", cfg.VaultAddr, "anchor", cfg.AnchorAddr)

		// Chain-in: the deposit watcher credits off-chain USDX from confirmed
		// Vault Deposit events (+ reserve reconciliation, invariant i1).
		watcher, err := chain.NewWatcher(client, cfg.VaultAddr)
		if err != nil {
			log.Error("deposit watcher init failed", "error", err.Error())
			return 1
		}
		depositWorker = deposit.New(watcher, st, log, cfg.DepositPollInterval, cfg.DepositStartBlock, cfg.ChainConfirmations)
		// The watcher also serves the public reserve proof (GET /v1/supply, i1).
		reserveReader = watcher
	} else {
		log.Warn("chain not fully configured (need LAYERX_CHAIN_RPC + LAYERX_VAULT_ADDR + LAYERX_ANCHOR_ADDR + LAYERX_OPERATOR_KEY): using DevSettler — batches anchor with a pseudo tx hash, NOT on Paxeer; deposit watcher disabled")
	}
	worker := settle.New(st, settler, log, cfg.Window)
	worker.SetPublisher(broker)
	// At-least-once: re-anchor any transfer batch or withdrawal payout that was
	// sealed but never confirmed (e.g. a crash mid-settle). Idempotent on the root.
	if err := worker.RecoverPending(ctx); err != nil {
		log.Error("settlement recovery failed", "error", err.Error())
	}
	if err := worker.RecoverPendingWithdrawals(ctx); err != nil {
		log.Error("withdrawal recovery failed", "error", err.Error())
	}
	go worker.Run(ctx)
	if depositWorker != nil {
		go depositWorker.Run(ctx)
	}

	led := ledger.New(st, signer, cfg.MicroThresholdUSDX)

	// Perps (spec/layerx-perps): the durable market registry always mirrors the
	// compiled source of truth; the feed/engine/workers start only when a
	// Crossverse URL is configured AND the boot mode is not OFF. Everything
	// stays fail-closed otherwise.
	if err := st.SyncPerpMarkets(ctx, market.All()); err != nil {
		log.Error("perp market sync failed", "error", err.Error())
		return 1
	}
	perpModes, err := mode.NewRegistry(cfg.PerpsMode, cfg.PerpsMarketModes)
	if err != nil {
		log.Error("perps mode registry init failed", "error", err.Error())
		return 1
	}
	perpsDeps := &server.PerpsDeps{Modes: perpModes}
	crossverseConfigured := cfg.CrossverseURL != "" ||
		(cfg.CrossverseSymbolRESTBase != "" && cfg.CrossverseSymbolWSBase != "" &&
			cfg.CrossverseMarketsRESTBase != "" && cfg.CrossverseMarketsWSBase != "")
	if cfg.PerpsMode != mode.Off && crossverseConfigured {
		var symbols []string
		for _, sym := range market.Symbols() {
			if perpModes.Effective(sym) != mode.Off {
				symbols = append(symbols, sym)
			}
		}
		if len(symbols) > 0 {
			mgr, err := crossverse.New(crossverse.Config{
				BaseURL:         cfg.CrossverseURL,
				SymbolRESTBase:  cfg.CrossverseSymbolRESTBase,
				SymbolWSBase:    cfg.CrossverseSymbolWSBase,
				MarketsRESTBase: cfg.CrossverseMarketsRESTBase,
				MarketsWSBase:   cfg.CrossverseMarketsWSBase,
				Symbols:         symbols,
			})
			if err != nil {
				log.Error("crossverse manager init failed", "error", err.Error())
				return 1
			}
			if err := mgr.Start(ctx); err != nil {
				log.Error("crossverse manager start failed", "error", err.Error())
				return 1
			}
			canary := make(map[string]bool, len(cfg.PerpsCanaryDIDs))
			for _, did := range cfg.PerpsCanaryDIDs {
				canary[did] = true
			}
			eng := &engine.Engine{
				Store: st, Feed: mgr, Modes: perpModes, Signer: signer,
				LiquidatorDID: "did:layerx:perps:liquidator", CanaryDIDs: canary,
				Entitlements: engine.StaticEntitlements(cfg.PerpsMembership),
				Rollout:      engine.StoreRolloutAdmission{Store: st, StaffDIDs: canary},
			}
			perpsDeps.Engine = eng
			perpsDeps.Feed = mgr
			if err := eng.RecoverPerpBatches(ctx, settler); err != nil {
				log.Error("perp batch recovery failed", "error", err.Error())
			}
			go runPerpsWorkers(ctx, eng, settler, log)
			log.Info("perps enabled", "global_mode", string(cfg.PerpsMode), "symbols", len(symbols), "canary_dids", len(cfg.PerpsCanaryDIDs))
		}
	} else if cfg.PerpsMode != mode.Off {
		log.Warn("perps mode is set but LAYERX_CROSSVERSE_URL is empty: perps stay fail-closed (reads only)")
	}

	// Application-level rate limiting (defense in depth behind the nginx edge):
	// per-client token buckets, swept periodically to bound memory.
	var rl server.RateLimit
	if !cfg.RateLimitDisabled {
		rl = server.RateLimit{
			Enabled:    true,
			TrustProxy: cfg.RateLimitTrustProxy,
			Read:       ratelimit.New(float64(cfg.RateLimitReadRPS), cfg.RateLimitReadBurst),
			Write:      ratelimit.New(float64(cfg.RateLimitWriteRPS), cfg.RateLimitWriteBurst),
			Auth:       ratelimit.New(float64(cfg.RateLimitAuthRPS), cfg.RateLimitAuthBurst),
		}
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			const maxIdle = 10 * time.Minute
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					rl.Read.Purge(maxIdle)
					rl.Write.Purge(maxIdle)
					rl.Auth.Purge(maxIdle)
				}
			}
		}()
		log.Info("rate limiting enabled",
			"trust_proxy", cfg.RateLimitTrustProxy,
			"read_rps", cfg.RateLimitReadRPS, "write_rps", cfg.RateLimitWriteRPS, "auth_rps", cfg.RateLimitAuthRPS)
	} else {
		log.Warn("application rate limiting DISABLED (LAYERX_RATELIMIT_DISABLE=1): relying on the nginx edge limiter only")
	}

	srv := server.New(server.Deps{
		Store:            st,
		Ledger:           led,
		Settler:          worker,
		Challenges:       challenges,
		Tokens:           tokens,
		Reserve:          reserveReader,
		Log:              log,
		TransportToken:   cfg.TransportToken,
		RequireTransport: cfg.RequireTransport,
		VaultAddr:        cfg.VaultAddr,
		ReserveAsset:     cfg.ReserveAsset(),
		ChainID:          chain.DefaultChainID,
		AnchorAddr:       cfg.AnchorAddr,
		USDLAddr:         cfg.USDLAddr,
		DEXRouter:        cfg.DEXRouter,
		SequencerPubHex:  signer.PublicHex(),
		Window:           cfg.Window,
		MicroThreshold:   cfg.MicroThresholdUSDX,
		ChainConfigured:  chainConfigured,
		Events:           broker,
		Perps:            perpsDeps,
		RateLimit:        rl,
	})
	if cfg.RequireTransport && cfg.TransportToken != "" {
		log.Info("transport bearer ENFORCED on write/principal endpoints (private-fleet mode); public read surface stays open")
	} else {
		log.Info("public RPC mode: read surface + DID-signed writes are open; transport bearer NOT required (set LAYERX_REQUIRE_TRANSPORT=1 to gate writes)")
	}

	addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
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

// runPerpsWorkers drives the restart-safe perps workers: trigger evaluation,
// liquidation, funding settlement, invariant reconciliation, and perps-root
// anchoring. Every worker claims work transactionally, so overlapping runs and
// restarts stay exactly-once.
func runPerpsWorkers(ctx context.Context, eng *engine.Engine, settler chain.Settler, log *slog.Logger) {
	trigger := time.NewTicker(time.Second)
	liquidate := time.NewTicker(2 * time.Second)
	funding := time.NewTicker(time.Minute)
	reconcile := time.NewTicker(time.Minute)
	anchor := time.NewTicker(5 * time.Minute)
	defer trigger.Stop()
	defer liquidate.Stop()
	defer funding.Stop()
	defer reconcile.Stop()
	defer anchor.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger.C:
			if _, err := eng.RunTriggerOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("perps trigger worker failed", "error", err.Error())
			}
		case <-liquidate.C:
			if _, err := eng.RunLiquidationOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("perps liquidation worker failed", "error", err.Error())
			}
		case <-funding.C:
			if _, err := eng.RunFundingOnce(ctx, time.Now().UnixMilli()); err != nil && ctx.Err() == nil {
				log.Error("perps funding worker failed", "error", err.Error())
			}
		case <-reconcile.C:
			if err := eng.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("perps reconciliation failed (markets paused)", "error", err.Error())
			}
		case <-anchor.C:
			if _, err := eng.AnchorPerpJournalOnce(ctx, settler); err != nil && ctx.Err() == nil {
				log.Error("perps anchoring failed", "error", err.Error())
			}
		}
	}
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
