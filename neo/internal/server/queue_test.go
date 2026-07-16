// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeExec is a controllable executor for queue tests. The blocking variant
// (started+release non-nil) signals each turn's start on `started` and blocks
// on `release` until the test lets it proceed (or the context is cancelled).
// The fast variant (started+release nil) returns immediately — used for the
// race test.
type fakeExec struct {
	mu          sync.Mutex
	calls       []fakeCall
	maxInFlight int
	inFlight    int

	started chan fakeCall // blocking: signals turn start
	release chan struct{} // blocking: turn blocks here until released
}

type fakeCall struct {
	gen     uint64
	message string
}

func newBlockingExec() *fakeExec {
	return &fakeExec{
		started: make(chan fakeCall), // unbuffered: precise sync
		release: make(chan struct{}), // unbuffered: precise sync
	}
}

func newFastExec() *fakeExec {
	return &fakeExec{}
}

func (f *fakeExec) exec(ctx context.Context, gen uint64, msg string) error {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.calls = append(f.calls, fakeCall{gen: gen, message: msg})
	f.mu.Unlock()

	if f.started != nil {
		f.started <- fakeCall{gen: gen, message: msg}
	}

	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
			return ctx.Err()
		}
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return nil
}

func (f *fakeExec) callMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.message
	}
	return out
}

// resultTracker records every non-stale onResult callback.
type resultTracker struct {
	mu      sync.Mutex
	results []resultEntry
}

type resultEntry struct {
	gen uint64
	err error
}

func (rt *resultTracker) onResult(gen uint64, err error) {
	rt.mu.Lock()
	rt.results = append(rt.results, resultEntry{gen: gen, err: err})
	rt.mu.Unlock()
}

func (rt *resultTracker) gens() []uint64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]uint64, len(rt.results))
	for i, r := range rt.results {
		out[i] = r.gen
	}
	return out
}

// TestQueue_SerialPerSession verifies strict serial execution: no two turns
// overlap within a session. Three messages are enqueued; the executor blocks
// on each until released. maxInFlight must be 1 and the messages must be
// processed in FIFO order.
func TestQueue_SerialPerSession(t *testing.T) {
	exec := newBlockingExec()
	var rt resultTracker
	q := newCommandQueue(QueueFollowup, exec.exec, rt.onResult)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	q.Enqueue("msg1")
	q.Enqueue("msg2")
	q.Enqueue("msg3")

	// Turn 1
	<-exec.started
	exec.release <- struct{}{}

	// Turn 2
	<-exec.started
	exec.release <- struct{}{}

	// Turn 3
	<-exec.started
	exec.release <- struct{}{}

	q.Close()

	if exec.maxInFlight != 1 {
		t.Errorf("maxInFlight = %d, want 1 (strict serial — no two runs overlap)", exec.maxInFlight)
	}
	msgs := exec.callMessages()
	want := []string{"msg1", "msg2", "msg3"}
	if len(msgs) != len(want) {
		t.Fatalf("calls = %v, want %v", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, msgs[i], want[i])
		}
	}
}

// TestQueue_CollectCoalesces verifies collect mode: messages queued while a
// turn is active are coalesced into ONE followup turn when the active turn
// finishes.
func TestQueue_CollectCoalesces(t *testing.T) {
	exec := newBlockingExec()
	var rt resultTracker
	q := newCommandQueue(QueueCollect, exec.exec, rt.onResult)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	// Start turn A
	q.Enqueue("A")
	<-exec.started

	// Queue B and C while A is running
	q.Enqueue("B")
	q.Enqueue("C")

	// Release A — the pump should coalesce B+C into one turn
	exec.release <- struct{}{}

	// The coalesced followup turn
	c2 := <-exec.started
	exec.release <- struct{}{}

	q.Close()

	msgs := exec.callMessages()
	if len(msgs) != 2 {
		t.Fatalf("calls = %v, want 2 (A + one coalesced B+C turn)", msgs)
	}
	if msgs[0] != "A" {
		t.Errorf("call 0 = %q, want %q", msgs[0], "A")
	}
	if msgs[1] != "B\n\nC" {
		t.Errorf("call 1 = %q, want %q (coalesced B and C)", msgs[1], "B\n\nC")
	}
	if c2.gen < 2 {
		t.Errorf("coalesced turn gen = %d, want >= 2", c2.gen)
	}
}

// TestQueue_FollowupAfterActiveRun verifies followup mode: each queued message
// gets its own turn, in FIFO order, after the active run finishes. No
// coalescing.
func TestQueue_FollowupAfterActiveRun(t *testing.T) {
	exec := newBlockingExec()
	var rt resultTracker
	q := newCommandQueue(QueueFollowup, exec.exec, rt.onResult)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	// Start turn A
	q.Enqueue("A")
	c1 := <-exec.started

	// Queue B and C while A is running
	q.Enqueue("B")
	q.Enqueue("C")

	// Release A — B should start next (its own turn)
	exec.release <- struct{}{}
	c2 := <-exec.started

	// Release B — C should start next (its own turn)
	exec.release <- struct{}{}
	c3 := <-exec.started

	// Release C
	exec.release <- struct{}{}

	q.Close()

	msgs := exec.callMessages()
	want := []string{"A", "B", "C"}
	if len(msgs) != len(want) {
		t.Fatalf("calls = %v, want %v (followup = one turn each, no coalescing)", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, msgs[i], want[i])
		}
	}
	// Generations must be strictly increasing (each turn = new generation)
	if !(c1.gen < c2.gen && c2.gen < c3.gen) {
		t.Errorf("generations must be strictly increasing: %d, %d, %d", c1.gen, c2.gen, c3.gen)
	}
}

// TestQueue_StaleResultDiscarded verifies that a superseded turn's result is
// discarded. Stop() bumps the generation counter past the active turn; when
// the active turn's executor returns (context cancelled), its result is stale
// and onResult is NOT called. A subsequent message dispatches at a new
// generation and its result IS delivered.
func TestQueue_StaleResultDiscarded(t *testing.T) {
	exec := newBlockingExec()
	var rt resultTracker
	q := newCommandQueue(QueueFollowup, exec.exec, rt.onResult)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	// Start turn A (gen 1)
	q.Enqueue("A")
	c1 := <-exec.started
	if c1.gen != 1 {
		t.Fatalf("first turn gen = %d, want 1", c1.gen)
	}

	// Stop the active turn — supersedes gen 1, cancels its context
	q.Stop()

	// The executor returns via ctx.Done() (stale result discarded).
	// Enqueue B — dispatches at a new generation.
	q.Enqueue("B")
	c2 := <-exec.started
	exec.release <- struct{}{}

	q.Close()

	gens := rt.gens()
	// onResult must NOT have been called for gen 1 (stale), only for B's gen.
	for _, g := range gens {
		if g == c1.gen {
			t.Errorf("onResult called for stale gen %d — should have been discarded", c1.gen)
		}
	}
	if len(gens) == 0 {
		t.Fatal("onResult was never called — B's result should have been delivered")
	}
	// B's result must be the only one delivered
	if len(gens) != 1 {
		t.Errorf("onResult called %d times, want 1 (only B's non-stale result)", len(gens))
	}
	if gens[0] != c2.gen {
		t.Errorf("delivered gen = %d, want %d (B's gen)", gens[0], c2.gen)
	}
}

// TestQueue_RaceClean hammers the queue with concurrent Enqueue + Stop calls
// from multiple goroutines. The -race detector must report no data races, and
// strict serial execution (maxInFlight == 1) must hold under contention.
func TestQueue_RaceClean(t *testing.T) {
	exec := newFastExec()
	var rt resultTracker
	q := newCommandQueue(QueueFollowup, exec.exec, rt.onResult)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				q.Enqueue(fmt.Sprintf("g%d-msg%d", n, j))
				if j%17 == 0 {
					q.Stop()
				}
			}
		}(i)
	}
	wg.Wait()

	// Let the pump drain the backlog
	time.Sleep(200 * time.Millisecond)
	q.Close()

	if exec.maxInFlight > 1 {
		t.Errorf("maxInFlight = %d, want <= 1 (strict serial under contention)", exec.maxInFlight)
	}
}
