# Deus Marketplace + Dev Dashboard — Build Plan & Definition of Done

> Canonical checklist for the Deus frontend build. A fresh session should read
> this top-to-bottom before doing anything. Nothing here is "done" until its box
> is checked **with evidence** (a passing command, a working screen, a real
> round-trip against the live API). Do not claim completion without proof.

## 0. Mission

Build the **public Marketplace** and the **authenticated Dev Dashboard** for
Deus, and complete the **Appwrite-backed hosting** so a developer can upload
their API/agent code and have it run in "the cloud" — without ever seeing
Appwrite. One Next-gen React app (`marketplace/`), one live Go backend
(`deus/`, already deployed at `https://deus.paxeer.app`).

## 1. What already exists (do NOT rebuild)

- **Deus backend is LIVE** at `https://deus.paxeer.app` — REST `/v1` API
  (registry, discover, quote/invoke, analytics, spend, hosting endpoints).
  Postgres+pgvector, chain 125 (`ServiceRegistry` + `SettlementAnchor`), MinIO.
- **Appwrite / Paxeer Cloud** is deployed at `cloud.paxeer.app`
  (functions `functions.paxeer.app`, sites `sites.paxeer.app`). It is the
  hosting substrate ONLY. Its admin API key is a **server secret** held by
  `deusd`; the browser never talks to Appwrite.
- **`marketplace/`** is scaffolded: React Router v7.16 (`ssr: true`,
  `v8_viteEnvironmentApi` on), Tailwind v4, the full **smoothui** component
  library vendored under `marketplace/components/ui/smoothui/**`, and a complete
  **Paxeer Brand v3.0** design system in `marketplace/app/app.css`
  (dark obsidian, Ink Blue `#2841B8`, Pangram Sans + Inter + JetBrains Mono;
  shadcn tokens mapped + `@theme inline`). CSS is DONE — do not redo it.

## 2. Locked decisions (agreed with Andrew)

- **Stack:** React Router v7 + **Cloudflare Vite plugin**, deployed with
  **Wrangler** to **Cloudflare Workers**. Loaders/actions are the edge BFF.
- **Appwrite = invisible plumbing.** Dashboard → Deus API → Appwrite. The dev
  sees only "your code is live in the cloud." No Appwrite login, no Appwrite in
  the browser. (This is options A+B from brainstorming; C/Appwrite-auth rejected.)
- **Auth:** existing **Supabase** login (GoTrue at the Supabase box; Google/GitHub
  OAuth, ~57 users) + link an EVM wallet for payout/signing. NOT Appwrite auth.
- **Try-it = both:** real human invoke in the UI (behind a connected wallet) AND
  agent-via-MCP. Quotes shown to everyone.
- **Components:** compose screens ONLY from `marketplace/components/**`. Do not
  hand-roll UI primitives that already exist there. Do not edit `app.css`.
- **Scope:** full-featured v1, **no cuts**.
- **Hosted runtime v1:** node20 functions (matches `deus/internal/hosting/appwrite.go`).

## 3. Hard constraints (NEVER violate)

- **No git commit / push.** Andrew drives all commits via his own pre-commit
  hook. Deus/marketplace work is staged locally only. Never run
  `git add/commit/push` as a deliverable. Uncommitted state is expected/fine.
- **No secrets in the repo or in the browser bundle.** Appwrite key, Supabase
  service key, signing keys stay server-side (`deusd` / Worker secrets only).
- **No emojis, no purple gradients, no glow effects.** Depth via surface tone,
  not heavy borders. Follow the v3 token system already in `app.css`.
- **Show the result, not the tech.** No protocol jargon (Merkle/EIP-712/Appwrite)
  surfaced to end users.
- Verify endpoints against the ACTUAL Go handlers
  (`deus/internal/server/handlers_*.go`, `deus/pkg/types/api.go`) — the docs in
  `deus/docs/05-api.md` are the design and may differ from what's implemented.

## 4. Foundation — Phase 0 (sequential; blocks everything else)

This must land first and be green before fanning out the swarm.

- [ ] **Cloudflare preset conversion.** Add `@cloudflare/vite-plugin` to
      `vite.config.ts`; create `wrangler.jsonc` (name, `main = workers/app.ts`,
      `compatibility_date >= 2025-04-01`, `nodejs_compat`, assets binding,
      `vars`/secrets); create `workers/app.ts` (fetch handler delegating to the
      RR request handler, passing `{ cloudflare: { env, ctx } }`). Replace
      `@react-router/serve`/node `start` script with `wrangler dev` / `wrangler deploy`.
      Keep `ssr: true`.
- [ ] **Component integration shim** (no edits to `components/**` or `app.css`):
  - [ ] `app/lib/utils.ts` exporting `cn` (clsx + tailwind-merge).
  - [ ] Path aliases (tsconfig `paths` + vite `resolve.alias`):
        `@repo/shadcn-ui/lib/utils` → `app/lib/utils.ts`;
        `@repo/smoothui/components/smooth-button` →
        `components/ui/smoothui/smooth-button/index.tsx`;
        `@repo/shadcn-ui/components/ui/*` → real primitives.
  - [ ] Create the shadcn primitives the components import (at least `avatar`;
        sweep for others: grep `@repo/shadcn-ui/components/ui/`). Use `radix-ui`.
  - [ ] `pnpm add clsx tailwind-merge tw-animate-css` (and anything else a
        component used by a screen imports; do NOT add `@smoothui/data` — it is
        demo data and not needed).
  - [ ] Add `--brand` / `--color-brand` / `--brand-secondary` tokens IF the
        `candy` smooth-button variant is used (else avoid that variant).
  - [ ] Ensure Paxeer Grand Sans `.otf` files exist in `public/fonts/`
        (else accept Inter fallback — note it, don't block).
- [ ] **Shared server libs** (built once, used by both surfaces):
  - [ ] `app/lib/deus.server.ts` — typed Deus API client (base from
        `env.DEUS_API_URL`, default `https://deus.paxeer.app`); helpers for
        discover/catalog/service/quote/invoke/registry/analytics/hosting/me.
  - [ ] `app/lib/auth.server.ts` — Supabase session (cookie), JWT verify,
        attach `Authorization` to Deus calls; wallet-link helper.
  - [ ] Worker secrets/vars documented: `DEUS_API_URL`, `SUPABASE_URL`,
        `SUPABASE_ANON_KEY` (+ any server-only key). NO Appwrite keys here.
- [ ] `pnpm install` clean → `pnpm typecheck` (`react-router typegen && tsc`)
      passes → `pnpm build` succeeds → `wrangler dev` boots and serves the home route.

## 5. Marketplace (public, no login) — Definition of Done

- [ ] **Home / landing** — hero + plain-language search (use `ai-input`),
      featured/top services, value props. SSR.
- [ ] **Discover / search results** — `POST /v1/discover` with query + filters
      (kind data/agent, max price, min quality, confidential); ranked result
      cards (price via `price-flow`/`number-flow`, quality/uptime badges).
- [ ] **Catalog browse** — `GET /v1/catalog` paginated grid + filters +
      `pagination` component.
- [ ] **Service detail** — `GET /v1/services/{id}` (+ `/manifest`): summary,
      description, operations, pricing, quality/uptime, tags, JSON schemas.
      SSR meta (title/summary/canonical) for SEO + sharing.
- [ ] **Try-it panel** — `POST /v1/quote/{id}` (free, shown to all) then
      `POST /v1/invoke/{id}` (gated behind a connected wallet/agent bearer);
      render result + a clean receipt summary (no raw protocol jargon).
- [ ] Wallet connect flow (read-only quote without it; invoke requires it).

## 6. Dev Dashboard (authenticated) — Definition of Done

- [ ] **Login** (Supabase) + **wallet link** for payout/signing.
- [ ] **My services** — owner-scoped list with status (draft/active/paused/
      delisted), invocations, revenue.
- [ ] **New listing flow** — manifest builder (display_name, summary,
      description, tags, operations w/ input/output JSON schema, pricing plans)
      → choose **Proxy** (`endpoint.proxy_url`) or **Hosted** (upload code).
      `POST /v1/services` → draft.
- [ ] **Hosted upload UX** — `POST /v1/services/{id}/artifact` (upload bundle)
      → "deploying to cloud…" progress → live; poll
      `GET /v1/services/{id}/deployment`; `POST /v1/services/{id}/redeploy`;
      tail `GET /v1/services/{id}/logs`. Appwrite never named in UI.
- [ ] **Publish / pause / delist** controls (`POST .../publish|pause|delist`).
- [ ] **Service analytics** — `GET /v1/services/{id}/analytics`: invocations,
      revenue, latency, quality (charts using the v3 chart tokens).
- [ ] **Earnings** — settlements + payout address (`POST /v1/services/{id}/payout`).
- [ ] **Account** — profile, wallet, caller spend (`GET /v1/me`, `/v1/me/spend`).

## 7. Deus backend — Appwrite hosting completion (Go, `deus/`)

This is the "code lands in the magical cloud" backend. Today
`deus/internal/hosting/appwrite.go` creates a function + deployment but the
**artifact upload is stubbed** (`_ = a.blobs`, `_ = artifactKey`).

- [ ] **Implement real artifact deploy** in `appwrite.go`: fetch the uploaded
      bundle from objstore (`BlobReader.Get`), package it, and create the
      Appwrite deployment with the actual `code` (multipart upload, not the
      current JSON-only POST), `activate: true`, entrypoint `src/main.js`,
      `commands: npm install`.
- [ ] **Per-service config** → Appwrite function variables (secrets), resource
      caps (timeout/memory) from `configs/limits.<env>.yaml`.
- [ ] **Record `deployments` row** (appwrite_function_id, deployment_id,
      exec_endpoint, runtime, status, always_warm).
- [ ] **Gateway hosted route** (`internal/gateway/hosted.go` + `route.go`)
      invokes the deployed function via the Appwrite executions endpoint /
      function domain; end-to-end invoke works.
- [ ] **Runner harness** (`deploy/deus/runner/node20/`) — `handle(op,args,ctx)`
      wrapper that runs inside the Appwrite function: enforces timeout/max-bytes,
      reports units/outcome, co-signs the receipt with the runner key.
- [ ] **Budget / kill-switch** honored (`DEUS_HOSTING_KILL_SWITCH`,
      `DEUS_HOSTING_MAX_ALWAYS_WARM`); new always-warm refused past ceiling.
- [ ] **Live config on deployed `deusd`**: `DEUS_APPWRITE_ENDPOINT`,
      `DEUS_APPWRITE_PROJECT_ID`, `DEUS_APPWRITE_API_KEY` pointed at
      `cloud.paxeer.app` (server-side only; via `/opt/deus/deus.env`, not repo).
- [ ] `make -C deus deus-build deus-test deus-lint deus-mcp-selftest` green;
      `hosted_flow` e2e passes (real or mocked Appwrite).

## Verification (the "job is done" gate)

> Scope: the **server-side Cloudflare Worker** that runs RRv7 SSR (via `@cloudflare/vite-plugin` + `createRequestHandler`), its **bindings**, **service-binding topology**, **isolate/startup behavior**, and the **infrastructure for 1M+ concurrent users**. This is the runtime-and-config layer only — no browser concerns.

---

## 1. Worker runtime model — internalize before configuring

The architecture decisions below all follow from how the runtime actually behaves:

- [ ] **Understand the isolate model.** Your SSR Worker runs in a V8 **isolate**, not a container/process. There is no traditional cold start (no boot, no scale-from-zero pause) — isolates instantiate in sub-millisecond-to-low-millisecond range, and Cloudflare can spin one up *during the TLS handshake* so the first request often pays ~0 added latency.
- [ ] **Know the *real* startup cost.** What you actually pay on first execution in a given PoP is **script parse + compile + top-level eval**. This is governed by the **1 s startup-time limit** and scales with bundle size and module-init work. "Optimizing cold starts" here = shrinking eval cost, not warming containers (§4).
- [ ] **Treat every invocation as stateless + ephemeral.** Isolates are evicted on inactivity/memory pressure; global state does **not** reliably persist across requests. Anything durable goes to a binding (§5). Module-global `const` is fine as a cache *hint*, never as a source of truth.
- [ ] **CPU vs wall time.** Billing/limits track **CPU time** (active execution); time waiting on `fetch`/KV/D1/R2/DO does **not** count. Design for many concurrent I/O-bound requests per isolate.
- [ ] **128 MB memory ceiling per isolate.** No large in-memory caches, no buffering big payloads; stream instead.

---

## 2. RRv7 ↔ Worker integration

- [ ] **Worker entry** (`workers/app.ts`) uses `createRequestHandler(() => import('virtual:react-router/server-build'), import.meta.env.MODE)` and `export default { fetch }`.
- [ ] **`AppLoadContext` augmented** so `cloudflare.env` (bindings) and `cloudflare.ctx` (`ExecutionContext`) reach every loader/action type-safely. No `any` at the boundary.
- [ ] **`ctx.waitUntil()`** used for fire-and-forget work (logging, cache writes, analytics) so the response isn't blocked and the isolate stays alive to finish it.
- [ ] **`ctx.passThroughOnException()`** considered for graceful-failure routes.
- [ ] **Bindings injected via `loadContext`, never imported globally** — keeps the server build portable and testable.
- [ ] **Loader fan-out budgeted** against the subrequest limit (§7); prefer one batched/cached call over many serial `fetch`es.
- [ ] **`compatibility_date` + `nodejs_compat`** set only as the framework/deps require; audit that no Node-only API leaks into the SSR path.
- [ ] Streaming SSR enabled where it helps TTFB; the Worker flushes the shell before slow loader data resolves.

---

## 3. Service Bindings & Worker topology

- [ ] **Topology decided:** SSR-monolith vs **SSR Worker → API Worker(s) over Service Bindings**. For a real API surface, split it — the SSR Worker stays a thin render/proxy tier.
- [ ] **Service Bindings, not public `fetch`, for Worker→Worker calls.** Same-machine, **no public internet hop, no egress cost, sub-millisecond**, internal-only. Never route internal traffic out through a public hostname.
- [ ] **RPC via `WorkerEntrypoint`** preferred over fetch-style bindings — native method calls with typed args/returns across the boundary (no manual request/response marshalling).
- [ ] **Auth tokens / secrets stay server-side** behind the binding; the SSR Worker attaches credentials when proxying so they never reach the client.
- [ ] **Versioning across the boundary** managed — deploy order and backward-compat between SSR and API Workers defined so a deploy can't break the contract mid-rollout.
- [ ] **Local dev parity:** service bindings resolve under `vite dev` / `wrangler dev` (known footgun — verify the multi-worker dev setup actually wires bindings, not just prod).
- [ ] **Failure isolation:** API Worker errors degrade gracefully in the SSR tier (timeout + fallback), don't cascade into 5xx on every page.
- [ ] **Smart Placement** evaluated on the data-heavy Worker so it runs near its backend/D1 primary rather than near the eyeball (cuts serial-subrequest latency).

---

## 4. Startup / "cold start" optimization

- [ ] **Bundle size minimized** — parse+eval time scales with it. Stay well under the **10 MB** Worker limit; aim far lower. Analyze the server bundle; strip server-unused deps.
- [ ] **No heavy work in module/global scope.** Defer client construction, schema compilation, large constant building, etc. to **lazy first-use inside the handler**, not top-level eval.
- [ ] **Lazy-import rarely-hit code paths** (`await import()`) so they don't inflate the initial eval.
- [ ] **Avoid large dependency graphs / polyfills**; prefer Web Platform APIs over `nodejs_compat` shims that bloat the bundle.
- [ ] **Keep top-level async work out of the critical path**; nothing should block the first response on a slow init.
- [ ] **Startup time measured** against the 1 s limit in CI (it's a hard deploy-time check — a bundle that exceeds it won't deploy).
- [ ] Smart Placement understood as a **backend-latency** optimization, *not* a startup optimization (don't conflate the two).

---

## 5. State management — bindings

### D1 (relational, SQLite-at-edge)

- [ ] **Global read replication enabled** + **Sessions API** used for all reads — without Sessions, every query hits the **single primary** regardless of replicas. No extra charge for replication.
- [ ] **Session bookmarks threaded** across a logical user session for sequential consistency ("read-my-own-writes"); choose `first-primary` vs `first-unconstrained` per flow deliberately.
- [ ] **All writes go to primary** — design for write-path latency from far regions; batch writes; keep them off the hottest read paths.
- [ ] **Replica lag is expected** — UI/logic tolerates eventually-consistent reads outside a bookmarked session.
- [ ] **Migrations** versioned, forward-only, gated in CI, run before dependent code; tested on a copy.
- [ ] **Time Travel backups** (30-day point-in-time) confirmed; restore rehearsed.
- [ ] Per-DB throughput/size limits understood; shard or split DBs before hitting them; D1 is not a high-write-contention OLTP engine.

### KV (read-heavy, eventually consistent)

- [ ] Used for **read-mostly, edge-cached** data (config, feature flags, sessions, rendered fragments) — *not* for write-heavy or strongly-consistent data.
- [ ] **Write propagation lag (seconds, global) accounted for**; never read-after-write critical state from KV.
- [ ] Value (25 MiB) / key / metadata limits respected; no oversized blobs (use R2).
- [ ] Hot-read keys lean on KV's built-in edge caching; set sensible `cacheTtl`.
- [ ] Bulk/list access patterns reviewed (list is paginated + costlier than get).

### R2 (object storage)

- [ ] Used for blobs/media/large artifacts; **zero egress fees** leveraged vs S3.
- [ ] **Presigned URLs** for direct client up/download so bytes don't transit the Worker (saves CPU/memory/subrequests).
- [ ] **Multipart upload** for large objects; streaming, never buffer in the 128 MB isolate.
- [ ] Lifecycle rules + versioning/object-lock set where retention/immutability matters.
- [ ] Public bucket vs binding-only access decided; no accidental public exposure.

### Durable Objects (coordination + per-entity state + realtime)

- [ ] **Sharding strategy explicit.** A single DO is single-threaded and handles ~thousands of connections; **1M concurrent ⇒ many DOs**, keyed to shard (per-room/per-tenant/per-user-bucket). Never funnel global traffic through one DO.
- [ ] **SQLite-backed DOs** chosen (10 GB each, Paid) where per-object storage is needed.
- [ ] **Alarms** used for scheduled/per-object work instead of external cron fan-out.
- [ ] **Strong consistency** of DO storage used only where actually required; everything else stays in KV/D1/cache.
- [ ] Hot-DO mitigation: split a hot key, add a fan-out layer, or cache in front.

---

## 6. Realtime / WebSockets at 1M concurrent (DO Hibernation)

- [ ] **WebSocket Hibernation API used** (`ctx.acceptWebSocket()`), **not** the in-memory `addEventListener` pattern. With hibernation, clients **stay connected at the edge while the DO is evicted from memory** — duration (GB-s) billing only accrues while JS is actually executing. This is the mechanism that makes 1M idle connections economically viable. (GA as of 2026; auto-response-to-close is default.)
- [ ] **Per-connection state survives hibernation** via `serializeAttachment()` / `deserializeAttachment()` — because the DO **constructor re-runs on wake** and in-memory state is wiped.
- [ ] **Constructor is cheap and idempotent** (it runs on every wake), no heavy init.
- [ ] **Every WebSocket message = one billable request** — costed into the model; chatty protocols batched/debounced.
- [ ] **Connection fan-out sharded** across DOs by room/topic; broadcast loops bounded.
- [ ] **Reconnect/backoff + resume** handled server-side (bookmark/cursor on attachment).
- [ ] Threshold check: above ~100k sustained-active (not idle) concurrent with constant traffic, validate per-message cost vs a self-managed cluster — DO hibernation wins on *idle* fan-out, not necessarily on constant high-rate throughput.

---

## 7. Concurrency, limits & capacity for 1M+

- [ ] **`limits.cpu_ms` set deliberately** (default 30 s, max 5 min Paid) — set a *low* ceiling matching real SSR cost to cap runaway invocations.
- [ ] **`limits.subrequests` reviewed** (Paid default 10,000/invocation, raisable to 10M) — but reduce fan-out via caching rather than raising the cap.
- [ ] **6 simultaneous outbound connections/request** ceiling respected — parallel `fetch`es beyond 6 queue; batch or sequence accordingly.
- [ ] **Rate limiting** layered: Cloudflare Rate Limiting at the edge + per-identity counters in KV/DO for auth/expensive endpoints.
- [ ] **Async offload to Queues** (consumer Worker) or Workflows for anything heavy/slow; the SSR request path stays sub-CPU-budget. (Queues free-tier-eligible since Feb 2026; full throughput on Paid.)
- [ ] **Idempotency keys** on all write/action endpoints — retries and double-submits are guaranteed at this scale.
- [ ] **Backpressure + bounded, jittered, idempotent retries** on every binding/external call; timeouts on all outbound calls so CPU/wall budget isn't burned waiting.
- [ ] **No single chokepoint** (one DO, D1 primary, one upstream) on the hot path — verified by failure injection.
- [ ] **Backends scale, Workers already do.** Document that Workers autoscale, but your **D1 primary, DOs, and any external origin do not** — those are the real capacity limits.
- [ ] **Load tested for concurrency** (k6), not just RPS: sustained concurrent connections, p50/p95/p99 latency, error rate, at projected peak; one run with caching disabled to find the floor.
- [ ] Account-level ceilings checked: **500 Workers/account**, 250 cron triggers, 100k static-asset files (raise via support / Workers for Platforms if approached).

---

## 8. Caching at the Worker layer (primary scale + cost lever)

- [ ] **Cache API / edge cache** in front of cacheable SSR HTML and loader data; cache key normalized (strip tracking params; correct `Vary`).
- [ ] **`stale-while-revalidate` / `stale-if-error`** on cacheable responses so origin/DB load and blips don't surface to users.
- [ ] **Tiered Cache** enabled to collapse origin requests across PoPs.
- [ ] **Private/per-user responses marked uncacheable**; no PII into shared cache.
- [ ] **Cache writes via `ctx.waitUntil()`** so they don't block the response.
- [ ] **Purge/invalidation path** exists and is tested (deploy-time + content-driven).
- [ ] **Brotli** confirmed; immutable hashed assets long-cached; HTML short/validated.
- [ ] **Cache-hit ratio tracked with an alert** — a drop is an early warning of cost + origin-overload risk.

---

## 9. Wrangler configuration

- [ ] `wrangler.jsonc` committed; **`compatibility_date` pinned**, reviewed against changelog before bumps; `compatibility_flags` minimal.
- [ ] **Secrets via `wrangler secret`/dashboard**, never `vars`. `vars` = non-sensitive only. `.dev.vars` gitignored for local.
- [ ] **Per-environment configs** (`env.staging`/`env.production`) with distinct routes, bindings, secrets, and limits.
- [ ] **All bindings declared + typed** (`wrangler types` → `Env`): D1, KV, R2, DO, Queues, Service Bindings, AI, Hyperdrive as applicable.
- [ ] **`limits.cpu_ms` and `limits.subrequests`** set (§7) for cost/runaway protection.
- [ ] **`observability` block enabled** (Workers Logs) with sampling tuned to 1M traffic + budget.
- [ ] **Smart Placement** flag set on the appropriate Worker (§3).
- [ ] **DO migrations** (`migrations` tag) declared for class add/rename/delete; deploy-ordered.
- [ ] Custom domain + routes set; `workers.dev` disabled in prod (or internal-only).
- [ ] Deploy auth = scoped CI token (least privilege), not a personal key.

---

## 10. Observability for the Worker

- [ ] **Workers Logs / Logpush** to your observability backend; sampling tuned for cost.
- [ ] **Error tracking** (Sentry CF Workers SDK) capturing SSR + binding failures with request IDs.
- [ ] **Structured JSON logs** with a request ID propagated SSR Worker → service bindings → D1/DO.
- [ ] **`wrangler tail`** verified working against prod for live incident debugging.
- [ ] **Dashboards:** RPS, error rate, p50/p95/p99, **CPU-time-per-request**, subrequest counts, per-binding latency (D1/KV/R2/DO), DO active count + Queue depth, cache-hit ratio.
- [ ] **SLOs + burn-rate alerts** wired to on-call; alerts tested.
- [ ] External multi-region synthetic/uptime monitoring.

---

## 11. Release & rollout (server runtime)

- [ ] **Gradual / versioned deployments** in prod — route a % of traffic to the new Worker version, monitor, ramp; never big-bang at 1M.
- [ ] **One-command rollback**, rehearsed; previous version retained.
- [ ] **Preview deploys per PR** (versioned/preview URLs) with isolated bindings.
- [ ] **Migration gating**: D1/DO migrations run in correct order relative to code; service-binding contract compatibility verified across the rollout window.
- [ ] **Integration tests against the Workers runtime** (`@cloudflare/vitest-pool-workers`) so bindings behave like prod, not mocks.
- [ ] Deploy markers on dashboards + change notifications to the incident channel.
- [ ] Feature flags decouple launch from deploy for risky surfaces.

---

## 12. Cost governance (Worker + bindings)

- [ ] On **Workers Paid** ($5 base; requests + CPU-time). Cost model mapped across: requests (incl. **every WS message**), CPU-time, KV ops, D1 rows-read/written + storage, R2 ops + storage (egress free), **DO duration GB-s** (hibernation is the lever), Queues ops.
- [ ] **`limits.cpu_ms` / `limits.subrequests`** capped to bound runaway cost.
- [ ] **Caching + async offload treated as the primary cost levers**, not afterthoughts.
- [ ] **DO hibernation enforced** for all idle WebSocket connections (otherwise GB-s bills 24/7 per connection).
- [ ] **Log/observability sampling** tuned — full logging at 1M req destroys budget.
- [ ] Billing alerts + per-binding cost attribution; load-test cost extrapolated to peak and signed off.

---

### Verified platform facts (Cloudflare, current)

- **Isolates, not containers:** no scale-from-zero cold start; startup cost = script parse/compile/eval, bounded by the **1 s startup limit**. CPU time counts active execution only — I/O wait is free.
- **Workers Paid limits:** CPU 30 s default / 5 min max; **128 MB** memory; **10 MB** Worker size; **10,000** subrequests/invocation (→10M via config); **6** simultaneous outbound connections/request; **500** Workers/account.
- **Service Bindings:** Worker→Worker over the internal network — no public hop, no egress fee, sub-ms; **RPC via `WorkerEntrypoint`** gives native typed method calls.
- **D1 global read replication:** GA, no extra charge, via **Sessions API** (Worker binding); without it all queries hit the single primary. Writes always go to primary. 30-day Time Travel backups.
- **KV:** eventually consistent, edge-cached, read-heavy; global write propagation in seconds.
- **R2:** zero egress fees; presigned URLs + multipart for large objects.
- **Durable Objects:** single-threaded, globally-addressable, ~thousands of conns each → shard for scale; SQLite-backed DOs cap 10 GB each. **WebSocket Hibernation API GA (2026):** clients stay connected at edge while DO is evicted; GB-s billed only during active JS; constructor re-runs on wake; per-connection state via `serializeAttachment`/`deserializeAttachment`; each WS message is a billable request.
- **Queues:** free-tier-eligible since Feb 2026.

## 13. Swarm decomposition (parallel execution plan)

Andrew authorized parallel mode + a subagent swarm. Dependency order:

1. **Phase 0 (sequential, one agent, no parallel):** Section 4 — Cloudflare
   conversion + component shim + shared `deus.server.ts` / `auth.server.ts` +
   green typecheck/build. Everything else imports these, so build them first to
   avoid merge conflicts.
2. **Then fan out (parallel, independent file sets):**
   - **Agent MARKETPLACE** → Section 5 (public routes under
     `app/routes/` + marketplace-only components).
   - **Agent DASHBOARD** → Section 6 (authed routes; separate route files).
   - **Agent HOSTING-BACKEND** → Section 7 (Go, in `deus/` — fully disjoint
     from the frontend; can start immediately, even during Phase 0).
3. **Integrate + verify (sequential):** Section 8.

Keep agents on disjoint file sets. The Go backend (Agent HOSTING-BACKEND) shares
no files with the frontend and can run from the very start in parallel with
Phase 0.
