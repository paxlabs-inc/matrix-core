// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package memory defines typed memory records, schema validation, and
// canonical encoding for the Matrix cortex.
//
// Spec: research/04-cortex.md §3 (taxonomy), §4 (schema), §6 (versioning),
// §9 (forms), §10 (visibility).
//
// Phase 2 scope: types, encoders, validators. The Write/Update/Tombstone
// APIs that consume these live at the cortex top-level package and use
// store.BeginWrite for atomic batch composition.
package memory

import (
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
)

// Type is the 1-byte memory type discriminator (§3, "Type code" column).
// Higher bits reserved for future expansion.
type Type byte

const (
	TypeIdentity   Type = 0x01
	TypeFact       Type = 0x02
	TypePreference Type = 0x03
	TypeBelief     Type = 0x04
	TypeEvent      Type = 0x05
	TypeGoal       Type = 0x06
	TypeConstraint Type = 0x07
	TypeCapability Type = 0x08
	TypePattern    Type = 0x09
)

// String returns the human-readable name. Used in URIs and error messages.
func (t Type) String() string {
	switch t {
	case TypeIdentity:
		return "Identity"
	case TypeFact:
		return "Fact"
	case TypePreference:
		return "Preference"
	case TypeBelief:
		return "Belief"
	case TypeEvent:
		return "Event"
	case TypeGoal:
		return "Goal"
	case TypeConstraint:
		return "Constraint"
	case TypeCapability:
		return "Capability"
	case TypePattern:
		return "Pattern"
	default:
		return "Unknown"
	}
}

// Valid reports whether t is one of the nine canonical types.
func (t Type) Valid() bool { return t >= TypeIdentity && t <= TypePattern }

// Visibility levels (§10.1). Default for new memories is VisPrivate.
type Visibility byte

const (
	VisPrivate     Visibility = 1
	VisScoped      Visibility = 2
	VisActorPublic Visibility = 3
)

func (v Visibility) Valid() bool {
	return v == VisPrivate || v == VisScoped || v == VisActorPublic
}

// SourceKind tags how a memory entered the cortex (§4).
type SourceKind string

const (
	SourceUserInput SourceKind = "user_input"
	SourceDerived   SourceKind = "derived"
	SourceObserved  SourceKind = "observed"
	SourceImported  SourceKind = "imported"
)

func (s SourceKind) Valid() bool {
	switch s {
	case SourceUserInput, SourceDerived, SourceObserved, SourceImported:
		return true
	}
	return false
}

// ID is a 16-byte binary ULID. Mirrors keys.ULID. Textual rendering uses
// Crockford-base32 via oklog/ulid; binary in keys (§2.1).
type ID [16]byte

// NewID returns a random ULID with the current wallclock timestamp. ULIDs
// are 80 bits of entropy + 48 bits of millis, so collisions are negligible.
func NewID() ID {
	u := ulid.Make()
	var out ID
	copy(out[:], u[:])
	return out
}

// String returns the textual Crockford-base32 ULID.
func (i ID) String() string {
	var u ulid.ULID
	copy(u[:], i[:])
	return u.String()
}

// ParseID parses a Crockford-base32 ULID string into an ID.
func ParseID(s string) (ID, error) {
	u, err := ulid.Parse(s)
	if err != nil {
		return ID{}, err
	}
	var out ID
	copy(out[:], u[:])
	return out, nil
}

// IsZero reports whether i is the zero ID.
func (i ID) IsZero() bool {
	for _, b := range i {
		if b != 0 {
			return false
		}
	}
	return true
}

// Tag is a bounded-length actor-meaningful label.
type Tag string

// MaxTagLen caps tag string length so PutLPString in keys never overflows.
const MaxTagLen = 64

// Forms are the three render granularities per §9. `Full` is the canonical
// render of typed Data and is never overridden — only Short and Medium can
// be supplied via FormsOverride.
type Forms struct {
	Short  string `cbor:"1,keyasint"`
	Medium string `cbor:"2,keyasint"`
}

// MaxShortTokens / MaxMediumTokens are the budgets enforced at write time
// (§9.3).
//
// Token counting is the bytes/4 heuristic: tokens ≈ ceil(len(utf8_bytes)/4).
// This is deterministic (zero deps, snapshot-hash-stable), undercounts a real
// BPE tokenizer by ~10–20% for English (so we leave a small safety margin
// inside the cap), and overcounts for CJK (so CJK gets truncated tighter,
// acceptable for v1). Switching to a proper BPE tokenizer would require
// pinning the merges-file digest in models/ to keep cortex_snapshot_hash
// stable; deferred until the executor model lands. See
// research/04-cortex.md §9.3.
const (
	MaxShortTokens  = 50
	MaxMediumTokens = 200

	// BytesPerToken is the divisor used by countTokens. Exposed for the
	// forms package so its truncator agrees with validate.go.
	BytesPerToken = 4
)

// Tombstone marks a memory as soft-deleted (§D16, §11).
type Tombstone struct {
	Reason string    `cbor:"1,keyasint"`
	At     time.Time `cbor:"2,keyasint"`
	By     string    `cbor:"3,keyasint"`
}

// Provenance attests where a memory came from and who signed for it.
//
// SignedBy/SignedAt/Sig are optional in Phase 2 (skill/agent signing pipeline
// lands in Phase 6+). When absent, the cortex still records who CreatedBy.
type Provenance struct {
	Source       SourceKind `cbor:"1,keyasint"`
	DerivedFrom  []string   `cbor:"2,keyasint,omitempty"` // matrix://cortex/... URIs
	Attestations []string   `cbor:"3,keyasint,omitempty"`
	SignedBy     []byte     `cbor:"4,keyasint,omitempty"`
	SignedAt     *time.Time `cbor:"5,keyasint,omitempty"`
	Sig          []byte     `cbor:"6,keyasint,omitempty"`
}

// Head is the small, frequently-read primary record stored at m/<id>.
// Mutates on every new version, tag change, tombstone, or pin.
//
// Spec: §4.1.
type Head struct {
	ID                 ID         `cbor:"1,keyasint"`
	Type               Type       `cbor:"2,keyasint"`
	CurrentVersion     uint64     `cbor:"3,keyasint"`
	ActorScope         string     `cbor:"4,keyasint"` // actor name; DID once tools/registry lands
	Visibility         Visibility `cbor:"5,keyasint"`
	DeclaredImportance uint8      `cbor:"6,keyasint"`
	Tags               []Tag      `cbor:"7,keyasint,omitempty"`
	Tombstoned         *Tombstone `cbor:"8,keyasint,omitempty"`
	LastUpdatedAt      time.Time  `cbor:"9,keyasint"`
	// EmbeddingRef is filled async by the embedding worker (Phase 5). Phase 2
	// callers leave it nil.
	EmbeddingRef *VectorRef `cbor:"10,keyasint,omitempty"`
	// Forms mirrors the latest Version.Forms so Find/list paths can render
	// short/medium without a second Pebble Get per result. See
	// research/04-cortex.md §9; FormsOverride flag stays on Version because
	// only the per-version provenance needs to know whether the bytes were
	// caller-supplied or auto-rendered.
	Forms Forms `cbor:"11,keyasint"`
	// Frames are skill-authored "this memory is relevant for (verb, kind,
	// ref)" annotations consumed by Phase 8's cold-start composer
	// (cortex.context). Each FrameRef stamps idx/frame at Write time, and
	// for h.Type == TypeEvent ALSO stamps idx/actor_obj (the outcomes
	// index). Tags-like immutability across Update in Phase 8: changing
	// frames requires UpdateHead (Phase 10). Spec: research/04-cortex.md
	// §12.1 (deferred from Phase 3 per spec_divergence in matrix.ctx).
	Frames []FrameRef `cbor:"12,keyasint,omitempty"`
}

// VectorRef points into the vec/ namespace; opaque to Phase 2 callers.
// Lives on Head so a Find result can decide "embedded?" without fetching
// vec/meta. The full vector bytes live in VectorMeta at vec/meta/<id>.
type VectorRef struct {
	VertexID uint64 `cbor:"1,keyasint"`
	Model    string `cbor:"2,keyasint"`
	Dim      uint16 `cbor:"3,keyasint"`
	Stale    bool   `cbor:"4,keyasint,omitempty"`
}

// VectorMeta is the per-memory entry stored at vec/meta/<id:16>. Carries
// the full embedding vector (as float32 components) so the HNSW graph file
// under indexes/vector/<actor>/ can be dropped and rebuilt purely from
// Pebble without re-running the (potentially network-bound) embedder.
//
// Spec: research/04-cortex.md §2.3 ("vec/meta/  VectorMeta  Memory ↔ HNSW
// vertex mapping") and §13.1 ("Per-vertex metadata (memory_id ↔ vertex_id
// mapping, version, embedding model) lives in Pebble at vec/meta/<id>").
//
// Determinism: same (Memory text, Model, Dim) → same Vector bytes when the
// Embedder is itself deterministic (the Phase 5 HashEmbedder satisfies
// this; production HTTP-bound embedders are deterministic in practice).
// Vector is stored as a slice of float32 components in the CBOR map so it
// decodes without manual byte-arithmetic on the consumer side.
type VectorMeta struct {
	VertexID      uint64    `cbor:"1,keyasint"`
	Model         string    `cbor:"2,keyasint"`
	Dim           uint16    `cbor:"3,keyasint"`
	Vector        []float32 `cbor:"4,keyasint"`
	SourceVersion uint64    `cbor:"5,keyasint"`
	EmbeddedAt    time.Time `cbor:"6,keyasint"`
	// VectorHash mirrors the journaled EmbedPayload.VectorHash for the
	// replay harness; redundant with len(Vector)*4 bytes hashed but cheap
	// to carry and lets a single Pebble Get answer "is this vec/meta in
	// sync with its journal entry?".
	VectorHash [32]byte `cbor:"7,keyasint"`
}

// Version is the immutable per-version payload stored at mv/<id>/v/<n>.
//
// Spec: §4.1, §6.
//
// Data is the canonical CBOR encoding of one of the type-specific Data
// structs declared in data.go. Storing it pre-encoded keeps Hash stable
// across decode/re-encode cycles and avoids reflection at read time.
type Version struct {
	ID            ID         `cbor:"1,keyasint"`
	Version       uint64     `cbor:"2,keyasint"`
	Type          Type       `cbor:"3,keyasint"`
	Data          []byte     `cbor:"4,keyasint"` // canonical CBOR of typed Data
	CreatedAt     time.Time  `cbor:"5,keyasint"`
	CreatedBy     string     `cbor:"6,keyasint"`
	ExpiresAt     *time.Time `cbor:"7,keyasint,omitempty"`
	Confidence    float32    `cbor:"8,keyasint"`
	Provenance    Provenance `cbor:"9,keyasint"`
	Forms         Forms      `cbor:"10,keyasint"`
	FormsOverride bool       `cbor:"11,keyasint,omitempty"` // true if Forms.Short/Medium were skill-supplied
	Hash          [32]byte   `cbor:"12,keyasint"`           // SHA-256 over canonical body
}

// Memory is the ergonomic combined view used by callers of cortex.Write and
// returned by cortex.Resolve. It is NOT the on-disk shape; Head and Version
// are stored separately under m/<id> and mv/<id>/v/<n>.
type Memory struct {
	Head    Head
	Version Version
}

// URI is the canonical pointer matrix://cortex/<type>/<id>#<version>.
type URI string

// Errors used across writes / reads / validation.
var (
	ErrInvalidType       = errors.New("memory: invalid type")
	ErrInvalidVisibility = errors.New("memory: invalid visibility")
	ErrInvalidSource     = errors.New("memory: invalid source kind")
	ErrEmptyData         = errors.New("memory: data is empty")
	ErrTagTooLong        = errors.New("memory: tag exceeds 64 chars")
	ErrTooManyTags       = errors.New("memory: tag count exceeds 16")
	ErrFormTooLong       = errors.New("memory: form exceeds token budget")
	ErrTypeDataMismatch  = errors.New("memory: data type does not match memory type")
	ErrNotFound          = errors.New("memory: not found")
	ErrTombstoned        = errors.New("memory: memory is tombstoned")
	ErrVersionConflict   = errors.New("memory: version conflict")
	ErrBadURI            = errors.New("memory: malformed URI")
)

// MaxTagsPerMemory caps tag count to keep keys/scans bounded.
const MaxTagsPerMemory = 16

// Copyright © 2026 Paxlabs Inc. All rights reserved.
