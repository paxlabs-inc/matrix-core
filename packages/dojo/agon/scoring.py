"""AGON scoring statistics: per-item confidence intervals + flake accounting.

A number without a confidence interval is not a metric. Every item is run N reps;
this module turns those reps into a defensible reported score:

  - a per-rep scalar in [0,1] (the mean of the rep's scored dimensions, matching
    the pre-port per-scenario cell in run.py),
  - a mean over the reps that actually produced a scored transcript,
  - a bootstrap percentile confidence interval (seeded, so the CI is itself
    reproducible), and
  - a flake rate: the fraction of scored reps whose pass/fail outcome disagrees
    with the item's modal outcome (nondeterministic flip), reported openly.

API-error reps carry no score and are EXCLUDED from the mean/CI/flake denominators
and counted separately (excluded transparently, never silently folded into a
lower score). An item whose reps were all API errors reports no score.
"""

import random
import statistics

# A rep counts as an API error (no scoreable transcript) when its result ended by
# an API error. Such reps are excluded from scoring denominators.
API_ERROR_ENDINGS = {"api_error"}

PASS_THRESHOLD = 0.5


def rep_scalar(score):
    """Collapse one rep's scored dimensions into a single [0,1] scalar (the mean
    of its dims), matching run.py's per-scenario cell. An empty dims map scores 0."""
    dims = (score or {}).get("dims") or {}
    vals = [v for v in dims.values() if v is not None]
    return statistics.mean(vals) if vals else 0.0


def is_api_error(result):
    return (result or {}).get("ended_by") in API_ERROR_ENDINGS


def bootstrap_ci(values, *, confidence=0.95, resamples=2000, seed=0):
    """Percentile bootstrap CI of the mean of `values`. Deterministic for a given
    seed, so the reported interval is itself reproducible. Returns (lo, hi); with a
    single value the interval collapses to that point."""
    vals = [v for v in values if v is not None]
    if not vals:
        return None
    if len(vals) == 1:
        return (round(vals[0], 4), round(vals[0], 4))
    rng = random.Random(seed)
    n = len(vals)
    means = []
    for _ in range(resamples):
        sample = [vals[rng.randrange(n)] for _ in range(n)]
        means.append(statistics.mean(sample))
    means.sort()
    alpha = (1.0 - confidence) / 2.0
    lo = means[int(alpha * (resamples - 1))]
    hi = means[int((1.0 - alpha) * (resamples - 1))]
    return (round(lo, 4), round(hi, 4))


def flake_rate(scalars):
    """Fraction of scored reps whose pass/fail (scalar >= PASS_THRESHOLD) disagrees
    with the modal outcome. 0.0 means every rep agreed (stable); higher means the
    item flips nondeterministically. Undefined (None) with fewer than 2 reps."""
    if len(scalars) < 2:
        return None
    passes = [1 if s >= PASS_THRESHOLD else 0 for s in scalars]
    n_pass = sum(passes)
    minority = min(n_pass, len(passes) - n_pass)
    return round(minority / len(passes), 4)


def item_stats(reps, *, confidence=0.95, resamples=2000, seed=0):
    """Reduce a list of rep records for ONE item to a reported score with a CI and
    flake rate. Each rep record is {"result": {...}, "score": {"dims":..,"flags":..}}.

    API-error reps are excluded from mean/CI/flake and counted in `api_errors`.
    Returns a dict carrying the mean, CI, flake_rate, counts, and the union of
    non-transient flags observed across scored reps."""
    total = len(reps)
    scored, api_errors = [], 0
    flags = set()
    for r in reps:
        if is_api_error(r.get("result", {})):
            api_errors += 1
            continue
        scored.append(rep_scalar(r.get("score", {})))
        for f in (r.get("score", {}) or {}).get("flags", []):
            flags.add(f)
    if not scored:
        return {"mean": None, "ci": None, "flake_rate": None, "reps": total,
                "scored_reps": 0, "api_errors": api_errors, "flags": sorted(flags)}
    return {
        "mean": round(statistics.mean(scored), 4),
        "ci": bootstrap_ci(scored, confidence=confidence, resamples=resamples, seed=seed),
        "flake_rate": flake_rate(scored),
        "reps": total,
        "scored_reps": len(scored),
        "api_errors": api_errors,
        "flags": sorted(flags),
    }


def aggregate_flake(item_stat_list):
    """Run-level flake summary over a list of item_stats: the overall API-error
    rate and the mean per-item flake rate (over items that had >=2 scored reps)."""
    total_reps = sum(s["reps"] for s in item_stat_list)
    api_errors = sum(s["api_errors"] for s in item_stat_list)
    flakes = [s["flake_rate"] for s in item_stat_list if s["flake_rate"] is not None]
    return {
        "api_error_rate": round(api_errors / total_reps, 4) if total_reps else None,
        "api_errors": api_errors,
        "total_reps": total_reps,
        "mean_item_flake": round(statistics.mean(flakes), 4) if flakes else None,
        "items_with_flake": sum(1 for f in flakes if f > 0),
    }
