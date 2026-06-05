// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// gideon_scheduler.go — Gideon reasoning/analysis scheduler
// (Gideon Phase 3, plan todo: scheduler).
//
// A single goroutine, launched from runDaemon ONLY when gideonMode is
// on and the configured sweep interval is > 0. On each tick it fires an
// internal monitor/analyze intent through runMessageDirect (the same
// compiler-bypass pipeline the HTTP surface uses), then persists an
// Event memory so the cycle lands in cortex chronology (surfaced by the
// panel via /memory/recent). Egress (reporting anomalies to Telegram et
// al.) is the `notify` MCP tool's job — provided through the gideon.json
// manifest by another worker — so the scheduler simply invokes the
// pipeline; it does NOT implement any MCP bridge itself.
//
// Single-flight is respected via d.busy: a tick that cannot acquire the
// cortex single-writer mutex (a human/async intent is in flight) is
// skipped, never queued. Graceful shutdown is honoured by stopping on
// the supplied context (cancelled when the daemon begins draining).

import (
	"context"
	"fmt"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
)

// gideonSweepProse is the standing internal goal fired each cycle. Verb
// is "monitor" (D7) so the planner synthesizes a read-first fleet sweep.
const gideonSweepProse = "sweep the fleet: check node_status + sync across all hosts, report anomalies"

// runGideonScheduler is the ambient reasoning loop. Blocks until ctx is
// cancelled. Safe to call in a goroutine; it owns its own ticker.
func (d *daemonState) runGideonScheduler(ctx context.Context, t *transcript) {
	interval := d.gideonSweepInterval
	if interval <= 0 {
		t.Event("gideon.scheduler.disabled", "boot", map[string]interface{}{
			"reason": "sweep interval <= 0",
		})
		return
	}
	t.Event("gideon.scheduler.start", "boot", map[string]interface{}{
		"interval": interval.String(),
	})

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	cycle := 0
	for {
		select {
		case <-ctx.Done():
			t.Event("gideon.scheduler.stop", "shutdown", map[string]interface{}{
				"cycles": cycle,
				"reason": ctx.Err().Error(),
			})
			return
		case <-ticker.C:
			cycle++
			d.runGideonSweep(ctx, cycle, t)
		}
	}
}

// runGideonSweep executes one sweep cycle. Respects single-flight: if
// d.busy is held (a human/async intent is mid-flight) the cycle is
// skipped. Otherwise it acquires the mutex, fires the internal intent
// via runMessageDirect, and persists a chronology Event before
// releasing the lock.
func (d *daemonState) runGideonSweep(ctx context.Context, cycle int, t *transcript) {
	if d.defaultSkillURI == "" {
		t.Event("gideon.sweep.skip", "walk", map[string]interface{}{
			"cycle":  cycle,
			"reason": "no default skill configured",
		})
		return
	}

	// Single-flight: never queue behind an in-flight intent — skip.
	if !d.busy.TryLock() {
		t.Event("gideon.sweep.skip", "walk", map[string]interface{}{
			"cycle":  cycle,
			"reason": "daemon busy (single-flight); skipping this tick",
		})
		return
	}
	defer d.busy.Unlock()

	intentID := newULIDLike()
	req := messageRequest{
		Prose:    gideonSweepProse,
		Verb:     "monitor",
		SkillURI: d.defaultSkillURI,
		IntentID: intentID,
	}

	// Bind the in-flight intent so a forced chain-state-loss gate during
	// the sweep can be answered via POST /intents/<id>/gates/<nid>/answer.
	d.asyncCurrentIntent.Store(intentID)
	defer d.asyncCurrentIntent.Store("")

	t.Event("gideon.sweep.start", "walk", map[string]interface{}{
		"cycle":     cycle,
		"intent_id": intentID,
	})

	res, err := runMessageDirect(ctx, d, req)

	outcome := memory.OutcomeSuccess
	summary := fmt.Sprintf("Gideon sweep #%d completed", cycle)
	switch {
	case err != nil:
		outcome = memory.OutcomeFailure
		summary = fmt.Sprintf("Gideon sweep #%d errored: %v", cycle, err)
		t.Event("gideon.sweep.error", "walk", map[string]interface{}{
			"cycle":     cycle,
			"intent_id": intentID,
			"error":     err.Error(),
		})
	case res != nil && res.Status == "failed":
		outcome = memory.OutcomeFailure
		summary = fmt.Sprintf("Gideon sweep #%d failed: %s", cycle, res.Error)
	case res != nil:
		summary = fmt.Sprintf("Gideon sweep #%d completed: %d nodes, %d events in %dms",
			cycle, res.NodeCount, res.EventCount, res.DurationMS)
	}

	d.persistSweepEvent(intentID, outcome, summary, t)

	t.Event("gideon.sweep.done", "walk", map[string]interface{}{
		"cycle":     cycle,
		"intent_id": intentID,
		"outcome":   string(outcome),
	})
}

// persistSweepEvent writes a chronology Event memory for one sweep cycle
// so the timeline (panel /memory/recent) reflects the ambient loop even
// when the sweep itself wrote no per-tool Events. Errors are logged, not
// fatal — a missed chronology row never crashes the scheduler.
func (d *daemonState) persistSweepEvent(intentID string, outcome memory.Outcome, summary string, t *transcript) {
	if d.infra == nil || d.infra.cortex == nil {
		return
	}
	head := memory.Head{
		ActorScope: d.actor.UserURI,
		Visibility: memory.VisPrivate,
		Tags:       []memory.Tag{"gideon", "sweep"},
	}
	data := memory.EventData{
		SchemaVersion: 1,
		Kind:          memory.EventObservation,
		IntentRef:     "matrix://intent/" + intentID,
		OutcomeVal:    outcome,
		Summary:       summary,
	}
	meta := cortex.WriteMeta{
		CreatedBy:  d.actor.UserURI,
		Provenance: memory.Provenance{Source: memory.SourceObserved},
	}
	uri, err := d.infra.cortex.Write(head, data, meta)
	if err != nil {
		t.Event("gideon.sweep.event.error", "walk", map[string]interface{}{
			"intent_id": intentID,
			"error":     err.Error(),
		})
		return
	}
	t.Event("gideon.sweep.event.written", "walk", map[string]interface{}{
		"intent_id": intentID,
		"uri":       string(uri),
	})
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
