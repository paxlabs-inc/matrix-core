// Command uwacd is the UWAC control plane: the OAuth connector hub + token
// vault + tool-invoke service that Matrix daemons reach over the network.
//
//	uwacd                # run the HTTP server
//	uwacd -dump-tools    # print the MCP tool advertisement (generates uwac-tools.json)
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/paxlabs-inc/uwac/internal/catalog"
	"github.com/paxlabs-inc/uwac/internal/config"
	"github.com/paxlabs-inc/uwac/internal/engine"
	"github.com/paxlabs-inc/uwac/internal/oauth"
	"github.com/paxlabs-inc/uwac/internal/vault"
	"github.com/paxlabs-inc/uwac/pkg/api"
	"github.com/paxlabs-inc/uwac/pkg/mcp"
)

func main() {
	dumpTools := flag.Bool("dump-tools", false, "print the MCP tool advertisement JSON and exit")
	flag.Parse()

	logOut := os.Stdout
	if *dumpTools {
		logOut = os.Stderr
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	reg, err := catalog.Registry()
	if err != nil {
		logger.Error("connector registry failed", "error", err)
		os.Exit(1)
	}

	if *dumpTools {
		b, err := mcp.DumpJSON(reg)
		if err != nil {
			logger.Error("dump tools failed", "error", err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
		os.Stdout.Write([]byte("\n"))
		return
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	creds := map[string]oauth.ProviderCreds{
		"google": {
			ClientID:     os.Getenv("UWAC_GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("UWAC_GOOGLE_CLIENT_SECRET"),
			TokenURL:     "https://oauth2.googleapis.com/token",
		},
	}
	oc := oauth.New(cfg.SupabaseURL, cfg.SupabaseAnonKey, creds)

	// TODO: swap to a Postgres-backed vault (encrypting tokens via cryptox with
	// cfg.VaultKey) when cfg.DatabaseURI is set. The in-memory store is dev-only.
	var store vault.Store = vault.NewMemory()
	if cfg.DatabaseURI != "" {
		logger.Warn("uwacd: UWAC_DATABASE_URI set but the Postgres vault is not wired yet; using in-memory store (dev only)")
	}

	eng := engine.New(cfg, store, reg, oc, logger)
	srv := api.New(eng, cfg.AuthToken, logger)

	go func() {
		if err := srv.ListenAndServe(cfg.APIAddr); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
}
