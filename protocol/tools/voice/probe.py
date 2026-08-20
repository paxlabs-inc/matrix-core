from __future__ import annotations

import argparse
import asyncio
from pathlib import Path

from bridge import NeoBridge
from speech import MimoTTS


async def run(args: argparse.Namespace) -> None:
    bridge = NeoBridge(args.conversation, daemon_url=args.url, token=args.token)
    await bridge.start()
    try:
        async def consume(wav: bytes) -> None:
            async for chunk in bridge.run_turn(wav):
                print(chunk, flush=True)
                if args.synthesize:
                    await MimoTTS().synthesize(chunk).collect()

        if args.interrupt_wav:
            first = asyncio.create_task(consume(Path(args.wav).read_bytes()))
            await asyncio.sleep(args.interrupt_after)
            second = asyncio.create_task(consume(Path(args.interrupt_wav).read_bytes()))
            await asyncio.gather(first, second)
        else:
            await consume(Path(args.wav).read_bytes())
    finally:
        await bridge.aclose()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--conversation", required=True)
    parser.add_argument("--wav", required=True)
    parser.add_argument("--token", default="")
    parser.add_argument("--synthesize", action="store_true")
    parser.add_argument("--interrupt-wav")
    parser.add_argument("--interrupt-after", type=float, default=0.5)
    asyncio.run(run(parser.parse_args()))
