// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// matrix-router — front door for Fly daemon Machines.
//
// Two listeners:
//
//	ROUTER_ADDR (public, e.g. :443)        JWT-protected reverse proxy
//	ROUTER_INTERNAL_ADDR (private, :8088)  admin + healthz (admin-token)
//
// Both share state: one *db.DB pool, one *fly.Client, one
// *jwt.Verifier. systemd loads /etc/matrix/router.env +
// /etc/matrix/postgres.env (see deploy/box/router/router.service)
// before exec.
//
// Hot path:
//
//	client → public listener → mw.JWT (verify token, extract sub) →
//	proxy.Handler (DB lookup → fly.EnsureStarted → reverse-proxy to
//	the user's Machine over Fly 6PN through wg0)
//
// Admin path:
//
//	operator → internal listener → mw.Admin (constant-time bearer
//	check) → admin.Handler.{Mount} (POST /admin/users + lifecycle)
//
// Graceful shutdown drains both listeners on SIGTERM/SIGINT, closes
// the DB pool, then exits 0.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"matrix/router/internal/admin"
	"matrix/router/internal/config"
	"matrix/router/internal/db"
	"matrix/router/internal/fly"
	"matrix/router/internal/jwt"
	"matrix/router/internal/mw"
	"matrix/router/internal/proxy"
)

// version is the build identity; overridden via -ldflags="-X main.version=...".
var version = "dev"

func main() {
	logf := func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, time.Now().UTC().Format(time.RFC3339Nano)+" "+format+"\n", args...)
	}

	cfg, err := config.Load()
	if err != nil {
		logf("config: %v", err)
		os.Exit(2)
	}
	logf("matrix-router version=%s public=%s internal=%s app=%s region=%s",
		version, cfg.PublicAddr, cfg.InternalAddr, cfg.FlyApp, cfg.FlyRegion)

	// 1. JWT verifier (HS256 legacy + JWKS asymmetric).
	verifier, err := jwt.New(jwt.Options{
		LegacySecret: []byte(cfg.SupabaseLegacyJWTSecret),
		SupabaseURL:  cfg.SupabaseURL,
	})
	if err != nil {
		logf("jwt: %v", err)
		os.Exit(2)
	}
	primeCtx, cancelPrime := context.WithTimeout(context.Background(), 10*time.Second)
	if err := verifier.PrimeJWKS(primeCtx); err != nil {
		// Non-fatal: lazy-fetch on first asymmetric token covers us.
		logf("jwks prime warn: %v", err)
	}
	cancelPrime()

	// 2. Postgres pool.
	dbCtx, cancelDB := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Open(dbCtx, cfg.DatabaseURL)
	cancelDB()
	if err != nil {
		logf("db: %v", err)
		os.Exit(2)
	}
	logf("db: connected")

	// 3. Fly Machines API client.
	flycli := fly.New(cfg.FlyAPIToken, cfg.FlyApp)

	// 4. Reverse-proxy handler (JWT-protected via mw.JWT).
	proxyH := proxy.New(pool, flycli, cfg.DaemonPort, cfg.WakeTimeout, cfg.ProbeInterval, logf)
	// Post-wake daemon HTTP-readiness deadline: Fly state=started only
	// means the VM is up; the daemon still pulls its snapshot + inits git
	// before binding its port. Without this wait the first post-wake
	// request 502s. Defaults to 30s (ROUTER_DAEMON_READY_TIMEOUT).
	proxyH.ReadyTimeout = cfg.DaemonReadyTimeout

	// 5. Admin handler (admin-token-protected). Daemon image must be
	//    Fly-registry-pushable; passing via env keeps it operator-set.
	adminH := &admin.Handler{
		DB:               pool,
		Fly:              flycli,
		DefaultRegion:    cfg.FlyRegion,
		DaemonImage:      os.Getenv("ROUTER_DAEMON_IMAGE"),
		VolumeSizeGB:     5,
		ProvisionTimeout: 90 * time.Second,
		MachineEnv: map[string]string{
			"MATRIX_S3_ENDPOINT": cfg.S3Endpoint,
			"MATRIX_S3_BUCKET":   cfg.S3Bucket,
			"MATRIX_S3_KEY":      os.Getenv("MATRIX_S3_KEY"),
			"MATRIX_S3_SECRET":   os.Getenv("MATRIX_S3_SECRET"),
			// MatrixGateway (metered LLM). Provisioned daemons route EVERY
			// LLM call through the gateway: the daemon's -gateway-url flag
			// defaults to env MATRIX_GATEWAY_URL and the bearer is read from
			// MATRIX_GATEWAY_TOKEN, so injecting these two env vars is
			// sufficient (no entrypoint change). Machines reach the box-side
			// gateway via the public nginx /gw/ route. These MUST be set in
			// the router env for launch; empty leaves the daemon with no LLM
			// credential path (we do not inject a direct provider key).
			"MATRIX_GATEWAY_URL":   os.Getenv("MATRIX_GATEWAY_URL"),
			"MATRIX_GATEWAY_TOKEN": os.Getenv("MATRIX_GATEWAY_TOKEN"),
			// Pin the fleet to the gateway free-tier whitelist + rate card
			// (gateway/internal/rates, RateTableVersion 2). v1 launch
			// (2026-06-01) model decisions:
			//   compiler = gpt-oss-120b, escalating to deepseek-v4-pro on a
			//              low-confidence frame (MATRIX_COMPILER_ESCALATE_MODEL);
			//   planner  = kimi-k2.6 (dedicated MATRIX_PLANNER_MODEL knob,
			//              decoupled from the executor knob; strong tool/JSON
			//              fidelity + low hallucination for plan_tree@1 synthesis);
			//   executor = kimi-k2.6;
			//   liaison  = deepseek-v4-flash (user-facing conversational
			//              narrator; dedicated MATRIX_LIAISON_MODEL knob).
			// Override any of these via /etc/matrix/router.env if the gateway
			// whitelist changes.
			"MATRIX_COMPILER_MODEL":          envOr("MATRIX_COMPILER_MODEL", "accounts/fireworks/models/gpt-oss-120b"),
			"MATRIX_COMPILER_ESCALATE_MODEL": envOr("MATRIX_COMPILER_ESCALATE_MODEL", "accounts/fireworks/models/deepseek-v4-pro"),
			"MATRIX_PLANNER_MODEL":           envOr("MATRIX_PLANNER_MODEL", "accounts/fireworks/routers/kimi-k2p6-fast"),
			"MATRIX_EXECUTOR_MODEL":          envOr("MATRIX_EXECUTOR_MODEL", "accounts/fireworks/routers/kimi-k2p6-fast"),
			"MATRIX_LIAISON_MODEL":           envOr("MATRIX_LIAISON_MODEL", "accounts/fireworks/models/deepseek-v4-pro"),
			"MATRIX_DEFAULT_SKILL":           envOr("MATRIX_DEFAULT_SKILL", "matrix://skill/paxeer-assistant@0.1.0"),
			// Web search (tools/websearch/web-search.mjs MCP server in the
			// daemon image). The stdio bridge inherits the Machine env (its
			// manifest entry uses env:[]), boots even with no key (the tool
			// degrades to a structured "not configured" result), and reads
			// whichever is set: TAVILY_API_KEY (recommended) or BRAVE_API_KEY,
			// with an optional WEBSEARCH_PROVIDER (tavily|brave) override. Set
			// these in /etc/matrix/router.env to enable real internet search
			// fleet-wide; empty leaves the web_search/web_news tools dormant.
			"TAVILY_API_KEY":     os.Getenv("TAVILY_API_KEY"),
			"BRAVE_API_KEY":      os.Getenv("BRAVE_API_KEY"),
			"WEBSEARCH_PROVIDER": os.Getenv("WEBSEARCH_PROVIDER"),
			// Media I/O (tools/media/media.mjs stdio bridge in the daemon image
			// -> Together AI image/video/audio). The bridge boots even with no
			// key (the media_* tools degrade to a structured "not configured"
			// result, so an empty key never bricks daemon boot) and reads
			// TOGETHER_API_KEY from the inherited Machine env (its manifest
			// entry in BOTH default.json and neo.json uses env:[]). Set this in
			// /etc/matrix/router.env to enable image generation / editing,
			// video generation, and audio transcription fleet-wide; empty
			// leaves the media tools dormant. Outputs land on the per-Machine
			// volume at /data/media and are served by the Neo front at /media.
			"TOGETHER_API_KEY": os.Getenv("TOGETHER_API_KEY"),
			// Shared headless browser (tools/browser/browser.mjs stdio proxy in
			// the daemon image -> the matrix-browser Fly app running
			// @playwright/mcp over Streamable HTTP). The proxy answers
			// initialize/tools/list locally so an unreachable browser never
			// bricks daemon boot; it dials MATRIX_BROWSER_URL lazily on the
			// first browser_* call. Defaults to the single-instance private
			// 6PN address (MCP Streamable-HTTP sessions are instance-affine, so
			// the shared browser runs as one always-on machine; .internal hits
			// it directly without LB. Switch to .flycast only with sticky
			// sessions). MATRIX_BROWSER_TOKEN (optional) is sent as a bearer.
			"MATRIX_BROWSER_URL":   envOr("MATRIX_BROWSER_URL", "http://matrix-browser.internal:8931/mcp"),
			"MATRIX_BROWSER_TOKEN": os.Getenv("MATRIX_BROWSER_TOKEN"),
			// Shared Solidity/EVM engine (tools/tachyon/tachyon.mjs stdio proxy
			// in the daemon image -> the matrix-tachyon Fly app running tachyond
			// over its JSON-RPC /rpc transport). Like the browser proxy, it
			// answers initialize/tools/list locally so an unreachable engine
			// never bricks daemon boot; it dials MATRIX_TACHYON_URL lazily on
			// the first tachyon_* call. Defaults to the private 6PN address.
			// WRITE tools (deploy / broadcast call) are signed by the caller's
			// OWN embedded wallet: the proxy mints + forwards the agent's
			// did:matrix bearer per request (reusing the daemon's executor key,
			// same as the paxeer lane), so the shared engine holds NO seed.
			// MATRIX_TACHYON_TOKEN (optional) is the engine's own bearer.
			"MATRIX_TACHYON_URL":   envOr("MATRIX_TACHYON_URL", "http://matrix-tachyon.internal:8645/rpc"),
			"MATRIX_TACHYON_TOKEN": os.Getenv("MATRIX_TACHYON_TOKEN"),
			// UWAC connector hub (tools/uwac/uwac.mjs stdio proxy in the daemon
			// image -> the shared uwacd Fly app: OAuth connector vault + tool
			// invoke). Like the browser/tachyon proxies it answers
			// initialize/tools/list locally so an unreachable hub never bricks
			// daemon boot; it dials MATRIX_UWAC_URL lazily on the first uwac_*
			// call. The daemon mints its OWN agent-DID principal token (reusing
			// the executor key, label = MATRIX_USER_ID) so uwacd scopes the
			// vault lookup to this owner. MATRIX_UWAC_TOKEN is the shared
			// transport bearer; OAuth tokens never reach the daemon.
			"MATRIX_UWAC_URL":   envOr("MATRIX_UWAC_URL", "http://matrix-uwac.internal:8646"),
			"MATRIX_UWAC_TOKEN": os.Getenv("MATRIX_UWAC_TOKEN"),
			// Deus agent-service gateway (tools/deus/deus.mjs stdio proxy).
			"MATRIX_DEUS_URL":        envOr("MATRIX_DEUS_URL", "http://deus-control.internal:9095"),
			"MATRIX_DEUS_TIMEOUT_MS": os.Getenv("MATRIX_DEUS_TIMEOUT_MS"),
		},
		Log: logf,
	}

	// Wire router-side auto-provisioning: the proxy hands an
	// authenticated-but-unprovisioned user to the admin provisioner on
	// first request. Gated on DaemonImage so a router without
	// provisioning configured keeps returning 404.
	if adminH.DaemonImage != "" {
		proxyH.Provision = adminH
	}

	// ---------- public mux ----------
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})
	publicMux.HandleFunc("/v/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":%q}`, version)
	})
	// JWT-protected proxy for everything else (/messages, /events, /intents/*).
	publicMux.Handle("/", mw.JWT(verifier, logf)(proxyH))

	publicSrv := &http.Server{
		Addr:              cfg.PublicAddr,
		Handler:           mw.AccessLog(logf)(publicMux),
		ReadHeaderTimeout: 10 * time.Second,
		// SSE responses can be long-lived; do NOT set WriteTimeout.
	}

	// ---------- internal mux ----------
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})
	if cfg.AdminEnabled() {
		adminMux := http.NewServeMux()
		adminH.Mount(adminMux)
		internalMux.Handle("/admin/", mw.Admin(cfg.AdminToken, logf)(adminMux))
		logf("admin: enabled at %s/admin/*", cfg.InternalAddr)
	} else {
		logf("admin: DISABLED (ROUTER_ADMIN_TOKEN unset)")
	}

	// Scheduler wake door: chronosd POSTs here when a durable alarm fires; the
	// router resolves the user's Machine, wakes it, and delivers the chat turn
	// (reuses the proxy's EnsureStarted + waitDaemonReady path). Wake-token
	// auth (constant-time bearer). Empty token leaves it unmounted.
	if cfg.WakeToken != "" {
		internalMux.Handle("/internal/wake", mw.Admin(cfg.WakeToken, logf)(proxyH.WakeHandler()))
		logf("wake: enabled at %s/internal/wake", cfg.InternalAddr)
	} else {
		logf("wake: DISABLED (ROUTER_WAKE_TOKEN unset)")
	}

	internalSrv := &http.Server{
		Addr:              cfg.InternalAddr,
		Handler:           mw.AccessLog(logf)(internalMux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	// ---------- run + multiplex shutdown ----------
	srvErr := make(chan error, 2)
	go func() {
		logf("listening (public): %s", cfg.PublicAddr)
		if err := publicSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("public: %w", err)
			return
		}
		srvErr <- nil
	}()
	go func() {
		logf("listening (internal): %s", cfg.InternalAddr)
		if err := internalSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("internal: %w", err)
			return
		}
		srvErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logf("signal: %s", sig)
	case err := <-srvErr:
		if err != nil {
			logf("listener fatal: %v", err)
			pool.Close()
			os.Exit(1)
		}
	}

	// 30s drain budget for both listeners + DB close.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelDrain()
	logf("draining...")
	_ = publicSrv.Shutdown(drainCtx)
	_ = internalSrv.Shutdown(drainCtx)
	pool.Close()
	logf("drained; exiting 0")
}

// envOr returns the value of env key, or def when the key is unset or
// empty. Used to give the provisioned-machine model pins a sane,
// gateway-whitelisted default while letting the operator override via
// /etc/matrix/router.env.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
