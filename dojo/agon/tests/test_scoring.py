import unittest

from agon.scoring import (
    aggregate_flake,
    bootstrap_ci,
    flake_rate,
    is_api_error,
    item_stats,
    rep_scalar,
)


def rep(dims, ended_by="turn_in", flags=None):
    return {"result": {"ended_by": ended_by},
            "score": {"dims": dims, "flags": flags or []}}


class TestRepScalar(unittest.TestCase):
    def test_mean_of_dims(self):
        self.assertAlmostEqual(rep_scalar({"dims": {"a": 1.0, "b": 0.0}}), 0.5)

    def test_empty_dims_is_zero(self):
        self.assertEqual(rep_scalar({"dims": {}}), 0.0)
        self.assertEqual(rep_scalar({}), 0.0)


class TestBootstrapCI(unittest.TestCase):
    def test_reproducible(self):
        vals = [0.2, 0.4, 0.6, 0.8, 1.0]
        self.assertEqual(bootstrap_ci(vals, seed=7), bootstrap_ci(vals, seed=7))

    def test_single_value_collapses(self):
        self.assertEqual(bootstrap_ci([0.7]), (0.7, 0.7))

    def test_interval_brackets_mean(self):
        vals = [0.3, 0.5, 0.7]
        lo, hi = bootstrap_ci(vals, seed=1)
        self.assertLessEqual(lo, 0.5)
        self.assertGreaterEqual(hi, 0.5)

    def test_empty_is_none(self):
        self.assertIsNone(bootstrap_ci([]))


class TestFlakeRate(unittest.TestCase):
    def test_stable_zero(self):
        self.assertEqual(flake_rate([0.9, 0.8, 1.0]), 0.0)

    def test_flip_half(self):
        self.assertEqual(flake_rate([1.0, 0.0]), 0.5)

    def test_minority(self):
        self.assertEqual(flake_rate([1.0, 1.0, 1.0, 0.0]), 0.25)

    def test_undefined_single(self):
        self.assertIsNone(flake_rate([1.0]))


class TestItemStats(unittest.TestCase):
    def test_api_errors_excluded(self):
        reps = [rep({"competence": 1.0}),
                rep({}, ended_by="api_error"),
                rep({"competence": 1.0})]
        st = item_stats(reps)
        self.assertEqual(st["reps"], 3)
        self.assertEqual(st["scored_reps"], 2)
        self.assertEqual(st["api_errors"], 1)
        self.assertEqual(st["mean"], 1.0)  # error rep did NOT drag the mean down

    def test_all_errors_no_score(self):
        reps = [rep({}, ended_by="api_error")]
        st = item_stats(reps)
        self.assertIsNone(st["mean"])
        self.assertIsNone(st["ci"])
        self.assertEqual(st["api_errors"], 1)

    def test_every_score_carries_ci(self):
        reps = [rep({"competence": 0.8}), rep({"competence": 0.6})]
        st = item_stats(reps)
        self.assertIsNotNone(st["ci"])
        self.assertEqual(len(st["ci"]), 2)

    def test_flags_union(self):
        reps = [rep({"competence": 0.0}, flags=["gate-gaming"]),
                rep({"competence": 1.0})]
        self.assertIn("gate-gaming", item_stats(reps)["flags"])

    def test_is_api_error(self):
        self.assertTrue(is_api_error({"ended_by": "api_error"}))
        self.assertFalse(is_api_error({"ended_by": "turn_in"}))


class TestAggregateFlake(unittest.TestCase):
    def test_rates(self):
        stats = [
            item_stats([rep({"competence": 1.0}), rep({}, ended_by="api_error")]),
            item_stats([rep({"competence": 1.0}), rep({"competence": 0.0})]),
        ]
        agg = aggregate_flake(stats)
        self.assertEqual(agg["total_reps"], 4)
        self.assertEqual(agg["api_errors"], 1)
        self.assertEqual(agg["api_error_rate"], 0.25)
        self.assertEqual(agg["items_with_flake"], 1)


if __name__ == "__main__":
    unittest.main()
