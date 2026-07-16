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
	"matrix/router/internal/beta"
	"matrix/router/internal/config"
	"matrix/router/internal/db"
	"matrix/router/internal/fly"
	"matrix/router/internal/jwt"
	"matrix/router/internal/mw"
	"matrix/router/internal/preview"
	"matrix/router/internal/provision"
	"matrix/router/internal/proxy"
	"matrix/router/internal/railway"
	"matrix/router/internal/voice"
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
	logf("matrix-router version=%s public=%s internal=%s provider=%s app=%s region=%s",
		version, cfg.PublicAddr, cfg.InternalAddr, cfg.Provider, cfg.FlyApp, cfg.FlyRegion)

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

	// 3. Environment provisioner (provider-neutral seam, selected by
	//    ROUTER_PROVIDER). Fly Machines is the default and stays fully
	//    intact; railway targets the consolidated one-project Railway
	//    deployment (wake-on-request, private networking).
	daemonImage := os.Getenv("ROUTER_DAEMON_IMAGE")
	var prov provision.Provisioner
	switch cfg.Provider {
	case config.ProviderRailway:
		prov = &railway.Provisioner{
			Client: railway.New(cfg.RailwayAPIToken, cfg.RailwayProjectID, cfg.RailwayEnvironmentID),
			Image:  daemonImage,
		}
	default:
		prov = &fly.Provisioner{
			Client:        fly.New(cfg.FlyAPIToken, cfg.FlyApp),
			Image:         daemonImage,
			VolumeSizeGB:  5,
			ProbeInterval: cfg.ProbeInterval,
		}
	}

	// 4. Reverse-proxy handler (JWT-protected via mw.JWT).
	proxyH := proxy.New(pool, prov, cfg.DaemonPort, cfg.WakeTimeout, cfg.ProbeInterval, logf)
	// Post-wake daemon HTTP-readiness deadline: Fly state=started only
	// means the VM is up; the daemon still pulls its snapshot + inits git
	// before binding its port. Without this wait the first post-wake
	// request 502s. Defaults to 30s (ROUTER_DAEMON_READY_TIMEOUT).
	proxyH.ReadyTimeout = cfg.DaemonReadyTimeout
	// Route the /cody/* path prefix to the co-located codyd engine on its own
	// port (default :8090); everything else goes to the Neo front on :8080.
	proxyH.CodyPort = cfg.CodyPort

	// 5. Admin handler (admin-token-protected). Daemon image must be
	//    provider-registry-pushable; passing via env keeps it operator-set.
	adminH := &admin.Handler{
		DB:               pool,
		Prov:             prov,
		Provider:         cfg.Provider,
		DefaultRegion:    cfg.FlyRegion,
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
			// (gateway/internal/rates, RateTableVersion 10). Xiaomi/MiMo pins:
			//   compiler = mimo-v2.5-pro, escalating to the same on a
			//              low-confidence frame (MATRIX_COMPILER_ESCALATE_MODEL);
			//   planner  = mimo-v2.5-pro (dedicated MATRIX_PLANNER_MODEL
			//              knob, decoupled from the executor knob);
			//   executor = mimo-v2.5-pro;
			//   liaison  = mimo-v2.5-pro (user-facing conversational
			//              narrator; MATRIX_LIAISON_MODEL knob).
			// Override any of these via /etc/matrix/router.env if the gateway
			// whitelist changes.
			"MATRIX_COMPILER_MODEL":          envOr("MATRIX_COMPILER_MODEL", "mimo-v2.5-pro"),
			"MATRIX_COMPILER_ESCALATE_MODEL": envOr("MATRIX_COMPILER_ESCALATE_MODEL", "mimo-v2.5-pro"),
			"MATRIX_PLANNER_MODEL":           envOr("MATRIX_PLANNER_MODEL", "mimo-v2.5-pro"),
			"MATRIX_EXECUTOR_MODEL":          envOr("MATRIX_EXECUTOR_MODEL", "mimo-v2.5-pro"),
			"MATRIX_LIAISON_MODEL":           envOr("MATRIX_LIAISON_MODEL", "mimo-v2.5-pro"),
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
			// -> xAI Grok Imagine primary, Novita fallback). The bridge boots
			// even with no key (the media_* tools degrade to a structured "not
			// configured" result, so an empty key never bricks daemon boot) and
			// reads XAI_API_KEY + NOVITA_API_KEY from the inherited Machine env
			// (its manifest entry in BOTH default.json and neo.json uses
			// env:[]). XAI_API_KEY powers image generation/editing, the
			// prompt-based image utilities, and text/image-to-video;
			// NOVITA_API_KEY keeps the mask-exact ops (inpainting, cleanup),
			// alpha-transparent background removal, and text-to-speech. Set in
			// /etc/matrix/router.env; empty leaves the media tools dormant.
			// Outputs land on the per-Machine volume at /data/media and are
			// served by the Neo front at /media.
			"XAI_API_KEY":           os.Getenv("XAI_API_KEY"),
			"NOVITA_API_KEY":        os.Getenv("NOVITA_API_KEY"),
			"MIMO_API_KEY":          os.Getenv("MIMO_API_KEY"),
			"MATRIX_LIVEKIT_URL":    os.Getenv("MATRIX_LIVEKIT_URL"),
			"MATRIX_LIVEKIT_KEY":    os.Getenv("MATRIX_LIVEKIT_KEY"),
			"MATRIX_LIVEKIT_SECRET": os.Getenv("MATRIX_LIVEKIT_SECRET"),
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
			// KindleLaunch bridge (tools/kindle in the daemon image). All contract
			// addresses self-default to the 2026-06-20 chain-125 manifest in
			// lib/config.mjs; signing reuses the paxeer embedded-wallet lane (the
			// daemon executor key), so no new auth env is needed. These knobs let
			// the operator repoint the chain RPC, the media write edge (token
			// metadata + logo/banner upload so launches render on the frontend),
			// and the public frontend link via /etc/matrix/router.env. Defaults
			// equal the in-bridge defaults, so behavior is unchanged when unset.
			"KINDLE_RPC_URL":       envOr("KINDLE_RPC_URL", "https://public-mainnet.rpcpaxeer.online/evm"),
			"KINDLE_MEDIA_GATEWAY": envOr("KINDLE_MEDIA_GATEWAY", "https://cdn.kindlelaunch.com"),
			"KINDLE_METADATA_URL":  envOr("KINDLE_METADATA_URL", "https://metadata.kindlelaunch.com"),
			"KINDLE_FRONTEND_URL":  envOr("KINDLE_FRONTEND_URL", "https://kindlelaunch.com"),
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
			// Centralized scheduler (tools/chronos/chronos.mjs stdio proxy ->
			// the box-side chronosd at MATRIX_CHRONOS_URL). Unlike the
			// browser/tachyon/uwac/deus Fly apps, chronosd runs co-located with
			// the router on the front-door box, so Fly Machines reach it over
			// the public nginx /chronos/ route (NOT a 6PN .internal address).
			// The proxy answers initialize/tools/list locally so an unreachable
			// scheduler never bricks daemon boot; it dials MATRIX_CHRONOS_URL
			// lazily on the first alarm_* call and presents MATRIX_CHRONOS_TOKEN
			// (== chronosd CHRONOS_TOKEN) as a bearer. Override the URL via
			// /etc/matrix/router.env if the public host changes.
			"MATRIX_CHRONOS_URL":   envOr("MATRIX_CHRONOS_URL", "https://matrix.paxeer.app/chronos"),
			"MATRIX_CHRONOS_TOKEN": os.Getenv("MATRIX_CHRONOS_TOKEN"),
			// LayerX settlement fabric (tools/layerx/layerx.mjs stdio proxy ->
			// the box-side layerxd at MATRIX_LAYERX_URL). layerxd runs on its OWN
			// dedicated box behind its own domain (public-mapi.matrixlayerx.com),
			// serving the /v1/* RPC at the domain root — so the proxy appends
			// /v1/... directly to this URL (no path prefix). The proxy answers
			// initialize/tools/list locally so an unreachable layerxd never
			// bricks daemon boot; it dials the URL lazily on the first layerx_*
			// call. LayerX is a full-transparency rollup so the transport bearer
			// is OPTIONAL (writes are authorized by the DID signature, invariant
			// i6); MATRIX_LAYERX_TOKEN is sent only when set (== layerxd
			// LAYERX_TOKEN, legacy fleet mode). Override either in
			// /etc/matrix/router.env if the public host changes.
			"MATRIX_LAYERX_URL":   envOr("MATRIX_LAYERX_URL", "https://public-mapi.matrixlayerx.com"),
			"MATRIX_LAYERX_TOKEN": os.Getenv("MATRIX_LAYERX_TOKEN"),
			// Paxeer Cloud control plane (the `paxc` CLI baked into the daemon
			// image; the agent runs it via the exec shell tool to deploy static
			// sites). PAXC_API is the control-plane base URL; PAXC_TOKEN is the
			// API bearer. Both inherit into the Machine env, so the shell tool's
			// child processes (and thus paxc) see them. Override either in
			// /etc/matrix/router.env. PAXC_TOKEN is sent only when set; with no
			// token paxc errors at call time (boot-safe — it is not a server).
			"PAXC_API":   envOr("PAXC_API", "https://cloud.hyperpaxeer.com"),
			"PAXC_TOKEN": os.Getenv("PAXC_TOKEN"),
			// SearXNG metasearch (tools/searxng/searxng.mjs stdio bridge ->
			// the shared searxng service). Boot-safe when unset: the bridge
			// starts and searx_* calls return a structured "not configured"
			// result. MATRIX_SEARXNG_TOKEN is an optional bearer.
			"MATRIX_SEARXNG_URL":   os.Getenv("MATRIX_SEARXNG_URL"),
			"MATRIX_SEARXNG_TOKEN": os.Getenv("MATRIX_SEARXNG_TOKEN"),
			"MATRIX_SANDBOX_URL":   envOr("MATRIX_SANDBOX_URL", "http://matrix-sandboxd.railway.internal:8092"),
			"MATRIX_SANDBOX_TOKEN": os.Getenv("MATRIX_SANDBOX_TOKEN"),
			// Cody preview-as-deliverable (spec/cody-client req 7). codyd in the
			// user VM provisions a Railway sandbox on the PRIVATE network, deploys
			// the project, and registers the sandbox's private host:port at the
			// router's internal /internal/preview door; the public
			// /preview/{user} proxy then reverse-proxies to it under the user's
			// JWT. These pass the same Railway creds the router uses plus the
			// internal door URL + token down to every codyd. Boot-safe when
			// unset: codyd runs preview-less and the client shows "no preview yet".
			"RAILWAY_API_TOKEN":      os.Getenv("RAILWAY_API_TOKEN"),
			"RAILWAY_PROJECT_ID":     os.Getenv("RAILWAY_PROJECT_ID"),
			"RAILWAY_ENVIRONMENT_ID": os.Getenv("RAILWAY_ENVIRONMENT_ID"),
			"ROUTER_INTERNAL_URL":    envOr("ROUTER_INTERNAL_URL", "http://matrix-router.railway.internal:8088"),
			"ROUTER_PREVIEW_TOKEN":   os.Getenv("ROUTER_PREVIEW_TOKEN"),
			// Optional: a Node-plus base image override for preview sandboxes.
			"CODY_PREVIEW_IMAGE": os.Getenv("CODY_PREVIEW_IMAGE"),
		},
		Log: logf,
	}
	// Wake-on-request providers sleep on OUTBOUND inactivity: the
	// daemon's periodic snapshot push (executor/internal/snapshot, 5m
	// default) is outbound traffic and would hold every user service
	// awake forever. Disable the ticker on Railway (BootPull + shutdown
	// push remain — the volume is the durable state, the snapshot is the
	// migration/seed vehicle). Fly keeps the periodic push: its billing
	// stops on suspend regardless, and the 5m cadence is the recovery
	// story for volume loss there.
	if cfg.Provider == config.ProviderRailway {
		adminH.MachineEnv["MATRIX_SNAPSHOT_INTERVAL"] = envOr("MATRIX_SNAPSHOT_INTERVAL", "-1s")
	}

	// Data-at-rest vault: when the platform supplies a key-encryption key, every
	// per-user machine boots FAIL-CLOSED — VAULT_REQUIRED=true with that KEK — so
	// no daemon ever writes a user's conversations, memory, or media in the clear
	// (a per-user key is minted daemon-side and wrapped by this KEK). Absent a
	// configured KEK the injection is skipped so an un-provisioned fleet keeps
	// booting (plaintext) rather than bricking; key provisioning is an operator
	// step (a remote KMS is the production complement, out of scope here).
	if kek := os.Getenv("MATRIX_VAULT_KEK"); kek != "" {
		adminH.MachineEnv["VAULT_KEK"] = kek
		adminH.MachineEnv["VAULT_REQUIRED"] = envOr("VAULT_REQUIRED", "true")
		if id := os.Getenv("MATRIX_VAULT_KEK_ID"); id != "" {
			adminH.MachineEnv["VAULT_KEK_ID"] = id
		}
	}

	// Wire router-side auto-provisioning: the proxy hands an
	// authenticated-but-unprovisioned user to the admin provisioner on
	// first request. Gated on the daemon image so a router without
	// provisioning configured keeps returning 404.
	if daemonImage != "" {
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
	// Beta-launch user endpoints (JWT-authed, router-local — not proxied).
	betaH := &beta.Handler{DB: pool, Log: logf}
	if daemonImage != "" {
		betaH.Provision = adminH
	}
	betaMux := http.NewServeMux()
	betaH.Mount(betaMux)
	publicMux.Handle("/invite/", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/consent", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/disclosure/", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/reports", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/provision/", mw.JWT(verifier, logf)(betaMux))

	// Per-user application previews, served over the private network. The
	// {userID} path segment MUST match the JWT subject (enforced inside the
	// preview.Handler), so previews are never world-readable. codyd registers
	// its private preview target via the internal listener (see below).
	previewReg := preview.NewRegistry()
	publicMux.Handle("/preview/", mw.JWT(verifier, logf)(&preview.Handler{Reg: previewReg, Logf: logf}))
	publicMux.Handle("/voice/token", mw.JWT(verifier, logf)(&voice.Handler{
		Proxy:     proxyH,
		ServerURL: os.Getenv("MATRIX_LIVEKIT_URL"),
		APIKey:    os.Getenv("MATRIX_LIVEKIT_KEY"),
		Secret:    os.Getenv("MATRIX_LIVEKIT_SECRET"),
	}))

	// JWT-protected proxy for everything else (/messages, /events, /intents/*).
	publicMux.Handle("/", mw.JWT(verifier, logf)(proxyH))

	// CORS wraps OUTSIDE the mux so a browser preflight (OPTIONS, no token) is
	// answered before mw.JWT ever runs; the Next.js client is served from a
	// different origin and cannot reach the API otherwise.
	publicSrv := &http.Server{
		Addr:              cfg.PublicAddr,
		Handler:           mw.AccessLog(logf)(mw.CORS(cfg.CORSOrigins, logf)(publicMux)),
		ReadHeaderTimeout: 10 * time.Second,
		// SSE responses can be long-lived; do NOT set WriteTimeout.
	}
	if len(cfg.CORSOrigins) == 0 {
		logf("cors: allowing ANY origin (set ROUTER_CORS_ORIGINS to restrict)")
	} else {
		logf("cors: allow-list %v", cfg.CORSOrigins)
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

	// Preview registration door: codyd (inside a user's VM) registers /
	// deregisters the private host:port of its preview server here; the
	// public /preview/{userID}/ mount then reverse-proxies to it. Admin-token
	// auth (constant-time bearer). Empty token leaves it unmounted, and the
	// public mount serves 404 (no targets ever registered).
	if cfg.PreviewToken != "" {
		internalMux.Handle("/internal/preview/", mw.Admin(cfg.PreviewToken, logf)(preview.RegisterHandler(previewReg)))
		logf("preview: registration enabled at %s/internal/preview/", cfg.InternalAddr)
	} else {
		logf("preview: registration DISABLED (ROUTER_PREVIEW_TOKEN unset)")
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
