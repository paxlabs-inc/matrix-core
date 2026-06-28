// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/delegate"
)

// TestSuperviseDecision pins the persistent-supervisor policy: any non-clean
// exit keeps going (respawn) until the ceiling — EXCEPT a DETERMINISTIC blocker,
// which stops-and-asks without respawning or consuming the budget (a fresh
// agent would hit the same wall). A user stop and a genuine completion are
// honored immediately. This is the heart of the Task Durability Rule plus the
// NE-5 deterministic-stop fix.
func TestSuperviseDecision(t *testing.T) {
	hardErr := errors.New("neo: model call failed")
	incomplete := agent.ErrIncomplete
	wall := context.DeadlineExceeded

	cases := []struct {
		name             string
		stopped          bool
		attemptErr       error
		failClass        delegate.FailureClass
		taskCtxErr       error
		attempt, maxResp int
		want             superviseAction
	}{
		{"clean completion", false, nil, delegate.ClassNone, nil, 1, 50, actDone},
		{"clean completion ignores a mid-turn deterministic class", false, nil, delegate.ClassDeterministic, nil, 1, 50, actDone},
		{"user stop wins over error", true, hardErr, delegate.ClassNone, nil, 1, 50, actInterrupted},
		{"user stop wins over success", true, nil, delegate.ClassNone, nil, 3, 50, actInterrupted},
		{"user stop wins over deterministic", true, hardErr, delegate.ClassDeterministic, nil, 1, 50, actInterrupted},
		{"model error, budget left → respawn", false, hardErr, delegate.ClassNone, nil, 1, 50, actRespawn},
		{"incomplete (stall/budget) → respawn", false, incomplete, delegate.ClassNone, nil, 2, 50, actRespawn},
		{"deterministic blocker → stop, no respawn", false, incomplete, delegate.ClassDeterministic, nil, 1, 50, actStop},
		{"deterministic stops even with full budget", false, hardErr, delegate.ClassDeterministic, nil, 1, 50, actStop},
		{"deterministic stops before the ceiling", false, hardErr, delegate.ClassDeterministic, wall, 5, 50, actStop},
		{"transient failure → respawn (existing path)", false, incomplete, delegate.ClassTransient, nil, 1, 50, actRespawn},
		{"conflict → respawn in P1 (attach is P2)", false, incomplete, delegate.ClassConflict, nil, 1, 50, actRespawn},
		{"pending → respawn in P1 (slot-fill is P4)", false, incomplete, delegate.ClassPending, nil, 1, 50, actRespawn},
		{"wall-clock blown → ceiling", false, hardErr, delegate.ClassNone, wall, 5, 50, actCeiling},
		{"respawn budget exhausted → ceiling", false, hardErr, delegate.ClassNone, nil, 1, 0, actCeiling},
		{"last allowed attempt still respawns", false, hardErr, delegate.ClassNone, nil, 50, 50, actRespawn},
		{"one past budget → ceiling", false, hardErr, delegate.ClassNone, nil, 51, 50, actCeiling},
	}
	for _, c := range cases {
		got := superviseDecision(c.stopped, c.attemptErr, c.failClass, c.taskCtxErr, c.attempt, c.maxResp)
		if got != c.want {
			t.Errorf("%s: superviseDecision = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestResumePrime: the catch-up prime always carries the verbatim objective and
// the build-on-existing-work instruction; the decomposition nudge appears only
// after several stuck attempts.
func TestResumePrime(t *testing.T) {
	obj := "write a solid academic paper with citations on the thesis"
	early := resumePrime(obj, 1)
	if !strings.Contains(early, obj) {
		t.Error("resume prime must carry the verbatim objective")
	}
	if !strings.Contains(strings.ToLower(early), "build on") {
		t.Error("resume prime must tell the agent to build on existing work")
	}
	if strings.Contains(strings.ToLower(early), "spawn_subagents") {
		t.Error("the decomposition nudge must NOT appear on an early attempt")
	}
	late := resumePrime(obj, 3)
	if !strings.Contains(strings.ToLower(late), "spawn_subagents") {
		t.Error("the decomposition nudge must appear after several stuck attempts")
	}
}

// TestSuperviseBackoffCancelled: backoff returns false immediately when the
// task context is already cancelled (so a stopped task never sleeps).
func TestSuperviseBackoffCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if superviseBackoff(ctx, 3) {
		t.Error("backoff on a cancelled context must return false")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Error("backoff on a cancelled context must return promptly, not sleep")
	}
}

// TestSuperviseBackoffWaits: with a live context, backoff actually waits and
// returns true.
func TestSuperviseBackoffWaits(t *testing.T) {
	start := time.Now()
	if !superviseBackoff(context.Background(), 1) {
		t.Error("backoff on a live context must return true")
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Error("attempt-1 backoff should wait at least the base interval")
	}
}
