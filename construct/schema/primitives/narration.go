// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package primitives holds the typed payload for each of the 8 frozen
// Construct primitives (construct.frozen.kvx [vocabulary.*]). Each struct is
// the language-neutral shape the agent fills; the client renders it through a
// trusted, deterministic renderer (invariant i2: the agent fills trusted
// primitives, never emits arbitrary UI).
package primitives

// NarrationRole distinguishes the sub-state of the agent's language: its
// in-progress thinking, a stated intent, or the final answer
// (construct.frozen.kvx [vocabulary.narration]: "'thinking' vs 'answer' is a
// sub-state"). Empty defaults to a plain answer.
type NarrationRole string

const (
	NarrationThinking NarrationRole = "thinking"
	NarrationIntent   NarrationRole = "intent"
	NarrationAnswer   NarrationRole = "answer"
)

// Narration is the agent's language: reasoning, intent, answers
// (axis: language).
type Narration struct {
	// Text is the language content.
	Text string `json:"text"`
	// Role is the sub-state of the narration (thinking|intent|answer).
	Role NarrationRole `json:"role,omitempty"`
}
