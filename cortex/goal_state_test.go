// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Tests for the sess#32 ambient meta/goal_state sidecar. The replay
// invariant guard (TestGoalRuntimeState_NotInOverallRoot) is the
// load-bearing one: dropping every meta/goal_state/* key MUST NOT change
// OverallRoot, mirroring the meta/compile_cache and meta/salience_weights
// posture.

package cortex

import (
	"bytes"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/store"
)

func TestGoalRuntimeState_RoundTripCBOR(t *testing.T) {
	id := memory.NewID()
	now := time.Unix(1700000000, 0).UTC()
	src := &GoalRuntimeState{
		SchemaVersion:     goalStateSchemaVersion,
		GoalID:            id,
		NextEvalAt:        now.Add(5 * time.Minute),
		LastEvalAt:        now,
		LastEvalDecision:  "emit_intent",
		FailureStreak:     1,
		IntentsTodayCount: 3,
		SpentTodayPax:     "0.04",
		LastIntentID:      "01H000000000000000000000",
		LastResetAt:       now.Add(-2 * time.Hour),
	}
	enc, err := EncodeGoalState(src)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got GoalRuntimeState
	if err := DecodeGoalState(enc, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.GoalID != src.GoalID || got.LastEvalDecision != src.LastEvalDecision ||
		got.FailureStreak != src.FailureStreak || got.IntentsTodayCount != src.IntentsTodayCount ||
		got.SpentTodayPax != src.SpentTodayPax || got.LastIntentID != src.LastIntentID {
		t.Fatalf("scalar mismatch:\n want=%+v\n got =%+v", *src, got)
	}
	if !got.NextEvalAt.Equal(src.NextEvalAt) || !got.LastEvalAt.Equal(src.LastEvalAt) ||
		!got.LastResetAt.Equal(src.LastResetAt) {
		t.Fatalf("time mismatch:\n want=%+v\n got =%+v", *src, got)
	}
	enc2, err := EncodeGoalState(&got)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("non-canonical re-encode:\n a=%x\n b=%x", enc, enc2)
	}
}

func TestGoalRuntimeState_DailyResetSemantics(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	st := &GoalRuntimeState{
		IntentsTodayCount: 7,
		SpentTodayPax:     "0.42",
		LastResetAt:       now.Add(-25 * time.Hour),
	}
	if !st.MaybeResetDaily(now) {
		t.Fatalf("expected reset after 25h")
	}
	if st.IntentsTodayCount != 0 || st.SpentTodayPax != "0" || !st.LastResetAt.Equal(now) {
		t.Fatalf("post-reset: %+v", st)
	}
	if st.MaybeResetDaily(now) {
		t.Fatalf("idempotent within window")
	}
	if st.MaybeResetDaily(now.Add(23 * time.Hour)) {
		t.Fatalf("must not reset before 24h elapse")
	}
	if !st.MaybeResetDaily(now.Add(24 * time.Hour)) {
		t.Fatalf("must reset at exactly 24h")
	}
}

// TestGoalRuntimeState_ReadWriteList exercises the cortex API end-to-end
// against a real store.
func TestGoalRuntimeState_ReadWriteList(t *testing.T) {
	c := openCortex(t)
	id1, id2 := memory.NewID(), memory.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	st1 := &GoalRuntimeState{
		GoalID: id1, NextEvalAt: now.Add(time.Minute), LastResetAt: now,
		IntentsTodayCount: 2, LastEvalDecision: "emit_intent",
	}
	st2 := &GoalRuntimeState{
		GoalID: id2, NextEvalAt: now.Add(2 * time.Minute), LastResetAt: now,
		FailureStreak: 1, LastEvalDecision: "stuck",
	}

	if _, ok, err := c.ReadGoalState(id1); err != nil || ok {
		t.Fatalf("expected absent state, got ok=%v err=%v", ok, err)
	}
	if err := c.WriteGoalState(st1); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if err := c.WriteGoalState(st2); err != nil {
		t.Fatalf("write2: %v", err)
	}

	got1, ok, err := c.ReadGoalState(id1)
	if err != nil || !ok {
		t.Fatalf("read1: ok=%v err=%v", ok, err)
	}
	if got1.LastEvalDecision != "emit_intent" || got1.IntentsTodayCount != 2 {
		t.Fatalf("got1: %+v", got1)
	}

	all, err := c.ListGoalStates()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list len = %d", len(all))
	}
	// keys.MetaGoalStateKey orders by goal id bytes; verify both present
	found := map[string]bool{}
	for _, st := range all {
		found[st.GoalID.String()] = true
	}
	if !found[id1.String()] || !found[id2.String()] {
		t.Fatalf("missing rows: %v", found)
	}

	if err := c.DeleteGoalState(id1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := c.ReadGoalState(id1); ok {
		t.Fatalf("post-delete still present")
	}
	all, _ = c.ListGoalStates()
	if len(all) != 1 || all[0].GoalID != id2 {
		t.Fatalf("post-delete list: %+v", all)
	}
}

// TestGoalRuntimeState_NotInOverallRoot is the §13.4 invariant guard.
// Writing goal-state rows must NOT shift OverallRoot; dropping every
// goal_state row must also NOT shift OverallRoot.
func TestGoalRuntimeState_NotInOverallRoot(t *testing.T) {
	c := openCortex(t)
	root0, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("root0: %v", err)
	}

	for i := 0; i < 5; i++ {
		st := &GoalRuntimeState{
			GoalID:            memory.NewID(),
			NextEvalAt:        time.Unix(int64(1700000000+i), 0).UTC(),
			LastResetAt:       time.Unix(int64(1700000000+i), 0).UTC(),
			IntentsTodayCount: i,
		}
		if err := c.WriteGoalState(st); err != nil {
			t.Fatalf("write[%d]: %v", i, err)
		}
	}

	root1, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("root1: %v", err)
	}
	if root0 != root1 {
		t.Fatalf("OverallRoot shifted after goal_state writes:\n before=%x\n after =%x", root0, root1)
	}

	// Drop every meta/goal_state/* row.
	rows, err := c.ListGoalStates()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, st := range rows {
		if err := c.DeleteGoalState(st.GoalID); err != nil {
			t.Fatalf("delete %s: %v", st.GoalID, err)
		}
	}
	root2, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("root2: %v", err)
	}
	if root0 != root2 {
		t.Fatalf("OverallRoot shifted after goal_state drop:\n before=%x\n after =%x", root0, root2)
	}
}

// TestGoalRuntimeState_StaleSchemaTreatedAsCold tests forward-incompat
// safety: a row written under a different schema version reads back as
// "absent" instead of crashing the scheduler.
func TestGoalRuntimeState_StaleSchemaTreatedAsCold(t *testing.T) {
	c := openCortex(t)
	id := memory.NewID()

	// Inject a row with a future schema version that the current code
	// can't decode — simulates a downgrade after a forward-incompatible
	// upgrade rolled out and was later reverted.
	stale := &GoalRuntimeState{
		SchemaVersion: 99,
		GoalID:        id,
		NextEvalAt:    time.Unix(1700000000, 0).UTC(),
	}
	enc, err := goalStateEnc.Marshal(stale)
	if err != nil {
		t.Fatalf("encode stale: %v", err)
	}
	if err := c.s.SetMeta(keys.MetaGoalStateKey(toKeysULID(id)), enc); err != nil {
		t.Fatalf("setmeta: %v", err)
	}
	// Verify the row is physically present in pebble.
	_, ok, err := c.s.Get(keys.MetaGoalStateKey(toKeysULID(id)))
	if err != nil || !ok {
		t.Fatalf("inject precondition: ok=%v err=%v", ok, err)
	}
	// API treats stale-schema as absent.
	got, found, err := c.ReadGoalState(id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if found || got != nil {
		t.Fatalf("expected cold-start, got found=%v st=%+v", found, got)
	}
	// And ListGoalStates skips it.
	all, err := c.ListGoalStates()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(all))
	}
}

// TestMetaGoalStateKey_RoundTrip catches refactor accidents in the key
// helper.
func TestMetaGoalStateKey_RoundTrip(t *testing.T) {
	id := memory.NewID()
	k := keys.MetaGoalStateKey(toKeysULID(id))
	if !bytes.HasPrefix(k, keys.PrefixMetaGoalState) {
		t.Fatalf("key %q lacks prefix %q", k, keys.PrefixMetaGoalState)
	}
	got, err := keys.ParseMetaGoalStateKey(k)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != toKeysULID(id) {
		t.Fatalf("parse mismatch: %x vs %x", got, toKeysULID(id))
	}
}

// keep imports alive in case test-only utilities are added later
var _ = pebble.Sync
var _ = store.Options{}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
