# MCL Compiler Pipeline

The MCL compiler turns `intent.draft` (natural language + optional slot pre-fills) into `intent.compiled` (a typed, signed, deterministically-hashed Intent IR). It does this in 6 discrete stages, declared in `MCL/core/pipeline.mtx`.

Understanding the pipeline is key to understanding why certain things behave the way they do, and what you're actually changing when you modify a skill or core module.

---

## The 6 stages

```
intent.draft.prose
      │
      ▼
┌─────────────┐
│  Stage 1    │  Normalise — pure function, no LLM
│  normalise  │
└─────────────┘
      │ normalised_prose
      ▼
┌─────────────┐
│  Stage 2    │  Classify verb — grammar-constrained LLM (~10 tokens)
│ classify_   │
│   verb      │
└─────────────┘
      │ candidate_verb
      ▼
┌─────────────┐
│  Stage 3    │  Cortex pre-fetch — no LLM, reads cortex snapshot
│ cortex_pre- │
│   fetch     │
└─────────────┘
      │ cortex_bundle
      ▼
┌─────────────┐
│  Stage 4    │  Frame extraction — main LLM call, D11 seeded
│ extract_    │  skill's §PROCEDURE drives this stage
│   frame     │
└─────────────┘
      │ raw_frame_json
      ▼
┌─────────────┐
│  Stage 5    │  Entity resolution — pure, no LLM
│ resolve_    │
│  entities   │
└─────────────┘
      │ resolved_frame + unknowns
      ▼
┌─────────────┐
│  Stage 6    │  Score + sign — pure, emits intent.compiled or intent.clarify
│ score_and_  │
│    sign     │
└─────────────┘
      │
      ▼
intent.compiled  or  intent.clarify
```

---

## Stage 1 — Normalise

**Type:** `pure` (no LLM, no cortex)

Takes `intent.draft.prose` and produces `normalised_prose`. In practice:
- NFC Unicode normalisation
- Whitespace collapse
- Length cap (8192 chars by default; truncates with a warning on overflow)

This stage exists to ensure the rest of the pipeline operates on a canonical form. If two users type the same intent with different amounts of whitespace, the pipeline sees them as identical — which matters for D11 reproducibility.

---

## Stage 2 — Classify verb

**Type:** `llm_constrained`  
**Module:** `core/verb.mtx`  
**Grammar:** `verb_vocab@1`

Uses the classifier prompt from `verb.mtx` with a grammar constraint that forces output to exactly one of the 10 D7 verbs. Output is typically a single token.

```mtx
# core/verb.mtx
classifier.prompt
  system="You are a verb classifier for the Matrix Communication Layer..."
  user="User goal: {prose}\n\nVerb:"
end
classifier.threshold=0.80
classifier.top_k=3
```

The grammar constraint (`verb_vocab@1`) is an EBNF or JSON schema that the provider's grammar-constrained decoding enforces. The LLM physically cannot produce a token outside the vocabulary — this is not post-processing.

If the top result is below `threshold` (0.80), the verb is marked as `Unknown severity=preferred`. The pipeline continues with a best-effort guess.

This stage is seeded (D11) but because the output space is so small (10 tokens), seeding is more about audit trail than determinism.

---

## Stage 3 — Cortex pre-fetch

**Type:** `cortex` (pure over cortex snapshot)  
**Call:** `cortex.context(verb, prose)`

Fetches a context bundle from the actor's cortex snapshot before the main LLM call. The bundle is formatted text of relevant memories (facts, goals, preferences, patterns) weighted by verb routing.

Each verb has a routing table in `core/verb.mtx`:
```mtx
route.build=Goal Preference Constraint Fact Pattern
route.find=Fact Knowledge Preference Event
```

This tells the cortex fetcher which memory types to prioritize for each verb.

The output (`cortex_bundle`) is a string capped to `budget_tokens=3000`. It becomes available as `{cortex.bundle}` in skill prompt interpolation.

This stage is pure over the cortex snapshot hash. Same snapshot + same verb + same prose = same bundle. That determinism feeds D11.

---

## Stage 4 — Frame extraction

**Type:** `llm_constrained`  
**Grammar:** `intent_frame@1`  
**Seed:** `per_intent` (D11 seed)

This is the main LLM call. The skill's `§PROCEDURE` section drives this stage entirely — the pipeline just invokes the interpreter against the matched `on`-block.

The interpreter:
1. Initializes slots from `§INPUTS`
2. Walks `on`-blocks top-to-bottom, first-match-wins
3. Executes the matched block's entries (prompt, resolve, unknown, clarify)

The prompt interpolates `{prose}`, `{cortex.bundle}`, `{verb}`, and any slot references. The LLM output is grammar-constrained to the `intent_frame@1` schema — it physically emits valid JSON matching the `Frame` structure, not free-text.

The output is `raw_frame_json` — a JSON string with the Frame fields filled in. It's "raw" because entity references inside it are still NL text, not resolved `matrix://` URIs. That happens in stage 5.

**D11 seed computation:**
```
seed = sha256(intent.id || actor || cortex_snapshot_hash || mtx_digest || model_digest)
```

`mtx_digest` is the canonical AST hash of all loaded `.mtx` files (skill + core). Changing any `.mtx` file changes the seed, which changes LLM outputs even with identical input. This makes the compilation trace verifiable — you can reconstruct which `.mtx` versions were active at compile time.

---

## Stage 5 — Entity resolution (D13)

**Type:** `cortex` (pure)

Walks `raw_frame_json` and resolves every NL entity reference to a `matrix://` URI via `cortex.resolve()` or `cortex.find()`. The resolution instructions come from `resolve` statements in the skill's `§PROCEDURE`.

D13 rule: no unresolved NL references may reach the user sign-off. Every referent in the signed Intent must point to a versioned, content-addressable URI.

What happens to ambiguous or missing entities:
- **Ambiguous** → `Unknown(severity=preferred)` + a disambiguation question
- **Missing** → `Unknown(severity=blocking)` + a blocking question

These unknowns flow to stage 6.

---

## Stage 6 — Score and sign

**Type:** `pure`

Aggregates per-slot confidence scores into an overall `confidence` value. The scoring formula is in `core/confidence.mtx`.

Then one of three outcomes:

1. **`confidence >= auto_accept_threshold` (0.90) AND no blocking unknowns** → sign and emit `intent.compiled` immediately, skip the clarify round
2. **`confidence >= confidence_threshold` (0.75) AND no blocking unknowns** → sign and emit `intent.compiled` (user review, but no blocking questions)
3. **Below threshold OR blocking unknowns present** → emit `intent.clarify` with the structured questions

The compiler (agent key) signs first. The user signs later at `intent.accept`. Two signatures on the same IR — the compiler's says "I produced this", the user's says "I approved this".

---

## Clarification loop

If stage 6 emits `intent.clarify`, the protocol enters a back-and-forth loop:

```
intent.clarify (agent → user)
intent.answer  (user → agent, slot patches via RFC 6902)
   [re-run stage 5 + 6 with patches applied]
intent.clarify (if still unresolved)
...
intent.compiled (once resolved)
```

`pipeline.max_clarify_rounds=3` in `core/pipeline.mtx`. After 3 rounds with no resolution, the pipeline emits `intent.fail` with reason `ambiguous_request`.

---

## Pipeline error handling

`pipeline.on_stage_error=fail_fast` — any stage error immediately aborts the pipeline and emits `intent.fail` with the stage error as evidence. There is no partial-failure recovery within a single pipeline run.

The `pipeline.timeout_ms=5000` is a hard wall-clock ceiling for the entire pipeline (not per-stage). Exceeding it → `intent.fail` with reason `timeout`.

---

## How the interpreter works

The interpreter (`MCL/mtx/interpreter/`) is the thing that actually executes stage 4. It receives a parsed `ast.File` (the skill's SKILL.mtx), an `LLM` interface, and a `Cortex` interface. Its job is to walk `§PROCEDURE`.

```go
interp := interpreter.New(file, llmClient, cortexClient)
result, err := interp.Run(ctx, &interpreter.RunInput{
    Prose:      "user's goal text",
    Verb:       "build",             // from stage 2
    Bundle:     cortexBundle,        // from stage 3
    Grammar:    "intent_frame@1",
    Confidence: 0.95,
    SlotValues: map[string]string{}, // pre-fills from intent.draft body
})
```

The `RunResult` contains:
- `FrameJSON` — raw output from the LLM prompt
- `PromptMessages` — the interpolated messages that were sent (for audit/display)
- `Slots` — final state of all input slots
- `Unknowns` — registered gap declarations
- `ClarifyQuestions` — generated questions
- `MatchedCondition` — which on-block condition matched
- `StepKindHint` — the `kind=` annotation from the matched block
- `OutputCardinalityHint` — the `output_cardinality=` annotation

Both `LLM` and `Cortex` are interfaces (not concrete types), so the interpreter is testable without a real API or cortex instance. Pass `nil` for either to enter dry-run mode: prompts are built and interpolated but no LLM call is made.

```go
// Dry-run: no LLM, no cortex
interp := interpreter.New(file, nil, nil)
```

### Streaming

The interpreter exposes a `StreamingLLM` interface for incremental token delivery:

```go
type StreamingLLM interface {
    LLM
    Stream(ctx context.Context, messages []Message, grammar string,
        onDelta func(delta string)) (string, error)
}
```

`Stream` must return the same final text as an equivalent `Decode` call. Callers type-assert `llm.(interpreter.StreamingLLM)` and fall back to `Decode` when the assertion fails. The canonical output (and therefore D11 hash) is always the full text regardless of whether streaming was used.

---

## Modifying the pipeline

To change pipeline behaviour, edit the relevant `.mtx` file:

| What to change | Where |
|---|---|
| Verb classification prompt | `MCL/core/verb.mtx` — `classifier.prompt` |
| Verb routing (cortex memory types per verb) | `MCL/core/verb.mtx` — `route.<verb>=...` |
| Stage parameters (token budgets, timeouts) | `MCL/core/pipeline.mtx` |
| Confidence thresholds | `MCL/core/confidence.mtx` |
| Frame schema | `MCL/core/frame.mtx` |
| A skill's compile-time procedure | `skills/<slug>/SKILL.mtx` — `§PROCEDURE` |

Any change to a `.mtx` file changes its `mtx_digest`, which changes the D11 seed for all future compilations using that file. Old compilations remain verifiable against their pinned digests.
