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

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/channels"
	"github.com/paxlabs-inc/deus/internal/config"
	"github.com/paxlabs-inc/deus/internal/discovery"
	"github.com/paxlabs-inc/deus/internal/gateway"
	"github.com/paxlabs-inc/deus/internal/hosting"
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/objstore"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/internal/server"
	"github.com/paxlabs-inc/deus/internal/settlement"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/streams"
	"github.com/paxlabs-inc/deus/internal/telemetry"
	"github.com/paxlabs-inc/deus/internal/wallet"
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
		log.Error().Err(err).Msg("config load failed")
		return 1
	}

	db, err := store.New(ctx, cfg.PostgresURI)
	if err != nil {
		log.Error().Err(err).Msg("postgres connect failed")
		return 1
	}
	defer db.Close()

	migDir := cfg.MigrationsDir
	if !filepath.IsAbs(migDir) {
		migDir = filepath.Join(moduleRoot(), migDir)
	}
	if err := db.Migrate(ctx, migDir); err != nil {
		log.Error().Err(err).Str("dir", migDir).Msg("migrate failed")
		return 1
	}

	var chainClient *chain.Client
	if cfg.RPCURL != "" {
		chainClient, err = chain.New(ctx, cfg.RPCURL, cfg.ChainID)
		if err != nil {
			if cfg.Dev {
				log.Warn().Err(err).Msg("chain connect failed (dev mode continues)")
			} else {
				log.Error().Err(err).Msg("chain connect failed")
				return 1
			}
		}
	}
	if chainClient != nil {
		defer chainClient.Close()
	}

	var blobStore hosting.BlobStore
	if cfg.ObjStoreEndpoint != "" {
		s3, err := objstore.New(ctx, objstore.Config{
			Endpoint:  cfg.ObjStoreEndpoint,
			AccessKey: cfg.ObjStoreAccessKey,
			SecretKey: cfg.ObjStoreSecretKey,
			Bucket:    cfg.ObjStoreBucket,
			UseSSL:    cfg.ObjStoreUseSSL,
		})
		if err != nil {
			if cfg.Dev {
				log.Warn().Err(err).Msg("objstore connect failed (dev mode continues)")
			} else {
				log.Error().Err(err).Msg("objstore connect failed")
				return 1
			}
		} else {
			blobStore = s3
		}
	}
	if blobStore == nil && cfg.Dev {
		blobStore = objstore.NewMem(cfg.ObjStoreBucket)
		log.Info().Msg("using in-memory objstore (dev)")
	}

	var chainRegistry *chain.Registry
	if chainClient != nil && cfg.ServiceRegistryAddr != "" {
		chainRegistry, err = chain.NewRegistry(chainClient, cfg.ServiceRegistryAddr)
		if err != nil {
			if cfg.Dev {
				log.Warn().Err(err).Msg("registry bind failed (dev mode continues)")
			} else {
				log.Error().Err(err).Msg("registry bind failed")
				return 1
			}
		}
	}
	var ix *indexer.Indexer
	if chainRegistry != nil {
		ix = indexer.New(chainRegistry, db)
	}

	// Chain-backed settlement payer + escrow reader (PaymentChannel.payout +
	// SettlementAnchor.anchor). Required to move PAX in production.
	var chainPayer *chain.Payer
	if chainClient != nil && cfg.SettlerPrivateKey != "" && cfg.SettlementAnchorAddr != "" {
		chainPayer, err = chain.NewPayer(chainClient, cfg.SettlerPrivateKey, cfg.SettlementAnchorAddr)
		if err != nil {
			if cfg.Dev {
				log.Warn().Err(err).Msg("chain payer init failed (dev mode continues)")
			} else {
				log.Error().Err(err).Msg("chain payer init failed")
				return 1
			}
		}
	}
	if chainPayer == nil && !cfg.Dev {
		log.Warn().Msg("no chain settler key/anchor configured: net settlement payouts disabled")
	}
	rankPath := filepath.Join(moduleRoot(), "configs", "ranking.yaml")
	discSvc := discovery.New(db,
		discovery.WithEmbedder(discovery.NewEmbedderFromConfig(cfg.EmbedEndpoint, cfg.EmbedModel)),
		discovery.WithRankingWeights(discovery.LoadRankingWeights(rankPath)),
	)
	regSvc := registry.NewService(db, chainRegistry, ix)
	regSvc.SetManifestIndexer(discSvc)

	var hostOrchestrator *hosting.Orchestrator
	if blobStore != nil {
		limits := hosting.LoadLimits()
		if cfg.HostingKillSwitch {
			limits.KillSwitch = true
		}
		var backend hosting.Backend
		if cfg.AppwriteEndpoint != "" && cfg.AppwriteProjectID != "" && cfg.AppwriteAPIKey != "" {
			backend = hosting.NewAppwriteBackend(hosting.AppwriteConfig{
				Endpoint:  cfg.AppwriteEndpoint,
				ProjectID: cfg.AppwriteProjectID,
				APIKey:    cfg.AppwriteAPIKey,
			}, blobStore, limits)
		} else if cfg.Dev {
			backend = &hosting.DevBackend{ExecURL: cfg.HostingDevExecURL}
		}
		if backend != nil {
			hostOrchestrator = hosting.NewOrchestrator(db, blobStore, backend, limits)
		}
	}

	var gw *gateway.Gateway
	var settler *settlement.Settler
	var streamSvc *streams.Service
	pricingSvc := pricing.New(db)
	signKey := cfg.GatewaySigningKey
	if signKey == "" && cfg.Dev {
		signKey = cfg.PublishPrivateKey
	}
	if signKey != "" && cfg.ServiceRegistryAddr != "" {
		signer, err := receipts.NewSignerFromHex(cfg.ChainID, cfg.ServiceRegistryAddr, signKey)
		if err != nil {
			if cfg.Dev {
				log.Warn().Err(err).Msg("gateway signer init failed (dev mode continues)")
			} else {
				log.Error().Err(err).Msg("gateway signer init failed")
				return 1
			}
		} else {
			var wal wallet.Client
			if cfg.WalletAPIURL != "" {
				wal = &wallet.HTTPClient{BaseURL: cfg.WalletAPIURL}
			} else if cfg.Dev {
				wal = &wallet.DevClient{MaxPerCallWei: ""}
			}
			if wal != nil {
				var escrowReader channels.EscrowReader
				if chainPayer != nil {
					escrowReader = chainPayer
				}
				chSvc := channels.New(db, wal, escrowReader)
				vSvc := channels.NewVoucherService(db, signer)
				var streamBackend streams.AccrualBackend
				var devStreams *streams.DevBackend
				if cfg.Dev {
					devStreams = streams.NewDevBackend()
					streamBackend = devStreams
				}
				if streamBackend != nil {
					streamSvc = streams.New(streams.Config{
						Store:   db,
						Pricing: pricingSvc,
						Wallet:  wal,
						Backend: streamBackend,
						Dev:     devStreams,
					})
				}
				gw = gateway.New(gateway.Config{
					Store:           db,
					Pricing:         pricingSvc,
					Meter:           metering.New(db),
					Wallet:          wal,
					Signer:          signer,
					Quality:         quality.New(db),
					Channels:        chSvc,
					Vouchers:        vSvc,
					Hosting:         hostOrchestrator,
					Streams:         streamSvc,
					ChainID:         cfg.ChainID,
					AppwriteProject: cfg.AppwriteProjectID,
					AppwriteKey:     cfg.AppwriteAPIKey,
				})
			}
		}
	}
	// Settlement only needs the store + a payer; it must not depend on the
	// gateway signer being configured (payout works on a bare dev boot too).
	if chainPayer != nil {
		settler = settlement.NewSettler(db, chainPayer)
	} else if cfg.Dev {
		settler = settlement.NewSettler(db, &settlement.DevPayer{})
	}

	blobURL := func(key string) string { return "" }
	if blobStore != nil {
		blobURL = blobStore.URL
	}
	srv := server.New(server.Deps{
		Log:               log,
		Store:             db,
		Chain:             chainClient,
		Registry:          regSvc,
		Discovery:         discSvc,
		Gateway:           gw,
		Settler:           settler,
		Streams:           streamSvc,
		Hosting:           hostOrchestrator,
		BlobURL:           blobURL,
		DevMode:           cfg.Dev,
		PublishPrivateKey: cfg.PublishPrivateKey,
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info().Str("addr", addr).Msg("deusd listening")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("http server failed")
			cancel()
		}
	}()

	// Reservation reaper (audit F4): release escrow reserved by callers who
	// opened a window but never co-signed a voucher, once the window has ended.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := db.ReleaseExpiredChannelReserves(ctx); err != nil {
					log.Warn().Err(err).Msg("reserve reaper failed")
				} else if n > 0 {
					log.Info().Int64("channels", n).Msg("released expired channel reserves")
				}
			}
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Info().Msg("deusd shutdown complete")
	return 0
}

func moduleRoot() string {
	if root := os.Getenv("DEUS_ROOT"); root != "" {
		return root
	}
	// When run via `go run` from deus/, cwd is sufficient.
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
