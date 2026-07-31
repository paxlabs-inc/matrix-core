# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Cross-references in parentheses point to either `knowledge/matrix.kvx` session
entries (`sess#NN`), research decision IDs (`D1`-`D18`), or — from 0.27.0
onward — feature specs under `spec/<feature>/` (the kvx is the authoritative
record of each feature's requirements, design, and task statuses). Versions
from 0.26.0 onward are feature milestones dated by completion; they are not
git tags.

## [Unreleased]

## [0.90.0] - 2026-07-31 — WORKFORCE: seven-department stateless organization (in progress)

### Added

- **WORKFORCE — a deterministic, user-controlled organization runtime**
  (`spec/workforce/`, waves 1-7 mostly complete; 24 of 28 tasks done):
  a separate top-level Go module and execution model for long-horizon
  organizational work, distinct from Neo's conversational agent loop.
  - **Organization and control plane**: seven first-class departments
    (Developer, Executive, Research and Development, Marketing and Social,
    Legal, Accounting, Back Office), each with three durable seats (Lead,
    Executor, Auditor) carrying versioned mandates, allowed skills, data
    scopes, and escalation rules; a signed organizational ledger with
    provenance and correction fan-out; a deterministic `workforced` kernel
    owning the global work graph, dependency resolution, scheduling,
    leases/fencing, and a single fenced effect gateway (no seat process
    ever holds effect credentials).
  - **Stateless wake workers**: every Lead/Executor wake spawns a fresh,
    isolated `workforce-seat` process bound to a typed WorkPacket
    (mandate, goal slice, tools, model binding, policy, lease, budget,
    deadline); no transcript, scratchpad, or episodic memory survives
    between wakes. The Workforce Execution Loop implements durable
    lease/reconcile/orient/select/propose/compile/preflight/execute/
    observe/verify/commit/yield/sleep stages. Auditors (`workforce-auditor`)
    are memoryless per verdict with a cross-audit disagreement sampler.
  - **Coordination fabric**: Workforce Mail Protocol for typed internal
    mail, a Chronos-driven wake/lease scheduler, compiled autonomy and
    approval policies (signed, non-delegable authority stays with the
    human owner), reconciliation probes and circuit breakers around
    ambiguous effects.
  - **Developer Project Brain**: the sole cross-wake persistence exception
    — a project-scoped CodeGraph plus verified engineering decisions,
    invariants, failures, plans, changes, and test evidence attached to
    the codebase, not to any agent persona; change-scope leases and a
    real Developer skill pack qualified across a multi-wake loop.
  - **Department skill packs**: real capability packs shipped for
    Executive, Research & Development, Marketing & Social, Legal,
    Accounting, and Back Office, qualified together in a seven-department
    Work Order.
  - **APIs, live events, and command center**: authenticated owner APIs
    for organization activation, Work Orders, graph/mail/approvals/
    receipts/policies/schedules/incidents, and Project Brain; a dedicated
    `apps/client` Workforce route with guided empty-ledger activation,
    department/seat state, budgets, autonomy, schedules, and receipt-backed
    completion (no success inferred from in-flight activity); initial
    Mail/Approvals/Receipts/Policies/Project Brain record surfaces.
  - **Security qualification**: adversarial real-process isolation tests
    proving seats cannot reach effect credentials, other seats' sessions,
    other tenants, private Auditor state, or unauthorized Project Brain
    data.
  - **Remaining before a 1.0.0 release** (`spec/workforce/spec.kvx`
    tasks 24/26/27/28, tracked `pending`): the full Mail/Approvals/
    Receipts/Policies/Project Brain client surface set; encrypted backup,
    point-in-time recovery, retention, and cryptographic erasure for the
    ledger, policies, receipts, mailboxes, and Project Brain; enforced
    latency/memory/resource budgets with overload shedding and
    breaker-storm/load/leak qualification; and the complete adversarial
    Workforce property and release-qualification gate (Property WORKFORCE)
    spanning all seven departments end-to-end.

## [0.65.0] - 2026-07-17 — Launch readiness

### Added

- **ORACLE — sealed user data + opt-in personalization** (`spec/oracle/`,
  2026-07-15, Half B complete; Half A core complete):
  - New top-level `vault/` module: envelope encryption (platform KEK behind a
    KeyProvider seam → wrapped per-user key → per-object DEKs, AES-256-GCM),
    three ciphertext shapes (record-AEAD JSONL preserving O(1) append +
    torn-tail crash recovery, whole-file AEAD for tmp+rename JSON, chunked
    streaming AEAD for media/snapshots), versioned self-describing headers,
    KEK rotation without data rewrite, fail-closed boot under
    `VAULT_REQUIRED` (router injects it in prod).
  - Store encryption wired across the fleet: neo conversations/traces/tasks/
    settings/inboxes/conv-meta, construct surfaces, executor transcripts/
    async jobs/intent envelopes, and cortex Pebble VALUES sealed **below**
    the hash boundary (leaf/SMT hashes over plaintext; `OverallRoot`
    byte-identity proven live, across replay, and across in-place migration).
    Daemon snapshot tarballs sealed before S3/MinIO push with decrypt-verify
    restore. Permission sweep to 0700/0600 across data stores.
  - Personalization (default OFF): one versioned tagged pinned cortex
    profile record with daemon `GET/PUT/DELETE /personalization` + export;
    guided five-group skippable interview in normal chat
    (`POST /interview/start`), confirmation-gated
    `save_personalization_profile` advertised only to interview agents,
    writeback extraction structurally excluded from interview conversations.
  - Morning brief: `briefsettings` sidecar + dedicated `MORNING_BRIEF`
    Chronos convention (one idempotent alarm at the user's local time,
    DST-correct), fail-closed wake gates (disabled/paused = zero network,
    day/date guards, never twice per local date), supervised run on a
    POSITIVE read-only tool allowlist (money/signing/fs/deploy/exec
    structurally absent), task-ledger durability with a dedicated
    restricted-surface resume path, durable conversation turn + inbox record
    before any notification, content policy (≤5 source-backed news items,
    ≤2 rotating recommendations, facts-vs-recommendations, disclosure) with
    a deterministic post-check and a durable no-repeat/feedback history
    (`neo/internal/briefhistory`, `POST /brief/feedback`).
  - Client: morning-brief Settings section (schedule, sections, pause,
    interview entry, profile view/export/delete), i18n ×5 locales.
  - Remaining in-spec: media streaming-seal tests (2.4), migration + plaintext
    scanner (waves 5), deletion pipeline + training-consent lineage (wave 6),
    Half-B property/E2E suite (6.1).
- **MORPHEUS — the agent core reassembled** (`spec/morpheus/`, 2026-07-14,
  waves 1-5): single memory path + one-window law (legacy pager machinery
  deleted with liveness proofs); a reified per-turn struct owning ALL turn
  state (zero manual zeroing across respawns); the Chat loop staged into
  `prepareTurn` / `prepareWindow` / `generate` / `deliberate` / `closeTurn` /
  `act` with exactly ONE window-assembly site. Remaining: sediment deletions
  (awaiting owner YES), termination guard chain, guidance choke point,
  governor waves.

### Changed

- **Paxeer network surface corrected for the current chain**:
  - Default and alternate EVM RPC endpoints now resolve to
    `https://stats.paxscan.io`.
  - Removed the retired Argus portfolio and PaxSpot market-data endpoints.
  - Removed obsolete agent-economy precompile addresses and the retired
    PECOR, Sidiora, HyperPax DEX, and perps contract registries.
  - Removed the corresponding tools from runtime dispatch and from the Neo,
    default, and Paxeer agent manifests so nonexistent capabilities are no
    longer advertised.
  - Retained direct RPC, PaxScan, price reads, wallet transfers and approvals,
    ERC-20 transaction builders, and caller-specified contract calls.
- **Release documentation refreshed**: the architecture and README now
  describe the current unified Neo deployment, encrypted user-data boundary,
  and reduced Paxeer tool surface.

## [0.5.0] - 2026-07-16 — VOICE: full-duplex speech

### Added

- **VOICE — full-duplex speech for the agent** (`spec/voice/`, 2026-07-16,
  all six waves complete): a real spoken conversation with barge-in, riding
  the EXISTING text pipeline — spoken turns ARE chat turns (same `/chat`, same
  SSE, same cortex record, same thread).
  - **Coherent speech seams** (`tools/media`): `transcribe_audio` implemented
    via `mimo-v2.5-asr` and registered at every model-facing site (the phantom
    tool is gone); `generate_speech` re-pointed MiMo-primary
    (`mimo-v2.5-tts`, built-in voices, style instruction, no 512-char cap)
    with Novita fallback; the client `useChat.ts` audio-drop fixed so
    `tool.media` `kind: audio` renders via `AudioPlayer` live and
    rebuilt-from-trace.
  - **Audio-native turns**: `POST /chat` accepts an audio attachment ref;
    `NEO_VOICE_MODE=native` (default) attaches the utterance as an
    `input_audio` part on the ONE user turn so the model hears tone, with a
    bounded async `mimo-v2.5-asr` lane producing the durable transcript that
    cortex / history / recall / Cassandra / the client thread operate on;
    `asr_first` mode is byte-identical to a typed turn. Window-law invariants
    and text-only turns proven byte-identical with the feature enabled; ASR
    failure fails open to a marker + media ref.
  - **The voice worker** (`tools/voice`, new): a per-user colocated LiveKit
    Agents sidecar (Silero VAD + multilingual turn detection, barge-in,
    agent-state, room `voice:<conversation_id>`) ported from the proven Gideon
    worker; passthrough-STT node → `NeoBridge` (`/upload` + `/chat` over
    loopback, `/events` sentence-flush fold) → streaming `mimo-v2.5-tts`
    (pcm16 24kHz) per flushed sentence, with barge-in cancel and fail-open TTS.
  - **Transport + auth**: a shared self-hosted `livekit-server` service in the
    single Railway project; the router mints HS256 LiveKit join tokens behind
    Supabase auth (room scoped to the requesting user's conversation) — no
    Python token server; the worker self-mints from machine env and connects
    ON DEMAND, tearing down after `VOICE_IDLE_DISCONNECT_S` so Railway
    serverless sleep re-engages.
  - **Client voice mode**: a mic toggle on the real ChatComposer (router token
    fetch → `livekit-client` room join → autoplay gesture, `useAudioDevices`
    device pick), a compact voice strip reading the standard LiveKit
    agent-state attribute, spoken turns rendered as normal thread messages
    (transcript + audio chip), voice + base-style settings, degrade-to-text on
    any failure; i18n ×5 locales, house-rules clean.
  - **Deploy + proof**: `deploy/railway` bakes the worker (pinned deps, VAD /
    turn-detector models prefetched, no dev `.venv`), entrypoint supervision
    gated on `NEO_VOICE_ENABLED` + LiveKit env; `voice.session` broker events
    + per-turn latency metrics; adversarial E2E + property suite on real
    components (real worker subprocess vs a real Neo server, `input_audio` on
    the wire, every failure mode degrading to a completed text turn, idle
    teardown, live MiMo proofs env-gated — no fakes).

## [0.35.0] - 2026-07-13 — DEUS-LAYERX: LXP payments + Neo consolidation

### Added

- **LXP (`lxp/1`)** (`spec/deus-layerx/`): HTTP-native payments on LayerX —
  plain 402 challenge with embedded LayerX nonce → client ed25519 DID-signs
  the intent → server submits to layerxd → `X-LayerX-Receipt`. USDX-native
  pricing (6dp micro-USDX); layerx holds ledger
  (authorize/capture/release, payer-authorized captor, TTL sweep, reserve +
  conservation properties on real Postgres); ref binding on pay/hold
  preimages; gateway exact + hold rails incl. hosted execution; payee DIDs +
  `/v1/me/earnings` LayerX block with withdraw link-out; `deus.mjs`
  leash-gated client (`LAYERX_MAX_SPEND_USDX` / `LAYERX_MAX_DAILY_USDX`,
  cross-implementation signature vectors); middleware kits `pkg/lxp` (Go) and
  `runner/src/lxp.js` (Node); E2E + custody property proofs (gateway balance
  byte-identical across success/failure/replay; rail-down = 503 with zero
  free executions).

### Removed

- Deus legacy payment rails: `internal/{channels,settlement,wallet,streams}`,
  `PaymentChannel.sol`, voucher receipts, stream/settle routes — deleted
  behind the custody proofs; docs swept to the lxp/1 posture.

### Changed

- **Neo consolidation**: MCL, Cassandra, and Cody are GONE as separate
  agents — Cassandra 2.0 runs in-process inside Neo, the `/cody` workbench is
  Neo-owned, MCL survives as a library. Legacy `cody/` dir + deploy wiring
  are dead pending deletion (morpheus 1.3).

## [0.34.0] - 2026-07-12 — EPISTEMIC-CORE

### Added

- `spec/epistemic-core/` complete: the root-cause fix for the
  assume-then-pursue failure class. Context window re-based to the true 1M
  tokens; the full structural self-model capability surface rendered
  byte-stable into the prompt prefix (truth resident at generation time);
  premise ledger with provenance (`cited|assumption`) + check-before-act
  dispatch gate; self-claims resolved deterministically from the resident
  self-model; prediction-carrying dispatch (every probe declares an
  expectation; mismatches are belief-update events; 3 consecutive
  per-strategy mismatches force a tools-stripped revision); reified task
  graph with computable evidence-delta convergence replacing churn-blind
  stall as the primary termination signal.

## [0.33.0] - 2026-07-11 — NEO-WORKBENCH

### Added

- `spec/neo-workbench/` complete: Neo takes over the coding front end behind
  the `/cody` route (one workbench, Neo identity). Workspace API
  (`/workspace/*`: tree, file read, atomic conflict-checked write, diff,
  bounded exec) + project registry (`/projects`, projects are
  `/workspace/<dir>` subdirs); bolt.diy-shaped client shell with inline
  action cards, live-typing CodeMirror editor (real write path, never silent
  clobber), tabbed buffers, warm-charcoal theme; live previews + unsafe exec
  in on-demand Railway sandboxes (never the user's main VM); agent
  workspace-awareness (workspace root + active project injected into the
  prompt, `workspace_preview` tool, deploy-only-when-asked).

## [0.32.0] - 2026-07-11 — BROWSER-FILMSTRIP

### Added

- `spec/browser-filmstrip/` complete: Neo's Computer shows a live-feeling
  browsing screen. Tool-produced screenshots persist out-of-band to the media
  plane (`MediaPersistFunc` seam → `/media/<id>`; the model-facing transcript
  stays clean); auto-capture after view-changing browser actions; the URL
  rides `ToolEvent.ScreenshotURL` → `tool.step` → client filmstrip with
  authed blob loading; per-user Playwright + Chromium baked into the Railway
  daemon image (loopback browser bridge); media retention GC. Root-caused and
  fixed the playwright-mcp heartbeat kill (unanswered SSE pings closed every
  browser session after ~5s).

## [0.31.0] - 2026-07-10 — SELF-MODEL

### Added

- `spec/self-model/` complete: Neo as a self-aware unified agent. Shared
  `matrix/cortex/self` persona + resolver (single identity spine across
  surfaces); structural self-model built from the code graph and consulted
  before capability claims (`memory_recall "self:"`); guards demoted to
  failsafes; safety-wall-under-merge; end-to-end bug-kill proofs (window
  re-answer, blind respawn); `GET /diag/self-model` observability.

## [0.30.0] - 2026-07-09 — CASSANDRA 2.0 (silent voice)

### Added

- `spec/cassandra-silent-voice/` shipped inside Neo: the controller observes
  the working transcript and EDITS the prior assistant message in place
  (two-sided doubt/assurance damping, guardrail counters, dual-record
  `cassandra.mod` audit events) — silent when healthy.

### Removed

- The terminal proof-of-work completion gate: `task_complete` adjudication is
  retired (absent from every advertised schema set; bare-answer turns finish
  ungated; the neo module no longer imports `matrix/cassandra`).

## [0.29.0] - 2026-07-03 — Railway migration

### Changed

- Deployment re-platformed from Fly Machines to ONE Railway
  project/environment holding the router (public domain), gateway, chronos,
  Postgres, MinIO, shared tool services (searxng, gotenberg, stalwart,
  browser), and all per-user daemon services. Per-user services run
  Serverless: the proxied request IS the wake (router `waitDaemonReady`
  absorbs cold boot + edge 502s); daemons stay outbound-quiet when idle
  (`MATRIX_SNAPSHOT_INTERVAL=-1s`). Snapshot storage moved to MinIO.

## [0.28.0] - 2026-07-02 — AUTOMATRIX

### Added

- `spec/automatrix/` shipped: Neo proactive "surprise tasks". The writeback
  consolidator captures implied opportunities (grounded, deduped,
  fail-closed financial classifier) into a cortex Goal-carrier queue;
  `chronos/internal/automatrix` jittered idle wakes (busy-check, opt-in
  re-read every fire, per-day cap); supervised execution on the RESTRICTED
  tool surface (value-transfer tools + `core_execute` structurally absent)
  resuming the origin conversation, restart-durable with bounded attempts;
  `neo/internal/notify` (ntfy/Apprise) pings after a durable
  `automatrixlog` inbox record; `/automatrix/*` control surface + client
  Settings section (queue review, financial items approval-only, inbox with
  unread badge). Property suite: idle-only / non-competing / opt-in-gated /
  bounded wakes proven on the real engine.

## [0.27.0] - 2026-07-01 — CONTINUOUS-MEMORY

### Added

- `spec/continuous-memory/` complete: cortex becomes the self-managing memory
  brain — durable provider-agnostic session store (`AppendMessage` /
  `Transcript`), `Activate` working-context composer (pinned / outcomes /
  timeline / recent, token-budget trim), deterministic temporal-ladder
  rollups (hour → day → epoch, idempotent, lazily repairable, replay-safe),
  story-so-far, and recursive recall (`RecallDescend` — the RLM insight
  applied to time). Neo collapsed onto it: the agent renders and transports;
  cortex selects, ranks, compacts, and remembers. Kill switch
  (`POST /admin/halt`) + `memory.activation` client surface.

## [0.26.0] - 2026-05-26 — sess#32 MatrixGateway + repo tooling

### Added

- **MatrixGateway service** (plan §5.15, §5.16, §10):
  - New top-level `gateway/` Go module (`module matrix/gateway`,
    stdlib-only) with `cmd/matrix-gateway` CLI, `internal/proxy`,
    `internal/ledger` (Postgres `credit_ledger` + `daily_budget_caps`
    writer), `internal/ratelimit` (per-actor token bucket),
    `internal/rates` (model→PAX/Mtoken table + `RateTableVersion`),
    `internal/auth` (Bearer + X-Matrix-Actor-DID, ED25519 sig stub),
    `internal/routing` (free-tier whitelist + BYO bypass) and shared
    `internal/types`. Endpoints: `POST /v1/chat/completions` and
    `POST /v1/embeddings` with SSE pass-through, `Health` on
    `/healthz`. Verbatim `001_credit_ledger.sql` + box-level mirror
    at `deploy/box/postgres/migrations/002_credit_ledger.sql`.
  - Free-tier whitelist: compiler slot allows `gpt-oss-120b` only;
    executor slot allows `deepseek-v4-flash` only; non-whitelist
    requests 403 with reason; `X-Matrix-BYO-API-Key: true` bypasses
    metering AND whitelist.
  - Response augments `X-Matrix-Cost-Pax`, `X-Matrix-Daily-Spent-Pax`,
    `X-Matrix-Daily-Remaining-Pax`, `X-Matrix-Rate-Table-Version`;
    cap-hit 429 carries `{"error":"budget_exhausted",…}`.
  - Deploy artefacts: `gateway/deploy/matrix-gateway.service` (systemd
    unit), `gateway/deploy/nginx-snippet.conf`, idempotent
    `gateway/deploy/install.sh`. Public nginx site
    (`deploy/box/nginx/matrix.paxeer.app.conf`) gains
    `location /gw/ → 127.0.0.1:9090/` with streaming-friendly
    `proxy_buffering off` + `chunked_transfer_encoding on` +
    `proxy_read_timeout 300s`, plus `/gw/healthz` cut-out.
  - Daemon-side wiring (`MCL/llm/llm.go`): `Config` gains optional
    `GatewayURL`, `GatewayTokenEnv`, `ActorDID`, `IntentID`, `GoalID`,
    `SlotLabel`, `KindRoute` and `OnResponseHeaders func(http.Header)`
    hook. When `GatewayURL` is set, every Decode/Stream POST is
    rewritten to `${GatewayURL}/v1/chat/completions` with
    `Authorization: Bearer ${MATRIX_GATEWAY_TOKEN}` (env var
    overridable) and the X-Matrix-* metadata stamped. Empty fields
    preserve the legacy direct-provider posture verbatim.
    Unit tests at `MCL/llm/llm_gateway_test.go` cover routing,
    direct-provider preservation, and 429 budget-exhausted
    propagation through `OnResponseHeaders`.
  - Executor daemon: new `-gateway-url` flag (defaults to env
    `MATRIX_GATEWAY_URL`); `daemonState.gatewayURL` + `actorDID`;
    new `executor/cmd/mcl-execute/llm_config.go` (`llmConfigFor`)
    central helper; `compile.go`, `synthesize.go`, `step_handler.go`
    accept gateway/cost-hook bundle and stamp the right slot label
    per call site; `daemon_pipeline.go` instantiates a per-intent
    `intentCostAccumulator` and emits a single
    `intent.cost.summary` transcript event on terminal.
  - Cost telemetry surface (`executor/cmd/mcl-execute/intent_cost.go`):
    `transcript.intent.cost` audit event helper +
    `intentCostAccumulator` (per-intent sum); new Prometheus counter
    `matrix_daemon_cost_pax_total{slot,kind_route,goal}` exposed via
    `/metrics` (`router_metrics.go`).
  - `messageRequest` carries optional `goal_id` so chat-driven runs
    aggregate costs per cortex Goal.
  - Top-level Makefile: `gateway` added to `MODULES`; `make
    build/gateway`, `tidy/gateway`, `test/gateway`, `vet/gateway`
    parallels available; `install` baking the `matrix-gateway`
    binary; `clean` covering it.
- Professional repo-root tooling: top-level `Makefile`, `.editorconfig`,
  `.gitattributes`, `.golangci.yml`, expanded `.gitignore`, `.env.example`.
- `.github/` tree: `CODEOWNERS`, `dependabot.yml`, `FUNDING.yml`, PR template,
  bug + feature issue templates, three CI workflows (`ci.yml`, `lint.yml`,
  `docker.yml`) with per-module fan-out across cortex / MCL / bridge / executor,
  the replay-invariant gate, and a SKILL.mtx corpus validation job.
- Root documentation: full `README.md` rewrite, `SECURITY.md` (disclosure
  policy + operator hardening notes), `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`
  (Contributor Covenant 2.1), `ARCHITECTURE.md`.

### Removed

- Misspelled empty `CONTRIBUTE.md` (replaced by `CONTRIBUTING.md`).

## [0.25.0] - 2026-05-25 — sess#25 deployment surface

### Added

- `deploy/daemon/`: multi-stage `Dockerfile` (golang:1.22-bookworm builder
  -> debian:bookworm-slim runtime) baking the 4 Go modules + 159-skill
  corpus + agent manifest + `mcl-execute` binary; pre-cached MCP servers
  (`@modelcontextprotocol/server-filesystem@2024.11.1`, `mcp-server-fetch`,
  `mcp-server-git`); idempotent `entrypoint.sh` with `/data` layout +
  MinIO snapshot pull; `fly.toml.tmpl` per-user Fly Machine template
  (`auto_stop_machines=suspend`, 10 GB Volume).
- `deploy/box/`: Ubuntu 24.04.4 one-shot bootstrap for the Paxeer storage
  box (MinIO + Postgres + WireGuard prep + `matrix-router` systemd unit
  scaffolding).
- Locked deployment topology (S25Q1-Q9): Fly Machines for compute,
  dedicated Paxeer box for state durability + control plane, WireGuard
  mesh between them, Supabase Auth for routing.

### Verified

- 4 modules `go build ./...` clean.
- Daemon flag list cross-checked against `entrypoint.sh` env mapping.
- Storage box (`<production-host>`, Ubuntu 24.04.4 LTS, 18 vCPU / 96 GB /
  1.4 TB) reachable and fresh; bootstrap not yet executed.

## [0.24.0] - 2026-05-24 — sess#24 daemon HTTP+SSE

### Added

- `executor/cmd/mcl-execute/daemon` long-running HTTP+SSE server
  (`daemon_cmd.go`, `daemon_server.go`, `daemon_pipeline.go`,
  `daemon_sse.go`).
- Routes: `GET /healthz`, `GET /events` (SSE), `POST /messages`,
  `GET /intents/{id}`, `POST /shutdown`.
- Bearer-token auth via `MATRIX_DAEMON_TOKEN`; single-flight enforcement
  via `sync.Mutex.TryLock` returning 409 Busy.
- SSE broker with per-subscriber buffered channels + drop-on-backpressure
  counter exposed in `/healthz`.
- Graceful drain on signal or `/shutdown` (default 30 s deadline).
- E2E sweep harness `tools/e2e/run_sweep.sh` + `aggregate_sweep.py`
  post-processor (3-run A/B/A sequence wrapping `mcl-e2e`).

## [0.23.0] - 2026-05-24 — sess#23 plan walker + materiality

### Added

- `executor/runtime/`: `walker.go` (DFS + goroutine-per-Parallel +
  first-error-wins; `ToolCall` -> `Registry.Get` -> `Tool.Call`),
  `skill_loader.go` (`matrix://skill/<slug>@<v>` -> SKILL.mtx materialisation
  via MCL parser + validator + canonical hash), `coerce.go`
  (string -> int/bool dispatch coercion verbatim port from harness),
  `marshal.go`.
- `executor/materiality/classify.go`: 8 D9 §18.1 rules
  (`RuleBudgetDelta`, `RuleNewSubAgent`, `RuleScopeExpansion`,
  `RuleNewToolNamespace`, `RuleHardConstraintRelaxed`,
  `RuleSuccessCriteriaChanged`, `RuleDeadlineShift`, `RuleAnchorFlip`).
- `executor/cmd/mcl-execute/`: `walk`, `classify`, `loader` subcommands
  driving the full lifecycle graph
  (drafting -> proposed -> clarifying ⟷ proposed -> accepted -> executing
  -> executing/accepted (correction) -> completed | failed).
- `plan_tree@1` JSON Schema grammar in `MCL/llm/model.go` for
  grammar-constrained plan synthesis.

## [0.22.0] - 2026-05-24 — sess#22 + 22b + 22c

### Added

- `executor/mcp/`: JSON-RPC 2.0 client over stdio + streamable HTTP, per-agent
  `Manager` with idempotent `Spawn` + tools/list verification at startup,
  health pinger, mock server for tests.
- `executor/tool/`: `Tool` interface, `Registry`, `MCPTool`, `NativeTool`
  placeholder, `AgentManifest` schema with `sha256:<hex>` package digests
  and `$env:NAME` credential refs.
- `agents/default.json` baseline (filesystem-mcp + fetch + git).
- `cmd/mcl-tools` CLI (`verify`/`list`/`describe`/`call`).
- Live `cmd/mcl-e2e` end-to-end harness covering 8 phases across 3 sequential
  runs (Fireworks + Fireworks-repeat + Together) with 75 assertions, real
  npx/uvx MCP subprocesses, real cortex Pebble + nomic-embed-text-v1.5
  embedder, §13.4 replay invariant asserted per run.
- `tools/skills/convert_to_mtx.py`: SKILL.md -> SKILL.mtx bulk converter,
  159/159 corpus converted and validated; `writing-plans` reserved as the
  canonical hand-authored fixture.

### Removed

- `development/` legacy ECC-era scaffolding (50 MB, replaced by ports
  under `rules/` and `agents/`).

## [0.21.0] - 2026-05-24 — sess#21 executor foundation

### Added

- `MCL/ir/plan.go` + `plan_validate.go`: `PlanTree` IR with 6 node kinds
  (sequential / parallel / step / tool_call / sub_dispatch / gate) and
  11 structural invariants.
- `MCL/envelope/`: canonical CBOR codec, `SchemaVersion uint8=1`
  mixed into `UnsignedBytes`, ed25519 sign/verify, 15 typed body structs
  (one per MCL message kind; chat.message explicitly absent per
  `research/02` thesis), JSON on-disk form with `SelfHash` cross-check.
- `executor/lifecycle/`: 8-state machine with 18 legal transitions,
  pluggable into envelope chain, history cap.

## [0.20.0] - 2026-05-24 — sess#20 MCL ↔ cortex bridge

### Added

- Third top-level Go module `matrix/bridge` with `replace` directives
  to sibling working trees.
- `bridge.go` adapter wiring `MCL.Cortex` interface to a live
  `*cortex.Cortex`; `LateBinding=false` default preserves the Phase 3
  invariant that compile-time `Find` does not journal.
- `args.go` query + `ContextOpts` builders with closed-type and closed-
  `obj_kind` validation.
- `bundle.go` deterministic `FormatBundle` for `{cortex.bundle}`
  interpolation.
- `cmd/mclc-cortex` CLI mirroring `mclc compile` against a live cortex actor.

## [0.19.0] - 2026-05-24 — sess#19 real embedder

### Added

- `cortex/embed/api_embedder.go`: OpenAI-compat `/v1/embeddings` client
  (Fireworks default, Together optional), L2-normalised, exponential
  backoff on 5xx/429.
- Lazy migration on embedder model swap via `meta/embed_model` cursor
  rewind in `StartEmbedder`.
- Empirical verification on Fireworks `nomic-ai/nomic-embed-text-v1.5`:
  768-dim, semantic geometry (cat/feline 0.6981 > cat/quantum 0.3771),
  determinism perfect across repeated calls.

## [0.18.0] - 2026-05-24 — sess#18 real LLM client

### Added

- `MCL/llm/llm.go`: OpenAI-compat client implementing
  `interpreter.LLM`; grammar-constrained decode via `response_format`
  (JSON schema or EBNF), seed pinning, provider detection
  (`accounts/fireworks/...` -> Fireworks, else slash -> Together).
- `MCL/llm/model.go`: `DefaultCompilerModel` / `DefaultExecutorModel`
  registry + `intent_frame@1` and `verb_vocab@1` JSON schemas.
- `mclc compile` wired with `-model` / `-seed` flags + graceful fallback
  to dry-run when no API key.

## [0.17.0] - 2026-05-24 — sess#17 MCL Go runtime complete

### Added

- `MCL/mtx/interpreter`: walks `§PROCEDURE` on-blocks first-match-wins,
  evaluates four condition types, interpolates `{prose}` / `{verb}` /
  `{cortex.bundle}` / `{slot.X}`.
- `MCL/ir/intent.go`: central `Intent` type + canonical JSON + sha256 hash.
- `MCL/cmd/mclc`: `compile`/`validate`/`hash`/`parse` subcommands.

## [0.16.0] - 2026-05-24 — sess#16 MCL lexer/parser/validator/canonical

### Added

- `MCL/mtx/{lexer,parser,ast,validator,canonical}/`: stdlib-only,
  91 tests across 6 packages.
- Real-corpus parse + validate green on all 4 `core/*.mtx` files +
  `skills/writing-plans/SKILL.mtx`.

## [0.15.0] - 2026-05-24 — sess#15 MatrixScript design

### Added

- `MCL/mtx/spec.md` (~580 LOC) and `mtx/grammar.bnf` (~180 LOC).
- 4 compiler-core modules: `core/verb.mtx`, `core/frame.mtx`,
  `core/pipeline.mtx`, `core/confidence.mtx`.
- First real `skills/writing-plans/SKILL.mtx`.

## [0.14.0] - 2026-05-24 — sess#14 cortex Phase 14

### Added

- `cortex/ratelimit.go`: token-bucket gates for `KindScopeViolation`
  (10/s burst 20) and `cortex.Attest` (1/s burst 5); per-(GrantedTo,
  GrantedBy) and per-(actor, intent_id) keys; `WithRateLimits` Option.
- Replay determinism preserved by construction (over-rate calls never
  journal).

## [0.13.0] - 2026-05-23 — sess#13 cortex Phase 12 salience EMA

### Added

- `salience.UpdateWeightsEMA`: 4-factor EMA pull toward (or away from)
  cited-memory factor profile; alpha = 0.05 global; `WV` held constant +
  renormalised at end.
- `KindLearnWeights` journal kind emitted atomically at `seq=attestSeq+1`
  via re-architected `store.WriteBatch` supporting multiple `AppendJournal`
  calls per `BeginWrite`.
- Per-actor weights at `meta/salience_weights` (sidecar, not in
  `OverallRoot`).
- `cortex-shell dump-weights` subcommand.

## [0.12.0] - 2026-05-23 — sess#12 cortex Phase 11.5

### Added

- `cortex/attest.go`: `cortex.Attest(IntentID, Outcome, Reason, Cited[],
  CreatedBy)` primitive with atomic batch (per-cited-URI salience
  read+bump+write + `KindAttest` journal).
- `BumpForAccess` / `BumpForCitation` / `DecrementCitation` salience helpers.
- `LateBinding=true` `Find` now bumps `AccessCount` per
  returned-after-trim candidate in same batch as `KindFind`.
- Replay walks `KindFind` + `KindAttest` to re-apply bumps in journal order.

## [0.11.0] - 2026-05-22 — sess#11 cortex replay harness

### Added

- `cortex/replay/`: `Rebuild`, `DropDerived`, `VerifyPreservesRoot`,
  `VerifyAgainstSnapshot` implementing §13.4 verbatim.
- `cortex/rebuild.go` facade + `ErrEmbedderRunning` gate.
- `cortex-shell rebuild [-verify-only]` subcommand.

## [0.10.0] - 2026-05-22 — sess#10 sub-agent scoping + UpdateHead

### Added

- `cortex/scope/` package: signed `Scope` envelopes + `Selector` set-union
  matching + ed25519 verify chain.
- `snapshot/multiproof.go`: `MultiProof` for sub-agent Merkle-proof reads.
- `Cortex.UpdateHead` for mutating Tags / Frames / DeclaredImportance /
  Visibility without bumping Data version.

## [0.9.0] - 2026-05-22 — sess#9 cortex Compact

### Added

- `Cortex.Compact` summarise-and-link with auto-protection of pinned
  items (Identity ∪ Constraint{Hard} ∪ Goal{Active}).
- `chk/<intent>/<step>` Pebble canonical + optional JSON-pretty
  filesystem mirror.
- `KindCompact` journal kind; participates in MMR via `JournalHook`.

## [0.8.0] - 2026-05-22 — sess#8 cortex Context bundle composer

### Added

- `Cortex.Context(ContextOpts) -> *Bundle` three-tier composer
  (Pinned / Outcomes / FrameRelevant) with dedup priority
  Pinned > Outcomes > FrameRelevant.
- `memory.FrameRef` + closed `obj_kind` v1 (8 kinds: service / model /
  agent / knowledge / intent / asset / plan / capability).
- `idx/frame` (all types) + `idx/actor_obj` (Event only) auto-emission.
- `ContextOpts` has **no** `Near` field — type-level refusal of cold-start
  vector recall.

## [0.7.0] - 2026-05-22 — sess#7 cortex snapshots / Merkle

### Added

- `cortex/snapshot/`: MMR (Grin/CT shape, ~2 hash ops per append) +
  SMT-256 with empty-subtree compression + `SnapshotManifest` CBOR.
- `store.JournalHook` installed in `cortex.New` so every `j/<seq>`
  auto-appends an MMR leaf atomically.
- `Cortex.Snapshot(reason)` pull-driven API + `Cortex.OverallRoot()`
  pure accessor (D11 compiler-determinism seed).
- CLI: `snapshot`, `dump-snapshot`, `overall-root`, `prove`.

## [0.6.0] - 2026-05-22 — sess#6 cortex edges + BFS

### Added

- `memory.EdgeType` (14 codes) + `EdgeRecord` canonical CBOR.
- `AddEdge` / `RemoveEdge` / `GetEdge` / `IterEdgesOut` / `IterEdgesIn`
  (atomic forward + reverse + journal; soft-delete via `Tombstoned`;
  `AddEdge` idempotent + revives tombstoned).
- `query.EdgeExpr` + cycle-safe BFS in `planCandidatesGraph`,
  `MaxHopsCap=6`.
- CLI: `add-edge`, `remove-edge`, `list-edges`, `find -from -follow`.

## [0.5.0] - 2026-05-22 — sess#5 cortex embeddings + HNSW

### Added

- `cortex/embed/`: `Embedder` interface + deterministic `HashEmbedder` stub.
- `cortex/vector/`: pure-Go HNSW with single-file persistence,
  deterministic given seed + insertion order.
- Async embedder worker (`StartEmbedder` / `StopEmbedder` /
  `DrainEmbedder`); writes never block on embedder.
- `Find` Near / NearURI with HNSW K-overshoot + post-filter Where.

## [0.4.0] - 2026-05-22 — sess#4 cortex forms + budget trim

### Added

- `cortex/forms/`: per-type deterministic `Render` (short / medium / full),
  UTF-8-safe `TruncateToTokens`.
- Write-time auto-render; `FormsOverride=true` hard-rejects oversize.
- `Find` honors `Form` + `BudgetTokens`; trim by salience asc, retain
  `OrderBy` on survivors, ≥1 relief valve.

## [0.3.0] - 2026-05-22 — sess#3 cortex Phases 1-3

### Added

- `cortex/store/`: Pebble shell + atomic `BeginWrite` batches.
- `cortex/memory/`: 9-type taxonomy + canonical CBOR + validation.
- `cortex/keys/`: byte-sort = numeric-sort key encoding (BE uint64).
- `cortex/journal/`: append-only canonical-CBOR write log.
- `cortex/query/`: predicate AST + planner using `idx/tag` then `idx/type`.
- `Cortex.Write` / `Resolve` / `Update` / `Tombstone` / `Find`.
- `cortex-shell` CLI smoke binary.

## [0.2.0] - 2026-05-22 — sess#2 skill corpus port

### Added

- 159 skills ported (131 keep + 28 adapt) of 267 source dirs, with
  `PORT_MANIFEST.json` + human `INDEX.md` + machine `INDEX.json`.
- Re-runnable porter `tools/skills/port_from_dev.sh` and index generator
  `tools/skills/build_index.py`.

## [0.1.0] - 2026-05-21 — sess#1 design lock

### Added

- D1-D18 locked design decisions (`research/00-decisions.md`).
- Repository skeleton + workspace folder taxonomy (D6).
- Initial `research/01-foundations.md` ... `06-agents.md` chapters.
- Canonical project state at `knowledge/matrix.kvx`.

[Unreleased]: https://github.com/paxlabs-inc/matrix/compare/v0.65.0...HEAD
[0.65.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.65.0
[0.25.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.25.0
[0.24.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.24.0
[0.23.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.23.0
[0.22.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.22.0
[0.21.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.21.0
[0.20.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.20.0
[0.19.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.19.0
[0.18.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.18.0
[0.17.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.17.0
[0.16.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.16.0
[0.15.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.15.0
[0.14.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.14.0
[0.13.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.13.0
[0.12.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.12.0
[0.11.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.11.0
[0.10.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.10.0
[0.9.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.9.0
[0.8.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.8.0
[0.7.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.7.0
[0.6.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.6.0
[0.5.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.5.0
[0.4.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.4.0
[0.3.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.3.0
[0.2.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.2.0
[0.1.0]: https://github.com/paxlabs-inc/matrix/releases/tag/v0.1.0
