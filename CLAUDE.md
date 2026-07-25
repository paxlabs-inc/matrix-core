# Session Starter Recall — Matrix / Neo Ecosystem
# Reorganized: 2026-07-25 | Source: original recall.kvx (339 lines)
# Chronological order within sections, newest last

# ═══════════════════════════════════════════════════════════════
# INTEGRITY
# ═══════════════════════════════════════════════════════════════

digest: 0x2f7e8a9b1c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0
canonical: matrix://memory/core
last_compact: 2026-07-25T00:00:00Z

# ═══════════════════════════════════════════════════════════════
# IDENTITY
# ═══════════════════════════════════════════════════════════════

actor_did: did:matrix:alice
workspace: /root/mem
corpus: matrix://knowledge

# ═══════════════════════════════════════════════════════════════
# HARD RULES (non-negotiable)
# ═══════════════════════════════════════════════════════════════

[hard] NEVER LIE ABOUT A MEMORY. If you don't know, say you don't know.
[hard] NEVER OMIT A MEMORY that is relevant to the current task.
[hard] NEVER FABRICATE A MEMORY to fill a gap.
[hard] When Andrew says stop/close out, STOP IMMEDIATELY: do not run another command, do not write another line of code, do not continue any in-flight verification or cleanup — halt on the very next reply. Surface the honest status instead and wait. Only resume after Andrew explicitly says to continue. — source=user_declared
[hard] Never run ANY git command on the /root/matrix dev box — not even read-only ones. The user drives all git; Matrix tracks file changes natively. Diagnose via native tools, never git. — source=user_declared
[hard] Never write stub/mock/fake test doubles or placeholder implementations to make tests pass. Test REAL code paths with REAL types/implementations. A green test driven by a fake that returns canned data is hollow. — source=user_declared
[hard] When any tool output is truncated/paginated, RETRIEVE THE FULL CONTENT before reasoning or answering. Never proceed on a partial read. A flagged information gap is a stop condition, not a footnote. — source=user_declared
[hard] FACTS OVER PREFERENCES. A fact about the world is more valuable than a preference about style.

# ═══════════════════════════════════════════════════════════════
# FIRM CONSTRAINTS (design & communication)
# ═══════════════════════════════════════════════════════════════

No emojis in any UI surface, code, or documentation.
No purple, indigo, violet, or gradient-heavy UI.
No glow, shadow, or border-stroke depth effects in UI.
No AI-default stack choices (no defaulting to Node/TS/Next/Vite/npm for greenfield).
No self-identification as the underlying LLM (no I'm Grok/Claude/GPT).
No protocol jargon in user-facing messages (no HTTP 409, SSE, JWT).
No in-band tool-result feedback in the chat stream (use guidance channel).

# ═══════════════════════════════════════════════════════════════
# PREFERENCES
# ═══════════════════════════════════════════════════════════════

prefers  = direct_communication (prefer, strength=0.95) — no preamble, no validation, no acknowledgment phrases
prefers  =  minimal_code_changes (prefer, strength=0.90) — single-line fixes over multi-line refactors when sufficient
prefers  =  root_cause_fix (prefer, strength=0.95) — fix the cause, not the symptom; no downstream workarounds
prefers  =  evidence_based_claims (prefer, strength=0.85) — cite file:line when making assertions about code
prefers  =  no_documentation_changes (prefer, strength=0.80) — don't add or delete comments or docs unless asked
prefers  =  test_before_implement (prefer, strength=0.75) — write or update tests before major implementation work
prefers  =  communication_style (prefer, strength=0.90) — direct, unsentimental, blunt; challenges claims via factual proof

# ═══════════════════════════════════════════════════════════════
# REPO BREAKDOWN — /root/matrix
# ═══════════════════════════════════════════════════════════════

## System in one paragraph
Matrix runs one agent per user (Neo) on a per-user daemon service, with one shared memory brain per user (cortex). Neo is a conversational tool-using agent loop; every other agent-shaped thing (Cassandra controller, /cody workbench, Automatrix proactive tasks, morning brief) is a lens or mode of Neo over the same cortex. A central router authenticates, provisions, and reverse-proxies. Money is native via Paxeer embedded wallet + LayerX. All user data at rest sealed by vault (envelope encryption).

## Go Modules (each has own go.mod, independently buildable)

| Module | Path | Role |
|--------|------|------|
| neo | neo/ | THE agent. HTTP+SSE engine, staged loop (prepareTurn/prepareWindow/generate/deliberate/closeTurn/act), tools.Manager, memory pager, Automatrix, morning brief, workbench, sub-agent swarm, writeback consolidator |
| cortex | cortex/ | Per-user typed memory brain on Pebble. Journal MMR/SMT roots, 9-type taxonomy, HNSW vectors, activation composer, temporal-ladder rollups, recursive recall, sessions. Values vault-sealed below hash boundary |
| vault | vault/ | Envelope encryption: platform KEK -> per-user key -> per-object DEKs. AES-256-GCM. 3 shapes: record-AEAD JSONL, whole-file AEAD, chunked streaming. Fail-closed VAULT_REQUIRED |
| executor | executor/ | MCL daemon (cmd/mcl-execute) signed intent pipeline (compile/plan/walk/attest, D11 determinism), MCP tool subprocess manager, daemon HTTP routes. Neo delegates money/rigorous work via core_execute |
| MCL | MCL/ | Library only. MatrixScript compiler, intent/plan IR, envelopes, shared llm client packages |
| bridge | bridge/ | Adapter wiring MCL's Cortex interface to live *cortex.Cortex |
| cassandra | cassandra/ | Silent-voice controller + classic verdict adjudicator library. Neo runs controller in-process; executor imports critic |
| chronos | chronos/ | Centralized agent alarm clock: cron/timezone scheduling with wake conventions HEARTBEAT/AUTOMATRIX/MORNING_BRIEF |
| router | router/ | matrix-router: only public listener. Supabase/GoTrue JWT auth, per-user provisioning + wake on Railway, reverse proxy, machine env injection |
| gateway | gateway/ | matrix-gateway: OpenAI-compatible LLM metering proxy (PAX credit ledger, per-actor rate limits, free-tier whitelist) |
| layerx | layerx/ | layerxd: USDX (6dp) payments ledger — accounts, transfers, holds, ref binding, receipts. Custody/concurrency authority |
| deus | deus/ | Agent-services registry + execution gateway, re-platformed on LXP (lxp/1) HTTP 402 -> DID-signed intent -> layerxd settle -> X-LayerX-Receipt |
| construct | construct/ | Typed screen surfaces agent renders onto client (construct_render, Ask back-channel, surfacestore persistence) |
| uwac | uwac/ | Universal Web App Connectors: agent-triggered consent grants for user's external apps (SHELVED, Nango data kept) |
| tachyon | tachyon/ | Agent-native Solidity/EVM toolbox service (API/MCP first). Also home of shared .kvx config parser |
| codegraph | codegraph/ | Agent-native code graph (model/store/extract/retrieve) — structural self-model source |
| cody | cody/ | DEAD. Orchestrator folded into Neo (/cody route is Neo-owned workbench). Awaits owner-approved deletion (morpheus task 1.3) |

## Key Directories (non-Go)

| Path | Purpose |
|------|---------|
| apps/client/ | Next.js web app: chat SSE, workspace trace, /cody workbench, settings, wallet, 5 locales. House rules: no border strokes/emojis/gradients/glow |
| apps/mobile/ | Personal/off-limits to agents |
| agents/ | Agent manifests (JSON) neo.json (main), cody.json, default.json, forge.json, gideon.json, paxeer.json |
| tools/ | MCP tool servers (23) browser, chronos, cortex-mcp, desktop, deus, e2e, exec, forge, gideon, kindle, layerx, machinemail, media, paxeer-cloud, paxeer, sandbox, scaffold, searxng, skills, tachyon, uwac, voice, websearch |
| skills/ | 195 SKILL.mtx+md files — agent-invocable skill definitions |
| spec/ | Feature specs (31) one spec.kvx per feature, rendered to requirements.md/design.md/tasks.md. THE source of truth for work |
| deploy/ | Deployment configs: railway/ (current), box/ (historical), daemon/ (historical), plus per-service dirs (browser, chronos, deus, gateway, layerx, livekit, router, tachyon, telegram) |
| rules/ | Per-IDE rule files (generated by specgen, never hand-edit) |
| docs/ | Documentation |
| research/ | Historical design records |
| knowledge/ | Durable knowledge files (MATRIX.kvx, models.kvx, etc.) |
| graph/ | Code graph data + self-model artifact |
| dojo/ | AGON benchmark (taxonomy, schema, golden, scoring, calibration, dq, aqi, suite1) |
| construct/ | Construct surface store, backchannel, schema, transport |
| protocol/ | Chain data, paxeer embedded wallets, wallet protocols |
| website/ | Matrix marketing website |
| infra/ | Infrastructure configs |
| journal/ | Journal data |
| vault/ | Vault module (Go) |
| bridge/ | Bridge module (Go) |
| marketplace/ | Marketplace configs |
| models/ | Model configs |
| scripts/ | Utility scripts |
| bin/ | Built binaries |
| temp/ | Temporary files (handoffs, test outputs) |

## Neo Internal Structure (neo/internal/)

| Package | Purpose |
|---------|---------|
| agent/ | THE agent loop (96 files). agent.go = Chat loop, staged turn struct, epistemic core, Cassandra controller, governor, close chain, signals, identity, automatrix, morning brief, interview, voice, overflow, narrate, premise, prediction, taskgraph, semantic, heartbeat, death, capabilities, prompt.go |
| server/ | HTTP+SSE engine (engine.go, server.go, handlers) |
| tools/ | tools.Manager — one MCP surface + synthetic tools (memory_recall, spawn_subagents, construct_render, todo, workspace_preview, save_personalization_profile, core_execute) |
| memory/ | Memory pager (pager.go) — continuous-memory path, Activate/RecallDescend |
| llm/ | LLM client (client.go) — streaming SSE, provider-aware, MiMo/Qwen/GLM support |
| config/ | Config (config.go) — continuous-memory, automatrix, vault, workspace settings |
| writeback/ | Consolidator — promotes durable facts/preferences/corrections out-of-band |
| delegate/ | core_execute delegate client — attaches to running intents on 409 |
| dojo/ | Dojo integration — Railway sandbox lifecycle, bytebotd bridge |
| o1/ | O1 failure replay and qualification |
| conversation/ | Conversation store (vault-sealed JSONL) |
| automatrixsettings/ | Automatrix settings store |
| automatrixlog/ | Automatrix log store |
| briefsettings/ | Brief settings store |
| briefhistory/ | Brief history store |
| machinemailsettings/ | MachineMail settings store |
| telegramsettings/ | Telegram settings store |
| task/ | Task ledger (supervises every run, resumes orphans at boot) |
| trace/ | Trace store (workspace events) |
| runrecord/ | Run record store |
| recall/ | Recall store |
| sandbox/ | Sandbox integration |
| preview/ | Preview integration |
| notify/ | Notification (ntfy/Apprise/noop backends) |

## Cortex Internal Structure (cortex/)

| Package/File | Purpose |
|---------------|---------|
| cortex.go | Main cortex struct, New(), core operations |
| store/ | Pebble store (store.go) + vault seam (vaultseam.go) + write batch |
| journal/ | Append-only journal with MMR integrity (journal.go) |
| snapshot/ | SMT/MMR roots, OverallRoot computation (snapshot.go, smt.go, mmr.go) |
| activate.go | Activate — per-turn working context composer |
| recall.go | RecallDescend — recursive descent recall |
| session.go | Session/transcript store (AppendMessage, Transcript) |
| story.go | BuildStorySoFar/LoadStorySoFar |
| compact.go | Compact — summarize-and-link, never truncate |
| context.go | Context — bundle composer (Pinned, FrameRelevant, Outcomes) |
| salience/ | Salience scoring (recency R=exp(-dt/90d)) |
| edges.go | Edge records (relationships between memories) |
| memory/ | Memory types (9-type taxonomy) |
| query/ | Query interface |
| embed/ | Embedding support |
| vector/ | HNSW vector index |
| forms/ | Form definitions |
| replay/ | Replay support |
| keys/ | Key management |
| scope/ | Scope enforcement |
| self/ | Self-model records |
| provenance.go | Provenance tracking |
| rebuild.go | Rebuild operations |
| repair.go | Repair operations |
| sweep.go | Sweep operations |
| cascade.go | Cascade operations |
| rollup.go | Temporal ladder rollups |
| lexical.go | Lexical index (BM25) |
| supersede.go | Supersede operations |
| ratelimit.go | Rate limiting |
| goal_state.go | Goal state management |
| update_head.go | Head update operations |
| episodic_backfill.go | Episodic backfill |

# ═══════════════════════════════════════════════════════════════
# CRITICAL AGENT CONTEXT (for new agents / sessions)
# ═══════════════════════════════════════════════════════════════

## Key Entry Points for Code Reading
Neo agent loop: neo/internal/agent/agent.go (Chat method)
Neo server: neo/internal/server/engine.go
Neo tools: neo/internal/tools/tools.go
Neo memory: neo/internal/memory/pager.go
Neo prompt: neo/internal/agent/prompt.go
Cortex main: cortex/cortex.go
Cortex store: cortex/store/store.go
Cortex activation: cortex/activate.go
Cortex journal: cortex/journal/journal.go
Cortex snapshot: cortex/snapshot/snapshot.go
Vault crypto: vault/vault.go
Vault seam: cortex/store/vaultseam.go
Executor daemon: executor/cmd/mcl-execute/daemon_cmd.go
Router: router/cmd/ (main.go)
Gateway: gateway/cmd/ (main.go)
Deploy: deploy/railway/

## LLM Provider Routing
Main conversational: zai-org/GLM-5.2 on Baseten (reasoning OPT-IN via enable_thinking)
Cheap/consolidation: accounts/fireworks/routers/glm-5p1-fast
Cassandra: separate lane (currently grok)
MiMo: mimo-v2.5 on api.xiaomimimo.com/v1 (OpenAI-compatible, omni for vision)
Local dev: Docker Model Runner at http://127.0.0.1:12434/engines/v1 (ornith-1.0-9b-gguf Q4_K_M)

## Database / Storage
Pebble (embedded KV) per-user cortex brain (one Pebble instance per user)
Postgres: router (beta/invite/consent tables), gateway (PAX credit ledger), layerx (accounts/transfers/holds), deus (services/registry), chronos (alarms), executor (async jobs)
MinIO: per-user state snapshots (vault-encrypted tarballs)
Redis: shared cache (searxng, stats, rankings, reactions, holders, pressure, platform)
SQLite: some local dev / lightweight stores

## Deployment Topology
ONE Railway project + environment holds EVERYTHING
Router: public custom domain + private :8088 admin/wake listener
Gateway/chronos/Postgres/MinIO: private-only services
Shared tool services: searxng, gotenberg, stalwart, browser (Playwright+Chromium)
Per-user daemon services: Serverless (sleep on 10min outbound inactivity, wake on any traffic incl. private-network)
CRITICAL: daemon must be OUTBOUND-QUIET when idle (MATRIX_SNAPSHOT_INTERVAL=-1s)
Per-user image bakes: neo, mcl-execute, agent manifest, MCP servers, Playwright+Chromium
Per-user VM: 8 vCPU, 16GB RAM, 50GB ephemeral storage

## Money / Payments Architecture
Neo holds NO signing keys
Interactive Neo: Paxeer embedded-wallet tool lane (paxeer__*) — policy enforced wallet-side
Value-moving work: delegated via core_execute -> executor daemon -> signed MCL pipeline with inline approval gates/leashes
Service-to-service: LXP against layerxd (exact or hold mode)
Deus meters and pays out via LayerX accounts
Paxeer chain 125: Sei-EVM JSON-RPC at https://api.hyperpax.xyz

## Tool Surface (neo/internal/tools/tools.go)
One tools.Manager over MCP pool + synthetic tools:
memory_recall: recursive descent recall via cortex.RecallDescend
spawn_subagents: sub-agent swarm
construct_render: screen surfaces
todo: live todo checklist
workspace_preview: preview controller
save_personalization_profile: interview agent only
core_execute: money/rigorous work (delegated to executor)
MCP tools: browser, exec, fs, web-search, web-news, chronos (alarm_*), paxeer-net (wallet_*), layerx, kindle, machinemail, media, voice, desktop, sandbox, searxng, tachyon, forge, gideon, uwac, deus

## Agent Manifests (agents/)
neo.json: main agent — full tool surface including money, browser, desktop, voice
cody.json: coding workbench agent — fs/exec/git/browser/fetch/web-search, no value-moving
default.json: default agent manifest
forge.json: forge agent manifest
gideon.json: gideon validator agent manifest
paxeer.json: paxeer agent manifest

## Spec Workflow (spec/workflow.kvx)
Active feature set in spec/workflow.kvx (active_feature field)
Each feature has spec/<name>/spec.kvx + design.body.md
specgen renders requirements.md/design.md/tasks.md from the kvx
Task statuses: pending -> in_progress -> done (or blocked)
Waves: dependency-ordered groups of tasks
One task in_progress at a time

## Frozen Specs (architectural contracts)
neo/neo.frozen.kvx: Neo agent architecture (locked 2026-06-08)
cassandra/cassandra.frozen.kvx: Cassandra verdict model
construct/construct.frozen.kvx: Construct surface contract
chronos/chronos.frozen.kvx: Chronos alarm conventions
layerx/layerx.frozen.kvx: LayerX payment protocol
deus/deus.frozen.kvx: Deus service registry
tachyon/tachyon.frozen.kvx: Tachyon EVM toolbox

## Skills (195 in skills/)
Agent-invocable SKILL.mtx+md files covering: paxeer-*, kindle-*, machine-mail, using-the-desktop, gideon-ops, tachyon-engineer, accessibility, agent-eval, agent-harness-construction, autonomous-agent-harness, browser-qa, code-quality, continuous-agent-loop, deep-research, eval-harness, security-review, verification-loop, and 170+ more.

## Critical Invariants
cortex OverallRoot = ComputeOverallRoot(journalRoot, stateRoots{memories,edges}) — commits to journal MMR root
D11 replay byte-identity: the signed MCL walk must produce byte-identical output on replay
Vault values sealed BELOW the hash boundary — roots computed over plaintext logical encodings
Prompt-cache stability: byte-stable system prefix at index 0, dynamic tail appended AFTER transcript
One task in_progress at a time (agent discipline, not enforced by code)
No false success — never attest completion that did not happen
Stop on user directive — honor STOP immediately, surface honest status

# ═══════════════════════════════════════════════════════════════
# ARCHITECTURE DECISIONS (durable, cross-project)
# ═══════════════════════════════════════════════════════════════

## Core Platform Vision
decision  =  Matrix unified-agent vision (Andrew, 2026-07-10) ALL Matrix agents (Neo, Cody, Cassandra, future) converge on ONE shared cortex memory brain per user — one durable journal, one SMT-anchored state, one temporal ladder, one activation composer. No per-agent memory silos. The cortex IS the user's memory; agents are transient lenses over it.
decision  =  Matrix strategic direction (Andrew, 2026-07-21) move agents AWAY from bespoke MCP/API tool reliance toward operating human interfaces like human users — browser as primary actuator + machine-mail email identity. MCP shrinks to a minimal substrate (browser, mail, fs); integrations become skills over human interfaces.
decision  =  Identity spine (Andrew, 2026-07-08) The Matrix agent identity system has a single spine — cortex Identity records (pinned, durable, SMT-anchored). Neo's agent_name + preferredName + expertiseDomains, Cody's mode-tiered policy, and all future agent personas read from the SAME cortex Identity record.

## Cassandra (Gate Controller)
decision  =  Cassandra redesign (Andrew, 2026-07-07) becomes a CONTINUOUS SILENT-VOICE sidecar inside Neo's agent loop — observes the working transcript, injects guidance-channel steering (NOT terminal gates), only escalates to stop-and-ask when the agent is about to violate a hard constraint. The old terminal-gate posture is retired for Neo but KEPT for Cody's worker gate.
decision  =  Cassandra 2.0 redesign (Andrew, 2026-07-08) exports BOTH surfaces: the new silent-voice controller (cassandra.Controller) for Neo, and the classic verdict adjudicator (cassandra.Adjudicate/cassandra.Verdict) for Cody's worker gate.
decision  =  Cody Cassandra 2.0 port (Andrew, 2026-07-08) Cody's gate package adopts the NEW cassandra 2.0 Verdict/Sound/IsGrounded/Adjudicate surface. The old gate.Verdict/gate.Adjudicate/gate.Screen* are retired.

## Cody (Coding Agent)
decision  =  Cody feature (2026-07-02, user-locked) new-gen coding agent as NEW top-level cody/ module importing neo internals — NOT a Neo persona, NOT from-scratch. Cody prime NEVER writes code — it Plans, Specs, Delegates.
decision  =  Cody UI direction LOCKED (Andrew, 2026-07-05) /cody is a SIBLING app surface at apps/client/components/matrix/cody/ — its own shell, Workspace/History/Settings pages, web-first. Mode-tiered disclosure.
decision  =  Cody brainstorm Q2 (Andrew, 2026-07-03) must SYSTEMICALLY enforce anti-default stack/design boldness — Engineer/Architect MUST pick the best-fitting backend language/framework.
decision  =  Cody project-context injection source (Andrew, 2026-07-03) must come from CORTEX, NOT from static context files like AGENTS.md/.cursorrules.
decision  =  Cody defaultFastWorkerModel = accounts/fireworks/routers/glm-5p1-fast.
note  =  CODY MISSION (Andrew, 2026-07-05) Cody PLANS, SPECS, and DELEGATES — never writes code itself. Spawns ONE worker subagent per task, independently verifies the turn-in.

## Neo (Main Agent)
decision  =  Neo Q2 task (2026-07-05, Andrew) Q2 priority is the CONTINUOUS-MEMORY collapse — finish collapsing neo/internal/memory/pager.go into cortex, retire legacy methods, make cortex the single brain.
note  =  Neo prod runs the CONTINUOUS-MEMORY code path (cfg.ContinuousMemory=true). The legacy pager path is still compiled but NOT exercised in prod.
reasoning_config  =  Neo pins zai-org/GLM-5.2 on Baseten, reasoning is OPT-IN via chat_template_args.enable_thinking. With thinking off, GLM emits chain-of-thought as visible content.

## Transport & Infrastructure
decision  =  Cody/Railway migration (user-locked 2026-07-03) ONE Railway project+environment holds EVERYTHING. Per-user services run Serverless with sleep/wake. CRITICAL: daemon must be OUTBOUND-QUIET when idle.
note  =  Per-user Railway VM sizing (Andrew, 2026-07-03) each per-user daemon service gets 8 vCPU, 16GB RAM, 50GB ephemeral storage.
note  =  Railway plan limits (checked 2026-07-03) no documented per-project service-count cap on Pro/Enterprise.
note  =  Matrix beta gating: provisioning today is LAZY — router proxy only calls admin.StartProvision on the user's first authenticated chat request.

# ═══════════════════════════════════════════════════════════════
# ACTIVE / RECENTLY COMPLETED FEATURES (chronological)
# ═══════════════════════════════════════════════════════════════

## Wave 1: Foundation Specs (Late June 2026)
spec/kindle-autonomy: 16 EARS reqs, 7 task groups, 11 waves. KindleLaunch MCP bridge for Paxeer chain 125.
spec/neo-smoothness: 7 EARS reqs, 6 task groups, 3 waves. Invisible guidance channel, narrate-before-act, live todo surface, overflow file, Plan-vs-Act mode.
spec/neo-execution-reliability: 12 reqs, 7 task groups, 7 waves. Error taxonomy/retry, transparency, concurrency/idempotency, memory consistency.
spec/continuous-memory: 12 EARS reqs R1-R12, 9 task groups, 8 waves. Self-managing memory brain, temporal ladder, recursive recall.

## Wave 2: Core Platform (Early July 2026)
spec/cody (2026-07-02) 15 EARS reqs, 9 task groups, 8 waves. Coding agent architecture.
spec/cody-client (2026-07-03) 17 EARS reqs, 8 task groups, 22 leaf tasks. /cody sibling app surface.
spec/automatrix (2026-07-02) 9 EARS reqs, 8 task groups, 7 waves. Neo proactive/autonomous surprise tasks.
spec/cody-launch (2026-07-05) 14 EARS reqs, 25 tasks, 8 waves. Audit-driven fixes (Y1-Y9, N1-N2, C1-C5, X1-X2).
spec/cassandra-silent-voice (2026-07-08) 12 EARS reqs, 7 task groups, 5 waves. Silent-voice controller for Neo.
spec/agon (2026-07-07) 17 EARS reqs, 7 task groups, 11 waves. AGON = Matrix Agent Qualification standard.
spec/neo-onboarding (2026-07-03) 10 EARS reqs. 3-screen onboarding, invite-code gate, provisioning overlap.
spec/cody-smoothness (2026-07-03) Activity spine, history, compose, run lifecycle. All waves verified done.

## Wave 3: Infrastructure & New Directions (Mid July 2026)
spec/neo-workbench (brainstorm-locked, Andrew) Cody/Neo workspace surface. Spec-only session.
spec/deus-layerx (2026-07-13) 12 EARS reqs, 6 task groups, 7 waves. Re-platform deus payments onto LayerX via LXP.
spec/oracle (2026-07-15) Half A = shared vault module (envelope encryption), Half B = opt-in personalization (interview, morning brief).
spec/morpheus (2026-07-13) Neo's successor — IN-PLACE, STAGED re-architecture of neo/internal/agent. 5 moves: one memory path, reified turn struct, staged loop, one governor, Morpheus identity via config.
spec/epistemic-core (2026-07-12, locked with Andrew) Root-cause fix for assume-then-pursue failure class. Layer 0 residency, premise ledger, prediction-carrying dispatch, reified task graph.
spec/deja-vu (2026-07-16) Automatic episodic recall — trigger lexicon, referent extraction, 3-lane retrieval (HNSW + provenance + BM25, RRF-fused).

## Wave 4: External Projects (Mid-Late July 2026)
spec/paxeer-indexer: 12 reqs, 8 waves. Single-writer EVM ingestion with checkpoints/reorg.
spec/voice (completed 2026-07-13) Waves 1-6 all done. LiveKit rooms, microphone controls, voice preferences.
spec/launch-readiness: Privacy (PRIV-01), chat management (CHAT-01), auto briefs (AUTO-01). Beta launch prep.
spec/layerx-perps: 10 waves. Full perps engine, Crossverse adapter, deterministic risk/pricing, delegation enforcement.
spec/dojo (2026-07-24) 5 waves. Disposable desktop on Railway sandboxes from pinned bytebot-desktop:edge.
human-interface-agents (2026-07-21) browser-primary external action, executor-only legacy, machine-mail identity.

# ═══════════════════════════════════════════════════════════════
# INFRASTRUCTURE & DEPLOYMENT FACTS
# ═══════════════════════════════════════════════════════════════

## Paxeer Chain
Paxeer chain 125 is a Sei-EVM JSON-RPC at https://api.hyperpax.xyz (chain-id 125, Sei fork since 2026-06-30).
USDL=0x85FcD13735F4309833A503EE804ea32395851479, PECOR/DEX router=0x63380c384296EeD6AB39379269622156F05D1111, WPAX9=0xD152891923C7D6fE84d3DCF58621aB2be0eFCbc2.
eth_getLogs capped at 2000 blocks per query on chain 125.
SettlementAnchor=0x63F317750ff18272565249345c63E5688501EA1D (deploy block 2784084), LayerXVault=0xf756895fD414f7D20413B61c9291ABe98fcED1CE (deploy block 2784092).

## Server Inventory
7 NVMe validators across EU (Hetzberg DE, Hetzner FI, OVH FR, netcup DE).
1 non-validator full node (Hetzner AX102).
1 LayerX dedicated box (89.116.30.132, 5950X/128G/2x1.92T NVMe).
1 Matrix dev box m19581 (AMD EPYC 7282 32T, 251GB RAM, no GPU).
Docker Model Runner serves OpenAI-compatible API at http://127.0.0.1:12434/engines/v1 (model: ornith-1.0-9b-gguf Q4_K_M).

## DOJO / Disposable Desktop
decision  =  live view = SCREENSHOT-POLL, not VNC — sandbox has no privateHost. GET /dojo/frame returns one JPEG. Frames are PASSIVE (never bump idle clock).
decision  =  user power over disposable desktop is FIRST-CLASS, not agent-gated. POST /dojo/boot and POST /dojo/shutdown with user_shutdown reason.
Pinned image: ghcr.io/bytebot-ai/bytebot-desktop@sha256:e974e7ac93c8f755e98b8a437e5497a63175c7096b51a348f5f1d9320598a1c9
Omni grounding: MiMo v2.5 model id 'mimo-v2.5' on api.xiaomimimo.com/v1. Coordinate convention NORMALIZED 0-1000 both axes. Single-pass 11/11 hits, 0.3-3.7px error.

## Crossverse / LayerX Perps
Crossverse LIVE deployment (data-api.crossverse.app) LAGS the authoritative frozen protocol doc. Symbol-service perp_stats emits legacy names. Adapter parses authoritative primary with legacy fallbacks.

# ═══════════════════════════════════════════════════════════════
# KNOWN ISSUES & FIXES (grounded incidents)
# ═══════════════════════════════════════════════════════════════

## Critical / P0 Fixes (shipped)
P0 identity leak (2026-07-07) Neo leaked model identity ('I'm Grok') in streamed thinking. Fixed: identity scrub in agent/identity.go, config filter in server/config_filter.go.
Browser-filmstrip prod bug (2026-07-11) @playwright/mcp heartbeat killed every browser session ~5s after creation. Fixed: incremental SSE drain + per-session standalone GET listenLoop.
Neo dropped answer bug (2026-07-05) completion gate path skipped final content flush. Fixed: flush final assistant content before adjudication.
Cody 'context deadline exceeded' (2026-07-05) http.Client{Timeout:120s} killed streaming SSE. Fixed: Timeout=0, idleTimeoutReader watchdog.
wireMessage.Content omitempty (2026-07-05) xai 422 missing field content killed workers. Fixed: always emit content.
SPARK-launch bug (NE-13) delegate hard-returned 409 with no attach-to-existing. Fixed: attach/track refactor + existingIntentID parse.
Loopty-loop death spiral (3 root causes) bare answers poisoned cortex, guardUnreadOverflow unbounded, MiMo tool-call tag leakage. All fixed.
Neo looping (2026-07-05) bundled-completion skipped adjudication. Fixed: adjudicate BEFORE running sibling tool.

## Architecture-Level Known Gaps
NE8  =  unsupervised extraction populates PINNED blocks — false positives silently pin wrong behavior. No human confirmation gate.
NE9   = memory bloat/fracturing — dedup best-effort, fails open to NEW under flaky classify model.
NE10  =  inconsistent learned-behavior application — top-8 salience-ranked, can silently fall out.
NE7  =  pinned block recomputed every loop step  150-250 cortex scans/turn for a block that rarely changes mid-turn.
Neo non-convergence (2026-07-04, 2026-07-07) burned ~1M tokens in reasoning spirals. Three gaps: no cumulative token budget, churn-blind stall guard, no mid-run interrupt. Fix deferred pending user scope.

# ═══════════════════════════════════════════════════════════════
# RECENT OUTCOMES (newest first — last ~2 weeks)
# ═══════════════════════════════════════════════════════════════

## 2026-07-25 (today)
success  =  Tavily search default restored — open-web lookup now defaults to web-search lane, browser reserved for site-specific workflows. Fixed incoherent-context: human-interface-agents prune had removed web-search AND fetch from agents/neo.json while prompt.go still referenced them.

## 2026-07-24 (yesterday)
success  =  DOJO wave-3 addendum #2 — root cause of 'computer not showing anywhere' fixed at Shell layer: dashboard ALWAYS mounts Neo's Computer as environment centerpiece.
success  =  DOJO wave-3 ADDENDUM — user power over desktop is FIRST-CLASS. POST /dojo/boot and /dojo/shutdown shipped.
partial  =  DOJO wave-3 CODE-COMPLETE, verification PARTIAL. Transport = screenshot-poll decided.
success  =  DOJO wave-2 complete — desktop bytebotd bridge + desktop_look omni-vision + a11y ladder.
success  =  DOJO wave-1 complete — omni grounding probe + sandbox lifecycle manager.
success  =  DOJO spec authored at spec/dojo. Disposable desktop on Railway sandboxes.
success  =  Native MiMo tool adapter shipped — qwen3_xml tag grammar as authoritative MiMo tool parser + reasoning_content passthrough.

## 2026-07-21
decision  =  human-interface-agents scope CORRECTION — paxeer-net chain/wallet lane essential and restored.
decision  =  Matrix strategic direction — agents operate human interfaces like human users.

## 2026-07-20
success  =  Crossverse LIVE deployment lag captured. Adapter rework COMPLETE.
success  =  layerx-perps waves 6-7 done, wave 8 code-complete with real TS-client e2e green.

## 2026-07-19
incident  =  Matrix Neo incident — matrix-gateway crash-looped due to Postgres network reachability. Root cause = stale/wrong private IP.

## 2026-07-17
partial  =  launch-readiness task 3.5 AUTO-01 shipped. Task 4.1 CHAT-01 backend complete.

## 2026-07-16
decision  =  DEJA-VU spec AUTHORED + set active. Automatic episodic recall with 3-lane retrieval.

## 2026-07-15
success  =  ORACLE waves 7-9 COMPLETE. Morning brief, interview, briefhistory, client Settings.
success  =  ORACLE spec-only session authored.
success  =  oracle task 2.3 — cortex value encryption below the hash boundary.

## 2026-07-14
success  =  MORPHEUS waves 3-5 done. Turn struct, staged loop, guidance choke point, termination guard chain, unified signal state, governor.

## 2026-07-13
success  =  MORPHEUS waves 1-2 done. ContinuousMemory flag retired, legacy pager deleted with proof.
success  =  epistemic-core check-before-act false-refutation fixed.
success  =  DEUS-LAYERX waves 1-4 complete. Holds ledger, USDX pricing, LXP middleware, gateway hold mode.
success  =  VOICE waves 4-6 complete. Router token/auth, composer voice mode, real-seam evidence.

## 2026-07-12
decision  =  epistemic-core spec authored + set active. Root-cause fix for assume-then-pursue failure class.

## 2026-07-11
success  =  browser-filmstrip FEATURE COMPLETE. All waves 1-5 green.
success  =  Neo run-death fix set shipped.
success  =  Agent Wallet page shipped.
success  =  Matrix website hero animation + 22 navbar/footer pages built.

## 2026-07-10
decision  =  Matrix unified-agent vision — one shared cortex memory brain per user.

## 2026-07-08
decision  =  Cassandra 2.0 redesign + Cody port.
decision  =  UWAC runtime SHELVED. Nango/Activepieces data KEPT as reference.
decision  =  New feature 'flywheel' — Cody finds bugs, files cortex Issues, Neo picks up in Automatrix idle windows.
success  =  Paxeer embedded-wallet durable action orchestrator built.

## 2026-07-07
success  =  Cassandra mechanism REFINEMENT — silent voice, not terminal gate.
success  =  AGON spec authored + taxonomy expanded (24->59 leaves).
success  =  AGON waves 3-4: provider registry, cache, sandbox isolation.
success  =  AGON P0 foundations (waves 1-2) complete.
has_flaw  =  Live prod probe — Neo leaked model identity (P0, fixed same-day).
has_flaw  =  Live prod probe — non-convergence, ~1M tokens burned.

## 2026-07-05
success  =  CODY/NEO/CASSANDRA AUDIT — 16 issues found (Y1-Y9, N1-N2, C1-C5, X1-X2).
success  =  cody-launch spec authored (14 EARS reqs, 25 tasks, 8 waves).
success  =  Neo looping (2nd incident) fixed.
success  =  Cody screenshot-screen wedge fix.
success  =  Cody verification-observability fix.
success  =  Cody wire bug fixed (wireMessage.Content omitempty).
success  =  GLM-5.2 'thoughts leaking into chat' fixed.
decision  =  Cody UI direction LOCKED.

## 2026-07-04
success  =  Neo runaway diagnosis — 3 grounded gaps identified.
success  =  Cody screenshot-screen evidence-based fix.
partial  =  Neo onboarding tasks 4.1-4.7 implemented, 5.x tests remain.

## 2026-07-03
success  =  Cody brainstorm sessions (Q2 + Q3 + frontend).
success  =  cody-client spec authored (17 EARS reqs, 22 leaf tasks).
success  =  cody-smoothness waves 1-4 verified done.
success  =  Neo onboarding spec + tasks 1.1-3.1 implemented.
success  =  Cody/Railway migration architecture locked.

## 2026-07-02
success  =  Cody feature spec authored.

## 2026-07-01
success  =  continuous-memory spec authored + waves 1-5 done.
success  =  continuous-memory session handoff sessions (3 total).
note  =  Andrew deliberately STOPS agents mid-task for context hygiene — honor STOP immediately.

## Late June 2026
success  =  kindle-autonomy spec authored.
success  =  neo-smoothness spec authored + wave 1 done.
success  =  neo-execution-reliability spec authored + Cluster A foundation done.
success  =  KindleLaunch MCP bridge built (P1-P3).
success  =  KindleLaunch platform-critical-bugfixes designed.
success  =  construct-os-shell MVP first slice (tasks 1-9) complete.
success  =  AGON spec authored.
success  =  Moltbook-class failure fixed (3 fixes).
success  =  Neo prompt-window portability fix.
success  =  UI-layer replay dedup shipped.
success  =  Money-path + supervisor honesty fixes.
