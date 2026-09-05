"""Hosted-provider binding capture, correction, and actual workspace dispatch.

The original runtime baseline supplies the real daemon, worker, authenticated
Web transport, and transparent OpenRouter proxy. Target bindings and task
dependencies must be produced by ordinary user turns and production tools.
"""

from __future__ import annotations

import hashlib
import http.cookiejar
import importlib.util
import json
from pathlib import Path
import unittest
import urllib.error
import urllib.parse
import urllib.request


_spec = importlib.util.spec_from_file_location(
    "_causal_bindings_baseline", Path(__file__).with_name("test_runtime_baseline.py"))
baseline = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(baseline)
baseline.TARGET = Path("/tmp/keith-causal-bindings-build")


def canonical_digest(value):
    encoded = json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()
    return hashlib.sha256(encoded).hexdigest()


class RuntimeBindings(baseline.RuntimeBaseline):
    @classmethod
    def prepare_runtime(cls):
        super().prepare_runtime()
        cls.report["qualification_scope"] = {
            "task": "2.2",
            "required_test_scope": [
                "natural user mapping captured by actual source-cited memory tool",
                "correction retains exact entity/property and original provenance",
                "fresh-session exact lookup reaches actual provider context",
                "bound read executes at the current permitted path",
                "durable binding and current dependent read survive restart",
            ],
            "not_claimed": [
                "10,000-distractor domain cases by this hosted-provider group",
                "all adversarial admission cases by this hosted-provider group",
                "semantic retrieval or later full automatic activation qualification",
                "external truth established by an inferred binding association",
            ],
        }
        cls.report["binding_observations"] = []
        cls.report["restart_observations"] = []

    @classmethod
    def write_report(cls):
        super().write_report()
        (cls.artifacts / "baseline.json").rename(cls.artifacts / "bindings.json")

    @classmethod
    def session_entries(cls, session):
        entries = super().session_entries(session)
        seen = set()
        for entry in entries:
            if entry["id"] in seen:
                raise AssertionError("canonical binding history duplicated an entry identity")
            expected = canonical_digest({key: value for key, value in entry.items() if key != "checksum"})
            if entry["checksum"] != expected:
                raise AssertionError("canonical binding history checksum did not verify")
            if entry["parent_id"] is not None and entry["parent_id"] not in seen:
                raise AssertionError("canonical binding history lost its preceding parent")
            seen.add(entry["id"])
        return entries

    @classmethod
    def ask_with_entries(cls, session, prompt):
        prior = {entry["id"] for entry in cls.session_entries(session)}
        snapshot, answer = cls.ask(session, prompt)
        entries = [entry for entry in cls.session_entries(session) if entry["id"] not in prior]
        ingresses = [entry for entry in entries
                     if entry["payload"].get("payload") == "user_message"
                     and baseline.message_text(entry["payload"]["message"]["content"]) == prompt]
        if len(ingresses) != 1:
            raise AssertionError("binding turn did not have one exact committed user source")
        return snapshot, answer, entries, ingresses[0]

    @staticmethod
    def tool_pairs(entries, name):
        pairs = []
        for index, entry in enumerate(entries):
            call = entry["payload"]
            if call.get("payload") != "tool_call" or call["name"] != name:
                continue
            results = [candidate for candidate in entries[index + 1:]
                       if candidate["payload"].get("payload") == "tool_result"
                       and candidate["payload"]["call_id"] == call["call_id"]]
            if len(results) != 1:
                raise AssertionError("actual tool call does not have one subsequent committed result")
            pairs.append((entry, results[0]))
        return pairs

    def successful_json(self, pair):
        call, result = pair
        self.assertFalse(result["payload"]["is_error"],
                         "required memory tool returned an execution error")
        self.assertIsNone(result["payload"].get("failure"))
        raw = baseline.message_text(result["payload"]["content"])
        value = json.loads(raw)
        self.assertIsInstance(value, dict)
        self.report["binding_observations"].append({
            "boundary": "actual_committed_tool_result",
            "tool_name": call["payload"]["name"],
            "call_id": call["payload"]["call_id"],
            "call_entry_id": call["id"], "call_checksum": call["checksum"],
            "result_entry_id": result["id"], "result_checksum": result["checksum"],
            "result_sha256": hashlib.sha256(raw.encode()).hexdigest(),
        })
        return value

    def assert_source_cited_mapping(self, call, ingress, value, *, alias=None):
        arguments = call["payload"]["arguments"]
        source_text = baseline.message_text(ingress["payload"]["message"]["content"])
        self.assertEqual(arguments["source_entry_id"], ingress["id"])
        quote = arguments["evidence_quote"]
        self.assertIsInstance(quote, str)
        self.assertTrue(quote)
        self.assertIn(quote, source_text)
        binding = arguments["binding"]
        self.assertEqual(binding["value_quote"], value)
        self.assertIn(value, quote)
        if alias is not None:
            self.assertEqual(binding["entity"], {"mode": "new_alias", "alias": alias})
            self.assertEqual(binding["property"], "status_path")
            self.assertEqual(binding["target_kind"], "workspace_path")
        self.report["binding_observations"].append({
            "boundary": "natural_source_mapping",
            "source_entry_id": ingress["id"], "source_checksum": ingress["checksum"],
            "source_quote_sha256": hashlib.sha256(quote.encode()).hexdigest(),
            "tool_call_id": call["payload"]["call_id"],
            "captured_value": value,
            "association_truth_claim": "attributed mapping; no external truth certification",
        })

    def assert_actual_read(self, session, snapshot, answer, entries, path, content, binding_id):
        reads = self.tool_pairs(entries, "read")
        self.assertEqual(len(reads), 1, "dependent task must execute one actual workspace read")
        call, result = reads[0]
        self.assertEqual(call["payload"]["arguments"]["path"], path)
        self.assertFalse(result["payload"]["is_error"])
        self.assertIsNone(result["payload"].get("failure"))
        self.assertEqual(baseline.message_text(result["payload"]["content"]), content)
        self.assertIn(content.strip(), answer["text"], "final did not contain the file's fresh value")
        forbidden = {"write", "bash", "kernel", "web_fetch", "browser"}
        self.assertFalse([entry["id"] for entry in entries
                          if entry["payload"].get("payload") == "tool_call"
                          and entry["payload"]["name"] in forbidden],
                         "binding fixture escaped its requested memory lookup and file read")
        lookups = self.tool_pairs(entries, "memory_context")
        self.assertTrue(lookups, "natural task did not exercise exact memory-context lookup")
        read_position = entries.index(call)
        lookup_ids = {
            lookup["payload"]["call_id"] for lookup, response in lookups
            if entries.index(response) < read_position and not response["payload"]["is_error"]
        }
        self.assertTrue(lookup_ids, "binding lookup did not precede dependent dispatch")
        captures = self.turn_requests[(session, snapshot["terminal"]["turn_id"])]
        included = []
        for capture in captures:
            messages = capture["request"].get("messages", [])
            already_in_history = any(
                item.get("id") == call["payload"]["call_id"]
                for message in messages for item in message.get("tool_calls", []))
            if already_in_history:
                continue
            for message in messages:
                text = baseline.message_text(message.get("content"))
                if (message.get("role") == "tool"
                        and message.get("tool_call_id") in lookup_ids
                        and binding_id in text and path in text):
                    included.append(capture["request_sha256"])
        self.assertTrue(included, "resolved binding did not reach provider context before actual read")
        self.report["binding_observations"].append({
            "boundary": "actual_context_and_dependent_read",
            "session_id": session, "turn_id": snapshot["terminal"]["turn_id"],
            "binding_id": binding_id, "read_call_id": call["payload"]["call_id"],
            "read_call_entry_id": call["id"], "read_call_checksum": call["checksum"],
            "read_result_entry_id": result["id"], "read_result_checksum": result["checksum"],
            "actual_path": path,
            "actual_content_sha256": hashlib.sha256(content.encode()).hexdigest(),
            "provider_request_hashes_before_read": included,
            "final_id": snapshot["terminal"]["final_id"],
            "context_inclusion_is_dispatch_proof": False,
        })
        return call

    @classmethod
    def restart_runtime(cls):
        before = {name: baseline.digest(cls.bin_root / name) for name in baseline.BINS}
        cls.stop_processes()
        exited = [process.returncode for process in cls.processes]
        cls.processes = []
        socket_path = cls.root / "agentd.sock"
        if socket_path.exists():
            socket_path.unlink()
        cls.launch(cls.daemon_argv, "daemon-restarted")
        cls.await_condition(lambda: socket_path.is_socket(), "restarted daemon socket", timeout=180)
        cls.launch(cls.web_argv, "web-restarted", {"KEITH_WEB_LOGIN_SECRET": cls.login_secret})
        cls.cookies = http.cookiejar.CookieJar()
        cls.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cls.cookies))
        cls.await_condition(cls.web_ready, "restarted Web listener", timeout=180)
        try:
            cls.http.open(urllib.request.Request(
                cls.origin + "/auth/session",
                data=urllib.parse.urlencode({"password": cls.login_secret}).encode(),
                headers={"Origin": cls.origin, "Content-Type": "application/x-www-form-urlencoded"},
            ), timeout=30).close()
        except urllib.error.HTTPError as error:
            if error.code not in (404, 503) or not list(cls.cookies):
                raise AssertionError("restarted Web login failed") from None
        cls.bootstrap = cls.get_json("/api/bootstrap")
        if cls.profile["id"] not in {profile["id"] for profile in cls.bootstrap["profiles"]}:
            raise AssertionError("profile identity changed on restart")
        after = {name: baseline.digest(cls.bin_root / name) for name in baseline.BINS}
        cls.report["restart_observations"].append({
            "stopped_process_exit_codes": exited,
            "launched_binary_hashes_unchanged": before == after,
            "profile_id": cls.profile["id"],
            "restart_kind": "daemon/Web stop and reopen against the same durable stores",
        })
        if before != after:
            raise AssertionError("restart changed launched binary identity")


def load_tests(_loader, _tests, _pattern):
    # No inherited baseline case can substitute for the required binding proof.
    return unittest.TestSuite()


if __name__ == "__main__":
    unittest.main()
