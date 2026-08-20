// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_surfacestore.go — the Construct surface-store tee (the "never
// vanishing" persistence side-channel).
//
// This is a per-run broker subscriber, structurally a SIBLING of the Construct
// passive projector (daemon_construct.go) and the Liaison narrator
// (daemon_liaison.go): it subscribes to the SAME SSE event stream the pipeline
// already emits and, for every construct.surface[.patch] frame, tees that frame
// into the durable per-conversation surfacestore. Where the projector PRODUCES
// surfaces (turning pipeline events into construct.surface events) and the
// narrator PRODUCES chat turns, this tee is pure SINK: it records the surface
// frames the projector (and Neo's active tier) emit so a reopened conversation
// rehydrates exactly as the user left it.
//
// SIDE-CHANNEL INVARIANT (load-bearing, D11): like the projector and the
// narrator, the only effect is an append to the durable surface JSONL via
// surfacestore.Record. It NEVER signs an envelope, writes cortex, or touches
// the plan/walk, so it cannot perturb the D11 replay byte-identity invariant.
// It also adds NO new agent→client wire path (R14.1): it only tees the EXISTING
// broker frames into the store; the client still reads surfaces over the same
// chat transport.
//
// HOT-PATH DISCIPLINE (R16): the tee drains its own buffered subscriber channel
// on a dedicated goroutine (off the broker publish path), and Record is a
// non-blocking enqueue that drops on saturation — so neither the tee nor the
// store can ever block broker.Publish or the agent loop.

import (
	"context"
	"sync"
	"time"

	"centra/packages/construct/schema"
	"centra/packages/construct/transport"
)

// surfaceStoreTeeWait bounds how long shutdown blocks for the tee to finish
// draining its buffered frames before the run returns.
const surfaceStoreTeeWait = 5 * time.Second

// surfaceStoreEnabled reports whether the durable surface-store tee should run:
// only when the store resolved to a real /data-rooted directory (a disabled
// no-op store, e.g. dev/CLI, leaves the tee unspawned).
func (d *daemonState) surfaceStoreEnabled() bool {
	return d != nil && d.surfaceStore.Enabled()
}

// surfaceStoreTee is the handle runMessage uses to stop the tee goroutine and
// wait for it to drain.
type surfaceStoreTee struct {
	subID uint64
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// startSurfaceStoreTee subscribes to the broker SYNCHRONOUSLY (so no early
// surface frame is missed) and launches the tee loop. runMessage defers
// tee.shutdown() so the loop drains and detaches before t.Close(). It mirrors
// startConstructProjector exactly, except it records surface frames instead of
// projecting pipeline events.
func (d *daemonState) startSurfaceStoreTee(ctx context.Context, intentID, conversationID string) *surfaceStoreTee {
	id, ch := d.broker.SubscribeFiltered(sseFilter{IntentID: intentID})
	tee := &surfaceStoreTee{subID: id, stop: make(chan struct{}), done: make(chan struct{})}
	go d.runSurfaceStoreTee(ctx, tee, ch, conversationID)
	return tee
}

// shutdown signals the tee to drain and detach, blocking (bounded) until it
// does. Idempotent.
func (tee *surfaceStoreTee) shutdown() {
	if tee == nil {
		return
	}
	tee.once.Do(func() { close(tee.stop) })
	select {
	case <-tee.done:
	case <-time.After(surfaceStoreTeeWait):
	}
}

// runSurfaceStoreTee is the per-run tee loop. It records each construct surface
// frame live (so the durable record tracks the live workspace), then drains any
// buffered frames on stop so a surface that arrived just before the run
// returned is still persisted. Pure side-channel: the only effect is an async,
// best-effort append via the store.
func (d *daemonState) runSurfaceStoreTee(ctx context.Context, tee *surfaceStoreTee, ch <-chan sseEvent, conversationID string) {
	defer close(tee.done)
	defer d.broker.Unsubscribe(tee.subID)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tee.stop:
			// Drain whatever is immediately available so a late surface frame
			// is still recorded, then exit.
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						return
					}
					d.recordSurfaceFrame(ev, conversationID)
				default:
					return
				}
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			d.recordSurfaceFrame(ev, conversationID)
		}
	}
}

// recordSurfaceFrame tees ONE broker event into the durable surface store, but
// only for construct-phase surface frames (construct.surface[.patch]); every
// other pipeline event is ignored. The sseEvent → schema.Frame mapping is
// 1:1 (the wire shapes are identical by design), so a reopen replays the
// persisted stream through the client's reducer byte-for-byte.
//
// store.Record is non-blocking and drops on saturation, so this never blocks
// the broker publish path (R16.1/16.3).
func (d *daemonState) recordSurfaceFrame(ev sseEvent, conversationID string) {
	if ev.Phase != transport.Phase {
		return
	}
	if ev.Type != transport.EventSurface && ev.Type != transport.EventSurfacePatch {
		return
	}
	d.surfaceStore.Record(conversationID, schema.Frame{
		Seq:    int(ev.Seq),
		Ts:     ev.TS,
		Phase:  ev.Phase,
		Type:   ev.Type,
		Fields: ev.Fields,
	})
}
