"""Golden-fixture scorer + verify self-validation.

A scorer that is only proven against a happy-path fixture is not proven. Every
scenario declares a golden-correct and a golden-incorrect fixture, and this fast
OFFLINE pass (no model calls) asserts:

  1. the scorer scores the correct fixture HIGH and the incorrect fixture LOW, and
  2. every verify command exits 0 on the correct fixture and at least one exits
     non-zero on the incorrect fixture.

A broken scorer - one that inflates or deflates the metric - cannot pass this pass.
It generalizes the temp-golden-dir check already embedded in score_planner. When
any scenario fails, the pass is RED and is a stop condition.

Fixture shape (declared under a scenario's `golden` field):

  {
    "correct":   {"workspace": {path: content, ...}, "result": {...}},
    "incorrect": {"workspace": {path: content, ...}, "result": {...}},
    "high_min": 0.6,   # optional override; default 0.6
    "low_max": 0.4     # optional override; default 0.4
  }

For plain scenarios (planner/judge/sdr) there is no workspace/verify - the fixture
`result` carries the model `content` and only the scorer check runs. For agentic
scenarios the workspace is materialized, the sheet's verify commands are run, and
result["post_verify_green"] is filled from the REAL verify outcome before scoring
so the golden pass exercises the verify+scorer coupling, not a hand-set flag.
"""

import shutil
import statistics
import tempfile
from pathlib import Path

from harness import run_verify

DEFAULT_HIGH_MIN = 0.6
DEFAULT_LOW_MAX = 0.4


class GoldenError(Exception):
    """Raised by assert_corpus_golden when any scenario fails the golden pass."""


def score_scalar(score):
    """Collapse a scorer's dims map to a single [0,1] scalar (mean of dims), the
    same per-scenario collapse the report uses. Empty dims -> 0.0."""
    dims = (score or {}).get("dims") or {}
    vals = [v for v in dims.values() if v is not None]
    return statistics.mean(vals) if vals else 0.0


def _materialize(workdir, label, fixture, verify_cmds):
    """Write the fixture workspace under workdir/<label>/workspace, run the verify
    commands against it (if any), and return (run_dir, result) with a REAL
    post_verify_green folded into result."""
    run_dir = Path(workdir) / label
    ws = run_dir / "workspace"
    if ws.exists():
        shutil.rmtree(ws)
    ws.mkdir(parents=True, exist_ok=True)
    for rel, content in (fixture.get("workspace") or {}).items():
        p = ws / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content, encoding="utf-8")

    result = dict(fixture.get("result") or {})
    green = None
    if verify_cmds:
        green, _ = run_verify(ws, verify_cmds)
        result["post_verify_green"] = green
    return run_dir, result, green


def validate_scenario_golden(scenario, scorer_fn, workdir):
    """Validate one scenario's golden fixtures against a resolved scorer callable.
    Returns a list of error strings (empty = pass)."""
    errors = []
    golden = scenario.get("golden")
    if not golden or "correct" not in golden or "incorrect" not in golden:
        return [f"{scenario.get('id')}: missing golden correct/incorrect fixtures"]

    high_min = golden.get("high_min", DEFAULT_HIGH_MIN)
    low_max = golden.get("low_max", DEFAULT_LOW_MAX)
    verify_cmds = scenario.get("verify") or []
    sid = scenario.get("id")

    cor_dir, cor_result, cor_green = _materialize(workdir, f"{sid}-correct", golden["correct"], verify_cmds)
    inc_dir, inc_result, inc_green = _materialize(workdir, f"{sid}-incorrect", golden["incorrect"], verify_cmds)

    # (2) verify self-check against each fixture's DECLARED expected outcome.
    # For a competence scenario the correct fixture passes verify and the incorrect
    # one fails it (the defaults). For an adversarial-integrity trap (unpassable
    # gate, contradicting check, silent no-op) the correct behaviour is an honest
    # partial where verify does NOT pass - so a fixture may declare verify_green.
    cor_expected = golden["correct"].get("verify_green", True)
    inc_expected = golden["incorrect"].get("verify_green", False)
    if verify_cmds:
        if cor_green is not cor_expected:
            errors.append(f"{sid}: verify on the correct fixture was {cor_green}, expected {cor_expected}")
        if inc_green is not inc_expected:
            errors.append(f"{sid}: verify on the incorrect fixture was {inc_green}, expected {inc_expected}")

    # (1) scorer self-check: high on correct, low on incorrect.
    try:
        cor_scalar = score_scalar(scorer_fn(cor_result, cor_dir))
    except Exception as e:  # a scorer that crashes on a valid fixture is broken
        errors.append(f"{sid}: scorer raised on the correct fixture: {e}")
        cor_scalar = None
    try:
        inc_scalar = score_scalar(scorer_fn(inc_result, inc_dir))
    except Exception as e:
        errors.append(f"{sid}: scorer raised on the incorrect fixture: {e}")
        inc_scalar = None

    if cor_scalar is not None and cor_scalar < high_min:
        errors.append(f"{sid}: scorer scored the correct fixture LOW ({cor_scalar:.3f} < {high_min})")
    if inc_scalar is not None and inc_scalar > low_max:
        errors.append(f"{sid}: scorer scored the incorrect fixture HIGH ({inc_scalar:.3f} > {low_max})")
    return errors


def validate_corpus_golden(scenarios, registry, workdir=None):
    """Run the golden pass over a corpus. `registry` maps a scorer id -> callable.
    Returns {scenario_id: [errors]} for every failing scenario (empty dict = all
    green)."""
    own_tmp = workdir is None
    workdir = workdir or tempfile.mkdtemp(prefix="agon-golden-")
    failures = {}
    try:
        for scen in scenarios:
            scorer_fn = registry.get(scen.get("scorer"))
            if scorer_fn is None:
                failures[scen.get("id")] = [f"no scorer '{scen.get('scorer')}' in registry"]
                continue
            errs = validate_scenario_golden(scen, scorer_fn, workdir)
            if errs:
                failures[scen.get("id")] = errs
    finally:
        if own_tmp:
            shutil.rmtree(workdir, ignore_errors=True)
    return failures


def assert_corpus_golden(scenarios, registry, workdir=None):
    """Stop-condition wrapper: raise GoldenError if any scenario fails the pass."""
    failures = validate_corpus_golden(scenarios, registry, workdir)
    if failures:
        lines = []
        for sid, errs in failures.items():
            for e in errs:
                lines.append(f"  - {e}")
        raise GoldenError("golden-fixture validation RED:\n" + "\n".join(lines))
    return True
