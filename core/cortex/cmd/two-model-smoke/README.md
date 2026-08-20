# two-model-smoke — cortex API smoke under two real LLMs

Drives two LLMs (Together + Fireworks by default) through a shared cortex
instance via OpenAI-compatible tool-calling, then asserts the strongest
cortex invariant: `Cortex.Rebuild` preserves `OverallRoot` byte-identically
after the run.

**Goal:** validate cortex API behaviour under realistic LLM-generated
typed memory inputs. NOT an agent runtime — there is no MCL, no scope
dispatch, no `intent.attest`. Two models share one cortex; each is
identified by a distinct `CreatedBy` string.

## What's exercised

- Typed `Write` / `Update` / `Tombstone` (Phases 2-3) under arbitrary
  LLM-generated `Statement` / `Topic` / `Tag` strings.
- `forms.Render` + `TruncateToTokens` (Phase 4) — rendered short/medium
  forms persisted on disk under CJK / emoji / code that LLMs emit.
- `Find` with `Type` / `HasTag` / `Near` / `Limit` (Phases 4-5).
- `AddEdge` + `IterEdgesOut/In` (Phase 6).
- `UpdateHead` for retag / re-importance flow (Phase 10).
- `Snapshot` + `OverallRoot` stability across two-actor traffic.
- `replay.Rebuild` byte-identical pre/post `OverallRoot` (Phase 11) —
  the strongest invariant.

## What's NOT exercised

- Sub-agent scope dispatch (no DID resolver wired here).
- `intent.attest` feedback loop (Phase 11.5 not built).
- Cross-actor message protocol (no MCL).
- Real semantic Find Near (`HashEmbedder` is sha256-chaos, not
  semantic; R3 blocks swap to a real embedder).

## Usage

```sh
export TOGETHER_API_KEY="tgp_v1_..."
export FIREWORKS_API_KEY="fw_..."

go run ./cmd/two-model-smoke \
    -root /tmp/two-model-cortex \
    -actor andrew \
    -turns 6 \
    -tools-per-turn 5 \
    -transcript /tmp/two-model-transcript.jsonl

# or a clean run from scratch:
rm -rf /tmp/two-model-cortex /tmp/two-model-transcript.jsonl
go run ./cmd/two-model-smoke -root /tmp/two-model-cortex -actor andrew
```

### Flags

- `-root` cortex data root (required)
- `-actor` cortex actor name; both LLMs share this single Pebble DB
  (required)
- `-turns` total exchanges, alternating between the two models
  (default 6)
- `-tools-per-turn` max tool calls a single LLM may make in one turn
  before being forced to emit a final assistant message (default 5)
- `-transcript` JSONL transcript output path (default
  `./transcript.jsonl`)
- `-scenario` name of built-in scenario; currently only `default`
  (collaborative knowledge construction)
- `-temperature` sampling temperature for both models (default 0.4)
- `-no-rebuild-assert` skip the final replay-determinism assertion
  (default false; the assertion is the whole point — only skip for
  partial diagnostics)

### Models

Defaults match what was tested:

- Together AI: `openai/gpt-oss-120b` (researcher / writer)
- Fireworks AI: `accounts/fireworks/models/deepseek-v4-flash`
 (reviewer
  / questioner)

Override with `-model-a` and `-model-b`. The provider is detected
from the model string format: `openai/...` → Together,
`accounts/fireworks/...` → Fireworks. Both endpoints are
OpenAI-compatible chat-completions with tool-calling.

## Tool surface exposed to the LLMs

| Name | Purpose |
|------|---------|
| `cortex_write` | Write a typed memory (Identity/Fact/Preference/Belief/Event/Goal/Constraint/Capability/Pattern) with optional tags |
| `cortex_resolve` | Read a memory by URI |
| `cortex_find` | Query by type / tag / near / limit |
| `cortex_list` | List all memory IDs of a type |
| `cortex_update` | Replace a memory's typed data (bumps version) |
| `cortex_update_head` | Retag / re-importance without bumping version |
| `cortex_tombstone` | Soft-delete with audit reason |
| `cortex_add_edge` | Type-tagged edge (`derived_from`, `references`, ...) |
| `cortex_list_edges` | Walk outgoing/incoming edges |

All return JSON. All errors come back as `{"error": "..."}` so the
LLM can recover.

## Final assertion

After the run:

1. `Snapshot("two-model-smoke-end")` — captures `OverallRoot_pre`.
2. `replay.Rebuild()` — drops every key under `indexes/` namespace
   and rebuilds from canonical state.
3. Compute `OverallRoot_post`.
4. Assert `OverallRoot_pre == OverallRoot_post` byte-identically.

Failure here means the cortex landed in a state where canonical
storage and derived projections disagree — the most important
correctness bug class for the cortex layer. This is exactly the
Phase 11 invariant `TestReplayProducesIdenticalSnapshotRoots`
asserts on synthetic input; this harness asserts it on
LLM-generated input.

## Cost guard

Both providers charge per token. Default `-turns 6 -tools-per-turn 5`
caps at roughly 60 LLM API calls per run. With both models being
small/fast (~$0.20–$0.50/M tokens) a full run is single-digit cents.
Bump turns/tools-per-turn for longer-horizon stress tests.
