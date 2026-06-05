// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Edge taxonomy and EdgeRecord shape for the Matrix cortex.
//
// Spec: research/04-cortex.md §5 (edge taxonomy, 14 byte-tagged types) and
// §11 (AddEdge/RemoveEdge atomicity — both directions commit in one batch).
//
// Edges are first-class typed adjacency. Every edge is written twice:
//
//	e/from/<src:16>/<edge:1>/<dst:16>   forward
//	e/to/<dst:16>/<edge:1>/<src:16>     reverse
//
// Both keys carry the SAME canonical CBOR EdgeRecord value so a hop in
// either direction reads consistent metadata in one Get. Removal is
// soft (Tombstoned=true) per §6 audit-trail semantics; the keys remain
// in the store and replay reproduces them. Default traversal filters
// tombstoned edges; query callers can opt in via EdgeExpr.IncludeTombstoned.

package memory

import (
	"errors"
	"time"
)

// EdgeType is the 1-byte edge discriminator from §5.
type EdgeType byte

const (
	EdgeDerivedFrom  EdgeType = 0x01 // this memory was inferred from those
	EdgeSupersedes   EdgeType = 0x02 // this version replaces an earlier conceptual memory
	EdgeReferences   EdgeType = 0x03 // soft pointer (entity reference)
	EdgeContradicts  EdgeType = 0x04 // conflict marker
	EdgeCorroborates EdgeType = 0x05 // mutual support
	EdgeConsentsTo   EdgeType = 0x06 // actor consents to action/agent
	EdgeDispatchedTo EdgeType = 0x07 // intent dispatched to agent
	EdgeAttestedBy   EdgeType = 0x08 // outcome attested by agent
	EdgeCitedIn      EdgeType = 0x09 // memory cited in a plan/intent
	EdgeTombstones   EdgeType = 0x0A // tombstone of another
	EdgePartOf       EdgeType = 0x0B // hierarchical
	EdgeInstanceOf   EdgeType = 0x0C // type/instance
	EdgeCausedBy     EdgeType = 0x0D // causal
	EdgeObservedBy   EdgeType = 0x0E // observation provenance
)

// Valid reports whether t is one of the 14 canonical edges.
func (t EdgeType) Valid() bool {
	return t >= EdgeDerivedFrom && t <= EdgeObservedBy
}

// String returns the human-readable name. Used in CLI output and error
// messages; canonical CBOR uses the byte directly.
func (t EdgeType) String() string {
	switch t {
	case EdgeDerivedFrom:
		return "derived_from"
	case EdgeSupersedes:
		return "supersedes"
	case EdgeReferences:
		return "references"
	case EdgeContradicts:
		return "contradicts"
	case EdgeCorroborates:
		return "corroborates"
	case EdgeConsentsTo:
		return "consents_to"
	case EdgeDispatchedTo:
		return "dispatched_to"
	case EdgeAttestedBy:
		return "attested_by"
	case EdgeCitedIn:
		return "cited_in"
	case EdgeTombstones:
		return "tombstones"
	case EdgePartOf:
		return "part_of"
	case EdgeInstanceOf:
		return "instance_of"
	case EdgeCausedBy:
		return "caused_by"
	case EdgeObservedBy:
		return "observed_by"
	default:
		return "Unknown"
	}
}

// ParseEdgeType returns the EdgeType named s. Used by the CLI parser; the
// query engine and Cortex API consume bytes directly.
func ParseEdgeType(s string) (EdgeType, bool) {
	switch s {
	case "derived_from":
		return EdgeDerivedFrom, true
	case "supersedes":
		return EdgeSupersedes, true
	case "references":
		return EdgeReferences, true
	case "contradicts":
		return EdgeContradicts, true
	case "corroborates":
		return EdgeCorroborates, true
	case "consents_to":
		return EdgeConsentsTo, true
	case "dispatched_to":
		return EdgeDispatchedTo, true
	case "attested_by":
		return EdgeAttestedBy, true
	case "cited_in":
		return EdgeCitedIn, true
	case "tombstones":
		return EdgeTombstones, true
	case "part_of":
		return EdgePartOf, true
	case "instance_of":
		return EdgeInstanceOf, true
	case "caused_by":
		return EdgeCausedBy, true
	case "observed_by":
		return EdgeObservedBy, true
	}
	return 0, false
}

// EdgeRecord is the canonical CBOR value stored at BOTH e/from and e/to.
//
// Spec: §5 ("Edge record"). The Data field is reserved for edge-type-
// specific opaque CBOR (e.g. a `contradicts` record might carry which
// fields conflict). It is opaque to the cortex; the writer's skill is
// responsible for its shape. Empty in Phase 6 callers.
//
// Tombstoned semantics: RemoveEdge rewrites the record with Tombstoned=true
// rather than deleting the keys. Default traversal skips tombstoned edges
// (see query.EdgeExpr.IncludeTombstoned for opt-in audit reads).
type EdgeRecord struct {
	Type       EdgeType  `cbor:"1,keyasint"`
	Src        ID        `cbor:"2,keyasint"`
	Dst        ID        `cbor:"3,keyasint"`
	CreatedAt  time.Time `cbor:"4,keyasint"`
	CreatedBy  string    `cbor:"5,keyasint,omitempty"`
	Weight     float32   `cbor:"6,keyasint,omitempty"`
	Tombstoned bool      `cbor:"7,keyasint,omitempty"`
	// TombstonedAt and TombstonedReason carry the audit trail when
	// Tombstoned is true. Both are zero-valued for live edges.
	TombstonedAt     *time.Time `cbor:"8,keyasint,omitempty"`
	TombstonedReason string     `cbor:"9,keyasint,omitempty"`
	TombstonedBy     string     `cbor:"10,keyasint,omitempty"`
	Data             []byte     `cbor:"11,keyasint,omitempty"`
}

// EncodeEdge returns canonical CBOR of e.
func EncodeEdge(e *EdgeRecord) ([]byte, error) {
	if e == nil {
		return nil, errors.New("memory: nil EdgeRecord")
	}
	return canonicalEnc.Marshal(e)
}

// DecodeEdge parses canonical CBOR into out.
func DecodeEdge(b []byte, out *EdgeRecord) error {
	return canonicalDec.Unmarshal(b, out)
}

// Errors exported for AddEdge / RemoveEdge callers.
var (
	ErrInvalidEdgeType = errors.New("memory: invalid edge type")
	ErrSelfEdge        = errors.New("memory: self-edges are not permitted")
)

// Copyright © 2026 Paxlabs Inc. All rights reserved.
