# Control Loop

Package `matrix/neo/internal/agent` implements Neo's recursive LLM tool-calling loop. The conversation transcript IS the state; the model emits text + tool-call intents; the harness is the only effector. This is deliberately NOT the MCL compile→plan→execute machine — MCL is reached only through the `core_execute` tool for rigorous / monetary tasks.

Source files: `neo/internal/agent/agent.go`, `neo/internal/agent/compaction.go`, `neo/internal/agent/prompt.go`, `neo/internal/agent/reporter.go`, `neo/internal/agent/validate.go`.

---

## Design decisions

**The transcript is the state.** There is no hidden state machine, no plan tree, no compiled intent. The model sees the full conversation (minus what was compacted) and decides what to do next. This is the "normal agent" shape — familiar, debuggable, and inspectable.

**System block is re-derived every turn.** The system prompt (identity + rules + retrieved memory + budget stat) is rebuilt fresh on every iteration. It can never drift because it's never stored.

**Two-model architecture.** A main model (conversational, tool-calling) and a cheap model (compaction, validation, write-back). The cheap model falls back to the main model if unavailable.

**Context budget with hard/soft thresholds.** The window is monitored as a percentage of the configured context window. Soft threshold (80%) triggers cooperative compaction at a clean boundary. Hard threshold (92%) forces compaction immediately as a runaway backstop.

**No-progress stall detection.** If the model repeats the same tool-call batch without making progress, the loop stops after `NoProgressStall` repeats and returns an honest partial.

---

## The Chat loop

```
user message
      │
      ▼
┌─────────────────┐
│  faultMemory    │  Page-fault relevant cortex records (once/turn)
│  faultPatterns  │  Retrieve proven procedural patterns
│  recallTurns    │  Surface relevant past conversation turns
└─────────────────┘
      │
      ▼
┌─────────────────┐
│  buildSystem    │  Compose system block: identity + rules + memory + budget stat
└─────────────────┘
      │
      ▼
┌─────────────────┐
│  budget check   │  If >= hard_pct: compact NOW
└─────────────────┘
      │
      ▼
┌─────────────────┐
│  call LLM       │  Send window (system + working transcript + tool schemas)
└─────────────────┘
      │
      ▼
┌─────────────────┐
│  no tool calls? │  → Final answer (termination check + return)
│  yes            │
└─────────────────┘
      │
      ▼
┌─────────────────┐
│  no-progress?   │  → Stop with honest partial
│  runToolCalls   │  Execute each tool call, append results to transcript
└─────────────────┘
      │
      ▼
      (loop back to buildSystem)
```

---

## Chat method

```go
func (a *Agent) Chat(ctx context.Context, userInput string) error
```

Runs one user turn through the recursive loop until:
1. The model yields a final answer (no tool calls)
2. The step budget is exhausted → honest partial
3. The loop stalls (no progress) → honest partial
4. Context cancellation

Conversation state (`working` transcript, `summary`, `activeGoal`) persists across calls.

### Pre-turn setup

```go
a.working = append(a.working, llm.UserMessage(userInput))
if a.activeGoal == "" {
    a.activeGoal = userInput
}
```

The `activeGoal` is pinned every turn and used for memory retrieval routing.

### Mid-turn refault

Every `refaultEvery` (6) steps, the loop re-faults memory against the latest assistant narration to track sub-goal drift in long tool loops. The query is `userInput + lastAssistantText`.

### Termination

When the model returns no tool calls:
- `finish_reason=length` → nudge to retry compactly (never emit truncated text raw)
- Empty answer + no tools → nudge once to continue
- Otherwise → `Reporter.Say(answer)`, then background consolidation, then soft-compaction check

---

## Compaction

When the context window fills, the agent swaps older working history into a consolidated summary.

```go
func (a *Agent) compact(ctx context.Context, reason string)
```

**"hard"** — forced compaction at the hard threshold. The agent announces: "I'm right at my working-memory limit — one moment while I consolidate..."

**"soft"** — cooperative compaction at a clean boundary. The agent announces: "We've covered a lot — let me quickly consolidate where we are..."

The cheap model (or main as fallback) reads the full transcript and fills the active-session schema:

```
GOAL: <the task being pursued>
DECISIONS: <choices made, each with a one-line why>
ARTIFACTS: <files / addresses / tx hashes / IDs produced or referenced, verbatim>
OPEN: <unresolved questions or blockers>
LAST_RESULTS: <still-relevant tool outputs worth carrying forward>
NEXT: <the planned next step(s)>
```

After summarization, the `validateSummary` pass checks that every high-entropy token from the original transcript survived verbatim. Any dropped identifiers are re-appended under a `ARTIFACTS (preserved verbatim):` line — the trust contract (i3).

If summarization fails, the loop degrades to `safeTail` — keeping the transcript from the last user message onward, so no tool-result is left without its preceding assistant call.

---

## System prompt

The system block is composed fresh every turn by `buildSystem`:

1. **Static charter** — `systemPrompt()`: who Neo is, how it works, money rules, media rules, voice rules
2. **Ground truth** — embedded `knowledge.md`: Paxeer is real and live, canonical endpoints, `core_execute` usage
3. **Pinned block** — from `Pager.Pinned()`: identity DID, inviolable rules, hard constraints from cortex, user profile, active goal
4. **Consolidated summary** — the active-session summary from compaction
5. **Recalled turns** — relevant past conversation turns (deduped against live transcript)
6. **Retrieved memory** — page-faulted cortex records (facts, events, patterns, preferences, goals)
7. **Procedural patterns** — proven how-to recipes whose trigger matches the current goal
8. **Budget stat** — `[context: 62% used]`

---

## Tool dispatch

```go
func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall)
```

For each tool call:
1. Parse arguments (JSON → map)
2. `Reporter.Status("• " + name)` — ephemeral progress
3. `dispatchWithRetry` — bounded retries (recovery ladder rung 1)
4. Append tool result to transcript
5. `ToolObserver` callback — surfaces the work to the presentation layer

### Recovery ladder

| Rung | Action | When |
|---|---|---|
| 1 | Retry with backoff | Transient/invocation errors |
| 2 | Adapt approach | Bad args/approach (error as signal) |
| 3 | Escalate to MCL | Money/rigor boundary |
| 4 | Surface honest partial | After ladder exhaustion or stall |

`MaxRetriesPerTool` (default 3) bounds rung 1. `MaxAdaptAttempts` (default 2) bounds rung 2.

---

## Reporter interface

The agent never writes to a terminal directly. It speaks through a `Reporter`:

```go
type Reporter interface {
    Say(text string)      // User-facing answer / narration
    Status(text string)   // Ephemeral progress (tool starting, interim preamble)
    Notice(text string)   // Deliberate visible promise (compaction, escalation)
}
```

Implementations:
- **CLI** (`stdoutReporter`): Say → stdout, Status/Notice → stderr
- **Server** (`sseReporter`): All three map to SSE event types

---

## Budget math

```go
func (a *Agent) budgetPct(system string) int
```

```
used = EstimateTokens(system) + estimateMessagesTokens(working) + schemaTokens
pct  = used * 100 / ContextWindowTokens
```

`schemaTokens` is the JSON-serialized tool schema size — a fixed overhead paid every turn. `estimateMessagesTokens` counts content + tool calls + 4 tokens per message overhead.

---

## Seeding a resumed conversation

```go
func (a *Agent) Seed(history []llm.Message, goal string)
```

Primes a fresh agent with durable history from the conversation store. No-op once the live transcript has content (never clobbers an in-flight conversation). The history is `DefaultRecallTurns` (16) recent turns, oldest-first.

---

## Modifying the loop

To change loop behavior, edit the relevant source:

| What to change | Where |
|---|---|
| System prompt text | `agent/prompt.go` — `systemPrompt()` |
| Inviolable rules | `agent/prompt.go` — `invariantRules` |
| Ground truth facts | `agent/knowledge.md` (embedded, ships in binary) |
| Compaction schema | `agent/compaction.go` — `compactionSystemPrompt` |
| Context thresholds | `config/config.go` — `SoftPct`, `HardPct` |
| Step budget | `config/config.go` — `StepBudget` |
| No-progress stall count | `config/config.go` — `NoProgressStall` |
| Recovery ladder bounds | `config/config.go` — `MaxRetriesPerTool`, `MaxAdaptAttempts` |
