# Tool Surface

Chronos exposes three MCP tools to agents: `alarm_set`, `alarm_list`, and `alarm_cancel`. The MCP stdio proxy (`tools/chronos/chronos.mjs`) mirrors the tachyon/uwac pattern: `initialize`/`tools/list` are answered locally from `chronos-tools.json` (so an unreachable chronosd never bricks daemon boot), and `tools/call` is forwarded to `MATRIX_CHRONOS_URL`.

Source files: `tools/chronos/chronos.mjs`, `tools/chronos/chronos-tools.json`.

---

## Design decisions

**Local tool listing.** The proxy answers `tools/list` from a baked JSON file. If chronosd is unreachable, the daemon still boots — tools just fail at call time with a clear error.

**Agent-DID auth per call.** The proxy mints a fresh agent-DID principal token for every `tools/call` by signing a challenge with the daemon's `executor.key`. No long-lived tokens are stored.

**Strict bijection.** The tool set declared in `chronos-tools.json` MUST exactly match the tools declared in `agents/default.json` and `agents/neo.json`. The daemon's `Manager.verifyTools` enforces this at boot — any mismatch is fatal (invariant i8).

---

## The three tools

### `alarm_set`

Schedule a new alarm.

**Input:**
```json
{
  "label": "daily portfolio summary",
  "kind": "once",
  "delay_seconds": 600,
  "wake_message": "Check if the transaction confirmed on Paxeer chain 125…",
  "payload": {"tx_hash": "0xABC…", "chain_id": 125},
  "conversation_id": "conv_abc123",
  "idempotency_key": "check-tx-0xABC"
}
```

For cron:
```json
{
  "label": "hourly health check",
  "kind": "cron",
  "cron_expr": "@hourly",
  "timezone": "UTC",
  "wake_message": "Run the system health check: verify RPC, check balances…",
  "payload": {"check": "health", "thresholds": {}},
  "idempotency_key": "health-check-v1"
}
```

**Output:**
```json
{
  "id": "uuid-of-the-alarm",
  "next_fire_at": "2026-06-15T08:10:00Z",
  "status": "active"
}
```

### `alarm_list`

List the caller's own alarms.

**Input:**
```json
{
  "limit": 50
}
```

**Output:**
```json
{
  "alarms": [ … ],
  "count": 3
}
```

### `alarm_cancel`

Cancel one of the caller's alarms by ID.

**Input:**
```json
{
  "alarm_id": "uuid-of-the-alarm"
}
```

**Output:** the alarm view with `status: "cancelled"`.

---

## Proxy architecture

```
┌─────────────────────────────────────────┐
│  chronos.mjs (stdio MCP proxy)          │
│                                          │
│  initialize() → {protocolVersion,        │
│                  serverInfo,             │
│                  capabilities}           │
│                                          │
│  tools/list() → reads chronos-tools.json │
│                  from disk (local)       │
│                                          │
│  tools/call(name, args)                  │
│       │                                  │
│       ├── 1. Read executor.key           │
│       ├── 2. POST /v1/agent/auth/        │
│       │      challenge → nonce           │
│       ├── 3. Sign challenge message      │
│       ├── 4. POST /v1/agent/auth/        │
│       │      verify → principal token    │
│       ├── 5. POST /v1/alarms (or GET/    │
│       │      DELETE) with transport      │
│       │      bearer + X-Chronos-Agent    │
│       └── 6. Return result to MCP client │
└─────────────────────────────────────────┘
```

---

## Selftest

```bash
node tools/chronos/chronos.mjs --selftest
```

The selftest is an offline drift guard. It:

1. Reads `chronos-tools.json`
2. Reads each `agents/*.json` that declares a `chronos` server
3. Asserts the tool sets are identical (same names, same count)
4. Exits 0 on match, non-zero on mismatch

This is the same pattern as `tachyon.mjs --selftest`. The daemon's `Manager.verifyTools` runs it at boot.

---

## Manifest wiring

### agents/default.json

```json
{
  "servers": [
    {
      "alias": "chronos",
      "command": "node /root/matrix/tools/chronos/chronos.mjs",
      "env": {
        "MATRIX_CHRONOS_URL": "${MATRIX_CHRONOS_URL}",
        "MATRIX_CHRONOS_TOKEN": "${MATRIX_CHRONOS_TOKEN}"
      }
    }
  ]
}
```

### agents/neo.json

Same server entry. Neo is the primary consumer — it schedules tasks via `alarm_set` and resumes via the wake delivery path.

### skills/paxeer-assistant SKILL.mtx

The three tool URIs should be added to `§TOOLS` so the freeform hero path can schedule:

```mtx
§TOOLS
  matrix://tool/mcp/chronos/alarm_set@0.1.0
  matrix://tool/mcp/chronos/alarm_list@0.1.0
  matrix://tool/mcp/chronos/alarm_cancel@0.1.0
end
```

---

## chronos-tools.json

The baked tool definitions:

```json
{
  "tools": [
    {
      "name": "alarm_set",
      "description": "Schedule an alarm…",
      "inputSchema": { … }
    },
    {
      "name": "alarm_list",
      "description": "List your scheduled alarms…",
      "inputSchema": { … }
    },
    {
      "name": "alarm_cancel",
      "description": "Cancel a scheduled alarm by ID…",
      "inputSchema": { … }
    }
  ]
}
```

Exactly three tools in v1. The proxy, the manifest, and the skill must all agree on this set.

---

## Available to

Chronos tools are available to BOTH Neo and the MCL pipeline — it's an ordinary MCP server. Any agent with a valid transport bearer + agent DID can schedule alarms. The wake always delivers into the agent's `/chat` endpoint, so the receiving agent must be Neo (or Neo-compatible).

---

## Error handling at the proxy level

If chronosd is unreachable:
- `tools/list` still succeeds (local)
- `tools/call` fails with a clear transport error surfaced to the LLM

If the agent auth handshake fails:
- The error is surfaced to the LLM (the proxy does not retry auth)
- The LLM can decide to re-attempt or report the failure

If chronosd returns a non-2xx:
- The error code + message are passed through to the LLM
- The LLM sees structured errors (`invalid_request`, `not_found`, etc.)
