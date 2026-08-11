// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"strings"
	"sync"
)

// QueueMode controls how messages queued while a turn is in flight are handled
// once the active turn finishes.
type QueueMode int

const (
	// QueueFollowup waits for the active turn to finish, then starts a new turn
	// for each queued message in FIFO order (one turn per message).
	QueueFollowup QueueMode = iota
	// QueueCollect coalesces all messages queued during an active turn into a
	// single followup turn (joined with a blank-line separator) once the active
	// turn finishes.
	QueueCollect
)

// commandQueue is the per-session command lane. It guarantees strict serial
// execution — no two turns overlap within a session — and processes queued
// inbound messages according to its mode:
//
//   - QueueCollect: coalesce queued messages into one followup turn.
//   - QueueFollowup: run each queued message as its own turn, in FIFO order.
//
// A monotonically increasing generation counter tags every dispatched turn.
// A result whose generation has been superseded (by an explicit Stop or a
// newer dispatch) is discarded as stale — the onResult callback fires ONLY for
// the current generation.
//
// This implements P2-6 (lane-based concurrency). Steer / nextTurn are out of
// scope and logged as a follow-up.
type commandQueue struct {
	mode QueueMode

	mu      sync.Mutex
	pending []string
	gen     uint64 // current generation (last dispatched or superseded)
	active  bool   // a turn is in flight
	notify  chan struct{}

	// executor runs one turn. gen is the turn's generation. The context is
	// cancelled on Stop/supersede. Returns the turn's terminal error
	// (nil = clean completion).
	executor func(ctx context.Context, gen uint64, message string) error

	// onResult is called (in the pump goroutine) when a turn completes, ONLY
	// if the turn's generation is still current (not superseded). Stale
	// results are silently dropped.
	onResult func(gen uint64, err error)

	runCancel context.CancelFunc // cancels the active turn's context

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// newCommandQueue creates a per-session command queue with the given mode,
// executor, and result callback. Call Start to launch the pump goroutine.
func newCommandQueue(mode QueueMode, executor func(ctx context.Context, gen uint64, message string) error, onResult func(gen uint64, err error)) *commandQueue {
	return &commandQueue{
		mode:     mode,
		executor: executor,
		onResult: onResult,
		notify:   make(chan struct{}, 1),
	}
}

// Start launches the single pump goroutine that serializes turns. Safe to call
// once; subsequent calls are no-ops.
func (q *commandQueue) Start(ctx context.Context) {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.ctx, q.cancel = context.WithCancel(ctx)
	q.mu.Unlock()

	q.wg.Add(1)
	go q.pump()
}

// Enqueue adds a message to the pending buffer and wakes the pump. The message
// will be dispatched (or coalesced, in collect mode) when the active turn
// finishes — never interrupting the in-flight turn.
func (q *commandQueue) Enqueue(msg string) {
	q.mu.Lock()
	q.pending = append(q.pending, msg)
	q.mu.Unlock()
	// Non-blocking wake: if the pump is in its select it consumes this; if it
	// is busy (in the executor) the pending slice is already updated and will
	// be drained on the next loop iteration.
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Stop supersedes the active turn: bumps the generation counter past it and
// cancels its context. When the active turn's executor returns, its result is
// discarded as stale (onResult is NOT called). No-op when no turn is active.
func (q *commandQueue) Stop() {
	q.mu.Lock()
	if q.active {
		q.gen++ // supersede: the active generation is now stale
		if q.runCancel != nil {
			q.runCancel()
		}
	}
	q.mu.Unlock()
}

// Generation returns the current generation counter (last dispatched or
// superseded). A result tagged with a generation < this value is stale.
func (q *commandQueue) Generation() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.gen
}

// IsStale reports whether the given generation has been superseded.
func (q *commandQueue) IsStale(gen uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return gen < q.gen
}

// Close shuts down the pump goroutine, cancelling any in-flight turn. It waits
// for the pump to exit. After Close, the queue must not be reused.
func (q *commandQueue) Close() {
	q.mu.Lock()
	if q.cancel != nil {
		q.cancel()
	}
	q.mu.Unlock()
	q.wg.Wait()
}

// pump is the single goroutine that serializes turns. It drains pending
// messages according to the queue mode, dispatches one at a time, and delivers
// results only for non-stale generations.
func (q *commandQueue) pump() {
	defer q.wg.Done()
	for {
		// Wait for pending messages.
		q.mu.Lock()
		for len(q.pending) == 0 {
			if q.ctx.Err() != nil {
				q.mu.Unlock()
				return
			}
			q.mu.Unlock()
			select {
			case <-q.notify:
			case <-q.ctx.Done():
				return
			}
			q.mu.Lock()
		}

		msg := q.drainLocked()
		q.gen++
		gen := q.gen
		q.active = true
		runCtx, runCancel := context.WithCancel(q.ctx)
		q.runCancel = runCancel
		q.mu.Unlock()

		// Execute the turn (blocks until done — strict serial).
		err := q.executor(runCtx, gen, msg)
		runCancel()

		q.mu.Lock()
		q.active = false
		q.runCancel = nil
		stale := gen < q.gen // a newer gen was dispatched (Stop supersede)
		q.mu.Unlock()

		if !stale && q.onResult != nil {
			q.onResult(gen, err)
		}
	}
}

// drainLocked drains pending messages according to the queue mode. Caller must
// hold q.mu.
func (q *commandQueue) drainLocked() string {
	if len(q.pending) == 0 {
		return ""
	}
	switch q.mode {
	case QueueCollect:
		msg := strings.Join(q.pending, "\n\n")
		q.pending = q.pending[:0]
		return msg
	default: // QueueFollowup — one message at a time, FIFO
		msg := q.pending[0]
		q.pending = q.pending[1:]
		return msg
	}
}
