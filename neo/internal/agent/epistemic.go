// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// epistemic.go holds the epistemic-core loop wiring shared by the mechanisms
// (premise ledger, prediction-carrying dispatch, task graph). The tail
// discipline (req.3.1): turn-varying epistemic state renders AFTER the
// activation/memory block in FIXED section order — premise ledger, then task
// graph — so residency is structured law, not heap.
package agent

// epistemicTail renders the turn-varying epistemic run state in its fixed
// tail order: the premise ledger slot, then the task graph slot. A mechanism
// with no state renders nothing (a clean turn is byte-identical to the
// pre-epistemic tail).
func (a *Agent) epistemicTail() string {
	return a.turn.premiseTail + a.turn.graphTail
}
