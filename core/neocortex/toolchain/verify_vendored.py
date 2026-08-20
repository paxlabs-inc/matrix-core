#!/usr/bin/env python3

import hashlib
import json
from pathlib import Path


def tree_digest(directory: Path) -> str:
    outer = hashlib.sha256()
    paths = (item for item in directory.rglob("*") if item.is_file())
    for path in sorted(paths, key=lambda item: item.relative_to(directory).as_posix()):
        inner = hashlib.sha256(path.read_bytes()).hexdigest()
        relative = path.relative_to(directory).as_posix()
        outer.update(f"{inner}  {relative}\n".encode())
    return outer.hexdigest()


root = Path(__file__).resolve().parents[1]
lock = json.loads((root / "third_party" / "LOCK.json").read_text())
for dependency in lock["dependencies"]:
    actual = tree_digest(root / "third_party" / dependency["directory"])
    expected = dependency["tree_sha256"]
    if actual != expected:
        raise SystemExit(f"{dependency['name']}: tree digest {actual} != {expected}")
    print(f"{dependency['name']}: {actual}")
