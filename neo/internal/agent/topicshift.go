// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Topic-shift detection (memory capabilities v2, Phase 3). A long thread that
// pivots to an unrelated subject should not drag the previous topic's recalled
// past-turns into the new one. The tracker keeps a rolling embedding centroid
// of recent turns; when a new turn's cosine to that centroid drops below the
// pivot threshold it is a topic PIVOT, and the agent resets the per-turn
// RETRIEVED working set (conversational recall) for that turn while preserving
// the pinned tier (identity / hard constraints / active goal / learned
// guidance) and the durable cortex retrieval, which is re-faulted fresh against
// the new turn anyway.
//
// Pure Neo-side working-set hygiene: nothing is persisted, no cortex byte
// changes. Best-effort — a nil embedder or an embed hiccup reports "no pivot"
// so context is never dropped on a transient failure.
package agent

import (
	"strings"

	"matrix/cortex/embed"
)

// topicPivotCosine is the cosine-to-centroid floor below which a new turn is
// treated as a topic pivot. 0.30 matches the off-topic floor used by the
// rejection-attest path (salience_realism.go) so "off-topic to the running
// thread" means the same thing on both the read and the learning sides.
const topicPivotCosine float32 = 0.30

// topicWindowN bounds the rolling centroid so it tracks the LAST N turns
// rather than the whole thread — old turns decay out instead of anchoring the
// centroid forever, keeping pivot detection responsive on long threads.
const topicWindowN = 8

// topicTracker maintains the rolling topic centroid for one conversation.
// Single-threaded by construction (a session serializes its turns), so it
// needs no lock.
type topicTracker struct {
	emb      embed.Embedder
	centroid []float32
	count    int
}

func newTopicTracker(emb embed.Embedder) *topicTracker {
	return &topicTracker{emb: emb}
}

// observe embeds text, folds it into the rolling centroid, and reports whether
// the turn is a topic pivot. A nil tracker/embedder, empty text, or an embed
// failure reports no pivot (the conservative default: never drop context on a
// hiccup).
func (t *topicTracker) observe(text string) bool {
	if t == nil || t.emb == nil {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	vec, err := t.emb.Embed(text)
	if err != nil || len(vec) == 0 {
		return false
	}
	return t.observeVec(vec)
}

// observeVec is the embedding-free core (unit-tested directly with synthetic
// vectors, since the hash-stub embedder's geometry is not semantic). It seeds
// the centroid on the first turn, detects a pivot against the established
// centroid, and otherwise folds the turn into a window-capped rolling mean.
func (t *topicTracker) observeVec(vec []float32) bool {
	// First turn (or a dimension change): seed the centroid, no pivot.
	if t.count == 0 || len(t.centroid) != len(vec) {
		t.centroid = append([]float32(nil), vec...)
		t.count = 1
		return false
	}
	if embed.Cosine(vec, t.centroid) < topicPivotCosine {
		// Pivot: the new turn seeds a fresh topic centroid.
		t.centroid = append([]float32(nil), vec...)
		t.count = 1
		return true
	}
	// Rolling mean capped at topicWindowN: weight the existing centroid by at
	// most N−1 so the newest turn always keeps real influence (EMA-like once
	// the window is full).
	w := t.count
	if w >= topicWindowN {
		w = topicWindowN - 1
	}
	inv := 1.0 / float32(w+1)
	for i := range t.centroid {
		t.centroid[i] = (t.centroid[i]*float32(w) + vec[i]) * inv
	}
	if t.count < topicWindowN {
		t.count++
	}
	return false
}

// observeTopic folds the current user turn into the topic centroid and reports
// whether it is a pivot. Safe on a nil tracker.
func (a *Agent) observeTopic(userInput string) bool {
	if a == nil || a.topic == nil {
		return false
	}
	return a.topic.observe(userInput)
}
