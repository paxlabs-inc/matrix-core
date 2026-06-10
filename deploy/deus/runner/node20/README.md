# Deus node20 function template (Paxeer Cloud)

The bundle `internal/hosting` uploads when deploying a hosted listing. The
uploaded artifact is expected to be a **gzipped tar (`code.tar.gz`)** of this
project layout; the Appwrite deployment runs `npm install` and uses
`src/main.js` as the entrypoint.

## Layout

- `src/main.js` — Appwrite entrypoint (`export default async ({ req, res, log, error })`); parses the invoke payload and returns the response envelope
- `src/server.js` — local dev HTTP server exposing `POST /invoke` (not used by Appwrite)
- `src/dispatch.js` — shared invoke logic: deadline, max-response-bytes cap, optional runner co-signature
- `src/harness.js` — `runHandle` runs the developer handler under a hard deadline
- `src/handler.js` — developer `handle(operation, args, ctx)` implementation
- `package.json` — node20 ESM metadata

## Invoke contract

Request (gateway → function), as JSON:

```json
{
  "invocation_id": "…",
  "operation": "echo",
  "args": { "message": "hi" },
  "caller_did": "did:…",
  "deadline_ms": 5000,
  "receipt_digest": "0x…"
}
```

Response (function → gateway):

```json
{ "outcome": "ok", "result": { "echo": "hi" }, "units": "1", "runner_sig": "0x…" }
```

`outcome` is `ok` or `error`. `runner_sig` is included only when
`RUNNER_SIGNING_KEY` is set; it is an HMAC-SHA256 co-signature over
`receipt_digest` (stub for the production runner key scheme).

## Function variables (set on deploy)

- `DEUS_MAX_RESPONSE_BYTES` — response cap enforced by `dispatch.js`
- `RUNNER_SIGNING_KEY` — optional; enables `runner_sig` co-signing
- any per-service `Env` entries threaded from the deploy request

## Local dev

```bash
npm run check
PORT=18080 npm start
curl -s localhost:18080/invoke -H 'content-type: application/json' \
  -d '{"operation":"echo","args":{"message":"hi"},"caller_did":"did:test","invocation_id":"1","deadline_ms":5000}'
```

Set `DEUS_HOSTING_DEV_EXEC_URL=http://127.0.0.1:18080` when running `deusd` in
dev to route deploys to a local runner.
