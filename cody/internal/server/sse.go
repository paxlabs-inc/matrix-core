// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"sync"
	"time"
)

// Event is one SSE frame on a run topic. The wire envelope
// {seq, ts, phase, type, fields} matches the Neo daemon's shape so the client
// reuses one reducer.
type Event struct {
	Seq    int                    `json:"seq"`
	Ts     string                 `json:"ts"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

const (
	// subBuffer is a subscriber's channel depth; a slower consumer drops.
	subBuffer = 256
	// maxReplayBuf caps the per-topic replay ring.
	maxReplayBuf = 512
)

// broker fans events out per run topic with seq-stamped replay, mirroring the
// proven Neo shape: ensure-at-dispatch, non-blocking publish, replay + live
// subscribe, closeRun at terminal, and a post-publish tap for the durable
// trace.
type broker struct {
	mu     sync.Mutex
	topics map[string]*topicState
	// tap receives every published event outside the topic lock — the
	// durable-trace seam.
	tap func(id string, ev Event)
}

type topicState struct {
	mu     sync.Mutex
	seq    int
	buf    []Event
	subs   map[int]chan Event
	nextID int
	closed bool
}

func newBroker() *broker { return &broker{topics: map[string]*topicState{}} }

func (b *broker) setTap(tap func(id string, ev Event)) { b.tap = tap }

// ensure creates the topic BEFORE dispatch returns, closing the
// dispatch->subscribe race.
func (b *broker) ensure(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.topics[id]; !ok {
		b.topics[id] = &topicState{subs: map[int]chan Event{}}
	}
}

func (b *broker) topic(id string) *topicState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.topics[id]
}

// has reports whether the broker owns a live-or-replayable topic for id.
func (b *broker) has(id string) bool { return b.topic(id) != nil }

// publish stamps and fans an event out to subscribers (non-blocking; a slow
// subscriber drops), appends it to the replay ring, then feeds the tap.
func (b *broker) publish(id, typ, phase string, fields map[string]interface{}) Event {
	b.ensure(id)
	t := b.topic(id)
	t.mu.Lock()
	t.seq++
	ev := Event{Seq: t.seq, Ts: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Type: typ, Fields: fields}
	t.buf = append(t.buf, ev)
	if len(t.buf) > maxReplayBuf {
		t.buf = t.buf[len(t.buf)-maxReplayBuf:]
	}
	for _, ch := range t.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	t.mu.Unlock()
	if b.tap != nil {
		b.tap(id, ev)
	}
	return ev
}

// subscribe returns the replay buffer past sinceSeq plus a live channel; the
// channel is nil when the topic already closed (replay-only).
func (b *broker) subscribe(id string, sinceSeq int) ([]Event, chan Event, func()) {
	t := b.topic(id)
	if t == nil {
		return nil, nil, func() {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var replay []Event
	for _, ev := range t.buf {
		if ev.Seq > sinceSeq {
			replay = append(replay, ev)
		}
	}
	if t.closed {
		return replay, nil, func() {}
	}
	subID := t.nextID
	t.nextID++
	ch := make(chan Event, subBuffer)
	t.subs[subID] = ch
	cancel := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, ok := t.subs[subID]; ok {
			delete(t.subs, subID)
			close(ch)
		}
	}
	return replay, ch, cancel
}

// closeRun marks a topic terminal: live subscribers are closed; the replay
// ring stays readable until drop.
func (b *broker) closeRun(id string) {
	t := b.topic(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for subID, ch := range t.subs {
		delete(t.subs, subID)
		close(ch)
	}
}

// drop forgets a topic entirely.
func (b *broker) drop(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.topics, id)
}
