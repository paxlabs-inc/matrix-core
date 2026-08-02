// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/o1"
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

func TestPostureRespawnLimit(t *testing.T) {
	cfg := config.Default()
	cfg.TaskMaxRespawns = 7
	if got := postureRespawnLimit(cfg, "hello there"); got != 7 {
		t.Fatalf("flag-off limit = %d, want 7", got)
	}
	cfg.InteractionPosture = true
	if got := postureRespawnLimit(cfg, "hello there"); got != 0 {
		t.Fatalf("conversation limit = %d, want 0", got)
	}
	if got := postureRespawnLimit(cfg, "research the current status"); got != 0 {
		t.Fatalf("exploration limit = %d, want 0", got)
	}
	if got := postureRespawnLimit(cfg, "implement and deploy the fix"); got != 7 {
		t.Fatalf("execution limit = %d, want 7", got)
	}
	if got := superviseDecision(false, agent.ErrIncomplete, delegate.ClassNone, nil, 1, postureRespawnLimit(cfg, "implement the fix")); got != actRespawn {
		t.Fatalf("wedged execution must retain supervisor respawn, got %v", got)
	}
}

func TestSuperviseFromO1UsesTypedDecision(t *testing.T) {
	internal := o1.SupervisorDecision{Action: o1.SupContinue}
	if got := superviseFromO1(actStop, internal); got != actStop {
		t.Fatalf("typed default continue must not erase a terminal attempt classification, got %v", got)
	}
	if got := superviseFromO1(actCeiling, internal); got != actCeiling {
		t.Fatalf("typed default continue must not erase an exhausted execution ceiling, got %v", got)
	}
	if got := superviseFromO1(actRespawn, internal); got != actRespawn {
		t.Fatalf("typed continue should preserve a valid respawn, got %v", got)
	}
	stop := o1.SupervisorDecision{Action: o1.SupStop}
	if got := superviseFromO1(actRespawn, stop); got != actStop {
		t.Fatalf("typed stop should stop, got %v", got)
	}
	if got := superviseFromO1(actInterrupted, internal); got != actInterrupted {
		t.Fatalf("user interruption must remain sovereign, got %v", got)
	}
}

// TestResumePrime: the catch-up prime always carries the verbatim objective and
// the build-on-existing-work instruction; the decomposition nudge appears only
// after several stuck attempts.
func TestResumePrime(t *testing.T) {
	obj := "write a solid academic paper with citations on the thesis"
	early := resumePrime(obj, 1, nil)
	if !strings.Contains(early, obj) {
		t.Error("resume prime must carry the verbatim objective")
	}
	if !strings.Contains(strings.ToLower(early), "build on") {
		t.Error("resume prime must tell the agent to build on existing work")
	}
	if strings.Contains(strings.ToLower(early), "spawn_subagents") {
		t.Error("the decomposition nudge must NOT appear on an early attempt")
	}
	late := resumePrime(obj, 3, nil)
	if !strings.Contains(strings.ToLower(late), "spawn_subagents") {
		t.Error("the decomposition nudge must appear after several stuck attempts")
	}
}

// TestResumePrimeDeathDigest: when the predecessor died with an ErrIncomplete
// where-it-got-stuck digest, the successor's prime folds it in (the immediate
// death-journal read path) with a do-NOT-repeat framing, and strips the raw
// ErrIncomplete sentinel so only the digest reaches the model. A nil predecessor
// (first attempt) folds in nothing.
func TestResumePrimeDeathDigest(t *testing.T) {
	obj := "compile and ship the migration"
	const stuck = "repeating the same step without progress. Where it got stuck: re-rendering a value already in hand"
	prev := fmt.Errorf("%w: %s", agent.ErrIncomplete, stuck)

	with := resumePrime(obj, 2, prev)
	if !strings.Contains(with, stuck) {
		t.Errorf("prime must fold in the predecessor's death digest; got:\n%s", with)
	}
	if !strings.Contains(strings.ToLower(with), "do not repeat") {
		t.Error("prime must tell the successor not to repeat the losing move")
	}
	if strings.Contains(with, agent.ErrIncomplete.Error()) {
		t.Errorf("prime must strip the raw ErrIncomplete sentinel; got:\n%s", with)
	}

	if got := resumePrime(obj, 2, nil); strings.Contains(strings.ToLower(got), "do not repeat") {
		t.Error("a nil predecessor must not add a death-digest section")
	}
}

// TestDeathDigest: the digest strips the ErrIncomplete sentinel prefix, returns
// the remaining where-it-got-stuck text, and is empty for a nil error.
func TestDeathDigest(t *testing.T) {
	if got := deathDigest(nil); got != "" {
		t.Errorf("deathDigest(nil) = %q, want empty", got)
	}
	prev := fmt.Errorf("%w: reached the step budget without finishing. Progress so far: wrote 2 of 5 files", agent.ErrIncomplete)
	got := deathDigest(prev)
	if strings.Contains(got, "turn incomplete") {
		t.Errorf("deathDigest must strip the sentinel; got %q", got)
	}
	if !strings.Contains(got, "wrote 2 of 5 files") {
		t.Errorf("deathDigest must keep the progress digest; got %q", got)
	}
}

// TestDeathPathsConsistent proves the two death-journal read paths describe the
// SAME death (self-model task 3.1, req.4.3): the immediate successor-prime and
// the durable cortex record are both rendered from the where-it-got-stuck digest
// of the one attempt error, over the same objective — so a successor's prime and
// the durable journal entry can never disagree about how the predecessor died.
func TestDeathPathsConsistent(t *testing.T) {
	obj := "compile and ship the migration to the new schema"
	const stuck = "kept re-running the same status check without editing a file. Progress: 2 of 5 tables migrated"
	err := fmt.Errorf("%w: %s", agent.ErrIncomplete, stuck)
	const state = " [loop-state: reason=no_progress_stall faculty=conversation last_tool=web_search repeats=4 distinct_tools=1 context_fill=12% steps=6]"

	entry := newDeathEntry(obj, 2, err, delegate.ClassNone, state)
	durable := entry.durableSummary()
	prime := resumePrime(obj, 3, err)

	// The single shared digest is the where-it-got-stuck text, sentinel-stripped.
	digest := deathDigest(err)
	if digest == "" || !strings.Contains(digest, "2 of 5 tables") {
		t.Fatalf("precondition: shared digest not derived; got %q", digest)
	}
	// Both paths carry that SAME digest.
	if !strings.Contains(durable, digest) {
		t.Errorf("durable record must carry the shared digest:\n%s", durable)
	}
	if !strings.Contains(prime, digest) {
		t.Errorf("immediate prime must carry the shared digest:\n%s", prime)
	}
	// Both paths carry the SAME objective (durable clips to 160; this objective
	// is shorter, so it appears verbatim in both).
	if !strings.Contains(durable, obj) || !strings.Contains(prime, obj) {
		t.Error("both paths must describe the same objective")
	}
	// The durable record additionally carries the attempt, class, and the rich
	// loop-state suffix (its richer surface), and never re-embeds the raw
	// ErrIncomplete sentinel.
	if !strings.Contains(durable, "attempt 2") || !strings.Contains(durable, "class=none") {
		t.Errorf("durable record must carry attempt and class:\n%s", durable)
	}
	if !strings.Contains(durable, "reason=no_progress_stall") {
		t.Errorf("durable record must carry the rich loop-state suffix:\n%s", durable)
	}
	if strings.Contains(durable, agent.ErrIncomplete.Error()) {
		t.Errorf("durable record must not re-embed the raw ErrIncomplete sentinel:\n%s", durable)
	}
}

// TestSuperviseBackoffCancelled: backoff returns false immediately when the
// task context is already cancelled (so a stopped task never sleeps) — for both
// the normal and the (much longer) rate-limited path.
func TestSuperviseBackoffCancelled(t *testing.T) {
	for _, rateLimited := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if superviseBackoff(ctx, 3, rateLimited) {
			t.Errorf("rateLimited=%v: backoff on a cancelled context must return false", rateLimited)
		}
		if time.Since(start) > 200*time.Millisecond {
			t.Errorf("rateLimited=%v: backoff on a cancelled context must return promptly, not sleep", rateLimited)
		}
	}
}

// TestSuperviseBackoffWaits: with a live context, backoff actually waits and
// returns true.
func TestSuperviseBackoffWaits(t *testing.T) {
	start := time.Now()
	if !superviseBackoff(context.Background(), 1, false) {
		t.Error("backoff on a live context must return true")
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Error("attempt-1 backoff should wait at least the base interval")
	}
}

// TestSuperviseBackoffRateLimitedIsLonger: a 429 backoff waits far longer than
// the normal path at the same attempt, so an overloaded upstream is not
// amplified into a request storm. Cancellation still short-circuits it (proven
// above), so this asserts the floor without sleeping the full interval.
func TestSuperviseBackoffRateLimitedIsLonger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	// attempt 1, rate-limited: base is 15s (>> the 8s normal cap), so the
	// 2s ctx deadline fires first and returns false — the point is that it did
	// NOT return within the normal-path window.
	if superviseBackoff(ctx, 1, true) {
		t.Error("a 15s rate-limited backoff must not complete within the 2s test deadline")
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Errorf("rate-limited backoff returned after %v; expected it to wait past the normal-path interval", elapsed)
	}
}
