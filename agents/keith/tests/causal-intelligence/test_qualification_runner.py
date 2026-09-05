"""Exercise the runner with real isolated subprocesses and serialized evidence.

These are infrastructure tests. Their toy subprocesses are deliberately not
Keith runtime, provider, or causal-learning qualification.
"""

from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[2]
RUNNER = ROOT / "scripts/qualification/causal_intelligence.py"
MODULE_SPEC = importlib.util.spec_from_file_location("causal_intelligence", RUNNER)
assert MODULE_SPEC and MODULE_SPEC.loader
runner = importlib.util.module_from_spec(MODULE_SPEC)
MODULE_SPEC.loader.exec_module(runner)


class RunnerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="keith-runner-selftest-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        script = self.root / "scripts/qualification/causal_intelligence.py"
        script.parent.mkdir(parents=True)
        shutil.copyfile(RUNNER, script)
        git_dir = subprocess.run(["git", "rev-parse", "--absolute-git-dir"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()
        # Only read the actual revision. Never commit or mutate repository state.
        (self.root / ".git").write_text(f"gitdir: {git_dir}\n")
        self.spec = self.root / "spec/example/spec.kvx"
        self.spec.parent.mkdir(parents=True)
        self.spec.write_text('[task.1.1]\nstatus = "pending"\n')
        self.manifest = self.root / "tests/causal-intelligence/manifests/1.1.json"
        self.manifest.parent.mkdir(parents=True)
        self.definition = {
            "schema_version": 1,
            "task": "1.1",
            "source_paths": ["test_probe.py"],
            "required_case_ids": ["probe"],
            "cases": [{"id": "probe", "kind": "unittest", "argv": [sys.executable, "-m", "unittest", "test_probe", "-v"], "expected_case_count": 1, "timeout_seconds": 10}],
        }
        self.write_probe("self.assertEqual(2 + 2, 4)")

    def write_probe(self, body: str) -> None:
        (self.root / "test_probe.py").write_text(
            "import json, os, time, unittest\nfrom pathlib import Path\n\n"
            "class Probe(unittest.TestCase):\n    def test_probe(self):\n" + textwrap.indent(textwrap.dedent(body), "        ") + "\n"
        )

    def execute(self, *, save_manifest: bool = True) -> tuple[int, dict, Path]:
        if save_manifest:
            self.manifest.write_text(json.dumps(self.definition))
        result = subprocess.run(
            [sys.executable, "scripts/qualification/causal_intelligence.py", "verify", "--task", "1.1", "--spec", "spec/example/spec.kvx"],
            cwd=self.root,
            capture_output=True,
            text=True,
            timeout=20,
        )
        reports = sorted((self.root / "evidence/causal-intelligence/1.1").glob("*/result.json"), key=lambda path: path.stat().st_mtime_ns)
        self.assertTrue(reports, result.stderr)
        return result.returncode, json.loads(reports[-1].read_text()), reports[-1]

    def assert_failed(self, expected_reason: str) -> dict:
        code, report, _ = self.execute()
        self.assertNotEqual(code, 0)
        self.assertIn(expected_reason, json.dumps(report))
        return report

    def test_real_subprocess_count_and_identity(self) -> None:
        code, report, _ = self.execute()
        self.assertEqual(code, 0)
        self.assertEqual(report["cases"][0]["counts"], {"executed": 1, "passed": 1, "skipped": 0})
        self.assertEqual(len(report["source_digest"]), 64)
        self.assertIn("test_probe.py", report["source_identity"]["files"])
        self.assertEqual(len(report["cases"][0]["executable_sha256"]), 64)

    def test_executable_symlink_preserves_argv0_and_detects_retargeting(self) -> None:
        proxy = self.root / "python3-proxy"
        proxy.symlink_to(Path(sys.executable).resolve())
        self.definition["cases"][0]["argv"][0] = str(proxy)
        self.write_probe('import sys\nself.assertEqual(Path(sys.executable).name, "python3-proxy")')
        code, report, _ = self.execute()
        self.assertEqual(code, 0)
        self.assertEqual(report["cases"][0]["invocation_path"], str(proxy))
        self.assertEqual(report["cases"][0]["executable"], str(Path(sys.executable).resolve()))
        self.write_probe('Path("python3-proxy").unlink()\nPath("python3-proxy").symlink_to("/usr/bin/true")')
        self.assert_failed("executable changed")

    def test_skipped_required_case_fails(self) -> None:
        self.write_probe('self.skipTest("live provider unavailable")')
        self.assert_failed("skipped")

    def test_zero_discovered_cases_fail(self) -> None:
        (self.root / "test_probe.py").write_text("import unittest\n")
        code, report, _ = self.execute()
        self.assertNotEqual(code, 0)
        self.assertNotEqual(report["cases"][0]["status"], "passed")

    def test_failed_assertion_fails(self) -> None:
        self.write_probe("self.assertEqual(1, 2)")
        self.assert_failed("returned nonzero")

    def test_expected_count_mismatch_fails(self) -> None:
        self.definition["cases"][0]["expected_case_count"] = 2
        self.assert_failed("observed 1")

    def test_zero_expected_count_is_invalid(self) -> None:
        self.definition["cases"][0]["expected_case_count"] = 0
        self.assert_failed("positive integer")

    def test_missing_manifest_is_not_success(self) -> None:
        code, report, _ = self.execute(save_manifest=False)
        self.assertNotEqual(code, 0)
        self.assertIn("missing", report["reason"])

    def test_empty_manifest_is_not_success(self) -> None:
        self.definition.update(cases=[], required_case_ids=[])
        self.assert_failed("nonempty required cases")

    def test_missing_required_case_is_not_success(self) -> None:
        self.definition["required_case_ids"].append("runtime")
        self.assert_failed("complete ordered manifest")

    def test_unavailable_journey_blocks(self) -> None:
        self.definition["cases"] = [{"id": "probe", "kind": "unavailable", "reason": "Runtime implementation is not present", "expected_case_count": 2}]
        code, report, _ = self.execute()
        self.assertEqual(code, 3)
        self.assertEqual(report["status"], "blocked")

    def test_pass_json_cannot_replace_executed_test_summary(self) -> None:
        (self.root / "test_probe.py").write_text('print(\'{"status":"passed","count":1}\')\n')
        code, report, _ = self.execute()
        self.assertNotEqual(code, 0)
        self.assertNotEqual(report["cases"][0]["status"], "passed")

    def test_preexisting_pass_artifact_cannot_satisfy_fresh_run(self) -> None:
        self.definition["cases"][0]["required_artifacts"] = ["proof.json"]
        old = self.root / "evidence/causal-intelligence/1.1/old/probe"
        old.mkdir(parents=True)
        (old / "proof.json").write_text('{"status":"passed"}')
        self.assert_failed("missing or external source file")

    def test_new_artifact_requires_matching_invocation(self) -> None:
        self.definition["cases"][0]["required_artifacts"] = ["proof.json"]
        self.write_probe('''
            artifact = Path(os.environ["KEITH_QUALIFICATION_ARTIFACT_DIR"]) / "proof.json"
            artifact.write_text(json.dumps({"run_id": "old", "status": "passed"}))
            self.assertTrue(artifact.exists())
        ''')
        self.assert_failed("invocation identity mismatch")

    def write_valid_artifact_probe(self, *, stale: bool = False) -> None:
        self.definition["cases"][0]["required_artifacts"] = ["proof.json"]
        self.write_probe(textwrap.dedent('''
            artifact = Path(os.environ["KEITH_QUALIFICATION_ARTIFACT_DIR"]) / "proof.json"
            record = {key: os.environ["KEITH_QUALIFICATION_" + key.upper()] for key in ["run_id", "case_id", "source_digest"]}
            artifact.write_text(json.dumps(record))
            self.assertTrue(artifact.is_file())
        ''') + ('\nos.utime(artifact, (1, 1))' if stale else ''))

    def test_fresh_artifact_supplements_real_tests(self) -> None:
        self.write_valid_artifact_probe()
        code, report, _ = self.execute()
        self.assertEqual(code, 0)
        self.assertEqual(report["cases"][0]["artifacts"][0]["path"], "probe/proof.json")

    def test_old_timestamp_artifact_fails(self) -> None:
        self.write_valid_artifact_probe(stale=True)
        self.assert_failed("stale required artifact")

    def test_source_change_during_execution_fails(self) -> None:
        self.write_probe('Path("test_probe.py").write_text("# changed source\\n")')
        self.assert_failed("source, configuration, or binary changed")

    def test_binary_change_during_execution_fails(self) -> None:
        binary = self.root / "test-binary"
        binary.write_bytes(b"initial")
        self.definition["binary_paths"] = ["test-binary"]
        self.write_probe('Path("test-binary").write_bytes(b"changed")')
        self.assert_failed("source, configuration, or binary changed")

    def test_config_change_during_execution_fails(self) -> None:
        (self.root / "config.json").write_text('{"model":"initial"}')
        self.definition["config_paths"] = ["config.json"]
        self.write_probe('Path("config.json").write_text(\'{"model":"changed"}\')')
        self.assert_failed("source, configuration, or binary changed")

    def test_external_fixture_is_hashed_without_exporting_contents(self) -> None:
        fixture = self.root / "operator-fixture.json"
        fixture.write_text('{"private":"held-out-secret-sentinel"}')
        self.definition["external_fixture_paths"] = [str(fixture)]
        code, report, path = self.execute()
        self.assertEqual(code, 0)
        self.assertEqual(report["source_identity"]["external_fixtures"], {str(fixture): runner.digest(fixture)})
        self.assertNotIn(str(fixture), report["source_identity"]["binaries"])
        self.assertNotIn("held-out-secret-sentinel", path.read_text())

    def test_relative_external_fixture_is_rejected(self) -> None:
        for value in ["test_probe.py", str(self.root / "missing-fixture"), str(self.root)]:
            with self.subTest(value=value):
                self.definition["external_fixture_paths"] = [value]
                self.assert_failed("absolute regular file")

    def test_external_fixture_symlink_is_rejected(self) -> None:
        (self.root / "fixture-link").symlink_to(self.root / "test_probe.py")
        (self.root / "directory-link").symlink_to(self.root, target_is_directory=True)
        for value in [self.root / "fixture-link", self.root / "directory-link/test_probe.py"]:
            with self.subTest(value=value):
                self.definition["external_fixture_paths"] = [str(value)]
                self.assert_failed("fixture symlinks are forbidden")

    def test_external_fixture_change_during_execution_fails(self) -> None:
        fixture = self.root / "operator-fixture.json"
        fixture.write_text('{"version":1}')
        self.definition["external_fixture_paths"] = [str(fixture)]
        self.write_probe('Path("operator-fixture.json").write_text(\'{"version":2}\')')
        self.assert_failed("external fixture changed")

    def test_timeout_fails(self) -> None:
        self.write_probe("time.sleep(3)")
        self.definition["cases"][0]["timeout_seconds"] = 0.1
        report = self.assert_failed("timed out")
        self.assertTrue(report["cases"][0]["timed_out"])

    def test_raw_output_is_not_written_to_evidence(self) -> None:
        self.write_probe('print("sensitive-output-sentinel")')
        code, report, path = self.execute()
        self.assertEqual(code, 0)
        self.assertNotIn("sensitive-output-sentinel", path.read_text())
        self.assertGreater(report["cases"][0]["output_bytes"], 0)

    def test_external_source_symlink_fails(self) -> None:
        (self.root / "external.py").symlink_to(RUNNER)
        self.definition["source_paths"].append("external.py")
        self.assert_failed("external source path")

    def test_duplicate_json_keys_fail(self) -> None:
        self.manifest.write_text('{"schema_version":1,"schema_version":1}')
        code, report, _ = self.execute(save_manifest=False)
        self.assertNotEqual(code, 0)
        self.assertIn("duplicate JSON key", report["reason"])

    def test_cargo_counts_reject_empty_ignored_and_mismatch(self) -> None:
        good = "test result: ok. 2 passed; 0 failed; 0 ignored; 0 measured; 8 filtered out; finished in 0.01s\n"
        self.assertEqual(runner.test_counts("cargo_test", good, 2)["executed"], 2)
        for output, expected in [(good, 3), (good.replace("2 passed", "0 passed"), 1), (good.replace("0 ignored", "1 ignored"), 2), ("", 1)]:
            with self.subTest(output=output, expected=expected), self.assertRaises(runner.InvalidProof):
                runner.test_counts("cargo_test", output, expected)

    def test_rust_checks_enforce_locked_strict_scope(self) -> None:
        case = self.definition["cases"][0]
        case.update(kind="cargo_clippy", argv=["cargo", "clippy", "-p", "keith-memory", "--locked", "--all-targets", "--no-deps", "--", "-D", "warnings"])
        runner.validate_manifest(self.definition, "1.1")
        for omitted in ["--locked", "--all-targets", "--no-deps", "-D", "warnings"]:
            with self.subTest(omitted=omitted):
                modified = json.loads(json.dumps(self.definition))
                modified["cases"][0]["argv"].remove(omitted)
                with self.assertRaises(runner.InvalidProof):
                    runner.validate_manifest(modified, "1.1")


if __name__ == "__main__":
    unittest.main()
