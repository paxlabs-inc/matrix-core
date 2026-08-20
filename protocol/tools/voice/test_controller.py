from __future__ import annotations

import os
import sys
import time
import unittest

from controller import Supervisor


class ControllerLifecycleTests(unittest.TestCase):
    def test_idle_has_no_child_then_tears_down_and_reconnects(self) -> None:
        old_idle = os.environ.get("VOICE_IDLE_DISCONNECT_S")
        os.environ["VOICE_IDLE_DISCONNECT_S"] = "0.2"
        starts: list[str] = []

        def child(conversation: str) -> list[str]:
            starts.append(conversation)
            return [sys.executable, "-c", "import time; time.sleep(30)"]

        supervisor = Supervisor(child)
        try:
            self.assertEqual(supervisor.state(), {"active": False, "conversation_id": ""})
            self.assertEqual(starts, [])
            self.assertTrue(supervisor.start("conversation_one")["active"])
            deadline = time.monotonic() + 3
            while supervisor.state()["active"] and time.monotonic() < deadline:
                time.sleep(0.03)
            self.assertFalse(supervisor.state()["active"])
            self.assertTrue(supervisor.start("conversation_one")["active"])
            self.assertEqual(starts, ["conversation_one", "conversation_one"])
        finally:
            supervisor.close()
            if old_idle is None:
                os.environ.pop("VOICE_IDLE_DISCONNECT_S", None)
            else:
                os.environ["VOICE_IDLE_DISCONNECT_S"] = old_idle

    def test_worker_command_is_room_bound_and_self_mints(self) -> None:
        values = {
            "MATRIX_LIVEKIT_URL": "wss://livekit.example",
            "MATRIX_LIVEKIT_KEY": "key",
            "MATRIX_LIVEKIT_SECRET": "secret",
        }
        old = {key: os.environ.get(key) for key in values}
        os.environ.update(values)
        try:
            command = Supervisor._worker_command("conversation_two")
            child_env = Supervisor._worker_env("Chloe", "Calm and concise.")
        finally:
            for key, value in old.items():
                if value is None:
                    os.environ.pop(key, None)
                else:
                    os.environ[key] = value
        self.assertIn("connect", command)
        self.assertEqual(command[command.index("--room") + 1], "voice:conversation_two")
        self.assertNotIn("secret", command)
        self.assertEqual(child_env["LIVEKIT_API_KEY"], "key")
        self.assertEqual(child_env["LIVEKIT_API_SECRET"], "secret")
        self.assertEqual(child_env["NEO_VOICE_TTS_VOICE"], "Chloe")
        self.assertEqual(child_env["NEO_VOICE_TTS_STYLE"], "Calm and concise.")

    def test_stale_stop_cannot_terminate_replacement_session(self) -> None:
        def child(conversation: str) -> list[str]:
            return [sys.executable, "-c", "import time; time.sleep(30)"]

        supervisor = Supervisor(child)
        try:
            supervisor.start("conversation_one")
            supervisor.start("conversation_two")
            with self.assertRaises(LookupError):
                supervisor.stop("conversation_one")
            self.assertEqual(supervisor.state(), {"active": True, "conversation_id": "conversation_two"})
        finally:
            supervisor.close()


if __name__ == "__main__":
    unittest.main()
