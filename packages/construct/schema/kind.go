// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package schema defines the on-the-wire contract for the Construct: the
// agent-to-human surface-projection primitive. It is the Go source of truth
// from which the client TypeScript types are generated (see
// internal/codegen). The vocabulary is FROZEN at the 8 primitives declared
// in construct.frozen.kvx [vocabulary]; richness comes from COMPOSITION +
// attributes, never from adding kinds without a version bump (invariant i3).
package schema

// Kind is the surface discriminator: which one of the 8 frozen primitives a
// surface projects onto. Pinned as stable STRING constants (not enum ints) so
// the wire and cross-language consumers are forward-stable.
type Kind string

// The 8 frozen primitive kinds (construct.frozen.kvx [vocabulary].primitives).
const (
	KindNarration Kind = "narration"
	KindMetric    Kind = "metric"
	KindEntity    Kind = "entity"
	KindStructure Kind = "structure"
	KindStream    Kind = "stream"
	KindTimeline  Kind = "timeline"
	KindCanvas    Kind = "canvas"
	KindAsk       Kind = "ask"
)

// Kinds is the frozen, ordered set of all 8 primitive kinds. The order is the
// vocabulary order in the frozen spec and is the deterministic emit order for
// codegen. Do NOT reorder; appending is a version-bump event (invariant i3).
var Kinds = []Kind{
	KindNarration,
	KindMetric,
	KindEntity,
	KindStructure,
	KindStream,
	KindTimeline,
	KindCanvas,
	KindAsk,
}

// ValidKind reports whether k is one of the 8 frozen primitive kinds.
func ValidKind(k Kind) bool {
	switch k {
	case KindNarration, KindMetric, KindEntity, KindStructure,
		KindStream, KindTimeline, KindCanvas, KindAsk:
		return true
	default:
		return false
	}
}
