# Config System

Package `matrix/neo/internal/config` holds Neo's runtime configuration. The locked operational contract — context-budget thresholds, loop discipline, and the execution surface — comes from the frozen design spec at `neo/neo.frozen.kvx` and is encoded as `Default()` values. Deployment wiring is overlaid from an optional runtime `.kvx` file and then from environment variables.

Source files: `neo/internal/config/config.go`, `neo/internal/config/kvx.go`.

---

## Design decisions

**Precedence: Default < runtime .kvx < environment.** A fresh checkout runs with zero config. Deployment-specific values (models, cortex location, daemon URL) are overlaid without touching source.

**Frozen spec defaults.** The `Default()` function encodes the locked operational contract from `neo.frozen.kvx`. Changing these values changes the spec — they are not merely "sensible defaults."

**Missing .kvx is non-fatal.** `Load("/nonexistent/neo.kvx")` returns defaults with no error. This lets dev/CLI runs work unchanged.

---

## Config struct

```go
type Config struct {
    // Identity / runtime wiring
    AgentName    string // "Neo"
    CortexRoot   string // "/root/.cortex"
    CortexActor  string // "neo"
    DaemonURL    string // "http://127.0.0.1:8080"
    ManifestPath string // "agents/default.json"
    SkillsRoot   string // "skills"

    // Models (provider-qualified ids)
    MainModel  string // "accounts/fireworks/models/kimi-k2p7-code"
    CheapModel string // "accounts/fireworks/routers/glm-5p1-fast"
    EmbedModel string // "nomic-ai/nomic-embed-text-v1.5"

    // Memory budget
    ContextWindowTokens   int // 256000
    SoftPct               int // 80
    HardPct               int // 92
    RetrievalTopK         int // 8
    RetrievalBudgetTokens int // 6000
    PinnedBudgetTokens    int // 2000
    RecallTopK            int // 6
    RecallBudgetTokens    int // 2500

    // Loop discipline
    StepBudget        int // 50
    NoProgressStall   int // 4
    MaxRetriesPerTool int // 3
    MaxAdaptAttempts  int // 2

    // Procedural memory guards
    MinPatternSuccesses int // 3

    // Execution surface
    NaturalAllow    []string // reversible actions
    EscalateActions []string // money-moving actions

    // LLM transport
    GatewayURL string // optional metered gateway
    ActorDID   string // actor DID for gateway headers
}
```

---

## Loading

```go
cfg, err := config.Load(path) // path may be "" or missing
```

### Precedence

1. `Default()` — frozen spec values
2. Runtime `.kvx` file — overlays defaults
3. Environment variables — highest precedence

### Environment variables

| Variable | Overrides |
|---|---|
| `NEO_MAIN_MODEL` | `MainModel` |
| `NEO_CHEAP_MODEL` | `CheapModel` |
| `NEO_EMBED_MODEL` | `EmbedModel` |
| `NEO_CORTEX_ROOT` | `CortexRoot` |
| `NEO_CORTEX_ACTOR` | `CortexActor` |
| `NEO_DAEMON_URL` | `DaemonURL` |
| `NEO_MANIFEST` | `ManifestPath` |
| `NEO_SKILLS_ROOT` | `SkillsRoot` |
| `NEO_ACTOR_DID` | `ActorDID` |
| `MATRIX_GATEWAY_URL` | `GatewayURL` (also `NEO_GATEWAY_URL`) |
| `NEO_CONTEXT_WINDOW_TOKENS` | `ContextWindowTokens` |

---

## .kvx format

The Matrix `.kvx` convention (mirrors `tachyon/internal/config/kvx.go`):

```kvx
# comment
[section]
key = "string"            # double-quoted strings
num = 50                  # bare ints
list = ["shell", "git"]   # bracketed, comma-separated, quoted

[section.sub]
ref = "${ENV_VAR}"        # ${ENV} interpolated from process env
```

Features:
- Comments stripped (respecting quoted strings)
- Later duplicate keys win
- `${ENV}` interpolation
- String values are ALWAYS double-quoted (Matrix `.mtx` lexer convention)

---

## Execution surface helpers

```go
func (c Config) IsEscalateAction(action string) bool
```

Checks whether the named action crosses the wall into MCL (requires a user wallet signature).

```go
func (c Config) SoftBudgetTokens() int // ContextWindowTokens * SoftPct / 100
func (c Config) HardBudgetTokens() int // ContextWindowTokens * HardPct / 100
```

---

## Modifying config

| What to change | Where |
|---|---|
| Frozen spec defaults | `config/config.go` — `Default()` |
| New env variable | `config/config.go` — `applyEnv()` |
| New .kvx section/key | `config/config.go` — `applyDoc()` + `kvx.go` accessors |
| Execution surface lists | `config/config.go` — `NaturalAllow`, `EscalateActions` |
