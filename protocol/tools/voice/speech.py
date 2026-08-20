from __future__ import annotations

import asyncio
import base64
import json
import os
import logging
import time
import uuid
from typing import Any

import aiohttp
from livekit import rtc
from livekit.agents import APIConnectOptions, DEFAULT_API_CONNECT_OPTIONS, NOT_GIVEN, stt, tts

logger = logging.getLogger("neo.voice.speech")


class PassthroughSTT(stt.STT):
    def __init__(self) -> None:
        super().__init__(capabilities=stt.STTCapabilities(streaming=False, interim_results=False))
        self._audio: dict[str, bytes] = {}

    async def _recognize_impl(
        self,
        buffer,
        *,
        language=NOT_GIVEN,
        conn_options: APIConnectOptions,
    ) -> stt.SpeechEvent:
        del language, conn_options
        frame = rtc.combine_audio_frames(buffer)
        token = "voice-audio:" + uuid.uuid4().hex
        self._audio[token] = frame.to_wav_bytes()
        return stt.SpeechEvent(
            type=stt.SpeechEventType.FINAL_TRANSCRIPT,
            alternatives=[stt.SpeechData(language="", text=token)],
        )

    def take(self, token: str) -> bytes:
        return self._audio.pop(token, b"")


def tts_body(text: str, voice: str, style: str) -> dict[str, Any]:
    messages: list[dict[str, str]] = []
    if style.strip():
        messages.append({"role": "user", "content": style.strip()})
    messages.append({"role": "assistant", "content": text})
    return {
        "model": "mimo-v2.5-tts",
        "messages": messages,
        "audio": {"format": "pcm16", "voice": voice},
        "stream": True,
    }


def audio_from_sse(payload: str) -> bytes:
    if payload == "[DONE]":
        return b""
    try:
        obj = json.loads(payload)
        encoded = obj["choices"][0]["delta"]["audio"]["data"]
        return base64.b64decode(encoded, validate=True)
    except (KeyError, IndexError, TypeError, ValueError, json.JSONDecodeError):
        return b""


class _MimoChunkedStream(tts.ChunkedStream):
    def __init__(self, owner: "MimoTTS", text: str, conn_options: APIConnectOptions) -> None:
        super().__init__(tts=owner, input_text=text, conn_options=conn_options)
        self._owner = owner

    async def _run(self, output_emitter: tts.AudioEmitter) -> None:
        started = time.monotonic()
        first = True
        output_emitter.initialize(
            request_id=uuid.uuid4().hex,
            sample_rate=24000,
            num_channels=1,
            mime_type="audio/pcm",
            stream=False,
        )
        timeout = aiohttp.ClientTimeout(total=None, sock_connect=self._owner.deadline, sock_read=self._owner.deadline)
        async with aiohttp.ClientSession(timeout=timeout) as session:
            async with session.post(
                self._owner.endpoint,
                json=tts_body(self.input_text, self._owner.voice, self._owner.style),
                headers={"Authorization": "Bearer " + self._owner.api_key},
            ) as response:
                if response.status < 200 or response.status >= 300:
                    raise RuntimeError("MiMo speech synthesis is unavailable")
                async for raw in response.content:
                    line = raw.decode("utf-8", "replace").strip()
                    if not line.startswith("data:"):
                        continue
                    pcm = audio_from_sse(line[5:].strip())
                    if pcm:
                        output_emitter.push(pcm)
                        if first:
                            first = False
                            elapsed = round((time.monotonic() - started) * 1000)
                            logger.info("voice.turn tts_first_chunk_ms=%d playback_handoff_ms=%d", elapsed, elapsed)


class MimoTTS(tts.TTS):
    def __init__(self) -> None:
        super().__init__(capabilities=tts.TTSCapabilities(streaming=False), sample_rate=24000, num_channels=1)
        self.api_key = (os.getenv("MIMO_API_KEY") or "").strip()
        base = (os.getenv("MATRIX_MIMO_BASE") or "https://api.xiaomimimo.com/v1").rstrip("/")
        self.endpoint = base + "/chat/completions"
        self.voice = (os.getenv("NEO_VOICE_TTS_VOICE") or "Mia").strip()
        self.style = (os.getenv("NEO_VOICE_TTS_STYLE") or "Warm, direct, conversational delivery.").strip()
        self.deadline = float(os.getenv("NEO_VOICE_TTS_DEADLINE_SECONDS") or "30")

    def synthesize(self, text: str, *, conn_options: APIConnectOptions = DEFAULT_API_CONNECT_OPTIONS) -> tts.ChunkedStream:
        if not self.api_key:
            raise RuntimeError("MiMo speech synthesis is not configured")
        return _MimoChunkedStream(self, text, conn_options)
