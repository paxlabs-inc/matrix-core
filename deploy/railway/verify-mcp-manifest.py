#!/usr/bin/env python3
# Copyright © 2026 Sidiora Labs. All rights reserved.
# SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
# Contact · license@Paxeer.app · legal@Paxeer.app

"""Manifest spawn gate for the per-user daemon image.

For every uvx-launched MCP server declared in the given agent manifests, spawn
the EXACT command the daemon will spawn, run the MCP initialize handshake, and
assert that tools/list returns exactly the tool names the manifest advertises.

Why this exists
---------------
The Python MCP servers pin themselves but NOT the `mcp` SDK they import
(mcp-server-fetch declares `mcp>=1.1.3`, mcp-server-git `mcp>=1.0.0` — both
unbounded above). On 2026-07-29 the freshly released `mcp` 2.0.0 renamed
McpError -> MCPError; mcp-server-fetch 2025.4.7 then died with ImportError at
startup, the daemon's spawn of "fetch" hit its initialize deadline,
mcl-execute exited 1, and the entrypoint stopped the co-located pair — a boot
crash loop with no prior warning.

The manifests now pin the SDK explicitly via `uvx --with mcp==<version>`. This
script makes that pin load-bearing: if a pin is dropped, drifts, or resolves to
something whose import breaks, the IMAGE BUILD fails instead of production.

It also enforces the standing pin discipline — a server whose tools/list no
longer matches its manifest entry is a silent capability drift, so it fails
here too.

Run as the agent uid so resolution happens in the same cache the daemon's
subprocesses use (which additionally warms that cache into the image).
"""

import json
import os
import subprocess
import sys
import tempfile
import threading

INIT_TIMEOUT_S = 180


def probe(command: str, args: list[str]) -> list[str]:
    """Spawn one MCP stdio server and return its sorted tools/list names.

    stdin is held OPEN until the tools/list reply arrives. Closing it right
    after the writes (what subprocess.run does) races the server: some servers
    see EOF and exit before answering, which made this check flaky.
    """
    handshake = (
        json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {},
                    "clientInfo": {"name": "image-build-gate", "version": "0"},
                },
            }
        )
        + "\n"
        + json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"})
        + "\n"
        + json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
        + "\n"
    )

    proc = subprocess.Popen(
        [command, *args],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )

    result: dict[str, object] = {}

    def reader() -> None:
        assert proc.stdout is not None
        for line in proc.stdout:
            try:
                msg = json.loads(line)
            except ValueError:
                continue
            if msg.get("id") == 2 and "result" in msg:
                result["tools"] = sorted(
                    t["name"] for t in msg["result"].get("tools", [])
                )
                return

    thread = threading.Thread(target=reader, daemon=True)
    thread.start()
    try:
        assert proc.stdin is not None
        proc.stdin.write(handshake)
        proc.stdin.flush()
        thread.join(INIT_TIMEOUT_S)
    finally:
        proc.kill()
        try:
            _, stderr = proc.communicate(timeout=20)
        except subprocess.TimeoutExpired:
            stderr = ""

    if "tools" in result:
        return result["tools"]  # type: ignore[return-value]

    raise RuntimeError(
        f"no tools/list response within {INIT_TIMEOUT_S}s\n"
        f"  stderr={(stderr or '').strip()[-1500:]}"
    )


def materialise_repo_args(args: list[str], scratch: str) -> list[str]:
    """Point --repository at a throwaway git repo.

    The manifest's real value (/workspace) is a runtime symlink the entrypoint
    creates; it does not exist during the image build. This gate validates the
    SERVER — that it imports against the pinned SDK and advertises the declared
    tools — not the repo path, so a temp repo is the right substitution.
    """
    if "--repository" not in args:
        return args
    repo = os.path.join(scratch, "probe-repo")
    os.makedirs(repo, exist_ok=True)
    subprocess.run(
        ["git", "init", "--quiet", repo],
        check=True,
        capture_output=True,
    )
    out = list(args)
    out[out.index("--repository") + 1] = repo
    return out


def main(paths: list[str]) -> int:
    failures: list[str] = []
    checked = 0

    for path in paths:
        with open(path) as fh:
            manifest = json.load(fh)

        for server in manifest.get("servers", []):
            if server.get("command") != "uvx":
                continue

            alias = server.get("alias", "?")
            label = f"{path}:{alias}"
            declared = sorted(t["name"] for t in server.get("tools", []))

            # The pin is the whole point of this gate — require it explicitly.
            args = server.get("args", [])
            if "--with" not in args or not any(a.startswith("mcp==") for a in args):
                failures.append(
                    f"{label}: no `--with mcp==<version>` pin; an unbounded "
                    f"transitive SDK can break this server at any time"
                )
                continue

            try:
                with tempfile.TemporaryDirectory() as scratch:
                    actual = probe(
                        server["command"], materialise_repo_args(args, scratch)
                    )
            except Exception as exc:  # noqa: BLE001 - report any spawn failure
                failures.append(f"{label}: spawn failed: {exc}")
                continue

            checked += 1
            if actual != declared:
                failures.append(
                    f"{label}: tools/list drift\n"
                    f"    manifest declares: {declared}\n"
                    f"    server returned:   {actual}"
                )
            else:
                print(f"  ok  {label}  ({len(actual)} tools)")

    if failures:
        print("\nMCP manifest gate FAILED:\n", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    print(f"\nMCP manifest gate passed ({checked} uvx servers verified)")
    return 0


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: verify-mcp-manifest.py <manifest.json> [...]", file=sys.stderr)
        raise SystemExit(2)
    raise SystemExit(main(sys.argv[1:]))
