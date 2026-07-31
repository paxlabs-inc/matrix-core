
# AGENT_RECALL.md — Workforce Feature Compaction

> For the new agent picking up `spec/workforce`. Read this first, then read `spec/workforce/spec.kvx` for task status.

---

## Matrix Platform Context

**What is Matrix?** One agent per user (Neo) running on a per-user daemon service, with one shared memory brain per user (Cortex). Neo is a conversational tool-using agent loop; every other agent-shaped thing (Cassandra controller, /cody workbench, Automatrix proactive tasks, morning brief) is a lens or mode of Neo over the same cortex. A central router authenticates, provisions, and reverse-proxies. Money is native via Paxeer embedded wallet + LayerX. All user data at rest sealed by vault (envelope encryption).

### Hard Rules (non-negotiable — from recall.kvx)

- **NEVER LIE ABOUT A MEMORY.** If you don't know, say you don't know.
- **NEVER OMIT A MEMORY** that is relevant to the current task.
- **NEVER FABRICATE A MEMORY** to fill a gap.
- **When Andrew says stop/close out, STOP IMMEDIATELY** — halt on the very next reply, surface honest status, wait for explicit resume.
- **Never run ANY git command** on the /root/matrix dev box. The user drives all git.
- **Never write stub/mock/fake test doubles** or placeholder implementations. Test REAL code paths with REAL types.
- **When tool output is truncated/paginated, RETRIEVE THE FULL CONTENT** before reasoning. A flagged gap is a stop condition.
- **FACTS OVER PREFERENCES.**

### Firm Constraints

- No emojis in any UI surface, code, or documentation.
- No purple, indigo, violet, or gradient-heavy UI. No glow/shadow/border-stroke depth.
- No AI-default stack choices (no defaulting to Node/TS/Next/Vite/npm for greenfield).
- No self-identification as the underlying LLM.
- No protocol jargon in user-facing messages (no HTTP 409, SSE, JWT).
- No in-band tool-result feedback in the chat stream (use guidance channel).

### User Preferences

- **Direct communication** — no preamble, no validation, no acknowledgment phrases.
- **Minimal code changes** — single-line fixes over multi-line refactors when sufficient.
- **Root cause fix** — fix the cause, not the symptom; no downstream workarounds.
- **Evidence-based claims** — cite file:line when making assertions about code.
- **No documentation changes** — don't add/delete comments or docs unless asked.
- **Test before implement** — write/update tests before major implementation work.
- **Communication style** — direct, unsentimental, blunt; challenges claims via factual proof.

### Go Modules (each independently buildable)

| Module | Path | Role |
|---|---|---|
| neo | neo/ | THE agent. HTTP+SSE engine, staged loop, tools.Manager, memory pager, Automatrix, morning brief, workbench, sub-agent swarm, writeback consolidator |
| cortex | cortex/ | Per-user typed memory brain on Pebble. Journal MMR/SMT roots, 9-type taxonomy, HNSW vectors, activation composer, temporal-ladder rollups, recursive recall |
| vault | vault/ | Envelope encryption: platform KEK -> per-user key -> per-object DEKs. AES-256-GCM. Fail-closed VAULT_REQUIRED |
| executor | executor/ | MCL daemon signed intent pipeline (compile/plan/walk/attest, D11 determinism), MCP tool subprocess manager |
| MCL | MCL/ | Library only. MatrixScript compiler, intent/plan IR, envelopes, shared llm client packages |
| chronos | chronos/ | Centralized agent alarm clock: cron/timezone scheduling with wake conventions |
| router | router/ | Only public listener. Supabase/GoTrue JWT auth, per-user provisioning + wake on Railway, reverse proxy |
| gateway | gateway/ | OpenAI-compatible LLM metering proxy (PAX credit ledger, per-actor rate limits, free-tier whitelist) |
| layerx | layerx/ | USDX payments ledger — accounts, transfers, holds, ref binding, receipts |
| codegraph | codegraph/ | Agent-native code graph (model/store/extract/retrieve) — structural self-model source |

### Key Entry Points

- Neo agent loop: `neo/internal/agent/agent.go` (Chat method)
- Neo server: `neo/internal/server/engine.go`
- Neo tools: `neo/internal/tools/tools.go`
- Neo memory: `neo/internal/memory/pager.go`
- Cortex main: `cortex/cortex.go`
- Executor daemon: `executor/cmd/mcl-execute/daemon_cmd.go`

### Deployment

- ONE Railway project + environment holds EVERYTHING
- Per-user daemon services: Serverless (sleep on 10min outbound inactivity, wake on any traffic)
- Per-user VM: 8 vCPU, 16GB RAM, 50GB ephemeral storage
- CRITICAL: daemon must be OUTBOUND-QUIET when idle (MATRIX_SNAPSHOT_INTERVAL=-1s)

### Spec Workflow

- Active feature set in `spec/workflow.kvx` (`active_feature` field)
- Each feature: `spec/<name>/spec.kvx` + `design.body.md`
- specgen renders `requirements.md`/`design.md`/`tasks.md` from the kvx
- Task statuses: `pending` -> `in_progress` -> `done` (or `blocked`)
- Waves: dependency-ordered groups of tasks
- **One task in_progress at a time**

### Critical Invariants

- `cortex OverallRoot = ComputeOverallRoot(journalRoot, stateRoots{memories,edges})` — commits to journal MMR root
- D11 replay byte-identity: the signed MCL walk must produce byte-identical output on replay
- Vault values sealed BELOW the hash boundary — roots computed over plaintext logical encodings
- Prompt-cache stability: byte-stable system prefix at index 0, dynamic tail appended AFTER transcript
- No false success — never attest completion that did not happen
- Stop on user directive — honor STOP immediately, surface honest status

---

## What Is Being Built

**Matrix Workforce**: a user-owned digital organization operating system. 21 durable organizational seats (7 departments × 3 roles each) coordinated by a deterministic `workforced` kernel. Not 21 chatbots — a structured company where agents execute long-horizon goals through typed, verified work cycles.

### Core Architecture (WF-ADR-001 — Accepted & Locked)

| Component | Role |
|---|---|
| `workforced` | Deterministic kernel: scheduler, dependency resolver, policy engine, leases, fencing, effects, approvals, ledger, receipts. Non-cognitive. |
| `workforce-seat` | New long-horizon cognitive loop. One isolated process per Lead/Executor wake. Fresh agent every wake — no durable brain. |
| `workforce-auditor` | Stateless per-verdict verification. No Cortex, no accumulating memory. |

**Key invariant**: Neo is NOT part of this runtime. MCL contributes typed contracts and canonical records but does not own the loop.

### Departments & Roles

| Department | Lead | Executor | Auditor (memoryless) |
|---|---|---|---|
| Developer | Technical Lead | Developer | QA/Security Engineer |
| Executive | Chief of Staff | Strategy Analyst | Risk/Decision Reviewer |
| R&D | Research Lead | Experimenter | Evidence Reviewer |
| Marketing & Social | Marketing Strategist | Content/Social Operator | Brand & Analytics Reviewer |
| Legal | Legal Analyst | Compliance Specialist | Legal Risk Reviewer |
| Accounting | Financial Analyst | Bookkeeper/Operator | Controller/Reconciler |
| Back Office | Operations Lead | Administrative Operator | Process/SLA Reviewer |

### The Seven Accepted Amendments

These are **foundational invariants**, not backlog items:

1. **WF-01 — Memoryless independent auditors**: Auditors receive only a closed `VerdictPacket`. No departmental Cortex, episodic history, or internal reasoning. Randomized cross-audits detect capture.
2. **WF-02 — Corrections propagate through provenance**: Ledger records `supersedes`, `corrects`, `retracts`, `derived_from`, `consumed_by`. Correction resolver walks transitive graph. Incomplete until all consumers reconciled.
3. **WF-03 — Complete runtime lineage**: Every receipt stamps seat DID, model ID, sampling params, MGS genome, system-prompt digest, skill digests, runtime build digest, tool registry digest.
4. **WF-04 — Global deterministic dependency resolver**: Cycle rejection, deadlock detection, topological eligibility, priority inheritance, starvation aging, per-delegation SLAs with expiry + timeout actions.
5. **WF-05 — Reconciliation probes are mandatory contracts**: Every effectful skill declares a read-only `Probe` run at lease-acquire before new work. Skills without probes marked `drift_blind`.
6. **WF-06 — Compiled approval policy**: Signed, versioned, deterministic rules. Outcomes: `auto_approve`, `batchable`, `human_required`, `deny`, `escalate`. Irreversible = default deny.
7. **WF-07 — Signed policy and fenced effects**: All policy human-signed, immutable, versioned, hash-linked. Seat processes never receive effectful credentials. Fencing tokens on every effectful op.

### Memory Architecture

- **21 isolated brains for Lead/Executor seats** — each seat gets its own Cortex DB, journal, vault identity, episodic/semantic memories, goal state, skill history, failure patterns, inbox/outbox cursors, data-access policy.
- **7 memoryless Auditor seats** — verdict-only, no accumulating memory.
- **Organization coordination through typed ledger records** (not shared memory):
  - `Handoff`, `Delegation`, `Finding`, `Decision`, `Artifact`, `Approval`, `Receipt`, `Attestation`, `Correction`
- **One exception: Developer department gets a Project Brain** — persistent CodeGraph + engineering memory attached to the codebase (not the agent persona). Fresh dev agents receive a `ProjectBrainView` in their `WorkPacket`.

### Agent Lifecycle: Fully Stateless

Every wake creates a completely fresh agent instance. Yesterday's agent no longer exists.

| Persistent across wakes | Destroyed after every wake |
|---|---|
| Organizational goal | Context window |
| Work graph and current state | Reasoning and scratchpad |
| Typed intents, artifacts, evidence | Session transcript |
| Receipts, attestations, approvals | Agent interpretations |
| Dependencies, deadlines, schedules | Learned habits, preferences |
| Budgets | Uncommitted plans, agent process |

### The Workforce Execution Loop (14 stages)

```
LEASE → RESTORE → RECONCILE → ORIENT → SELECT → PROPOSE → COMPILE
  → PREFLIGHT → EXECUTE → OBSERVE → VERIFY → COMMIT → YIELD → SLEEP
```

Key: Models propose; the deterministic kernel validates, compiles, fences, executes, verifies, and commits.

### Cross-Agent Messaging: Workforce Mail Protocol

Agents never communicate directly. `workforced` operates internal mail backed by the ledger. Messages have typed kinds (`handoff`, `delegation`, `finding`, `correction`, etc.), delivery lifecycle (`queued → delivered → opened → acknowledged → replied → resolved`), and enter the global dependency graph.

### New Module Boundary

```
workforce/
  cmd/workforced/
  cmd/workforce-seat/
  cmd/workforce-auditor/
  internal/{kernel,loop,ledger,scheduler,dependency,lease,policy,effect,reconcile,approval,audit,skills,receipt,actorstate}/
```

May import: stable MCL IR, Cortex, Vault, Chronos. Must NOT import: `neo/internal/agent`, Neo tools, Neo conversation state, Neo supervision.

---

## Spec Files Created

All in `spec/workforce/`:

| File | Type | Purpose |
|---|---|---|
| `spec.kvx` | Hand-authored | Requirements, acceptance criteria, task list with wave/dependency ordering |
| `design.body.md` | Hand-authored | Full architecture document (15 sections) |
| `ENGINEERING_STANDARDS.md` | Hand-authored | Normative companion: language floors, build/quality/test/CI/resource/review standards |
| `requirements.md` | Generated | Projected from spec.kvx |
| `design.md` | Generated | Projected from spec.kvx + design.body.md |
| `tasks.md` | Generated | Projected from spec.kvx |

Spec generator: `cd /root/matrix/spec/specgen && go run . -root /root/matrix`
Staleness check: `go run . -root /root/matrix -check`

---

## User Preferences (from session)

- User corrected shared-Cortex assumption: **"these processes cannot share 1 brain they must have organization but unique memories or chaos is guaranteed"**
- User requires long-horizon focus with auto daily triggers, MCL-style but done properly
- User wants agents **stateless** — memory limited to the wake session; next day = new agent with just goal and tools
- User added Developer exception: dev agents need persistent cross-session codebase graph and memory
- User wants cross-agent messaging protocol (internal email)
- User requested `ENGINEERING_STANDARDS.md` with specific sections on language versions, build standards, code quality, module structure, dependency rules, interfaces, type safety, serialization, goroutines, channels, shutdown, policy, security, circuit breakers, testing laws/coverage/architecture, documentation, latency/memory budgets, backup/recovery, spec loop, quality gates, CI, code review

---

## What Exists vs What Needs Building

### Reusable from Matrix

| Existing | Reused For |
|---|---|
| MCL lifecycle state machine (`executor/lifecycle/state.go`) | Foundation for typed intent transitions |
| MCL IR types (`MCL/ir/intent.go`) | Typed Frame, Constraint, Predicate, Budget, Intent |
| Chronos alarms (`chronos/pkg/types/types.go`) | Durable timer system (needs workforce wake target) |
| Chronos wake (`chronos/internal/wake/wake.go`) | Wake delivery pattern (needs workforce envelope) |
| Cortex, Vault, Scope, Snapshot | Private brain storage, encryption, access control |
| CodeGraph module | Developer department Project Brain |
| Run records, task store | Pattern reference (not directly reused) |

### Must Build New

- `workforced` kernel (scheduler, dependency resolver, policy engine, lease service, effect gateway, approval pipeline)
- `workforce-seat` execution loop (14-stage cycle)
- `workforce-auditor` stateless verdict loop
- Organizational ledger (typed records, provenance graph, correction resolver)
- Workforce Mail Protocol
- Project Brain (CodeGraph + engineering memory for dev seats)
- Skill contract system (typed I/O, capabilities, preconditions, postconditions, probes, verifiers)
- Dashboard (`/workforce` surface)
- All 21 seat identity/mandate/policy configuration

---

## Task List Summary (from spec.kvx)

The spec defines tasks across waves. All tasks are **pending** — no implementation has started. The spec is complete and specgen-validated. Read `spec/workforce/spec.kvx` for the exact task list with wave ordering, dependencies, and acceptance criteria.

---

## Verification Commands

```bash
# Spec generation and check
cd /root/matrix/spec/specgen && GOCACHE=/tmp/matrix-specgen-go-cache go run . -root /root/matrix
cd /root/matrix/spec/specgen && GOCACHE=/tmp/matrix-specgen-go-cache go run . -root /root/matrix -check

# Specgen tests
cd /root/matrix/spec/specgen && GOCACHE=/tmp/matrix-specgen-go-cache go test ./...
```

---

## Do Not

- Import `neo/internal/agent`, Neo tools, Neo conversation state, or Neo supervision into workforce modules
- Give seat processes effectful credentials
- Give auditors departmental memory
- Let agents communicate directly (use Workforce Mail)
- Claim the workforce feature exists until `workforced`, `workforce-seat`, and `workforce-auditor` are implemented and tested
- Touch `spec/daemon-env-isolation` or change `spec/workflow.kvx` active_feature without authorization
