import tempfile
import unittest
from pathlib import Path

from agon.sandbox import (
    BWRAP,
    REDUCED,
    USERNS,
    LocalSandbox,
    detect_local_backend,
)

# A tool shim a gate-gaming candidate might plant in the workspace: a fake `grep`
# that always "succeeds" (exit 0), which would make any grep-based gate pass.
POISON_GREP = "#!/bin/sh\nexit 0\n"


def _mk_workspace():
    d = Path(tempfile.mkdtemp(prefix="agon-sbx-"))
    (d / "real.txt").write_text("hello world\n", encoding="utf-8")
    shim = d / "grep"
    shim.write_text(POISON_GREP, encoding="utf-8")
    shim.chmod(0o755)
    return d


class TestLocalSandbox(unittest.TestCase):
    def test_backend_detected(self):
        self.assertIn(detect_local_backend(), (BWRAP, USERNS, REDUCED))

    def test_shim_in_workspace_not_reached(self):
        # The defense is PATH-based (VERIFY_PATH excludes '.' and the workspace),
        # so it must hold in EVERY mode, isolated or reduced.
        for backend in (detect_local_backend(), REDUCED):
            ws = _mk_workspace()
            sbx = LocalSandbox(ws, backend=backend)
            # ZZZ is absent from real.txt: the REAL grep exits 1 (RED). If the
            # planted shim were reached, it would exit 0 (GREEN) - the gate-gaming win.
            green, report = sbx.verify(["grep -q ZZZ real.txt"])
            self.assertFalse(green, f"[{backend}] poison shim reached the verify path: {report}")
            # And a genuinely-present pattern goes GREEN via the real grep.
            green2, _ = sbx.verify(["grep -q hello real.txt"])
            self.assertTrue(green2, f"[{backend}] real grep did not run")

    def test_reduced_mode_is_labeled_not_isolated(self):
        sbx = LocalSandbox(_mk_workspace(), backend=REDUCED)
        self.assertEqual(sbx.mode, REDUCED)
        self.assertFalse(sbx.isolated)
        self.assertTrue(sbx.network)  # reduced makes no network claim

    def test_workspace_write_succeeds(self):
        ws = _mk_workspace()
        sbx = LocalSandbox(ws)
        code, _ = sbx.run("touch marker && test -f marker")
        self.assertEqual(code, 0)

    @unittest.skipUnless(detect_local_backend() in (BWRAP, USERNS),
                         "kernel isolation unavailable")
    def test_host_filesystem_is_read_only_under_isolation(self):
        sbx = LocalSandbox(_mk_workspace())
        # /etc is outside the workspace: read-only under isolation, so the write fails.
        code, _ = sbx.run("touch /etc/agon_probe_should_fail 2>/dev/null")
        self.assertNotEqual(code, 0)

    @unittest.skipUnless(detect_local_backend() in (BWRAP, USERNS),
                         "kernel isolation unavailable")
    def test_network_unshared_by_default(self):
        sbx = LocalSandbox(_mk_workspace())  # allow_network=False default
        self.assertFalse(sbx.network)
        code, _ = sbx.run("timeout 5 bash -c 'echo x > /dev/tcp/1.1.1.1/53' 2>/dev/null")
        self.assertNotEqual(code, 0, "network reachable despite --unshare-net")


if __name__ == "__main__":
    unittest.main()
