# Wake Delivery

Package `chronos/internal/wake` delivers a due alarm to its agent by asking the router to wake the machine and inject a chat turn. Chronos NEVER talks to Fly or the daemon directly — it reuses the router's battle-tested `EnsureStarted` + `waitDaemonReady` + 6PN reverse-proxy path.

Source file: `internal/wake/wake.go`.

---

## Design decisions

**Chronos owns timing, the router owns waking.** The hard half (waking a suspended Fly machine, waiting for the daemon to be ready, routing over 6PN) already exists in the router. Chronos adds only the easy half (durable timing + context storage) and hands the fire off.

**One new router surface.** The only new piece is `POST /internal/wake` on the router's internal listener. Everything downstream of that is reused.

**No daemon code change for v1.** The router delivers the wake by POSTing to the daemon's existing `/chat` endpoint with `{message, conversation_id}`. A dedicated `/wake` endpoint is a possible later refinement.

---

## The 6-step wake path

```
Step 1 ─ dispatch worker claims a due alarm
         (status=active, next_fire_at <= now)
         FOR UPDATE SKIP LOCKED
              │
Step 2 ─ chronosd POSTs http://127.0.0.1:8088/internal/wake
         {user_id, conversation_id, message, payload, alarm_id}
         Authorization: Bearer <CHRONOS_WAKE_TOKEN>
              │
Step 3 ─ router looks up user_id
         → fly.EnsureStarted(machine)
         → waitDaemonReady(/healthz)
              │
Step 4 ─ router POSTs the woken daemon :8080/chat
         {message, conversation_id}
         over Fly 6PN with X-Matrix-User=<user_id>
              │
Step 5 ─ Neo resumes conversation_id
         (seeds from cortex/Recent)
         reads the contextful turn + payload
         continues the task
              │
Step 6 ─ router returns 2xx
         → chronosd marks the fire done
         (cron: reschedule next_fire_at; once: status=fired)
         non-2xx → retry ladder
```

---

## Wake request

```go
type Request struct {
    UserID         string          `json:"user_id"`
    ConversationID string          `json:"conversation_id,omitempty"`
    Message        string          `json:"message"`
    Payload        json.RawMessage `json:"payload,omitempty"`
    AlarmID        string          `json:"alarm_id"`
    Origin         string          `json:"origin"` // always "chronos"
}
```

The `Origin` field is always set to `"chronos"` by the `Wake` method. The router and daemon can use this to distinguish timer wakes from user-typed messages.

---

## Waker interface

```go
type Waker interface {
    Wake(ctx context.Context, req Request) error
}
```

Implementations must return a non-nil error on any non-2xx response so the dispatch retry ladder can act. The only implementation is `HTTPWaker`.

---

## HTTPWaker

```go
type HTTPWaker struct {
    URL    string
    Token  string
    Client *http.Client
}

func New(url, token string) *HTTPWaker
```

Constructs a waker with a 60s client timeout. The `Wake` method:

1. Sets `req.Origin = "chronos"`
2. Marshals the request as JSON
3. POSTs to `URL` with `Authorization: Bearer <Token>` (when token is non-empty)
4. Returns an error for any transport failure or non-2xx response
5. On error, the response body (first 300 chars) is included in the error message for honest failure recording

```go
waker := wake.New("http://127.0.0.1:8088/internal/wake", os.Getenv("CHRONOS_WAKE_TOKEN"))
err := waker.Wake(ctx, wake.Request{
    UserID:         "11111111-2222-3333-4444-555555555555",
    ConversationID: "conv_abc123",
    Message:        "Resume the airdrop: cursor at holder 240/512, batch size 50…",
    Payload:        json.RawMessage(`{"cursor":240,"batch":50}`),
    AlarmID:        "alarm_xyz789",
})
```

---

## Context is the point

The wake message and payload are the key feature. The agent writes a message TO ITS FUTURE SELF at post time. Chronos is a faithful courier that delivers it at the due moment, unchanged.

**wake_message** — agent-authored, first-person, self-sufficient. Contains what it was doing, what to do now, and any ids/state needed.

Example:
> "Resume the airdrop you paused: cursor at holder 240/512, batch size 50, contract 0xABC…; continue from holder 241"

**payload** — structured side-channel for state too bulky or precise for prose (verbatim ids, hashes, cursors). Echoed back so high-entropy tokens survive intact — the same verbatim discipline as Neo compaction.

**conversation_id** — delivering into the stored conversation means the agent also regains its full prior transcript + cortex memory for that thread. The wake message is the trigger; the conversation is the continuity.

---

## Delivery marker

The delivered turn is tagged so the agent knows it was woken by a timer, not typed by the user:

- `origin = "chronos"`
- `alarm_id` = the alarm's UUID

This lets the agent adjust its behavior (e.g. skip the greeting, go straight to resuming the task).

---

## Security

The wake endpoint is gated by `CHRONOS_WAKE_TOKEN` — a shared secret between chronosd and the router. The router's `ROUTER_WAKE_TOKEN` must match. In production, both are required; in dev (`CHRONOS_DEV=1`), a missing token is allowed with a warning.

The router resolves `user_id` from the wake request body, but the wake token proves the caller is chronosd (not an arbitrary agent). The router trusts chronosd to supply the correct `user_id` because chronosd derived it from the verified agent DID at alarm creation time.
