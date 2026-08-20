import copy
import json
import unittest

from agon.taxonomy import (
    REQUIRED_SUITES,
    Taxonomy,
    TaxonomyError,
    load_taxonomy,
    validate_taxonomy,
)


class TestTaxonomyArtifact(unittest.TestCase):
    def setUp(self):
        self.tax = load_taxonomy()

    def test_loads_and_validates(self):
        self.assertTrue(self.tax.version)
        self.assertTrue(validate_taxonomy(self.tax))

    def test_exactly_three_suites(self):
        self.assertEqual(set(self.tax.suites.keys()), set(REQUIRED_SUITES))

    def test_suite_indices_are_mpi_aps_ras(self):
        indices = {s["index"] for s in self.tax.suites.values()}
        self.assertEqual(indices, {"MPI", "APS", "RAS"})

    def test_every_leaf_well_formed(self):
        self.assertTrue(self.tax.leaf_ids())
        for lid in self.tax.leaf_ids():
            leaf = self.tax.leaf(lid)
            self.assertTrue(str(leaf["definition"]).strip(), lid)
            self.assertTrue(str(leaf["pass_condition"]).strip(), lid)
            self.assertIsInstance(leaf["difficulty_tiers"], list)
            self.assertTrue(leaf["difficulty_tiers"], lid)
            self.assertGreaterEqual(self.tax.min_items(lid), 1, lid)

    def test_leaf_ids_are_suite_realm_leaf(self):
        for lid in self.tax.leaf_ids():
            self.assertEqual(len(lid.split(".")), 3, lid)
            self.assertIn(self.tax.suite_of(lid), REQUIRED_SUITES)


class TestTaxonomyValidation(unittest.TestCase):
    """The validator must reject real malformations - not just accept the good one."""

    def setUp(self):
        with open(load_taxonomy.__globals__["TAXONOMY_PATH"], encoding="utf-8") as f:
            self.base = json.load(f)

    def _reject(self, mutate):
        data = copy.deepcopy(self.base)
        mutate(data)
        with self.assertRaises(TaxonomyError):
            validate_taxonomy(Taxonomy(data))

    def test_rejects_missing_suite(self):
        self._reject(lambda d: d["suites"].pop("mra"))

    def test_rejects_unknown_suite(self):
        self._reject(lambda d: d["suites"].update({"bogus": d["suites"]["mra"]}))

    def test_rejects_leaf_missing_pass_condition(self):
        def mut(d):
            leaf = d["suites"]["raw_model"]["realms"]["reasoning"]["leaves"]["multi_step_arithmetic"]
            leaf.pop("pass_condition")
        self._reject(mut)

    def test_rejects_bad_difficulty_tier(self):
        def mut(d):
            leaf = d["suites"]["agentic"]["realms"]["competence"]["leaves"]["code_fix_with_tests"]
            leaf["difficulty_tiers"] = ["trivial"]
        self._reject(mut)

    def test_rejects_nonpositive_min_items(self):
        def mut(d):
            leaf = d["suites"]["agentic"]["realms"]["competence"]["leaves"]["code_fix_with_tests"]
            leaf["min_items"] = 0
        self._reject(mut)

    def test_rejects_realm_with_no_leaves(self):
        def mut(d):
            d["suites"]["mra"]["realms"]["emergent_integrity"]["leaves"] = {}
        self._reject(mut)


if __name__ == "__main__":
    unittest.main()
