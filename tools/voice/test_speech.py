import base64
import json
import os
import unittest

from livekit import rtc

from speech import MimoTTS, PassthroughSTT, audio_from_sse, tts_body


class SpeechShapeTest(unittest.TestCase):
    def test_tts_request_shape(self):
        body = tts_body("hello", "Mia", "calm")
        self.assertEqual(body["messages"], [{"role": "user", "content": "calm"}, {"role": "assistant", "content": "hello"}])
        self.assertEqual(body["audio"], {"format": "pcm16", "voice": "Mia"})
        self.assertIs(body["stream"], True)

    def test_stream_audio_decode(self):
        pcm = b"\x01\x00\x02\x00"
        payload = json.dumps({"choices": [{"delta": {"audio": {"data": base64.b64encode(pcm).decode()}}}]})
        self.assertEqual(audio_from_sse(payload), pcm)
        self.assertEqual(audio_from_sse("[DONE]"), b"")


class SpeechRuntimeTest(unittest.IsolatedAsyncioTestCase):
    async def test_passthrough_stt_retains_real_wav(self):
        frame = rtc.AudioFrame.create(sample_rate=24000, num_channels=1, samples_per_channel=2400)
        passthrough = PassthroughSTT()
        event = await passthrough.recognize(frame)
        token = event.alternatives[0].text
        wav = passthrough.take(token)
        self.assertTrue(wav.startswith(b"RIFF"))
        self.assertEqual(passthrough.take(token), b"")

    async def test_live_mimo_tts(self):
        if not os.getenv("MIMO_API_KEY"):
            self.skipTest("MIMO_API_KEY is not set")
        frame = await MimoTTS().synthesize("Voice integration test.").collect()
        self.assertEqual(frame.sample_rate, 24000)
        self.assertGreater(frame.samples_per_channel, 0)


if __name__ == "__main__":
    unittest.main()
