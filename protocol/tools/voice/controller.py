from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Callable

_VOICES = {"mimo_default", "冰糖", "茉莉", "苏打", "白桦", "Mia", "Chloe", "Milo", "Dean"}


def _positive_seconds(name: str, default: float) -> float:
    try:
        value = float(os.getenv(name, str(default)))
    except ValueError:
        return default
    return value if value > 0 else default


class Supervisor:
    def __init__(self, command: Callable[[str], list[str]] | None = None) -> None:
        self._command = command or self._worker_command
        self._lock = threading.Lock()
        self._process: subprocess.Popen[bytes] | None = None
        self._conversation = ""
        self._last_activity = 0.0
        self._idle_seconds = _positive_seconds("VOICE_IDLE_DISCONNECT_S", 120.0)
        self._closed = threading.Event()
        self._watchdog = threading.Thread(target=self._watch, name="voice-idle", daemon=True)
        self._watchdog.start()

    @staticmethod
    def _worker_command(conversation: str) -> list[str]:
        url = (os.getenv("MATRIX_LIVEKIT_URL") or "").strip()
        key = (os.getenv("MATRIX_LIVEKIT_KEY") or "").strip()
        secret = (os.getenv("MATRIX_LIVEKIT_SECRET") or "").strip()
        if not url or not key or not secret:
            raise RuntimeError("voice transport is not configured")
        return [
            sys.executable,
            str(Path(__file__).with_name("agent.py")),
            "connect",
            "--room",
            "voice:" + conversation,
            "--participant-identity",
            "neo-voice",
        ]

    @staticmethod
    def _worker_env(voice: str = "", style: str = "") -> dict[str, str]:
        env = os.environ.copy()
        env["LIVEKIT_URL"] = (os.getenv("MATRIX_LIVEKIT_URL") or "").strip()
        env["LIVEKIT_API_KEY"] = (os.getenv("MATRIX_LIVEKIT_KEY") or "").strip()
        env["LIVEKIT_API_SECRET"] = (os.getenv("MATRIX_LIVEKIT_SECRET") or "").strip()
        if voice:
            env["NEO_VOICE_TTS_VOICE"] = voice
        if style:
            env["NEO_VOICE_TTS_STYLE"] = style
        return env

    def start(self, conversation: str, voice: str = "", style: str = "") -> dict[str, object]:
        if not conversation or any(ch not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-" for ch in conversation):
            raise ValueError("invalid conversation")
        voice = voice.strip()
        style = style.strip()
        if (voice and voice not in _VOICES) or len(style) > 500:
            raise ValueError("invalid voice settings")
        exited = ""
        stopped = ""
        with self._lock:
            if self._process is not None and self._process.poll() is not None:
                exited = self._conversation
                self._process = None
                self._conversation = ""
            if self._process is not None and self._conversation != conversation:
                stopped = self._conversation
                self._terminate_locked()
            if self._process is None:
                self._process = subprocess.Popen(
                    self._command(conversation),
                    env=self._worker_env(voice, style),
                    start_new_session=True,
                )
                self._conversation = conversation
            self._last_activity = time.monotonic()
            state = self.state_locked()
        if exited:
            self._notify(exited, "stop", "worker_exited")
        if stopped:
            self._notify(stopped, "stop", "replaced")
        return state

    def touch(self, conversation: str) -> dict[str, object]:
        with self._lock:
            if self._process is None or self._conversation != conversation or self._process.poll() is not None:
                raise LookupError("voice session is not active")
            self._last_activity = time.monotonic()
            return self.state_locked()

    def stop(self, conversation: str = "", reason: str = "requested") -> dict[str, object]:
        with self._lock:
            if conversation and self._conversation and self._conversation != conversation:
                raise LookupError("voice session is not active")
            conversation = self._conversation
            self._terminate_locked()
            state = self.state_locked()
        if conversation and reason != "requested":
            self._notify(conversation, "stop", reason)
        return state

    def state(self) -> dict[str, object]:
        exited = ""
        with self._lock:
            if self._process is not None and self._process.poll() is not None:
                exited = self._conversation
                self._process = None
                self._conversation = ""
            state = self.state_locked()
        if exited:
            self._notify(exited, "stop", "worker_exited")
        return state

    def state_locked(self) -> dict[str, object]:
        active = self._process is not None
        return {"active": active, "conversation_id": self._conversation if active else ""}

    def close(self) -> None:
        self._closed.set()
        self.stop(reason="shutdown")

    def _terminate_locked(self) -> None:
        process = self._process
        self._process = None
        self._conversation = ""
        self._last_activity = 0.0
        if process is None or process.poll() is not None:
            return
        try:
            os.killpg(process.pid, signal.SIGTERM)
            process.wait(timeout=5)
        except (ProcessLookupError, subprocess.TimeoutExpired):
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=2)

    def _watch(self) -> None:
        while not self._closed.wait(min(1.0, self._idle_seconds / 4)):
            with self._lock:
                exited = self._process is not None and self._process.poll() is not None
                exited_conversation = self._conversation if exited else ""
                if exited:
                    self._process = None
                    self._conversation = ""
                    self._last_activity = 0.0
                expired = self._process is not None and time.monotonic() - self._last_activity >= self._idle_seconds
                conversation = self._conversation if expired else ""
                if expired:
                    self._terminate_locked()
            if exited_conversation:
                self._notify(exited_conversation, "stop", "worker_exited")
            if conversation:
                self._notify(conversation, "stop", "idle")

    @staticmethod
    def _notify(conversation: str, event: str, state: str) -> None:
        base = (os.getenv("MATRIX_NEO_URL") or "http://127.0.0.1:8080").rstrip("/")
        body = json.dumps({"conversation_id": conversation, "event": event, "state": state}).encode()
        try:
            request = urllib.request.Request(base + "/voice/session/event", data=body, headers={"Content-Type": "application/json"})
            urllib.request.urlopen(request, timeout=2).close()
        except Exception:
            pass


class Handler(BaseHTTPRequestHandler):
    supervisor: Supervisor

    def do_GET(self) -> None:
        if self.path != "/state":
            self.send_error(404)
            return
        self._write(200, self.supervisor.state())

    def do_POST(self) -> None:
        try:
            length = min(int(self.headers.get("Content-Length", "0")), 4096)
            payload = json.loads(self.rfile.read(length) or b"{}")
            conversation = str(payload.get("conversation_id") or "")
            if self.path == "/start":
                result = self.supervisor.start(
                    conversation,
                    str(payload.get("voice") or ""),
                    str(payload.get("style") or ""),
                )
            elif self.path == "/touch":
                result = self.supervisor.touch(conversation)
            elif self.path == "/stop":
                result = self.supervisor.stop(conversation)
            else:
                self.send_error(404)
                return
        except (ValueError, LookupError, RuntimeError):
            self._write(409, {"error": "voice session unavailable"})
            return
        self._write(200, result)

    def _write(self, status: int, payload: dict[str, object]) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: object) -> None:
        return


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8791)
    args = parser.parse_args()
    supervisor = Supervisor()
    Handler.supervisor = supervisor
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    try:
        server.serve_forever()
    finally:
        supervisor.close()


if __name__ == "__main__":
    main()
