// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"encoding/json"
	"strings"
	"time"

	"matrix/cortex/memory"
)

// OpportunityStatus is the lifecycle of a captured Automatrix opportunity.
// The closed transition order is pending -> scheduled -> in_progress ->
// done | dismissed; status is the single authoritative field the autonomous
// picker and the management UI gate on.
type OpportunityStatus string

const (
	OpportunityPending    OpportunityStatus = "pending"
	OpportunityScheduled  OpportunityStatus = "scheduled"
	OpportunityInProgress OpportunityStatus = "in_progress"
	OpportunityDone       OpportunityStatus = "done"
	OpportunityDismissed  OpportunityStatus = "dismissed"
)

// Valid reports whether s is one of the five lifecycle states.
func (s OpportunityStatus) Valid() bool {
	switch s {
	case OpportunityPending, OpportunityScheduled, OpportunityInProgress,
		OpportunityDone, OpportunityDismissed:
		return true
	}
	return false
}

// ParseOpportunityStatus coerces a free-form string to an OpportunityStatus,
// returning ok=false when it is not one of the lifecycle states.
func ParseOpportunityStatus(s string) (OpportunityStatus, bool) {
	st := OpportunityStatus(strings.ToLower(strings.TrimSpace(s)))
	return st, st.Valid()
}

// OpportunitySpec is the structured Automatrix opportunity — a specific,
// actionable thing the user implied wanting (or would clearly benefit from)
// but did NOT explicitly ask Neo to do. It is the durable proactive-work
// queue item.
//
// cortex has a closed nine-type taxonomy, so — exactly as PatternSpec maps a
// richer procedural schema onto cortex's flat Pattern — an OpportunitySpec is
// mapped ONTO a cortex Goal record (the closest typed-record fit): the summary
// rides Goal.Statement (so semantic dedup ranks on the summary), the lifecycle
// status rides Goal.Status (the authoritative, queryable field), and the
// remaining bookkeeping is encoded as a canonical-JSON sidecar carried in the
// Goal's SuccessCriteria. The record is distinguished from a real Goal by the
// opportunityTag on its head, so the autonomous picker scopes to opportunities
// and the ambient pager excludes them from the prompt window. This keeps
// cortex — the shared, replay-critical, tamper-evident store — untouched.
//
// No secrets, tokens, private keys, or .env values are ever stored on an
// opportunity: it carries only the user-facing summary, the grounding
// rationale, and lifecycle bookkeeping.
type OpportunitySpec struct {
	// Summary is the actionable, self-sufficient task (imperative).
	Summary string
	// Rationale grounds the opportunity in something the user actually said
	// or did (anti-hallucination).
	Rationale string
	// Status is the lifecycle state (authoritative).
	Status OpportunityStatus
	// EligibleAutonomous is true only for non-financial work the woken run
	// may perform unprompted; a financial opportunity is captured with this
	// false (surfaced for explicit approval, never auto-run).
	EligibleAutonomous bool
	// Confidence is the extraction confidence in [0,1] (a ranking input).
	Confidence float32
	// OriginConversationID is the thread the need arose in; the autonomous
	// run resumes here for context fidelity.
	OriginConversationID string
	// Attempts counts autonomous run attempts (bounded retry bookkeeping).
	Attempts int
	// CreatedAt / UpdatedAt are lifecycle timestamps (UTC).
	CreatedAt time.Time
	UpdatedAt time.Time
	// URI is the cortex URI of the record; populated on read, ignored on write.
	URI string
}

// opportunityTag marks a cortex Goal record as an Automatrix opportunity. It
// scopes the autonomous picker's tag query and lets the ambient pager exclude
// the proactive queue from the prompt window (opportunities are a separate
// queue, not general recallable memory).
const opportunityTag memory.Tag = "automatrix:opportunity"

// opportunityMetaPrefix tags the canonical-JSON sidecar encoded into the
// Goal's SuccessCriteria so a hand-written or legacy criterion is
// distinguishable and decoding degrades gracefully (mirrors patternEncPrefix).
const opportunityMetaPrefix = "neo.automatrix.opportunity.v1:"

// opportunityMeta is the JSON sidecar carrying the opportunity fields that do
// not have a natural home on GoalData (summary -> Statement, status ->
// Status). Timestamps are unix-nano; zero time encodes as 0.
type opportunityMeta struct {
	Rationale            string  `json:"rationale,omitempty"`
	EligibleAutonomous   bool    `json:"eligible_autonomous"`
	Confidence           float32 `json:"confidence,omitempty"`
	OriginConversationID string  `json:"origin_conversation_id,omitempty"`
	Attempts             int     `json:"attempts,omitempty"`
	CreatedAt            int64   `json:"created_at"`
	UpdatedAt            int64   `json:"updated_at"`
}

// encodeOpportunityGoal maps an OpportunitySpec onto a cortex GoalData. The
// summary drives the embedded form (semantic dedup), the status is the
// authoritative queryable field, and the bookkeeping rides a canonical-JSON
// sidecar in SuccessCriteria (rendered only as a count, so it never pollutes
// the summary's embedding).
func encodeOpportunityGoal(spec OpportunitySpec) memory.GoalData {
	meta := opportunityMeta{
		Rationale:            strings.TrimSpace(spec.Rationale),
		EligibleAutonomous:   spec.EligibleAutonomous,
		Confidence:           clampUnit(spec.Confidence),
		OriginConversationID: strings.TrimSpace(spec.OriginConversationID),
		Attempts:             spec.Attempts,
		CreatedAt:            unixNanoOrZero(spec.CreatedAt),
		UpdatedAt:            unixNanoOrZero(spec.UpdatedAt),
	}
	encoded := opportunityMetaPrefix
	if b, err := json.Marshal(meta); err == nil {
		encoded = opportunityMetaPrefix + string(b)
	}
	return memory.GoalData{
		SchemaVersion:   1,
		Statement:       strings.TrimSpace(spec.Summary),
		Status:          memory.GoalStatus(spec.Status),
		SuccessCriteria: []string{encoded},
	}
}

// decodeOpportunityGoal reverses encodeOpportunityGoal. A Goal without the
// sidecar criterion decodes to a summary+status-only spec (graceful degrade).
func decodeOpportunityGoal(g memory.GoalData, uri string) OpportunitySpec {
	spec := OpportunitySpec{
		Summary: strings.TrimSpace(g.Statement),
		Status:  OpportunityStatus(g.Status),
		URI:     uri,
	}
	for _, c := range g.SuccessCriteria {
		rest, ok := strings.CutPrefix(strings.TrimSpace(c), opportunityMetaPrefix)
		if !ok {
			continue
		}
		var meta opportunityMeta
		if err := json.Unmarshal([]byte(rest), &meta); err == nil {
			spec.Rationale = meta.Rationale
			spec.EligibleAutonomous = meta.EligibleAutonomous
			spec.Confidence = meta.Confidence
			spec.OriginConversationID = meta.OriginConversationID
			spec.Attempts = meta.Attempts
			spec.CreatedAt = fromUnixNanoOrZero(meta.CreatedAt)
			spec.UpdatedAt = fromUnixNanoOrZero(meta.UpdatedAt)
		}
		break
	}
	// Live eligibility heal: items captured under the old topic-keyword
	// classifier carry a stored eligible_autonomous=false for pure analysis
	// work ("Model the PAX price…" tripped on "price"). Re-derive with the
	// current action-based classifier so a stale stored verdict can only be
	// RELAXED, never tightened — a stored-eligible item stays eligible, and an
	// action-shaped summary still fails closed through ClassifyFinancial.
	if !spec.EligibleAutonomous && !ClassifyFinancial(spec.Summary) {
		spec.EligibleAutonomous = true
	}
	return spec
}

// asGoalData narrows a decoded TypedData to GoalData (value or pointer form).
func asGoalData(data memory.TypedData) (memory.GoalData, bool) {
	switch x := data.(type) {
	case memory.GoalData:
		return x, true
	case *memory.GoalData:
		return *x, true
	default:
		return memory.GoalData{}, false
	}
}

// headHasOpportunityTag reports whether a head carries the opportunity tag, so
// the ambient retrieval lanes can exclude the proactive queue from the prompt
// window cheaply (no Data decode).
func headHasOpportunityTag(h memory.Head) bool {
	for _, t := range h.Tags {
		if t == opportunityTag {
			return true
		}
	}
	return false
}

func unixNanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromUnixNanoOrZero(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
