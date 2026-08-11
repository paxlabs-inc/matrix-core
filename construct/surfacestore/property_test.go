// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package surfacestore

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

// Property 1 (durability half): rehydration source fidelity.
//
// Validates: Requirements 1.1, 16.3.
//
// This is the server-side foundation of the "never vanishing" durability
// property. It asserts that for ANY generated sequence of surface frames
// recorded for a conversation, Store.Load returns them:
//   - oldest-first, with record order preserved and NO reordering, AND
//   - capped to the newest `retain` frames (when the recorded sequence exceeds
//     `retain`, the oldest are dropped and only the newest `retain` remain,
//     still in order).
//
// The cap is exact for consumers (Load), even though the on-disk file is only
// trimmed amortized at 2×retain (handle/rollLocked). We use a small `retain`
// with openWithCap so both the below-cap and well-above-cap (rollup-triggering)
// regimes are exercised cheaply by testing/quick's generated sizes.
//
// Real Store, real filesystem (t.TempDir()): no stub/mock/fake. Flush() makes
// the async writer deterministic before Load.

// propFrameSeq is a testing/quick-generated arbitrary sequence of surface
// frames. It implements quick.Generator so testing/quick can synthesize
// sequences of varying length, including lengths well above the (small) retain
// cap used by the property.
type propFrameSeq []Frame

// Generate produces an arbitrary frame sequence. Frame content is constrained
// to values that survive a JSON append→read round-trip cleanly (strings + small
// ints), so the only thing under test is ordering + the retained-frame cap, not
// JSON numeric coercion. The size testing/quick passes (default 50) yields
// sequences both below and well above the small retain cap, exercising the
// amortized rollup path (>2×retain) as well as the simple append path.
func (propFrameSeq) Generate(r *rand.Rand, size int) reflect.Value {
	n := r.Intn(size + 1) // 0..size
	frames := make(propFrameSeq, n)
	for i := range frames {
		frames[i] = propGenFrame(r, i)
	}
	return reflect.ValueOf(frames)
}

// propGenFrame builds one distinguishable frame. seq is the record position so
// every frame in a sequence is uniquely identifiable, letting the equality
// check detect any reordering.
func propGenFrame(r *rand.Rand, pos int) Frame {
	typ := "construct.surface"
	if r.Intn(2) == 0 {
		typ = "construct.surface.patch"
	}
	return Frame{
		Seq:   pos,
		Ts:    fmt.Sprintf("2026-01-01T00:00:%02dZ", pos%60),
		Phase: "construct",
		Type:  typ,
		Fields: map[string]interface{}{
			"id":   fmt.Sprintf("surface-%d", pos),
			"kind": propKinds[r.Intn(len(propKinds))],
			"note": fmt.Sprintf("payload-%d-%d", pos, r.Intn(1000)),
		},
	}
}

var propKinds = []string{
	"narration", "metric", "entity", "structure",
	"stream", "timeline", "canvas", "ask",
}

// propMarshalFrame canonicalizes a frame to JSON for comparison. encoding/json
// sorts map keys, so this neutralizes map iteration order; comparing the
// persisted (round-tripped) frame against the in-memory original on this basis
// proves payload fidelity in addition to ordering.
func propMarshalFrame(t *testing.T, fr Frame) string {
	b, err := json.Marshal(fr)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return string(b)
}

func TestLoadOrderingAndCapProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 500}

	prop := func(fs propFrameSeq, capSeed uint8) bool {
		// Small retain (1..8) so the cap is exercised cheaply and the rollup
		// path (>2×retain) is reached by generated sequences up to size 50.
		retain := int(capSeed%8) + 1

		store := openWithCap(t.TempDir(), retain)
		defer store.Close()

		const conv = "conv-prop-1"
		for _, fr := range fs {
			store.Record(conv, fr)
		}
		store.Flush() // make the async write deterministic before Load

		got := store.Load(conv)

		// Expected: the recorded sequence, oldest-first, capped to the newest
		// `retain` frames (oldest dropped when over cap).
		want := []Frame(fs)
		if len(want) > retain {
			want = want[len(want)-retain:]
		}

		// Cap invariant: Load never presents more than `retain` frames, even
		// though the on-disk file may briefly hold up to 2×retain.
		if len(got) > retain {
			t.Logf("cap violated: got %d frames, retain=%d", len(got), retain)
			return false
		}
		// Length must match the capped expectation exactly.
		if len(got) != len(want) {
			t.Logf("length mismatch: got %d, want %d (recorded=%d, retain=%d)",
				len(got), len(want), len(fs), retain)
			return false
		}
		// Ordering + payload fidelity: oldest-first, no reordering.
		for i := range got {
			if propMarshalFrame(t, got[i]) != propMarshalFrame(t, want[i]) {
				t.Logf("frame mismatch at index %d (retain=%d, recorded=%d):\n got=%s\nwant=%s",
					i, retain, len(fs), propMarshalFrame(t, got[i]), propMarshalFrame(t, want[i]))
				return false
			}
		}
		return true
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Fatalf("Load ordering/cap property failed: %v", err)
	}
}
