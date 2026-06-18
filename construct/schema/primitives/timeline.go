// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// StepStatus is the state of one timeline step. This is what distinguishes a
// Timeline from a Stream: each step is STATEFUL (has status/result), not just
// append-only bytes (frozen [vocabulary.timeline].distinct_from_stream).
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
)

// StepStatusValues is the frozen, ordered set of timeline step states.
var StepStatusValues = []StepStatus{StepPending, StepRunning, StepDone, StepFailed}

// ValidStepStatus reports whether s is a known step status.
func ValidStepStatus(s StepStatus) bool {
	switch s {
	case StepPending, StepRunning, StepDone, StepFailed:
		return true
	default:
		return false
	}
}

// TimelineStep is one stateful step over time (a plan node, async job stage,
// lifecycle transition, swarm member).
type TimelineStep struct {
	ID     string     `json:"id"`
	Label  string     `json:"label"`
	Status StepStatus `json:"status"`
	// Detail is optional human context for the step's current state.
	Detail string `json:"detail,omitempty"`
	// Ref optionally links the step to another surface (e.g. a sub-agent's
	// own timeline, or the entity it produced).
	Ref string `json:"ref,omitempty"`
}

// Timeline is structured, STATEFUL steps over time (axis: temporal-process).
// It covers plan execution, async jobs, lifecycle, swarm (frozen
// [vocabulary.timeline]).
type Timeline struct {
	// Title is an optional human header for the timeline.
	Title string `json:"title,omitempty"`
	// Steps are the stateful steps, in order.
	Steps []TimelineStep `json:"steps"`
}
