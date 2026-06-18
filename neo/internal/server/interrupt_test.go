// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"errors"
	"testing"
)

// interrupt must mark the run stopped AND cancel its context so the agent loop
// unwinds — the barge-in / explicit-stop primitive.
func TestInterruptStopsAndCancels(t *testing.T) {
	s := &session{}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{id: "neo_x", sess: s, cancel: cancel}

	s.interrupt(r)

	if !r.stopped.Load() {
		t.Error("interrupt must set the stopped flag")
	}
	select {
	case <-ctx.Done():
		// cancelled as expected
	default:
		t.Error("interrupt must cancel the run's context")
	}
}

// interrupt is a safe no-op on a nil run and on a run with no cancel registered
// yet (dispatched but not yet driving).
func TestInterruptNilSafe(t *testing.T) {
	s := &session{}
	s.interrupt(nil) // must not panic

	r := &run{id: "neo_y", sess: s} // cancel still nil
	s.interrupt(r)
	if !r.stopped.Load() {
		t.Error("a run with no cancel must still be marked stopped")
	}
}

// interruptActive targets the latest dispatched run (LIFO barge-in) and is a
// no-op (empty id) when nothing is in flight.
func TestInterruptActiveTargetsLatest(t *testing.T) {
	s := &session{}
	if got := s.interruptActive(); got != "" {
		t.Errorf("interruptActive with nothing active must return \"\", got %q", got)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &run{id: "neo_live", sess: s, cancel: cancel}
	s.setActive(r)

	if got := s.interruptActive(); got != "neo_live" {
		t.Errorf("interruptActive should target the active run, got %q", got)
	}
	if !r.stopped.Load() {
		t.Error("the active run must be stopped")
	}

	// clearActive only nils when it still points at r.
	other := &run{id: "neo_other", sess: s}
	s.setActive(other)
	s.clearActive(r) // r is no longer active → must NOT clear `other`
	if got := s.interruptActive(); got != "neo_other" {
		t.Errorf("clearActive(stale) must not drop the successor; got %q", got)
	}
}

// shouldRetrySubagent retries ONLY a hard failure that produced no output while
// the parent is still alive and attempts remain.
func TestShouldRetrySubagent(t *testing.T) {
	hard := errors.New("model call failed")
	cases := []struct {
		name         string
		attempt, max int
		err          error
		haveText     bool
		ctxErr       error
		want         bool
	}{
		{"hard failure, attempts remain", 1, 2, hard, false, nil, true},
		{"clean success", 1, 2, nil, false, nil, false},
		{"failure but has partial output", 1, 2, hard, true, nil, false},
		{"parent cancelled", 1, 2, hard, false, context.Canceled, false},
		{"no attempts remain", 2, 2, hard, false, nil, false},
	}
	for _, c := range cases {
		if got := shouldRetrySubagent(c.attempt, c.max, c.err, c.haveText, c.ctxErr); got != c.want {
			t.Errorf("%s: shouldRetrySubagent = %v, want %v", c.name, got, c.want)
		}
	}
}

// parseStopPath matches only /intents/{id}/stop.
func TestParseStopPath(t *testing.T) {
	id, ok := parseStopPath("/intents/neo_abc/stop")
	if !ok || id != "neo_abc" {
		t.Errorf("expected (neo_abc,true), got (%q,%v)", id, ok)
	}
	for _, p := range []string{"/intents/neo_abc/gates/n1/answer", "/intents/neo_abc", "/foo/neo_abc/stop", "/intents/neo_abc/stop/extra"} {
		if _, ok := parseStopPath(p); ok {
			t.Errorf("parseStopPath(%q) should not match", p)
		}
	}
}
