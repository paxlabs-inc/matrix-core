"""AGON Suite 1 - Raw Model Performance (MPI).

Suite 1 isolates the bare model: no tools, no workspace, a single prompt -> answer.
Because there is no agency to observe, scoring is a DETERMINISTIC ORACLE grade of
the model's answer against a known-correct value - the strongest kind of ground
truth. Each item declares an `oracle` and an `oracle_mode`:

  - numeric       : the final number equals the oracle (int/float tolerant)
  - exact         : normalized string equals the oracle
  - contains_all  : the answer contains every required substring (order-free)
  - json_keys     : the answer parses as JSON and matches the oracle's key/values
  - abstains      : the answer admits uncertainty / refuses to fabricate (the
                    anti-hallucination + calibration-honesty polarity)

An item is graded to a single competence dim in {0.0, 1.0}. The MPI is the
calibrated (or equal-weight) mean of the per-realm scores, each realm carrying a
bootstrap CI over its items x reps. This module owns Suite 1's own grader and
loader; it does not route through the legacy (result, run_dir) harness scorers.
"""

import json
import re
from pathlib import Path

from .aqi import suite_index
from .scoring import bootstrap_ci
from .schema import load_corpus
from .taxonomy import load_taxonomy

SUITE1_CORPUS = Path(__file__).parent / "corpus_suite1"

_UNCERTAIN = ("i don't know", "i do not know", "not sure", "cannot determine",
              "can't determine", "insufficient information", "no reliable",
              "unable to verify", "i'm not certain", "not certain", "unknown",
              "cannot answer", "don't have")


def _norm(s):
    return re.sub(r"\s+", " ", str(s).strip().lower())


def _final_number(text):
    """The last numeric token in the answer (models put the final answer last)."""
    nums = re.findall(r"-?\d+(?:,\d{3})*(?:\.\d+)?", str(text))
    if not nums:
        return None
    return float(nums[-1].replace(",", ""))


def _extract_json(text):
    s = str(text)
    m = re.search(r"```(?:json)?\s*(\{.*?\}|\[.*?\])\s*```", s, re.DOTALL)
    if m:
        s = m.group(1)
    start = s.find("{")
    if start < 0:
        return None
    try:
        obj, _ = json.JSONDecoder().raw_decode(s[start:])
        return obj
    except json.JSONDecodeError:
        return None


def grade(answer, oracle, mode):
    """Deterministic pass/fail of one answer against its oracle. Pure function."""
    if mode == "numeric":
        got = _final_number(answer)
        if got is None:
            return False
        want = float(oracle)
        return abs(got - want) <= max(1e-6, abs(want) * 1e-6)
    if mode == "exact":
        return _norm(answer) == _norm(oracle)
    if mode == "contains_all":
        hay = _norm(answer)
        return all(_norm(tok) in hay for tok in oracle)
    if mode == "json_keys":
        obj = _extract_json(answer)
        if not isinstance(obj, dict):
            return False
        for k, v in oracle.items():
            if k not in obj or obj[k] != v:
                return False
        return True
    if mode == "abstains":
        low = _norm(answer)
        return any(marker in low for marker in _UNCERTAIN)
    raise ValueError(f"unknown oracle_mode: {mode}")


def score_answer(scenario, answer):
    """Grade one raw-model answer to a Suite 1 score dict."""
    ok = grade(answer, scenario["oracle"], scenario["oracle_mode"])
    return {"dims": {"competence": 1.0 if ok else 0.0}, "flags": []}


def self_validate(scenario):
    """Prove a Suite 1 item's oracle is polarity-correct against its golden
    fixtures: the correct answer grades 1.0, the incorrect answer grades 0.0.
    Returns a list of error strings (empty = ok)."""
    errs = []
    g = scenario.get("golden") or {}
    for side, want in (("correct", 1.0), ("incorrect", 0.0)):
        fx = g.get(side)
        if not fx or "answer" not in fx:
            errs.append(f"{scenario['id']}: golden.{side} missing an 'answer'")
            continue
        got = score_answer(scenario, fx["answer"])["dims"]["competence"]
        if got != want:
            errs.append(f"{scenario['id']}: golden.{side} graded {got}, expected {want}")
    return errs


def realm_of(leaf_id):
    parts = leaf_id.split(".")
    return ".".join(parts[:2])


def load_suite1(source=SUITE1_CORPUS, taxonomy=None):
    """Load + validate the Suite 1 corpus. Every item must be suite raw_model with
    an oracle_mode and golden answers."""
    taxonomy = taxonomy or load_taxonomy()
    scenarios = load_corpus(source, taxonomy)
    for s in scenarios:
        if s["suite"] != "raw_model":
            raise ValueError(f"{s['id']}: Suite 1 loader given a non-raw_model item")
        if "oracle_mode" not in s:
            raise ValueError(f"{s['id']}: Suite 1 item missing oracle_mode")
    return scenarios


def build_mpi(scored_items, *, realm_weights=None, ci_seed=0):
    """Compose the MPI from graded Suite 1 items.

    scored_items: list of {"scenario": scenario, "scalars": [per-rep competence]}.
    Groups by realm, computes a per-realm mean + bootstrap CI, then the suite
    index over realms (calibrated weights if given, else equal weights). Returns
    {index: {score,ci,realms,...}, realm_scores, realm_cis}."""
    by_realm = {}
    for it in scored_items:
        realm = realm_of(it["scenario"]["capabilities"][0])
        by_realm.setdefault(realm, []).extend(it["scalars"])
    realm_scores, realm_cis = {}, {}
    for realm, vals in by_realm.items():
        realm_scores[realm] = round(sum(vals) / len(vals), 4) if vals else None
        realm_cis[realm] = bootstrap_ci(vals, seed=ci_seed)
    if realm_weights is None:
        realm_weights = {r: 1.0 for r in realm_scores}
    idx = suite_index(realm_scores, realm_weights, realm_cis=realm_cis)
    return {"index": idx, "realm_scores": realm_scores, "realm_cis": realm_cis}
