// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"fmt"
	"sync"

	"centra/agents/neo/internal/o1"
)

type O1FailureReplayLane struct {
	mu      sync.Mutex
	records []o1.FailureReplayCase
}

func (lane *O1FailureReplayLane) RecordIncomplete(
	_ context.Context,
	record IncompleteRecord,
) {
	if lane == nil {
		return
	}
	layer, transition := incompleteRecovery(record.Phase)
	outcome := o1.NewOutcome("runtime."+record.Phase).
		Fail(layer, "runtime_incomplete").
		Retryable(true).
		Evidence(fmt.Sprintf(
			"turn=%s phase=%s attempts=%d",
			record.TurnID, record.Phase, record.AttemptCount,
		)).
		AllowTransition(transition).
		SetMeta("recovery_advice", record.RecoveryAdvice).
		MustBuild()
	replay := o1.FailureReplayCase{
		ID: fmt.Sprintf(
			"runtime-incomplete-%s-%s-%d",
			record.TurnID, record.Phase, record.AttemptCount,
		),
		Name:             "runtime incomplete in " + record.Phase,
		FailureClass:     layer,
		ExpectedBehavior: "typed_recovery",
		Source:           incompleteHazard(layer),
		Outcome:          outcome,
	}
	lane.mu.Lock()
	lane.records = append(lane.records, replay)
	lane.mu.Unlock()
}

func (lane *O1FailureReplayLane) Records() []o1.FailureReplayCase {
	if lane == nil {
		return nil
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return append([]o1.FailureReplayCase(nil), lane.records...)
}

func incompleteRecovery(
	phase string,
) (o1.FailureLayer, o1.TransitionKind) {
	switch phase {
	case "effect_reconciliation":
		return o1.LayerConflict, o1.TransitionReconcile
	case "tool":
		return o1.LayerProcess, o1.TransitionReconcile
	case "turn":
		return o1.LayerCancellation, o1.TransitionRetry
	default:
		return o1.LayerTransport, o1.TransitionRetry
	}
}

func incompleteHazard(layer o1.FailureLayer) string {
	switch layer {
	case o1.LayerProcess, o1.LayerCancellation:
		return "O1-HZ-PROCESS-001"
	case o1.LayerConflict:
		return "O1-HZ-STATE-001"
	default:
		return "O1-HZ-EXTERNAL-001"
	}
}
