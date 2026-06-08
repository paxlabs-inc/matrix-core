// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"sync"
	"time"
)

// The broker fans Neo's per-run events out to SSE subscribers and retains a
// replay buffer per run so a client that subscribes a beat after POST /chat
// (the dispatch→subscribe race) still receives every event. The wire envelope
// is byte-compatible with the daemon's sseEvent ({seq,ts,phase,type,fields}),
// so the existing web + Telegram clients parse Neo's stream unchanged.

// Event is one SSE frame. Mirrors executor/cmd/mcl-execute sseEvent.
type Event struct {
	Seq    int                    `json:"seq"`
	Ts     string                 `json:"ts"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

const (
	subBuffer    = 256 // per-subscriber channel depth before drop
	maxReplayBuf = 512 // per-run retained events for replay
)

// broker keeps one topic per run id (the conversation turn's intent_id).
type broker struct {
	mu     sync.Mutex
	topics map[string]*topicState
}

type topicState struct {
	mu     sync.Mutex
	seq    int
	buf    []Event
	subs   map[int]chan Event
	nextID int
	closed bool
}

func newBroker() *broker {
	return &broker{topics: map[string]*topicState{}}
}

func (b *broker) topic(id string) *topicState {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts, ok := b.topics[id]
	if !ok {
		ts = &topicState{subs: map[int]chan Event{}}
		b.topics[id] = ts
	}
	return ts
}

// has reports whether a run id is one of Neo's own topics (vs. a daemon intent
// the request should be reverse-proxied for).
func (b *broker) has(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.topics[id]
	return ok
}

// ensure creates the topic for id if absent. session.start calls this at
// dispatch time — BEFORE POST /chat returns and before the run's first publish
// — so handleEvents/handleReplay/handleAsyncPoll recognise the id as Neo's the
// instant the client connects. Without it the client's immediate subscribe
// loses the race against the first publish, has(id) is false, and the stream
// is reverse-proxied to the daemon's (empty) event stream forever — the run
// completes but the user never sees the answer.
func (b *broker) ensure(id string) { b.topic(id) }

// publish stamps and appends an event, then fans it out to live subscribers.
func (b *broker) publish(id, typ, phase string, fields map[string]interface{}) Event {
	ts := b.topic(id)
	ts.mu.Lock()
	ts.seq++
	ev := Event{
		Seq:    ts.seq,
		Ts:     time.Now().UTC().Format(time.RFC3339Nano),
		Phase:  phase,
		Type:   typ,
		Fields: fields,
	}
	ts.buf = append(ts.buf, ev)
	if len(ts.buf) > maxReplayBuf {
		ts.buf = ts.buf[len(ts.buf)-maxReplayBuf:]
	}
	for _, ch := range ts.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than block the agent loop
		}
	}
	ts.mu.Unlock()
	return ev
}

// closeRun marks a topic done and closes all subscriber channels so their SSE
// handlers return.
func (b *broker) closeRun(id string) {
	ts := b.topic(id)
	ts.mu.Lock()
	if !ts.closed {
		ts.closed = true
		for _, ch := range ts.subs {
			close(ch)
		}
		ts.subs = map[int]chan Event{}
	}
	ts.mu.Unlock()
}

// subscribe returns the current replay buffer plus a live channel for
// subsequent events, and a cancel func to detach. If the run already closed,
// ch is nil (caller streams the replay then returns).
func (b *broker) subscribe(id string, sinceSeq int) (replay []Event, ch chan Event, cancel func()) {
	ts := b.topic(id)
	ts.mu.Lock()
	defer ts.mu.Unlock()

	for _, ev := range ts.buf {
		if ev.Seq > sinceSeq {
			replay = append(replay, ev)
		}
	}
	if ts.closed {
		return replay, nil, func() {}
	}
	id2 := ts.nextID
	ts.nextID++
	c := make(chan Event, subBuffer)
	ts.subs[id2] = c
	return replay, c, func() {
		ts.mu.Lock()
		if cur, ok := ts.subs[id2]; ok {
			delete(ts.subs, id2)
			close(cur)
		}
		ts.mu.Unlock()
	}
}

// drop discards a finished run's topic to bound memory. Called after a grace
// period so late reconnects can still replay.
func (b *broker) drop(id string) {
	b.mu.Lock()
	delete(b.topics, id)
	b.mu.Unlock()
}
