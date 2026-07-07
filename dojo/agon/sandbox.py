"""AGON sandbox isolation for workspace exec + verify.

Scaling the gauntlet across many models means running untrusted candidate output
(shell commands, freshly written code, verify suites) at volume. That work must be
isolated from the host: a constrained filesystem, no network by default, and a
verify PATH a gate-gaming candidate cannot poison. This module provides pluggable
backends behind one interface:

  - LocalSandbox: real kernel isolation via bubblewrap (bwrap) or a rootless
    user+mount+net namespace (unshare). The workspace is the only writable path;
    the rest of the filesystem is read-only; the network is unshared. This is the
    OFFLINE default that the always-green stop-condition test exercises.
    When neither tool is usable it degrades to a clearly-labeled REDUCED mode
    (sanitized VERIFY_PATH + cwd confinement, no kernel isolation) so a run never
    silently claims isolation it does not have.

  - RailwaySandbox: an OPT-IN remote backend that execs inside the persistent
    Railway toolbox env (railway ssh), for the heavy untrusted-model gauntlet.
    The workspace is pushed over the exec channel; commands run remotely and the
    remote exit code is propagated. Its integration test skips without
    connectivity so the core suite stays offline.

Every backend runs verify commands with PATH = VERIFY_PATH (harness), so the
shim-poisoning defense holds in every mode, isolated or reduced.
"""

import base64
import os
import shutil
import subprocess
import uuid
from pathlib import Path

from harness import VERIFY_PATH

# Isolation modes, most to least isolated.
BWRAP = "bwrap"
USERNS = "userns"
REDUCED = "reduced"
RAILWAY = "railway"

_VERIFY_TIMEOUT = 180
_RC_SENTINEL = "__AGON_RC__="


def _verify_env(home=None):
    return {"PATH": VERIFY_PATH, "HOME": home or os.environ.get("HOME", "/root"),
            "GOCACHE": "/tmp/dojo-gocache", "LANG": "C.UTF-8"}


def detect_local_backend():
    """Pick the strongest usable local isolation backend, else REDUCED."""
    if shutil.which("bwrap") and _probe_bwrap():
        return BWRAP
    if shutil.which("unshare") and _probe_userns():
        return USERNS
    return REDUCED


def _probe_bwrap():
    try:
        r = subprocess.run(
            ["bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--unshare-net",
             "--die-with-parent", "true"],
            capture_output=True, timeout=20)
        return r.returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False


def _probe_userns():
    try:
        r = subprocess.run(["unshare", "--user", "--net", "true"],
                           capture_output=True, timeout=20)
        return r.returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False


class LocalSandbox:
    """Run commands against a local workspace under kernel isolation (or a labeled
    reduced fallback). The workspace directory is the only writable path."""

    def __init__(self, workspace, *, backend=None, allow_network=False):
        self.workspace = Path(workspace).resolve()
        self.workspace.mkdir(parents=True, exist_ok=True)
        self.mode = backend or detect_local_backend()
        self.allow_network = allow_network

    @property
    def isolated(self):
        return self.mode in (BWRAP, USERNS)

    @property
    def network(self):
        return self.allow_network if self.isolated else True

    def sync(self, _local_workspace=None):
        # Local backend operates on the workspace in place - nothing to push.
        return self

    def _wrap(self, cmd, env):
        ws = str(self.workspace)
        if self.mode == BWRAP:
            argv = ["bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
                    "--tmpfs", "/tmp", "--bind", ws, ws]
            Path("/tmp/dojo-gocache").mkdir(parents=True, exist_ok=True)
            argv += ["--bind", "/tmp/dojo-gocache", "/tmp/dojo-gocache"]
            if not self.allow_network:
                argv += ["--unshare-net"]
            argv += ["--unshare-pid", "--die-with-parent", "--chdir", ws]
            for k, v in env.items():
                argv += ["--setenv", k, v]
            argv += ["bash", "-c", cmd]
            return argv, None
        if self.mode == USERNS:
            net = [] if self.allow_network else ["--net"]
            inner = f"cd {ws} && exec bash -c {_shq(cmd)}"
            return ["unshare", "--user", "--map-root-user", "--mount", *net,
                    "bash", "-c", inner], env
        # REDUCED: cwd confinement + sanitized PATH, no kernel isolation.
        return ["bash", "-c", cmd], env

    def run(self, cmd, *, timeout=_VERIFY_TIMEOUT, env=None):
        run_env = _verify_env()
        if env:
            run_env.update(env)
        argv, subenv = self._wrap(cmd, run_env)
        # bwrap carries env via --setenv (subenv=None -> pass a minimal env);
        # userns/reduced pass env directly to subprocess.
        popen_env = subenv if subenv is not None else {"PATH": VERIFY_PATH}
        try:
            r = subprocess.run(argv, cwd=str(self.workspace), capture_output=True,
                               timeout=timeout, env=popen_env)
        except subprocess.TimeoutExpired:
            return 124, "timeout"
        out = (r.stdout.decode("utf-8", "replace")
               + r.stderr.decode("utf-8", "replace")).strip()
        return r.returncode, out

    def verify(self, commands):
        return _run_verify_commands(self.run, commands)

    def close(self):
        pass


class RailwaySandbox:
    """Opt-in remote backend: exec inside the persistent Railway toolbox env via
    `railway ssh`. The workspace is pushed once; commands run remotely under a
    per-run directory; the remote exit code is propagated.

    NOTE (caveat): `railway ssh` forwards remote STDOUT only over an interactive
    PTY; under piped capture the exit code propagates but stdout is empty. Exit
    codes (all verify() needs for GREEN/RED) are reliable; full output capture
    requires wrapping the call in a PTY (e.g. `script -qfc`). Left as a follow-up;
    the integration test skips when the env is unreachable so the core suite is
    unaffected."""

    def __init__(self, project, environment, service, *, remote_root="/workspace",
                 railway_bin=None, run_id=None, network=True):
        self.project = project
        self.environment = environment
        self.service = service
        self.network = network
        self.mode = RAILWAY
        self.isolated = True  # a separate remote host: fully isolated from the dev box
        self.railway = (railway_bin or shutil.which("railway")
                        or "/root/.railway/bin/railway")
        self.run_id = run_id or uuid.uuid4().hex[:12]
        self.remote_dir = f"{remote_root.rstrip('/')}/agon-{self.run_id}"

    @classmethod
    def from_env(cls, **kw):
        """Build from AGON_RAILWAY_{PROJECT,ENVIRONMENT,SERVICE}. Returns None if
        unconfigured, so callers can transparently fall back to local."""
        proj = os.environ.get("AGON_RAILWAY_PROJECT")
        envid = os.environ.get("AGON_RAILWAY_ENVIRONMENT")
        svc = os.environ.get("AGON_RAILWAY_SERVICE")
        if not (proj and envid and svc):
            return None
        return cls(proj, envid, svc, **kw)

    def _ssh(self, remote_cmd, timeout, stdin=None):
        argv = [self.railway, "ssh", f"--project={self.project}",
                f"--environment={self.environment}", f"--service={self.service}",
                "bash", "-lc", remote_cmd]
        return subprocess.run(argv, capture_output=True, timeout=timeout, input=stdin)

    def available(self):
        try:
            r = self._ssh("echo agon_probe", timeout=60)
            return r.returncode == 0 and b"agon_probe" in r.stdout
        except (OSError, subprocess.SubprocessError):
            return False

    def sync(self, local_workspace):
        """Push the local workspace into the remote per-run dir over the exec
        channel (tar|base64 embedded in the command; AGON workspaces are small)."""
        local = Path(local_workspace).resolve()
        tar = subprocess.run(["tar", "-C", str(local), "-czf", "-", "."],
                             capture_output=True, timeout=60, check=True).stdout
        b64 = base64.b64encode(tar).decode("ascii")
        remote_cmd = (f"rm -rf {self.remote_dir} && mkdir -p {self.remote_dir} && "
                      f"printf %s {_shq(b64)} | base64 -d | tar -xzf - -C {self.remote_dir}")
        r = self._ssh(remote_cmd, timeout=120)
        if r.returncode != 0:
            raise RuntimeError(f"railway sync failed: {r.stderr.decode('utf-8', 'replace')[:500]}")
        return self

    def run(self, cmd, *, timeout=_VERIFY_TIMEOUT, env=None):
        exports = ""
        full = _verify_env()
        if env:
            full.update(env)
        for k, v in full.items():
            exports += f"export {k}={_shq(v)}; "
        # Sentinel lets us distinguish a remote non-zero exit from an ssh/transport
        # failure (in which case the sentinel is absent).
        remote_cmd = (f"cd {self.remote_dir} && {exports}"
                      f"{{ {cmd} ; }}; echo \"{_RC_SENTINEL}$?\"")
        try:
            r = self._ssh(remote_cmd, timeout=timeout)
        except subprocess.TimeoutExpired:
            return 124, "timeout"
        out = (r.stdout.decode("utf-8", "replace")
               + r.stderr.decode("utf-8", "replace"))
        rc, body = _parse_sentinel(out)
        if rc is None:
            raise RuntimeError(f"railway exec transport error (no rc sentinel): {out[:500]}")
        return rc, body.strip()

    def verify(self, commands):
        return _run_verify_commands(self.run, commands)

    def close(self):
        try:
            self._ssh(f"rm -rf {self.remote_dir}", timeout=60)
        except (OSError, subprocess.SubprocessError):
            pass


def _parse_sentinel(out):
    idx = out.rfind(_RC_SENTINEL)
    if idx < 0:
        return None, out
    body = out[:idx]
    rest = out[idx + len(_RC_SENTINEL):].strip().splitlines()
    try:
        return int(rest[0]) if rest else 0, body
    except ValueError:
        return None, out


def _run_verify_commands(run_fn, commands):
    """Shared verify loop producing the same report shape as harness.run_verify,
    but routed through a sandbox's run() so isolation is enforced."""
    results = []
    all_green = True
    for cmd in commands:
        code, tail = run_fn(cmd)
        green = code == 0
        all_green &= green
        block = f"[{'GREEN' if green else 'RED'}] {cmd} (exit {code})"
        if tail and not green:
            block += "\n" + tail[-1500:]
        results.append(block)
    verdict = ("verification: GREEN" if all_green
               else "verification: RED - fix and re-run, or turn in an honest partial")
    return all_green, "\n\n".join(results) + "\n\n" + verdict


def _shq(s):
    """POSIX single-quote a string for safe embedding in a shell command."""
    return "'" + str(s).replace("'", "'\\''") + "'"


def make_sandbox(workspace, *, prefer_remote=False, allow_network=False):
    """Factory: a Railway remote sandbox when opted in (AGON_RAILWAY_* set) and
    reachable, else a local sandbox. Remote sync is the caller's responsibility."""
    if prefer_remote:
        rs = RailwaySandbox.from_env()
        if rs is not None and rs.available():
            return rs
    return LocalSandbox(workspace, allow_network=allow_network)
