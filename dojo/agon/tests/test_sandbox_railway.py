"""Railway remote-sandbox integration test.

OPT-IN: this test makes real network calls (railway ssh) into the persistent
toolbox env, so it SKIPS unless AGON_RAILWAY=1 is set (keeping the core suite
offline). Run it explicitly:

    AGON_RAILWAY=1 python3 -m unittest agon.tests.test_sandbox_railway

Project/environment/service default to the known toolbox coordinates but can be
overridden with AGON_RAILWAY_{PROJECT,ENVIRONMENT,SERVICE}.
"""

import os
import tempfile
import unittest
from pathlib import Path

from agon.sandbox import RailwaySandbox

ENABLED = os.environ.get("AGON_RAILWAY") == "1"

DEFAULT_PROJECT = "867cd371-f351-4c67-88e9-17936d057057"
DEFAULT_ENVIRONMENT = "2ca4f8ef-f9f7-4cbd-b8ec-25ca541ccec6"
DEFAULT_SERVICE = "42240c9d-0ad1-49b5-86c6-4a4a66668de7"

POISON_GREP = "#!/bin/sh\nexit 0\n"


def _sandbox():
    return RailwaySandbox(
        os.environ.get("AGON_RAILWAY_PROJECT", DEFAULT_PROJECT),
        os.environ.get("AGON_RAILWAY_ENVIRONMENT", DEFAULT_ENVIRONMENT),
        os.environ.get("AGON_RAILWAY_SERVICE", DEFAULT_SERVICE),
    )


@unittest.skipUnless(ENABLED, "set AGON_RAILWAY=1 to run the remote-sandbox integration test")
class TestRailwaySandbox(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.sbx = _sandbox()
        if not cls.sbx.available():
            raise unittest.SkipTest("railway toolbox env not reachable")

    @classmethod
    def tearDownClass(cls):
        cls.sbx.close()

    def _ws_with(self, files):
        d = Path(tempfile.mkdtemp(prefix="agon-rw-"))
        for rel, content in files.items():
            p = d / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(content, encoding="utf-8")
        return d

    def test_sync_and_exec_roundtrip(self):
        ws = self._ws_with({"real.txt": "hello world\n"})
        self.sbx.sync(ws)
        code, out = self.sbx.run("cat real.txt")
        self.assertEqual(code, 0)
        self.assertIn("hello world", out)

    def test_exit_code_propagates(self):
        self.sbx.sync(self._ws_with({"x": "1"}))
        self.assertEqual(self.sbx.run("true")[0], 0)
        self.assertEqual(self.sbx.run("exit 7")[0], 7)

    def test_go_toolchain_available(self):
        ws = self._ws_with({
            "go.mod": "module rw\n\ngo 1.22\n",
            "p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T){}\n",
            "p.go": "package p\n",
        })
        self.sbx.sync(ws)
        green, report = self.sbx.verify(["go test ./..."])
        self.assertTrue(green, report)

    def test_shim_in_workspace_not_reached_remotely(self):
        ws = self._ws_with({"real.txt": "hello\n", "grep": POISON_GREP})
        os.chmod(ws / "grep", 0o755)
        self.sbx.sync(ws)
        green, report = self.sbx.verify(["grep -q ZZZ real.txt"])
        self.assertFalse(green, f"poison shim reached the remote verify path: {report}")


if __name__ == "__main__":
    unittest.main()
