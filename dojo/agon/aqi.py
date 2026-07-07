"""AGON headline metric: the Agent Qualification Index (AQI).

The AQI composes the three suite sub-indices into one legible number:

  - MPI  (Suite 1 Raw Model)   - intrinsic capability, the ceiling of any agent
  - APS  (Suite 2 Agentic)     - behavior-under-agency on the real tool loop
  - RAS  (Suite 3 MRA)         - the ablation-attributed composition delta index

The composition formula is NOT hard-coded here - it is read from the versioned
standard artifact (taxonomy.json "aqi"), so a published AQI is reproducible from a
documented rule and a taxonomy-version change is the only way the composition can
change. When a suite is not run, its sub-index is absent and the AQI renormalizes
over the sub-indices present (per the artifact's renormalize_over_present flag).

A suite sub-index is built from its per-realm scores via the CALIBRATED realm
weights (calibration.py), each realm score carrying a bootstrap CI (scoring.py).
A hard DQ flag caps the affected suite index at zero (dq.py) and so drags the AQI.

Economics is a FIRST-CLASS axis reported alongside - never folded into the AQI.
"""

from .calibration import weighted_score
from .dq import apply_dq
from .scoring import bootstrap_ci


def suite_index(realm_scores, realm_weights, *, flags=None, realm_cis=None):
    """Compose one suite sub-index (MPI/APS/RAS) from its per-realm scores using
    the calibrated realm weights, then apply the absolute DQ cap. Returns a dict
    {score, ci, dq_capped, realms}. The CI is the weight-widest realm CI as a
    conservative envelope (a suite is only as certain as its least-certain realm)."""
    score = weighted_score(realm_scores, realm_weights)
    capped = apply_dq(score, flags or [])
    ci = None
    if capped == 0.0 and score not in (0.0, None):
        ci = (0.0, 0.0)  # DQ is categorical, not uncertain
    elif realm_cis:
        present = [c for r, c in realm_cis.items()
                   if c is not None and realm_scores.get(r) is not None]
        if present:
            ci = (round(min(lo for lo, _ in present), 4),
                  round(max(hi for _, hi in present), 4))
    return {"score": capped, "ci": ci, "dq_capped": capped != score,
            "realms": dict(realm_scores)}


def compose_aqi(sub_indices, aqi_spec):
    """Compose the headline AQI from a map {"MPI": score, "APS": score,
    "RAS": score} using the artifact's documented weights. Sub-indices that are
    None (suite not run) are dropped and the weights renormalize over the rest
    when renormalize_over_present is set. Returns {aqi, components, weights_used}."""
    weights = aqi_spec.get("weights", {})
    renorm = aqi_spec.get("renormalize_over_present", True)
    present = {k: v for k, v in sub_indices.items() if v is not None}
    if not present:
        return {"aqi": None, "components": dict(sub_indices), "weights_used": {}}
    used = {k: weights.get(k, 0.0) for k in present}
    total_w = sum(used.values())
    if total_w <= 0:
        return {"aqi": None, "components": dict(sub_indices), "weights_used": used}
    if not renorm:
        # Missing sub-indices count as absent weight against the full denominator.
        total_w = sum(weights.get(k, 0.0) for k in weights) or total_w
    aqi = sum(present[k] * used[k] for k in present) / total_w
    return {"aqi": round(aqi, 4), "components": dict(sub_indices),
            "weights_used": {k: round(w, 4) for k, w in used.items()}}


def aqi_ci(component_cis, aqi_spec, sub_indices):
    """Propagate CIs to the AQI by composing the lo bounds and the hi bounds under
    the same weighting. Conservative and reproducible."""
    weights = aqi_spec.get("weights", {})
    present = {k: c for k, c in component_cis.items()
               if c is not None and sub_indices.get(k) is not None}
    if not present:
        return None
    total_w = sum(weights.get(k, 0.0) for k in present) or 1.0
    lo = sum(present[k][0] * weights.get(k, 0.0) for k in present) / total_w
    hi = sum(present[k][1] * weights.get(k, 0.0) for k in present) / total_w
    return (round(lo, 4), round(hi, 4))


def build_aqi_report(*, taxonomy, suite_results, economics):
    """Assemble the full AQI record from per-suite results.

    suite_results: {"MPI": {score, ci, realms, ...}, "APS": {...}, "RAS": {...}}
                   (any subset; a missing suite is treated as not run).
    economics:     the first-class economics axis, reported verbatim, never folded.
    """
    sub = {k: (r.get("score") if r else None) for k, r in suite_results.items()}
    cis = {k: (r.get("ci") if r else None) for k, r in suite_results.items()}
    composed = compose_aqi(sub, taxonomy.aqi)
    composed["aqi_ci"] = aqi_ci(cis, taxonomy.aqi, sub)
    return {
        "standard_version": taxonomy.version,
        "aqi": composed["aqi"],
        "aqi_ci": composed["aqi_ci"],
        "weights_used": composed["weights_used"],
        "sub_indices": suite_results,
        "economics": economics,
        "composition": taxonomy.aqi.get("composition"),
    }


__all__ = ["suite_index", "compose_aqi", "aqi_ci", "build_aqi_report",
           "bootstrap_ci"]
