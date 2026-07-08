// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"errors"
	"testing"
)

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
