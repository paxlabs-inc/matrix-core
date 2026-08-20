from __future__ import annotations

import asyncio
import base64
import json
import os
import re
import time
import logging
from dataclasses import dataclass, field
from typing import AsyncIterable, Awaitable, Callable

import aiohttp

_SENTENCE_RE = re.compile(r"^(.*?[.!?…])(\s+)(.*)$", re.DOTALL)
_FLUSH_AT = 180
logger = logging.getLogger("neo.voice.bridge")


@dataclass
class _Run:
    intent_id: str
    queue: asyncio.Queue[tuple[str, object]] = field(default_factory=asyncio.Queue)
    buffer: str = ""
    spoken: str = ""
    paused: bool = False
    started_at: float = field(default_factory=time.monotonic)
    post_ms: int = 0
    first_delta_logged: bool = False


@dataclass
class _Pending:
    kind: str
    intent_id: str
    item_id: str
    question: str
    ask_kind: str = ""


class NeoBridge:
    def __init__(
        self,
        conversation_id: str,
        daemon_url: str | None = None,
        token: str | None = None,
        on_intent: Callable[[str], Awaitable[None]] | None = None,
    ) -> None:
        self.conversation_id = conversation_id
        self.base_url = (daemon_url or os.getenv("MATRIX_DAEMON_URL") or "http://127.0.0.1:8080").rstrip("/")
        self.token = token if token is not None else (os.getenv("MATRIX_DAEMON_TOKEN") or "")
        self.on_intent = on_intent
        self._session: aiohttp.ClientSession | None = None
        self._active: _Run | None = None
        self._pending: _Pending | None = None
        self._lock = asyncio.Lock()

    async def start(self) -> None:
        if self._session is None:
            self._session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=None, sock_read=None))

    async def aclose(self) -> None:
        if self._session is not None:
            await self._session.close()
            self._session = None

    def _headers(self) -> dict[str, str]:
        return {"Authorization": "Bearer " + self.token} if self.token else {}

    @property
    def busy(self) -> bool:
        return self._active is not None

    @property
    def awaiting_answer(self) -> bool:
        return self._pending is not None

    async def interrupt_active(self) -> None:
        run = self._active
        if run is None or self._session is None:
            return
        try:
            async with self._session.post(self.base_url + f"/intents/{run.intent_id}/stop", headers=self._headers()) as response:
                await response.read()
            await self._wait_terminal(run.intent_id)
            await self._wait_settled(run.intent_id)
        finally:
            if self._active is run:
                self._active = None

    async def _wait_terminal(self, intent_id: str) -> None:
        assert self._session is not None
        timeout = aiohttp.ClientTimeout(total=5)
        try:
            async with self._session.get(
                self.base_url + f"/events?intent_id={intent_id}&since=0",
                headers={**self._headers(), "Accept": "text/event-stream"},
                timeout=timeout,
            ) as response:
                if response.status != 200:
                    return
                async for raw in response.content:
                    line = raw.decode("utf-8", "replace").strip()
                    if not line.startswith("data:"):
                        continue
                    try:
                        event = json.loads(line[5:].strip())
                    except json.JSONDecodeError:
                        continue
                    if event.get("type") == "message.complete":
                        return
        except (aiohttp.ClientError, asyncio.TimeoutError):
            return

    async def _wait_settled(self, intent_id: str) -> None:
        assert self._session is not None
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                async with self._session.get(
                    self.base_url + f"/conversations/{self.conversation_id}",
                    headers=self._headers(),
                    timeout=aiohttp.ClientTimeout(total=1),
                ) as response:
                    if response.status == 200:
                        payload = await response.json()
                        if str(payload.get("live_run") or "") != intent_id:
                            return
            except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
                pass
            await asyncio.sleep(0.025)

    async def run_turn(self, wav: bytes) -> AsyncIterable[str]:
        if not wav:
            return
        started = time.monotonic()
        await self._touch_controller()
        if self._pending is not None:
            async for chunk in self._answer_pending(wav):
                yield chunk
            return
        async with self._lock:
            if self._active is not None:
                yield "I am stopping the previous reply and listening to this one."
                await self.interrupt_active()
            try:
                run = await self._submit(wav, started)
            except Exception:
                yield "Voice could not reach the agent. Text chat is still available."
                return
            self._active = run
        if self.on_intent is not None:
            await self.on_intent(run.intent_id)
        async for chunk in self._events(run):
            yield chunk

    async def _transcribe_answer(self, wav: bytes) -> str:
        assert self._session is not None
        key = (os.getenv("MIMO_API_KEY") or "").strip()
        if not key:
            raise RuntimeError("voice answer transcription is not configured")
        base = (os.getenv("MATRIX_MIMO_BASE") or "https://api.xiaomimimo.com/v1").rstrip("/")
        data_url = "data:audio/wav;base64," + base64.b64encode(wav).decode("ascii")
        body = {
            "model": "mimo-v2.5-asr",
            "messages": [{"role": "user", "content": [{"type": "input_audio", "input_audio": {"data": data_url}}]}],
            "asr_options": {"language": "auto"},
        }
        async with self._session.post(base + "/chat/completions", json=body, headers={"Authorization": "Bearer " + key}) as response:
            if response.status < 200 or response.status >= 300:
                raise RuntimeError("voice answer transcription is unavailable")
            payload = await response.json()
        try:
            return str(payload["choices"][0]["message"]["content"]).strip()
        except (KeyError, IndexError, TypeError):
            return ""

    async def _answer_pending(self, wav: bytes) -> AsyncIterable[str]:
        pending = self._pending
        run = self._active
        if pending is None or run is None or self._session is None:
            return
        try:
            transcript = await self._transcribe_answer(wav)
            if not transcript:
                raise RuntimeError("empty answer")
            if pending.kind == "gate":
                answer = transcript.strip().lower().strip(" .!,?")
                approved = answer in {"yes", "yeah", "yep", "approve", "approved", "confirm", "confirmed", "go ahead", "do it", "ok", "okay"}
                denied = answer in {"no", "nope", "deny", "denied", "cancel", "stop", "abort", "do not", "don't"}
                if not approved and not denied:
                    yield "I did not catch a clear yes or no. Please say yes to approve or no to deny."
                    return
                url = self.base_url + f"/intents/{pending.intent_id}/gates/{pending.item_id}/answer"
                body = {"approved": approved, "answer": transcript}
            else:
                url = self.base_url + f"/intents/{pending.intent_id}/asks/{pending.item_id}/answer"
                if pending.ask_kind == "confirm":
                    value = transcript.strip().lower().strip(" .!,?") in {"yes", "yeah", "yep", "confirm", "confirmed", "ok", "okay"}
                    body = {"confirmed": value}
                elif pending.ask_kind == "choose":
                    body = {"choice": transcript}
                elif pending.ask_kind == "input":
                    body = {"value": transcript}
                else:
                    yield "That decision must be completed in text chat."
                    return
            async with self._session.post(url, json=body, headers=self._headers()) as response:
                if response.status < 200 or response.status >= 300:
                    raise RuntimeError("voice answer was rejected")
                await response.read()
        except Exception:
            yield "I could not record that voice answer. Please use text chat for this decision."
            return
        self._pending = None
        run.paused = False
        async for chunk in self._events(run):
            yield chunk

    async def _submit(self, wav: bytes, started: float) -> _Run:
        assert self._session is not None
        form = aiohttp.FormData()
        form.add_field("file", wav, filename="utterance.wav", content_type="audio/wav")
        async with self._session.post(self.base_url + "/upload", data=form, headers=self._headers()) as response:
            if response.status not in (200, 201):
                raise RuntimeError("upload unavailable")
            upload = await response.json()
        ref = str(upload.get("url") or "")
        if not ref.startswith("/media/"):
            raise RuntimeError("upload returned no media reference")
        body = {"message": f"[attached audio: {ref}]", "conversation_id": self.conversation_id}
        async with self._session.post(self.base_url + "/chat", json=body, headers=self._headers()) as response:
            if response.status not in (200, 202):
                raise RuntimeError("chat unavailable")
            dispatch = await response.json()
        intent_id = str(dispatch.get("intent_id") or "")
        if not intent_id:
            raise RuntimeError("chat returned no intent")
        return _Run(intent_id, started_at=started, post_ms=round((time.monotonic() - started) * 1000))

    async def _events(self, run: _Run) -> AsyncIterable[str]:
        assert self._session is not None
        backoff = 0.25
        since = 0
        while self._active is run:
            try:
                url = self.base_url + f"/events?intent_id={run.intent_id}&since={since}"
                async with self._session.get(url, headers={**self._headers(), "Accept": "text/event-stream"}) as response:
                    if response.status != 200:
                        raise RuntimeError("events unavailable")
                    async for raw in response.content:
                        line = raw.decode("utf-8", "replace").strip()
                        if not line.startswith("data:"):
                            continue
                        try:
                            event = json.loads(line[5:].strip())
                        except json.JSONDecodeError:
                            continue
                        since = max(since, int(event.get("seq") or 0))
                        for chunk in self._fold(run, event):
                            yield chunk
                        if run.paused:
                            return
                        if self._active is not run:
                            return
                backoff = 0.25
            except asyncio.CancelledError:
                raise
            except Exception:
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 5.0)

    async def _touch_controller(self) -> None:
        url = (os.getenv("VOICE_CONTROLLER_URL") or "http://127.0.0.1:8791").rstrip("/")
        try:
            async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=1)) as session:
                async with session.post(url + "/touch", json={"conversation_id": self.conversation_id}) as response:
                    await response.read()
        except Exception:
            pass

    def _fold(self, run: _Run, event: dict) -> list[str]:
        fields = event.get("fields") or {}
        typ = event.get("type")
        out: list[str] = []
        if typ == "chat.delta" and fields.get("channel") == "content":
            if not run.first_delta_logged:
                run.first_delta_logged = True
                logger.info(
                    "voice.turn conversation=%s vad_to_post_ms=%d vad_to_first_delta_ms=%d",
                    self.conversation_id,
                    run.post_ms,
                    round((time.monotonic() - run.started_at) * 1000),
                )
            run.buffer += str(fields.get("text") or "")
            while True:
                match = _SENTENCE_RE.match(run.buffer)
                if match and len(match.group(1)) >= 12:
                    out.append(match.group(1).strip() + " ")
                    run.spoken += match.group(1).strip()
                    run.buffer = match.group(3)
                    continue
                if len(run.buffer) >= _FLUSH_AT and " " in run.buffer[:_FLUSH_AT]:
                    cut = run.buffer.rfind(" ", 0, _FLUSH_AT)
                    out.append(run.buffer[:cut].strip() + " ")
                    run.spoken += run.buffer[:cut].strip()
                    run.buffer = run.buffer[cut + 1 :]
                    continue
                break
        elif typ == "chat.assistant":
            final = str(fields.get("text") or "")
            tail = run.buffer.strip()
            if tail and tail not in run.spoken and final.endswith(tail):
                out.append(tail + " ")
            run.buffer = ""
        elif typ == "gate.invoked":
            self._pending = _Pending("gate", run.intent_id, str(fields.get("node_id") or ""), str(fields.get("question") or ""))
            run.paused = True
            out.append((self._pending.question or "This action needs approval.") + " Say yes to approve or no to deny.")
        elif typ == "ask.awaiting":
            self._pending = _Pending("ask", run.intent_id, str(fields.get("ask_id") or ""), "The agent needs more information.", str(fields.get("ask_kind") or ""))
            run.paused = True
            out.append("The agent needs more information. Say your answer now.")
        elif typ in {"task.completed", "task.failed", "message.complete"}:
            if run.buffer.strip():
                out.append(run.buffer.strip() + " ")
            run.buffer = ""
            if self._active is run:
                self._active = None
        return out
