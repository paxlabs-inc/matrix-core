// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command matrix-gateway is the loopback HTTP proxy described in
// journal/plan/01-ambient-architect.md §5.15. It mediates every LLM
// call from per-user Fly Machines to upstream Fireworks/Together,
// debiting a Postgres credit_ledger per call and enforcing a daily
// PAX hard-stop via 429 + ErrBudgetExhausted.
//
// Usage:
//
//	matrix-gateway \
//	  -addr 127.0.0.1:9090 \
//	  -postgres-uri postgres://matrix@localhost/matrix?sslmode=disable \
//	  -free-tier-only=true \
//	  -log-format text
//
// Flags:
//
//	-addr            HTTP listen address (default 127.0.0.1:9090)
//	-postgres-uri    Postgres credit_ledger URI; empty selects in-memory
//	-postgres-driver database/sql driver name (default "postgres" via
//	                 lib/pq blank import; build with -tags pgx to swap
//	                 to jackc/pgx/v5 in a follow-up)
//	-free-tier-only  Reject BYO requests; force every call through the
//	                 metered free tier (default true for sess#32 v1)
//	-log-format      "text" (default) | "json"
//	-default-cap-pax Default daily PAX cap when an actor lacks a row
//	                 in daily_budget_caps (default "10")
//	-rate-per-sec    Per-actor request rate (default 5)
//	-rate-burst      Per-actor burst (default 25)
//	-pre-estimate    PAX projected cost used for the pre-flight budget
//	                 gate (default "0.5")
//	-shutdown        Graceful shutdown drain budget (default 30s)
//
// Required environment:
//
//	MATRIX_GATEWAY_TOKEN  shared bearer token; clients send Authorization: Bearer ...
//	FIREWORKS_API_KEY     gateway's own upstream key for Fireworks
//	TOGETHER_API_KEY      gateway's own upstream key for Together (optional)
//
// Optional environment:
//
//	MATRIX_GATEWAY_DISABLED=true  → kill switch; responds 503 to everything
//	                                except /healthz until cleared.
//
// The Postgres driver registration is intentionally left to the build
// system: this file imports nothing driver-specific so the gateway
// module's go.mod stays free of database-driver deps. To enable the
// real Postgres backend, build with a side-import shim that registers
// the driver, e.g.:
//
//	//go:build pq
//	package main
//	import _ "github.com/lib/pq"
//
// During sess#32 v1 the daemon-facing `-postgres-uri=""` path runs the
// in-memory ledger so smoke tests, CI, and local dev all work without
// a Postgres dep.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"matrix/gateway/internal/auth"
	"matrix/gateway/internal/ledger"
	"matrix/gateway/internal/proxy"
	"matrix/gateway/internal/ratelimit"
	"matrix/gateway/internal/routing"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "matrix-gateway: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("matrix-gateway", flag.ContinueOnError)
	var (
		addr            = fs.String("addr", "127.0.0.1:9090", "HTTP listen address")
		postgresURI     = fs.String("postgres-uri", "", "Postgres URI for credit_ledger; empty selects in-memory ledger")
		postgresDriver  = fs.String("postgres-driver", "postgres", "database/sql driver name")
		freeTierOnly    = fs.Bool("free-tier-only", true, "reject BYO requests; force metered free tier")
		logFormat       = fs.String("log-format", "text", "log output format: text|json")
		defaultCap      = fs.String("default-cap-pax", ledger.DefaultDailyPaxCap, "default daily PAX cap")
		ratePerSec      = fs.Float64("rate-per-sec", 5, "per-actor request rate (tokens/sec)")
		rateBurst       = fs.Float64("rate-burst", 25, "per-actor burst capacity")
		preEstimate     = fs.String("pre-estimate", "0.5", "PAX projected cost for pre-flight budget gate")
		shutdownTimeout = fs.Duration("shutdown", 30*time.Second, "graceful shutdown drain budget")
		readHeader      = fs.Duration("read-header-timeout", 10*time.Second, "ReadHeaderTimeout for the http.Server")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	logf := newLogger(*logFormat)

	// Fail-fast: free-tier-only routes every whitelisted model to a
	// Fireworks upstream, so the gateway's FIREWORKS_API_KEY is mandatory.
	// Without it the gateway boots fine but 401s every upstream call — a
	// silent fleet-wide outage. (MATRIX_GATEWAY_TOKEN is enforced by
	// auth.New below.)
	if *freeTierOnly && os.Getenv("FIREWORKS_API_KEY") == "" {
		return fmt.Errorf("matrix-gateway: -free-tier-only=true requires FIREWORKS_API_KEY (gateway upstream key)")
	}

	authn, err := auth.New(auth.Options{
		Token: os.Getenv("MATRIX_GATEWAY_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("matrix-gateway: auth: %w", err)
	}

	router := routing.New(routing.Options{FreeTierOnly: *freeTierOnly})

	var lg ledger.Ledger
	switch {
	case *postgresURI == "":
		logf("ledger.memory", map[string]any{"default_cap": *defaultCap})
		lg = ledger.NewMemory(*defaultCap)
	default:
		db, err := sql.Open(*postgresDriver, *postgresURI)
		if err != nil {
			return fmt.Errorf("matrix-gateway: sql.Open: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("matrix-gateway: sql ping: %w", err)
		}
		logf("ledger.postgres", map[string]any{"driver": *postgresDriver})
		lg = ledger.NewPostgres(db, *defaultCap)
	}

	rl := ratelimit.New(*ratePerSec, *rateBurst)

	var disabled atomic.Bool
	if v := os.Getenv("MATRIX_GATEWAY_DISABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil && b {
			disabled.Store(true)
			logf("kill_switch.engaged_at_boot", map[string]any{
				"reason": "MATRIX_GATEWAY_DISABLED env",
			})
		}
	}

	srv, err := proxy.New(proxy.Options{
		Auth:        authn,
		Router:      router,
		Ledger:      lg,
		RateLimiter: rl,
		Provider: proxy.ProviderKeys{
			FireworksKey: os.Getenv("FIREWORKS_API_KEY"),
			TogetherKey:  os.Getenv("TOGETHER_API_KEY"),
		},
		Logf:           logf,
		Disabled:       func() bool { return disabled.Load() },
		PreEstimatePax: *preEstimate,
	})
	if err != nil {
		return fmt.Errorf("matrix-gateway: proxy.New: %w", err)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Mux(),
		ReadHeaderTimeout: *readHeader,
	}

	logf("gateway.listen", map[string]any{
		"addr":           *addr,
		"free_tier_only": *freeTierOnly,
		"postgres":       *postgresURI != "",
		"default_cap":    *defaultCap,
		"rate_per_sec":   *ratePerSec,
		"rate_burst":     *rateBurst,
	})
	listenErr := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logf("gateway.signal", map[string]any{"signal": sig.String()})
	case err := <-listenErr:
		if err != nil {
			return fmt.Errorf("matrix-gateway: listen: %w", err)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logf("gateway.shutdown.timeout", map[string]any{"error": err.Error()})
	}
	if err := lg.Close(); err != nil {
		logf("ledger.close.error", map[string]any{"error": err.Error()})
	}
	logf("gateway.shutdown.done", nil)
	return nil
}

// newLogger returns a logging function. text emits one event per line
// in `key=value` shape; json emits a single-line JSON object per event.
// Logging is deliberately minimal — operators rely on Prometheus +
// the box's nginx access log for the bulk of observability; this
// daemon-side logger is for state transitions only.
func newLogger(format string) func(string, map[string]any) {
	switch format {
	case "json":
		enc := log.New(os.Stderr, "", 0)
		return func(event string, fields map[string]any) {
			row := map[string]any{
				"event": event,
				"ts":    time.Now().UTC().Format(time.RFC3339Nano),
			}
			for k, v := range fields {
				row[k] = v
			}
			b, err := json.Marshal(row)
			if err != nil {
				enc.Printf(`{"event":"log_marshal_err","error":%q}`, err.Error())
				return
			}
			enc.Println(string(b))
		}
	default:
		l := log.New(os.Stderr, "matrix-gateway ", log.LstdFlags|log.LUTC)
		return func(event string, fields map[string]any) {
			if len(fields) == 0 {
				l.Println(event)
				return
			}
			l.Printf("%s %v", event, fields)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
