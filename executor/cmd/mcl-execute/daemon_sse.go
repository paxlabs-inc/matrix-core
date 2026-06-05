// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_sse.go — SSE (Server-Sent Events) broker.
//
// The daemon's *transcript is the single source-of-truth event stream.
// Every runtime.EventSink call (compile.intent.hashed, plan.tool.dispatch,
// replay.rebuild.complete, …) is forwarded to:
//
//   1. The on-disk JSONL transcript (durable, append-only).
//   2. Every currently-connected SSE subscriber (live web clients).
//
// (1) is unchanged from CLI mode. (2) is provided by this broker; the
// transcript taps it via transcript.AttachBroker(broker) when the daemon
// boots.
//
// Slow subscribers DO NOT block the publisher — events are dropped
// (counter increment) when the per-subscriber buffer fills, so a stalled
// browser tab can never stall the pipeline.

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// sseEvent is the wire-format object pushed to subscribers. Mirrors the
// existing transcript JSONL row exactly so a client can replay either
// the live SSE feed or the on-disk JSONL with identical parsing.
type sseEvent struct {
	Seq    uint64                 `json:"seq"`
	TS     string                 `json:"ts"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// sseFilter narrows which events a subscriber receives.
//
// Empty fields disable the corresponding filter. The filter is
// evaluated at Publish time inside the broker so a noisy publisher
// doesn't burn CPU pushing events to subscribers that would just
// drop them.
//
// IntentID matches against fields["intent_id"] (string-typed).
// Phase matches against the event's Phase header verbatim.
// SinceSeq filters out events with seq strictly less than this.
type sseFilter struct {
	IntentID string
	Phase    string
	SinceSeq uint64
}

// allows reports whether ev passes this filter.
func (f sseFilter) allows(ev sseEvent) bool {
	if f.SinceSeq > 0 && ev.Seq < f.SinceSeq {
		return false
	}
	if f.Phase != "" && ev.Phase != f.Phase {
		return false
	}
	if f.IntentID != "" {
		got, _ := ev.Fields["intent_id"].(string)
		if got != f.IntentID {
			return false
		}
	}
	return true
}

// sseSubscriber pairs a delivery channel with its filter.
type sseSubscriber struct {
	ch     chan sseEvent
	filter sseFilter
}

// sseBroker fans a single producer (the daemon's transcript wrapper)
// to N concurrent subscribers via per-subscriber buffered channels.
// Each subscriber may register an sseFilter so it only receives
// events relevant to its scope (per-intent transcript pane, per-phase
// debug stream, etc).
type sseBroker struct {
	mu          sync.RWMutex
	subscribers map[uint64]*sseSubscriber
	nextID      uint64

	bufferSize int

	// Counters surfaced via /healthz for ops visibility.
	totalPublished uint64
	totalDropped   uint64
}

// newSSEBroker returns a broker that buffers `bufferSize` events per
// subscriber. 256 is generous for live UIs and small enough that a
// single stalled subscriber doesn't pin large amounts of RAM.
func newSSEBroker(bufferSize int) *sseBroker {
	if bufferSize <= 0 {
		bufferSize = 256
	}
	return &sseBroker{
		subscribers: map[uint64]*sseSubscriber{},
		bufferSize:  bufferSize,
	}
}

// Subscribe registers an unfiltered subscriber. Equivalent to
// SubscribeFiltered(sseFilter{}). Preserved for callers that don't
// need per-event filtering.
func (b *sseBroker) Subscribe() (uint64, <-chan sseEvent) {
	return b.SubscribeFiltered(sseFilter{})
}

// SubscribeFiltered registers a new subscriber with a per-Publish
// filter. The returned channel receives only events for which
// filter.allows(ev) returns true. Caller is responsible for draining
// the channel from a goroutine and calling Unsubscribe(id) when done.
func (b *sseBroker) SubscribeFiltered(filter sseFilter) (uint64, <-chan sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID
	sub := &sseSubscriber{
		ch:     make(chan sseEvent, b.bufferSize),
		filter: filter,
	}
	b.subscribers[id] = sub
	return id, sub.ch
}

// Unsubscribe removes a subscriber and closes its channel. Safe to call
// multiple times for the same id.
func (b *sseBroker) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		close(sub.ch)
	}
}

// Publish sends ev to every current subscriber whose filter accepts
// the event. Slow subscribers see the event dropped (and the broker
// increments TotalDropped); they do NOT block the publisher.
func (b *sseBroker) Publish(ev sseEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	atomic.AddUint64(&b.totalPublished, 1)
	for _, sub := range b.subscribers {
		if !sub.filter.allows(ev) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			atomic.AddUint64(&b.totalDropped, 1)
		}
	}
}

// Stats returns a snapshot of broker counters for /healthz.
func (b *sseBroker) Stats() (subscribers int, published, dropped uint64) {
	b.mu.RLock()
	subscribers = len(b.subscribers)
	b.mu.RUnlock()
	published = atomic.LoadUint64(&b.totalPublished)
	dropped = atomic.LoadUint64(&b.totalDropped)
	return
}

// CloseAll terminates every subscriber. Used during shutdown so SSE
// clients see clean disconnects rather than HTTP-level resets.
func (b *sseBroker) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subscribers {
		close(sub.ch)
		delete(b.subscribers, id)
	}
}

// encodeSSEEvent renders an sseEvent in the on-the-wire SSE format.
// Each event is one line `data: <json>\n\n` per the SSE spec.
func encodeSSEEvent(ev sseEvent) ([]byte, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+8)
	out = append(out, []byte("data: ")...)
	out = append(out, body...)
	out = append(out, '\n', '\n')
	return out, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
