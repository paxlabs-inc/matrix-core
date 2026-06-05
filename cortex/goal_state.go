// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Goal runtime-state sidecar for the sess#32 ambient scheduler.
//
// Spec: journal/plan/01-ambient-architect.md §5.2 + S32Q4. The sidecar
// posture mirrors meta/salience_weights (Phase 12) and meta/compile_cache
// (sess#31d): runtime policy state stored under meta/, NEVER part of the
// cortex OverallRoot, mutates on every scheduler tick (≈1Hz under load),
// rebuildable cold-start because the scheduler tolerates missing state by
// firing the goal at NextEvalAt = now.
//
// Atomicity: each Read/Write is one Pebble Get/Set against meta/. The
// scheduler holds these state rows in memory between ticks; persistence
// happens after each decision so a daemon crash mid-tick at worst replays
// one extra LLM call against the same goal (idempotent at the IR layer).
//
// Replay invariant: §13.4 "drop meta/goal_state, replay, root unchanged".
// Verified by TestGoalRuntimeState_NotInOverallRoot (goal_state_test.go).
package cortex

import (
	"fmt"
	"time"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
)

// goalStateSchemaVersion is bumped on any incompatible GoalRuntimeState
// CBOR-shape change; mismatched rows are treated as a cold start (zero
// values + LastResetAt=now) so a forward-incompatible upgrade never
// poisons scheduler decisions.
const goalStateSchemaVersion uint8 = 1

// GoalRuntimeState carries the per-Goal scheduler bookkeeping that mutates
// on every tick. Persisted at meta/goal_state/<goal_id:16> as canonical
// CBOR. Sidecar — never part of OverallRoot.
type GoalRuntimeState struct {
	SchemaVersion     uint8     `cbor:"0,keyasint"`
	GoalID            memory.ID `cbor:"1,keyasint"`
	NextEvalAt        time.Time `cbor:"2,keyasint"`
	LastEvalAt        time.Time `cbor:"3,keyasint,omitempty"`
	LastEvalDecision  string    `cbor:"4,keyasint,omitempty"` // "emit_intent"|"complete"|"stuck"|"noop"
	FailureStreak     int       `cbor:"5,keyasint,omitempty"`
	IntentsTodayCount int       `cbor:"6,keyasint,omitempty"`
	SpentTodayPax     string    `cbor:"7,keyasint,omitempty"`
	LastIntentID      string    `cbor:"8,keyasint,omitempty"`
	LastResetAt       time.Time `cbor:"9,keyasint,omitempty"` // daily counter reset anchor
}

// goalStateEnc / goalStateDec enforce deterministic CBOR so repeated writes
// with the same fields produce byte-identical bytes (cheap dedup at the
// pebble level + matches the established sidecar convention).
var (
	goalStateEnc cbor.EncMode
	goalStateDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("cortex: goal_state EncMode: %w", err))
	}
	goalStateEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("cortex: goal_state DecMode: %w", err))
	}
	goalStateDec = dm
}

// EncodeGoalState returns canonical CBOR for st.
func EncodeGoalState(st *GoalRuntimeState) ([]byte, error) {
	if st == nil {
		return nil, fmt.Errorf("cortex.EncodeGoalState: nil state")
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = goalStateSchemaVersion
	}
	return goalStateEnc.Marshal(st)
}

// DecodeGoalState parses canonical CBOR into out. Returns
// errGoalStateSchemaMismatch when the persisted SchemaVersion is not
// the current goalStateSchemaVersion.
func DecodeGoalState(b []byte, out *GoalRuntimeState) error {
	if out == nil {
		return fmt.Errorf("cortex.DecodeGoalState: nil out")
	}
	if err := goalStateDec.Unmarshal(b, out); err != nil {
		return err
	}
	if out.SchemaVersion != goalStateSchemaVersion {
		return errGoalStateSchemaMismatch
	}
	return nil
}

var errGoalStateSchemaMismatch = fmt.Errorf("cortex: goal_state schema version mismatch")

// ReadGoalState fetches the runtime state for goalID. Returns
// (nil, false, nil) when the row is absent OR the SchemaVersion is
// stale (forward-incompat upgrade); callers treat both as cold-start
// and the next scheduler tick re-derives.
//
// Errors are returned only on store I/O or genuine CBOR decode failure
// against a current-schema blob (corruption).
func (c *Cortex) ReadGoalState(goalID memory.ID) (*GoalRuntimeState, bool, error) {
	raw, ok, err := c.s.Get(keys.MetaGoalStateKey(toKeysULID(goalID)))
	if err != nil {
		return nil, false, fmt.Errorf("cortex.ReadGoalState: get %s: %w", goalID, err)
	}
	if !ok {
		return nil, false, nil
	}
	var st GoalRuntimeState
	if err := DecodeGoalState(raw, &st); err != nil {
		if err == errGoalStateSchemaMismatch {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cortex.ReadGoalState: decode %s: %w", goalID, err)
	}
	return &st, true, nil
}

// WriteGoalState persists st at meta/goal_state/<id>. Overwrites silently;
// scheduler ticks are the only writer per S32Q4 (single-writer cortex
// invariant). SchemaVersion is auto-set to goalStateSchemaVersion when
// caller leaves it zero.
func (c *Cortex) WriteGoalState(st *GoalRuntimeState) error {
	if st == nil {
		return fmt.Errorf("cortex.WriteGoalState: nil state")
	}
	if st.GoalID.IsZero() {
		return fmt.Errorf("cortex.WriteGoalState: zero goal id")
	}
	enc, err := EncodeGoalState(st)
	if err != nil {
		return fmt.Errorf("cortex.WriteGoalState: encode: %w", err)
	}
	return c.s.SetMeta(keys.MetaGoalStateKey(toKeysULID(st.GoalID)), enc)
}

// DeleteGoalState removes the runtime-state row for goalID. Idempotent.
// Used by the daemon when a Goal transitions to GoalAbandoned so the
// row doesn't linger in ListGoalStates output.
func (c *Cortex) DeleteGoalState(goalID memory.ID) error {
	return c.s.DeleteMeta(keys.MetaGoalStateKey(toKeysULID(goalID)))
}

// ListGoalStates returns every persisted GoalRuntimeState for this actor,
// in goal-id-byte-ascending order (matches keys.MetaGoalStateKey layout).
// Stale-schema rows are skipped silently so a stale persistence cannot
// crash callers.
func (c *Cortex) ListGoalStates() ([]*GoalRuntimeState, error) {
	out := make([]*GoalRuntimeState, 0, 16)
	err := c.s.PrefixIter(keys.PrefixMetaGoalState, func(_, value []byte) error {
		var st GoalRuntimeState
		if derr := DecodeGoalState(value, &st); derr != nil {
			if derr == errGoalStateSchemaMismatch {
				return nil
			}
			return fmt.Errorf("decode: %w", derr)
		}
		out = append(out, &st)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cortex.ListGoalStates: %w", err)
	}
	return out, nil
}

// MaybeResetDaily zeroes the per-day counters when LastResetAt is more
// than 24h before now. Returns true when a reset occurred so callers can
// log the rollover. The state row is mutated in place; persistence is the
// caller's job (typically WriteGoalState immediately after).
func (st *GoalRuntimeState) MaybeResetDaily(now time.Time) bool {
	if st == nil {
		return false
	}
	if st.LastResetAt.IsZero() || now.Sub(st.LastResetAt) >= 24*time.Hour {
		st.IntentsTodayCount = 0
		st.SpentTodayPax = "0"
		st.LastResetAt = now
		return true
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
