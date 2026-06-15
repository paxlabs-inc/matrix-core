# Write-back Consolidation

Package `matrix/neo/internal/writeback` is Neo's automatic background consolidation pass. After each turn, a cheap model sweeps the transcript and promotes durable learnings into cortex — objective facts (semantic), task outcomes (episodic), and reusable how-to patterns (procedural). The main agent never has to consciously call `remember()`.

Source file: `neo/internal/writeback/consolidator.go`.

---

## Design decisions

**Option B: automatic background consolidation.** The frozen spec considered two options: (A) agent-must-remember and (B) automatic background pass. Neo chose B — the main agent never has to consciously call `remember()`. This keeps the durable store current so compaction only has to capture the ephemeral story-so-far.

**Cheap model.** The consolidation pass uses the cheap model (or main as fallback). This is a background goroutine that never blocks the agent loop.

**Best-effort, bounded queue.** Jobs are enqueued on a channel of depth 8. If the queue is full, the job is dropped — cortex stays eventually-current; the live transcript is ground truth for the turn anyway.

**Very selective.** Most interactions yield nothing, and that is the correct, common answer. The prompt explicitly instructs the model to return empty arrays when nothing is durable.

---

## Consolidator

```go
type Consolidator struct {
    cfg   config.Config
    model *llm.Client
    pager *memory.Pager
    jobs  chan string
    done  chan struct{}
}
```

```go
wc := writeback.New(cm, pager, cfg)
wc.Start()
defer wc.Stop()

// In the agent loop, after a turn completes:
if a.consolidator != nil {
    a.consolidator.Consolidate(renderTranscript(a.working))
}
```

---

## Consolidation prompt

The cheap model reads the transcript and extracts ONLY durable learnings:

```
Return STRICT JSON:
{
  "facts": ["..."],
  "user_facts": ["..."],
  "patterns": [
    {
      "name": "...",
      "trigger": "...",
      "preconditions": ["..."],
      "steps": ["..."],
      "gotchas": ["..."],
      "success_criteria": ["..."]
    }
  ],
  "outcome": {"summary": "...", "status": "success|failure|partial"}
}
```

Rules:
- `facts`: objective, durable truths about repo/environment/domain (NOT transient chit-chat)
- `user_facts`: durable truths about the USER (name, role, preferences) — pinned to every future conversation
- `patterns`: reusable how-to recipes (name, trigger, preconditions, steps, gotchas, success_criteria)
- `outcome`: include ONLY if a concrete task was completed or failed; otherwise null
- Copy identifiers verbatim
- If nothing is durable, return `{"facts": [], "patterns": [], "outcome": null}`

---

## Processing

For each extracted category:

### Facts (up to 5)

```go
_, _ = pager.RememberFact(ctx, statement)
```

Stored as `FactData` with subject `matrix://knowledge/neo`.

### User facts (up to 5)

```go
_, _ = pager.RememberUserFact(ctx, statement)
```

Stored as `FactData` with subject `matrix://knowledge/user`. Deduped by normalized statement before writing. These are pinned to every future conversation via `UserProfile`.

### Patterns (up to 3)

```go
_, _ = pager.ReinforcePattern(ctx, spec, nil)
```

If a pattern with the same dedup identity (name → trigger → steps) already exists, it is reinforced (coverage++, strength nudged up). Otherwise a fresh low-confidence candidate is written.

### Outcome (1)

```go
_, _ = pager.RecordOutcome(ctx, summary, mapOutcome(status), "")
```

Stored as `EventData` with `EventObservation` kind.

---

## Loose JSON parsing

The model may wrap JSON in prose or code fences. `parseLooseJSON` extracts the outermost `{...}` object before unmarshaling:

```go
parseLooseJSON("```json\n{...}\n```", &out)
parseLooseJSON("Sure! Here is the result:\n{...}\nHope that helps.", &out)
```

---

## Modifying write-back

| What to change | Where |
|---|---|
| Consolidation prompt | `writeback/consolidator.go` — `consolidatePrompt` |
| Extraction limits | `writeback/consolidator.go` — `process()` loop bounds |
| Queue depth | `writeback/consolidator.go` — `jobs` channel buffer |
| Timeout | `writeback/consolidator.go` — `process()` context timeout |
| JSON parsing | `writeback/consolidator.go` — `parseLooseJSON()` |
