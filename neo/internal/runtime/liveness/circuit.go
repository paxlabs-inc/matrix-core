// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package liveness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// DefaultRepeatThreshold is the number of dispatches of one canonical tool
// signature a turn may make before the breaker opens. It sits deliberately
// above the same-strategy retry bound (which tops out at four consecutive
// dispatches of one signature) so both mechanisms stay reachable: the retry
// bound catches consecutive repetition and returns the more informative typed
// incomplete, while the breaker is the phase-crossing backstop that also sees
// interleaved repetition — the A/B/A/B churn the legacy similarity heuristic
// was blind to.
const DefaultRepeatThreshold = 6

var ErrCircuitOpen = errors.New("runtime liveness: repetition circuit open")

type BreakerConfig struct {
	RepeatThreshold int `json:"repeat_threshold"`
}

func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{RepeatThreshold: DefaultRepeatThreshold}
}

func (config BreakerConfig) normalized() BreakerConfig {
	if config.RepeatThreshold <= 0 {
		config.RepeatThreshold = DefaultRepeatThreshold
	}
	return config
}

// BreakerState is the serializable repetition ledger for one turn. It rides the
// loop cursor into the durable checkpoint so a resumed turn cannot spend the
// churn budget twice.
type BreakerState struct {
	Signatures map[string]int `json:"signatures,omitempty"`
	Open       bool           `json:"open,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Signature  string         `json:"signature,omitempty"`
}

// CircuitError is the typed refusal a tripped breaker produces at whichever
// phase observed it.
type CircuitError struct {
	Phase     string `json:"phase"`
	Reason    string `json:"reason"`
	Signature string `json:"signature"`
}

func (err *CircuitError) Error() string {
	return fmt.Sprintf(
		"runtime liveness: repetition circuit open at the %s phase: %s",
		err.Phase, err.Reason,
	)
}

func (err *CircuitError) Unwrap() error { return ErrCircuitOpen }

// Observe records one dispatched canonical signature and opens the breaker when
// that signature has been dispatched more times than the threshold allows. It
// is monotonic: an open breaker never closes inside a turn.
func (config BreakerConfig) Observe(state *BreakerState, signature string) {
	if state == nil || signature == "" {
		return
	}
	if state.Signatures == nil {
		state.Signatures = make(map[string]int)
	}
	state.Signatures[signature]++
	if state.Open {
		return
	}
	count := state.Signatures[signature]
	if count > config.normalized().RepeatThreshold {
		state.Open = true
		state.Signature = signature
		state.Reason = fmt.Sprintf(
			"one canonical tool signature was dispatched %d times in this turn without converging",
			count,
		)
	}
}

// Enforce fails closed at the turn, provider, and tool phases once the breaker
// is open.
func (state BreakerState) Enforce(phase string) error {
	if !state.Open {
		return nil
	}
	return &CircuitError{
		Phase: phase, Reason: state.Reason, Signature: state.Signature,
	}
}

// CanonicalToolSignature is the breaker and same-strategy key: the tool name
// plus its arguments in canonical JSON encoding, so semantically identical
// calls that differ only in key order or whitespace share one signature. It is
// deliberately NOT the idempotency key, which is per call ID and therefore
// unique to every dispatch.
func CanonicalToolSignature(name string, arguments json.RawMessage) string {
	canonical := arguments
	var value any
	if json.Unmarshal(arguments, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			canonical = encoded
		}
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(name))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(canonical)
	return hex.EncodeToString(digest.Sum(nil))
}
