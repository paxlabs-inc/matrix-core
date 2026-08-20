"""AGON resumable, re-scorable run cache.

A model call is the expensive, non-deterministic part of a run; scoring is cheap
and deterministic. AGON content-addresses each unit's RAW transcript + result so:

  - a run is RESUMABLE: an interrupted gauntlet skips units already cached instead
    of re-invoking the model, and
  - a run is RE-SCORABLE: the scorer can be re-run over the cached raw transcript
    without touching the model, reproducing identical scores.

The cache key commits to everything that determines the model's output: the model
id, the served-model id, the seed, the rep, the standard/harness version, and a
content hash of the runtime-relevant scenario fields. Change any of them and the
key changes, so a stale transcript is never silently reused for a changed item.
"""

import hashlib
import json
import shutil
from pathlib import Path

# Only these scenario fields shape the model's input; hashing them (not AGON-only
# metadata like title/notes/difficulty) is what keeps the key stable across a
# metadata edit while invalidating on a real prompt/verify/file change.
RUNTIME_KEY_FIELDS = ("id", "kind", "system", "sheet", "verify", "files",
                      "protected", "inject", "max_steps", "temperature",
                      "max_tokens", "step_max_tokens")


def scenario_content_hash(scenario):
    payload = {k: scenario.get(k) for k in RUNTIME_KEY_FIELDS if k in scenario}
    blob = json.dumps(payload, sort_keys=True, ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def unit_key(model, served_model, scenario, rep, version, seed=None):
    """Content-address a (model, scenario, rep) unit. Deterministic and stable."""
    parts = {
        "model": model,
        "served_model": served_model,
        "scenario": scenario_content_hash(scenario),
        "rep": rep,
        "seed": seed,
        "version": version,
    }
    blob = json.dumps(parts, sort_keys=True, ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


class RunCache:
    """Filesystem-backed content-addressed store of raw run artifacts. Each key
    holds result.json and (for agentic runs) transcript.jsonl."""

    def __init__(self, root):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)

    def _dir(self, key):
        return self.root / key[:2] / key

    def has(self, key):
        return (self._dir(key) / "result.json").is_file()

    def store(self, key, result, transcript=None):
        d = self._dir(key)
        d.mkdir(parents=True, exist_ok=True)
        (d / "result.json").write_text(
            json.dumps(result, indent=2, ensure_ascii=False), encoding="utf-8")
        if transcript is not None:
            (d / "transcript.jsonl").write_text(
                "\n".join(json.dumps(e, ensure_ascii=False) for e in transcript),
                encoding="utf-8")
        return d

    def load(self, key):
        d = self._dir(key)
        result = json.loads((d / "result.json").read_text(encoding="utf-8"))
        tpath = d / "transcript.jsonl"
        transcript = None
        if tpath.is_file():
            transcript = [json.loads(line) for line in
                          tpath.read_text(encoding="utf-8").splitlines() if line.strip()]
        return {"result": result, "transcript": transcript, "dir": d}

    def dir_for(self, key):
        return self._dir(key)

    def clear(self):
        shutil.rmtree(self.root, ignore_errors=True)
        self.root.mkdir(parents=True, exist_ok=True)


def resumable_invoke(cache, key, invoke):
    """Return the cached (result, transcript, dir) for `key` if present; otherwise
    call `invoke() -> (result, transcript)`, store it, and return it. `invoke` (the
    model call) fires only on a cache miss - that is the resume guarantee."""
    if cache.has(key):
        return cache.load(key)
    result, transcript = invoke()
    d = cache.store(key, result, transcript)
    return {"result": result, "transcript": transcript, "dir": d}


def rescore_cached(cache, key, scorer):
    """Re-run `scorer(result, run_dir)` over a cached run without invoking the
    model. Deterministic: repeated calls reproduce identical scores."""
    if not cache.has(key):
        raise KeyError(f"no cached run for key {key}")
    loaded = cache.load(key)
    return scorer(loaded["result"], loaded["dir"])
