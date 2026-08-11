# Auth System

Package `chronos/internal/auth` implements Chronos's two-layer principal authentication: a shared transport bearer proves "a legitimate Centra AI daemon," and an ed25519 agent-DID challenge/verify handshake proves WHICH owner — so alarms are owner-scoped and the wake target resolves from the DID alone.

Source files: `internal/auth/identity.go`, `internal/auth/token.go`, `internal/auth/auth_test.go`.

---

## Two-layer model

```
┌──────────────────────────────────────┐
│  Layer 1: Transport Auth             │
│  Shared bearer (CHRONOS_TOKEN)       │
│ Proves: "a legitimate Centra AI daemon" │
│  Checked: on every /v1/* request     │
└──────────────┬───────────────────────┘
               │
┌──────────────▼───────────────────────┐
│  Layer 2: Principal Auth             │
│  ed25519 agent-DID challenge/verify  │
│  Proves: WHICH agent/owner           │
│  Checked: on /v1/alarms* requests    │
│  Result: HMAC token in X-Chronos-Agent│
└──────────────────────────────────────┘
```

This mirrors UWAC's credential flow and the live wallet lane. The daemon's executor key IS the agent identity, and the DID label IS the owner's Supabase user UUID.

---

## Transport auth

A shared bearer token injected by the router into every daemon's environment as `MATRIX_CHRONOS_TOKEN`. The MCP proxy presents it on every request.

Implemented as HTTP middleware in `server.transportMiddleware`:

```go
func (s *Server) transportMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if s.transportToken == "" || isPublicPath(r) {
            next.ServeHTTP(w, r)
            return
        }
        if subtle.ConstantTimeCompare(
            []byte(bearerToken(r)),
            []byte(s.transportToken),
        ) != 1 {
            writeFail(w, 401, "unauthorized", "missing or invalid transport bearer")
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

Public paths (`GET /` and `GET /healthz`) skip transport auth. When `transportToken` is empty, all paths are open (loopback/dev only — logged as a warning).

---

## Agent DID identity

### DID format

```
did:matrix:<label>:<16-hex-key-fingerprint>
```

Example: `did:matrix:11111111-2222-3333-4444-555555555555:0123456789abcdef`

- **label** — the owner's Supabase user UUID (the AGENT_BIND_OWNER_FROM_DID convention)
- **keyfp** — first 16 hex chars of the ed25519 public key

### Parsing

```go
func ParseDID(s string) (DID, error)
```

Validates against the regex `^did:matrix:([^:]+):([0-9a-fA-F]{16})$`. Returns a `DID` struct with `Raw`, `Label`, and `KeyFP` fields.

### Owner extraction

```go
func OwnerFromDID(d DID) string
```

When the label is a UUID, returns the lowercase UUID. Otherwise falls back to the raw label (so non-UUID labels like dev `"executor"` still route deterministically).

---

## Challenge/verify handshake

The agent proves possession of its ed25519 private key without revealing it.

### Step 1: Request a challenge

```
POST /v1/agent/auth/challenge
{"did": "did:matrix:<user_id>:<keyfp>"}

→ 200
{
  "ok": true,
  "data": {
    "did": "did:matrix:…",
    "nonce": "base64url-24-random-bytes",
    "message": "matrix-chronos-auth:did:matrix:…:nonce",
    "expires_in": 120
  }
}
```

The challenge message format is:

```go
func ChallengeMessage(did, nonce string) string {
    return "matrix-chronos-auth:" + did + ":" + nonce
}
```

This MUST stay in lockstep with `tools/chronos/chronos.mjs`.

### Step 2: Sign and verify

The agent signs `ChallengeMessage(did, nonce)` with its ed25519 private key and POSTs:

```
POST /v1/agent/auth/verify
{
  "did": "did:matrix:…",
  "public_key": "hex(ed25519 pubkey)",
  "nonce": "base64url-nonce",
  "signature": "hex(ed25519 signature)"
}

→ 200
{
  "ok": true,
  "data": {
    "token": "base64url-payload.base64url-mac",
    "owner_user_id": "11111111-2222-3333-4444-555555555555",
    "expires_in": 86400
  }
}
```

`VerifySignature` checks three things:

1. The public key is a valid 32-byte ed25519 key
2. The public key's first 16 hex chars match the DID's fingerprint (so a caller cannot present an unrelated key for a known DID)
3. The ed25519 signature verifies against `ChallengeMessage(did, nonce)`

```go
func VerifySignature(didStr, pubHex, nonce, sigHex string) error
```

### Nonce store

`Challenges` is an in-memory single-use nonce store with TTL:

```go
type Challenges struct {
    mu  sync.Mutex
    ttl time.Duration
    m   map[string]entry  // nonce → {did, expiry}
}
```

- `Create(did)` → generates a 24-byte random nonce, stores it bound to the DID, returns `(nonce, message)`
- `Consume(nonce, did)` → atomically validates + deletes. Returns false for unknown, expired, already-used, or DID-mismatched nonces
- `Purge()` → drops expired entries (called every 5 minutes from a background goroutine)

---

## Principal tokens

After a successful verify, chronosd mints a short-lived, stateless HMAC token. The token is presented on subsequent `/v1/alarms*` requests via the `X-Chronos-Agent` header.

### Token format

```
base64url(payload) . base64url(mac)
```

Where `payload = "<did>|<owner>|<expUnix>"`.

### Minting

```go
func (t *Tokens) Mint(did, owner string) (token string, expiresIn int)
```

Derives an HMAC-SHA256 key from `CHRONOS_AGENT_AUTH_SECRET`, signs the payload, and returns the token + TTL in seconds.

### Verification

```go
func (t *Tokens) Verify(tok string) (Claims, error)
```

Checks:
1. Token format (`payload.mac`)
2. Base64 decoding of both parts
3. HMAC signature match (constant-time)
4. Payload format (`did|owner|expUnix`)
5. Expiry not passed
6. Non-empty DID and owner

Returns `Claims{DID, Owner}` on success.

### Stateless design

No session store. Tokens work across chronosd instances. The DID is carried (not just the owner) so alarm ownership scopes on the full `owner_did` while the wake target resolves from the owner user id.

---

## Principal resolution in handlers

Alarm handlers call `s.principal(w, r)` which extracts and verifies the `X-Chronos-Agent` token:

```go
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
    tok := strings.TrimSpace(r.Header.Get("X-Chronos-Agent"))
    if tok == "" {
        writeFail(w, 401, "unauthorized", "missing X-Chronos-Agent principal token")
        return auth.Claims{}, false
    }
    claims, err := s.tokens.Verify(tok)
    if err != nil {
        writeFail(w, 401, "unauthorized", "invalid principal token: " + err.Error())
        return auth.Claims{}, false
    }
    return claims, true
}
```

The returned `claims.DID` is used as `owner_did` for all alarm queries. The `claims.Owner` is used as `user_id` for the wake target. Neither is ever taken from the request body.

---

## Key material

The agent's ed25519 key lives on disk at `${MATRIX_DATA_DIR}/.matrix/executor.key` (64 hex chars, the seed). The DID is `did:matrix:<label>:<hex(pubkey)[:16]>`. This is the same identity used by the executor, tachyon, and UWAC — one key, one DID, consistent across all subsystems.

---

## Config

| Env variable | Purpose | Required in prod |
|---|---|---|
| `CHRONOS_TOKEN` | Transport bearer the MCP proxy presents | Yes |
| `CHRONOS_AGENT_AUTH_SECRET` | HMAC secret for nonces + principal tokens | Yes |
| `CHRONOS_DEV` | Relaxes required-secret checks | No (dev only) |

When `CHRONOS_AGENT_AUTH_SECRET` is empty and not in dev mode, a hardcoded dev secret is used with a warning. This is a boot-time safety net, not a production path.
