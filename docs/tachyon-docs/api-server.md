# API Server

**Source file:** `pkg/api/server.go`

The HTTP API server exposes REST endpoints and JSON-RPC on a single listener. It is the primary transport for agent integration when the daemon runs as a service.

---

## Design decisions

### Single listener, dual protocol

REST and JSON-RPC share the same HTTP server. REST endpoints are at `/v1/*`; JSON-RPC is at `/rpc`. This reduces port usage and simplifies deployment.

### Structured JSON everywhere

Every response is JSON. Success:

```json
{ "ok": true, "data": { ... } }
```

Failure:

```json
{ "ok": false, "error": { "code": "...", "message": "...", "retry": false } }
```

HTTP status codes are secondary: 200 for success, 400 for malformed requests, 401 for auth failures, 422 for semantic failures (compile error, test failure, revert). The envelope is the primary contract.

### Bearer token auth

When `auth_token` is configured, every request except `GET /healthz` and `GET /` requires `Authorization: Bearer <token>`. The comparison uses `subtle.ConstantTimeCompare` to prevent timing attacks.

When auth is disabled, the server logs a warning: "no auth_token set — all callers on this address can compile/deploy/sign; bind to loopback or set server.auth_token".

### Route table

| Method | Path | Handler |
|---|---|---|
| GET | `/healthz` | Health check |
| GET | `/` | Service info |
| POST | `/rpc` | JSON-RPC 2.0 |
| POST | `/v1/compile` | Compile |
| POST | `/v1/test` | Test |
| POST | `/v1/simulate` | Simulate |
| POST | `/v1/deploy` | Deploy |
| POST | `/v1/call` | Call |
| GET | `/v1/chains` | List chains |
| POST | `/v1/chains` | Register chain |
| POST | `/v1/chains/use` | Set active chain |
| GET | `/v1/artifacts/{name}` | Get artifact |
| GET | `/v1/registry/deployments` | Lookup deployment |

### Request decoding

All POST handlers use a shared `decode` helper:

```go
func decode(w http.ResponseWriter, r *http.Request, v any) bool
```

- Closes body via defer
- Returns false on JSON decode error, writing a 400 envelope
- Ignores EOF (empty body is valid for some endpoints)

### Forge version probing

The server probes `forge --version` at startup and caches the result. This is included in the health check response so agents know the forge version available.

---

## Server lifecycle

```go
srv := api.New(eng, logger)
err := srv.ListenAndServe(cfg.APIAddr)  // blocks
// ... later ...
err := srv.Shutdown(ctx)  // graceful with 10s timeout
```

---

## Modifying the API server

| What to change | Where |
|---|---|
| Add endpoint | `pkg/api/server.go` — register handler + method |
| Change auth | `pkg/api/server.go` — `authMiddleware` |
| Add middleware | `pkg/api/server.go` — wrap `mux` in `ListenAndServe` |
| Change response format | `pkg/types/types.go` — `Envelope[T]` |
