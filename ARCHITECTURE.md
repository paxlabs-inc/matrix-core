# Centra AI Architecture

This is the high-level map for contributors, current as of 2026-08-04
(Centra AI `1.0.0`). The
canonical detailed sources of truth are:

- **How to work in this repo** — `protocol/spec/workflow.kvx` (rendered by
  `protocol/spec/specgen`; the per-IDE rule files are generated pointers).
- **Feature requirements / design / tasks** — one `protocol/spec/<feature>/spec.kvx`
  per feature, rendered to `requirements.md` / `design.md` / `tasks.md`.
- **Durable cross-session memory** — cortex (recall runs at session start).
- **History** — `CHANGELOG.md` and the early design record under `research/`
  and `knowledge/matrix.kvx` (both are historical; the specs win).

If anything below contradicts a `spec.kvx`, the spec wins.

## The system in one paragraph

Centra runs **one agent per user — Neo** — on a per-user daemon service, with
**one deterministic evidence process per user — Neocortex (`cortexd`)** when
the new substrate is selected. Neo is a conversational
tool-using agent loop; every other agent-shaped thing (the Cassandra
controller, the `/cody` coding workbench, proactive automations, the
morning brief) is a lens or a mode of Neo, not a separate agent. Neocortex
preserves conversation, intent, work, evidence, beliefs, and checkpoints as
typed events and deterministic projections. The Go Cortex remains an intact
compatibility/default path until an explicit owner-approved cutover. A central
**router** authenticates users, provisions and wakes their
daemon, and reverse-proxies to it. Money is native: agents hold no keys and
spend through the Paxeer embedded wallet and **LayerX** (USDX ledger + the LXP
HTTP payment protocol). All user data at rest is being sealed by the **vault**
(envelope encryption, fail-closed in production).

## Layered model

```text
                    client (Next.js web application)
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
   |   - memory seam   ------------------+------------------------------------           |
   |                                     v                                               |
   |                  cortexd / Neocortex (selected substrate)                           |
   |       typed evidence log · BLAKE3 MMR · sealed records · intent/work ledger         |
   |       deterministic LMDB projections · exact recall · activation · checkpoints      |
   |                    cortex (compatibility / rollback path)                            |
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
| `agents/neo/`       | **The agent.** HTTP+SSE server engine, staged agent loop, tools manager, memory pager, proactive automations, morning brief, workbench backend, sub-agent swarm, writeback consolidator. |
| `core/neocortex/` | C++23 deterministic single-writer evidence engine and `cortexd`: typed append-only actor logs, sealed payloads, BLAKE3 MMR checkpoints, replay-built LMDB projections, exact entity/vector/BM25 recall, temporal descent, intent frame, work ledger, and one activation composer. |
| `core/cortexclient/` | Go client and migration seam for `cortexd`: capability-scoped protocol, resurrection-loop interfaces, checkpoint recovery, evidence citations, bounded reconnect, and Pebble export/import. |
| `core/cortex/`    | Compatibility and rollback memory substrate on Pebble. It remains working and is the legacy migration source until the separately gated Neocortex cutover. |
| `packages/vault/`     | Envelope encryption for user data at rest: platform KEK behind a KeyProvider seam → wrapped per-user key → per-object DEKs; AES-256-GCM; three shapes (record-AEAD JSONL, whole-file AEAD, chunked streaming AEAD); fail-closed under `VAULT_REQUIRED`; cryptographic deletion by user-key destruction. |
| `executor/`  | The MCL daemon (`cmd/mcl-execute`): the signed intent pipeline (compile → plan → walk → attest, D11 determinism), MCP tool subprocess manager, daemon HTTP routes (profile, personalization, transcripts, async jobs), snapshot push/pull. Neo delegates money/rigorous work here via `core_execute`. |
| `core/mcl/`       | **Library only** (no longer a separate agent): MCL compiler, intent/plan IR, envelopes, and the shared `llm` client packages that neo and executor import. |
| `bridge/`    | Adapter wiring MCL's `Cortex` interface to a live `*cortex.Cortex`.                                       |
| `core/cassandra/` | The silent-voice controller + classic verdict adjudicator library (Neo runs the controller in-process; executor imports the critic). |
| `packages/chronos/`   | Centralized agent alarm clock (`chronosd`): cron/timezone scheduling with wake conventions `HEARTBEAT`, `AUTOMATRIX`, `MORNING_BRIEF` delivered as `/chat` wake turns. |
| `router/`    | `matrix-router`: the only public listener. Supabase/GoTrue JWT auth, per-user daemon provisioning + wake on Railway, reverse proxy, machine env injection (vault KEK gate, provider keys, snapshot policy). |
| `gateway/`   | `matrix-gateway`: OpenAI-compatible LLM metering proxy (PAX credit ledger, per-actor rate limits, free-tier whitelist, BYO bypass). |
| `packages/construct/` | Typed screen surfaces the agent renders onto the client (`construct_render`, Ask back-channel, surfacestore persistence). |
| `protocol/codegraph/` | Agent-native code graph (model/store/extract/retrieve) — the structural self-model source.                |

`packages/sandboxd/` (Node) rounds out the deployed set: the Railway sandbox and
branded-preview plane the dojo disposable desktop boots on.

### Services that live outside this repo

Both are live and both are reached from the daemon through an MCP stdio bridge
in `protocol/tools/`, never by importing their Go packages.

| Service  | Role                                                                                                     |
| -------- | -------------------------------------------------------------------------------------------------------- |
| LayerX   | `layerxd`: the USDX (6dp) payments ledger — accounts, transfers, holds (authorize/capture/release), ref binding, receipts. The custody/concurrency authority for agent payments. |
| Deus     | Agent-services registry, discovery, and execution gateway on **LXP** (`lxp/1`): HTTP 402 challenge → DID-signed intent → layerxd settle → `X-LayerX-Receipt`. |

## Neo, the unified agent

- **One loop** (`agents/neo/internal/agent`): staged turn — `prepareTurn` /
  `prepareWindow` (the ONE window-assembly site) / `generate` / `deliberate` /
  `closeTurn` / `act` — over a reified per-turn struct. 1M-token window,
  byte-stable prompt prefix for cache hits.
- **Evidence and continuity**: under Neocortex, the resurrection loop records
  user, assistant, delivery, tool, intent, checkpoint, and consolidation events
  through `cortexclient`. `cortexd` composes the next activation from faithful
  conversation, current intent, reconciled work, exact recall, and temporal
  descent. The legacy `cortex.Activate` path remains available for rollback.
- **Epistemic core**: premise ledger with provenance, prediction-carrying
  tool dispatch, convergence-by-measurement termination; capability surface
  and self-model rendered resident in the prompt.
- **Cassandra 2.0**: an in-process controller that edits the agent's prior
  assistant message in place (doubt/assurance) — no terminal completion gate.
- **Tool surface** (`agents/neo/internal/tools`): one `tools.Manager` over the MCP
  pool + synthetic tools (`memory_recall`, `spawn_subagents`,
  `construct_render`, `todo`, `workspace_preview`, `save_personalization_profile`,
  `core_execute` when escalated). Restricted advertised sets per mode:
  sub-agents (no money/recursion), proactive automation (no value transfer), morning
  brief (positive read-only allowlist).
- **Autonomy**: Centra automations capture implied opportunities into a cortex queue
  and runs them supervised on the restricted surface during idle wakes; the
  ORACLE morning brief is a scheduled, profile-driven, policy-checked
  restricted run delivered as a durable conversation turn + inbox record.
  Boundary: the three non-negotiables (no monetary, reputational, or
  psychological damage); autonomous surfaces structurally exclude money tools.
- **Workbench and coding**: the `/cody` client route is Neo-owned — projects are
  `/workspace/<dir>` subdirs, editable CodeMirror buffers over the
  `/workspace/*` API, previews in on-demand Railway sandboxes. Neo's native
  filesystem, bounded shell, durable services, read-only Git, task list, and
  coding checkpoint own execution. AgentCore Build dispatch is retained only
  as dormant compatibility code and is disabled, so `build_project` is not
  visible to Neo.
- **Durability**: task ledger (`agents/neo/internal/task`) supervises every run and
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
idle). The per-user image bakes `neo`, `mcl-execute`, `cortexd`, the agent
manifest, MCP servers, and Playwright+Chromium for the local browser bridge.
When `NEO_MEMORY_SUBSTRATE=neocortex`, the entrypoint creates the private
capability-scoped configuration, starts `cortexd`, waits for its Unix socket,
and only then starts Neo. Per-user state
snapshots (vault-encrypted tarballs) push to MinIO. LLM traffic goes through
the gateway (main/cheap lanes currently MiMo; grok on the Cassandra lanes).

`deploy/railway` is the only per-user daemon image. The earlier single-box and
Fly Machines topologies have been retired out of the tree.

## Clients

- `client` — the Next.js web app: chat with live SSE workspace ("Neo's
  Computer": tool steps, searches, browser filmstrip, todos, construct
  surfaces), the `/cody` workbench, wallet/leash pages, settings (automations,
  morning brief + personalization interview, notifications), inbox. Five
  locales. House rules: separation by background contrast only — no border
  strokes, emojis, gradients, or glow.
- `apps/mobile` — personal; off-limits to agents.

## Where to start reading

- How work happens here: `CLAUDE.md` → `protocol/spec/workflow.kvx` → the active
  feature's `protocol/spec/<feature>/spec.kvx`.
- Neo: `agents/neo/internal/agent/agent.go` (loop) → `agents/neo/internal/server/engine.go`
  → `agents/neo/internal/tools/tools.go` → `neo/internal/memory/pager.go`.
- Neocortex: `core/neocortex/src/` → `core/neocortex/cmd/cortexd/` →
  `core/cortexclient/client.go` → `agents/neo/internal/runtime/loop/neocortex.go`.
- Cortex compatibility path: `core/cortex/cortex.go` → `cortex/store/store.go` →
  `core/cortex/activate.go` → `core/cortex/replay/replay.go`.
- Vault: `packages/vault/` (crypto core) → `cortex/store/vaultseam.go` →
  `agents/neo/internal/conversation/store.go` (record-AEAD JSONL in practice).
- Payments: the money lane from this side is `tools/layerx/layerx.mjs` and
  `tools/deus/deus.mjs`; the ledger and gateway themselves live outside this
  repo.
- Executor/MCL: `executor/cmd/mcl-execute/daemon_cmd.go` → `core/mcl/mtx/spec.md`.
- Deploy: `deploy/railway/`.
