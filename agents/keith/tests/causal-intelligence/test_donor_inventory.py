"""Source-conformance proof for task 1.4; this never imports donor code.

The manifest is a pinned review snapshot, not a generated-on-test inventory.
Missing or changed donor sources fail visibly. Relocated checkouts can be
provided with KEITH_GIDEON_CORE_SOURCE and KEITH_GIDEON_APPS_SOURCE.
No test here proves an imported runtime capability or a release.
"""

from __future__ import annotations

import ast
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tomllib
import unittest


ROOT = Path(__file__).resolve().parents[2]
INVENTORY = Path(__file__).with_name("donor-inventory.json")
DISPOSITIONS = {"implemented_here", "existing_equivalent", "follow_on_adapter", "stateful_product_migration"}
COUPLINGS = {"content_or_pure_function", "io_adapter", "stateful_controller"}


class InventoryError(ValueError):
    """The review snapshot cannot establish source conformance."""


def relative_path(value: str) -> Path:
    path = Path(value)
    if not value or path.is_absolute() or ".." in path.parts or "." in path.parts:
        raise InventoryError(f"unsafe source path: {value}")
    return path


def source_roots(data: dict) -> dict[str, Path]:
    roots = {}
    for name, source in data["sources"].items():
        root = Path(os.environ.get(source["environment_override"], source["path"]))
        if not root.is_dir():
            raise InventoryError(f"source checkout unavailable: {root}; supply {source['environment_override']}")
        roots[name] = root.resolve()
    return roots


def source_file(roots: dict[str, Path], row: dict) -> Path:
    root = roots[row["root"]]
    path = root / relative_path(row["path"])
    if not path.is_file() or path.is_symlink() or not path.resolve().is_relative_to(root):
        raise InventoryError(f"missing or unsafe source file: {path}")
    return path


def validate_snapshot(data: dict, roots: dict[str, Path]) -> None:
    if data["scope"] != "source_inventory_only" or data["runtime_qualified"] is not False:
        raise InventoryError("inventory may not claim runtime qualification")
    seen = set()
    for row in data["files"]:
        key = (row["root"], row["path"])
        if key in seen:
            raise InventoryError(f"duplicate source: {key}")
        seen.add(key)
        path = source_file(roots, row)
        payload = path.read_bytes()
        if len(payload) != row["bytes"] or hashlib.sha256(payload).hexdigest() != row["sha256"]:
            raise InventoryError(f"source changed since review: {path}")
        if row["family"] not in data["families"]:
            raise InventoryError(f"unclassified source: {path}")
    for group in data["collections"]:
        root = roots[group["root"]]
        actual = {str(path.relative_to(root)) for path in root.glob(group["pattern"]) if path.is_file()}
        recorded = {row["path"] for row in data["files"] if row["collection"] == group["id"]}
        if len(actual) != group["count"] or actual != recorded:
            raise InventoryError(f"incomplete or stale collection: {group['id']}")


class DonorInventoryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.data = json.loads(INVENTORY.read_text())
        cls.roots = source_roots(cls.data)

    def test_real_source_snapshot_and_complete_discovery(self) -> None:
        validate_snapshot(self.data, self.roots)
        self.assertEqual(self.data["schema_version"], 1)
        self.assertEqual(self.data["task"], "1.4")
        self.assertEqual(sum(group["count"] for group in self.data["collections"]), 152)

    def test_every_candidate_has_explicit_owner_disposition_and_proof_boundary(self) -> None:
        used = {row["family"] for row in self.data["files"]}
        self.assertEqual(used, set(self.data["families"]))
        for family in self.data["families"].values():
            with self.subTest(family=family["keith_owner"]):
                self.assertIn(family["coupling"], COUPLINGS)
                self.assertIn(family["disposition"], DISPOSITIONS)
                self.assertNotEqual(family["disposition"], "implemented_here")
                self.assertFalse(family["runtime_qualified"])
                for key in ["missing_behavior", "existing_equivalent", "dependencies", "changed_imports", "platforms", "proof_required", "next_task"]:
                    self.assertTrue(family[key], key)
                if re.fullmatch(r"\d+\.\d+", family["next_task"]):
                    spec = (ROOT / "spec/keith-causal-intelligence/spec.kvx").read_text()
                    self.assertIn(f"[task.{family['next_task']}]", spec)
                for owner in family["keith_owner"]:
                    self.assertTrue((ROOT / relative_path(owner)).exists(), owner)

    def test_git_revision_and_hash_snapshot_provenance(self) -> None:
        result = subprocess.run(["git", "rev-parse", "HEAD"], cwd=self.roots["core"], check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), self.data["sources"]["core"]["git_head"])
        self.assertIsNone(self.data["sources"]["apps"]["git_head"])
        self.assertIn("per-file SHA-256", self.data["sources"]["apps"]["provenance"])
        for row in self.data["files"]:
            self.assertRegex(row["sha256"], r"^[0-9a-f]{64}$")

    def test_manifest_versions_dependencies_and_permissions_match_sources(self) -> None:
        for row in self.data["files"]:
            if not row["path"].endswith("/app.json"):
                continue
            with self.subTest(path=row["path"]):
                manifest = json.loads(source_file(self.roots, row).read_text())
                self.assertEqual(manifest["version"], row["manifest_version"])
                self.assertEqual(manifest.get("dependencies", {}), row["manifest_dependencies"])
                self.assertEqual(manifest.get("permissions", {}), row["manifest_permissions"])

    def test_license_notices_and_format_dependency_lock(self) -> None:
        for row in self.data["licenses"]:
            payload = source_file(self.roots, row).read_bytes()
            self.assertEqual(hashlib.sha256(payload).hexdigest(), row["sha256"])
            if row["license"] == "MIT":
                self.assertIn(b"MIT License", payload)
        record = self.data["dependency_lock"]
        payload = source_file(self.roots, record).read_bytes()
        self.assertEqual(hashlib.sha256(payload).hexdigest(), record["sha256"])
        versions = {item["name"]: item["version"] for item in tomllib.loads(payload.decode())["package"]}
        for name, version in record["versions"].items():
            self.assertEqual(versions[name], version)

    def test_first_writer_has_closed_document_imports_and_one_external_format_dependency(self) -> None:
        selected = self.data["first_slice"]["package_files"]
        modules = {path.removeprefix("src/").removesuffix(".py").replace("/", ".") for path in selected}
        external = set()
        for path in selected:
            tree = ast.parse((self.roots["core"] / path).read_text())
            for node in ast.walk(tree):
                names = [node.module] if isinstance(node, ast.ImportFrom) else [alias.name for alias in node.names] if isinstance(node, ast.Import) else []
                for name in names:
                    if name and name.startswith("gideon."):
                        self.assertIn(name, modules, f"uncounted donor dependency in {path}")
                    if name and name.startswith(("openpyxl", "docx", "pptx", "reportlab")):
                        external.add(name.split(".")[0])
        self.assertEqual(external, {"openpyxl"})
        self.assertEqual(self.data["first_slice"]["writer"], "xlsx")
        self.assertNotIn("src/gideon/documents/model_codec.py", selected)

    def test_selected_skills_map_to_actual_keith_tool_ids(self) -> None:
        skills = self.data["first_slice"]["skills"]
        self.assertGreaterEqual(len(skills), 3)
        self.assertLessEqual(len(skills), 5)
        runtime = (ROOT / "crates/local-runtime/src/lib.rs").read_text()
        enabled_tools = set(re.findall(r'\("([a-z_-]+)"\.into\(\), ToolPermission::Allow\)', runtime))
        for skill in skills:
            self.assertTrue(skill["required_tools"])
            self.assertLessEqual(set(skill["required_tools"]), enabled_tools)
            source = (self.roots["core"] / skill["source"]).read_text()
            self.assertTrue(source.startswith("---\n"))
            self.assertIn(f"name: {skill['id']}\n", source)
        self.assertNotIn("web-verify", [skill["id"] for skill in skills])
        contract = (ROOT / "docs/features/skills.md").read_text()
        self.assertIn("TOML `+++`", contract)

    def test_keith_entrypoints_exist_without_claiming_runtime_behavior(self) -> None:
        for entry in self.data["keith_entrypoints"]:
            source = (ROOT / relative_path(entry["path"])).read_text()
            for anchor in entry["anchors"]:
                self.assertIn(anchor, source, f"review the changed owner: {entry['path']}")
        self.assertTrue(self.data["unverified"])
        for family in ["stateful-apps", "sync", "device"]:
            self.assertEqual(self.data["families"][family]["disposition"], "stateful_product_migration")

    def test_same_count_source_substitution_is_rejected(self) -> None:
        changed = copy.deepcopy(self.data)
        changed["files"][0]["sha256"] = "0" * 64
        with self.assertRaisesRegex(InventoryError, "source changed since review"):
            validate_snapshot(changed, self.roots)
        changed = copy.deepcopy(self.data)
        changed["files"][1] = copy.deepcopy(changed["files"][0])
        with self.assertRaisesRegex(InventoryError, "duplicate source"):
            validate_snapshot(changed, self.roots)

    def test_unsafe_paths_and_runtime_claim_laundering_are_rejected(self) -> None:
        for path in ["/etc/passwd", "../other/manifest.json", ""]:
            with self.assertRaises(InventoryError):
                relative_path(path)
        changed = copy.deepcopy(self.data)
        changed["runtime_qualified"] = True
        with self.assertRaisesRegex(InventoryError, "may not claim runtime qualification"):
            validate_snapshot(changed, self.roots)


if __name__ == "__main__":
    unittest.main()
