#!/usr/bin/env python3
"""MCP stdio end-to-end exerciser for tachyond."""
import json
import subprocess
import sys


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: mcp_e2e.py /path/to/tachyond", file=sys.stderr)
        return 2
    bin_path = sys.argv[1]
    p = subprocess.Popen(
        [bin_path, "--mcp"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
        bufsize=1,
    )

    def send(obj):
        p.stdin.write(json.dumps(obj) + "\n")
        p.stdin.flush()
        # Notifications have no JSON-RPC response.
        if obj.get("method", "").startswith("notifications/"):
            return None
        if "id" not in obj:
            return None
        line = p.stdout.readline()
        if not line:
            raise RuntimeError("mcp closed stdout")
        return json.loads(line)

    send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2024-11-05"}})
    send({"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}})
    tools = send({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
    n = len(tools["result"]["tools"])
    assert n >= 9, tools

    compile_resp = send(
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": "tachyon_compile", "arguments": {"targets": ["Create2", "BridgeERC20"]}},
        }
    )
    compile_body = json.loads(compile_resp["result"]["content"][0]["text"])
    assert compile_body.get("ok"), compile_body

    test_resp = send(
        {
            "jsonrpc": "2.0",
            "id": 4,
            "method": "tools/call",
            "params": {"name": "tachyon_test", "arguments": {"match_path": "test/utils/Create2.t.sol"}},
        }
    )
    test_body = json.loads(test_resp["result"]["content"][0]["text"])
    assert test_body.get("ok"), test_body

    pax_rpc = "https://public-mainnet.rpcpaxeer.online/evm"
    sim_resp = send(
        {
            "jsonrpc": "2.0",
            "id": 5,
            "method": "tools/call",
            "params": {
                "name": "tachyon_simulate",
                "arguments": {
                    "rpc_url": pax_rpc,
                    "to": "0x0000000000000000000000000000000000000000",
                    "data": "0x",
                },
            },
        }
    )
    sim_body = json.loads(sim_resp["result"]["content"][0]["text"])
    assert sim_body.get("ok"), sim_body

    chain_resp = send(
        {"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": {"name": "tachyon_chain_list", "arguments": {}}}
    )
    chain_body = json.loads(chain_resp["result"]["content"][0]["text"])
    assert chain_body.get("ok"), chain_body

    p.terminate()
    print(f"mcp_ok {n} tools compile test simulate chains")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"mcp_fail {exc}", file=sys.stderr)
        raise SystemExit(1)
