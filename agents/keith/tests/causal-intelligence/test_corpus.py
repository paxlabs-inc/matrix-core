"""Frozen corpus structure and provenance checks; never executes feature cases."""

from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import unittest


ROOT = Path(__file__).resolve().parents[2]
CORPUS = Path(__file__).with_name("corpus")
PRIVATE = Path("/root/keith-evaluation/causal-intelligence/v1")


def read(path):
    return json.loads(path.read_text())


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def words(value):
    stop = set("a an the of to in on at by as is are was were be been being it its and or for from with without that this those these which what where when why how can could should must do does did have has had we our their them they you your before after while than then only not into during again also one same me i so if will whose who get both about all through even most may need every".split())
    return {word for word in re.findall(r"[a-z]+", value.lower()) if word not in stop}


class CorpusTests(unittest.TestCase):
    def test_required_cases_have_executable_ownership_and_no_fabricated_outcomes(self):
        cases = read(CORPUS / "cases.json")["cases"]
        self.assertEqual({case["id"] for case in cases}, {f"C{i:02}" for i in range(1, 21)} | {f"B{i:02}" for i in range(1, 11)})
        for case in cases:
            with self.subTest(case=case["id"]):
                self.assertEqual(case["expected_case_count"], len(case["variants"]))
                self.assertGreater(case["expected_case_count"], 0)
                for field in ["initial_state", "stimulus", "expected_authoritative_observations", "owners", "owner_tasks", "required_evidence_levels", "required_versions", "remaining_ambiguity"]:
                    self.assertTrue(case[field])
                self.assertEqual(case["result"], "pending")
                self.assertEqual(case["actual_evidence"], [])

    def test_every_spec_task_has_a_non_success_missing_proof_mapping(self):
        spec = (ROOT / "spec/keith-causal-intelligence/spec.kvx").read_text()
        expected = set(re.findall(r"^\[task\.(\d+\.\d+)\]$", spec, re.MULTILINE))
        tasks = read(CORPUS / "task-map.json")["tasks"]
        self.assertEqual(set(tasks), expected)
        for task, value in tasks.items():
            with self.subTest(task=task):
                self.assertTrue(set(value["requires"]) <= expected)
                self.assertIn(task, value["verify_argv"])
                self.assertTrue(value["required_checks"])
                self.assertTrue(value["requirements"])
                self.assertIn("non-success", value["missing_proof_policy"])
                for package in value["packages"]:
                    self.assertTrue(package.startswith("keith-"))

    def test_private_bundle_matches_exact_commitments_and_permissions(self):
        commitment = read(CORPUS / "commitments.json")
        self.assertEqual(Path(commitment["operator_root"]), PRIVATE)
        self.assertFalse(PRIVATE.is_symlink())
        self.assertEqual(stat.S_IMODE(PRIVATE.stat().st_mode), 0o700)
        for name, expected in commitment["external_files"].items():
            path = PRIVATE / name
            self.assertTrue(path.is_file(), "sealed operator bundle missing; restore separately")
            self.assertFalse(path.is_symlink())
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertTrue(sha(path) == expected["sha256"], "sealed fixture identity changed")
            self.assertEqual(path.stat().st_size, expected["bytes"])
            if "records" in expected:
                self.assertEqual(len(read(path)["records"]), expected["records"])

    def test_frozen_public_artifacts_match_operator_copy(self):
        public = read(CORPUS / "freeze.json")
        self.assertTrue(public == read(PRIVATE / "public-freeze.json"), "operator freeze commitment mismatch")
        previous = public["previous_public_freeze"]
        archived = PRIVATE / previous["file"]
        self.assertEqual(sha(archived), previous["sha256"])
        original = read(archived)
        changed = {name for name, digest in original["sha256"].items() if public["sha256"][name] != digest}
        self.assertEqual(changed, {"task-map.json"})
        for filename, expected in public["sha256"].items():
            self.assertTrue(sha(CORPUS / filename) == expected, f"frozen public artifact changed: {filename}")

    def test_family_source_and_chronological_splits_are_disjoint(self):
        groups = [read(CORPUS / "development.json")["records"], read(PRIVATE / "validation.json")["records"], read(PRIVATE / "semantic-held-out.json")["records"]]
        prior_families, prior_roots, prior_ids = set(), set(), set()
        last_query = None
        for group in groups:
            families = {row["family"] for row in group}
            roots = {row["source_root"] for row in group}
            identifiers = {row["id"] for row in group}
            self.assertFalse(families & prior_families, "family leakage between splits")
            self.assertFalse(roots & prior_roots, "source leakage between splits")
            self.assertFalse(identifiers & prior_ids, "case leakage between splits")
            earliest = min(datetime.fromisoformat(row["observed_at"]) for row in group)
            if last_query:
                self.assertGreater(earliest, last_query)
            for row in group:
                self.assertLess(datetime.fromisoformat(row["observed_at"]), datetime.fromisoformat(row["query_at"]))
            last_query = max(datetime.fromisoformat(row["query_at"]) for row in group)
            prior_families |= families
            prior_roots |= roots
            prior_ids |= identifiers

    def test_semantic_gate_has_200_distinct_queries_with_declared_clusters(self):
        data = read(PRIVATE / "semantic-held-out.json")
        rows = data["records"]
        self.assertEqual(len(rows), 200)
        self.assertEqual(len({row["id"] for row in rows}), 200)
        self.assertEqual(len({row["query"] for row in rows}), 200)
        self.assertEqual(len({row["source_root"] for row in rows}), 40)
        self.assertEqual(data["correlation_unit"], "source_root")
        sources = {}
        for row in rows:
            self.assertEqual(row["gold_anchor_ids"], [row["anchor_id"]])
            self.assertFalse(row["anchor_id"] in row["query"])
            self.assertFalse(row["source_root"] in row["query"])
            if row["anchor_id"] in sources:
                self.assertTrue(sources[row["anchor_id"]] == row["source"], "same anchor has inconsistent content")
            sources[row["anchor_id"]] = row["source"]

    def test_semantic_pairs_meet_predeclared_low_overlap_rule(self):
        # Structural overlap is not a trained-encoder benchmark or a relevance score.
        for row in read(PRIVATE / "semantic-held-out.json")["records"]:
            left, right = words(row["source"]), words(row["query"])
            self.assertGreaterEqual(len(right), 3, "query lacks substantive words")
            self.assertLessEqual(len(left & right) / len(left | right), 0.25, "sealed pair exceeds predeclared content-word Jaccard ceiling")

    def test_candidate_input_allowlist_is_development_only(self):
        manifest = read(CORPUS / "candidate-inputs.json")
        self.assertEqual(manifest["allowed_files"], ["development.json"])
        self.assertFalse(manifest["external_fixture_access"])
        for name in manifest["allowed_files"]:
            path = CORPUS / name
            self.assertTrue(path.resolve().is_relative_to(CORPUS.resolve()))
            self.assertFalse(path.is_symlink())
        self.assertFalse(any(path.name.startswith("semantic-held-out") for path in CORPUS.rglob("*")))

    def test_evaluation_gates_metrics_and_replicates_are_frozen(self):
        policy = read(CORPUS / "evaluation-policy.json")
        self.assertEqual(policy["replicates"]["replicate_ids"], [1, 2, 3])
        self.assertTrue(policy["replicates"]["report_all"])
        self.assertEqual(policy["gates"]["semantic_recall_at_10"], 0.95)
        self.assertEqual(policy["gates"]["required_actual_inclusion"], 1.0)
        self.assertEqual(policy["gates"]["ordinary_success_loss_max_percentage_points"], 2.0)
        self.assertEqual(policy["comparison_status"], "pending_execution")
        self.assertEqual(set(policy["ablations"]), {"A", "B", "C", "C_semantic_only", "C_prediction_only", "D", "E", "F"})
        for metric in policy["metrics"].values():
            self.assertTrue(metric["numerator"] and metric["denominator"] and metric["authority"])
            self.assertIn("unavailable", metric["zero_denominator"])
        optional = policy["optional_gates"]
        self.assertEqual(optional["reconstruction"]["primary_metric"], "paired_verified_task_success")
        self.assertEqual(optional["pattern_retention"]["primary_metric"], "later_correct_decision_rate")
        self.assertEqual(optional["shared_constraints"], {"ordinary_success_loss_max_percentage_points": 2.0, "unsupported_claim_rate_increase_max_percentage_points": 0.0, "unnecessary_intervention_rate_increase_max_percentage_points": 2.0})

    def test_optional_experiments_have_actual_chronological_held_out_inputs(self):
        expected = {
            "latent-held-out.json": {"explicit-reversal", "rare-exception", "scoped-correction", "forgetting", "self-caused-feedback", "topic-not-preference", "delayed-usefulness", "counterevidence"},
            "reconstruction-transfer-held-out.json": {"incidental-variation", "causal-condition-change", "misleading-analogy", "missing-bridge", "unsupported-detail", "delayed-outcome", "source-correction", "contradicted-lesson"},
        }
        for filename, coverage in expected.items():
            data = read(PRIVATE / filename)
            self.assertEqual(len(data["records"]), 24)
            self.assertEqual({row["family"] for row in data["records"]}, coverage)
            self.assertEqual(data["correlation_unit"], "family")
            for row in data["records"]:
                self.assertTrue(row["events"] and row["query"] and row["gold"])
                self.assertEqual(row["status"], "pending")
                decision_time = datetime.fromisoformat(row["decision_time"])
                self.assertTrue(all(datetime.fromisoformat(event["time"]) < decision_time for event in row["events"]))
                if filename == "latent-held-out.json":
                    self.assertTrue(all(event["self_caused"] for event in row["events"] if event["kind"] == "assistant_generated"))

    def test_ordinary_set_is_pending_and_baseline_is_an_actual_prior_run(self):
        ordinary = read(PRIVATE / "ordinary-held-out.json")
        self.assertEqual(len(ordinary["records"]), 60)
        self.assertEqual(len({row["family"] for row in ordinary["records"]}), 20)
        self.assertTrue(all(row["status"] == "pending" for row in ordinary["records"]))
        baseline = read(PRIVATE / "baseline.json")
        self.assertEqual(baseline["status"], "passed")
        self.assertEqual([case["counts"]["executed"] for case in baseline["cases"]], [24, 2])
        self.assertTrue(all(case["exit_code"] == 0 for case in baseline["cases"]))


if __name__ == "__main__":
    unittest.main()
