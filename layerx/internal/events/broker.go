// Package events is the in-process publish/subscribe fan-out backing the live
// SSE stream (GET /v1/stream). The Matrix settlement fabric is ONE always-on
// sequencer process that owns every writer (the ledger Pay path, the settlement
// worker) and every reader (the SSE handler), so a simple in-memory broker is
// the entire transport — no external bus, no extra moving part. This is the
// scalable replacement for explorer polling: N clients each hold one cheap
// streamed connection instead of hammering the REST feed on a timer.
package events

import (
	"encoding/json"
	"sync"
)

// Event kinds the explorer subscribes to.
const (
	TypeTransfer = "transfer" // a new accepted transfer (carries Seq)
	TypeAnchor   = "anchor"   // a settlement batch anchored on Paxeer
	TypeBatch    = "batch"    // a settlement batch sealed (reserved; not yet emitted)
)

// Event is one server-sent event. Seq is the transfer's monotonic sequence for
// transfer events — it doubles as the SSE `id:` and the Last-Event-ID replay
// cursor. It is 0 for non-transfer events (anchor/batch), which are live-only;
// a reconnecting client refetches those over REST.
type Event struct {
	Type string
	Seq  int64
	Data json.RawMessage
}

// Publisher is the narrow write side, so producers (e.g. the settlement worker)
// depend only on Publish and stay decoupled from the broker's lifecycle.
type Publisher interface {
	Publish(Event)
}

// Broker fans Events out to all current subscribers. A slow subscriber whose
// buffer fills is dropped (its channel closed) rather than blocking the
// publisher or silently losing events: the SSE handler detects the close, tells
// the client to resync, and the client reconnects + replays from its
// Last-Event-ID. Correctness over best-effort delivery.
type Broker struct {
	mu      sync.Mutex
	subs    map[int]chan Event
	nextID  int
	bufSize int
}

// NewBroker constructs a broker. bufSize is the per-subscriber buffer; a
// subscriber that falls more than bufSize events behind is dropped.
func NewBroker(bufSize int) *Broker {
	if bufSize <= 0 {
		bufSize = 256
	}
	return &Broker{subs: make(map[int]chan Event), bufSize: bufSize}
}

// Subscribe registers a new subscriber and returns its event channel plus a
// cancel func the caller MUST invoke (defer) when done. The channel is closed
// either by cancel or when the subscriber is dropped for lagging.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, b.bufSize)
	b.subs[id] = ch
	return ch, func() { b.remove(id) }
}

// remove unregisters + closes a subscriber exactly once (guarded by the lock,
// so it is safe whether triggered by cancel or by a lagging drop in Publish).
func (b *Broker) remove(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish fans an event out to every subscriber, non-blocking. A subscriber
// whose buffer is full is dropped (channel closed) so it reconnects + replays.
func (b *Broker) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			delete(b.subs, id)
			close(ch)
		}
	}
}

// Subscribers reports the current subscriber count (observability/tests).
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
