// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package controller

import (
	"context"

	"centra/core/cortexclient"
	"centra/agents/neo/internal/runtime/loop"
)

// ObserveMismatch implements loop.DoubtController. Only evidence that is
// actually anchored arms the voice: an execution with no verified citation is
// not a mismatch the controller may act on, because the mismatch verdict
// itself would be uncited (req.7.3).
func (controller *Controller) ObserveMismatch(
	ctx context.Context,
	step int,
	execution loop.ToolExecution,
) (string, bool) {
	if err := ctx.Err(); err != nil {
		return "", false
	}
	if execution.Citation == nil {
		return "", false
	}
	signals := Signals{Step: step, Citation: execution.Citation}
	if execution.MatchVerdict == cortexclient.ToolMatchMismatched ||
		execution.Error != "" {
		signals.MismatchedEvidence = 1
	}
	mod, armed := controller.Observe(signals)
	if !armed {
		return "", false
	}
	return mod.Line, true
}
