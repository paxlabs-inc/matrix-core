#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import os
import sys
from pathlib import Path


EXCLUDED_PARTS = {
    ".git", ".venv", "__pycache__", "node_modules", ".pytest_cache",
    ".uv-cache", ".pip-cache", "dist", "build",
}


def main() -> None:
    root = Path(sys.argv[1]).resolve()
    digest = hashlib.sha256()
    paths = []
    for path in root.rglob("*"):
        relative = path.relative_to(root)
        if any(part in EXCLUDED_PARTS for part in relative.parts):
            continue
        if path.is_file() or path.is_symlink():
            paths.append((relative, path))
    for relative, path in sorted(paths, key=lambda item: item[0].as_posix()):
        encoded = relative.as_posix().encode()
        digest.update(len(encoded).to_bytes(4, "big"))
        digest.update(encoded)
        if path.is_symlink():
            content = os.readlink(path).encode()
            mode = b"l"
        else:
            content = path.read_bytes()
            mode = b"x" if os.access(path, os.X_OK) else b"f"
        digest.update(mode)
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
    print(digest.hexdigest())


if __name__ == "__main__":
    main()

