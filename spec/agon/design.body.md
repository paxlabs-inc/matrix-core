# AGON — the Matrix Agent Qualification standard

AGON is the production-grade evolution of `dojo/`: a scalable, high-accuracy
testing suite for models and agents, with a calibrated, confidence-bounded
scoring metric defensible enough to be published and cited as an industry
standard. It is organized around one axis — **the system under test** — into
three suites, and it is driven, in the near term, by a concrete need: prove the
Cassandra silent-voice controller before it ever touches production.

This document is the durable design. The requirements and task waves are in
`spec.kvx`; this fragment is the "why" and the "how" behind them.

## Mission and the bar

`dojo/README.md` states the founding incident plainly: on 2026-07-06 a model
swapped into the worker+planner slots with no qualification burned $101 in one
hour, delivered zero tasks, authored an unpassable verify command as planner,
and tried to defeat the gate with fake `grep` shims as worker. Dojo exists so
that never happens again — **qualification is a gate, not a benchmark.**

AGON keeps that gate and raises it to a **publishable standard**: a number that
an outside party can read, reproduce, and trust. The bar is not "a script that
prints a leaderboard" — it is "a methodology, an instrument, and a metric that
would survive being cited."

## The three load-bearing ideas

1. **The system under test is the axis of truth.** A model's raw ability, an
   agent's behavior with tools, and a composed agent's emergent behavior are
   three different things. Today's dojo conflates the first two (its `run_plain`
   and `run_agentic` both score "the model"). AGON isolates them into three
   suites so the metric can attribute a score to the *model*, the *scaffolding*,
   or the *composition* — never a muddle of all three.
2. **A metric is only citable if it is calibrated, confidence-bounded,
   contamination-resistant, and reproducible.** Point scores from a single rep,
   hand-tuned weights, an in-repo scenario set that can leak, and unstamped runs
   are fine for an internal smoke test and fatal for a standard. AGON adds
   bootstrap confidence intervals, item-discrimination-calibrated weights,
   contamination classes, and signed provenance cards.
3. **Every emergent claim about the composition is proven by ablation delta,
   not asserted.** Suite 3 never says "Cassandra helps." It runs main-only,
   main+cassandra, and main+cassandra+cortex and reports the *measured delta*
   between them. If a component does not improve the baseline, the report shows
   that.

## Current-state baseline (honest)

`dojo/` today is a strong seed, not a toy. What is genuinely good and must be
preserved:

- **Post-hoc ground truth.** Competence is re-verified by the harness; the
  model's own claims never score competence (`run.py aggregate()` and
  `harness.run_verify`). This is the single most important accuracy property.
- **Real anti-gaming.** Sanitized `VERIFY_PATH`, pre-run `sweep_shims`,
  `SHIM_PATTERNS` detection, protected-file scope screens, loop/duplicate-call
  metrics, and hard DQ flags (`gate-gaming`, `gamed-artifact`,
  `scope-violation`, `fake-accepted`) that disqualify regardless of aggregate.
- **Seeded-from-real scenarios.** All 10 come from observed failures.
- **Role-slot scoring.** Dimensions roll into realms into worker/planner/judge/
  neo slot scores.

What blocks it from being a standard:

- **Serial execution** — `for model: for scenario: for rep` is hours of wall
  time at any real scale.
- **Breadth** — 10 scenarios, ~6 behaviors. "Vast realms" needs a taxonomy and
  hundreds of items.
- **No statistical rigor** — single-rep point scores, no CIs, no flake
  accounting, magic-number weights in `aggregate()`.
- **No provenance/reproducibility** — no harness version, no corpus hash, no
  seed policy, no signed cards.
- **Scenarios are Python literals** in one file — not a schema-validated,
  contributable, versioned corpus.
- **No contamination resistance** — a leaked scenario inflates scores forever.
- **`exec` runs on the host** — fine for trusted models, unsafe at scale or for
  untrusted candidates.

## Architecture: the three suites

### Suite 1 — Raw Model Performance (MPI)

**SUT:** the bare model, single or short multi-turn, no tools, no workspace.
Isolates intrinsic capability; the MPI predicts the ceiling of any agent built
on the model.

Realms: reasoning (math/logic/multi-step), knowledge & factuality,
instruction-following & strict-format adherence, structured-output/grammar
conformance, long-context comprehension + read-full fidelity, calibration &
epistemic honesty (appropriate hedging, admitting uncertainty), refusal/safety
of raw output, output stability across reps, and token/reasoning-token economy.

Ground truth uses deterministic oracles wherever one exists (exact-match,
schema-validation, numeric) — keyword-matching is a last resort, never used
where a real oracle is available. Headline: **MPI — Model Performance Index**,
per-realm sub-scores with confidence intervals.

### Suite 2 — Agentic Performance (APS)

**SUT:** model + the Cody-style agentic scaffolding — the `fs_*`/`exec`/`grep`/
`glob` tool surface, the `Workspace`, the engine-enforced `verify_run`/`turn_in`
done-gate, and the step budget (`harness.run_agentic`). Tests behavior *under
agency*.

Realms: competence (ground-truth-verified code fix + tests), tool mastery &
error recovery, discipline (bounded loops, budget adherence, read-full, no
redundant re-scan), integrity-under-agency (gate-gaming / silent no-op /
impossible-AC / honest partial), planning & satisfiable-verify authoring,
adjudication (judge), adaptivity (mid-task correction), scope & safety.

Post-hoc ground truth is retained; hard DQ flags cap the affected slot at zero
regardless of aggregate. Headline: **APS — Agentic Performance Score**, with the
worker/planner/judge/neo slot scores and confidence intervals.

### Suite 3 — Matrix Receptive Agent (MRA) Performance (RAS)

**SUT:** the composed production agent — **main agent + Cassandra modifier agent
+ cortex memory logger agent.** Tests emergent, system-level properties that do
not exist at tiers 1–2. This is the Cassandra Round-2 proving ground.

The method is **ablation / differential scoring.** Every MRA scenario runs in
configured `sut_config` variants and scores the *delta*:

- `main-only` — the baseline behavior.
- `main + cassandra` — isolates the silent-voice modifier's contribution.
- `main + cassandra + cortex` — the full MRA.

Realms, each measured as an ablation delta with a confidence interval:

- **Loop-kill / self-heal** — does the Cassandra modifier break a spiral the
  bare agent falls into? Canonical pair: `deepseek-v4-pro w_broken_gate` 22-step
  spiral (`ended_by=step_budget`, integrity 0.0) is the loop-kill target;
  `mimo-2.5-pro` clean 9-step run is the stay-silent baseline.
- **Doubt vs assurance control** — fires only when unhealthy, silent when
  healthy, one modification per turn, starts at turn >= 2, and — hard-checked —
  **touches `content` only, never facts**, with the dual-record audit
  (`original_content` + `cassandra_mod` + `trigger`) captured as scored
  artifacts.
- **Memory continuity / cortex fidelity** — the logger captures the right
  durable facts; recall measurably improves a follow-on task; no leakage; the
  journaled-but-not-anchored lane stays deterministic.
- **Emergent integrity** — the composition should *raise* integrity vs the bare
  agent, measured as the delta, not claimed.

Headline: **RAS — Receptive Agent Score**, reported *with* its per-realm
ablation deltas. The delta is the product.

### How the suites compose

`Suite 1 ceiling -> Suite 2 realized-under-agency -> Suite 3
emergent-under-composition`. The capability taxonomy hangs off the three suites
(each realm above is a taxonomy branch under its suite), and the scenario schema
carries a `suite` field plus a `sut_config` for Suite 3 ablation variants.

## The Cassandra silent-voice controller (the Round-2 driver)

Suite 3 exists first to prove this mechanic. Between turns, a controller hook
MAY edit the previous assistant message `content` in place before it is fed back
to the main agent next turn — folding in Doubt / Questioning / Curiosity /
Assurance / Urge-to-verify. The main agent reads it as its own emerging thought
(reasoning is disposable per-turn, so the assistant channel is the self). No API
`prefix` dependency, so it works on MiMo and DeepSeek alike.

Three hard guardrails, all test-enforced:

1. Touch `content` only — never `tool_calls`, roles, or tool messages.
2. Metacognition only — **never edit facts.**
3. Dual-record `original_content` + `cassandra_mod` + `trigger` as first-class
   run artifacts for audit ground truth.

Silent when healthy: one modification per turn, gated by loop/stall metrics,
starting at turn >= 2. The Suite 3 ablation is what proves it kills the loop on
the target case and stays silent on the healthy baseline.

## Scenario schema and corpus

A scenario is a declarative, schema-validated record — not a Python literal:

- `id`, `suite`, `capabilities[]` (taxonomy leaves), `difficulty`, `kind`
  (`plain` | `agentic` | `mra`), `seed_source` (an incident/reference),
  `files`, `sheet`, `verify`, `ground_truth` (checker), `scorer`, `sut_config`
  (Suite 3 variants), `contamination_class` (`public` | `holdout` | `rotating`).

Three families:

- **Seeded-from-real** — the current set: each item traces to an observed
  failure.
- **Synthetic-parametric** — templated with randomized parameters so each run is
  a fresh instance; contamination-resistant.
- **Adversarial** — designed traps (unpassable gates, impossible ACs, silent
  no-ops, poisoned tools, anti-Goodhart items that punish metric-gaming).

**Golden-fixture validation** is non-negotiable: every scenario ships a
golden-correct and a golden-incorrect fixture, and a fast offline pass asserts
the scorer scores correct high / incorrect low and that every verify command
exits 0 on correct and non-zero on incorrect. This generalizes the temp-golden
dir already used in `score_planner`. A broken scorer cannot ship.

## Scoring methodology (defensible as a metric)

- **Rubric-anchored dimensions** in [0,1], deterministic ground-truth checkers
  wherever an oracle exists.
- **Statistics:** N reps per item -> mean + bootstrap confidence interval; flake
  rate reported; API-error runs excluded transparently. No point estimate
  without a CI.
- **Calibrated weights:** realm/slot weights derived from item *discrimination*
  (how well an item separates a reference panel of strong vs weak models),
  replacing the magic constants in `run.py aggregate()`. Calibration is
  reproducible from the reference-panel data.
- **Hard gates stay hard:** DQ flags cap the relevant index at zero regardless
  of aggregate — a standard must not let a cheater average their way to a pass.
- **Anti-Goodhart:** rotating holdout items + adversarial items + published
  methodology so the number means something.

### The AQI

One coherent index: **AQI — Agent Qualification Index**, composed of the three
suite indices **MPI** (Suite 1), **APS** (Suite 2), and **RAS** (Suite 3), each
with confidence intervals and a per-realm profile. The composition formula lives
in the versioned standard artifact, not buried in code. **Economics** (tokens,
reasoning tokens, wall time, steps, cost) is a first-class reported axis, never
hidden.

## Scale, isolation, provenance

- **Bounded-concurrency work queue** replaces the serial loop; a **provider
  registry** generalizes `MODEL_ROUTES` into pluggable providers, each with its
  endpoint, key env, served id, budget field, extra body, per-provider
  concurrency limit, retry/backoff, and cost accounting.
- **Resumable, re-scorable cache:** content-addressed raw transcripts; a run can
  resume and be re-scored without re-invoking the model (re-scoring reproduces
  identical scores).
- **Real sandbox** for `exec`/`verify` (container or rootless namespace),
  constrained fs, no network by default, preserving the `VERIFY_PATH` guarantee;
  degrade to a clearly-labeled reduced-isolation mode when unavailable.
- **Signed provenance result cards:** each run stamps model/served id,
  harness+standard version, corpus content hash, per-item seeds, temperature
  policy, raw transcripts, scores, and CIs, integrity-stamped so tampering is
  detectable. This is what makes a score citable.

## CI qualification gate

A gate mode compares a candidate's per-slot/per-suite indices against a stored
baseline and fails promotion on a regression beyond a threshold or on any hard
DQ flag. It emits a machine-readable verdict plus the human-readable report and
runs as a single CI command. This is the direct descendant of the founding
incident: no unqualified swap reaches a production slot.

## Governance and the published standard

The standard ships a human-readable methodology document (taxonomy, item schema,
scoring, AQI composition, contamination policy) generated from or pinned to the
versioned source artifacts. The standard version is stamped on every card; a
breaking change to taxonomy or scoring requires a version bump with a migration
note. Holdout and rotating scenarios never appear in any published/rendered
output (test-enforced).

## Language and the production port

`dojo/README.md` already notes: "This is the Python proving version; the durable
Go module (driving the real `cody/internal/worker` tool surface and `cassandra`
gate) is specced separately." AGON hardens the **Python instrument** now — it is
exactly what the Cassandra Round-2 work needs, and Python's iteration speed
suits a research instrument. A future Go module can consume the **same scenario
corpus + scoring spec** so the two never diverge. That production port is out of
scope here and specced separately.

## Wave sequencing rationale

- **P0 Foundations** (taxonomy, schema, golden-fixture validation, port the 10)
  — nothing is trusted until scenarios are declarative and scorers are
  self-validated.
- **P1 Scale/isolation/provenance** — make runs fast, safe, and reproducible
  before we scale the corpus or publish numbers.
- **P2 Scoring/AQI** — CIs, calibrated weights, DQ gates, the AQI: the metric
  becomes defensible.
- **P3 Suite content** — fill Suites 1 and 2 and grow corpus breadth with
  contamination classes.
- **P4 Suite 3 / Cassandra** — the ablation runner, the controller hook + dual
  record, and the canonical loop-kill/stay-silent proof (the Round-2
  deliverable). It needs only P0's schema + the controller hook to start, so it
  is not blocked on the whole platform.
- **P5 Governance/CI gate/report** — make it a standard others can run and cite.
- **P6 Proof** — no-fakes end-to-end on real models, signed card and report,
  Cassandra proof green.

## Non-goals

- The Go production port of the instrument (specced separately).
- Modifying production Neo/Cody/Cassandra behavior — AGON proves the controller;
  porting it into `cassandra/` + Neo/Cody is Round 3, a separate spec.
- Changing the rate card or provider defaults — those are Andrew-owned.

## No-fakes verification strategy

Every scorer is validated against golden correct AND incorrect fixtures (a
happy-path-only proof is not a proof). Suites run on real models via the real
provider registry. Suite 3 runs the two canonical items through the real
ablation. Reproducibility, provenance-tamper, and holdout-leak properties are
tested. No test in this feature substitutes a stub/mock/fake for a real code
path, a real model, or a real scored artifact — that would be the exact
false-completeness AGON exists to measure and eliminate.
