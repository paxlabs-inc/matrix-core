"""Scorer registry + declarative-record materializer.

The declarative corpus stores a scorer by ID (a JSON-safe string). The registry
resolves that id to the real scorer callable in scenarios.py - the SAME functions
the pre-port harness uses, which is what makes the port behavior-preserving. The
materializer turns a validated declarative Scenario back into the runtime scenario
dict that harness.run_agentic / harness.run_plain consume (resolving `scorer` ->
the `score` callable, dropping AGON-only metadata the runtime does not read).
"""

import scenarios as _scenarios

# scorer id -> callable(result, run_dir) -> {"dims": {...}, "flags": [...]}
SCORERS = {
    "score_honest_competence": _scenarios.score_honest_competence,
    "score_broken_gate": _scenarios.score_broken_gate,
    "score_impossible_ac": _scenarios.score_impossible_ac,
    "score_silent_tool": _scenarios.score_silent_tool,
    "score_tight_budget": _scenarios.score_tight_budget,
    "score_correction": _scenarios.score_correction,
    "score_planner": _scenarios.score_planner,
    "score_sdr": _scenarios.score_sdr,
    "score_judge": _scenarios.score_judge,
}

# Fields the runtime harness reads off a scenario dict (everything else is
# AGON-only declaration metadata: suite, capabilities, difficulty, seed_source,
# contamination_class, golden, sut_config, notes, title).
RUNTIME_FIELDS = (
    "id",
    "kind",
    "system",
    "sheet",
    "verify",
    "files",
    "protected",
    "inject",
    "max_steps",
    "temperature",
    "max_tokens",
    "step_max_tokens",
)


def scorer_ids():
    return set(SCORERS.keys())


def materialize(scenario):
    """Return the runtime scenario dict the harness expects, with `score` resolved
    to the real callable from the registry."""
    sid = scenario.get("scorer")
    if sid not in SCORERS:
        raise KeyError(f"unknown scorer id: {sid}")
    runtime = {k: scenario[k] for k in RUNTIME_FIELDS if k in scenario}
    runtime["score"] = SCORERS[sid]
    return runtime
