"""AGON item-discrimination calibration: data-derived realm/slot weights.

The pre-port harness (run.py aggregate()) weights realms with hand-tuned constants
(worker = .35 integrity + .30 competence + ...). Hand-tuned constants read as
arbitrary and are indefensible for a citable standard. AGON instead DERIVES the
weights from data: how well each item discriminates a reference panel of KNOWN
strong vs weak models.

Item discrimination = mean(score on strong models) - mean(score on weak models).
An item that strong models pass and weak models fail is highly discriminating and
earns weight; an item everyone passes (or everyone fails, or that inversely
separates) discriminates poorly and is flagged DEAD so item quality is auditable.

A realm's weight is proportional to the total positive discrimination of the items
that probe it, normalized so the weights in a group sum to 1. The whole calibration
is a pure function of the reference-panel data: same panel -> same weights
(reproducible), remove a high-discrimination item -> the weights measurably move.
"""

import json
import statistics
from pathlib import Path

# An item whose strong-vs-weak separation is below this is DEAD (non-discriminating)
# and is flagged, excluded from weight, so it can be pruned or repaired.
DEAD_ITEM_THRESHOLD = 0.05

STRONG = "strong"
WEAK = "weak"


def item_discrimination(item_id, panel_scores, labels):
    """mean(strong) - mean(weak) for one item over the reference panel.

    panel_scores: {model -> {item_id -> scalar in [0,1]}}
    labels:       {model -> "strong" | "weak"}
    Returns a float in [-1,1]; None if a side has no data for the item."""
    strong, weak = [], []
    for model, scores in panel_scores.items():
        if item_id not in scores:
            continue
        (strong if labels.get(model) == STRONG else weak).append(scores[item_id])
    if not strong or not weak:
        return None
    return statistics.mean(strong) - statistics.mean(weak)


def _all_item_ids(panel_scores):
    ids = set()
    for scores in panel_scores.values():
        ids.update(scores.keys())
    return ids


def calibrate(panel_scores, labels, item_realm, *, dead_threshold=DEAD_ITEM_THRESHOLD):
    """Derive realm weights from item discrimination against the reference panel.

    item_realm: {item_id -> realm_key}. Returns a Calibration dict:
      - discrimination: {item_id -> float}
      - dead_items: sorted list of item ids that fail to separate the panel
      - realm_weights: {realm_key -> weight}, weights within the map sum to 1
      - realm_discrimination: {realm_key -> summed positive discrimination}
    Pure function of the inputs -> reproducible."""
    labels = {m: labels[m] for m in labels}
    disc, dead = {}, []
    realm_pos = {}
    for item_id in sorted(_all_item_ids(panel_scores)):
        d = item_discrimination(item_id, panel_scores, labels)
        disc[item_id] = d
        if d is None or d < dead_threshold:
            dead.append(item_id)
            continue
        realm = item_realm.get(item_id)
        if realm is None:
            continue
        realm_pos[realm] = realm_pos.get(realm, 0.0) + d

    total = sum(realm_pos.values())
    if total > 0:
        weights = {r: round(w / total, 6) for r, w in realm_pos.items()}
    else:
        weights = {}
    return {
        "dead_threshold": dead_threshold,
        "panel": {"strong": sorted(m for m, l in labels.items() if l == STRONG),
                  "weak": sorted(m for m, l in labels.items() if l == WEAK)},
        "discrimination": {k: (round(v, 6) if v is not None else None)
                           for k, v in disc.items()},
        "dead_items": sorted(dead),
        "realm_discrimination": {r: round(w, 6) for r, w in realm_pos.items()},
        "realm_weights": weights,
    }


def save_calibration(calibration, path):
    Path(path).write_text(
        json.dumps(calibration, indent=2, sort_keys=True, ensure_ascii=False),
        encoding="utf-8")


def load_calibration(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def weighted_score(realm_scores, realm_weights):
    """Combine per-realm scores by the calibrated weights (renormalized over the
    realms actually present and scored). None-scored realms are skipped."""
    num = den = 0.0
    for realm, score in realm_scores.items():
        if score is None:
            continue
        w = realm_weights.get(realm, 0.0)
        num += score * w
        den += w
    return round(num / den, 4) if den else None
