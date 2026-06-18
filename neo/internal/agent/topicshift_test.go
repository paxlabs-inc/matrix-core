// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import "testing"

// The tracker is exercised through observeVec with synthetic vectors: the
// hash-stub embedder's geometry is not semantic, so driving the math directly
// is the only deterministic way to assert pivot behavior.

// A run of on-topic turns must report NO pivot; a turn orthogonal to the
// established centroid must report a pivot and reseed the centroid; a turn
// on-topic with the NEW centroid must again report no pivot.
func TestTopicTrackerPivotDetection(t *testing.T) {
	tr := newTopicTracker(nil)

	// First turn seeds the centroid — never a pivot.
	if tr.observeVec([]float32{1, 0, 0, 0}) {
		t.Fatal("first turn must not be a pivot (centroid seed)")
	}
	// A close turn (cosine ~0.95) stays on-topic.
	if tr.observeVec([]float32{0.95, 0.31, 0, 0}) {
		t.Error("a near-collinear turn must not pivot")
	}
	// An orthogonal turn (cosine ~0 < 0.30) is a pivot.
	if !tr.observeVec([]float32{0, 1, 0, 0}) {
		t.Error("an orthogonal turn must be detected as a topic pivot")
	}
	// After the pivot the centroid is the new topic; a turn close to it does
	// not pivot again.
	if tr.observeVec([]float32{0, 0.95, 0.31, 0}) {
		t.Error("a turn on the NEW topic must not pivot")
	}
}

// A borderline turn just above the threshold must NOT pivot, and one just below
// MUST — the boundary is at topicPivotCosine.
func TestTopicTrackerThresholdBoundary(t *testing.T) {
	above := newTopicTracker(nil)
	above.observeVec([]float32{1, 0})
	// cosine = 0.40 (> 0.30): on-topic.
	if above.observeVec([]float32{0.40, 0.91651}) {
		t.Errorf("cosine ~0.40 (> %.2f) must not pivot", topicPivotCosine)
	}

	below := newTopicTracker(nil)
	below.observeVec([]float32{1, 0})
	// cosine = 0.20 (< 0.30): pivot.
	if !below.observeVec([]float32{0.20, 0.9798}) {
		t.Errorf("cosine ~0.20 (< %.2f) must pivot", topicPivotCosine)
	}
}

// A nil tracker and a tracker with no embedder must both be safe no-ops that
// never report a pivot (context is never dropped on a missing embedder).
func TestTopicTrackerNilSafe(t *testing.T) {
	var nilTracker *topicTracker
	if nilTracker.observe("anything") {
		t.Error("nil tracker must report no pivot")
	}
	noEmb := newTopicTracker(nil)
	if noEmb.observe("anything") {
		t.Error("a tracker without an embedder must report no pivot")
	}

	var a *Agent
	if a.observeTopic("x") {
		t.Error("nil agent observeTopic must be a safe no-op")
	}
}
