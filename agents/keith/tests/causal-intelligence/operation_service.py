#!/usr/bin/env python3
"""Separate durable HTTP effect service for externally injected acknowledgement loss.

This is a test integration target, not a Keith collaborator. SQLite is its own
authoritative world: every committed operation changes a named integer counter.
"""

from __future__ import annotations

import argparse
from contextlib import closing
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import socket
import sqlite3
import sys
from urllib.parse import unquote, urlsplit


MAX_BODY = 16 * 1024
NAME = re.compile(r"[a-zA-Z0-9_.-]{1,128}\Z")


def connect(database: Path) -> sqlite3.Connection:
    connection = sqlite3.connect(database, timeout=10, isolation_level=None)
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA synchronous=FULL")
    return connection


def initialize(database: Path, drop_once: bool) -> None:
    database.parent.mkdir(parents=True, exist_ok=True)
    with closing(connect(database)) as connection:
        connection.execute("PRAGMA journal_mode=WAL")
        connection.executescript("""
            CREATE TABLE IF NOT EXISTS effects (
                sequence INTEGER PRIMARY KEY AUTOINCREMENT,
                scope TEXT NOT NULL,
                operation_key TEXT NOT NULL,
                payload_digest TEXT NOT NULL,
                target TEXT NOT NULL,
                delta INTEGER NOT NULL,
                value_after INTEGER NOT NULL,
                UNIQUE(scope, operation_key)
            );
            CREATE TABLE IF NOT EXISTS counters (
                scope TEXT NOT NULL,
                target TEXT NOT NULL,
                value INTEGER NOT NULL,
                PRIMARY KEY(scope, target)
            );
            CREATE TABLE IF NOT EXISTS fault_state (
                id INTEGER PRIMARY KEY CHECK(id = 1),
                drops_remaining INTEGER NOT NULL CHECK(drops_remaining IN (0, 1))
            );
        """)
        connection.execute("INSERT OR IGNORE INTO fault_state VALUES(1, ?)", (int(drop_once),))


def receipt(row: sqlite3.Row) -> dict:
    return {"schema_version": 1, "effect": "committed", **dict(row)}


def commit(database: Path, request: dict) -> tuple[int, dict, bool]:
    if set(request) != {"scope", "operation_key", "target", "delta"}:
        return 400, {"error": "invalid_fields"}, False
    if any(not isinstance(request[key], str) or NAME.fullmatch(request[key]) is None for key in ["scope", "operation_key", "target"]):
        return 400, {"error": "invalid_identity"}, False
    if type(request["delta"]) is not int or not -10000 <= request["delta"] <= 10000:
        return 400, {"error": "invalid_delta"}, False
    payload = {key: request[key] for key in ["target", "delta"]}
    payload_digest = hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    with closing(connect(database)) as connection:
        connection.execute("BEGIN IMMEDIATE")
        row = connection.execute("SELECT * FROM effects WHERE scope=? AND operation_key=?", (request["scope"], request["operation_key"])).fetchone()
        if row is not None:
            connection.commit()
            if row["payload_digest"] != payload_digest:
                return 409, {"error": "operation_payload_conflict"}, False
            return 200, receipt(row), False
        old = connection.execute("SELECT value FROM counters WHERE scope=? AND target=?", (request["scope"], request["target"])).fetchone()
        value = (old["value"] if old else 0) + request["delta"]
        if not -(2**63) <= value < 2**63:
            connection.rollback()
            return 409, {"error": "counter_overflow"}, False
        connection.execute("INSERT INTO counters VALUES(?,?,?) ON CONFLICT(scope,target) DO UPDATE SET value=excluded.value", (request["scope"], request["target"], value))
        cursor = connection.execute("INSERT INTO effects(scope,operation_key,payload_digest,target,delta,value_after) VALUES(?,?,?,?,?,?)", (request["scope"], request["operation_key"], payload_digest, request["target"], request["delta"], value))
        row = connection.execute("SELECT * FROM effects WHERE sequence=?", (cursor.lastrowid,)).fetchone()
        drop = bool(connection.execute("SELECT drops_remaining FROM fault_state WHERE id=1").fetchone()[0])
        if drop:
            connection.execute("UPDATE fault_state SET drops_remaining=0 WHERE id=1")
        connection.commit()  # Actual counter/effect and consumed fault survive restart together.
        return 201, receipt(row), drop


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, *_args) -> None:
        pass

    def respond(self, code: int, value: dict) -> None:
        body = json.dumps(value, sort_keys=True).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def do_POST(self) -> None:
        if self.path != "/effects" or self.headers.get("Transfer-Encoding"):
            self.respond(400, {"error": "invalid_request"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= MAX_BODY:
                self.respond(413, {"error": "body_limit"})
                return
            raw = self.rfile.read(length)
            request = json.loads(raw)
            if len(raw) != length or not isinstance(request, dict):
                raise ValueError("invalid body")
            code, value, drop = commit(self.server.database, request)
        except (ValueError, TypeError):
            self.respond(400, {"error": "invalid_json"})
            return
        except sqlite3.Error:
            self.respond(503, {"error": "store_unavailable"})
            return
        if drop:
            self.close_connection = True
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        self.respond(code, value)

    def do_GET(self) -> None:
        if self.path == "/health":
            self.respond(200, {"schema_version": 1, "ready": True})
            return
        parts = [unquote(value) for value in urlsplit(self.path).path.strip("/").split("/")]
        if len(parts) != 3 or parts[0] != "operations" or any(NAME.fullmatch(value) is None for value in parts[1:]):
            self.respond(404, {"error": "unknown_route"})
            return
        with closing(connect(self.server.database)) as connection:
            row = connection.execute("SELECT * FROM effects WHERE scope=? AND operation_key=?", parts[1:]).fetchone()
        self.respond(200, receipt(row)) if row is not None else self.respond(404, {"effect": "not_found"})


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", type=Path, required=True)
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--drop-ack-once", action="store_true")
    args = parser.parse_args()
    initialize(args.database, args.drop_ack_once)
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    server.daemon_threads = True
    server.database = args.database
    print(json.dumps({"schema_version": 1, "origin": f"http://127.0.0.1:{server.server_port}"}), flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
