// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"sync"

	"matrix/cortexclient"
)

type StateCompletionGate struct {
	mu       sync.RWMutex
	decision CompletionDecision
}

// EvidenceCompletionGate is the production completion gate for an interactive
// Resurrection turn. Requests that require inspecting or changing an external
// state cannot close until this turn has produced a citation-verified matched
// tool execution. Absence of evidence is a blocker, never permission to pass.
type EvidenceCompletionGate struct {
	mu       sync.RWMutex
	required bool
	verified int
}

func NewEvidenceCompletionGate(required bool) *EvidenceCompletionGate {
	return &EvidenceCompletionGate{required: required}
}

func (gate *EvidenceCompletionGate) ObserveToolExecution(
	_ context.Context,
	execution ToolExecution,
) error {
	if gate == nil || execution.Error != "" || execution.Citation == nil ||
		execution.MatchVerdict != cortexclient.ToolMatchMatched {
		return nil
	}
	gate.mu.Lock()
	gate.verified++
	gate.mu.Unlock()
	return nil
}

func (gate *EvidenceCompletionGate) CheckCompletion(
	context.Context,
) (CompletionDecision, error) {
	if gate == nil || !gate.required {
		return CompletionDecision{Ready: true}, nil
	}
	gate.mu.RLock()
	verified := gate.verified
	gate.mu.RUnlock()
	if verified > 0 {
		return CompletionDecision{Ready: true}, nil
	}
	return CompletionDecision{
		Ready:      false,
		Reason:     "the request requires inspection or action, but this turn has no citation-verified successful tool evidence",
		NextAction: "use the appropriate native tool and verify its result before answering",
	}, nil
}

func NewStateCompletionGate(decision CompletionDecision) *StateCompletionGate {
	return &StateCompletionGate{decision: decision}
}

func (gate *StateCompletionGate) Update(decision CompletionDecision) {
	gate.mu.Lock()
	gate.decision = decision
	gate.mu.Unlock()
}

func (gate *StateCompletionGate) CheckCompletion(
	context.Context,
) (CompletionDecision, error) {
	if gate == nil {
		return CompletionDecision{Ready: true}, nil
	}
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	return gate.decision, nil
}
