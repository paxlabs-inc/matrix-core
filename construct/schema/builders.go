// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package schema

import "matrix/construct/schema/primitives"

// Convenience constructors. Each returns a well-formed Surface (Kind set, the
// matching payload attached) so callers — transport emit helpers, projectors,
// tests — never hand-wire the discriminator/payload pairing and risk a
// kind/payload mismatch. Decorate the result with WithRef/WithAttributes/etc.

// NewNarration builds a narration surface.
func NewNarration(id string, n *primitives.Narration) *Surface {
	return &Surface{Kind: KindNarration, ID: id, Narration: n}
}

// NewMetric builds a metric surface.
func NewMetric(id string, m *primitives.Metric) *Surface {
	return &Surface{Kind: KindMetric, ID: id, Metric: m}
}

// NewEntity builds an entity surface.
func NewEntity(id string, e *primitives.Entity) *Surface {
	return &Surface{Kind: KindEntity, ID: id, Entity: e}
}

// NewStructure builds a structure surface.
func NewStructure(id string, st *primitives.Structure) *Surface {
	return &Surface{Kind: KindStructure, ID: id, Structure: st}
}

// NewStream builds a stream surface.
func NewStream(id string, s *primitives.Stream) *Surface {
	return &Surface{Kind: KindStream, ID: id, Stream: s}
}

// NewTimeline builds a timeline surface.
func NewTimeline(id string, tl *primitives.Timeline) *Surface {
	return &Surface{Kind: KindTimeline, ID: id, Timeline: tl}
}

// NewCanvas builds a canvas surface.
func NewCanvas(id string, c *primitives.Canvas) *Surface {
	return &Surface{Kind: KindCanvas, ID: id, Canvas: c}
}

// NewAsk builds an ask surface.
func NewAsk(id string, a *primitives.Ask) *Surface {
	return &Surface{Kind: KindAsk, ID: id, Ask: a}
}

// WithRef sets the linking ref and returns the surface for chaining.
func (s *Surface) WithRef(ref string) *Surface {
	s.Ref = ref
	return s
}

// WithSeq sets the ordering seq and returns the surface for chaining.
func (s *Surface) WithSeq(seq uint64) *Surface {
	s.Seq = seq
	return s
}

// WithParent sets the composition parent and returns the surface for chaining.
func (s *Surface) WithParent(parent string) *Surface {
	s.Parent = parent
	return s
}

// WithAttributes sets the decoration block and returns the surface for
// chaining.
func (s *Surface) WithAttributes(a *Attributes) *Surface {
	s.Attributes = a
	return s
}
