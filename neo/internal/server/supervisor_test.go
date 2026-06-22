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
)

// TestSuperviseDecision pins the persistent-supervisor policy: any non-clean
// exit keeps going (respawn) until the ceiling — the only terminal-without-
// completion is the ceiling — while a user stop and a genuine completion are
// honored immediately. This is the heart of the Task Durability Rule.
func TestSuperviseDecision(t *testing.T) {
	hardErr := errors.New("neo: model call failed")
	incomplete := agent.ErrIncomplete
	wall := context.DeadlineExceeded

	cases := []struct {
		name             string
		stopped          bool
		attemptErr       error
		taskCtxErr       error
		attempt, maxResp int
		want             superviseAction
	}{
		{"clean completion", false, nil, nil, 1, 50, actDone},
		{"user stop wins over error", true, hardErr, nil, 1, 50, actInterrupted},
		{"user stop wins over success", true, nil, nil, 3, 50, actInterrupted},
		{"model error, budget left → respawn", false, hardErr, nil, 1, 50, actRespawn},
		{"incomplete (stall/budget) → respawn", false, incomplete, nil, 2, 50, actRespawn},
		{"wall-clock blown → ceiling", false, hardErr, wall, 5, 50, actCeiling},
		{"respawn budget exhausted → ceiling", false, hardErr, nil, 1, 0, actCeiling},
		{"last allowed attempt still respawns", false, hardErr, nil, 50, 50, actRespawn},
		{"one past budget → ceiling", false, hardErr, nil, 51, 50, actCeiling},
	}
	for _, c := range cases {
		got := superviseDecision(c.stopped, c.attemptErr, c.taskCtxErr, c.attempt, c.maxResp)
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
