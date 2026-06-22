// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"errors"
	"testing"

	"matrix/neo/internal/config"
)

// TestGateStrict pins the "gate everything" placement rule: a money/chain
// (state-touching) turn is ALWAYS strict; under GateAllWork any substantive
// tool-work turn is strict too; a pure conversational turn (no tools) stays on
// the light path. With GateAllWork off, only state-touching turns are strict
// (the legacy placement).
func TestGateStrict(t *testing.T) {
	cases := []struct {
		name         string
		gateAll      bool
		stateTouched bool
		workTouched  bool
		want         bool
	}{
		{"pure chat, gate-all on → light", true, false, false, false},
		{"substantive work, gate-all on → strict", true, false, true, true},
		{"money turn, gate-all on → strict", true, true, true, true},
		{"substantive work, gate-all off → light", false, false, true, false},
		{"money turn, gate-all off → strict", false, true, false, true},
		{"pure chat, gate-all off → light", false, false, false, false},
	}
	for _, c := range cases {
		cfg := config.Default()
		cfg.GateAllWork = c.gateAll
		a := New(Options{Config: cfg})
		if got := a.gateStrict(c.stateTouched, c.workTouched); got != c.want {
			t.Errorf("%s: gateStrict(%v,%v) = %v, want %v", c.name, c.stateTouched, c.workTouched, got, c.want)
		}
	}
}

// TestErrIncompleteIsWrapped confirms the stall/step-budget sentinel is
// detectable through fmt.Errorf wrapping (errors.Is), the contract the
// supervisor relies on to tell "not done" apart from a genuine completion.
func TestErrIncompleteIsWrapped(t *testing.T) {
	wrapped := errors.New("x")
	if errors.Is(wrapped, ErrIncomplete) {
		t.Fatal("an unrelated error must not match ErrIncomplete")
	}
	if !errors.Is(ErrIncomplete, ErrIncomplete) {
		t.Fatal("ErrIncomplete must match itself")
	}
}
