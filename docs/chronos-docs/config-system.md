# Config System

Package `chronos/internal/config` loads chronosd configuration from the environment with an optional `chronos.config.kvx` overlay. Environment variables always win. Mirrors tachyon/uwac's config layering so operators get one consistent knob story.

Source files: `internal/config/config.go`, `internal/config/kvx.go`.

---

## Resolution order

```
1. chronos.config.kvx  (optional file, lowest precedence)
2. Environment variables (override kvx)
3. Hardcoded defaults   (fallback when neither is set)
```

`CHRONOS_DEV=1` relaxes production fail-closed checks on required secrets.

---

## Config struct

```go
type Config struct {
    Port            int           // default 9096
    PostgresURI     string        // required
    MigrationsDir   string        // default "migrations"
    TransportToken  string        // shared bearer for MCP proxy
    AgentAuthSecret string        // HMAC secret for nonces + tokens
    ChallengeTTL    time.Duration // default 120s
    TokenTTL        time.Duration // default 24h
    RouterWakeURL   string        // default http://127.0.0.1:8088/internal/wake
    WakeToken       string        // shared secret for /internal/wake
    Tick            time.Duration // dispatch poll interval, default 1s
    MaxFailures     int           // default 5
    ClaimLease      time.Duration // default 2m
    ClaimBatch      int           // default 100
    Dev             bool          // relaxes required-secret checks
}
```

---

## Environment variables

| Variable | Config field | Default |
|---|---|---|
| `CHRONOS_PORT` | `Port` | `9096` |
| `CHRONOS_POSTGRES_URI` | `PostgresURI` | (required) |
| `CHRONOS_MIGRATIONS_DIR` | `MigrationsDir` | `"migrations"` |
| `CHRONOS_TOKEN` | `TransportToken` | `""` |
| `CHRONOS_AGENT_AUTH_SECRET` | `AgentAuthSecret` | `""` |
| `CHRONOS_ROUTER_WAKE_URL` | `RouterWakeURL` | `http://127.0.0.1:8088/internal/wake` |
| `CHRONOS_WAKE_TOKEN` | `WakeToken` | `""` |
| `CHRONOS_TICK_MS` | `Tick` | `1000` (1s) |
| `CHRONOS_MAX_FAILURES` | `MaxFailures` | `5` |
| `CHRONOS_DEV` | `Dev` | `false` |

`ChallengeTTL`, `TokenTTL`, `ClaimLease`, and `ClaimBatch` are currently only settable via the kvx overlay, not environment variables.

---

## KVX overlay

The optional `chronos.config.kvx` file uses the same sectioned key/value format as tachyon and UWAC:

```kvx
# chronos.config.kvx
[server]
port = 9096
dev = "0"

[auth]
transport_token = "${CHRONOS_TOKEN}"
agent_secret = "${CHRONOS_AGENT_AUTH_SECRET}"
challenge_ttl_seconds = 120
token_ttl_seconds = 86400

[store]
postgres_uri = "${CHRONOS_POSTGRES_URI}"
migrations_dir = "migrations"

[wake]
router_url = "http://127.0.0.1:8088/internal/wake"
token = "${CHRONOS_WAKE_TOKEN}"

[dispatch]
tick_ms = 1000
max_failures = 5
claim_lease_seconds = 120
claim_batch = 100
```

### KVX format rules

- `#` starts a comment (not inside double-quoted strings)
- `[section]` / `[section.sub]` headers
- `key = "string"` — double-quoted strings (Matrix `.mtx` convention)
- `key = 9096` — bare integers
- `${ENV_VAR}` — interpolated from the process environment
- Missing file is not an error (returns an empty doc)
- Later duplicate keys in the same section win

### KVX parser

The parser (`kvx.go`) is zero-dependency — a simple `bufio.Scanner`-based line parser. It is shared with tachyon and UWAC's config loaders. The `kvxDoc` struct holds parsed sections and provides typed accessors:

```go
doc.str("section", "key")        // returns interpolated string
doc.uint64Or("section", "key", 0) // returns parsed uint64 or fallback
```

---

## Production vs dev

In production (default):

| Check | Behavior |
|---|---|
| `PostgresURI` empty | Fatal: `"CHRONOS_POSTGRES_URI is required"` |
| `TransportToken` empty | Fatal: `"CHRONOS_TOKEN is required in production"` |
| `AgentAuthSecret` empty | Fatal: `"CHRONOS_AGENT_AUTH_SECRET is required in production"` |
| `WakeToken` empty | Fatal: `"CHRONOS_WAKE_TOKEN is required in production"` |

In dev (`CHRONOS_DEV=1`):

- Missing secrets are allowed
- Empty `AgentAuthSecret` falls back to a hardcoded dev secret (with a warning)
- Empty `TransportToken` disables transport auth (all paths open)
- Empty `WakeToken` logs a warning

---

## Router-side config

The router injects these into every daemon's environment:

| Router env key | Purpose |
|---|---|
| `MATRIX_CHRONOS_URL` | The URL the chronos.mjs proxy dials (default the public nginx `/chronos/` route) |
| `MATRIX_CHRONOS_TOKEN` | The transport bearer the proxy presents (= `CHRONOS_TOKEN` on the chronosd side) |

The router also needs a new config key `ROUTER_WAKE_TOKEN` — the shared secret gating `POST /internal/wake`. This must match `CHRONOS_WAKE_TOKEN`.

---

## Load function

```go
func Load() (*Config, error)
```

1. Reads `CHRONOS_CONFIG` env var for the kvx path (default `"chronos.config.kvx"`)
2. Parses the kvx file (missing file → empty doc, not an error)
3. Applies kvx values as defaults
4. Overrides with environment variables via `pick()` / `pickUint()`
5. Validates required fields (fail-closed in production)
6. Applies hardcoded defaults for any remaining zero values
7. Returns the resolved `*Config`
