"""AGON provenance: signed, reproducible result cards.

A qualification claim is only citable if it is reproducible and tamper-evident.
Every gauntlet run emits a RESULT CARD that stamps everything needed to reproduce
and audit it:

  - the standard + harness versions (what ruleset produced this)
  - per-model: model id, served-model id
  - the corpus content hash (which exact items, by their runtime content)
  - the temperature policy and per-item seeds (the generation determinants)
  - per-item: the content-address key into the raw-transcript cache, the score
    dims, and the confidence interval
  - the aggregate scores

The card body is then INTEGRITY-STAMPED with an HMAC over its canonical (sorted,
compact) JSON. Any later mutation of the card invalidates the stamp. The signing
key comes from AGON_SIGNING_KEY; when unset a documented public default is used so
offline reproduction still verifies (the default provides tamper-evidence, not
secrecy - set a real key for attributable signing).

A deterministic-eligible item (temperature 0, a scorer that is a pure function of
the recorded transcript) carries a reproduction fingerprint that is a pure
function of (standard version, corpus hash, model, served model, seed, item
content). Same inputs -> same fingerprint, which is the reproducibility contract.
"""

import hashlib
import hmac
import json
import os
import time

from .cache import scenario_content_hash, unit_key

INTEGRITY_FIELD = "integrity"
INTEGRITY_ALGO = "hmac-sha256"

# Public, non-secret default key: gives tamper-evidence for offline reproduction.
# Set AGON_SIGNING_KEY for attributable signing.
DEFAULT_SIGNING_KEY = b"agon-public-provenance-key-v1"


def signing_key():
    env = os.environ.get("AGON_SIGNING_KEY")
    return env.encode("utf-8") if env else DEFAULT_SIGNING_KEY


def corpus_content_hash(scenarios):
    """Hash committing to the exact runtime content of every scenario in the
    corpus, order-independent (keyed by id). Change any item's prompt/verify/files
    and the corpus hash changes."""
    per_item = sorted((s["id"], scenario_content_hash(s)) for s in scenarios)
    blob = json.dumps(per_item, sort_keys=True, ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def item_fingerprint(*, standard_version, corpus_hash, model, served_model, seed,
                     scenario):
    """Reproduction fingerprint of a single deterministic-eligible item. A pure
    function of the generation determinants; identical inputs reproduce it."""
    parts = {
        "standard_version": standard_version,
        "corpus_hash": corpus_hash,
        "model": model,
        "served_model": served_model,
        "seed": seed,
        "scenario": scenario_content_hash(scenario),
    }
    blob = json.dumps(parts, sort_keys=True, ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def _deterministic_eligible(scenario):
    """An item is deterministic-eligible when generation is pinned (temperature 0)
    AND scoring is a pure function of the recorded transcript (a plain item, no
    live sandbox verify influencing the score)."""
    return scenario.get("temperature", None) == 0 and scenario.get("kind") == "plain"


def build_item_record(*, scenario, model, served_model, seed, score, ci=None,
                      standard_version, harness_version, corpus_hash, cache_version):
    deterministic = _deterministic_eligible(scenario)
    rec = {
        "scenario": scenario["id"],
        "model": model,
        "served_model": served_model,
        "seed": seed,
        "kind": scenario.get("kind"),
        "temperature": scenario.get("temperature"),
        "deterministic": deterministic,
        "transcript_ref": unit_key(model, served_model, scenario, seed,
                                    cache_version, seed=seed),
        "score": score,
        "ci": ci,
    }
    if deterministic:
        rec["fingerprint"] = item_fingerprint(
            standard_version=standard_version, corpus_hash=corpus_hash, model=model,
            served_model=served_model, seed=seed, scenario=scenario)
    return rec


def build_card(*, standard_version, harness_version, scenarios, temperature_policy,
               items, aggregates, cache_version=None, generated_at=None):
    """Assemble an unsigned result card. `items` is a list of build_item_record
    outputs; `aggregates` is the per-model aggregate scores."""
    corpus_hash = corpus_content_hash(scenarios)
    return {
        "standard_version": standard_version,
        "harness_version": harness_version,
        "cache_version": cache_version or f"{standard_version}",
        "generated_at": generated_at or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "corpus_hash": corpus_hash,
        "corpus_item_count": len(scenarios),
        "temperature_policy": temperature_policy,
        "items": items,
        "aggregates": aggregates,
    }


def canonical_bytes(card):
    """Canonical serialization of the card BODY (integrity field excluded)."""
    body = {k: v for k, v in card.items() if k != INTEGRITY_FIELD}
    return json.dumps(body, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False).encode("utf-8")


def body_digest(card, key=None):
    key = key if key is not None else signing_key()
    return hmac.new(key, canonical_bytes(card), hashlib.sha256).hexdigest()


def sign_card(card, key=None):
    """Attach the integrity stamp over the card body. Returns the same card."""
    card[INTEGRITY_FIELD] = {"algo": INTEGRITY_ALGO, "digest": body_digest(card, key)}
    return card


def verify_card(card, key=None):
    """True iff the card carries a valid integrity stamp for its body under `key`.
    Any mutation of the body (or a wrong key) fails verification."""
    stamp = card.get(INTEGRITY_FIELD)
    if not isinstance(stamp, dict) or stamp.get("algo") != INTEGRITY_ALGO:
        return False
    expected = body_digest(card, key)
    return hmac.compare_digest(str(stamp.get("digest", "")), expected)


def reproduces(card_a, card_b):
    """True iff two cards describe the SAME deterministic-eligible items: identical
    fingerprints per (scenario, model, seed). Non-deterministic items are ignored -
    they carry no reproduction contract."""
    def det_map(card):
        return {(i["scenario"], i["model"], i["seed"]): i["fingerprint"]
                for i in card.get("items", []) if i.get("deterministic")}
    a, b = det_map(card_a), det_map(card_b)
    return a == b and len(a) > 0
