// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package converge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type FailureKind string

const (
	FailureTransient          FailureKind = "transient"
	FailureDeterministic      FailureKind = "deterministic"
	FailureUnknownEffect      FailureKind = "unknown_effect"
	FailureRepeatedSemantic   FailureKind = "repeated_semantic"
	FailureProviderCorruption FailureKind = "provider_corruption"
	FailureProcessUnusable    FailureKind = "process_unusable"
	FailureDelivery           FailureKind = "delivery"
)

type Action string

const (
	ActionBackoffRetry   Action = "bounded_backoff_retry"
	ActionStrategyChange Action = "strategy_change"
	ActionReconcile      Action = "reconcile"
	ActionDegrade        Action = "degrade_or_honest_blocker"
	ActionRetryProvider  Action = "retry_provider"
	ActionResumeTurn     Action = "resume_same_logical_turn"
	ActionRetryDelivery  Action = "retry_delivery_only"
	ActionStop           Action = "stop"
)

type Fingerprint struct {
	Operation           string          `json:"operation"`
	NormalizedArguments json.RawMessage `json:"normalized_arguments,omitempty"`
	Phase               string          `json:"phase"`
	FailureLayer        string          `json:"failure_layer"`
	NormalizedCause     string          `json:"normalized_cause"`
	EffectStatus        string          `json:"effect_status"`
}

func (fingerprint Fingerprint) Key() string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(fingerprint.Operation),
		string(fingerprint.NormalizedArguments),
		strings.TrimSpace(fingerprint.Phase),
		strings.TrimSpace(fingerprint.FailureLayer),
		strings.ToLower(strings.Join(strings.Fields(fingerprint.NormalizedCause), " ")),
		strings.TrimSpace(fingerprint.EffectStatus),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

type Failure struct {
	Kind        FailureKind `json:"kind"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

type Attempt struct {
	Failure Failure `json:"failure"`
	Count   uint64  `json:"count"`
}

type State struct {
	Attempts        map[string]Attempt `json:"attempts,omitempty"`
	StrategyChanges []string           `json:"strategy_changes,omitempty"`
}

type Decision struct {
	Action                 Action        `json:"action"`
	Count                  uint64        `json:"count"`
	Retry                  bool          `json:"retry"`
	RequiresStrategyChange bool          `json:"requires_strategy_change"`
	Backoff                time.Duration `json:"backoff,omitempty"`
}

type Controller struct{}

func (Controller) Observe(state *State, failure Failure, limits Limits) Decision {
	if state.Attempts == nil {
		state.Attempts = make(map[string]Attempt)
	}
	key := failure.Fingerprint.Key()
	attempt := state.Attempts[key]
	attempt.Failure = failure
	attempt.Count++
	state.Attempts[key] = attempt
	decision := Decision{Count: attempt.Count, Action: ActionStop}
	switch failure.Kind {
	case FailureTransient:
		decision.Action, decision.Retry = ActionBackoffRetry, attempt.Count <= max(limits.IdenticalFailureRepeats, 1)
		decision.Backoff = time.Duration(attempt.Count) * 250 * time.Millisecond
		if !decision.Retry {
			decision.Action = ActionDegrade
		}
	case FailureDeterministic:
		decision.Action, decision.RequiresStrategyChange = ActionStrategyChange, true
	case FailureUnknownEffect:
		decision.Action = ActionReconcile
	case FailureRepeatedSemantic:
		decision.Action, decision.RequiresStrategyChange = ActionStrategyChange, true
		if attempt.Count > max(limits.IdenticalFailureRepeats, 1) {
			decision.Action = ActionDegrade
		}
	case FailureProviderCorruption:
		decision.Action, decision.Retry = ActionRetryProvider, attempt.Count <= max(limits.CassandraO1Repairs, 1)
		if !decision.Retry {
			decision.Action = ActionDegrade
		}
	case FailureProcessUnusable:
		decision.Action = ActionResumeTurn
	case FailureDelivery:
		decision.Action, decision.Retry = ActionRetryDelivery, attempt.Count < max(limits.DeliveryAttempts, 1)
		if !decision.Retry {
			decision.Action = ActionStop
		}
	}
	if decision.RequiresStrategyChange {
		state.StrategyChanges = appendUnique(state.StrategyChanges, key)
	}
	return decision
}

type AttemptFacts struct {
	GenerationAttempts uint64
	ToolAttempts       uint64
	ResultsPreserved   uint64
	UnresolvedIssue    string
	RetryClass         FailureKind
	NextRecovery       Action
}

func CeilingReport(facts AttemptFacts) string {
	generation := "generation"
	if facts.GenerationAttempts != 1 {
		generation = "generations"
	}
	tool := "tool attempt"
	if facts.ToolAttempts != 1 {
		tool = "tool attempts"
	}
	return fmt.Sprintf(
		"Stopped after %d %s and %d %s; %d results are preserved. Unresolved: %s. Retry class: %s. Next recovery: %s.",
		facts.GenerationAttempts, generation, facts.ToolAttempts, tool,
		facts.ResultsPreserved, strings.TrimSpace(facts.UnresolvedIssue),
		facts.RetryClass, facts.NextRecovery,
	)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
