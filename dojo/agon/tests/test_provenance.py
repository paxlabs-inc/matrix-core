import copy
import unittest
from pathlib import Path

from agon.provenance import (
    build_card,
    build_item_record,
    corpus_content_hash,
    item_fingerprint,
    reproduces,
    sign_card,
    verify_card,
)
from agon.schema import load_corpus
from agon.taxonomy import load_taxonomy

TAX = load_taxonomy()
CORPUS_DIR = Path(__file__).resolve().parent.parent / "corpus"
STD = "agon/0.2.0"
HARNESS = "dojo-harness/1"

PLAIN = {"id": "det_item", "kind": "plain", "system": "s", "sheet": "answer",
         "temperature": 0}
AGENTIC = {"id": "live_item", "kind": "agentic", "system": "s", "sheet": "do",
           "temperature": 0.3, "verify": ["test -f x"]}


def _item(scenario, model="m", seed=0, score=None):
    return build_item_record(
        scenario=scenario, model=model, served_model="served-" + model, seed=seed,
        score=score or {"dims": {"competence": 0.8}, "flags": []},
        ci=(0.7, 0.9), standard_version=STD, harness_version=HARNESS,
        corpus_hash="deadbeef", cache_version=STD)


class TestCorpusHash(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scen = load_corpus(CORPUS_DIR, TAX)

    def test_order_independent(self):
        h1 = corpus_content_hash(self.scen)
        h2 = corpus_content_hash(list(reversed(self.scen)))
        self.assertEqual(h1, h2)

    def test_changes_on_content(self):
        base = corpus_content_hash(self.scen)
        mutated = copy.deepcopy(self.scen)
        mutated[0]["sheet"] = mutated[0]["sheet"] + " (edited)"
        self.assertNotEqual(base, corpus_content_hash(mutated))


class TestFingerprint(unittest.TestCase):
    def _fp(self, **over):
        kw = dict(standard_version=STD, corpus_hash="ch", model="m",
                  served_model="sm", seed=0, scenario=PLAIN)
        kw.update(over)
        return item_fingerprint(**kw)

    def test_reproducible(self):
        self.assertEqual(self._fp(), self._fp())

    def test_sensitive_to_determinants(self):
        base = self._fp()
        self.assertNotEqual(base, self._fp(seed=1))
        self.assertNotEqual(base, self._fp(standard_version="agon/9"))
        self.assertNotEqual(base, self._fp(corpus_hash="other"))
        self.assertNotEqual(base, self._fp(model="m2"))


class TestDeterministicEligibility(unittest.TestCase):
    def test_plain_temp0_gets_fingerprint(self):
        rec = _item(PLAIN)
        self.assertTrue(rec["deterministic"])
        self.assertIn("fingerprint", rec)

    def test_agentic_item_has_no_fingerprint(self):
        rec = _item(AGENTIC)
        self.assertFalse(rec["deterministic"])
        self.assertNotIn("fingerprint", rec)


class TestCardIntegrity(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scen = load_corpus(CORPUS_DIR, TAX)

    def _card(self):
        return build_card(
            standard_version=STD, harness_version=HARNESS, scenarios=self.scen,
            temperature_policy={"plain": 0.2, "agentic": 0.3},
            items=[_item(PLAIN), _item(AGENTIC)],
            aggregates={"m": {"worker": 0.8}}, generated_at="2026-07-07T00:00:00Z")

    def test_sign_then_verify(self):
        card = sign_card(self._card())
        self.assertTrue(verify_card(card))

    def test_mutated_body_fails(self):
        card = sign_card(self._card())
        card["aggregates"]["m"]["worker"] = 0.99  # tamper after signing
        self.assertFalse(verify_card(card))

    def test_mutated_digest_fails(self):
        card = sign_card(self._card())
        card["integrity"]["digest"] = "0" * 64
        self.assertFalse(verify_card(card))

    def test_wrong_key_fails(self):
        card = sign_card(self._card(), key=b"key-a")
        self.assertFalse(verify_card(card, key=b"key-b"))
        self.assertTrue(verify_card(card, key=b"key-a"))

    def test_unsigned_card_fails(self):
        self.assertFalse(verify_card(self._card()))


class TestReproducibility(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scen = load_corpus(CORPUS_DIR, TAX)

    def _card(self, seed=0):
        return build_card(
            standard_version=STD, harness_version=HARNESS, scenarios=self.scen,
            temperature_policy={"plain": 0.0},
            items=[build_item_record(
                scenario=PLAIN, model="m", served_model="sm", seed=seed,
                score={"dims": {"competence": 1.0}, "flags": []}, ci=None,
                standard_version=STD, harness_version=HARNESS,
                corpus_hash=corpus_content_hash(self.scen), cache_version=STD)],
            aggregates={}, generated_at="2026-07-07T00:00:00Z")

    def test_same_inputs_reproduce(self):
        # Same corpus hash + version + seed -> a deterministic item reproduces
        # exactly, even across two independently built cards.
        self.assertTrue(reproduces(self._card(seed=0), self._card(seed=0)))

    def test_different_seed_does_not_reproduce(self):
        self.assertFalse(reproduces(self._card(seed=0), self._card(seed=1)))


if __name__ == "__main__":
    unittest.main()
