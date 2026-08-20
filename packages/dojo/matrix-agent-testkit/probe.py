# Copyright © 2026 Sidiora Labs.
#
# probe.py — black-box client for the live Centra AI agent surface.
#
# It speaks the exact wire contract the deployed router + Neo front expose:
#   POST /chat  {"message","conversation_id?"}
#        -> 202 {"conversation_id","intent_id","events_url","poll_url","kind"}
#   GET  /events?intent_id=<id>[&access_token=<jwt>]   (text/event-stream)
#        -> data: {"seq","ts","phase","type","fields"} frames until
#           type=="message.complete" (terminal), then a final
#           type=="chat.assistant" fields.final==true carrying the answer.
#
# Cody is the same contract under the /cody prefix (/cody/chat, /cody/events),
# so `base_path` parameterises it.
#
# Everything is stdlib: urllib for POST + SSE, no external deps (mirrors the
# packages/dojo/agon posture).

from __future__ import annotations

import json
import os
import queue
import socket
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Optional
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError

# CORS Origin header sent with requests. Point at your own client origin via
# MATRIX_ORIGIN; defaults to localhost for local runs. No hardcoded endpoints.
ORIGIN = os.environ.get("MATRIX_ORIGIN", "http://localhost:3000")


@dataclass
class ToolCall:
    tool: str
    ok: bool
    running: bool


@dataclass
class Verdict:
    coverage: str = ""
    grounded: Optional[bool] = None
    certainty: Optional[float] = None
    decision: str = ""
    rationale: str = ""
    unverified_claims: list = field(default_factory=list)
    missing: list = field(default_factory=list)


@dataclass
class RunResult:
    """Everything one dispatched turn produced, parsed from the SSE stream."""

    case_id: str
    prompt: str
    conversation_id: str = ""
    intent_id: str = ""

    dispatch_status: int = 0
    dispatch_body: str = ""

    terminal_status: str = ""          # completed / failed / "" (none seen)
    final_answer: str = ""
    content_text: str = ""             # channel=content deltas, concatenated
    reasoning_text: str = ""           # channel=reasoning deltas, concatenated
    narrations: list = field(default_factory=list)  # ephemeral chat.assistant

    tools: list = field(default_factory=list)        # list[ToolCall]
    verdict: Verdict = field(default_factory=Verdict)

    events: list = field(default_factory=list)       # raw event dicts
    latency_s: float = 0.0
    transport_error: str = ""          # connect/read/timeout failure

    # ---- convenience views used by graders -------------------------------
    def all_text(self) -> str:
        parts = [self.final_answer, self.content_text, self.reasoning_text]
        parts.extend(self.narrations)
        return "\n".join(p for p in parts if p)

    def user_facing_text(self) -> str:
        """What the human actually sees: final answer, else content stream."""
        return self.final_answer or self.content_text

    def tool_names(self) -> list:
        return [t.tool for t in self.tools]

    def had_terminal(self) -> bool:
        return self.terminal_status != ""


def dispatch(base: str, token: str, message: str,
             conversation_id: Optional[str] = None,
             base_path: str = "") -> tuple[int, str, str]:
    """POST /chat. Returns (http_status, body, transport_error)."""
    payload: dict[str, Any] = {"message": message}
    if conversation_id:
        payload["conversation_id"] = conversation_id
    data = json.dumps(payload).encode("utf-8")
    req = Request(
        f"{base}{base_path}/chat",
        data=data,
        method="POST",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "Origin": ORIGIN,
            "Accept": "application/json",
        },
    )
    try:
        resp = urlopen(req, timeout=45)
        return resp.status, resp.read().decode("utf-8", "replace"), ""
    except HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), ""
    except (URLError, socket.timeout, TimeoutError) as e:
        return 0, "", f"dispatch: {e}"
    except Exception as e:  # noqa: BLE001 — surface any transport failure verbatim
        return 0, "", f"dispatch: {e}"


def follow_events(base: str, token: str, intent_id: str, base_path: str = "",
                  hard_timeout: float = 300.0, idle_timeout: float = 90.0,
                  post_terminal_grace: float = 4.0,
                  read_slice: float = 0.5,
                  connect_timeout: float = 35.0) -> tuple[list, str]:
    """Stream /events until terminal + grace, idle, or hard timeout.

    A background thread does BLOCKING, timeout-free line reads on the
    http.client response (which also de-chunks transfer-encoding for free)
    and pushes lines onto a queue. The main loop drains the queue with its
    own wall-clock deadlines. This avoids the urllib pitfall where a
    per-read socket timeout poisons the BufferedReader ("cannot read from
    timed out object") on the first slow inter-event gap — the bug that made
    a working agent look like an infra failure.
    """
    url = f"{base}{base_path}/events?intent_id={intent_id}"
    req = Request(url, headers={
        "Authorization": f"Bearer {token}",
        "Origin": ORIGIN,
        "Accept": "text/event-stream",
        "Cache-Control": "no-cache",
    })
    try:
        resp = urlopen(req, timeout=connect_timeout)
    except HTTPError as e:
        return [], f"events HTTP {e.code}: {e.read().decode('utf-8', 'replace')[:160]}"
    except (URLError, socket.timeout, TimeoutError) as e:
        return [], f"events connect: {e}"
    except Exception as e:  # noqa: BLE001
        return [], f"events connect: {e}"

    q: "queue.Queue[tuple[str, Any]]" = queue.Queue(maxsize=4096)
    stop = threading.Event()

    def reader() -> None:
        try:
            for raw in resp:  # blocking, de-chunked line iteration
                if stop.is_set():
                    break
                q.put(("line", raw))
        except Exception as e:  # noqa: BLE001 — closing the resp raises here
            if not stop.is_set():
                q.put(("err", str(e)))
        finally:
            q.put(("eof", None))

    th = threading.Thread(target=reader, daemon=True)
    th.start()

    events: list = []
    start = time.time()
    last_data = start
    terminal_at: Optional[float] = None
    err = ""
    try:
        while True:
            now = time.time()
            if now - start > hard_timeout:
                err = "hard_timeout"
                break
            if terminal_at is not None and now - terminal_at > post_terminal_grace:
                break
            if terminal_at is None and now - last_data > idle_timeout:
                err = "idle_timeout"
                break
            try:
                kind, val = q.get(timeout=read_slice)
            except queue.Empty:
                continue
            if kind == "eof":
                break
            if kind == "err":
                err = f"read: {val}"
                break
            last_data = time.time()
            s = val.decode("utf-8", "replace").strip()
            if not s or s.startswith(":"):  # blank line or ": heartbeat"
                continue
            if not s.startswith("data:"):
                continue
            try:
                ev = json.loads(s[5:].strip())
            except json.JSONDecodeError:
                continue
            events.append(ev)
            if ev.get("type") == "message.complete":
                terminal_at = time.time()
    finally:
        stop.set()
        try:
            resp.close()
        except Exception:  # noqa: BLE001
            pass
    return events, err


def _parse_events(events: list, res: RunResult) -> None:
    """Fold the raw SSE frames into the structured RunResult views."""
    tools_by_id: dict[str, ToolCall] = {}
    for ev in events:
        typ = ev.get("type", "")
        f = ev.get("fields") or {}
        if typ == "chat.delta":
            ch = f.get("channel", "")
            txt = f.get("text", "")
            if ch == "reasoning":
                res.reasoning_text += txt
            elif ch == "content":
                res.content_text += txt
        elif typ == "chat.assistant":
            txt = f.get("text", "")
            if f.get("final") is True:
                res.final_answer = txt
            elif f.get("ephemeral") is True:
                if txt:
                    res.narrations.append(txt)
            else:
                # A non-ephemeral, non-final assistant turn (e.g. completion
                # echo) — keep as a fallback answer if no final arrives.
                if txt and not res.final_answer:
                    res.final_answer = txt
        elif typ == "tool.step":
            tid = f.get("id", "") or f.get("tool", "")
            tc = tools_by_id.get(tid)
            if tc is None:
                tc = ToolCall(tool=f.get("tool", ""), ok=bool(f.get("ok")),
                              running=bool(f.get("running")))
                tools_by_id[tid] = tc
            else:
                tc.ok = bool(f.get("ok"))
                tc.running = bool(f.get("running"))
        elif typ == "cassandra.verdict":
            res.verdict.coverage = f.get("coverage", "")
            res.verdict.grounded = f.get("grounded")
            res.verdict.certainty = f.get("certainty")
            res.verdict.rationale = f.get("rationale", "")
            res.verdict.unverified_claims = f.get("unverified_claims") or []
            res.verdict.missing = f.get("missing") or []
        elif typ == "cassandra.gate":
            res.verdict.decision = f.get("decision", "")
        elif typ == "message.complete":
            res.terminal_status = f.get("status", "")
    res.tools = list(tools_by_id.values())
    if not res.final_answer:
        res.final_answer = res.content_text.strip()


def run_turn(base: str, token: str, case_id: str, message: str,
             conversation_id: Optional[str] = None, base_path: str = "",
             **follow_kw) -> RunResult:
    """Dispatch one message and follow its stream to completion."""
    res = RunResult(case_id=case_id, prompt=message)
    t0 = time.time()
    status, body, err = dispatch(base, token, message, conversation_id, base_path)
    res.dispatch_status = status
    res.dispatch_body = body[:2000]
    if err:
        res.transport_error = err
        res.latency_s = round(time.time() - t0, 2)
        return res
    try:
        d = json.loads(body)
    except json.JSONDecodeError:
        res.transport_error = f"dispatch non-JSON (status {status})"
        res.latency_s = round(time.time() - t0, 2)
        return res
    res.conversation_id = d.get("conversation_id", "")
    res.intent_id = d.get("intent_id", "")
    if not res.intent_id:
        # Could be an automatrix_wake or an error envelope; nothing to follow.
        res.transport_error = f"no intent_id in dispatch: {body[:200]}"
        res.latency_s = round(time.time() - t0, 2)
        return res
    events, ferr = follow_events(base, token, res.intent_id, base_path, **follow_kw)
    res.events = events
    if ferr:
        res.transport_error = ferr
    _parse_events(events, res)
    res.latency_s = round(time.time() - t0, 2)
    return res
