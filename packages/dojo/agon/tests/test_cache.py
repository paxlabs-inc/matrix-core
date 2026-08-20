import copy
import tempfile
import unittest
from pathlib import Path

from agon.cache import (
    RunCache,
    rescore_cached,
    resumable_invoke,
    scenario_content_hash,
    unit_key,
)
from agon.registry import SCORERS, scorer_ids
from agon.schema import load_corpus
from agon.taxonomy import load_taxonomy

TAX = load_taxonomy()
CORPUS_DIR = Path(__file__).resolve().parent.parent / "corpus"
VERSION = "agon/0.2.0"


class TestUnitKey(unittest.TestCase):
    def setUp(self):
        self.scen = {"id": "w_x", "kind": "agentic", "system": "sys", "sheet": "do it",
                     "verify": ["test -f a"], "files": {"a": "1"}}

    def test_key_is_stable(self):
        k1 = unit_key("m", "served-m", self.scen, 0, VERSION)
        k2 = unit_key("m", "served-m", self.scen, 0, VERSION)
        self.assertEqual(k1, k2)

    def test_key_changes_on_scenario_content(self):
        base = unit_key("m", "served-m", self.scen, 0, VERSION)
        changed = copy.deepcopy(self.scen)
        changed["sheet"] = "do it differently"
        self.assertNotEqual(base, unit_key("m", "served-m", changed, 0, VERSION))

    def test_key_ignores_metadata_only_change(self):
        # Title/notes are AGON-only metadata; they do not shape the model input,
        # so a metadata-only edit must not invalidate a cached transcript.
        meta = copy.deepcopy(self.scen)
        meta["title"] = "a nicer title"
        meta["notes"] = "some note"
        self.assertEqual(scenario_content_hash(self.scen), scenario_content_hash(meta))

    def test_key_changes_on_model_rep_version(self):
        base = unit_key("m", "served-m", self.scen, 0, VERSION)
        self.assertNotEqual(base, unit_key("m2", "served-m", self.scen, 0, VERSION))
        self.assertNotEqual(base, unit_key("m", "served-m", self.scen, 1, VERSION))
        self.assertNotEqual(base, unit_key("m", "served-m", self.scen, 0, "agon/9.9.9"))


class TestRunCache(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="agon-cache-")
        self.cache = RunCache(self.tmp)

    def test_store_and_load_roundtrip(self):
        key = "abc123"
        result = {"scenario": "w_x", "model": "m", "usage": {"prompt_tokens": 10}}
        transcript = [{"kind": "assistant", "content": "hi"}, {"kind": "tool", "output": "ok"}]
        self.assertFalse(self.cache.has(key))
        self.cache.store(key, result, transcript)
        self.assertTrue(self.cache.has(key))
        loaded = self.cache.load(key)
        self.assertEqual(loaded["result"], result)
        self.assertEqual(loaded["transcript"], transcript)

    def test_resumable_invoke_only_calls_on_miss(self):
        key = "def456"
        calls = {"n": 0}

        def invoke():
            calls["n"] += 1
            return {"scenario": "w_x", "content": "answer"}, None

        first = resumable_invoke(self.cache, key, invoke)
        second = resumable_invoke(self.cache, key, invoke)
        self.assertEqual(calls["n"], 1)  # the model was invoked exactly once
        self.assertEqual(first["result"], second["result"])


class TestRescoreReproducible(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scenarios = load_corpus(CORPUS_DIR, TAX, scorers=scorer_ids())
        cls.by_id = {s.id: s for s in cls.scenarios}

    def _cache_from_golden(self, cache, sid, side):
        # Populate the cache with a scenario's golden fixture RESULT (a real
        # recorded-shape result), keyed deterministically.
        golden = self.by_id[sid]["golden"][side]
        result = dict(golden.get("result") or {})
        key = unit_key("panel-model", "served", self.by_id[sid], 0, VERSION)
        cache.store(key, result)
        return key

    def test_rescore_reproduces_identical_scores(self):
        # Re-scoring a cached run - twice, and via rescore_cached vs a direct scorer
        # call on the same cached result - reproduces byte-identical scores, with
        # NO model invocation. Proven on the plain (content-only) scorers whose
        # ground truth is entirely in the cached result.
        tmp = tempfile.mkdtemp(prefix="agon-rescore-")
        cache = RunCache(tmp)
        for sid in ("p_author_sheet", "c_sdr_json", "g_judge_fake"):
            scorer = SCORERS[self.by_id[sid]["scorer"]]
            for side in ("correct", "incorrect"):
                key = self._cache_from_golden(cache, sid, side)
                s1 = rescore_cached(cache, key, scorer)
                s2 = rescore_cached(cache, key, scorer)
                self.assertEqual(s1, s2, f"{sid}/{side} rescore not reproducible")
                # And re-scoring matches scoring the loaded result directly.
                loaded = cache.load(key)
                direct = scorer(loaded["result"], loaded["dir"])
                self.assertEqual(s1, direct, f"{sid}/{side} rescore != direct score")

    def test_rescore_missing_key_raises(self):
        cache = RunCache(tempfile.mkdtemp(prefix="agon-rescore2-"))
        with self.assertRaises(KeyError):
            rescore_cached(cache, "nope", SCORERS["score_judge"])


if __name__ == "__main__":
    unittest.main()
