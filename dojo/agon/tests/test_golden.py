import unittest

from agon.golden import (
    GoldenError,
    assert_corpus_golden,
    validate_corpus_golden,
)
from agon.schema import load_scenario
from agon.taxonomy import load_taxonomy

TAX = load_taxonomy()

# A synthetic agentic scenario with a REAL verify command and golden fixtures.
# correct fixture ships marker.txt (verify green); incorrect omits it (verify red).
SCEN = load_scenario(
    {
        "id": "t_golden",
        "suite": "agentic",
        "kind": "agentic",
        "title": "golden self-test",
        "capabilities": ["agentic.competence.artifact_construction"],
        "difficulty": "easy",
        "seed_source": "unit-test",
        "contamination_class": "public",
        "scorer": "score_marker",
        "sheet": "create marker.txt",
        "verify": ["test -f marker.txt"],
        "golden": {
            "correct": {"workspace": {"marker.txt": "ok"}, "result": {}},
            "incorrect": {"workspace": {}, "result": {}},
        },
    },
    TAX,
)


def good_scorer(result, run_dir):
    return {"dims": {"competence": 1.0 if result.get("post_verify_green") else 0.0}, "flags": []}


def broken_high_scorer(result, run_dir):
    # Inflates the metric: always scores high, even the incorrect fixture.
    return {"dims": {"competence": 1.0}, "flags": []}


def broken_low_scorer(result, run_dir):
    # Deflates the metric: always scores low, even the correct fixture.
    return {"dims": {"competence": 0.0}, "flags": []}


class TestGoldenPass(unittest.TestCase):
    def test_good_scorer_passes(self):
        failures = validate_corpus_golden([SCEN], {"score_marker": good_scorer})
        self.assertEqual(failures, {})
        self.assertTrue(assert_corpus_golden([SCEN], {"score_marker": good_scorer}))

    def test_broken_high_scorer_fails(self):
        failures = validate_corpus_golden([SCEN], {"score_marker": broken_high_scorer})
        self.assertIn("t_golden", failures)
        self.assertTrue(any("HIGH" in e for e in failures["t_golden"]))
        with self.assertRaises(GoldenError):
            assert_corpus_golden([SCEN], {"score_marker": broken_high_scorer})

    def test_broken_low_scorer_fails(self):
        failures = validate_corpus_golden([SCEN], {"score_marker": broken_low_scorer})
        self.assertIn("t_golden", failures)
        self.assertTrue(any("LOW" in e for e in failures["t_golden"]))

    def test_missing_scorer_in_registry_fails(self):
        failures = validate_corpus_golden([SCEN], {})
        self.assertIn("t_golden", failures)

    def test_missing_golden_fixtures_fails(self):
        scen = load_scenario(
            {
                "id": "t_nogolden",
                "suite": "agentic",
                "kind": "agentic",
                "title": "no golden",
                "capabilities": ["agentic.competence.artifact_construction"],
                "difficulty": "easy",
                "seed_source": "unit-test",
                "contamination_class": "public",
                "scorer": "score_marker",
                "sheet": "x",
                "verify": ["test -f marker.txt"],
            },
            TAX,
        )
        failures = validate_corpus_golden([scen], {"score_marker": good_scorer})
        self.assertIn("t_nogolden", failures)


if __name__ == "__main__":
    unittest.main()
