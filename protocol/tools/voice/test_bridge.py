import unittest

from bridge import NeoBridge, _Run


class BridgeFoldTest(unittest.TestCase):
    def test_sentence_flush_and_tail_dedup(self):
        bridge = NeoBridge("conv")
        run = _Run("intent")
        bridge._active = run
        got = bridge._fold(run, {"type": "chat.delta", "fields": {"channel": "content", "text": "This is a complete sentence. Tail"}})
        self.assertEqual(got, ["This is a complete sentence. "])
        got = bridge._fold(run, {"type": "chat.assistant", "fields": {"text": "This is a complete sentence. Tail"}})
        self.assertEqual(got, ["Tail "])

    def test_json_fragments_are_not_mistaken_for_events(self):
        bridge = NeoBridge("conv")
        run = _Run("intent")
        bridge._active = run
        got = bridge._fold(run, {"type": "chat.delta", "fields": {"channel": "reasoning", "text": '{"secret":true}'}})
        self.assertEqual(got, [])

    def test_terminal_clears_single_flight(self):
        bridge = NeoBridge("conv")
        run = _Run("intent", buffer="final tail")
        bridge._active = run
        got = bridge._fold(run, {"type": "message.complete", "fields": {}})
        self.assertEqual(got, ["final tail "])
        self.assertFalse(bridge.busy)

    def test_gate_pauses_without_stopping_the_run(self):
        bridge = NeoBridge("conv")
        run = _Run("intent")
        bridge._active = run
        got = bridge._fold(run, {"type": "gate.invoked", "fields": {"node_id": "n1", "question": "Approve transfer?"}})
        self.assertEqual(got, ["Approve transfer? Say yes to approve or no to deny."])
        self.assertTrue(run.paused)
        self.assertTrue(bridge.awaiting_answer)
        self.assertTrue(bridge.busy)


if __name__ == "__main__":
    unittest.main()
