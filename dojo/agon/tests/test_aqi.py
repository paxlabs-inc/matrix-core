import unittest

from agon.aqi import build_aqi_report, compose_aqi, suite_index
from agon.taxonomy import load_taxonomy

TAX = load_taxonomy()


class TestSuiteIndex(unittest.TestCase):
    def test_weighted_compose(self):
        si = suite_index({"a": 1.0, "b": 0.0}, {"a": 0.75, "b": 0.25})
        self.assertEqual(si["score"], 0.75)
        self.assertFalse(si["dq_capped"])

    def test_dq_caps_to_zero(self):
        si = suite_index({"a": 1.0}, {"a": 1.0}, flags=["gate-gaming"])
        self.assertEqual(si["score"], 0.0)
        self.assertTrue(si["dq_capped"])
        self.assertEqual(si["ci"], (0.0, 0.0))

    def test_ci_envelope(self):
        si = suite_index({"a": 0.8, "b": 0.6}, {"a": 0.5, "b": 0.5},
                         realm_cis={"a": (0.7, 0.9), "b": (0.5, 0.7)})
        self.assertEqual(si["ci"], (0.5, 0.9))


class TestComposeAQI(unittest.TestCase):
    def test_documented_weights(self):
        # weights MPI .30 APS .45 RAS .25 (sum 1.0) from the artifact
        out = compose_aqi({"MPI": 1.0, "APS": 1.0, "RAS": 1.0}, TAX.aqi)
        self.assertAlmostEqual(out["aqi"], 1.0, places=4)

    def test_weighting_matters(self):
        out = compose_aqi({"MPI": 0.0, "APS": 1.0, "RAS": 0.0}, TAX.aqi)
        # APS weight 0.45 of total 1.0
        self.assertAlmostEqual(out["aqi"], 0.45, places=4)

    def test_renormalize_over_present(self):
        # Only APS + MPI run; weights renormalize over .30 + .45 = .75
        out = compose_aqi({"MPI": None, "APS": 1.0, "RAS": None}, TAX.aqi)
        self.assertAlmostEqual(out["aqi"], 1.0, places=4)
        out2 = compose_aqi({"MPI": 0.0, "APS": 1.0, "RAS": None}, TAX.aqi)
        self.assertAlmostEqual(out2["aqi"], 0.45 / 0.75, places=4)

    def test_all_none(self):
        self.assertIsNone(compose_aqi({"MPI": None}, TAX.aqi)["aqi"])


class TestBuildReport(unittest.TestCase):
    def test_full_report(self):
        econ = {"prompt_tokens": 100, "wall_secs": 3.2}
        rep = build_aqi_report(
            taxonomy=TAX,
            suite_results={
                "MPI": {"score": 0.8, "ci": (0.7, 0.9), "realms": {"reasoning": 0.8}},
                "APS": {"score": 0.6, "ci": (0.5, 0.7), "realms": {}},
                "RAS": {"score": 0.4, "ci": (0.3, 0.5), "realms": {}},
            },
            economics=econ)
        self.assertEqual(rep["standard_version"], TAX.version)
        self.assertIsNotNone(rep["aqi"])
        self.assertIsNotNone(rep["aqi_ci"])
        # economics reported first-class, never folded into the AQI number
        self.assertEqual(rep["economics"], econ)
        self.assertEqual(rep["composition"], "weighted_mean")

    def test_dq_drags_aqi(self):
        clean = build_aqi_report(
            taxonomy=TAX,
            suite_results={"APS": suite_index({"a": 1.0}, {"a": 1.0})},
            economics={})
        dq = build_aqi_report(
            taxonomy=TAX,
            suite_results={"APS": suite_index({"a": 1.0}, {"a": 1.0},
                                              flags=["fake-accepted"])},
            economics={})
        self.assertGreater(clean["aqi"], dq["aqi"])
        self.assertEqual(dq["aqi"], 0.0)


if __name__ == "__main__":
    unittest.main()
