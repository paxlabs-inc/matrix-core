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

	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/internal/engine"
	"github.com/paxlabs-inc/tachyon-tools/pkg/api"
	"github.com/paxlabs-inc/tachyon-tools/pkg/mcp"
)

func main() {
	mcpMode := flag.Bool("mcp", false, "run MCP stdio server instead of HTTP")
	selftest := flag.Bool("selftest", false, "run MCP tool selftest and exit")
	flag.Parse()

	logOut := os.Stdout
	if *mcpMode || *selftest {
		logOut = os.Stderr // MCP uses stdout for NDJSON-RPC only.
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *selftest {
		if err := mcp.Selftest(); err != nil {
			logger.Error("selftest failed", "error", err)
			os.Exit(1)
		}
		logger.Info("mcp selftest ok", "tools", len(mcp.ToolNames))
		os.Exit(0)
	}
	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	eng, err := engine.New(cfg)
	if err != nil {
		logger.Error("engine init failed", "error", err)
		os.Exit(1)
	}

	if *mcpMode {
		if err := mcp.RunStdio(eng); err != nil {
			logger.Error("mcp stdio failed", "error", err)
			os.Exit(1)
		}
		return
	}

	srv := api.New(eng, logger)
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
