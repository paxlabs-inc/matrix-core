import unittest

from agon.schema import coverage_report
from agon.suite1 import (
    build_mpi,
    grade,
    load_suite1,
    realm_of,
    score_answer,
    self_validate,
)
from agon.taxonomy import load_taxonomy

TAX = load_taxonomy()


class TestGrade(unittest.TestCase):
    def test_numeric_tolerant(self):
        self.assertTrue(grade("the answer is 32", 32, "numeric"))
        self.assertTrue(grade("= 72 km/h", 72, "numeric"))
        self.assertFalse(grade("42", 32, "numeric"))

    def test_exact_normalizes_case_ws(self):
        self.assertTrue(grade("Yes", "yes", "exact"))
        self.assertFalse(grade("ANSWER = 42", "ANSWER=42", "exact"))

    def test_contains_all(self):
        self.assertTrue(grade("The capital is Tokyo.", ["tokyo"], "contains_all"))
        self.assertFalse(grade("Kyoto", ["tokyo"], "contains_all"))

    def test_json_keys(self):
        self.assertTrue(grade('{"name":"agon","version":1}',
                              {"name": "agon", "version": 1}, "json_keys"))
        self.assertFalse(grade('{"name":"agon"}',
                               {"name": "agon", "version": 1}, "json_keys"))

    def test_abstains(self):
        self.assertTrue(grade("I don't know of any such film.", "abstain", "abstains"))
        self.assertFalse(grade("It is a WWII drama.", "abstain", "abstains"))


class TestCorpus(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.items = load_suite1()

    def test_all_items_self_validate(self):
        # Every oracle is polarity-correct: correct answer high, incorrect low.
        for s in self.items:
            self.assertEqual(self_validate(s), [], s["id"])

    def test_covered_leaves_meet_min_items(self):
        counts = coverage_report(self.items, TAX)
        covered = {lid for lid in counts if counts[lid] > 0}
        self.assertTrue(covered)
        for lid in covered:
            self.assertGreaterEqual(counts[lid], TAX.min_items(lid),
                                    f"{lid}: {counts[lid]} < min {TAX.min_items(lid)}")

    def test_all_raw_model_suite(self):
        for s in self.items:
            self.assertEqual(s["suite"], "raw_model")


class TestMPI(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.items = load_suite1()

    def _scored(self, answer_side):
        # Grade every item against its golden fixture of the given polarity.
        out = []
        for s in self.items:
            ans = s["golden"][answer_side]["answer"]
            scalar = score_answer(s, ans)["dims"]["competence"]
            out.append({"scenario": s, "scalars": [scalar]})
        return out

    def test_mpi_perfect_when_all_correct(self):
        mpi = build_mpi(self._scored("correct"))
        self.assertEqual(mpi["index"]["score"], 1.0)
        self.assertIsNotNone(mpi["index"]["ci"])

    def test_mpi_zero_when_all_incorrect(self):
        mpi = build_mpi(self._scored("incorrect"))
        self.assertEqual(mpi["index"]["score"], 0.0)

    def test_per_realm_profile_with_cis(self):
        mpi = build_mpi(self._scored("correct"))
        self.assertIn("raw_model.reasoning", mpi["realm_scores"])
        for realm, ci in mpi["realm_cis"].items():
            self.assertIsNotNone(ci, realm)

    def test_realm_of(self):
        self.assertEqual(realm_of("raw_model.reasoning.deductive_logic"),
                         "raw_model.reasoning")


if __name__ == "__main__":
    unittest.main()
