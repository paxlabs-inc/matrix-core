// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_construct.go — the Construct passive projector (the side-channel floor).
//
// This is the PASSIVE tier of the Construct projection engine (Phase 4). It is
// a per-run broker subscriber, structurally a sibling of the Liaison narrator
// (daemon_liaison.go): it subscribes to the SAME SSE event stream the pipeline
// already emits, runs each event through the deterministic projection engine
// (construct/projection.ProjectEvent), and emits the resulting Construct
// surfaces as construct.surface transcript events (packages/construct/transport).
//
// It exists to cover the MCL compiler/planner/executor pipeline — where the
// agent-authored ACTIVE tier (Neo's construct_render tool) does not run — and
// to fix the historical "tool-only plan renders a 200-char dump / empty
// narration" tap-out for free, by giving a tool result a typed Entity/Structure/
// Metric surface.
//
// SIDE-CHANNEL INVARIANT (load-bearing): like the Liaison, the only output is
// transcript SSE events (construct.surface[.patch]). It NEVER signs an
// envelope, writes cortex, or touches the plan/walk, so it cannot perturb the
// D11 replay byte-identity invariant. The projection is deterministic, so a
// replay of the stored surface events reproduces them identically.

import (
	"context"
	"strings"
	"sync"
	"time"

	"centra/packages/construct/projection"
	"centra/packages/construct/transport"
)

// constructState holds the passive projector's runtime knobs. nil disables the
// projector entirely (the pipeline runs exactly as before, emitting no
// construct.surface events). Set at boot unless -construct-disable. It carries
// no model: the passive tier is purely deterministic.
type constructState struct{}

// constructProjectorWait bounds how long shutdown blocks for the projector to
// finish draining before the transcript file closes.
const constructProjectorWait = 5 * time.Second

// constructEnabled reports whether the passive Construct projector should run.
func (d *daemonState) constructEnabled() bool {
	return d != nil && d.construct != nil
}

// constructProjector is the handle runMessage uses to stop the projection
// goroutine and wait for it to drain.
type constructProjector struct {
	subID uint64
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// startConstructProjector subscribes to the broker SYNCHRONOUSLY (so no early
// pipeline event is missed) and launches the projection loop. runMessage defers
// projector.shutdown() so the loop drains and detaches before t.Close().
func (d *daemonState) startConstructProjector(ctx context.Context, t *transcript, intentID, conversationID string) *constructProjector {
	id, ch := d.broker.SubscribeFiltered(sseFilter{IntentID: intentID})
	p := &constructProjector{subID: id, stop: make(chan struct{}), done: make(chan struct{})}
	go d.runConstructProjector(ctx, t, p, ch, intentID, conversationID)
	return p
}

// shutdown signals the projector to drain and detach, blocking (bounded) until
// it does. Idempotent.
func (p *constructProjector) shutdown() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.stop) })
	select {
	case <-p.done:
	case <-time.After(constructProjectorWait):
	}
}

// runConstructProjector is the per-run projection loop. It projects each
// pipeline event live (so surfaces stream progressively, like the narration),
// then drains any buffered events on stop so a result that arrived just before
// the run returned is not lost. Pure side-channel: the only writes are
// construct.surface transcript events.
func (d *daemonState) runConstructProjector(ctx context.Context, t *transcript, p *constructProjector, ch <-chan sseEvent, intentID, conversationID string) {
	defer close(p.done)
	defer d.broker.Unsubscribe(p.subID)

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			// Drain whatever is immediately available so a late result is
			// still projected, then exit.
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						return
					}
					d.projectConstructEvent(t, ev, intentID, conversationID)
				default:
					return
				}
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			d.projectConstructEvent(t, ev, intentID, conversationID)
		}
	}
}

// projectConstructEvent runs one event through the deterministic projector and
// emits any resulting surfaces. The loop guard drops our OWN construct events
// so the projector never re-projects what it just emitted.
func (d *daemonState) projectConstructEvent(t *transcript, ev sseEvent, intentID, conversationID string) {
	if ev.Phase == transport.Phase || strings.HasPrefix(ev.Type, "construct.") {
		return
	}
	surfaces := projection.ProjectEvent(projection.Event{
		Type:   ev.Type,
		Phase:  ev.Phase,
		Seq:    ev.Seq,
		Fields: ev.Fields,
	})
	for _, s := range surfaces {
		// EmitSurface validates before emitting; an invalid surface is
		// silently skipped rather than failing the run (pure side-channel).
		_ = transport.EmitSurface(t, intentID, conversationID, s)
	}
}
