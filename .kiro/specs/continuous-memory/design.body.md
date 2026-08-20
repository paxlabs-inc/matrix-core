# Design — Continuous Memory (cortex as the self-managing memory brain)

## Overview

**Continuous Memory** makes **cortex** the *self-managing memory brain* of a Centra AI agent.
Today the brain is split: cortex is a durable, tamper-evident store, but the agent (Neo) runs its
own parallel controller on top of it — `neo/internal/memory/pager.go` does selection, ranking and
recency re-ranking; `a.summary` / `a.compact` (`agents/neo/internal/agent/agent.go`) keep an ephemeral
"story so far" per process; the transcript lives only in the agent's in-memory `working` slice.
cortex's own `Context()` composer and `Compact()` primitive have **no Neo consumer**. That is the
split brain we are closing.

The locked architecture: **cortex owns the entire memory lifecycle** — durable storage, the live
conversation transcript, working-set/activation selection, ranking, multi-resolution temporal
compaction, and recursive recall. The agent is reduced to **four verbs**:

```
Append(conv, message)  →  Activate(conv, query, budget) → bundle  →  render  →  transport
```

Like a human brain, memory manages *itself* rather than an external agent-side controller
delegating to a passive store. The agent no longer decides *what to remember*, *what to surface
now*, or *at what resolution*; it appends what happens and renders what cortex hands back.

The model is given a **perceived continuous linear memory** through selective orchestration across
four tiers, realised as a **multi-resolution temporal index over the cortex journal** exposed as a
**recursive retrieval environment** — the RLM (Recursive Language Models, Zhang/Kraska/Khattab,
arXiv 2512.24601) insight applied to *time* rather than a single long prompt. Exact events are
**paged in on demand, never resident**.

## The tier model

| Tier | Meaning | Residency | Backing |
|------|---------|-----------|---------|
| **T0** | High-level timeline — coarse narrative of the general past | Summary resident | Temporal rollup records (week/epoch) |
| **T1** | Mid-range recent events — last N episodes, higher fidelity | Summary resident | Temporal rollup records (hour/day) + recency reader |
| **T2** | Active context — the in-window working set + recent transcript slice | Fully resident | Pinned + session/transcript store |
| **T3** | Exact specifics — retrieved on demand as an invocation | Paged in on demand | Recursive recall over the journal / member Refs |

## Locked decisions (carried into this design)

- **cortex is the whole brain, transcript included** (SCOPE, maximal). The raw turn-by-turn
  messages (user / assistant / tool_call / tool_result / system) become durable cortex-managed
  **session state**, stored and compacted by cortex as the conversation happens. The agent holds
  only the current **activation** cortex hands it each turn.
- **No separate agent-side pager/context-assembler.** cortex owns
  SELECTION / RANKING / COMPACTION / TIERING / RECALL and returns a structured, ranked, budgeted
  working-context bundle + durable session state. The agent owns only **rendering + transport**
  (bundle → prompt string, tool plumbing, provider byte caps).
- **Scope = full vertical.** One spec covers: (1) the cortex-core brain, (2) collapsing Neo's
  memory controller into it, and (3) client-visible tie-ins.
- **Derived / journaled-but-NOT-anchored lane.** *Everything* new and mutable (session/transcript
  records, temporal-ladder rollups, activation caches, story-so-far checkpoints) lives in the same
  posture as `cortex.Compact` (`compact.go:429-431`): it **appends to the journal MMR** for
  durability + replay-rebuild, writes derived `chk/`-family records, and performs **NO `memories`
  / `edges` SMT write**. cortex can therefore actively manage itself **without perturbing
  `OverallRoot` / the `cortex_snapshot_hash` root / D11 replay determinism**. `cortex.Context`
  stays a pure read composer for the anchored world-state.

### Resolved open questions

- **Q1 — Rollup storage shape.** Derived `chk/`-style records in the Compact lane, **not** a 10th
  memory type. A 10th type is a snapshot-hash / schema change (heavy, cross-language consumers pin
  the type ints) and would pull rollups into the anchored SMT world, violating the derived-lane
  rule. Rollups are a new `roll/`-family key keyed by **time window** instead of intent/step,
  written with the exact `Compact` posture (journal entry + derived record, no SMT).
- **Q2 — Window keying.** Fixed wall-clock buckets — **hour → day → week/epoch** — with an
  **event-count floor** so empty windows spawn no rollup. Deterministic, idempotent, and trivially
  rebuildable from journal `CreatedAt`/`Seq`, which the replay-safety requirement demands. Adaptive
  salience-density keying is deferred (non-deterministic to rebuild).
- **Q3 — Summarizer.** **Two-lane.** The ladder's replayable floor is a **deterministic extractive
  template** (top-salience Refs + counts + verb/outcome tallies). An **optional LLM enrichment**
  layer may write a richer prose summary, but only as a **separate derived record that is
  rebuildable from the deterministic one** — never load-bearing for determinism.
- **Q4 — Cascade trigger.** **Eager on window-close** (a Chronos-scheduled sweep rolls a window up
  when it closes; bounded work) **plus lazy-repairable** — a read that finds a missing or stale
  coarser tier triggers an idempotent rebuild. Self-healing without an unbounded hot path.
- **Q5 — Recursive-recall budget.** Depth cap **4** (mirrors T0→T3) + a **per-descent token
  budget**; the hard ceiling reuses the existing `MaxHopsCap = 6` discipline.
- **Q7 — Neutral message schema.** A provider-agnostic cortex `Message` record:
  `{role: user|assistant|tool_call|tool_result|system, content, tool_name?, tool_args?,
  tool_result_ref?, media_refs[], ts, seq}`, decoupled from Fireworks / OpenAI / Baseten wire
  shapes so cortex can store + compact the transcript without wire coupling; the agent renders it
  into the provider shape at transport time.
- **Q8 — `Activate` latency.** Hold the existing `Context` **p50 < 80 ms** discipline for the whole
  `Activate` call *including* the transcript slice, achieved by serving **T2 / T1 / T0 from
  materialized derived records** (no live recompute) plus a **per-turn pinned cache** (which also
  fixes audit finding **NE-7**, the per-step pinned recompute). T3 recursive descent stays **off
  the hot path** (tool-invoked, on demand).

## Seam map (grounded in current code)

| Concern | Location | State |
|---------|----------|-------|
| Linear event log (time + seq axis) | `core/cortex/journal/journal.go:471-482` (`Entry{Seq,Kind,CreatedAt,Payload}`, per-actor monotonic gap-free, MMR-anchored) | Exists; low-level write log, not episodic narrative |
| Rollup primitive (summarize + link) | `core/cortex/compact.go:241` `Compact()`; item shape `:87-91`; load `:534` | Exists; intent/step-keyed, agent-invoked, no cascade |
| Derived-not-anchored lane | `core/cortex/compact.go:429-431` (journal + `chk/` record, **no SMT write**) | The posture the whole feature rides |
| Tiered assembler + invocation handle | `core/cortex/context.go:259` `Context()`; `ReachableURIs` `:213` (cap 64); `tierOutcomes` `:612` | Exists; tiers are Pinned/Frame/Outcomes, not coarse/mid/recent temporal |
| Invocation / recall tool | `agents/neo/internal/tools/tools.go:45`, `RecallFunc:132`, `as_of:843` | Exists; **flat** lookup, bi-temporal works, not recursive |
| Pager recall backing | `neo/internal/memory/pager.go:707` (`RecallHits`), `:781` (`Recall`) | Exists; to be absorbed into cortex |
| Neo turn assembly | `agents/neo/internal/agent/agent.go:430` (`Chat`), pinned `:556`, ambient knob `:537`, recall `:1079`, transcript compaction `:562` (`a.summary` + `a.compact`) | `cortex.Compact`/`Context` **not consumed** — the integration gap |
| Render tail (tier inject point) | `agents/neo/internal/agent/prompt.go:70` (`dynamicTail`) | Attach point for the T0/T1 sections of the activation bundle |
| Recency ranking substrate | `core/cortex/salience/salience.go:209-216` (`R=exp(-Δt/90d)`), Neo per-type multiplier `pager.go:519-526` | Substrate for T1 |
| Event volume type / no Data timestamp | `core/cortex/memory/data.go:148-159` (`EventData`; `created_at` lives on the Version, not in Data) | Storage-shape input |

## What exists vs. the gap

- **T2 — done.** Pinned + `Context`/Retrieve already compose the resident working set.
- **T3 — primitive done.** `ReachableURIs` + resolve, `memory_recall` + `as_of` exist; needs the
  **RLM recursive-descent upgrade**.
- **T1 — partial.** `tierOutcomes` is Event-only/verb-keyed; the recency math exists; needs a
  general "last N episodes across types" reader.
- **T0 — missing.** No time-windowed rollup builder, no cascade, no consumer.

## New cortex surfaces (proposed)

All signatures are proposals to be finalised in implementation; they are shown to fix the shape
and prove the seams line up. All writes ride the derived lane (journal + `chk/`-family, no SMT).

### 1. Session / transcript store (`cortex/session`)

```go
type Role uint8 // RoleUser, RoleAssistant, RoleToolCall, RoleToolResult, RoleSystem

type Message struct {
    ConversationID string
    Seq            uint64   // per-conversation monotonic, gap-free
    Role           Role
    Content        string
    ToolName       string   // tool_call / tool_result only
    ToolArgs       []byte   // canonical JSON, tool_call only
    ToolResultRef  string   // large tool results spilled to a resolvable ref (overflow discipline)
    MediaRefs      []string
    TS             int64    // UnixNano
}

func (c *Cortex) AppendMessage(ctx, m Message) (uri string, err error)   // derived: journal KindSession + sess/<conv>/<seq>
func (c *Cortex) Transcript(ctx, conv string, sinceSeq uint64, limit int) ([]Message, error)
```

- A large `ToolResult` spills to a resolvable ref (the same overflow discipline as
  `agents/neo/internal/agent/overflow.go`) so the transcript store never bloats with megabyte payloads —
  it stays a page-in target (T3), not resident.
- `AppendMessage` appends a derived-kind journal entry (durability + rebuild) and a `sess/`
  record; **no SMT write** — identical posture to `Compact`.

### 2. Temporal ladder (`cortex/ladder`, sharing the Compact machinery)

```go
type Window struct { Tier Tier; Start, End int64 } // TierHour < TierDay < TierEpoch

type Rollup struct {
    Window     Window
    Members    []Ref     // resolvable pointers to constituent journal entries / finer rollups
    ShortForm  string    // deterministic extractive summary (<= budget)
    Salience   float64
    EnrichRef  string    // optional LLM-enriched record, rebuildable from ShortForm; may be ""
}

func (c *Cortex) BuildRollup(ctx, w Window) (uri string, err error) // deterministic, idempotent
func (c *Cortex) Cascade(ctx, upTo Tier, now int64) error           // eager window-close sweep
func (c *Cortex) Rollups(ctx, tier Tier, since, until int64) ([]Rollup, error)
```

- **Deterministic floor:** `ShortForm` is built purely from journal facts in the window
  (top-salience Refs by `salience.go` recency, verb/outcome tallies, counts). Same inputs → same
  bytes → rebuildable.
- **Optional enrichment:** `EnrichRef` points to a separate derived record produced by an LLM; if
  absent or stale, the deterministic `ShortForm` stands alone. Enrichment never feeds determinism.
- **Cascade:** hour rollups roll into day, day into week/epoch. An **event-count floor** skips
  empty windows. `BuildRollup` is idempotent — re-running over the same window yields the same
  record, enabling lazy read-repair.

### 3. Activation composer (extends `cortex.Context` → `cortex.Activate`)

```go
type ActivationBundle struct {
    Pinned        []Item    // identity + hard constraints + active goals (per-turn cached)
    Timeline      []Rollup  // T0 coarse
    Recent        []Rollup  // T1 mid-range
    Transcript    []Message // T2 recent slice for this conversation
    StorySoFar    string    // durable rolling summary of THIS conversation (replaces a.summary)
    ReachableURIs []string  // T3 handles — resolved lazily on descent
}

func (c *Cortex) Activate(ctx, conv, query string, budget Budget) (*ActivationBundle, error)
```

- Supersedes both `cortex.Context` (extended, not deleted — it remains the pure anchored-world
  read composer) **and** Neo's pager + `a.summary`.
- Serves `Timeline`/`Recent` from **materialized rollups** (no recompute) and `Transcript` from
  the session store; `Pinned` from a per-turn cache. Global salience-asc trim under `budget`, same
  discipline as `Context`.
- **Latency:** whole call held to `Context`'s p50 < 80 ms / < 250 ms ceiling.

### 4. Recursive recall (upgrade of `RecallFunc`)

`RecallFunc` becomes a **decomposable descent** instead of a flat lookup:

```
list T0 windows  →  pick a window  →  resolve its member Refs  →  sub-query within
```

- Depth cap **4** (T0→T3); per-descent token budget; hard ceiling reuses `MaxHopsCap = 6`.
- Bi-temporal `as_of` is preserved at every level (timeline time-travel — "what was true then").
- Backed by the session store + `Rollups` + journal member resolution.

## System map

```
                       ┌──────────────────────── every turn ────────────────────────┐
   user / assistant / tool message ──▶ cortex.AppendMessage  (derived: journal + sess/<conv>/<seq>)
                                                                     │
   Neo turn start ──▶ cortex.Activate(conv, query, budget) ──────────┘
                          │  Pinned (cached)  T0 Timeline  T1 Recent  T2 Transcript  StorySoFar  T3 handles
                          ▼
                   render bundle → prompt (agent owns ONLY render + transport)
                          │
                          ▼ (model may descend)
                   memory_recall (recursive) ──▶ list T0 → pick window → resolve Refs → sub-query (depth≤4)

   ── background, D11-safe derived lane ──────────────────────────────────────────────
   Chronos window-close sweep ──▶ cortex.Cascade ──▶ BuildRollup(hour) → day → week/epoch
                                                        (deterministic floor + optional enrich)
   read-time repair ──▶ missing/stale coarser tier ──▶ idempotent BuildRollup
```

## Neo collapse (the vertical)

- `neo/internal/memory/pager.go` selection / ranking / recency re-rank → **thin client** of
  `cortex.Activate`. The pager shrinks to a translation shim (or is retired) — no independent
  brain logic remains agent-side.
- `a.summary` "story so far" + `a.compact(ctx,"hard")` (`agent.go:562`) → **retired**. Story-so-far
  is now durable in cortex, produced by the ladder over the conversation's own transcript.
- The `dynamicTail` memory sections (`prompt.go:70`) → replaced by the rendered `ActivationBundle`
  (T0/T1/T2/story sections), respecting the prompt-window portability fix (the trailing dynamic
  tail is a **user-role** message, not system — Qwen-template safety).
- Neo turn loop (`agent.go:430`): each user/assistant/tool message → `cortex.AppendMessage`; once
  per turn → `cortex.Activate` → render. Pinned computed **once per turn** (fixes **NE-7**).
- `AmbientRetrievalTopK` philosophy (**pull > push**) is preserved: `Activate` returns the hot set
  + T3 handles; the model still pulls exact specifics via recursive `memory_recall`.

## Migration & retro-compatibility

- `cortex.Context` is **kept** (pure anchored-world read composer); `Activate` is additive and
  builds on it. Existing `Context` consumers (cold-start, CLI) are unaffected.
- Existing conversations without a session store degrade gracefully: `Activate` with an empty
  transcript falls back to the `Context` tiers, so the CLI / any pre-migration path keeps working.
- The collapse of pager / `a.summary` / `dynamicTail` is mechanical once the cortex surface exists;
  done behind the `Activate` seam so the turn loop changes in one place.

## Config knobs (proposed)

- `[continuous_memory] enabled` (feature flag while the collapse lands), `hour_window_minutes`,
  `day_rollup`, `epoch_rollup`, `event_count_floor`, `t1_recent_episodes`,
  `recall_max_depth` (4), `recall_descent_budget_tokens`, `activate_budget_tokens`,
  `enrich_enabled` (LLM enrichment on/off), `enrich_model`.
- Cascade schedule is a Chronos alarm interval (window-close cadence).

## Non-goals

- **No change to the signed MCL walk** (compile / plan / walk / synthesize / critic /
  emitFinalTurn, D11 byte-identity) and **no new signing / cortex-write capability for the
  Liaison** — the Liaison remains a pure observability side-channel.
- **No 10th memory type** and **no `memories`/`edges` SMT change** — the anchored world-state and
  its `snapshot_hash` root are untouched.
- **No adaptive salience-density window keying** (deferred; fixed wall-clock buckets only).
- The **anchored** `cortex.Context` semantics are not altered; only extended by `Activate`.

## Verification strategy (no fakes)

- **Replay-safety (load-bearing):** a real journal driven twice — with the continuous-memory lane
  active vs inactive — yields a **byte-identical `OverallRoot` / `cortex_snapshot_hash`**. This is
  the D11 proof that the brain can be active without perturbing the anchored world.
- **Storage/round-trip:** real `AppendMessage` → real `Transcript` slice; real journal window →
  real `BuildRollup` → deterministic `ShortForm` is reproducible byte-for-byte on rebuild.
- **Cascade:** real hour→day→epoch cascade over a seeded real journal; idempotent re-run is a
  no-op; an empty window under the event-count floor produces no rollup; lazy read-repair rebuilds
  a deleted coarser record identically.
- **Recursive recall:** a real decomposable descent (list T0 → pick → resolve Refs → sub-query)
  reaches an exact event that is *not* resident in the activation bundle; depth cap and per-descent
  budget are enforced; `as_of` time-travel returns the then-true view.
- **Activation:** real `Activate` returns Pinned + T0 + T1 + transcript slice + story-so-far under
  budget; p50 latency held; pinned computed once per turn.
- **Neo collapse:** the real Neo turn loop consumes `Activate` (not the pager), appends messages to
  cortex, and renders the bundle; `a.summary`/`a.compact` are gone and story-so-far survives a
  simulated process restart (durability).
- **No test** substitutes a stub/mock/fake for a real code path or type to manufacture a pass.
