import tempfile
import unittest
from pathlib import Path

from agon.calibration import (
    calibrate,
    item_discrimination,
    load_calibration,
    save_calibration,
    weighted_score,
)

# Reference panel: two strong models, two weak. Item scores designed so that:
#  - disc_hi separates cleanly (strong pass, weak fail) -> high discrimination
#  - disc_mid separates moderately
#  - dead_easy everyone passes -> dead
#  - dead_hard everyone fails -> dead
PANEL = {
    "strong_a": {"disc_hi": 1.0, "disc_mid": 0.9, "dead_easy": 1.0, "dead_hard": 0.0},
    "strong_b": {"disc_hi": 0.9, "disc_mid": 0.8, "dead_easy": 1.0, "dead_hard": 0.0},
    "weak_a":   {"disc_hi": 0.1, "disc_mid": 0.5, "dead_easy": 1.0, "dead_hard": 0.0},
    "weak_b":   {"disc_hi": 0.0, "disc_mid": 0.4, "dead_easy": 1.0, "dead_hard": 0.0},
}
LABELS = {"strong_a": "strong", "strong_b": "strong",
          "weak_a": "weak", "weak_b": "weak"}
ITEM_REALM = {"disc_hi": "raw_model.reasoning", "disc_mid": "raw_model.reasoning",
              "dead_easy": "raw_model.knowledge", "dead_hard": "raw_model.knowledge"}


class TestDiscrimination(unittest.TestCase):
    def test_high_separates(self):
        d = item_discrimination("disc_hi", PANEL, LABELS)
        self.assertAlmostEqual(d, 0.95 - 0.05, places=6)

    def test_dead_items_zero(self):
        self.assertEqual(item_discrimination("dead_easy", PANEL, LABELS), 0.0)
        self.assertEqual(item_discrimination("dead_hard", PANEL, LABELS), 0.0)

    def test_missing_side_none(self):
        panel = {"strong_a": {"x": 1.0}}
        self.assertIsNone(item_discrimination("x", panel,
                                              {"strong_a": "strong"}))


class TestCalibrate(unittest.TestCase):
    def test_reproducible(self):
        c1 = calibrate(PANEL, LABELS, ITEM_REALM)
        c2 = calibrate(PANEL, LABELS, ITEM_REALM)
        self.assertEqual(c1, c2)

    def test_dead_items_flagged(self):
        c = calibrate(PANEL, LABELS, ITEM_REALM)
        self.assertIn("dead_easy", c["dead_items"])
        self.assertIn("dead_hard", c["dead_items"])
        self.assertNotIn("disc_hi", c["dead_items"])

    def test_weights_sum_to_one(self):
        c = calibrate(PANEL, LABELS, ITEM_REALM)
        self.assertAlmostEqual(sum(c["realm_weights"].values()), 1.0, places=5)
        # only the discriminating realm earned weight
        self.assertIn("raw_model.reasoning", c["realm_weights"])
        self.assertNotIn("raw_model.knowledge", c["realm_weights"])

    def test_removing_high_disc_item_changes_weights(self):
        base = calibrate(PANEL, LABELS, ITEM_REALM)
        # add a second realm with its own discriminating item so weights are shared
        panel = {m: dict(s) for m, s in PANEL.items()}
        panel["strong_a"]["other"] = 1.0
        panel["strong_b"]["other"] = 1.0
        panel["weak_a"]["other"] = 0.0
        panel["weak_b"]["other"] = 0.0
        item_realm = dict(ITEM_REALM, other="raw_model.instruction")
        full = calibrate(panel, LABELS, item_realm)
        # drop the high-discrimination reasoning item
        panel2 = {m: {k: v for k, v in s.items() if k != "disc_hi"}
                  for m, s in panel.items()}
        reduced = calibrate(panel2, LABELS, item_realm)
        self.assertNotEqual(full["realm_weights"], reduced["realm_weights"])
        # reasoning realm's weight must drop when its strongest item is removed
        self.assertLess(reduced["realm_weights"]["raw_model.reasoning"],
                        full["realm_weights"]["raw_model.reasoning"])
        self.assertNotEqual(base, full)

    def test_persist_roundtrip(self):
        c = calibrate(PANEL, LABELS, ITEM_REALM)
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "cal.json"
            save_calibration(c, p)
            self.assertEqual(load_calibration(p), c)


class TestWeightedScore(unittest.TestCase):
    def test_combine(self):
        weights = {"a": 0.75, "b": 0.25}
        self.assertEqual(weighted_score({"a": 1.0, "b": 0.0}, weights), 0.75)

    def test_none_realm_renormalizes(self):
        weights = {"a": 0.5, "b": 0.5}
        self.assertEqual(weighted_score({"a": 0.8, "b": None}, weights), 0.8)


if __name__ == "__main__":
    unittest.main()
