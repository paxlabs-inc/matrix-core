// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_surfacestore_test.go — integration test for the Construct surface-store
// tee (daemon_surfacestore.go) wired to the REAL SSE broker (daemon_sse.go) and
// the REAL durable store (packages/construct/surfacestore), with a real /data-style temp
// dir. No fakes/mocks for any component under test: the whole point of this test
// is that the live broker → tee goroutine → durable store path behaves correctly
// as an integrated whole.
//
// It proves the two load-bearing hot-path properties (R16.1/16.2/16.3) plus the
// side-channel invariant (R11.1) at the INTEGRATION level — distinct from the
// store's own unit-level TestRecordNeverBlocks, which exercises Store.Record in
// isolation:
//
//  1. NON-BLOCKING UNDER SATURATION (R16.1/16.2): flooding the wired path far
//     beyond the store's writer-queue depth (queueDepth = 8192) must NOT slow the
//     broker publish path. A saturated writer queue (which drops) can never push
//     back through the tee subscriber channel to stall broker.Publish or the agent
//     loop. We measure that every Publish returns promptly and the whole burst
//     finishes well within a deadline.
//
//  2. DROP, NOT BLOCK (R16.3): under a burst larger than can be drained, frames
//     are DROPPED rather than blocking. The system makes progress (no deadlock /
//     timeout): some frames persist, but strictly fewer than were produced — the
//     surface record is a best-effort observability sink.
//
//  3. NO CORTEX/PLAN/WALK SIDE EFFECTS (R11.1, D11): the tee is a pure
//     observability SINK. recordSurfaceFrame's ONLY effect is a best-effort
//     surfacestore.Record append; it never signs, writes cortex, or mutates
//     plan/walk. We assert this structurally: (a) the daemonState under test wires
//     ONLY the broker + store, so there is no cortex/plan/walk handle the tee could
//     touch; (b) the only filesystem artifact produced is the conversation's
//     append-only surfaces JSONL — nothing else is created (no signed envelope, no
//     cortex segment); and (c) the tee records ONLY construct-phase surface frames,
//     ignoring every other pipeline event, so it cannot perturb any other phase.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"centra/packages/construct/surfacestore"
	"centra/packages/construct/transport"
)

// TestSurfaceStoreTee_NonBlockingDropOnSaturation drives the real broker + tee +
// store together and asserts the tee never blocks the publish path, drops on
// saturation while still making progress, and produces no side effect beyond the
// best-effort surface append (D11 side-channel).
func TestSurfaceStoreTee_NonBlockingDropOnSaturation(t *testing.T) {
	const (
		intentID       = "intent_tee_saturation"
		conversationID = "conv_tee_saturation"
		// Far beyond the store writer queue depth (8192) so the queue is
		// guaranteed to saturate and Record must drop. The producer is orders of
		// magnitude faster than the disk writer (which open/append/close-s per
		// frame), so the queue fills almost immediately.
		burst = 20000
		// Generous per-Publish latency ceiling. A correct (non-blocking) publish
		// path returns in microseconds even while the writer queue is saturated;
		// a regression that let store saturation push back through the tee would
		// blow past this by orders of magnitude (the writer is disk-bound).
		maxPublishLatency = 250 * time.Millisecond
		// Overall burst deadline: a blocking path would never finish.
		burstDeadline = 5 * time.Second
	)

	// Retain far above the burst so the amortized rollup (which triggers at
	// 2×retain) NEVER fires for this test. That removes rollup as a confound:
	// any shortfall between produced and persisted frames is therefore due to
	// drop-on-saturation alone, making the "drop, not block" assertion exact.
	t.Setenv("CONSTRUCT_SURFACE_RETAIN", "1000000")

	storeDir := t.TempDir()
	store := surfacestore.Open(storeDir)
	defer store.Close()
	if !store.Enabled() {
		t.Fatal("store should be enabled for a non-empty /data-style dir")
	}

	broker := newSSEBroker(0) // default per-subscriber buffer (256)
	d := &daemonState{broker: broker, surfaceStore: store}

	// The store's drop path logs one line to os.Stderr per dropped frame. Under a
	// 20k burst that is thousands of lines, and writing them to the captured test
	// stderr would dominate wall-clock (especially under -race), masking the
	// non-blocking property under test. Redirect stderr to a temp file for the
	// duration of the burst — mirroring the store's own TestRecordNeverBlocks
	// discipline — so the deadline measures the publish path, not terminal I/O.
	sink, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if err != nil {
		t.Fatalf("create stderr sink: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = sink
	defer func() { os.Stderr = origStderr; _ = sink.Close() }()

	tee := d.startSurfaceStoreTee(context.Background(), intentID, conversationID)

	// (Filter check) Publish a handful of NON-construct pipeline events first,
	// while the broker channel is empty so they are delivered deterministically.
	// recordSurfaceFrame must ignore them (phase != "construct"), so they must
	// never reach the durable store — proving the tee is a pure construct-surface
	// sink that cannot perturb other phases.
	for i := 0; i < 8; i++ {
		broker.Publish(sseEvent{
			Seq:   uint64(i + 1),
			TS:    "2026-01-01T00:00:00Z",
			Phase: "plan",
			Type:  "plan.tool.dispatch",
			Fields: map[string]interface{}{
				"intent_id": intentID,
				"step":      strconv.Itoa(i),
			},
		})
	}

	// (Properties 1 & 2) Flood the wired path with construct surface frames and
	// measure that no single Publish stalls and the whole burst completes within
	// the deadline. Done on a goroutine so we can detect a hard block via the
	// deadline; maxLatency is published to the main goroutine via the done close.
	var maxLatency time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < burst; i++ {
			ev := sseEvent{
				Seq:   uint64(i + 100),
				TS:    "2026-01-01T00:00:00Z",
				Phase: transport.Phase,
				Type:  transport.EventSurface,
				Fields: map[string]interface{}{
					"intent_id": intentID,
					"id":        "s" + strconv.Itoa(i),
				},
			}
			start := time.Now()
			broker.Publish(ev)
			if lat := time.Since(start); lat > maxLatency {
				maxLatency = lat
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(burstDeadline):
		t.Fatalf("broker publish path blocked under writer-queue saturation after %s — "+
			"the tee/store must drop rather than ever stall Publish (R16.1/16.3)", burstDeadline)
	}

	if maxLatency > maxPublishLatency {
		t.Fatalf("a single broker publish took %v under saturation — publish latency must be "+
			"unaffected by a saturated writer queue (R16.1/16.2)", maxLatency)
	}

	// Drain the tee (so every delivered frame has been handed to Record) and
	// flush the store (so every non-dropped frame is on disk), then read back.
	tee.shutdown()
	store.Flush()
	loaded := store.Load(conversationID)

	// (Property 2 — progress) The system made progress: it persisted frames
	// rather than deadlocking.
	if len(loaded) == 0 {
		t.Fatal("no frames persisted — the tee made no progress under load (expected best-effort persistence)")
	}

	// (Property 2 — drop, not block) Strictly fewer frames persisted than were
	// produced: frames were DROPPED on saturation, not blocked through. With
	// rollup disabled (huge retain), this shortfall is attributable to drops
	// alone.
	if len(loaded) >= burst {
		t.Fatalf("persisted %d of %d frames with rollup disabled — saturation must DROP frames, "+
			"so persisted must be strictly fewer than produced (R16.3)", len(loaded), burst)
	}
	t.Logf("integration: produced=%d persisted=%d dropped=%d (drop-on-saturation, non-blocking)",
		burst, len(loaded), burst-len(loaded))

	// (Side-channel, part c) Every persisted frame is a construct-phase surface
	// frame: the non-construct plan events were ignored, so the tee touched no
	// other phase.
	for _, fr := range loaded {
		if fr.Phase != transport.Phase {
			t.Fatalf("persisted a non-construct frame (phase=%q) — the tee must record ONLY "+
				"construct-phase surface frames", fr.Phase)
		}
		if fr.Type != transport.EventSurface && fr.Type != transport.EventSurfacePatch {
			t.Fatalf("persisted a non-surface frame (type=%q) — the tee must record ONLY "+
				"construct.surface[.patch] frames", fr.Type)
		}
	}

	// (Side-channel, part b) The ONLY filesystem artifact is the conversation's
	// append-only surfaces JSONL. No signed envelope, no cortex segment, no
	// plan/walk mutation — the tee is a pure observability sink (R11.1 / D11).
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	wantFile := conversationID + ".surfaces.jsonl"
	if len(entries) != 1 || entries[0].Name() != wantFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("store dir contains %v — the only side effect must be the surface JSONL %q "+
			"(no signing / cortex / plan / walk artifacts)", names, wantFile)
	}

	// Sanity: the persisted file is exactly where we expect under the per-user
	// /data-style root (conversation-scoped, no path escape).
	if _, err := os.Stat(filepath.Join(storeDir, wantFile)); err != nil {
		t.Fatalf("expected surface JSONL at %s: %v", filepath.Join(storeDir, wantFile), err)
	}
}
