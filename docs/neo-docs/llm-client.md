# LLM Client

Package `matrix/neo/internal/llm` is Neo's OpenAI-compatible function-calling transport. It reuses `matrix/mcl/llm`'s Config, provider detection, and model registry for gateway metering and provider routing, but owns its own tool-calling message shape.

Source files: `neo/internal/llm/client.go`, `neo/internal/llm/message.go`.

---

## Design decisions

**Chat-completions shape only.** v1 supports the OpenAI chat-completions API with native `tools`/`tool_calls`/`tool` role. Anthropic Messages and OpenAI Responses shapes are rejected with a clear error — their tool schemas differ and would break the loop.

**Gateway metering path.** When `Config.GatewayURL` is set, calls are rewritten to `${GatewayURL}/v1/chat/completions` with `MATRIX_GATEWAY_TOKEN` bearer and `X-Matrix-*` metadata headers. Spend is attributed to the actor under slot "neo".

**Reasoning channel extraction.** Some providers inline chain-of-thought inside `content` as `…` or `<thinking>…</thinking>`. `splitInlineThink` moves this out of the visible channel and into the `Reasoning` field, so internal monologue never leaks into the chat.

**Dry-run support.** Any code that constructs an `interpreter.Interpreter` with `llm=nil` runs in dry-run mode — prompts are built and interpolated but no LLM call is made.

---

## Client

```go
type Client struct {
    model       string
    provider    mcllm.Provider
    endpoint    string
    apiKey      string
    gatewayURL  string
    actorDID    string
    intentID    string
    slotLabel   string
    temperature float64
    maxTokens   int
    seed        int64
}
```

```go
client, err := llm.New(mcllm.Config{
    Model:       "accounts/fireworks/models/kimi-k2p7-code",
    Temperature: 0.4,
    MaxTokens:   4096,
    GatewayURL:  cfg.GatewayURL,
    ActorDID:    cfg.ActorDID,
    SlotLabel:   "neo",
})
```

Provider is auto-detected from the model string. Endpoint defaults to the provider's canonical URL. API key is read from environment (`FIREWORKS_API_KEY`, `TOGETHER_API_KEY`, `OPENCODE_API_KEY`) unless overridden in `Config.APIKey`.

---

## Chat

```go
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResult, error)
```

Sends the message list + optional tool schemas, returns the model's single assistant turn.

```go
type ChatRequest struct {
    Messages   []Message
    Tools      []Tool
    ToolChoice string // "auto" (default), "none", "required"
}
```

```go
type ChatResult struct {
    Message      Message
    FinishReason string
    Usage        Usage
}
```

`FinishReason` values: `"stop"`, `"length"` (truncated), `"tool_calls"`. Truncated generation is handled by the agent loop — never emitted raw.

---

## Message types

```go
type Message struct {
    Role       string     // "system" | "user" | "assistant" | "tool"
    Content    string     // text content
    ToolCalls  []ToolCall // assistant turn: requested calls
    ToolCallID string     // tool turn: which call this answers
    Name       string     // tool turn: function name
    Reasoning  string     // chain-of-thought (not serialized)
}
```

### Constructors

```go
llm.SystemMessage("be helpful")
llm.UserMessage("what is the PAX price")
llm.AssistantMessage("PAX is trading around $X") // seeding from history
llm.ToolResult("call-1", "paxeer__price", `{"pax": "0.42"}`)
```

### ToolCall

```go
type ToolCall struct {
    ID       string
    Type     string // always "function"
    Function FunctionCall
}

type FunctionCall struct {
    Name      string
    Arguments string // JSON-encoded args
}
```

`ParseArgs()` decodes the JSON string into a `map[string]interface{}`.

---

## Tool schema

```go
type Tool struct {
    Type     string      // always "function"
    Function FunctionDef
}

type FunctionDef struct {
    Name        string
    Description string
    Parameters  map[string]interface{} // JSON Schema object
}
```

```go
tool := llm.NewFunctionTool("fs__read_file", "Read a file", map[string]interface{}{
    "type": "object",
    "properties": map[string]interface{}{
        "path": map[string]interface{}{"type": "string"},
    },
    "required": []string{"path"},
})
```

---

## Reasoning channel

`fromWireRespMessage` handles three provider postures:

1. **Separate `reasoning_content` field** — copied directly to `Message.Reasoning`
2. **Inline `…` or `<thinking>…</thinking>` in `content`** — extracted by `splitInlineThink`
3. **Unterminated opening tag** — the whole remainder is reasoning (truncated generation safety)

The `Reasoning` field is never serialized onto the wire and never treated as the answer. It is surfaced as a distinct channel only.

---

## Gateway headers

When `gatewayURL` is set, the request carries:

```
Authorization: Bearer ${MATRIX_GATEWAY_TOKEN}
X-Matrix-Actor-DID: <actor DID>
X-Matrix-Intent-ID: <intent ID>
X-Matrix-Slot: neo
```

This matches the daemon/router environment key `MATRIX_GATEWAY_URL` and the MCL compiler's gateway posture.

---

## Error handling

HTTP errors are parsed for structured error bodies:

```
neo/llm: fireworks http 429: Rate limit exceeded (type=rate_limit)
```

Empty choices, parse failures, and API errors all return wrapped errors with the provider name for attribution.

---

## Modifying the client

| What to change | Where |
|---|---|
| Supported API shapes | `llm/client.go` — `New()` shape guard |
| Gateway header set | `llm/client.go` — `newHTTPRequest()` |
| Inline reasoning tags | `llm/client.go` — `splitInlineThink()` |
| Message constructors | `llm/message.go` — `SystemMessage`, `UserMessage`, etc. |
| Tool schema defaults | `llm/message.go` — `NewFunctionTool()` |
