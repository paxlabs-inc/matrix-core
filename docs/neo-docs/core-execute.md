# core_execute Delegation

Package `matrix/neo/internal/delegate` is the bridge between Neo's conversational loop and the MCL pipeline. It hands a prose intent to the daemon's async HTTP API, services in-walk approval gates inline, and returns the verifiable outcome.

Source file: `neo/internal/delegate/client.go`.

---

## Design decisions

**Poll-based, not SSE.** The daemon's async API is polled for status and gates. This keeps the delegate simple and avoids a second SSE dependency — the conversation's existing SSE stream carries gate events to the user.

**Inline approval.** When the daemon fires a gate (e.g. "Approve spend of 5 PAX?"), the delegate blocks until the user answers via the conversation's gate-answer endpoint. The user never leaves the chat context.

**Safe default.** A nil `Approver` denies every gate — no unattended spends.

**Bounded wait.** `MaxWait` (default 30 minutes) prevents a stuck intent from blocking the conversation forever.

---

## Client

```go
type Client struct {
    base      string
    http      *http.Client
    token     string
    did       string
    wallet    string
    skill     string
    approve   Approver
    notify    func(string)
    pollEvery time.Duration
    maxWait   time.Duration
}
```

```go
delegate.New(delegate.Options{
    BaseURL:      "http://127.0.0.1:8080",
    Token:        os.Getenv("NEO_DAEMON_TOKEN"),
    CallerDID:    cfg.ActorDID,
    CallerWallet: os.Getenv("NEO_CALLER_WALLET"),
    Approver:     newApprover(in, rep),
    Notify:       rep.Notice,
})
```

---

## Run

```go
func (c *Client) Run(ctx context.Context, prose string) (string, error)
```

The full lifecycle:

1. **Submit** — `POST /messages/async` with the prose intent → returns `intent_id`
2. **Poll loop** — every `pollEvery` (default 1.5s):
   - Check for pending gates → ask approver → `POST /intents/{id}/gates/{nid}/answer`
   - Check status → terminal states: `completed`, `failed`, `cancelled`
   - Clarify questions → return as error ("needs more detail")
3. **Return** — the deliverable answer, or an error describing the failure

### Status handling

| Status | Outcome |
|---|---|
| `completed` | Return `Result.Answer` (or "Done" if empty) |
| `failed` | Return error with pipeline error message |
| `cancelled` | Return error "the delegated task was cancelled" |
| `clarify` present | Return error with the clarification question |
| timeout | Return error after `maxWait` |

---

## Approver

```go
type Approver func(ctx context.Context, nodeID, question string, options []string) (approved bool, answer string)
```

Called for every pending gate. The `nodeID` lets the UI route the user's answer back to the exact gate. Returns:
- `approved=true` — the daemon proceeds with the spend/action
- `approved=false` — the daemon treats the gate as denied

### CLI approver

In the interactive CLI, the approver reads from stdin:

```
approval needed — Approve spend of 5 PAX?
    options: yes | no
    approve? [y/N] y
```

Safe because `Chat` runs synchronously while the REPL is blocked — no concurrent stdin reads.

### Server approver

In the HTTP service, the approver publishes a `gate.invoked` SSE event and blocks on a channel until the user answers via `POST /intents/:id/gates/:nid/answer`.

---

## HTTP contract

### Submit

```
POST /messages/async
{"prose": "send 5 PAX to 0x..."}
→ {"intent_id": "i1"}
```

### Gates

```
GET /intents/{id}/gates
→ {"pending": [{"node_id": "n1", "question": "Approve spend?", "options": ["yes", "no"]}]}

POST /intents/{id}/gates/{nid}/answer
{"approved": true, "answer": ""}
```

### Status

```
GET /messages/async/{id}
→ {"status": "completed", "result": {"answer": "settled"}}
```

---

## Modifying the delegate

| What to change | Where |
|---|---|
| Poll interval | `delegate/client.go` — `Options.PollInterval` |
| Max wait | `delegate/client.go` — `Options.MaxWait` |
| HTTP timeout | `delegate/client.go` — `Options.Timeout` |
| Gate answer shape | `delegate/client.go` — `answerGate()` |
| Status response parsing | `delegate/client.go` — `status()` |
