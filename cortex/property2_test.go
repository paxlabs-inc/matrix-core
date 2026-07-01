// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 8.2 — Property 2: the ladder is idempotent,
// bounded, and self-healing (validates req.4.4, 5.1, 5.2, 12.2).
//
// hour->day->epoch cascade + idempotent re-run + the event-count floor are
// already proven per-piece by cascade_test.go / rollup_test.go. The ONE
// scenario none of those exercise: an already-BUILT coarser rollup that is
// then DELETED and read again — lazy read-repair (repair.go, task 3.1) must
// reconstruct it byte-identically, over a real seeded journal, no
// stub/mock/fake (req.12.7).
package cortex_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"matrix/cortex"
	"matrix/cortex/keys"
)

// TestProperty2_DeletedCoarseRollupRepairsIdentically proves req.5.2 over a
// record that EXISTED (built by a real Cascade) and was then deleted from
// the store: reading it again via RepairRollup must trigger an idempotent
// rebuild yielding bytes byte-identical to the original eager build.
func TestProperty2_DeletedCoarseRollupRepairsIdentically(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	clk.t = baseHour.Add(11 * 24 * time.Hour)
	if err := c.Cascade(cortex.TierDay, now); err != nil {
		t.Fatalf("Cascade(day): %v", err)
	}

	dayWindow := cortex.DayWindow(baseHour.UnixNano())
	before, err := c.LoadRollup(cortex.TierDay, dayWindow.Start)
	if err != nil {
		t.Fatalf("LoadRollup(day) before delete: %v", err)
	}
	beforeBytes, err := cortex.EncodeRollupRecord(before)
	if err != nil {
		t.Fatalf("encode before: %v", err)
	}

	// Delete the already-built day record directly (simulating loss /
	// corruption of the derived record while the journal — the source of
	// truth — is untouched).
	dayKey := keys.RollupKey(uint8(cortex.TierDay), uint64(dayWindow.Start))
	if err := c.Store().DB().Delete(dayKey, pebble.Sync); err != nil {
		t.Fatalf("delete day record: %v", err)
	}
	if _, err := c.LoadRollup(cortex.TierDay, dayWindow.Start); err == nil {
		t.Fatal("LoadRollup after direct delete: want error, got nil")
	}

	// Reading it again triggers lazy read-repair (repair.go), which must
	// reconstruct the SAME bytes from the still-intact journal + hour
	// rollup children.
	repaired, err := c.RepairRollup(dayWindow, now)
	if err != nil {
		t.Fatalf("RepairRollup after delete: %v", err)
	}
	repairedBytes, err := cortex.EncodeRollupRecord(repaired)
	if err != nil {
		t.Fatalf("encode repaired: %v", err)
	}
	if !bytes.Equal(beforeBytes, repairedBytes) {
		t.Fatalf("repaired record not byte-identical to pre-delete record:\n before %x\n after  %x", beforeBytes, repairedBytes)
	}

	// The repair persisted the record — a follow-up LoadRollup finds it
	// without another repair.
	if _, err := c.LoadRollup(cortex.TierDay, dayWindow.Start); err != nil {
		t.Fatalf("LoadRollup after repair: %v", err)
	}
}

// TestProperty2_LadderIsIdempotentBoundedAndSelfHealing composes the full
// property statement over ONE real seeded journal: hour->day->epoch cascade
// builds real records (req.4.4), a second SweepNow over unchanged state is
// a no-op (req.5.1's eager sweep, req.4.1 idempotence — JournalCount
// unchanged), an empty/under-floor window produces no rollup (req.4.1), and
// a deleted coarse record self-heals via lazy repair (req.5.2) — the same
// journal, the same instance, in one flow.
func TestProperty2_LadderIsIdempotentBoundedAndSelfHealing(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	sweepNow := baseHour.Add(10 * 24 * time.Hour)
	clk.t = sweepNow
	if err := c.SweepNow(sweepNow.UnixNano()); err != nil {
		t.Fatalf("SweepNow #1: %v", err)
	}

	epochs, err := c.Rollups(cortex.TierEpoch, cascadeSince, sweepNow.UnixNano())
	if err != nil || len(epochs) == 0 {
		t.Fatalf("Rollups(epoch) after SweepNow: len=%d err=%v", len(epochs), err)
	}

	// Idempotent re-run: SweepNow again over unchanged state appends no
	// new journal entries (req.4.1/5.1).
	before := c.Store().JournalCount()
	clk.t = sweepNow.Add(365 * 24 * time.Hour)
	if err := c.SweepNow(sweepNow.UnixNano()); err != nil {
		t.Fatalf("SweepNow #2: %v", err)
	}
	after := c.Store().JournalCount()
	if after != before {
		t.Fatalf("JournalCount changed across idempotent SweepNow: before=%d after=%d", before, after)
	}

	// Event-count floor: a window with zero entries produces no rollup.
	emptyWindow := cortex.HourWindow(sweepNow.Add(365 * 24 * time.Hour).UnixNano())
	uri, err := c.BuildRollup(emptyWindow)
	if err != nil {
		t.Fatalf("BuildRollup(empty): %v", err)
	}
	if uri != "" {
		t.Fatalf("BuildRollup(empty) uri = %q, want empty (below floor)", uri)
	}

	// Self-healing: delete an epoch record built above and repair it.
	epochWindow := epochs[0].Window
	epochKey := keys.RollupKey(uint8(cortex.TierEpoch), uint64(epochWindow.Start))
	if err := c.Store().DB().Delete(epochKey, pebble.Sync); err != nil {
		t.Fatalf("delete epoch record: %v", err)
	}
	repaired, err := c.RepairRollup(epochWindow, sweepNow.Add(365*24*time.Hour).UnixNano())
	if err != nil {
		t.Fatalf("RepairRollup(epoch) after delete: %v", err)
	}
	wantBytes, err := cortex.EncodeRollupRecord(&epochs[0])
	if err != nil {
		t.Fatalf("encode want: %v", err)
	}
	gotBytes, err := cortex.EncodeRollupRecord(repaired)
	if err != nil {
		t.Fatalf("encode got: %v", err)
	}
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Fatalf("repaired epoch record not byte-identical:\n want %x\n got  %x", wantBytes, gotBytes)
	}
}
