#!/usr/bin/env python3
from __future__ import annotations

import argparse
import http.client
import json
import os
import select
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from websockets.exceptions import ConnectionClosed, InvalidStatus
from websockets.sync.client import connect


class RpcClient:
    def __init__(self, socket):
        self.socket = socket
        self.next_id = 1
        self.events: list[dict] = []

    def request(self, method: str, params: dict, timeout: float = 30) -> dict:
        request_id = self.next_id
        self.next_id += 1
        self.socket.send(json.dumps({
            "jsonrpc": "2.0", "id": request_id, "method": method, "params": params,
        }))
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            frame = self._receive(deadline - time.monotonic())
            if frame.get("id") == request_id:
                if frame.get("error"):
                    raise RuntimeError(f"{method}: {frame['error']}")
                result = frame.get("result")
                return result if isinstance(result, dict) else {"value": result}
            self._buffer_event(frame)
        raise TimeoutError(f"timed out waiting for {method}")

    def wait_event(self, event_type: str, session_id: str, timeout: float = 180) -> dict:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for index, frame in enumerate(self.events):
                params = frame.get("params") or {}
                if params.get("type") == event_type and params.get("session_id") == session_id:
                    return self.events.pop(index)
            frame = self._receive(deadline - time.monotonic())
            self._buffer_event(frame)
        raise TimeoutError(f"timed out waiting for {event_type}")

    def _receive(self, timeout: float) -> dict:
        raw = self.socket.recv(timeout=max(0.1, timeout))
        return json.loads(raw)

    def _buffer_event(self, frame: dict) -> None:
        if frame.get("method") == "event":
            self.events.append(frame)


def start_server(workspace: Path) -> tuple[subprocess.Popen, int]:
    process = subprocess.Popen(
        ["agentcore", "serve", "--host", "127.0.0.1", "--port", "0", "--isolated"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
        start_new_session=True,
        cwd=workspace,
    )
    assert process.stdout is not None
    deadline = time.monotonic() + 90
    output = []
    while time.monotonic() < deadline:
        readable, _, _ = select.select([process.stdout], [], [], 0.5)
        if not readable:
            if process.poll() is not None:
                break
            continue
        line = process.stdout.readline()
        if line == "":
            if process.poll() is not None:
                break
            continue
        output.append(line)
        if "AGENTCORE_BACKEND_READY port=" in line:
            port = int(line.rsplit("port=", 1)[1].strip())
            return process, port
    if process.poll() is None:
        process.terminate()
        process.wait(timeout=10)
    raise RuntimeError("AgentCore did not become ready: " + "".join(output[-20:]))


def health(port: int) -> None:
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    connection.request("GET", "/api/health")
    response = connection.getresponse()
    body = response.read()
    connection.close()
    if response.status != 200 or not body:
        raise RuntimeError(f"health failed: HTTP {response.status}")


def assert_auth(port: int, token: str) -> None:
    try:
        bad = connect(
            f"ws://127.0.0.1:{port}/api/ws?token=wrong-{token}",
            origin=f"http://127.0.0.1:{port}",
            open_timeout=5,
        )
    except InvalidStatus as exc:
        if exc.response.status_code != 403:
            raise RuntimeError(
                f"invalid token handshake returned HTTP {exc.response.status_code}"
            ) from exc
        return
    try:
        bad.recv(timeout=5)
        raise RuntimeError("invalid WebSocket token was accepted")
    except ConnectionClosed as exc:
        if exc.rcvd is None or exc.rcvd.code != 4401:
            raise RuntimeError(f"invalid token closed with unexpected code: {exc}") from exc
    finally:
        bad.close()


def event_tool_name(frame: dict) -> str:
    payload = (frame.get("params") or {}).get("payload") or {}
    for key in ("name", "tool", "tool_name", "function"):
        value = payload.get(key)
        if isinstance(value, str):
            return value
        if isinstance(value, dict) and isinstance(value.get("name"), str):
            return value["name"]
    return ""


def wait_turn(client: RpcClient, session_id: str, timeout: float = 240) -> tuple[dict, set[str]]:
    deadline = time.monotonic() + timeout
    tools = set()
    while time.monotonic() < deadline:
        frame = client.wait_event("message.complete", session_id, timeout=deadline - time.monotonic())
        for queued in client.events:
            params = queued.get("params") or {}
            if params.get("session_id") == session_id and params.get("type") in {"tool.start", "tool.complete"}:
                name = event_tool_name(queued)
                if name:
                    tools.add(name)
        return frame, tools
    raise TimeoutError("turn did not complete")


def scan_for_credential(paths: list[Path], credential: str) -> None:
    needle = credential.encode()
    for root in paths:
        for path in root.rglob("*"):
            if not path.is_file():
                continue
            try:
                if needle in path.read_bytes():
                    raise RuntimeError(f"scoped gateway credential persisted in {path}")
            except OSError:
                continue


def run_full(client: RpcClient, workspace: Path) -> None:
    created = client.request("session.create", {
        "cwd": str(workspace),
        "source": "codingruntime-smoke",
        "title": "Matrix coding runtime smoke",
        "cols": 100,
        "close_on_disconnect": False,
        "model": "mimo-v2.5-pro",
        "provider": "xiaomi",
    })
    live_id = created["session_id"]
    stored_id = created["stored_session_id"]
    client.request("prompt.submit", {
        "session_id": live_id,
        "text": (
            "Use the native patch tool in replace mode to change the exact text alpha to beta "
            "in smoke.txt. Then use the native terminal tool to run a command that reads "
            "smoke.txt and exits nonzero unless its exact content is beta. Do not use any other "
            "editing or execution mechanism."
        ),
    })
    completed, tools = wait_turn(client, live_id)
    status = ((completed.get("params") or {}).get("payload") or {}).get("status")
    if status not in {"complete", "completed", "success"}:
        raise RuntimeError(f"edit turn ended with status {status!r}")
    if not {"patch", "terminal"}.issubset(tools):
        raise RuntimeError(f"required native tools were not observed: {sorted(tools)}")
    if (workspace / "smoke.txt").read_text(encoding="utf-8") != "beta":
        raise RuntimeError("native patch did not produce the expected file")

    client.request("session.close", {"session_id": live_id})
    resumed = client.request("session.resume", {"session_id": stored_id, "cols": 100}, timeout=90)
    resumed_live = resumed["session_id"]
    if resumed.get("resumed") != stored_id or not resumed.get("messages"):
        raise RuntimeError("durable session did not resume its prior history")

    client.request("prompt.submit", {
        "session_id": resumed_live,
        "text": (
            "Use the native terminal tool in the foreground to run exactly: "
            "python -c 'import time; time.sleep(45)'. Do not finish before the command returns."
        ),
    })
    while True:
        event = client.wait_event("tool.start", resumed_live, timeout=90)
        if event_tool_name(event) == "terminal":
            break
    interrupted = client.request("session.interrupt", {"session_id": resumed_live}, timeout=30)
    if interrupted.get("status") != "interrupted":
        raise RuntimeError(f"interrupt returned {interrupted}")
    terminal = client.wait_event("message.complete", resumed_live, timeout=60)
    terminal_status = ((terminal.get("params") or {}).get("payload") or {}).get("status")
    if terminal_status != "interrupted":
        raise RuntimeError(f"interrupted turn ended with {terminal_status!r}")
    client.request("session.close", {"session_id": resumed_live})


def run_protocol_only(client: RpcClient, workspace: Path) -> None:
    created = client.request("session.create", {"cwd": str(workspace), "source": "codingruntime-smoke"})
    live_id = created["session_id"]
    patched = client.request("shell.exec", {
        "command": "sed -i -- 's/alpha/beta/' smoke.txt",
    })
    checked = client.request("shell.exec", {
        "command": "grep -qx -- beta smoke.txt",
    })
    if patched.get("code") != 0 or checked.get("code") != 0:
        raise RuntimeError(f"shell RPC failed: patch={patched} check={checked}")
    client.request("session.close", {"session_id": live_id})


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--protocol-only", action="store_true")
    args = parser.parse_args()

    token = os.environ["AGENTCORE_DASHBOARD_SESSION_TOKEN"]
    workspace = Path(os.environ["AGENTCORE_WRITE_SAFE_ROOT"])
    home = Path(os.environ["AGENTCORE_HOME"])
    workspace.mkdir(parents=True, exist_ok=True)
    home.mkdir(parents=True, exist_ok=True)
    (workspace / "smoke.txt").write_text("alpha", encoding="utf-8")

    process, port = start_server(workspace)
    try:
        health(port)
        assert_auth(port, token)
        with connect(
            f"ws://127.0.0.1:{port}/api/ws?token={token}",
            origin=f"http://127.0.0.1:{port}",
            open_timeout=10,
        ) as socket:
            ready = json.loads(socket.recv(timeout=10))
            if (ready.get("params") or {}).get("type") != "gateway.ready":
                raise RuntimeError(f"first WebSocket frame was not gateway.ready: {ready}")
            client = RpcClient(socket)
            if args.protocol_only:
                run_protocol_only(client, workspace)
            else:
                run_full(client, workspace)
        if not args.protocol_only:
            scan_for_credential([home, workspace], os.environ["XIAOMI_API_KEY"])
    finally:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            return_code = process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
            raise RuntimeError("AgentCore did not stop within ten seconds")
        if return_code not in {0, -signal.SIGTERM} and sys.exc_info()[0] is None:
            raise RuntimeError(f"AgentCore exited with {return_code}")

    print("matrix-agentcore artifact smoke ok", flush=True)


if __name__ == "__main__":
    main()
