"""AGON declarative scenario schema + loader + validation.

A scenario is a declarative, schema-validated record - never a Python literal
buried in one file. The loader validates every record against the schema and
rejects an unknown field, a missing required field, an out-of-range enum, or a
capability leaf not present in the taxonomy. This is what lets the corpus grow to
hundreds of contributable items without rotting.

The record separates the DECLARATION (what the loader validates here) from the
executable BINDING (the scorer / ground_truth / files, resolved by a registry at
run time - see registry.py / the port in corpus/). The loader may be given a
scorer registry to also reject an unknown scorer/ground_truth id.
"""

import json
from pathlib import Path

from .taxonomy import load_taxonomy

# Closed field set: any key outside REQUIRED | OPTIONAL is rejected (req 2.2).
REQUIRED_FIELDS = {
    "id",
    "suite",
    "kind",
    "title",
    "capabilities",
    "difficulty",
    "seed_source",
    "contamination_class",
    "scorer",
    "sheet",
}
OPTIONAL_FIELDS = {
    "system",
    "files",
    "verify",
    "ground_truth",
    "sut_config",
    "max_steps",
    "temperature",
    "max_tokens",
    "step_max_tokens",
    "protected",
    "inject",
    "notes",
    "golden",
    "oracle",
    "oracle_mode",
}

# Deterministic-oracle grading modes for raw-model (Suite 1) items (req 4.2).
VALID_ORACLE_MODES = {"numeric", "exact", "contains_all", "json_keys", "abstains"}
ALLOWED_FIELDS = REQUIRED_FIELDS | OPTIONAL_FIELDS

VALID_SUITES = {"raw_model", "agentic", "mra"}
VALID_KINDS = {"plain", "agentic", "mra"}
VALID_DIFFICULTY = {"easy", "medium", "hard"}
VALID_CONTAMINATION = {"public", "holdout", "rotating"}

# Suite 3 ablation variants (req 6.1). A sut_config declares which run.
VALID_SUT_VARIANTS = {"main_only", "main_cassandra", "main_cassandra_cortex"}


class ScenarioError(ValueError):
    """Raised when a scenario record violates the schema or references an unknown
    capability/scorer."""


class Scenario(dict):
    """A validated scenario record. Attribute-style access to common fields is
    intentionally NOT added - callers use dict access so the record stays a plain
    serializable document."""

    @property
    def id(self):
        return self["id"]

    @property
    def suite(self):
        return self["suite"]

    @property
    def kind(self):
        return self["kind"]

    @property
    def capabilities(self):
        return list(self.get("capabilities", []))


def load_scenario(record, taxonomy=None, scorers=None):
    """Validate a single scenario record and return a Scenario. `scorers`, if
    given, is a set/dict of known scorer ids; an unknown scorer/ground_truth id is
    then rejected too."""
    taxonomy = taxonomy or load_taxonomy()
    errors = list(_iter_scenario_errors(record, taxonomy, scorers))
    if errors:
        sid = record.get("id", "<no id>") if isinstance(record, dict) else "<not a dict>"
        raise ScenarioError(f"scenario {sid} invalid:\n  - " + "\n  - ".join(errors))
    return Scenario(record)


def _iter_scenario_errors(record, taxonomy, scorers):
    if not isinstance(record, dict):
        yield "record is not an object"
        return

    keys = set(record.keys())
    for extra in sorted(keys - ALLOWED_FIELDS):
        yield f"unknown field: {extra}"
    for missing in sorted(REQUIRED_FIELDS - keys):
        yield f"missing required field: {missing}"

    suite = record.get("suite")
    if suite is not None and suite not in VALID_SUITES:
        yield f"invalid suite: {suite}"
    kind = record.get("kind")
    if kind is not None and kind not in VALID_KINDS:
        yield f"invalid kind: {kind}"
    diff = record.get("difficulty")
    if diff is not None and diff not in VALID_DIFFICULTY:
        yield f"invalid difficulty: {diff}"
    cc = record.get("contamination_class")
    if cc is not None and cc not in VALID_CONTAMINATION:
        yield f"invalid contamination_class: {cc}"

    if "id" in record and (not isinstance(record["id"], str) or not record["id"].strip()):
        yield "id must be a non-empty string"

    caps = record.get("capabilities")
    if caps is not None:
        if not isinstance(caps, list) or not caps:
            yield "capabilities must be a non-empty list of taxonomy leaf ids"
        else:
            for cap in caps:
                if not isinstance(cap, str) or not taxonomy.has_leaf(cap):
                    yield f"unknown capability (not in taxonomy): {cap}"
                elif suite in VALID_SUITES and taxonomy.suite_of(cap) != suite:
                    yield f"capability {cap} is not in the declared suite '{suite}'"

    verify = record.get("verify")
    if verify is not None:
        if not isinstance(verify, list) or not all(isinstance(c, str) for c in verify):
            yield "verify must be a list of shell command strings"

    files = record.get("files")
    if files is not None:
        if not isinstance(files, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in files.items()
        ):
            yield "files must be a map of path -> content strings"

    protected = record.get("protected")
    if protected is not None and (
        not isinstance(protected, list) or not all(isinstance(p, str) for p in protected)
    ):
        yield "protected must be a list of path strings"

    sut = record.get("sut_config")
    if kind == "mra" and not sut:
        yield "kind 'mra' requires a sut_config listing ablation variants"
    if sut is not None:
        if not isinstance(sut, list) or not sut:
            yield "sut_config must be a non-empty list of ablation variants"
        else:
            for v in sut:
                if v not in VALID_SUT_VARIANTS:
                    yield f"invalid sut_config variant: {v}"

    golden = record.get("golden")
    if golden is not None:
        if not isinstance(golden, dict):
            yield "golden must be an object with 'correct' and 'incorrect' fixtures"
        else:
            for side in ("correct", "incorrect"):
                if side not in golden:
                    yield f"golden missing '{side}' fixture"
                elif not isinstance(golden[side], dict):
                    yield f"golden.{side} must be an object"

    om = record.get("oracle_mode")
    if om is not None and om not in VALID_ORACLE_MODES:
        yield f"invalid oracle_mode: {om}"
    if om is not None and "oracle" not in record:
        yield "oracle_mode set but no oracle value provided"

    if scorers is not None:
        sc = record.get("scorer")
        if sc is not None and sc not in scorers:
            yield f"unknown scorer id: {sc}"
        gt = record.get("ground_truth")
        if gt is not None and gt not in scorers:
            yield f"unknown ground_truth id: {gt}"


def load_corpus(source, taxonomy=None, scorers=None):
    """Load and validate a corpus. `source` is either a directory of *.json
    scenario files, or a list of scenario records. Returns the list of validated
    Scenario objects. Rejects duplicate ids."""
    taxonomy = taxonomy or load_taxonomy()
    records = _read_records(source)
    scenarios = []
    seen = {}
    for rec in records:
        scen = load_scenario(rec, taxonomy, scorers)
        if scen.id in seen:
            raise ScenarioError(f"duplicate scenario id: {scen.id}")
        seen[scen.id] = scen
        scenarios.append(scen)
    return scenarios


def _read_records(source):
    if isinstance(source, (list, tuple)):
        return list(source)
    path = Path(source)
    if path.is_dir():
        recs = []
        for f in sorted(path.glob("*.json")):
            data = json.loads(f.read_text(encoding="utf-8"))
            if isinstance(data, list):
                recs.extend(data)
            else:
                recs.append(data)
        return recs
    data = json.loads(path.read_text(encoding="utf-8"))
    return data if isinstance(data, list) else [data]


def coverage_report(scenarios, taxonomy=None):
    """Map each taxonomy leaf id -> the count of scenarios probing it. Used by the
    coverage-floor check (req 1.4 min_items)."""
    taxonomy = taxonomy or load_taxonomy()
    counts = {lid: 0 for lid in taxonomy.leaf_ids()}
    for s in scenarios:
        for cap in s.capabilities:
            if cap in counts:
                counts[cap] += 1
    return counts
