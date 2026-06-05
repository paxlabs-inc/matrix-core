# `tools/e2e/` — mcl-e2e sweep harness

Drive `executor/cmd/mcl-e2e` N times back-to-back against real Fireworks +
Together + npx/uvx MCP subprocess servers, aggregate the results, and emit a
machine-readable dataset.

## Files

- **`run_sweep.sh`** — orchestrator. Builds `mcl-e2e` if needed, verifies API
  keys, loops `N` iterations with per-iteration timeout, captures stdout +
  stderr per iter, continues on individual failures, kicks off the aggregator
  at the end.
- **`aggregate_sweep.py`** — pure-stdlib aggregator. Walks every iteration's
  transcript JSONL files, extracts compile latency / tool call distribution /
  replay invariant / D11 hash equality / lifecycle reach, and writes
  `summary.{md,csv,json}` into the sweep root.

## Usage

```bash
# Default: N=50, full A+B+C sub-runs, 15min/iter timeout.
tools/e2e/run_sweep.sh

# Smoke (2 iterations, custom outdir).
tools/e2e/run_sweep.sh -n 2 -o /root/matrix/runs/sweep-smoke

# Skip the Together cross-model run to save budget.
tools/e2e/run_sweep.sh --skip-together

# Background: detach + tail-friendly log.
nohup tools/e2e/run_sweep.sh -n 50 > /root/matrix/runs/sweep.nohup 2>&1 &
```

## Output layout

```
/root/matrix/runs/sweep-<TS>/
├── sweep.config.json     runner config (n, models, prose, host, started_at)
├── sweep.log             one-line per-iteration status (pass/fail/timeout/dur)
├── iterations/
│   ├── iter-001/
│   │   ├── stdout.log    mcl-e2e stdout
│   │   ├── stderr.log    mcl-e2e stderr (banners + assertion log)
│   │   └── <e2e-ts>/     mcl-e2e -root: per-sub-run artefacts
│   │       ├── TOPLEVEL.jsonl
│   │       ├── A/transcript.jsonl + workspace/ + repo/ + manifest.json
│   │       ├── B/transcript.jsonl + ...
│   │       └── C/transcript.jsonl + ...
│   ├── iter-002/...
│   └── ...
├── summary.md            human-readable headline
├── summary.csv           one row per iteration (machine-parseable)
└── summary.json          full structured aggregate
```

## Metrics surfaced

Per sub-run (A, B, C):
- **Replay invariant rate** — pre/post OverallRoot equal after Snapshot →
  DropDerived → Rebuild (research/04 §13.4).
- **Compile latency** — `compile.llm.complete.ms` distribution (min, p50,
  p95, p99, max, mean).
- **Tool call count + error rate** — `plan.tool.dispatch` / `plan.tool.result`
  per iter; `is_error=true` raises the error count.
- **Per-tool latency** — `plan.tool.result.ms` distribution.
- **Weights updated rate** — fraction of iterations where the salience EMA
  step actually mutated `meta/salience_weights` (Phase 12).
- **Distinct intent_hashes / plan_hashes / post_roots** — diversity counts
  across iterations (D11 informational; Fireworks does not honor seed today
  per sess#22b finding).

Cross-run:
- **A==B Intent.Hash equality rate** — D11 informational only (cortex+MCL
  determinism is the hard contract; LLM-side determinism is the variable).
- **A==B OverallRoot equality rate** — cascades from intent hash variability.

## Known properties

- Replay invariant should be **100%** every iteration (cortex spec §13.4 is
  byte-deterministic by construction).
- A==B byte-equal rates are **expected near 0%** with Fireworks today (seed
  not honored upstream); will rise to 100% if a deterministic provider lands
  or the compile slot moves to a local model.
- Tool error rate should be **0%** absent network flakiness — fs/git MCP
  servers operate on a per-iteration workspace + repo.
