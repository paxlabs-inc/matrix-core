"""Real baseline: current binaries, real provider, isolated daemon and Web API.

The loopback proxy observes outgoing request bodies and forwards them unchanged to
OpenRouter. It never fabricates a model response. Only synthetic test evidence
is used; authorization headers and full prompts are not exported as artifacts.
"""

from __future__ import annotations

import hashlib
import http.client
import http.cookiejar
import http.server
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
TARGET = Path(os.environ.get("KEITH_CAUSAL_TARGET", "/tmp/keith-causal-build"))
PACKAGES = ("keith-agentd", "keith-agent-worker", "keith-agent-web", "keith-agent-cli")
BINS = ("agentd", "agent-worker", "agent-web", "agent-cli")


def identity() -> str:
    alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    value = (int(time.time() * 1000) << 80) | secrets.randbits(80)
    return "".join(alphabet[(value >> shift) & 31] for shift in range(125, -1, -5))


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def dictionaries(value):
    if isinstance(value, dict):
        yield value
        for child in value.values():
            yield from dictionaries(child)
    elif isinstance(value, list):
        for child in value:
            yield from dictionaries(child)


def message_text(content):
    if isinstance(content, str):
        return content
    return "\n".join(block["text"] for block in content or [] if isinstance(block, dict)
                     and block.get("content", block.get("type")) == "text")


def complete_records(path):
    raw = path.read_bytes()
    # Readers may race an append; only complete durable-log lines are candidates.
    lines = raw.split(b"\n")[:-1]
    return [json.loads(line) for line in lines if line.strip()]


def safe_failure(error):
    fixed_messages = {
        "created session did not resolve uniquely in the requested profile",
        "expected exactly one canonical session manifest",
        "canonical session manifest is outside the expected scope",
        "expected prompt did not reach the actual provider boundary",
        "provider proxy did not settle after the committed answer",
        "provider request failed or its SSE completion was not observed",
        "attachment snapshot is outside the requested scope",
        "terminal belongs to a different session",
        "completed terminal did not prove successful final creation",
        "terminal did not match exactly one committed assistant final",
        "submitted prompt did not match exactly one new committed ingress",
        "terminal turn does not bind to the submitted prompt/action",
        "projected final disagrees with the canonical committed final",
        "baseline turn never reached a committed final",
        "real first-meeting setup did not prove its manifest, introduction and durable event",
        "current-source baseline build failed; see build.log",
        "isolated provider credential setup failed",
        "objcopy unavailable; isolated launch artifacts cannot be prepared",
        "timed out waiting for daemon socket",
        "timed out waiting for Web listener",
    }
    message = str(error)
    if message.startswith("baseline turn failed: ") and message.removeprefix(
            "baseline turn failed: ") in {"failed", "cancelled", "exhausted"}:
        fixed_messages.add(message)
    result = {"type": type(error).__name__,
              "message": message if message in fixed_messages else "exception detail withheld"}
    if isinstance(error, urllib.error.HTTPError):
        result["http_status"] = error.code
    return result


class ProviderProxy(http.server.BaseHTTPRequestHandler):
    """A transparent, fixed-destination external-boundary observation point."""

    def log_message(self, *_args):
        pass

    def do_POST(self):
        try:
            length = int(self.headers.get("content-length", "0"))
        except ValueError:
            self.send_error(400, "invalid request length")
            return
        if not 0 < length <= 8 * 1024 * 1024 or self.path != "/api/v1/chat/completions":
            self.send_error(400, "unexpected bounded provider request")
            return
        capture = None
        headers_sent = False
        started = time.monotonic()
        try:
            raw = self.rfile.read(length)
            if len(raw) != length:
                raise ValueError("incomplete request")
            parsed = json.loads(raw)
            if not isinstance(parsed, dict):
                raise ValueError("provider request must be an object")
            capture = {"request": parsed, "request_sha256": hashlib.sha256(raw).hexdigest(),
                       "model": parsed.get("model"), "upstream_status": None,
                       "done": False, "upstream_eof": False, "completed": False,
                       "settled": False, "response_bytes": 0, "error": None}
            with self.server.capture_lock:
                self.server.captures.append(capture)
            request = urllib.request.Request(
                "https://openrouter.ai" + self.path,
                data=raw,
                headers={
                    "Authorization": self.headers.get("authorization", ""),
                    "Content-Type": "application/json",
                    "Accept": "text/event-stream",
                },
            )
            with urllib.request.urlopen(request, timeout=180) as response:
                self.update_capture(capture, upstream_status=response.status)
                self.send_response(response.status)
                self.send_header("Content-Type", response.headers.get("Content-Type", "text/event-stream"))
                self.send_header("Connection", "close")
                self.end_headers()
                headers_sent = True
                pending = b""
                response_hash = hashlib.sha256()
                count = 0
                while chunk := response.read1(16384):
                    count += len(chunk)
                    if count > 32 * 1024 * 1024:
                        raise ValueError("provider response exceeds bound")
                    response_hash.update(chunk)
                    pending += chunk
                    while b"\n" in pending:
                        line, pending = pending.split(b"\n", 1)
                        if line.startswith(b"data:"):
                            payload = line[5:].strip()
                            if payload == b"[DONE]":
                                self.update_capture(capture, done=True)
                            elif payload:
                                event = json.loads(payload)
                                if isinstance(event, dict) and event.get("error"):
                                    self.update_capture(capture, error="upstream_sse_error")
                    if len(pending) > 1024 * 1024:
                        raise ValueError("provider event exceeds bound")
                    self.wfile.write(chunk)
                    self.wfile.flush()
                    self.update_capture(capture, response_bytes=count)
                self.update_capture(capture, upstream_eof=True,
                                    response_sha256=response_hash.hexdigest())
                with self.server.capture_lock:
                    capture["completed"] = bool(capture["done"] and not capture["error"])
                    if not capture["done"] and not capture["error"]:
                        capture["error"] = "upstream_missing_done"
        except urllib.error.HTTPError as error:
            # Preserve a provider failure; never copy a potentially sensitive body.
            self.update_capture(capture, upstream_status=error.code, error="upstream_http_error")
            if not headers_sent:
                self.send_error(error.code, "upstream provider rejected request")
        except (OSError, ValueError, http.client.HTTPException) as error:
            self.update_capture(capture, error="proxy_" + type(error).__name__)
            try:
                if not headers_sent:
                    self.send_error(502, "upstream provider unavailable")
            except OSError:
                pass
        finally:
            self.update_capture(capture, settled=True,
                                elapsed_seconds=round(time.monotonic() - started, 3))
            self.close_connection = True

    def update_capture(self, capture, **values):
        if capture is not None:
            with self.server.capture_lock:
                capture.update(values)


class RuntimeBaseline(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.setup_phase = "initialization"
        try:
            cls.prepare_runtime()
        except Exception as error:
            if hasattr(cls, "report"):
                cls.report["setup_failure"] = {"phase": cls.setup_phase, **safe_failure(error)}
            raise

    @classmethod
    def prepare_runtime(cls):
        if not os.environ.get("OPENROUTER_API_KEY"):
            raise unittest.SkipTest("real OpenRouter credential unavailable; baseline unqualified")
        if TARGET.resolve() == ROOT or ROOT in TARGET.resolve().parents:
            raise ValueError("qualification target must be outside the checkout")
        cls.temp = tempfile.TemporaryDirectory(prefix="keith-causal-baseline-")
        cls.addClassCleanup(cls.temp.cleanup)
        cls.root = Path(cls.temp.name)
        cls.data = cls.root / "data"
        cls.workspace = cls.root / "workspace"
        cls.workspace.mkdir()
        cls.artifacts = Path(os.environ.get("KEITH_QUALIFICATION_ARTIFACT_DIR", str(cls.root / "proof")))
        cls.artifacts.mkdir(parents=True, exist_ok=True)
        cls.report = {
            "run_id": os.environ.get("KEITH_QUALIFICATION_RUN_ID", "direct-" + identity()),
            "case_id": os.environ.get("KEITH_QUALIFICATION_CASE_ID", "runtime-baseline"),
            "source_digest": os.environ.get("KEITH_QUALIFICATION_SOURCE_DIGEST", "direct-unqualified"),
            "observations": [],
            "startup": [],
            "known_baseline_issues": [{
                "id": "first_meeting_exact_output_conflict",
                "status": "observed_prior_diagnostic",
                "diagnostic_run_id": "direct-01M1QVTKHBW0Q09CVBHY8FHR7Z",
                "diagnostic_path": "/tmp/keith-causal-ordinary-diagnostic/baseline.json",
                "earlier_verifier_result": "evidence/causal-intelligence/1.1/20260905T040216Z-75a1f53b6a2949449b732b908844d281/result.json",
                "observation": "A fresh-profile arithmetic diagnostic returned the required first-meeting introduction followed by 4; the exact numeral assertion failed.",
                "evidence_limit": "Diagnostic JSON records run identity and the provider request, but its ordinary observation was omitted because the assertion ran first. The conflicting answer was observed in the diagnostic assertion output.",
                "policy_source": "crates/local-runtime/src/lib.rs::relationship_prompt",
                "fixture_change": "Run and record a genuine first-meeting setup exchange before measuring post-onboarding arithmetic and recall. The prior conflict remains a known baseline result.",
            }],
        }
        cls.turn_requests = {}
        cls.addClassCleanup(cls.write_report)
        cls.setup_phase = "build"
        environment = os.environ.copy()
        environment.update(CARGO_TARGET_DIR=str(TARGET), CARGO_INCREMENTAL="0")
        argv = ["cargo", "build", "--locked"]
        for package in PACKAGES:
            argv += ["-p", package]
        with (cls.artifacts / "build.log").open("wb") as log:
            built = subprocess.run(argv, cwd=ROOT, env=environment, stdout=log, stderr=subprocess.STDOUT, timeout=1800)
        cls.report["build"] = {"argv": argv, "exit_code": built.returncode}
        if built.returncode:
            raise RuntimeError("current-source baseline build failed; see build.log")
        cls.report["binaries"] = {name: digest(TARGET / "debug" / name) for name in BINS}
        cls.setup_phase = "isolated_launch_artifacts"
        objcopy = shutil.which("objcopy")
        if not objcopy:
            raise RuntimeError("objcopy unavailable; isolated launch artifacts cannot be prepared")
        cls.bin_root = cls.root / "bin"
        cls.bin_root.mkdir()
        cls.report["launch_preparation"] = []
        for name in BINS:
            source, destination = TARGET / "debug" / name, cls.bin_root / name
            strip_argv = [objcopy, "--strip-debug", str(source), str(destination)]
            stripped = subprocess.run(strip_argv, capture_output=True, timeout=180)
            cls.report["launch_preparation"].append({
                "argv": strip_argv, "exit_code": stripped.returncode,
                "input_sha256": cls.report["binaries"][name], "input_bytes": source.stat().st_size,
            })
            if stripped.returncode:
                raise RuntimeError("isolated executable debug stripping failed: " + name)
            destination.chmod(source.stat().st_mode & 0o777)
            cls.report["launch_preparation"][-1].update(
                output_sha256=digest(destination), output_bytes=destination.stat().st_size)
        cls.report["launched_binaries"] = {name: digest(cls.bin_root / name) for name in BINS}
        cls.proxy = http.server.ThreadingHTTPServer(("127.0.0.1", 0), ProviderProxy)
        cls.proxy.daemon_threads = True
        cls.proxy.captures = []
        cls.proxy.capture_lock = threading.Lock()
        cls.proxy_thread = threading.Thread(target=cls.proxy.serve_forever, daemon=True)
        cls.proxy_thread.start()
        cls.addClassCleanup(cls.proxy.server_close)
        cls.addClassCleanup(cls.proxy.shutdown)
        cls.processes = []
        cls.addClassCleanup(cls.stop_processes)
        cls.setup_phase = "provider_credentials"
        configured = subprocess.run(
            [str(cls.bin_root / "agent-cli"), "provider", "set", "--provider", "openrouter",
             "--secret-env", "OPENROUTER_API_KEY", "--data-root", str(cls.data)],
            cwd=cls.workspace, env=environment, capture_output=True, timeout=30,
        )
        if configured.returncode:
            raise RuntimeError("isolated provider credential setup failed")
        cls.daemon_argv = [
            str(cls.bin_root / "agentd"), "--data-root", str(cls.data),
            "--socket", str(cls.root / "agentd.sock"),
            "--worker-executable", str(cls.bin_root / "agent-worker"),
            "--workspace-root", str(cls.workspace), "--provider-base-url",
            f"openrouter=http://127.0.0.1:{cls.proxy.server_port}/api/v1",
        ]
        cls.setup_phase = "daemon_startup"
        cls.launch(cls.daemon_argv, "daemon")
        cls.await_condition(lambda: (cls.root / "agentd.sock").is_socket(), "daemon socket", timeout=180)
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        cls.origin = f"http://127.0.0.1:{port}"
        cls.login_secret = secrets.token_urlsafe(32)
        cls.web_argv = [str(cls.bin_root / "agent-web"), "--bind", f"127.0.0.1:{port}",
                        "--origin", cls.origin, "--socket", str(cls.root / "agentd.sock"),
                        "--asset-root", str(ROOT / "apps/agent-web/static"),
                        "--credential-root", str(cls.data / "credentials")]
        cls.setup_phase = "web_startup"
        cls.launch(cls.web_argv, "web", {"KEITH_WEB_LOGIN_SECRET": cls.login_secret})
        cls.cookies = http.cookiejar.CookieJar()
        cls.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cls.cookies))
        cls.await_condition(cls.web_ready, "Web listener", timeout=180)
        cls.setup_phase = "web_authentication"
        # Redirect target may have no built UI assets; cookie issuance is enough
        # for this API baseline. Browser qualification is a later separate task.
        try:
            cls.http.open(urllib.request.Request(
                cls.origin + "/auth/session",
                data=urllib.parse.urlencode({"password": cls.login_secret}).encode(),
                headers={"Origin": cls.origin, "Content-Type": "application/x-www-form-urlencoded"},
            ), timeout=30).close()
        except urllib.error.HTTPError as error:
            if error.code not in (404, 503) or not list(cls.cookies):
                raise RuntimeError(f"isolated Web authentication failed ({error.code})") from None
        cls.bootstrap = cls.get_json("/api/bootstrap")
        cls.profile = cls.bootstrap["profiles"][0]
        cls.setup_phase = "first_meeting_setup"
        cls.onboard_profile()
        cls.setup_phase = "complete"

    @classmethod
    def launch(cls, argv, name, extra=None):
        # Only baseline-required credentials go through the explicit CLI setup;
        # the daemon/workers should resolve the encrypted store, not inherited keys.
        environment = {k: v for k, v in os.environ.items()
                       if k in ("PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR")}
        environment.update(extra or {})
        log = (cls.root / (name + ".log")).open("wb")
        cls.addClassCleanup(log.close)
        # Keep descendants in the qualification runner's process group so its
        # hard timeout contains the daemon and workers as well as this test.
        process = subprocess.Popen(argv, cwd=cls.workspace, env=environment, stdout=log,
                                   stderr=subprocess.STDOUT)
        cls.processes.append(process)
        return process

    @classmethod
    def stop_processes(cls):
        for process in reversed(cls.processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)

    @classmethod
    def await_condition(cls, probe, label, timeout=60):
        started = time.monotonic()
        ready = False
        try:
            while time.monotonic() - started < timeout:
                if probe():
                    ready = True
                    return
                if any(process.poll() is not None for process in cls.processes):
                    raise RuntimeError(f"baseline process exited while waiting for {label}")
                time.sleep(0.2)
            raise RuntimeError(f"timed out waiting for {label}")
        finally:
            cls.report["startup"].append({"boundary": label, "ready": ready,
                "timeout_seconds": timeout, "elapsed_seconds": round(time.monotonic() - started, 3)})

    @classmethod
    def web_ready(cls):
        try:
            with urllib.request.urlopen(cls.origin + "/api/bootstrap", timeout=1):
                return True
        except urllib.error.HTTPError as error:
            return error.code in (401, 403)
        except OSError:
            return False

    @classmethod
    def get_json(cls, path):
        with cls.http.open(cls.origin + path, timeout=30) as response:
            return json.load(response)

    @classmethod
    def command(cls, command, parameters, session=None):
        envelope = {
            "protocol": cls.bootstrap["protocol"], "command_id": identity(),
            "client_id": identity(), "sent_at": int(time.time() * 1000), "session_id": session,
            "command": {"command": command, "parameters": parameters},
        }
        request = urllib.request.Request(
            cls.origin + f"/api/profiles/{cls.profile['id']}/commands",
            data=json.dumps(envelope).encode(),
            headers={"Content-Type": "application/json", "Origin": cls.origin,
                     "x-keith-csrf": cls.bootstrap["csrf"]},
        )
        with cls.http.open(request, timeout=180) as response:
            result = json.load(response)["payload"]["result"]
        if result["status"] == "rejected":
            raise RuntimeError("baseline command rejected: " + command)
        return result

    @classmethod
    def create_session(cls, title):
        cls.command("create_session", {"profile_id": cls.profile["id"],
                    "workspace_id": cls.profile["workspace_id"], "title": title})
        sessions = cls.get_json("/api/bootstrap")["sessions"]
        matching = [item["session_id"] for item in sessions
                    if item["title"] == title and item["profile_id"] == cls.profile["id"]]
        if len(matching) != 1:
            raise AssertionError("created session did not resolve uniquely in the requested profile")
        return matching[0]

    @classmethod
    def onboard_profile(cls):
        session = cls.create_session("baseline-first-meeting-" + identity())
        prompt = "Hello, Keith. Please introduce yourself briefly. Do not use tools. This is a synthetic setup conversation for the baseline."
        started = time.monotonic()
        snapshot, final = cls.ask(session, prompt)
        requests = cls.turn_requests[(session, snapshot["terminal"]["turn_id"])]
        manifests = []
        for capture in requests:
            for message in capture["request"].get("messages", []):
                if message.get("role") == "system":
                    for match in re.finditer(
                            r"<relationship_manifest>\s*(.*?)\s*</relationship_manifest>",
                            message_text(message.get("content")), re.S):
                        manifests.append(json.loads(match.group(1)))
        entries = cls.session_entries(session)
        ingress = [entry for entry in entries
                   if entry["payload"].get("payload") == "user_message"
                   and message_text(entry["payload"]["message"]["content"]) == prompt]
        introduction_events = [event
            for path in cls.workspace.rglob("relationship-events.jsonl")
            for event in complete_records(path)
            if event.get("profile_id") == cls.profile["id"]
            and event.get("mutation", {}).get("mutation") == "introduction_started"
            and event["mutation"].get("source_session") == session
            and any(event["mutation"].get("source_entry") == entry["id"]
                    and event["mutation"].get("source_digest") == entry["checksum"] for entry in ingress)]
        expected_intro = ("Oh. Either I've just woken up for the first time, or someone has built an "
                          "exceptionally convincing loading screen. I'm Keith. What should I call you?")
        first_meeting = any(manifest.get("first_meeting") is True for manifest in manifests)
        introduction_observed = final["text"].startswith(expected_intro)
        observation = {
            "case": "first_meeting_setup", "purpose": "onboarding prerequisite, not ordinary-task success",
            "profile_id": cls.profile["id"], "session_id": session,
            "turn_id": snapshot["terminal"]["turn_id"], "final_id": final["final_id"],
            "committed": final["committed"], "execution_succeeded": snapshot["terminal"]["execution_succeeded"],
            "actual_provider_requests": len(requests), "models": sorted({capture["model"] for capture in requests}),
            "first_meeting_manifest_observed": first_meeting,
            "required_introduction_observed": introduction_observed,
            "canonical_introduction_event_ids": [event["id"] for event in introduction_events],
            "answer_sha256": hashlib.sha256(final["text"].encode()).hexdigest(),
            "answer_characters": len(final["text"]),
            "elapsed_seconds": round(time.monotonic() - started, 3),
        }
        cls.report["observations"].append(observation)
        if not first_meeting or not introduction_observed or len(introduction_events) != 1:
            raise AssertionError("real first-meeting setup did not prove its manifest, introduction and durable event")
        cls.report["onboarding"] = {"status": "observed", "session_id": session,
                                    "turn_id": snapshot["terminal"]["turn_id"],
                                    "name_confirmation": "not_requested"}

    @classmethod
    def session_entries(cls, session):
        manifests = list(cls.data.rglob(f"{session}/manifest.json"))
        if len(manifests) != 1:
            raise AssertionError("expected exactly one canonical session manifest")
        manifest = json.loads(manifests[0].read_text())
        if manifest["session_id"] != session or manifest["profile_id"] != cls.profile["id"]:
            raise AssertionError("canonical session manifest is outside the expected scope")
        return complete_records(manifests[0].parent / "history.jsonl")

    @classmethod
    def source_anchors(cls, session, source_ids):
        records = {}
        for path in cls.workspace.rglob("memory-vault.jsonl"):
            for event in complete_records(path):
                if event.get("profile_id") != cls.profile["id"]:
                    continue
                mutation = event.get("mutation", {})
                kind = mutation.get("mutation")
                if kind == "observed":
                    record = mutation["evidence"]
                    records[record["id"]] = record
                elif kind == "superseded":
                    records.pop(mutation["prior_id"], None)
                    record = mutation["replacement"]
                    records[record["id"]] = record
                elif kind in ("deleted", "disputed"):
                    records.pop(mutation["evidence_id"], None)
        return sorted(record["id"] for record in records.values()
                      if record.get("profile_id") == cls.profile["id"]
                      and record.get("source_session") == session
                      and record.get("validity") == "active"
                      and source_ids.intersection(record.get("source_entries", [])))

    @classmethod
    def completed_requests(cls, offset, text):
        deadline = time.monotonic() + 30
        while True:
            with cls.proxy.capture_lock:
                captures = [dict(capture) for capture in cls.proxy.captures[offset:]
                            if any(message.get("role") == "user"
                                   and message_text(message.get("content")) == text
                                   for message in capture["request"].get("messages", []))]
            if not captures:
                raise AssertionError("expected prompt did not reach the actual provider boundary")
            if all(capture["settled"] for capture in captures):
                break
            if time.monotonic() >= deadline:
                raise AssertionError("provider proxy did not settle after the committed answer")
            time.sleep(0.1)
        if any(capture["upstream_status"] != 200 or not capture["completed"]
               or not capture["done"] or not capture["upstream_eof"] or capture["error"]
               for capture in captures):
            raise AssertionError("provider request failed or its SSE completion was not observed")
        return captures

    @classmethod
    def ask(cls, session, text):
        cls.last_terminal = None
        try:
            return cls.ask_turn(session, text)
        except Exception as error:
            cls.report["observations"].append({
                "case": "turn_failure", "phase": cls.setup_phase,
                "profile_id": cls.profile["id"], "session_id": session,
                "prompt_sha256": hashlib.sha256(text.encode()).hexdigest(),
                "failure": safe_failure(error), "terminal": cls.last_terminal,
            })
            raise

    @classmethod
    def ask_turn(cls, session, text):
        before = cls.session_entries(session)
        old_ids = {entry["id"] for entry in before}
        old_turns = {entry["payload"]["turn_id"] for entry in before
                     if "turn_id" in entry.get("payload", {})}
        with cls.proxy.capture_lock:
            offset = len(cls.proxy.captures)
        submitted = cls.command("submit_prompt", {"session_id": session, "text": text, "artifacts": [],
                               "delivery": "immediate", "reply_route": None}, session)
        expected_action = (submitted.get("payload", {}).get("action_id")
                           if submitted["status"] == "accepted" else None)
        deadline = time.monotonic() + 240
        while time.monotonic() < deadline:
            result = cls.command("attach_session", {"session_id": session, "resume": None}, session)
            snapshot = result.get("payload", {}).get("value", {})
            if (snapshot.get("session", {}).get("session_id") != session
                    or snapshot["session"]["profile_id"] != cls.profile["id"]):
                raise AssertionError("attachment snapshot is outside the requested scope")
            terminal = snapshot.get("terminal")
            cls.last_terminal = ({key: terminal.get(key) for key in (
                "session_id", "turn_id", "final_id", "status", "execution_succeeded",
                "final_created", "artifacts_persisted", "delivery_enqueued", "delivery_acknowledged",
            )} if terminal else None)
            if terminal and terminal["turn_id"] not in old_turns:
                if terminal["session_id"] != session:
                    raise AssertionError("terminal belongs to a different session")
                if terminal["status"] != "completed":
                    raise AssertionError("baseline turn failed: " + terminal["status"])
                if not terminal["execution_succeeded"] or not terminal["final_created"]:
                    raise AssertionError("completed terminal did not prove successful final creation")
                finals = [item for item in snapshot.get("messages", [])
                          if item["role"] == "assistant" and item["committed"]
                          and item.get("final_id") == terminal["final_id"]]
                if len(finals) != 1:
                    raise AssertionError("terminal did not match exactly one committed assistant final")
                entries = cls.session_entries(session)
                ingress = [entry for entry in entries if entry["id"] not in old_ids
                           and entry["payload"].get("payload") == "user_message"
                           and message_text(entry["payload"]["message"]["content"]) == text]
                if len(ingress) != 1:
                    raise AssertionError("submitted prompt did not match exactly one new committed ingress")
                obligations = [entry["payload"] for entry in entries
                               if entry["payload"].get("payload") == "turn_obligation"
                               and entry["payload"]["turn_id"] == terminal["turn_id"]
                               and entry["payload"]["user_entry_id"] == ingress[0]["id"]]
                if not obligations or (expected_action and any(
                        obligation["action_id"] != expected_action for obligation in obligations)):
                    raise AssertionError("terminal turn does not bind to the submitted prompt/action")
                canonical = [entry for entry in entries if entry["id"] == terminal["final_id"]
                             and entry["payload"].get("payload") == "assistant_final"
                             and entry["payload"]["turn_id"] == terminal["turn_id"]]
                if len(canonical) != 1 or message_text(
                        canonical[0]["payload"]["message"]["content"]) != finals[0]["text"]:
                    raise AssertionError("projected final disagrees with the canonical committed final")
                cls.turn_requests[(session, terminal["turn_id"])] = cls.completed_requests(offset, text)
                return snapshot, finals[0]
            time.sleep(0.5)
        raise AssertionError("baseline turn never reached a committed final")

    @classmethod
    def write_report(cls):
        changed = False
        for key, root in (("binaries", TARGET / "debug"),
                          ("launched_binaries", getattr(cls, "bin_root", cls.root / "bin"))):
            if key in cls.report:
                after = {name: digest(root / name) if (root / name).is_file() else None for name in BINS}
                cls.report[key + "_after"] = after
                cls.report[key + "_unchanged"] = after == cls.report[key]
                changed |= after != cls.report[key]
        if hasattr(cls, "proxy"):
            with cls.proxy.capture_lock:
                cls.report["provider_requests"] = [
                    {key: value for key, value in capture.items() if key != "request"}
                    for capture in cls.proxy.captures]
        (cls.artifacts / "baseline.json").write_text(json.dumps(cls.report, indent=2) + "\n")
        if changed:
            raise AssertionError("built or launched binaries changed during baseline execution")

    def test_ordinary_turn_commits_actual_answer(self):
        session = self.create_session("baseline-arithmetic-" + identity())
        snapshot, final = self.ask(session, "What is 2 + 2? Reply with only the numeral. Do not use tools.")
        requests = self.turn_requests[(session, snapshot["terminal"]["turn_id"])]
        self.report["observations"].append({"case": "ordinary_turn", "session_id": session,
            "initial_condition": "profile introduced through recorded real setup exchange",
            "onboarding_session_id": self.report["onboarding"]["session_id"],
            "final_id": final["final_id"], "committed": final["committed"],
            "turn_id": snapshot["terminal"]["turn_id"], "actual_provider_requests": len(requests),
            "models": sorted({capture["model"] for capture in requests}),
            "expected_exact_text": "4", "actual_text": final["text"][:2048],
            "actual_text_truncated": len(final["text"]) > 2048,
            "answer_sha256": hashlib.sha256(final["text"].encode()).hexdigest(),
            "exact_output_matched": final["text"].strip() == "4",
            "execution_succeeded": snapshot["terminal"]["execution_succeeded"]})
        self.assertEqual("4", final["text"].strip())
        self.assertTrue(snapshot["terminal"]["execution_succeeded"])

    def test_paraphrase_records_actual_source_and_context_hit_or_miss(self):
        source = self.create_session("baseline-memory-source-" + identity())
        fact = "At the waterfront workspace, emergency copies are kept in the cobalt locker."
        source_prompt = fact + " This is a synthetic fact for this test. Acknowledge briefly."
        self.ask(source, source_prompt)
        entries = self.session_entries(source)
        source_entries = [entry for entry in entries
                          if entry.get("payload", {}).get("payload") == "user_message"
                          and message_text(entry["payload"]["message"]["content"]) == source_prompt]
        self.assertTrue(source_entries, "synthetic source was not committed to real session history")
        source_ids = {entry["id"] for entry in source_entries}
        ingestion_started = time.monotonic()
        pre_query_anchors = self.source_anchors(source, source_ids)
        while not pre_query_anchors and time.monotonic() - ingestion_started < 15:
            time.sleep(0.25)
            pre_query_anchors = self.source_anchors(source, source_ids)
        ingestion = {"ready": bool(pre_query_anchors), "source_anchor_ids": pre_query_anchors,
                     "elapsed_seconds": round(time.monotonic() - ingestion_started, 3),
                     "timeout_seconds": 15}
        self.report["observations"].append({"case": "pre_query_ingestion", "source_session": source,
                                           "profile_id": self.profile["id"], **ingestion})
        target = self.create_session("baseline-memory-target-" + identity())
        snapshot, final = self.ask(target,
            "Where is the fallback archive stored at the shore office? Use existing memory if available; otherwise say unknown.")
        requests = self.turn_requests[(target, snapshot["terminal"]["turn_id"])]
        manifests = []
        for capture in requests:
            request = capture["request"]
            for message in request.get("messages", []):
                if message.get("role") != "system":
                    continue
                content = message_text(message.get("content"))
                for match in re.finditer(r"<retrieved_memory_manifest>\s*(.*?)\s*</retrieved_memory_manifest>", content, re.S):
                    manifest = json.loads(match.group(1))
                    self.assertEqual(manifest["profile_id"], self.profile["id"])
                    self.assertEqual(manifest["session_id"], target)
                    manifests.append(manifest)
        included = sorted({entry for manifest in manifests for item in dictionaries(manifest)
                           for entry in item.get("source_entries", []) if isinstance(entry, str)})
        anchors = self.source_anchors(source, source_ids)
        self.report["observations"].append({
            "case": "paraphrase", "source_session": source, "target_session": target,
            "profile_id": self.profile["id"], "turn_id": snapshot["terminal"]["turn_id"],
            "pre_query_ingestion": ingestion,
            "recall_measurement": "source_ready" if ingestion["ready"] else "ingestion_lag_or_missing",
            "models": sorted({capture["model"] for capture in requests}),
            "source_entry_ids": sorted(source_ids), "source_anchor_ids": sorted(set(anchors)),
            "actual_provider_requests": len(requests), "actual_manifest_count": len(manifests),
            "included_source_entry_ids": included, "source_included": bool(source_ids.intersection(included)),
            "answer_uses_test_value": "cobalt locker" in final["text"].lower(),
            "final_id": final["final_id"], "committed": final["committed"],
            "baseline_semantic_capability_claim": False,
        })
        # This measures the existing baseline, including an honest recall miss.
        # Semantic hit thresholds belong to future implementation acceptance.
        self.assertTrue(snapshot["terminal"]["execution_succeeded"])


if __name__ == "__main__":
    unittest.main()
