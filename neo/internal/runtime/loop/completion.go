// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"sync"
)

type StateCompletionGate struct {
	mu       sync.RWMutex
	decision CompletionDecision
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
