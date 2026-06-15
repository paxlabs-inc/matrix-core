# Config

**Source files:** `internal/config/config.go`, `internal/config/kvx.go`

The config system loads daemon settings from a `.kvx` file with environment variable precedence. It supports two wallet modes, capability policy profiles, and custom chain registrations.

---

## Design decisions

### `.kvx` format, not TOML/YAML/JSON

The config uses a custom sectioned key/value format (`.kvx`) that is zero-dependency, deterministic, and matches the Matrix `.mtx` convention:

```
[section]
key = "string"            # double-quoted strings
num = 31337               # bare ints/bools
list = ["a", "b"]         # bracketed, comma-separated, quoted items
[section.sub]
ref = "${ENV_VAR}"        # env interpolation
```

This avoids pulling in TOML/YAML parsers and keeps the config file human-readable and comment-friendly.

### Precedence: environment > kvx > defaults

Every config value is resolved via `pick(envKey, kvxVal, fallback)`:

1. Environment variable (if set and non-empty)
2. `.kvx` file value (if present)
3. Hardcoded default

This lets operators override any setting via environment without editing the file.

### `.env` file loading

A `.env` file in the working directory is loaded when present (KEY=VALUE lines only, no export prefix needed). This is loaded before `.kvx` parsing, so `.kvx` `${ENV_VAR}` references resolve values from `.env`.

### Wallet mode auto-detection

If `wallet.mode` is not explicitly set, the config loader auto-detects from environment:

- `TACHYON_ALLOW_DEV_SIGNER=true` or `TACHYON_DEV_PRIVATE_KEY` set → `self_hosted`
- `PAXEER_WALLET_TOKEN` or `MATRIX_EXECUTOR_KEY` set → `embedded`

This backwards-compatibility shim supports the original dev/Paxeer signer environment variables.

### Foundry root discovery

If `project_root` is `.` (default), the config loader walks up to 12 parent directories looking for `foundry.toml`. This means the daemon can be started from any subdirectory of a Foundry project.

---

## Config structure

```go
type Config struct {
    APIAddr      string
    ProjectRoot  string
    ArtifactsDir string
    RegistryPath string
    SolcPath     string
    ForgePath    string
    AuthToken    string
    Wallet       WalletConfig
    Policies     map[string]PolicyProfile
    Chains       []ChainConfig
}

type WalletConfig struct {
    Mode             string // "" | self_hosted | embedded
    Signer           string // raw | keystore | env
    PrivateKey       string
    KeystorePath     string
    KeystorePassword string
    Keyfile          string // ed25519 seed path
    Label            string // DID label
    API              string // embedded wallet base URL
}

type PolicyProfile struct {
    Name        string
    SpendCapWei string
    Allow       []string
    Chains      []string
}

type ChainConfig struct {
    ID       string
    Name     string
    RPCURL   string
    ChainID  uint64
    Explorer string
}
```

---

## KVX parser

```go
type kvxDoc struct {
    sections map[string]map[string]string
    order    []string
}
```

### Typed accessors

```go
func (d *kvxDoc) str(section, key string) string      // interpolated string
func (d *kvxDoc) list(section, key string) []string   // bracketed list
func (d *kvxDoc) uint64Or(section, key string, fallback uint64) uint64
func (d *kvxDoc) sectionsWithPrefix(prefix string) []string
```

### Interpolation

`${ENV_VAR}` is replaced with `os.Getenv("ENV_VAR")` at parse time. This happens in `interpolate()`, called by `str()` and `list()`.

---

## Example config

```
[server]
addr       = "127.0.0.1:8645"
auth_token = "${TACHYON_AUTH_TOKEN}"

[wallet]
mode = "self_hosted"

[wallet.self_hosted]
signer      = "env"
private_key = "${DEPLOYER_PK}"

[wallet.embedded]
keyfile = "/data/.matrix/executor.key"
label   = "executor"
api     = "https://connect.paxportwallet.com"

[policy.default]
spend_cap_wei = "100000000000000000"
allow         = []
chains        = ["paxeer-mainnet"]

[chains.anvil]
name     = "Local Anvil"
rpc_url  = "http://127.0.0.1:8545"
chain_id = 31337
```

---

## Modifying the config

| What to change | Where |
|---|---|
| Add config field | `internal/config/config.go` — `Config` struct + `Load` |
| Add kvx accessor | `internal/config/kvx.go` — new typed method |
| Change precedence | `internal/config/config.go` — `pick` function |
| Add env shim | `internal/config/config.go` — `loadWallet` |
