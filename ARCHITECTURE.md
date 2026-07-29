# Matrix Architecture

This is the high-level map for contributors, current as of 2026-07-17
(Matrix `0.65.0`). The
canonical detailed sources of truth are:

- **How to work in this repo** — `spec/workflow.kvx` (rendered by
  `spec/specgen`; the per-IDE rule files are generated pointers).
- **Feature requirements / design / tasks** — one `spec/<feature>/spec.kvx`
  per feature, rendered to `requirements.md` / `design.md` / `tasks.md`.
- **Durable cross-session memory** — cortex (recall runs at session start).
- **History** — `CHANGELOG.md` and the early design record under `research/`
  and `knowledge/matrix.kvx` (both are historical; the specs win).

If anything below contradicts a `spec.kvx`, the spec wins.

## The system in one paragraph

Matrix runs **one agent per user — Neo** — on a per-user daemon service, with
**one shared memory brain per user — cortex**. Neo is a conversational
tool-using agent loop; every other agent-shaped thing (the Cassandra
controller, the `/cody` coding workbench, Automatrix proactive tasks, the
morning brief) is a lens or a mode of Neo over the same cortex, not a separate
agent. A central **router** authenticates users, provisions and wakes their
daemon, and reverse-proxies to it. Money is native: agents hold no keys and
spend through the Paxeer embedded wallet and **LayerX** (USDX ledger + the LXP
HTTP payment protocol). All user data at rest is being sealed by the **vault**
(envelope encryption, fail-closed in production).

## Layered model

```text
                 apps/client (Next.js web; apps/mobile is personal/off-limits)
                        |  chat SSE · workspace trace · /cody workbench · settings
                        v
   +--------------- router (public :443, JWT auth, per-user provisioning) ---------------+
                        |                                            wakes/proxies
                        v
   +------------------------------- per-user daemon service -----------------------------+
   |                                                                                     |
   |   neo (the agent)                          executor / mcl-execute daemon            |
   |   - internal/server  HTTP+SSE engine  <--> - core_execute delegate (signed MCL walk)|
   |   - internal/agent   staged turn loop      - /profile /personalization routes       |
   |     (prepare/generate/deliberate/act)      - transcripts, async jobs, snapshots     |
   |   - Cassandra 2.0 silent-voice controller  - MCP tool subprocesses                  |
   |   - tools.Manager (one MCP surface)                                                 |
   |   - memory pager  ------------------+------------------------------------           |
   |                                     v                                               |
   |                          cortex (one Pebble brain per user)                          |
   |            journal MMR/SMT roots · 9-type memories · HNSW · activation               |
   |            temporal ladder · sessions · recursive recall · self-model               |
   |                                     |                                               |
   |                       vault (KEK -> user key -> per-object DEKs)                     |
   |            record-AEAD JSONL · whole-file AEAD · streaming AEAD (media/snapshots)   |
   +-------------------------------------------------------------------------------------+
          |                |                 |                  |
          v                v                 v                  v
     chronos           gateway            sandboxd            layerx / deus
   (alarm clock:    (LLM metering,     (dojo sandboxes,     (external services:
    HEARTBEAT,       PAX credit         branded             USDX ledger + holds,
    AUTOMATRIX,      ledger)            previews)           service registry/LXP)
    MORNING_BRIEF)
```

## Go modules (top level)

Each has its own `go.mod` (sibling `replace` directives where they import
each other) and is independently buildable/testable.

| Module       | Role                                                                                                     |
| ------------ | -------------------------------------------------------------------------------------------------------- |
| `neo/`       | **The agent.** HTTP+SSE server engine, staged agent loop, tools manager, memory pager, Automatrix, morning brief, workbench backend, sub-agent swarm, writeback consolidator. |
| `cortex/`    | Per-user typed memory brain on Pebble: append-only journal with MMR/SMT integrity roots (`OverallRoot`, §13.4 replay invariant), 9-type taxonomy, HNSW vectors, activation composer, temporal-ladder rollups, recursive recall, sessions, self-model records. Values are vault-sealed **below** the hash boundary (roots stay computed over plaintext logical encodings). |
| `vault/`     | Envelope encryption for user data at rest: platform KEK behind a KeyProvider seam → wrapped per-user key → per-object DEKs; AES-256-GCM; three shapes (record-AEAD JSONL, whole-file AEAD, chunked streaming AEAD); fail-closed under `VAULT_REQUIRED`; cryptographic deletion by user-key destruction. |
| `executor/`  | The MCL daemon (`cmd/mcl-execute`): the signed intent pipeline (compile → plan → walk → attest, D11 determinism), MCP tool subprocess manager, daemon HTTP routes (profile, personalization, transcripts, async jobs), snapshot push/pull. Neo delegates money/rigorous work here via `core_execute`. |
| `MCL/`       | **Library only** (no longer a separate agent): MatrixScript compiler, intent/plan IR, envelopes, and the shared `llm` client packages that neo and executor import. |
| `bridge/`    | Adapter wiring MCL's `Cortex` interface to a live `*cortex.Cortex`.                                       |
| `cassandra/` | The silent-voice controller + classic verdict adjudicator library (Neo runs the controller in-process; executor imports the critic). |
| `chronos/`   | Centralized agent alarm clock (`chronosd`): cron/timezone scheduling with wake conventions `HEARTBEAT`, `AUTOMATRIX`, `MORNING_BRIEF` delivered as `/chat` wake turns. |
| `router/`    | `matrix-router`: the only public listener. Supabase/GoTrue JWT auth, per-user daemon provisioning + wake on Railway, reverse proxy, machine env injection (vault KEK gate, provider keys, snapshot policy). |
| `gateway/`   | `matrix-gateway`: OpenAI-compatible LLM metering proxy (PAX credit ledger, per-actor rate limits, free-tier whitelist, BYO bypass). |
| `construct/` | Typed screen surfaces the agent renders onto the client (`construct_render`, Ask back-channel, surfacestore persistence). |
| `codegraph/` | Agent-native code graph (model/store/extract/retrieve) — the structural self-model source.                |

`sandboxd/` (Node) rounds out the deployed set: the Railway sandbox and
branded-preview plane the dojo disposable desktop boots on.

### Services that live outside this repo

Both are live and both are reached from the daemon through an MCP stdio bridge
in `tools/`, never by importing their Go packages.

| Service  | Role                                                                                                     |
| -------- | -------------------------------------------------------------------------------------------------------- |
| LayerX   | `layerxd`: the USDX (6dp) payments ledger — accounts, transfers, holds (authorize/capture/release), ref binding, receipts. The custody/concurrency authority for agent payments. |
| Deus     | Agent-services registry, discovery, and execution gateway on **LXP** (`lxp/1`): HTTP 402 challenge → DID-signed intent → layerxd settle → `X-LayerX-Receipt`. |

## Neo, the unified agent

- **One loop** (`neo/internal/agent`): staged turn — `prepareTurn` /
  `prepareWindow` (the ONE window-assembly site) / `generate` / `deliberate` /
  `closeTurn` / `act` — over a reified per-turn struct. 1M-token window,
  byte-stable prompt prefix for cache hits.
- **Memory**: continuous-memory path only — `cortex.Activate` composes the
  working context, `AppendMessage` persists every turn, `RecallDescend` is the
  recursive descent behind `memory_recall`. The writeback consolidator
  promotes durable facts/preferences/corrections out-of-band.
- **Epistemic core**: premise ledger with provenance, prediction-carrying
  tool dispatch, convergence-by-measurement termination; capability surface
  and self-model rendered resident in the prompt.
- **Cassandra 2.0**: an in-process controller that edits the agent's prior
  assistant message in place (doubt/assurance) — no terminal completion gate.
- **Tool surface** (`neo/internal/tools`): one `tools.Manager` over the MCP
  pool + synthetic tools (`memory_recall`, `spawn_subagents`,
  `construct_render`, `todo`, `workspace_preview`, `save_personalization_profile`,
  `core_execute` when escalated). Restricted advertised sets per mode:
  sub-agents (no money/recursion), Automatrix (no value transfer), morning
  brief (positive read-only allowlist).
- **Autonomy**: Automatrix captures implied opportunities into a cortex queue
  and runs them supervised on the restricted surface during idle wakes; the
  ORACLE morning brief is a scheduled, profile-driven, policy-checked
  restricted run delivered as a durable conversation turn + inbox record.
  Boundary: the three non-negotiables (no monetary, reputational, or
  psychological damage); autonomous surfaces structurally exclude money tools.
- **Workbench**: the `/cody` client route is Neo-owned — projects are
  `/workspace/<dir>` subdirs, editable CodeMirror buffers over the
  `/workspace/*` API, previews in on-demand Railway sandboxes.
- **Durability**: task ledger (`neo/internal/task`) supervises every run and
  resumes orphans at boot (briefs resume on their own restricted path);
  conversations, traces, settings, and inboxes are sidecar stores on `/data`,
  all vault-sealed.

## Money

Neo holds no signing keys. Interactive Neo acts directly through the Paxeer
embedded-wallet tool lane (`paxeer__*`; policy enforced wallet-side). The
current Paxeer surface uses `https://stats.paxscan.io` as its default EVM RPC
and exposes direct RPC, PaxScan, price reads, wallet transfer/approval, and
caller-specified contract calls. Retired Argus/PaxSpot endpoints, agent-economy
precompiles, and hard-coded PECOR, Sidiora, HyperPax DEX, and perps registries
are not part of the advertised surface.
Value-moving work delegated via `core_execute` runs the signed MCL pipeline in
the executor daemon with inline approval gates / leashes. Service-to-service
payments ride LXP against layerxd (exact or hold mode); deus meters and pays
out via LayerX accounts.

## Deployment (`deploy/railway`)

One Railway project + environment holds everything: the router (public custom
domain), gateway, chronos, Postgres, MinIO, shared tool services (searxng,
gotenberg, stalwart, browser), and all per-user daemon services (serverless
sleep/wake — the proxied request is the wake; daemons stay outbound-quiet when
idle). The per-user image bakes `neo`, `mcl-execute`, the agent manifest, MCP
servers, and Playwright+Chromium for the local browser bridge. Per-user state
snapshots (vault-encrypted tarballs) push to MinIO. LLM traffic goes through
the gateway (main/cheap lanes currently MiMo; grok on the Cassandra lanes).

`deploy/railway` is the only per-user daemon image. The earlier single-box and
Fly Machines topologies have been retired out of the tree.

## Clients

- `apps/client` — the Next.js web app: chat with live SSE workspace ("Neo's
  Computer": tool steps, searches, browser filmstrip, todos, construct
  surfaces), the `/cody` workbench, wallet/leash pages, settings (Automatrix,
  morning brief + personalization interview, notifications), inbox. Five
  locales. House rules: separation by background contrast only — no border
  strokes, emojis, gradients, or glow.
- `apps/mobile` — personal; off-limits to agents.

## Where to start reading

- How work happens here: `CLAUDE.md` → `spec/workflow.kvx` → the active
  feature's `spec/<feature>/spec.kvx`.
- Neo: `neo/internal/agent/agent.go` (loop) → `neo/internal/server/engine.go`
  → `neo/internal/tools/tools.go` → `neo/internal/memory/pager.go`.
- Cortex: `cortex/cortex.go` → `cortex/store/store.go` →
  `cortex/activate.go` → `cortex/replay/replay.go`.
- Vault: `vault/` (crypto core) → `cortex/store/vaultseam.go` →
  `neo/internal/conversation/store.go` (record-AEAD JSONL in practice).
- Payments: the money lane from this side is `tools/layerx/layerx.mjs` and
  `tools/deus/deus.mjs`; the ledger and gateway themselves live outside this
  repo.
- Executor/MCL: `executor/cmd/mcl-execute/daemon_cmd.go` → `MCL/mtx/spec.md`.
- Deploy: `deploy/railway/`.
