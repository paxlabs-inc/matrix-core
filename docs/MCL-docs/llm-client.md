# LLM Client

Package `matrix/mcl/llm` implements the `interpreter.LLM` interface over OpenAI-compatible chat-completion endpoints. It's stdlib-only — no third-party SDK.

Source files: `MCL/llm/llm.go`, `MCL/llm/model.go`, `MCL/llm/identity.go`, `MCL/llm/messages_api.go`, `MCL/llm/responses_api.go`.

---

## Providers

Three providers are supported:

| Provider | Constant | Default endpoint |
|---|---|---|
| Together AI | `ProviderTogether` | `https://api.together.xyz/v1/chat/completions` |
| Fireworks AI | `ProviderFireworks` | `https://api.fireworks.ai/inference/v1/chat/completions` |
| Opencode | `ProviderOpencode` | `https://opencode.ai/zen/v1/...` (route depends on model) |

Provider is auto-detected from the model string via `llm.DetectProvider()`. You can override it explicitly via `Config.Provider`.

Environment variables used for API keys:
- Together: `TOGETHER_API_KEY`
- Fireworks: `FIREWORKS_API_KEY`
- Opencode: `OPENCODE_API_KEY`

`Config.APIKey` overrides the env var lookup.

---

## API shapes

Three wire shapes are supported:

| Shape | Constant | Endpoint suffix |
|---|---|---|
| OpenAI chat completions | `ShapeChatCompletions` | `/v1/chat/completions` |
| Anthropic messages | `ShapeMessages` | `/v1/messages` |
| OpenAI responses | `ShapeResponses` | `/v1/responses` |

The shape is auto-detected from the endpoint URL via `llm.DetectAPIShape(endpoint)`. The `messages_api.go` and `responses_api.go` files implement the Anthropic and OpenAI Responses API shapes respectively; `llm.go` handles the default chat completions path.

For the compiler slot, you're always on `ShapeChatCompletions` — Together and Fireworks both speak this. The other shapes are used by Neo's LLM routing for frontier models routed through opencode.

---

## Config

```go
type Config struct {
    Model       string   // provider-specific model identifier
    Provider    Provider // override auto-detection
    APIKey      string   // override env var
    Endpoint    string   // override default endpoint
    Temperature float64  // 0 = deterministic (compiler default)
    Seed        int64    // D11 seed (0 = no seed param sent)
    MaxTokens   int      // 0 = defaults to 4096
    Timeout     time.Duration // 0 = defaults to 90s
    GrammarMode GrammarMode
    Grammars    map[string]*GrammarDef
}
```

### Grammar modes

```go
const (
    GrammarModeNone         GrammarMode = iota // no constraint
    GrammarModeResponseJSON                    // response_format: {type: "json_object"}
    GrammarModeResponseSchema                  // response_format: {type: "json_schema", json_schema: {...}}
    GrammarModeFireworksGBNF                   // Fireworks grammar= EBNF parameter
)
```

Grammar constraints are how the compiler forces the LLM to produce structurally valid output. The provider determines which mode is available:

- Together AI supports `response_format.json_schema` (mode: `GrammarModeResponseSchema`)
- Fireworks supports both JSON schema and native EBNF (`grammar=` param, mode: `GrammarModeFireworksGBNF`)

The grammar ID (e.g. `"intent_frame@1"`) is passed from `RunInput.Grammar` through to `Decode`. The `Grammars` map in `Config` resolves it to the actual constraint payload.

```go
type GrammarDef struct {
    ID         string // e.g. "intent_frame@1"
    JSONSchema []byte // JSON Schema bytes (for response_format.json_schema)
    GBNF       string // EBNF grammar string (for Fireworks grammar= param)
}
```

---

## Creating a client

```go
cfg := llm.DefaultCompilerModel()
// cfg.Model is a fast, seedable, grammar-constrained model
// cfg.Temperature is 0 (deterministic)
// cfg.Seed is 42 (default)

client, err := llm.New(&cfg)
if err != nil {
    // API key not found — fall back to dry-run
}

// Implements interpreter.LLM
var _ interpreter.LLM = client
```

`llm.New` returns an error if the API key is missing. The caller decides whether to fall back to dry-run or propagate the error. The `mclc compile` command logs a warning and falls back.

`llm.DefaultCompilerModel()` returns a config for the project's default fast-seedable compiler model. Check `MCL/llm/model.go` for the current default. This is intentionally not hardcoded here because it changes as better options appear.

---

## Calling the LLM

```go
messages := []interpreter.Message{
    {Role: "system", Content: "You are a frame extractor..."},
    {Role: "user", Content: "Goal: build a deployment pipeline"},
}

output, err := client.Decode(ctx, messages, "intent_frame@1")
```

The grammar argument is resolved against `cfg.Grammars`. If it's not in the map, the call proceeds without a grammar constraint (same as passing `""`).

For streaming:

```go
if streamer, ok := client.(interpreter.StreamingLLM); ok {
    output, err := streamer.Stream(ctx, messages, "intent_frame@1", func(delta string) {
        // incremental token — send to UI
    })
} else {
    output, err = client.Decode(ctx, messages, "intent_frame@1")
}
```

`Stream` must return the same final text as `Decode` would have. The canonical output — and therefore the D11 hash — is always the full accumulated text. Streaming is a UX layer, not a semantic one.

---

## Model registry

`MCL/llm/model.go` contains the `ModelRegistry` — a mapping from model identifiers to routing metadata (which provider, which API shape, whether seedable, whether grammar-constrained). This is what the executor uses to pick the right model for a given `StepPayload.Kind`.

The registry is the source of truth for which models are available in which roles. Adding a new model means adding an entry here and (for prod metering) a rate-card entry in `gateway/internal/rates/rates.go`.

The step kind → model routing:

| Step kind | Model tier | Notes |
|---|---|---|
| `reason` | Default (GLM-5.1 fast) | General agentic reasoning |
| `code` | Code specialist | Code generation / analysis |
| `summarize` | Long-context specialist | Summarization over long context windows |
| `write` | Prose specialist | Free-form writing |
| `transform` | Deterministic structured I/O | JSON→JSON transformations |
| `classify` | Fast grammar-constrained | Pick-from-list, classifier steps |
| `hard_reason` | Frontier reasoning (expensive) | Multi-step reasoning, planning |

The model registry keeps `AllStepKindNames` in sync with `ir.StepKindNames`. There's a test in the executor that guards against drift between the two — if you add a kind to one, you must add it to the other.

---

## Identity

`MCL/llm/identity.go` provides `llm.Identity` — a per-invocation identity that flows through LLM calls for attribution and metering. The Matrix gateway routes LLM calls through the `X-Matrix-Actor-DID` header, and the `Identity` struct provides the values that populate it.

---

## Dry-run mode

Any code that constructs an `interpreter.Interpreter` with `llm=nil` runs in dry-run mode. The interpreter builds and interpolates prompts exactly as it would for a real call, but returns without calling the LLM. `RunResult.FrameJSON` is empty; `RunResult.PromptMessages` is fully populated.

This is useful for:
- Testing `.mtx` files without an API key
- Displaying what the compiler would have sent to the LLM
- CI validation of skill files

`mclc compile -dry-run` uses this mode explicitly.
