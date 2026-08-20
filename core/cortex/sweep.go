// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 3.1: the Chronos window-close CASCADE SWEEP entry
// point (req.5.1, req.5.3).
//
// SweepNow is the single, bounded-work call a Chronos-scheduled trigger (the
// memorysweep alarm convention, packages/chronos/internal/memorysweep) is meant to
// invoke on every fire: it rolls every CLOSED window up through the coarsest
// tier via Cascade (cascade.go), doing bounded work per call — Cascade only
// builds windows that are closed AND missing/stale, so a call over unchanged
// store state appends ZERO new journal entries (idempotent, safe on any
// cadence).
//
// Chronos itself never touches cortex: its alarm/dispatch machinery only
// carries a wake_message string to whatever process holds this actor's live
// *Cortex (chronos/internal/dispatch.Worker.fire -> wake.Waker.Wake). That
// receiving process is the ONLY thing that calls SweepNow, and SweepNow is
// the ONLY thing that writes — entirely inside cortex, on the derived lane
// (cascade.go's roll/ records + KindRollup journal entries, NO
// memories/edges SMT write). Chronos therefore gains no new signing /
// cortex-write / plan-walk capability beyond delivering the trigger
// (req.5.3): it has no cortex handle, no key material, and never runs a
// plan/walk.
package cortex

// SweepNow runs the window-close cascade up through the coarsest tier
// (TierEpoch) as of now. It is the whole "eager sweep" (req.5.1): idempotent
// and safe to call on any Chronos cadence — a call that finds no newly
// closed window does bounded, cheap work (a journal scan for new hour
// windows plus a small rollup-tier scan) and writes nothing new.
func (c *Cortex) SweepNow(now int64) error {
	return c.Cascade(TierEpoch, now)
}
