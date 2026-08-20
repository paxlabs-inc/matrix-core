#!/usr/bin/env python3

import hashlib
import json
from pathlib import Path


root = Path(__file__).resolve().parent / "sysroots" / "aarch64-linux-gnu"
lock = json.loads((root / "LOCK.json").read_text())
outer = hashlib.sha256()
paths = (path for path in root.rglob("*") if path.is_file() and path.name != "LOCK.json")
for path in sorted(paths, key=lambda item: item.relative_to(root).as_posix()):
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    relative = path.relative_to(root).as_posix()
    outer.update(f"{digest}  {relative}\n".encode())
actual = outer.hexdigest()
expected = lock["tree_sha256_without_lock"]
if actual != expected:
    raise SystemExit(f"arm64 sysroot digest {actual} != {expected}")
print(f"arm64 sysroot: {actual}")
