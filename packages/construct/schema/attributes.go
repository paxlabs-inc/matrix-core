// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package schema

// The decorating axes (construct.frozen.kvx [attributes]). These do NOT become
// primitives; they DECORATE any primitive (invariant i4: axes decorate, they
// do not duplicate). `ref` is carried on the envelope (Surface.Ref) for
// primitive-linking; the remaining four ride here.

// Stakes is the consequence axis of a surface.
type Stakes string

const (
	StakesFact         Stakes = "fact"
	StakesHypothesis   Stakes = "hypothesis"
	StakesDecision     Stakes = "decision"
	StakesIrreversible Stakes = "irreversible"
)

// StakesValues is the frozen, ordered set of stakes values.
var StakesValues = []Stakes{StakesFact, StakesHypothesis, StakesDecision, StakesIrreversible}

// ValidStakes reports whether s is a known stakes value (empty is allowed: it
// means unstated, i.e. a plain fact-like surface with no special framing).
func ValidStakes(s Stakes) bool {
	switch s {
	case "", StakesFact, StakesHypothesis, StakesDecision, StakesIrreversible:
		return true
	default:
		return false
	}
}

// Temporality is the time axis of a surface.
type Temporality string

const (
	TemporalityPoint      Temporality = "point"
	TemporalityStream     Temporality = "stream"
	TemporalityPersistent Temporality = "persistent"
)

// TemporalityValues is the frozen, ordered set of temporality values.
var TemporalityValues = []Temporality{TemporalityPoint, TemporalityStream, TemporalityPersistent}

// ValidTemporality reports whether tmp is a known temporality value (empty is
// allowed: unstated defaults to a discrete point).
func ValidTemporality(tmp Temporality) bool {
	switch tmp {
	case "", TemporalityPoint, TemporalityStream, TemporalityPersistent:
		return true
	default:
		return false
	}
}

// Cost is the metered cost/authority a surface or its affordance carries
// (gateway PAX spend, spend cap). Drives the auto-confirm affordance when
// paired with Stakes=irreversible (frozen [attributes].example).
type Cost struct {
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit,omitempty"`
	Cap    float64 `json:"cap,omitempty"`
}

// Attributes is the decoration block attached to any surface. All fields are
// optional; an absent block means an undecorated surface.
type Attributes struct {
	// Stakes frames the consequence of the surface (fact|hypothesis|
	// decision|irreversible).
	Stakes Stakes `json:"stakes,omitempty"`
	// Confidence is the agent's certainty in this surface, 0..1. A pointer so
	// an explicit 0 is distinguishable from "unstated".
	Confidence *float64 `json:"confidence,omitempty"`
	// Cost is the metered cost/authority the surface carries.
	Cost *Cost `json:"cost,omitempty"`
	// Temporality is the time character of the surface (point|stream|
	// persistent).
	Temporality Temporality `json:"temporality,omitempty"`
}

// Valid reports whether the attribute enums are within their frozen sets and
// confidence (when set) is in [0,1].
func (a *Attributes) Valid() bool {
	if a == nil {
		return true
	}
	if !ValidStakes(a.Stakes) || !ValidTemporality(a.Temporality) {
		return false
	}
	if a.Confidence != nil && (*a.Confidence < 0 || *a.Confidence > 1) {
		return false
	}
	return true
}
