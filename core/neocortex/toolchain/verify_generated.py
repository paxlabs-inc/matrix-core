#!/usr/bin/env python3

import hashlib
from pathlib import Path
import sys


EXPECTED = {
    "schema/events.fbs": "29899f4e375925a35461be68d56931b2208971bd82f46d64785d2aff7e3376e3",
    "src/schema/events_generated.h": "a0c99092cec6ef9a30b9dc027b992d0427617c86d3ccf366056ae73b0a0c8869",
    "schema/protocol.fbs": "faad17b5b0c13ced9e8f9c4efd5ebbe015a98dc2af4509a90ce76e7c6e642f57",
    "src/schema/protocol_generated.h": "b2aecbb9cf0cd9229ac64a5ac35079b5cde6ce3ad1e7a790e06cd16c7803ab4a",
}


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    for relative, expected in EXPECTED.items():
        actual = hashlib.sha256((root / relative).read_bytes()).hexdigest()
        if actual != expected:
            print(f"generated schema drift: {relative}: {actual} != {expected}", file=sys.stderr)
            return 1
        print(f"{relative}: {actual}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
