// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// matrix-router — front door for the per-user daemon services.
//
// Two listeners:
//
//	ROUTER_ADDR (public, e.g. :443)        JWT-protected reverse proxy
//	ROUTER_INTERNAL_ADDR (private, :8088)  admin + healthz (admin-token)
//
// Both share state: one *db.DB pool, one provider client, one
// *jwt.Verifier. On a systemd host, /etc/matrix/router.env +
// /etc/matrix/postgres.env are loaded before exec; on Railway the
// environment comes from the platform.
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
	"strings"
	"syscall"
	"time"

	"matrix/router/internal/admin"
	"matrix/router/internal/beta"
	"matrix/router/internal/config"
	"matrix/router/internal/db"
	"matrix/router/internal/finance"
	"matrix/router/internal/fly"
	"matrix/router/internal/jwt"
	"matrix/router/internal/mw"
	"matrix/router/internal/preview"
	"matrix/router/internal/provision"
	"matrix/router/internal/proxy"
	"matrix/router/internal/railway"
	"matrix/router/internal/shard"
	"matrix/router/internal/voice"
)

// version is the build identity; overridden via -ldflags="-X main.version=...".
var version = "dev"

func main() {
	logf := func(format string, args ...interface{}) {
		w := os.Stdout
		lower := strings.ToLower(format)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic") {
			w = os.Stderr
		}
		fmt.Fprintf(w, time.Now().UTC().Format(time.RFC3339Nano)+" "+format+"\n", args...)
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
	var shardRegistry *shard.Registry
	switch cfg.Provider {
	case config.ProviderRailway:
		if raw := os.Getenv("ROUTER_RAILWAY_SHARDS"); raw != "" {
			shardRegistry, err = shard.Load(raw, daemonImage)
			if err != nil {
				logf("shard registry: %v", err)
				os.Exit(2)
			}
			if err = shardRegistry.ValidateDB(context.Background(), pool); err != nil {
				logf("shard registry: %v", err)
				os.Exit(2)
			}
			p, ok := shardRegistry.Provisioner(os.Getenv("ROUTER_SHARD_ID"))
			if !ok {
				p, ok = shardRegistry.Provisioner("shard-0")
			}
			if !ok {
				logf("shard registry: no local or shard-0 provisioner")
				os.Exit(2)
			}
			prov = p
		} else {
			prov = &railway.Provisioner{
				Client: railway.New(cfg.RailwayAPIToken, cfg.RailwayProjectID, cfg.RailwayEnvironmentID),
				Image:  daemonImage,
			}
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
	proxyH.ShardProviders = shardRegistry
	// Post-wake daemon HTTP-readiness deadline: Fly state=started only
	// means the VM is up; the daemon still pulls its snapshot + inits git
	// before binding its port. Without this wait the first post-wake
	// request 502s. Defaults to 30s (ROUTER_DAEMON_READY_TIMEOUT).
	proxyH.ReadyTimeout = cfg.DaemonReadyTimeout
	// Route the /cody/* path prefix to the co-located codyd engine on its own
	// port (default :8090); everything else goes to the Neo front on :8080.
	proxyH.CodyPort = cfg.CodyPort
	directProxyH := proxyH
	if cfg.Provider == config.ProviderRailway && shardRegistry != nil {
		flyProv := &fly.Provisioner{
			Client:        fly.New(cfg.FlyAPIToken, cfg.FlyApp),
			Image:         daemonImage,
			VolumeSizeGB:  5,
			ProbeInterval: cfg.ProbeInterval,
		}
		directProxyH = proxy.New(pool, flyProv, cfg.DaemonPort, cfg.WakeTimeout, cfg.ProbeInterval, logf)
		directProxyH.ReadyTimeout = cfg.DaemonReadyTimeout
		directProxyH.CodyPort = cfg.CodyPort
		directProxyH.ShardProviders = shardRegistry
	}

	// 5. Admin handler (admin-token-protected). Daemon image must be
	//    provider-registry-pushable; passing via env keeps it operator-set.
	adminH := &admin.Handler{
		DB:               pool,
		Prov:             prov,
		ShardProviders:   shardRegistry,
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

	// Market data for the agent: the per-user daemon's finance bridge is told
	// where the internal lane is and given the token to reach it. It is handed
	// no vendor key — the keys stay in this process, and the daemon reads market
	// data through the same cache and quota the browser uses.
	if cfg.FinanceToken != "" {
		adminH.MachineEnv["MATRIX_FINANCE_URL"] = envOr("MATRIX_FINANCE_URL", "http://matrix-router.railway.internal:8088/internal/finance")
		adminH.MachineEnv["MATRIX_FINANCE_TOKEN"] = cfg.FinanceToken
	}

	// Wire router-side auto-provisioning: the proxy hands an
	// authenticated-but-unprovisioned user to the admin provisioner on
	// first request. Gated on the daemon image so a router without
	// provisioning configured keeps returning 404.
	if daemonImage != "" {
		proxyH.Provision = adminH
		directProxyH.Provision = adminH
	}
	if cfg.Provider == config.ProviderRailway && shardRegistry != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := adminH.ResumeRailwayOperations(ctx); err != nil {
				logf("railway operation recovery: %v", err)
			}
		}()
	}

	// ---------- public mux ----------
	routerRole := envOr("ROUTER_ROLE", "central")
	shardRouteMode := envOr("ROUTER_SHARDING_ROUTE_MODE", "sharded")
	if shardRouteMode != "sharded" && shardRouteMode != "direct" {
		logf("ROUTER_SHARDING_ROUTE_MODE must be direct or sharded")
		os.Exit(2)
	}
	var centralProxy *proxy.CentralProxy
	if routerRole == "central" && shardRegistry != nil && shardRouteMode == "sharded" {
		centralProxy = &proxy.CentralProxy{DB: pool, Direct: directProxyH, Resolve: func(shardID string) (string, []byte, string, bool) {
			entry, ok := shardRegistry.Entry(shardID)
			if !ok {
				return "", nil, "", false
			}
			keyID, key, ok := shardRegistry.CurrentIngress(shardID)
			return keyID, key, entry.RouterURL, ok
		}}
	}
	if routerRole == "central" && shardRegistry != nil {
		logf("railway shard routing mode: %s", shardRouteMode)
	}
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
	publicMux.Handle("/consent", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/disclosure/", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/onboarding/", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/subscription", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/subscription/", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/reports", mw.JWT(verifier, logf)(betaMux))
	publicMux.Handle("/provision/", mw.JWT(verifier, logf)(betaMux))

	// Per-user application previews, served over the private network. The
	// {userID} path segment MUST match the JWT subject (enforced inside the
	// preview.Handler), so previews are never world-readable. codyd registers
	// its private preview target via the internal listener (see below).
	previewReg := preview.NewRegistry()
	if centralProxy == nil {
		publicMux.Handle("/preview/", mw.JWT(verifier, logf)(&preview.Handler{Reg: previewReg, Logf: logf}))
	}
	voiceProxy := http.Handler(proxyH)
	if centralProxy != nil {
		voiceProxy = centralProxy
	}
	publicMux.Handle("/voice/token", mw.JWT(verifier, logf)(&voice.Handler{
		Proxy:     voiceProxy,
		ServerURL: os.Getenv("MATRIX_LIVEKIT_URL"),
		APIKey:    os.Getenv("MATRIX_LIVEKIT_KEY"),
		Secret:    os.Getenv("MATRIX_LIVEKIT_SECRET"),
	}))

	// Market data. The finance lane is served CENTRALLY here — not proxied to a
	// per-user daemon — so the vendor keys live in exactly one process, the
	// cache and quota are shared between the browser and Neo's finance bridge,
	// and every upstream call is counted in one place. It boots with or without
	// the keys; without them every call answers with a typed, plain-language
	// "not configured" naming the missing variable.
	financeSvc := finance.NewService(finance.ConfigFromEnv())
	publicMux.Handle("/finance/", mw.JWT(verifier, logf)(finance.NewHandler(financeSvc, logf)))

	// JWT-protected proxy for everything else (/messages, /events, /intents/*).
	if centralProxy != nil {
		publicMux.Handle("/", mw.JWT(verifier, logf)(centralProxy))
	} else {
		publicMux.Handle("/", mw.JWT(verifier, logf)(proxyH))
	}

	publicHandler := http.Handler(publicMux)
	if routerRole == "shard" {
		shardID := os.Getenv("ROUTER_SHARD_ID")
		if shardRegistry == nil || shardID == "" {
			logf("shard role requires ROUTER_RAILWAY_SHARDS and ROUTER_SHARD_ID")
			os.Exit(2)
		}
		keys, ok := shardRegistry.IngressKeys(shardID)
		if !ok {
			logf("shard role has no ingress keys for %s", shardID)
			os.Exit(2)
		}
		shardLocal := http.NewServeMux()
		shardLocal.Handle("/preview/", &preview.Handler{Reg: previewReg, Logf: logf})
		shardLocal.Handle("/internal/wake", proxyH.WakeHandler())
		shardLocal.Handle("/", proxyH)
		shardMux := http.NewServeMux()
		shardMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"ok","role":"shard","shard_id":%q}`, shardID)
		})
		shardMux.Handle("/", &proxy.ShardIngress{DB: pool, ShardID: shardID, Keys: keys, Next: shardLocal, Logf: logf})
		publicHandler = shardMux
	}

	// Only the browser-facing central router emits CORS. Shard responses flow
	// back through central, so wrapping them too would create duplicate
	// Access-Control-Allow-Origin headers that browsers reject.
	publicHTTPHandler := publicHandler
	if routerRole == "central" {
		publicHTTPHandler = mw.CORS(cfg.CORSOrigins, logf)(publicHandler)
		if len(cfg.CORSOrigins) == 0 {
			logf("cors: allowing ANY origin (set ROUTER_CORS_ORIGINS to restrict)")
		} else {
			logf("cors: allow-list %v", cfg.CORSOrigins)
		}
	}
	publicSrv := &http.Server{
		Addr:              cfg.PublicAddr,
		Handler:           mw.AccessLog(logf)(publicHTTPHandler),
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
		wakeHandler := http.Handler(proxyH.WakeHandler())
		if centralProxy != nil {
			wakeHandler = centralProxy.WakeHandler()
		}
		internalMux.Handle("/internal/wake", mw.Admin(cfg.WakeToken, logf)(wakeHandler))
		logf("wake: enabled at %s/internal/wake", cfg.InternalAddr)
	} else {
		logf("wake: DISABLED (ROUTER_WAKE_TOKEN unset)")
	}

	// Preview registration door: codyd (inside a user's VM) registers /
	// deregisters the private host:port of its preview server here; the
	// public /preview/{userID}/ mount then reverse-proxies to it. Admin-token
	// auth (constant-time bearer). Empty token leaves it unmounted, and the
	// public mount serves 404 (no targets ever registered).
	// Market-data door for the per-user daemon's finance bridge. It shares the
	// SAME finance.Service as the public lane, which is the whole point: the
	// agent asking for a quote and the user opening that symbol hit one cache,
	// one vendor quota, and one metering record — and no vendor key is ever
	// copied into a per-user machine. Admin-token auth; the acting user rides
	// the X-Matrix-Subject header so metering stays per user.
	if cfg.FinanceToken != "" {
		internalMux.Handle("/internal/finance/", mw.Admin(cfg.FinanceToken, logf)(finance.NewInternalHandler(financeSvc, logf)))
		logf("finance: internal lane enabled at %s/internal/finance/*", cfg.InternalAddr)
	} else {
		logf("finance: internal lane DISABLED (ROUTER_FINANCE_TOKEN unset)")
	}

	if cfg.PreviewToken != "" {
		previewHandler := preview.RegisterHandler(previewReg)
		if routerRole == "shard" {
			previewHandler = preview.RegisterHandlerForShard(previewReg, pool, os.Getenv("ROUTER_SHARD_ID"), time.Hour)
		}
		internalMux.Handle("/internal/preview/", mw.Admin(cfg.PreviewToken, logf)(previewHandler))
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
