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

	if cfg.ObjStoreEndpoint != "" {
		_, err = objstore.New(ctx, objstore.Config{
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
		}
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
	regSvc := registry.NewService(db, chainRegistry, ix)
	discSvc := discovery.New(db)

	var gw *gateway.Gateway
	var settler *settlement.Settler
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
				chSvc := channels.New(db, wal)
				vSvc := channels.NewVoucherService(db, signer)
				gw = gateway.New(gateway.Config{
					Store:    db,
					Pricing:  pricing.New(db),
					Meter:    metering.New(db),
					Wallet:   wal,
					Signer:   signer,
					Quality:  quality.New(db),
					Channels: chSvc,
					Vouchers: vSvc,
					ChainID:  cfg.ChainID,
				})
				if cfg.Dev {
					settler = settlement.NewSettler(db, &settlement.DevPayer{})
				}
			}
		}
	}

	srv := server.New(server.Deps{
		Log:               log,
		Store:             db,
		Chain:             chainClient,
		Registry:          regSvc,
		Discovery:         discSvc,
		Gateway:           gw,
		Settler:           settler,
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
