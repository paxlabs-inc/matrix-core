# mcl-e2e — Centra AI v1 end-to-end live test

A single Go binary that drives every layer of the Centra AI v1 stack against
real LLMs (Fireworks + Together), real MCP servers (`fs` + `fetch` + `git`
spawned as subprocesses), and a real Pebble-backed cortex with the real
Fireworks `nomic-embed-text-v1.5` 768-dim embedder.

This is the live equivalent of `go test ./...` for the v1 critical path.
Every layer that exists is exercised in production-like configuration.

## What it covers

| Layer | What gets tested |
|-------|------------------|
| `cortex` | Pebble store, deterministic clock + IDs, real `embed.APIEmbedder` against Fireworks `/v1/embeddings`, Snapshot, Attest, EMA weight learning, `Rebuild` byte-identical replay (§13.4) |
| `core/mcl/mtx` | parser, validator, canonical AST hash (`mtx_digest`) |
| `core/mcl/llm` | real `APIClient` against Fireworks DeepSeek-V4-Flash and Together openai/gpt-oss-120b with grammar-constrained decode |
| `core/mcl/mtx/interpreter` | real on-block execution against real LLM via `bridge.Adapter` |
| `core/mcl/ir` | Intent + PlanTree typed IR, canonical JSON, content-address hash, `ValidatePlan` 11 invariants |
| `core/mcl/envelope` | 15 typed body kinds, ed25519 sign + Verify, on-disk JSON journal, SelfHash |
| `bridge` | `Adapter` wiring `interpreter.Cortex` to live `*cortex.Cortex` |
| `executor/lifecycle` | full state-machine surface (drafting → proposed → clarifying → accepted → executing + non-material correct self-loop + material correct rewind → completed) |
| `executor/mcp` | JSON-RPC 2.0 client over real stdio subprocesses, `Manager.verifyTools` Q21 manifest match |
| `executor/tool` | Registry resolves `matrix://tool/mcp/<alias>/<name>@<v>`, capability gate, `MCPTool.Call` IsError handling |

## How it runs

Three independent runs in sequence:

| Run | Provider     | Model                                            | Purpose                          |
|-----|--------------|--------------------------------------------------|----------------------------------|
| A   | Fireworks    | `accounts/fireworks/models/deepseek-v4-flash`    | primary live run                 |
| B   | Fireworks    | `accounts/fireworks/models/deepseek-v4-flash`    | repeat for D11 determinism check |
| C   | Together AI  | `openai/gpt-oss-120b`                            | cross-model robustness           |

Each run produces a fresh per-run workspace, fresh cortex Pebble DB, and
fresh MCP subprocess pool. Artefacts land under `runs/<ts>/<run>/`:

```
runs/20260524-124557/
├── A/
│   ├── workspace/           ← fs-mcp jail (real files written/read here)
│   ├── repo/                ← git-mcp jail (initialised git repo)
│   ├── core/cortex/              ← Pebble store
│   ├── journal/<intentID>/  ← signed envelope JSON files (one per kind)
│   ├── agent-manifest.json  ← synthesised tool.AgentManifest
│   ├── transcript.jsonl     ← per-event JSONL audit log
│   └── mcp-stderr.log       ← captured subprocess stderr
├── B/                       ← same shape
├── C/                       ← same shape
└── TOPLEVEL.jsonl           ← cross-run analysis events
```

## Per-run flow (8 phases)

1. **Setup** — generate ed25519 keypair from fixed seed, build `tool.AgentManifest` with synthesised Q22-compliant `sha256:<64hex>` `package_digest` per server, spawn `fs` + `fetch` + `git` MCP servers via real `npx`/`uvx` subprocesses, `Manager.verifyTools` exact tool-list match.
2. **Seed cortex** — write Identity + 2 Facts + Goal + Constraint + Pattern under fixed clock + deterministic ID generator → byte-stable post-seed `OverallRoot`. Drain real Fireworks embedder. Snapshot baseline.
3. **Compile Intent** — parse + validate `protocol/skills/writing-plans/SKILL.mtx` (deterministic mtx_digest), real LLM compile via `bridge.Adapter`, decode `FrameJSON` → `ir.Intent`, canonical-JSON hash.
4. **Envelope + lifecycle** — sign `intent.draft` body via ed25519 + persist → drafting → proposed → clarifying → proposed → accepted (4 lifecycle transitions, 4 envelopes).
5. **PlanTree** — hand-build a 6-node `ir.PlanTree` (Sequential containing Sequential fs roundtrip + Parallel reads + ToolCall fetch + Step kind-coverage). `ir.ValidatePlan` enforces all 11 invariants, plan.proposed envelope → executing.
6. **Walk plan against real MCP** — DFS through plan, parallel branches concurrent. Each `tool_call` → `Registry.Get` → real subprocess MCP `tools/call`. Result captured as cortex Event memory + signed `plan.step` envelope. Then non-material `intent.correct` (executing → executing self-loop) + material `intent.correct` (executing → accepted) + replanning (accepted → executing).
7. **Attest + EMA** — `cortex.Attest` cites the Event memories + 3 seed memories. Asserts `KindAttest` + `KindLearnWeights` at consecutive `seq` (Phase 12 atomic batch). `intent.attest` envelope → completed.
8. **Replay byte-identical** — `StopEmbedder`, snapshot, `Cortex.Rebuild`, `replay.VerifyPreservesRoot`. Asserts `pre == post OverallRoot` byte-equal (§13.4 invariant).

## Cross-run analysis

After all 3 runs complete, the harness asserts:

- **A vs B** (same model + seed):
  - `mtx_digest` byte-equal (deterministic AST hash — always passes)
  - `Intent.Hash` byte-equal — **informational only** (Fireworks does not currently honor seed strictly at temp=0)
  - Final `OverallRoot` byte-equal — **informational only** (depends on LLM determinism)
- **A vs C** (cross-model):
  - both produce a non-empty Intent.Hash
  - both reach `lifecycle=completed`
  - both replay-verify (`pre==post` per run)

## Required environment

```bash
export FIREWORKS_API_KEY="fw_..."   # compiler model + embedder
export TOGETHER_API_KEY="tgp_..."   # cross-model run C
```

External binaries (auto-discovered):

- `npx` (Node 20+) for `@modelcontextprotocol/server-filesystem@2026.1.14`
- `uvx` (`uv` from astral.sh) for `mcp-server-fetch@2025.4.7` + `mcp-server-git@2026.1.14`

Install `uv` if missing:

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH=$HOME/.local/bin:$PATH
```

## How to run

```bash
# from /root/centra/executor
go build -o /tmp/mcl-e2e ./cmd/mcl-e2e

# full 3-run cycle (~6 min, real API calls)
set -a; . /root/matrix/.env; set +a
/tmp/mcl-e2e -root /tmp/mcl-e2e-runs

# fast single-run smoke (~2 min, Fireworks only)
/tmp/mcl-e2e -root /tmp/mcl-e2e-runs -skip-determinism -skip-together
```

## Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-root <dir>`            | `$PWD/runs` | Base directory for run artefacts |
| `-skill <path>`          | `/root/matrix/skills/writing-plans/SKILL.mtx` | SKILL.mtx to compile against |
| `-prose "<text>"`        | (sample text) | User natural-language goal |
| `-verb <verb>`           | `build`       | Pre-classified verb (skips stage 2 classifier) |
| `-seed <int>`            | `42`          | LLM seed for D11 determinism |
| `-fireworks-model <id>`  | `…/deepseek-v4-flash` | Fireworks compiler model id |
| `-together-model <id>`   | `openai/gpt-oss-120b` | Together compiler model id |
| `-skip-determinism`      | `false`       | Skip Run B (saves ~2 min + Fireworks budget) |
| `-skip-together`         | `false`       | Skip Run C (saves ~1.5 min + Together budget) |

## Exit code

- `0`: every assertion passed (informational findings recorded but not blocking)
- `1`: at least one assertion failed (transcript carries the details)

## Empirical findings recorded by this harness

1. **Cortex § 13.4 byte-identical replay holds across every mutation surface** — Write / Update / Tombstone / AddEdge / RemoveEdge / Compact / UpdateHead / Embedder / Attest. Pre and post OverallRoot match byte-for-byte after `Cortex.Rebuild` over the journal.

2. **Phase 12 EMA weight learning fires atomically** — every `cortex.Attest` emits `KindAttest` at `seq=N` and `KindLearnWeights` at `seq=N+1` in one Pebble batch. Verified by the harness asserting `AttestResult.LearnSeq == AttestResult.Seq + 1`.

3. **Q21 MCP manifest verification works against real subprocess servers** — `Manager.verifyTools` rejects manifest drift; the harness's manifest matches the real `tools/list` of the published server packages exactly.

4. **D11 strict-byte-equality for Intent.Hash requires deterministic upstream LLM** — Fireworks DeepSeek-V4-Flash at `temp=0 seed=42` does NOT currently produce byte-identical completions across calls. Cortex and the MCL compiler are deterministic by construction; the LLM output is the variability source. Recorded in `TOPLEVEL.jsonl` as `cross-run.AB` event with `finding`.

5. **Cross-model robustness** — both Fireworks DeepSeek-V4-Flash and Together openai/gpt-oss-120b produce valid Intent IRs against the same SKILL.mtx + same prose, both reach `lifecycle=completed`, both replay-verify.
