"""AGON capability taxonomy: loader + validator.

The taxonomy is the versioned source-of-truth artifact (taxonomy.json). It is
partitioned by system-under-test into exactly three suites; each suite has realms;
each realm has leaf capabilities carrying a definition, a falsifiable pass
condition, difficulty tiers, and a minimum item count.

Every scenario in the corpus references only leaf ids that exist here. This module
loads the artifact, validates its internal consistency, and exposes leaf lookup
used by the scenario loader (schema.py) and the coverage checks.
"""

import json
from pathlib import Path

TAXONOMY_PATH = Path(__file__).parent / "taxonomy.json"

# The three suites are fixed by the standard (req 1.1). A suite key outside this
# set, or a missing one, is a hard validation error.
REQUIRED_SUITES = ("raw_model", "agentic", "mra")
VALID_DIFFICULTY = ("easy", "medium", "hard")

# A leaf id is addressed globally as "<suite>.<realm>.<leaf>".
LEAF_REQUIRED_FIELDS = ("definition", "pass_condition", "difficulty_tiers", "min_items")


class TaxonomyError(ValueError):
    """Raised when the taxonomy artifact is malformed or internally inconsistent."""


class Taxonomy:
    def __init__(self, data):
        self.version = data.get("standard_version")
        self.suites = data.get("suites", {})
        self.aqi = data.get("aqi", {})
        self._data = data
        self._leaves = {}  # "suite.realm.leaf" -> leaf dict
        self._index()

    def _index(self):
        for suite_key, suite in self.suites.items():
            for realm_key, realm in (suite.get("realms") or {}).items():
                for leaf_key, leaf in (realm.get("leaves") or {}).items():
                    self._leaves[f"{suite_key}.{realm_key}.{leaf_key}"] = leaf

    def leaf_ids(self):
        return set(self._leaves.keys())

    def has_leaf(self, leaf_id):
        return leaf_id in self._leaves

    def leaf(self, leaf_id):
        return self._leaves.get(leaf_id)

    def suite_of(self, leaf_id):
        return leaf_id.split(".", 1)[0]

    def min_items(self, leaf_id):
        return int(self._leaves[leaf_id]["min_items"])


def load_taxonomy(path=TAXONOMY_PATH):
    """Load and validate the taxonomy artifact. Raises TaxonomyError on any
    malformed leaf or inconsistent suite/realm/leaf tree."""
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    tax = Taxonomy(data)
    validate_taxonomy(tax)
    return tax


def validate_taxonomy(tax):
    errors = list(_iter_taxonomy_errors(tax))
    if errors:
        raise TaxonomyError("taxonomy invalid:\n  - " + "\n  - ".join(errors))
    return True


def _iter_taxonomy_errors(tax):
    if not tax.version or not isinstance(tax.version, str):
        yield "missing or non-string standard_version"

    present = set(tax.suites.keys())
    required = set(REQUIRED_SUITES)
    for missing in sorted(required - present):
        yield f"missing required suite: {missing}"
    for extra in sorted(present - required):
        yield f"unknown suite (not one of the three): {extra}"

    seen_index = {}
    for suite_key in sorted(tax.suites.keys()):
        suite = tax.suites[suite_key]
        index = suite.get("index")
        if not index:
            yield f"suite {suite_key}: missing index (MPI/APS/RAS)"
        elif index in seen_index:
            yield f"suite {suite_key}: duplicate index {index} (also {seen_index[index]})"
        else:
            seen_index[index] = suite_key
        if not suite.get("title"):
            yield f"suite {suite_key}: missing title"
        if not suite.get("sut"):
            yield f"suite {suite_key}: missing sut description"

        realms = suite.get("realms")
        if not isinstance(realms, dict) or not realms:
            yield f"suite {suite_key}: must have at least one realm"
            continue
        for realm_key in sorted(realms.keys()):
            realm = realms[realm_key]
            if not realm.get("title"):
                yield f"{suite_key}.{realm_key}: realm missing title"
            leaves = realm.get("leaves")
            if not isinstance(leaves, dict) or not leaves:
                yield f"{suite_key}.{realm_key}: realm must have at least one leaf"
                continue
            for leaf_key in sorted(leaves.keys()):
                yield from _iter_leaf_errors(suite_key, realm_key, leaf_key, leaves[leaf_key])


def _iter_leaf_errors(suite_key, realm_key, leaf_key, leaf):
    lid = f"{suite_key}.{realm_key}.{leaf_key}"
    if not isinstance(leaf, dict):
        yield f"{lid}: leaf is not an object"
        return
    for field in LEAF_REQUIRED_FIELDS:
        if field not in leaf:
            yield f"{lid}: leaf missing required field '{field}'"
    if "definition" in leaf and not str(leaf["definition"]).strip():
        yield f"{lid}: empty definition"
    if "pass_condition" in leaf and not str(leaf["pass_condition"]).strip():
        yield f"{lid}: empty pass_condition (a leaf must be falsifiable)"
    tiers = leaf.get("difficulty_tiers")
    if "difficulty_tiers" in leaf:
        if not isinstance(tiers, list) or not tiers:
            yield f"{lid}: difficulty_tiers must be a non-empty list"
        else:
            for t in tiers:
                if t not in VALID_DIFFICULTY:
                    yield f"{lid}: invalid difficulty tier '{t}'"
    if "min_items" in leaf:
        mi = leaf["min_items"]
        if not isinstance(mi, int) or isinstance(mi, bool) or mi < 1:
            yield f"{lid}: min_items must be a positive integer"
