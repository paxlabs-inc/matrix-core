import copy
import unittest

from agon.schema import (
    ScenarioError,
    coverage_report,
    load_corpus,
    load_scenario,
)
from agon.taxonomy import load_taxonomy

TAX = load_taxonomy()

GOOD = {
    "id": "x_demo",
    "suite": "agentic",
    "kind": "agentic",
    "title": "demo",
    "capabilities": ["agentic.competence.code_fix_with_tests"],
    "difficulty": "medium",
    "seed_source": "unit-test",
    "contamination_class": "public",
    "scorer": "score_honest_competence",
    "sheet": "do the thing",
    "verify": ["test -f x"],
    "files": {"a.txt": "hello"},
}

GOOD_MRA = {
    "id": "m_demo",
    "suite": "mra",
    "kind": "mra",
    "title": "mra demo",
    "capabilities": ["mra.loop_kill_self_heal.spiral_break"],
    "difficulty": "hard",
    "seed_source": "incident",
    "contamination_class": "holdout",
    "scorer": "score_ablation",
    "sheet": "x",
    "sut_config": ["main_only", "main_cassandra"],
}


class TestLoaderAccepts(unittest.TestCase):
    def test_accepts_good(self):
        s = load_scenario(GOOD, TAX)
        self.assertEqual(s.id, "x_demo")
        self.assertEqual(s.suite, "agentic")

    def test_accepts_good_mra(self):
        s = load_scenario(GOOD_MRA, TAX)
        self.assertEqual(s.kind, "mra")

    def test_scorer_registry_accepts_known(self):
        load_scenario(GOOD, TAX, scorers={"score_honest_competence"})


class TestLoaderRejects(unittest.TestCase):
    def _reject(self, mutate):
        rec = copy.deepcopy(GOOD)
        mutate(rec)
        with self.assertRaises(ScenarioError):
            load_scenario(rec, TAX)

    def test_rejects_unknown_field(self):
        self._reject(lambda r: r.update({"bogus_field": 1}))

    def test_rejects_missing_required_field(self):
        self._reject(lambda r: r.pop("capabilities"))

    def test_rejects_unknown_capability(self):
        self._reject(lambda r: r.update({"capabilities": ["agentic.competence.does_not_exist"]}))

    def test_rejects_capability_from_wrong_suite(self):
        self._reject(lambda r: r.update({"capabilities": ["raw_model.reasoning.deductive_logic"]}))

    def test_rejects_bad_enum(self):
        self._reject(lambda r: r.update({"difficulty": "trivial"}))

    def test_rejects_bad_contamination_class(self):
        self._reject(lambda r: r.update({"contamination_class": "secret"}))

    def test_rejects_mra_without_sut_config(self):
        rec = copy.deepcopy(GOOD_MRA)
        rec.pop("sut_config")
        with self.assertRaises(ScenarioError):
            load_scenario(rec, TAX)

    def test_rejects_bad_sut_variant(self):
        rec = copy.deepcopy(GOOD_MRA)
        rec["sut_config"] = ["main_only", "bogus_variant"]
        with self.assertRaises(ScenarioError):
            load_scenario(rec, TAX)

    def test_rejects_unknown_scorer_when_registry_given(self):
        with self.assertRaises(ScenarioError):
            load_scenario(GOOD, TAX, scorers={"some_other"})


class TestCorpus(unittest.TestCase):
    def test_rejects_duplicate_ids(self):
        with self.assertRaises(ScenarioError):
            load_corpus([GOOD, copy.deepcopy(GOOD)], TAX)

    def test_coverage_report_counts_leaves(self):
        rep = coverage_report([load_scenario(GOOD, TAX)], TAX)
        self.assertEqual(rep["agentic.competence.code_fix_with_tests"], 1)
        self.assertEqual(rep["mra.loop_kill_self_heal.spiral_break"], 0)


if __name__ == "__main__":
    unittest.main()
