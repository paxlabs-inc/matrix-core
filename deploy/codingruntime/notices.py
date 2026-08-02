#!/usr/bin/env python3
from __future__ import annotations

import importlib.metadata
import json
import sys
from pathlib import Path


LICENSE_NAMES = {"license", "license.txt", "license.md", "copying", "notice"}


def python_notices() -> list[tuple[str, str, list[str]]]:
    rows = []
    for dist in sorted(importlib.metadata.distributions(), key=lambda item: (item.metadata.get("Name") or "").lower()):
        name = dist.metadata.get("Name") or "unknown"
        version = dist.version or "unknown"
        license_name = dist.metadata.get("License-Expression") or dist.metadata.get("License") or "unspecified"
        texts = []
        for file in dist.files or []:
            if Path(str(file)).name.lower() not in LICENSE_NAMES:
                continue
            located = Path(dist.locate_file(file))
            try:
                texts.append(located.read_text(encoding="utf-8", errors="replace").strip())
            except OSError:
                continue
        rows.append((f"Python: {name} {version}", license_name, sorted(set(texts))))
    return rows


def node_notices(root: Path) -> list[tuple[str, str, list[str]]]:
    rows = []
    seen = set()
    for package_json in sorted((root / "node_modules").glob("**/package.json")):
        if "node_modules/.bin" in str(package_json):
            continue
        try:
            data = json.loads(package_json.read_text(encoding="utf-8"))
        except Exception:
            continue
        key = (str(data.get("name") or "unknown"), str(data.get("version") or "unknown"))
        if key in seen:
            continue
        seen.add(key)
        license_name = data.get("license") or data.get("licenses") or "unspecified"
        if not isinstance(license_name, str):
            license_name = json.dumps(license_name, sort_keys=True)
        texts = []
        for child in package_json.parent.iterdir():
            if child.is_file() and child.name.lower() in LICENSE_NAMES:
                texts.append(child.read_text(encoding="utf-8", errors="replace").strip())
        rows.append((f"Node: {key[0]} {key[1]}", license_name, sorted(set(texts))))
    return rows


def main() -> None:
    root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path.cwd()
    output = Path(sys.argv[2]) if len(sys.argv) > 2 else Path("THIRD_PARTY_NOTICES.txt")
    lines = ["Matrix AgentCore third-party notices", ""]
    for label, license_name, texts in python_notices() + node_notices(root):
        lines.extend([label, f"Declared license: {license_name}"])
        for text in texts:
            lines.extend(["", text])
        lines.extend(["", "=" * 72, ""])
    output.write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    main()

