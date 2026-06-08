# Deus node20 function template (Paxeer Cloud)

Templates uploaded by `internal/hosting` when deploying hosted listings.

## Layout

- `src/main.js` — Appwrite entrypoint; loads harness + developer handler
- `src/handler.js` — developer `handle(operation, args, ctx)` implementation
- `package.json` — node20 ESM dependencies

## Local dev

From `deus/runner/`:

```bash
npm run check
PORT=18080 npm start
curl -s localhost:18080/invoke -H 'content-type: application/json' \
  -d '{"operation":"echo","args":{"message":"hi"},"caller_did":"did:test","invocation_id":"1","deadline_ms":5000}'
```

Set `DEUS_HOSTING_DEV_EXEC_URL=http://127.0.0.1:18080` when running `deusd` in dev to route deploys to a local runner.
