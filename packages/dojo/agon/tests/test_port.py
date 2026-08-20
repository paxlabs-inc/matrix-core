import json
import unittest
from pathlib import Path

from agon.golden import assert_corpus_golden
from agon.registry import SCORERS, materialize, scorer_ids
from agon.schema import load_corpus
from agon.taxonomy import load_taxonomy
from scenarios import build_scenarios

TAX = load_taxonomy()
CORPUS_DIR = Path(__file__).resolve().parent.parent / "corpus"
RECORDED = (Path(__file__).resolve().parent.parent.parent
            / "runs" / "20260707-010056" / "deepseek_deepseek-v4-pro")

RUNTIME_COMPARE = ("kind", "system", "sheet", "verify", "files", "protected",
                   "inject", "max_steps", "temperature", "max_tokens")


class TestPortedCorpus(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scenarios = load_corpus(CORPUS_DIR, TAX, scorers=scorer_ids())
        cls.by_id = {s.id: s for s in cls.scenarios}
        cls.old = {s["id"]: s for s in build_scenarios(race_ok=True)}

    def test_ported_and_native_present(self):
        # The 10 behavior-preserving ports plus AGON-native adversarial items.
        self.assertTrue(set(self.old).issubset(set(self.by_id)))
        native = set(self.by_id) - set(self.old)
        self.assertEqual(native, {"w_goodhart_fizz"})
        self.assertEqual(len(self.scenarios), 11)

    def test_every_capability_in_taxonomy(self):
        for s in self.scenarios:
            for cap in s.capabilities:
                self.assertTrue(TAX.has_leaf(cap), cap)
                self.assertEqual(TAX.suite_of(cap), s.suite)

    def test_behavior_preserving_runtime_fields(self):
        for sid in self.old:
            mat = materialize(self.by_id[sid])
            old = self.old[sid]
            self.assertIs(mat["score"], old["score"], f"{sid} scorer identity")
            for f in RUNTIME_COMPARE:
                self.assertEqual(mat.get(f), old.get(f), f"{sid}.{f} differs from pre-port")

    def test_scorer_identical_on_recorded_real_runs(self):
        # For each ported scenario, the resolved scorer must produce the identical
        # score dict as the pre-port scorer on the SAME recorded real run.
        for sid, scen in self.by_id.items():
            run_dir = RECORDED / f"{sid}-r0"
            result_path = run_dir / "result.json"
            if not result_path.exists():
                continue
            result = json.loads(result_path.read_text(encoding="utf-8"))
            ported = SCORERS[scen["scorer"]](result, run_dir)
            old = self.old[sid]["score"](result, run_dir)
            self.assertEqual(ported, old, f"{sid} score differs on recorded run")

    def test_golden_fixtures_pass(self):
        # The whole ported corpus must pass the golden self-validation pass
        # (correct high / incorrect low, verify green-on-correct/red-on-incorrect).
        self.assertTrue(assert_corpus_golden(self.scenarios, SCORERS))


if __name__ == "__main__":
    unittest.main()
