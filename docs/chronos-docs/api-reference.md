# API Reference

Package `chronos/internal/server` exposes chronosd over HTTP. All `/v1/*` endpoints require two-layer auth: the transport bearer on every request, and the agent principal token (`X-Chronos-Agent` header) on alarm endpoints.

Source file: `internal/server/server.go`.

---

## Base URL

```
http://127.0.0.1:9096    (box-local, default port)
```

In production, agents reach it through the public nginx route (e.g. `https://matrix.paxeer.app/chronos/` → `127.0.0.1:9096`).

---

## Response envelope

Every response uses the uniform `{ok, data, error}` envelope:

```json
// Success
{"ok": true, "data": { … }}

// Failure
{"ok": false, "error": {"code": "unauthorized", "message": "…"}}
```

Error codes (stable; callers may branch on these):

| Code | HTTP status | Meaning |
|---|---|---|
| `invalid_request` | 400 | Malformed body, missing required field, invalid kind |
| `unauthorized` | 401 | Missing/invalid transport bearer or principal token |
| `not_found` | 404 | Alarm id unknown or not owned by the caller |
| `conflict` | 409 | Idempotency key clash with a different alarm |
| `internal` | 500 | Database error or unexpected failure |

---

## Public endpoints

### `GET /healthz`

No auth. Returns service health + DB connectivity.

```
→ 200
{
  "ok": true,
  "data": {
    "status": "ok",
    "version": "0.1.0",
    "db": true
  }
}
```

`db: false` → HTTP 503. The router's `waitDaemonReady` equivalent for chronosd would poll this.

### `GET /`

No auth. Returns service identity.

```
→ 200
{
  "ok": true,
  "data": {
    "service": "chronosd",
    "version": "0.1.0",
    "health": "/healthz"
  }
}
```

---

## Agent auth endpoints

### `POST /v1/agent/auth/challenge`

Transport auth required. Requests a challenge nonce for a DID.

**Request:**
```json
{
  "did": "did:matrix:11111111-2222-3333-4444-555555555555:0123456789abcdef"
}
```

**Response (200):**
```json
{
  "ok": true,
  "data": {
    "did": "did:matrix:11111111-2222-3333-4444-555555555555:0123456789abcdef",
    "nonce": "base64url-encoded-24-random-bytes",
    "message": "matrix-chronos-auth:did:matrix:…:nonce",
    "expires_in": 120
  }
}
```

**Errors:**
- 400 `invalid_request` — malformed DID

### `POST /v1/agent/auth/verify`

Transport auth required. Proves possession of the DID's ed25519 key.

**Request:**
```json
{
  "did": "did:matrix:11111111-2222-3333-4444-555555555555:0123456789abcdef",
  "public_key": "hex-encoded-32-byte-ed25519-public-key",
  "nonce": "the-nonce-from-challenge",
  "signature": "hex-encoded-ed25519-signature-of-the-challenge-message"
}
```

**Response (200):**
```json
{
  "ok": true,
  "data": {
    "token": "base64url-payload.base64url-mac",
    "owner_user_id": "11111111-2222-3333-4444-555555555555",
    "expires_in": 86400
  }
}
```

**Errors:**
- 401 `unauthorized` — unknown/expired/used nonce, signature verification failed, or public key doesn't match DID fingerprint
- 400 `invalid_request` — malformed DID after successful signature (edge case)

---

## Alarm endpoints

All alarm endpoints require BOTH transport auth AND a valid `X-Chronos-Agent` principal token. Owner scoping is enforced from the token's DID — never from a request field.

### `POST /v1/alarms`

Create an alarm.

**Headers:**
```
Authorization: Bearer <CHRONOS_TOKEN>
X-Chronos-Agent: <principal-token>
```

**Request:**
```json
{
  "label": "daily portfolio summary",
  "kind": "cron",
  "cron_expr": "0 9 * * *",
  "timezone": "America/New_York",
  "conversation_id": "conv_abc123",
  "wake_message": "Generate the daily portfolio summary: check balances…",
  "payload": {"task": "daily_summary", "last_run": null},
  "idempotency_key": "daily-summary-v1",
  "max_failures": 3
}
```

For `kind=once`, use exactly one of `delay_seconds` or `fire_at`:
```json
{
  "kind": "once",
  "delay_seconds": 600,
  "wake_message": "Check if the transaction confirmed…"
}
```
```json
{
  "kind": "once",
  "fire_at": "2026-06-16T09:00:00Z",
  "wake_message": "Meeting reminder: …"
}
```

**Response (200):**
```json
{
  "ok": true,
  "data": {
    "id": "uuid-of-the-alarm",
    "next_fire_at": "2026-06-16T13:00:00Z",
    "status": "active"
  }
}
```

**Errors:**
- 400 `invalid_request` — `wake_message` is empty, invalid kind, invalid schedule parameters
- 401 `unauthorized` — missing/invalid transport bearer or principal token
- 500 `internal` — database error

**Idempotency:** When `idempotency_key` is non-empty and matches an existing alarm for the same owner, the existing alarm is returned (same 200 shape). No duplicate is created.

### `GET /v1/alarms`

List the caller's own alarms.

**Headers:** same as create.

**Query params:**
- `limit` (int, default 100, max 500)

**Response (200):**
```json
{
  "ok": true,
  "data": {
    "alarms": [
      {
        "id": "uuid",
        "label": "daily portfolio summary",
        "kind": "cron",
        "cron_expr": "0 9 * * *",
        "timezone": "America/New_York",
        "next_fire_at": "2026-06-16T13:00:00Z",
        "conversation_id": "conv_abc123",
        "wake_message": "Generate the daily portfolio summary…",
        "payload": {"task": "daily_summary"},
        "status": "active",
        "idempotency_key": "daily-summary-v1",
        "max_failures": 3,
        "failure_count": 0,
        "last_error": "",
        "created_at": "2026-06-15T08:00:00Z",
        "last_fired_at": null
      }
    ],
    "count": 1
  }
}
```

### `GET /v1/alarms/{id}`

Get one alarm by ID. Owner-checked.

**Headers:** same as create.

**Response (200):** single alarm view (same shape as list items).

**Errors:**
- 404 `not_found` — alarm unknown or not owned by caller

### `DELETE /v1/alarms/{id}`

Cancel an active alarm. Owner-checked. Idempotent — already-fired or already-cancelled alarms return success.

**Headers:** same as create.

**Response (200):** the alarm view with `status: "cancelled"`.

**Errors:**
- 404 `not_found` — alarm unknown or not owned by caller

---

## Internal note

All `/v1/alarms*` require BOTH the transport bearer AND a valid agent token. The transport bearer alone is not sufficient to scope to an owner. The agent token alone is not sufficient to prove the caller is a legitimate daemon. Both must be present.
