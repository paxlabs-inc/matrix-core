"""Real HTTP/process/SQLite tests of the external fault target, not Keith wiring."""

from concurrent.futures import ThreadPoolExecutor
import http.client
import json
from pathlib import Path
import selectors
import sqlite3
import subprocess
import sys
import tempfile
import unittest
import urllib.error
import urllib.request


SERVICE = Path(__file__).with_name("operation_service.py")


class OperationServiceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="keith-operation-target-")
        self.addCleanup(self.directory.cleanup)
        self.database = Path(self.directory.name) / "effects.sqlite3"
        self.process = None
        self.addCleanup(self.stop)
        self.start()

    def start(self, drop=False):
        argv = [sys.executable, str(SERVICE), "--database", str(self.database)]
        if drop:
            argv.append("--drop-ack-once")
        self.process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        with selectors.DefaultSelector() as selector:
            selector.register(self.process.stdout, selectors.EVENT_READ)
            self.assertTrue(selector.select(timeout=10), "service did not announce readiness")
        self.origin = json.loads(self.process.stdout.readline())["origin"]
        self.assertEqual(self.get("/health")[1]["ready"], True)

    def stop(self):
        if self.process is not None:
            self.process.kill()
            self.process.wait(timeout=10)
            self.process.stdout.close()
            self.process.stderr.close()
            self.process = None

    def request(self, path, payload=None, raw=None):
        body = json.dumps(payload).encode() if payload is not None else raw
        request = urllib.request.Request(self.origin + path, data=body, headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.load(error)

    def get(self, path):
        return self.request(path)

    @staticmethod
    def operation(key="op-one", scope="profile-one", delta=7):
        return {"scope": scope, "operation_key": key, "target": "completed-items", "delta": delta}

    def rows(self, table):
        self.assertIn(table, {"effects", "counters", "fault_state"})
        with sqlite3.connect(self.database.as_uri() + "?mode=ro", uri=True) as connection:
            connection.row_factory = sqlite3.Row
            return [dict(row) for row in connection.execute(f"SELECT * FROM {table}")]

    def test_commit_changes_authoritative_counter_and_returns_receipt(self):
        code, receipt = self.request("/effects", self.operation())
        self.assertEqual(code, 201)
        self.assertEqual(receipt["effect"], "committed")
        self.assertEqual(self.rows("counters")[0]["value"], 7)
        self.assertEqual(receipt["sequence"], self.rows("effects")[0]["sequence"])

    def test_duplicate_returns_same_receipt_without_second_effect(self):
        first = self.request("/effects", self.operation())[1]
        code, second = self.request("/effects", self.operation())
        self.assertEqual(code, 200)
        self.assertEqual(first, second)
        self.assertEqual(len(self.rows("effects")), 1)
        self.assertEqual(self.rows("counters")[0]["value"], 7)

    def test_reused_key_with_changed_payload_is_rejected(self):
        self.request("/effects", self.operation())
        self.assertEqual(self.request("/effects", self.operation(delta=8))[0], 409)
        self.assertEqual(len(self.rows("effects")), 1)

    def test_lost_ack_then_kill_restart_readback_preserves_one_effect(self):
        self.stop()
        # Start a fresh fault-enabled store; the flag never resets existing state.
        self.database = Path(self.directory.name) / "fault.sqlite3"
        self.start(drop=True)
        with self.assertRaises((http.client.RemoteDisconnected, urllib.error.URLError, ConnectionError)):
            self.request("/effects", self.operation())
        self.assertEqual(self.rows("counters")[0]["value"], 7)
        self.stop()
        self.start(drop=True)
        code, observed = self.get("/operations/profile-one/op-one")
        self.assertEqual(code, 200)
        self.assertEqual(self.request("/effects", self.operation())[1], observed)
        self.assertEqual(len(self.rows("effects")), 1)
        self.assertEqual(self.rows("fault_state")[0]["drops_remaining"], 0)
        self.assertEqual(self.request("/effects", self.operation(key="op-two"))[0], 201)

    def test_concurrent_duplicate_requests_commit_once(self):
        with ThreadPoolExecutor(max_workers=6) as executor:
            receipts = list(executor.map(lambda _: self.request("/effects", self.operation()), range(12)))
        self.assertEqual(sum(code == 201 for code, _ in receipts), 1)
        self.assertEqual(len({value["sequence"] for _, value in receipts}), 1)
        self.assertEqual(len(self.rows("effects")), 1)

    def test_operation_keys_are_scoped(self):
        self.request("/effects", self.operation(scope="profile-one"))
        self.request("/effects", self.operation(scope="profile-two"))
        self.assertEqual(len(self.rows("effects")), 2)
        self.assertEqual(self.get("/operations/profile-three/op-one")[0], 404)

    def test_malformed_and_oversized_requests_create_no_effect(self):
        for raw, status in [(b"{", 400), (b"[]", 400), (b"x" * 17000, 413)]:
            with self.subTest(status=status):
                self.assertEqual(self.request("/effects", raw=raw)[0], status)
        for payload in [{}, self.operation(scope="../escape"), self.operation(delta=True)]:
            self.assertEqual(self.request("/effects", payload)[0], 400)
        self.assertEqual(self.rows("effects"), [])

    def test_restart_keeps_receipt_and_effect_contents(self):
        before = self.request("/effects", self.operation())[1]
        self.stop()
        self.start()
        self.assertEqual(self.get("/operations/profile-one/op-one")[1], before)
        self.assertEqual(self.rows("counters")[0]["value"], 7)


if __name__ == "__main__":
    unittest.main()
