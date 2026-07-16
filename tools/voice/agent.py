from __future__ import annotations

import json
import logging
import os
from typing import AsyncIterable

from dotenv import load_dotenv
from livekit import agents
from livekit.agents import Agent, AgentServer, AgentSession, JobContext, JobRequest, ModelSettings, cli, llm
from livekit.plugins import silero
from livekit.plugins.turn_detector.multilingual import MultilingualModel

from bridge import NeoBridge
from speech import MimoTTS, PassthroughSTT

load_dotenv()
logging.basicConfig(level=os.getenv("VOICE_LOG_LEVEL", "INFO"))
logger = logging.getLogger("neo.voice")

AGENT_IDENTITY = "neo-voice"
INTENT_TOPIC = "neo.voice.intent"


def conversation_from_room(room: str) -> str:
    if not room.startswith("voice:") or len(room) <= len("voice:"):
        raise ValueError("voice room must be bound to a conversation")
    return room[len("voice:") :]


class NeoVoiceAgent(Agent):
    def __init__(self, bridge: NeoBridge, passthrough: PassthroughSTT) -> None:
        super().__init__(instructions="Relay the user's audio to the colocated agent and speak its reply verbatim.")
        self.bridge = bridge
        self.passthrough = passthrough

    async def llm_node(self, chat_ctx: llm.ChatContext, tools: list[llm.Tool], model_settings: ModelSettings) -> AsyncIterable[str]:
        del tools, model_settings
        token = ""
        for item in reversed(chat_ctx.items):
            if getattr(item, "role", None) == "user":
                token = getattr(item, "text_content", None) or ""
                break
        wav = self.passthrough.take(token)
        async for chunk in self.bridge.run_turn(wav):
            yield chunk

    async def on_user_turn_completed(self, turn_ctx: llm.ChatContext, new_message: llm.ChatMessage) -> None:
        del turn_ctx, new_message
        if self.bridge.busy and not self.bridge.awaiting_answer:
            await self.bridge.interrupt_active()


def prewarm(proc: agents.JobProcess) -> None:
    proc.userdata["vad"] = silero.VAD.load()


async def handle_request(req: JobRequest) -> None:
    conversation_from_room(req.room.name)
    await req.accept(identity=AGENT_IDENTITY, name="Neo Voice")


server = AgentServer(
    setup_fnc=prewarm,
    permissions=agents.WorkerPermissions(
        can_publish=True,
        can_subscribe=True,
        can_publish_data=True,
        can_update_metadata=True,
    ),
)


@server.rtc_session(on_request=handle_request)
async def entrypoint(ctx: JobContext) -> None:
    conversation_id = conversation_from_room(ctx.room.name)

    async def publish_intent(intent_id: str) -> None:
        if ctx.room.isconnected():
            await ctx.room.local_participant.publish_data(json.dumps({"intent_id": intent_id}), topic=INTENT_TOPIC, reliable=True)

    passthrough = PassthroughSTT()
    bridge = NeoBridge(conversation_id, on_intent=publish_intent)
    await bridge.start()
    ctx.add_shutdown_callback(bridge.aclose)
    session = AgentSession(
        vad=ctx.proc.userdata["vad"],
        stt=passthrough,
        tts=MimoTTS(),
        turn_detection=MultilingualModel(),
        min_interruption_duration=0.5,
    )
    await ctx.connect()
    await session.start(agent=NeoVoiceAgent(bridge, passthrough), room=ctx.room)


if __name__ == "__main__":
    cli.run_app(server)
