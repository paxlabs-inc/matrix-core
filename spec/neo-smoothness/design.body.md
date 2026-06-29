# Design — Neo Smoothness (Cascade-derived conversational UX)

## Overview

This feature makes Neo's conversational experience feel **smoother** by adopting
the proven turn-mechanics of the Cascade/Devin coding agent. It is grounded in a
study of **54 real Cascade session dumps** at `/root/dataset/sessions/` (the
decision is recorded in cortex). The goal is not to rebuild what Neo already does
well — it is to close the specific, named gaps where Cascade's loop reads as
more polished than Neo's.

The guiding result of the study: **Neo already equals or exceeds Cascade on the
heavy machinery, and only trails on a handful of surface mechanics.** This spec
is scoped to exactly those mechanics, and explicitly hands the overlapping items
to the already-open `neo-execution-reliability` feature instead of duplicating
them.

## The Cascade session architecture (what was studied)

Each session dump is `trajectories[] → messages[]` with roles
`system | user | assistant | tool`. Across 284 trajectories the study found
**three cooperating agent roles**, each with its own system prompt:

- **Primary agent** (`You are Devin…`, an ~18 KB prompt; also Claude Opus /
  `swe-1-6-fast` / `adaptive`). The conversational + execution loop. Its system
  context is delivered as **separate, layered messages** — persona → subagent
  profiles → model-id → `<system_info>` (platform/date/cwd) → `<rules>` →
  `<available_skills>` — with the **stable persona first** (prompt-cache
  friendly).
- **Summarizer** (a dedicated `summarizer` model with **no tools**). Fed the
  flattened execution trace, it emits a fixed-schema `<summary>`
  (`Overview / Key Details & Breadcrumbs / Current State`) for resumption. It
  ran in **218 of 284 trajectories** — trace compaction fires constantly. It is
  explicitly told **not** to recite rules verbatim.
- **Subagents** via `run_subagent`: `subagent_explore` (read-only: grep/glob/
  read) vs `subagent_general` (full read/write; background auto-denies
  unapproved tools).

**Turn mechanics** observed in a real primary trajectory:

- Every step is **`assistant narrates one terse intent sentence → tool call →
  tool-role result → assistant continues`**.
- Truncated tool output **spills to an overflow file** plus a
  `<truncation_notice>` carrying the path; the agent reads the file when it
  needs the rest.
- System steering arrives as **`<system_guidance>`**: *"hints/reminders injected
  by the system before you act … pay attention but do not acknowledge or respond
  to them directly — simply incorporate their guidance."*
- A **`todo_write`** tool is used *"VERY frequently … one `in_progress` at a
  time … mark completed as soon as done, do not batch."*
- Two **modes**: `Normal` (full autonomy) and `Plan` (explore + ask + plan, no
  changes until approved).

## Gap analysis vs Neo (what NOT to reinvent)

Neo is **at or ahead** of Cascade here — these are deliberately out of scope:

- **Context compaction** — `neo/internal/agent/compaction.go` already folds older
  turns into a fixed schema (`GOAL / DECISIONS / ARTIFACTS / OPEN /
  LAST_RESULTS / NEXT`) **and runs a verbatim high-entropy-token validator** that
  Cascade's summarizer does not have. Keep as-is.
- **Subagent swarm** — `spawn_subagents` + a restricted `SubagentSchemas()`
  already mirror the explore/general split
  (`neo/internal/tools/tools.go`, `neo/internal/server/swarm.go`).
- **Failure classification, attach-on-409, semantic stall** — landed in
  `neo-execution-reliability` P1 (`neo/internal/delegate/classify.go`,
  `neo/internal/agent/semantic.go`).
- **Durable trace + prompt-cache-stable system prefix** —
  `neo/internal/trace`, `assembleWindow`.

## Scope — the net-new Cascade patterns

### 1. Invisible guidance channel (highest impact)

**Problem.** Neo's completion gate (Cassandra Phase 1) returns rejection feedback
that is appended **in-band** as a tool result:

```
// neo/internal/agent/agent.go (~line 687)
a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, verdict.feedback))
```

The model then reasons over that text and streams it as **reasoning** to the
user — the root of both the *rigidity* feel (`NE-6`) and the *jargon leak* into
the Thinking panel (`NE-12`).

**Fix.** Adopt Cascade's `<system_guidance>` contract. Add a guidance message
kind that the model is instructed to **act on but never acknowledge**. Route
completion-gate feedback (and retry/stall nudges) through it, and **scrub it from
every user-facing surface** (`chat.delta channel=reasoning` / `chat.thinking` and
the assistant transcript). Cap consecutive nudges and **escalate to stop-and-ask**
rather than re-nudging unbounded. The agent still learns it must continue/finish —
in clean terms — but the user only sees the result.

### 2. Narrate-before-act

One concise, **action-specific** intent sentence before each tool/`core_execute`
call, drawn from the real operation — not a fixed boilerplate string reused every
step (the opposite of the `NE-2` "routing this through the secure path" wall).
Tone: direct, no preamble/validation phrases, no emojis, no jargon. This also
*feeds* the coalescing fix in `neo-execution-reliability` (its `req.7`): distinct
per-action content means coalescing collapses only true duplicates.

### 3. Live task-list (todo) surface

A first-class `todo` tool (alongside `core_execute` / `spawn_subagents`) that
records an ordered plan with per-item status, **one `in_progress` at a time**,
**done immediately** on completion, rendered as a live checklist in the client.
No list on a trivial single-step turn (no ceremony). State persists through the
**durable trace** so the checklist survives reopen and respawn (task-durability).
UI honors house rules: separation by background-tone contrast, **never** borders;
no emojis/glow.

### 4. Overflow-file for truncated output

At the tool/delegate result boundary, when output exceeds the inline budget,
write the **full** output to a run-scoped file and return an inline notice with
the path (mirroring `<truncation_notice>`), instead of silently cutting it. The
agent **must** read the overflow before reasoning over a flagged-incomplete
result — the read-full discipline, enforced in code. This directly prevents the
"confidently states a value pulled from a half-truncated result" failure class.

### 5. Plan-vs-Act mode (optional)

An `act` (default) | `plan` mode signal. In `plan` mode Neo explores, asks
focused questions, and proposes a plan, withholding value-moving/irreversible
actions until approval. In `act` mode it may still propose a plan first for
large/ambiguous requests. Mode never weakens key-isolation or the embedded-wallet
leash/approval governance.

## Cross-feature boundary (no duplication)

These belong to **`neo-execution-reliability`** and are **out of scope here** —
this feature only complements them:

- **Structured clarify pass-through** — its `req.8` / `task.4.4`.
- **Narration coalescing** (server + client) — its `req.7` / `task.4.3`.
- **Jargon scrub** of tool results + the reasoning channel — its `req.6` /
  `task.4.1`–`4.2`.

Note the natural pairing: this feature's narrate-before-act (`req.2`) produces the
distinct content that its coalescing consumes; this feature's guidance scrub
(`req.1`) and its jargon scrub operate on the same reasoning surface and should
land coherently.

## Preserved invariants

- **Signed MCL walk untouched** — `compile → plan → walk → synthesize → critic →
  emitFinalTurn` and all journaled bytes are unchanged; the **D11 replay
  byte-identity invariant** holds.
- **Key-isolation backstop** — Neo holds no signing key; all value-moving work
  crosses into MCL via `core_execute`. No new signing path.
- Changes are confined to **non-signed transcript events, loop control flow**
  (guidance / narration / todo / mode), **tool-result framing** (overflow), and
  the **client surface**.

## Verification (no fakes)

Each property is proven against **real** code paths — the real agent loop, the
real emitter, the real task-list + trace, the real delegate boundary — never a
stub standing in for the logic under test:

- **P1** — completion-gate feedback rides the guidance channel and is **absent**
  from the user-visible reasoning/transcript; nudges escalate to stop-and-ask
  after the bounded count.
- **P2** — todo lifecycle: one `in_progress`, immediate done, no list on a
  trivial turn, reconstructed from the durable trace after a simulated respawn.
- **P3** — an oversized tool result is persisted in full, the inline notice
  carries the path, and the agent reads the remainder before answering.
- **P4** — the signed MCL journaled bytes + D11 replay roots are byte-identical
  before/after, and no new signing path is introduced.
