# Conversation Store

Package `matrix/neo/internal/conversation` is Neo's durable chat-thread memory. It persists each turn as one JSON file per `conversation_id`, so history survives reloads, new chats, suspend, and redeploy.

Source file: `neo/internal/conversation/store.go`.

---

## Design decisions

**Unified history with the daemon.** The on-disk shape is byte-compatible with the daemon's own conversation store. Neo derives the SAME directory (`filepath.Dir(cortexRoot)/conversations` = `/data/conversations` in prod), so a user's pre-Neo daemon threads and their new Neo threads list together as one unified history.

**Pure side-channel.** It never touches cortex, signs anything, or perturbs replay. Conversation continuity and the audit/replay chain are independent storage.

**Atomic writes.** Each append uses tmp + rename so a crash never leaves a corrupt file.

**Best-effort.** IO errors are logged, never fatal. A blank conversation id or text is ignored.

---

## Store

```go
type Store struct {
    mu  sync.Mutex
    dir string
}
```

```go
store := conversation.Open(dir) // dir="" yields a disabled store (safe no-op)
```

### Turn

```go
type Turn struct {
    Role     string    `json:"role"` // "user" | "assistant"
    Text     string    `json:"text"`
    IntentID string    `json:"intent_id,omitempty"`
    TS       time.Time `json:"ts"`
}
```

### Record

```go
type Record struct {
    ConversationID string    `json:"conversation_id"`
    Title          string    `json:"title,omitempty"`
    Turns          []Turn    `json:"turns"`
    Updated        time.Time `json:"updated"`
}
```

---

## Operations

### Append

```go
func (s *Store) Append(convID string, turn Turn)
func (s *Store) AppendUser(convID, text string)
func (s *Store) AppendAssistant(convID, intentID, text string)
```

Records one turn and persists atomically. Zero TS is filled with `time.Now().UTC()`. Unbounded — all turns are retained (no cap).

### Recent

```go
func (s *Store) Recent(convID string, n int) []Turn
```

Returns the last `n` turns (oldest-first), or nil when there are none. Used for resume seeding — `DefaultRecallTurns` = 16.

### Get

```go
func (s *Store) Get(convID string) *Record
```

Returns the full turn log for one conversation, or nil when not found.

### List

```go
func (s *Store) List() []Summary
```

Returns a summary of every persisted conversation, newest-first:

```go
type Summary struct {
    ConversationID string    `json:"conversation_id"`
    Title          string    `json:"title"`
    Preview        string    `json:"preview"`
    TurnCount      int       `json:"turn_count"`
    Updated        time.Time `json:"updated"`
}
```

Title derives from the first user turn (trimmed to 60 runes). Preview is the most recent turn (trimmed to 100 runes).

---

## Directory resolution

```go
func Dir(override, cortexRoot string) string
```

- Explicit `NEO_CONVERSATIONS_DIR` wins
- Otherwise derives from `filepath.Dir(cortexRoot)` → sibling of cortex root
- Returns `""` when neither is available (persistence disabled)

This matches how `server.MediaDir` derives `/data/media` from `/data/cortex`.

---

## Daemon compatibility

The JSON tags match the daemon store and the web client's `ConversationTurn`. A file written by the daemon is readable by Neo, and vice versa. This is tested in `TestDaemonFileCompatible`.

---

## Modifying the store

| What to change | Where |
|---|---|
| Default recall turns | `conversation/store.go` — `DefaultRecallTurns` |
| Title/preview length | `conversation/store.go` — `truncateLabel()` |
| Directory derivation | `conversation/store.go` — `Dir()` |
| JSON shape | `conversation/store.go` — `Turn`, `Record`, `Summary` structs |
