// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package journal defines the append-only write log entry shape for the
// Matrix cortex, plus canonical encoding and Merkle leaf hashing.
//
// Spec: research/04-cortex.md §7.1 (journal Merkle accumulator) and §11.1
// (write batch atomicity: every store mutation appends one Entry).
//
// Invariants:
//   - Entries are encoded as canonical deterministic CBOR (RFC 8949 §4.2.1
//     core deterministic encoding, as implemented by fxamacker/cbor v2's
//     CoreDetEncOptions). Two parties encoding the same logical Entry MUST
//     produce byte-identical output.
//   - The leaf hash is sha256("matrix.cortex.journal.v1" || encoded). The
//     domain-separation prefix prevents cross-protocol collisions.
//   - Seq is per-actor monotonic and gap-free. Allocated by store.AllocSeq
//     inside the same Pebble batch as the journal write.
package journal

import (
	"crypto/sha256"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// LeafDomain is prepended to the canonical CBOR before hashing. Bumping the
// version string forces a clean re-anchor of all existing journals.
const LeafDomain = "matrix.cortex.journal.v1"

// Kind enumerates the journal operation types. Phase 1 only emits KindRaw;
// later phases will populate the rest. Kept as strings (not enums) so the
// journal stays self-describing under cbor diagnostics.
type Kind string

const (
	KindRaw        Kind = "raw"         // smoke-test / opaque payload, Phase 1
	KindWrite      Kind = "write"       // cortex.Write (Phase 2)
	KindUpdate     Kind = "update"      // cortex.Update via SlotPatch (Phase 2)
	KindTombstone  Kind = "tombstone"   // cortex.Tombstone (Phase 2)
	KindAddEdge    Kind = "add_edge"    // cortex.AddEdge (Phase 6)
	KindRemoveEdge Kind = "remove_edge" // cortex.RemoveEdge (Phase 6)
	KindGC         Kind = "gc"          // version GC sweep
	KindMigration  Kind = "migration"   // schema migration step
	// KindFind is appended ONLY when a Find call is flagged late_binding=true
	// (i.e. issued mid-execution rather than at compile time). Compile-time
	// Finds are not journaled; this is an audit hook for D13 anti-pattern
	// detection (research/04-cortex.md §12.3).
	KindFind Kind = "find_late"
	// KindEmbed is appended by the async embedding worker (Phase 5) when it
	// persists a freshly computed vector to vec/meta/<id> and updates the
	// corresponding MemoryHead.EmbeddingRef. The payload is a canonical
	// CBOR EmbedPayload (see below). Logged so the replay invariant covers
	// vec/meta: re-running the journal reproduces every embedding write
	// without re-embedding from scratch (the vector bytes live in the
	// vec/meta value rather than the journal payload to keep payloads
	// compact; replay re-embeds with the same deterministic Embedder).
	KindEmbed Kind = "embed"
	// KindCompact is appended by cortex.Compact (Phase 9) when it persists
	// a checkpoint record. Spec: research/03-retrieval-patterns.md §5.1
	// step 3 ("Emit a journal checkpoint") + §5.3 (the cortex.compact
	// primitive). The payload is a canonical CBOR CompactPayload (see
	// below); the full CheckpointRecord lives at chk/<intent>/<step>
	// and is integrity-bound to this entry via CheckpointHash. Same
	// shape rationale as KindEmbed: payload stays small (no full URI
	// list or compacted-stub vector), the heavy data lives at its own
	// content-addressable key, replay re-derives the heavy data from
	// the journal entry + cortex state at this seq.
	KindCompact Kind = "compact"
	// KindUpdateHead is appended by cortex.UpdateHead (Phase 10) when it
	// rewrites mutable Head fields (Tags, Frames, DeclaredImportance,
	// Visibility) on an existing memory WITHOUT bumping mv/<id>/v/n
	// (Phase 10 Q5 lock: Version is Data-versioned per research/04
	// §6.1; Head-only mutations don't create a new Data version).
	// The payload is a canonical CBOR UpdateHeadPayload (see below);
	// it carries the field diff (before+after for each mutated field)
	// so replay walks j/ forward, applies UpdateHeadPayload deltas to
	// the in-memory Head, and recomputes idx/tag + idx/frame +
	// idx/actor_obj keys deterministically.
	KindUpdateHead Kind = "update_head"
	// KindScopeViolation is appended whenever a sub-agent attempts to
	// access a memory outside its CortexScope, or to write under a
	// non-writable scope. Spec: research/06-agents.md §7.2 ("logged as
	// a j/ journal event with severity: high; repeated violations
	// from the same sub-agent trigger automatic dispatch revocation").
	// The payload is a canonical CBOR ScopeViolationPayload (see
	// below); the violating sub-agent's GrantedTo ref + the offending
	// memory id (or empty for write-no-scope-writable) + the kind of
	// violation are all captured for downstream auto-revocation.
	KindScopeViolation Kind = "scope_violation"
	// KindAttest is appended by cortex.Attest (Phase 11.5) when an
	// agent runtime calls back to record the outcome of an executed
	// intent. Spec: research/04-cortex.md §8.3 ("Two updates fire on
	// `intent.attest`"). The payload is a canonical CBOR AttestPayload
	// (see below); it carries the affected memory IDs so the replay
	// harness can re-apply salience.Citations bumps deterministically.
	// Citations and AccessCount on cited memories are mutated in the
	// same atomic batch as this journal entry. This is the cortex-
	// side primitive that the MCL `intent.attest` message kind (see
	// research/02-protocol.md §3 — kind 12 of 15) hands off to.
	KindAttest Kind = "attest"
	// KindLearnWeights is appended by cortex.Attest (Phase 12) immediately
	// after the matching KindAttest entry. It records one EMA step on the
	// per-actor salience weights (research/04-cortex.md §8.3 "EMA-update
	// the actor's weights toward / away from the high-performing
	// weighting"). Payload is a canonical CBOR LearnWeightsPayload (see
	// below) carrying both PrevW* and NewW* so the entry is
	// self-validating: a replay can reconstruct the same NewW* from the
	// SourceSeq's KindAttest entry + the salience.Score values at that
	// seq, and tampering with the persisted weights would diverge from
	// the journal-of-record.
	//
	// Atomic with the matching KindAttest: both entries land in one
	// store.BeginWrite batch (KindAttest at seq=N, KindLearnWeights at
	// seq=N+1). Replay determinism: rebuildSalienceFromJournal applies
	// the citation bumps from KindAttest first, then the weight update
	// from KindLearnWeights, in seq order. Phase 7 anchors the journal
	// MMR so both entries' leaves participate in OverallRoot.
	KindLearnWeights Kind = "learn_weights"
)

// WritePayload is the canonical shape carried by KindWrite and KindUpdate
// entries. Phase 2 carried the version hash as the raw Payload byte slice,
// which was sufficient for replay-by-bytes but didn't let downstream
// consumers (Phase 5 embedder) map an entry to its memory ID without a
// secondary index. The CBOR map shape carries everything needed for
// replay AND for the async worker to look up Head/Version by ID.
//
// Schema version is bumped if fields are added in a way that breaks
// decoders; new optional fields don't require a bump.
type WritePayload struct {
	SchemaVersion uint8    `cbor:"0,keyasint"`
	ID            [16]byte `cbor:"1,keyasint"`
	Version       uint64   `cbor:"2,keyasint"`
	Type          uint8    `cbor:"3,keyasint"`
	Hash          [32]byte `cbor:"4,keyasint"`
}

// EncodeWritePayload returns canonical deterministic CBOR for p.
func EncodeWritePayload(p *WritePayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil WritePayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeWritePayload parses canonical CBOR into out.
func DecodeWritePayload(b []byte, out *WritePayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// EmbedPayload is the canonical shape carried by KindEmbed entries. It is
// intentionally small (no full vector) so the journal stays a manageable
// size at 100k+ embeddings; the vector bytes live in vec/meta/<id> and the
// 32-byte VectorHash here is the integrity check used by the replay harness
// (Phase 11) to detect drift between recomputed and persisted vectors.
type EmbedPayload struct {
	SchemaVersion uint8    `cbor:"0,keyasint"`
	ID            [16]byte `cbor:"1,keyasint"` // memory.ID
	VertexID      uint64   `cbor:"2,keyasint"` // HNSW vertex id
	Model         string   `cbor:"3,keyasint"` // model identifier (name@digest)
	Dim           uint16   `cbor:"4,keyasint"` // vector dimensionality
	VectorHash    [32]byte `cbor:"5,keyasint"` // sha256 over canonical vector bytes
	SourceVersion uint64   `cbor:"6,keyasint"` // memory version that was embedded
}

// EncodeEmbedPayload returns canonical deterministic CBOR for p.
func EncodeEmbedPayload(p *EmbedPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil EmbedPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeEmbedPayload parses canonical CBOR into out.
func DecodeEmbedPayload(b []byte, out *EmbedPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// EdgePayload is the canonical shape carried by KindAddEdge and
// KindRemoveEdge entries (Phase 6). The full EdgeRecord lives in the
// store at e/from/<src>/<t>/<dst> and e/to/<dst>/<t>/<src>; this payload
// carries enough to replay the mutation deterministically and to audit
// "who edged what to what when" without a Pebble Get on the full record.
//
// Tombstoned distinguishes KindAddEdge (false) from KindRemoveEdge (true)
// at decode time so a reader inspecting only payloads doesn't have to
// also inspect Entry.Kind. Reason/By are populated on RemoveEdge only.
type EdgePayload struct {
	SchemaVersion uint8    `cbor:"0,keyasint"`
	Type          uint8    `cbor:"1,keyasint"`
	Src           [16]byte `cbor:"2,keyasint"`
	Dst           [16]byte `cbor:"3,keyasint"`
	Weight        float32  `cbor:"4,keyasint,omitempty"`
	Tombstoned    bool     `cbor:"5,keyasint,omitempty"`
	Reason        string   `cbor:"6,keyasint,omitempty"`
	By            string   `cbor:"7,keyasint,omitempty"`
}

// EncodeEdgePayload returns canonical deterministic CBOR for p.
func EncodeEdgePayload(p *EdgePayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil EdgePayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeEdgePayload parses canonical CBOR into out.
func DecodeEdgePayload(b []byte, out *EdgePayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// CompactPayload is the canonical shape carried by KindCompact entries
// (Phase 9). Spec: research/03-retrieval-patterns.md §5.1 step 3 + §5.3.
//
// IntentID + StepID together form the checkpoint identity (Pebble key
// chk/<intent>/<step>; filesystem mirror at journal/thoughts/<intent>/
// <step>.snapshot per Andrew lock A1). KeptCount + CompactedCount are
// audit denormalizations so a journal scan can summarize compactions
// without a Pebble Get on chk/. CheckpointHash is sha256 over the
// canonical CBOR encoding of the CheckpointRecord stored at chk/; same
// integrity discipline as EmbedPayload.VectorHash (the payload doesn't
// inline heavy data, but pins it cryptographically).
//
// BudgetTokens preserves the §5.3 budget_tokens parameter so replay /
// audit can verify the compaction was bounded at the value the caller
// asked for (caller can't claim "I asked for 8000" after the fact).
type CompactPayload struct {
	SchemaVersion  uint8    `cbor:"0,keyasint"`
	IntentID       string   `cbor:"1,keyasint"`
	StepID         string   `cbor:"2,keyasint"`
	BudgetTokens   uint32   `cbor:"3,keyasint"`
	KeptCount      uint32   `cbor:"4,keyasint"`
	CompactedCount uint32   `cbor:"5,keyasint"`
	CheckpointHash [32]byte `cbor:"6,keyasint"`
}

// EncodeCompactPayload returns canonical deterministic CBOR for p.
func EncodeCompactPayload(p *CompactPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil CompactPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeCompactPayload parses canonical CBOR into out.
func DecodeCompactPayload(b []byte, out *CompactPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// UpdateHeadPayload is the canonical shape carried by KindUpdateHead
// entries (Phase 10). Carries the post-write Head bytes so replay can
// reconstruct the new Head deterministically and re-derive the
// idx/tag, idx/frame, and idx/actor_obj key sets without needing to
// also load the prior Head from store/.
//
// HeadHash is sha256 over canonical CBOR Head bytes — a cross-check
// the replay harness uses to validate that a re-applied UpdateHead
// produced the same Head bytes the journal entry references. Mirrors
// EmbedPayload.VectorHash + CompactPayload.CheckpointHash discipline.
//
// Version is the unchanged Data version at the time of the
// UpdateHead. UpdateHead does NOT bump versions (Phase 10 Q5).
// Carrying it here lets replay verify "the Data version this
// UpdateHead targeted exists at mv/<id>/v/<version>".
type UpdateHeadPayload struct {
	SchemaVersion uint8    `cbor:"0,keyasint"`
	ID            [16]byte `cbor:"1,keyasint"`
	Version       uint64   `cbor:"2,keyasint"` // unchanged Data version
	HeadHash      [32]byte `cbor:"3,keyasint"` // sha256 over new canonical Head bytes
}

// EncodeUpdateHeadPayload returns canonical deterministic CBOR for p.
func EncodeUpdateHeadPayload(p *UpdateHeadPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil UpdateHeadPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeUpdateHeadPayload parses canonical CBOR into out.
func DecodeUpdateHeadPayload(b []byte, out *UpdateHeadPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// ScopeViolationPayload is the canonical shape carried by
// KindScopeViolation entries (Phase 10). Spec: research/06-agents.md
// §7.2 — "logged as a j/ journal event with severity: high; repeated
// violations from the same sub-agent trigger automatic dispatch
// revocation".
//
// At the cortex layer we don't currently auto-revoke (no DID
// resolution path); an agent runtime / tools/registry consumer counts
// these entries and decides to revoke. Cortex's job is just to make
// every violation visible in the canonical journal so it cannot be
// retroactively edited away.
//
// MemoryID is zero on violations that don't bind to a specific memory
// (e.g. UpdateHead under non-writable scope). Reason is a short
// machine-readable code (see scope.ErrViolation / scope.ErrNotWritable
// / scope.ErrActorMismatch — the cortex enforce path picks the
// appropriate code).
type ScopeViolationPayload struct {
	SchemaVersion uint8    `cbor:"0,keyasint"`
	GrantedTo     string   `cbor:"1,keyasint,omitempty"` // sub-agent ref from CortexScope.GrantedTo
	GrantedBy     string   `cbor:"2,keyasint,omitempty"` // parent agent ref
	MemoryID      [16]byte `cbor:"3,keyasint,omitempty"` // zero if N/A
	Reason        string   `cbor:"4,keyasint"`           // short code, e.g. "violation" / "not_writable"
	Mode          string   `cbor:"5,keyasint,omitempty"` // "read" | "write"
}

// EncodeScopeViolationPayload returns canonical deterministic CBOR for p.
func EncodeScopeViolationPayload(p *ScopeViolationPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil ScopeViolationPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeScopeViolationPayload parses canonical CBOR into out.
func DecodeScopeViolationPayload(b []byte, out *ScopeViolationPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// AttestOutcome enumerates the success/failure semantics carried by an
// AttestPayload. Closed enum (research/04-cortex.md §8.3 only defines
// two outcome branches: success and failure-with-reason).
type AttestOutcome uint8

const (
	AttestOutcomeSuccess AttestOutcome = 0
	AttestOutcomeFailure AttestOutcome = 1
)

// AttestReason* are the closed set of failure reasons that, per
// research/04-cortex.md §8.3, trigger decrement of cite_in_successful_plans
// on the cited memories. Any other reason (or empty) on a failure attest
// leaves Citations unchanged — the attest is logged for audit but no
// salience-side decrement fires.
const (
	AttestReasonFactualError    = "factual_error"
	AttestReasonWrongAssumption = "wrong_assumption"
)

// AttestPayload is the canonical shape carried by KindAttest entries
// (Phase 11.5). Spec: research/04-cortex.md §8.3.
//
// IntentID identifies the intent whose attestation produced this entry
// (matches MCL intent.attest message kind, research/02 §3). CitedIDs are
// the memory IDs referenced in the plan that this attest covers; the
// AttestPayload acts as the journal-level record of which salience cache
// entries were bumped (success) or decremented (failure-with-reason).
//
// Outcome+Reason determine the salience side-effect at write-and-replay
// time. We do NOT inline the (post-bump) salience values here — those
// live in salience/<id>; this payload only carries the inputs so the
// replay harness can reconstruct the same bumps from the same starting
// state. Cap on CitedIDs is enforced by the cortex.Attest caller, not
// the encoder, so this struct can also represent oversized attestations
// for diagnostic decoding.
type AttestPayload struct {
	SchemaVersion uint8         `cbor:"0,keyasint"`
	IntentID      string        `cbor:"1,keyasint"`
	Outcome       AttestOutcome `cbor:"2,keyasint"`
	Reason        string        `cbor:"3,keyasint,omitempty"`
	CitedIDs      [][16]byte    `cbor:"4,keyasint,omitempty"`
}

// EncodeAttestPayload returns canonical deterministic CBOR for p.
func EncodeAttestPayload(p *AttestPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil AttestPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeAttestPayload parses canonical CBOR into out.
func DecodeAttestPayload(b []byte, out *AttestPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// LearnWeightsPayload is the canonical shape carried by KindLearnWeights
// entries (Phase 12). Spec: research/04-cortex.md §8.3 (EMA weight
// learning).
//
// SourceSeq is the seq of the KindAttest entry that produced this update;
// always exactly SourceSeq+1 == this entry's seq (one EMA step per
// attest). DecrementOnFailure mirrors the attest-side flag for whether
// the EMA was a toward-pull (false; success or non-decrement-reason
// failure) or an away-pull (true; failure with reason ∈ §8.3 decrement
// set). Skipped is true when the EMA update was a no-op (degenerate
// profile or empty cited set); when true, the NewW* fields equal the
// PrevW* fields and Alpha is still stamped for audit.
//
// Prev/New weights are inlined as float32 fields (not a nested struct) so
// canonical CBOR encoding is byte-stable and a journal dump shows the
// full state transition in one record.
type LearnWeightsPayload struct {
	SchemaVersion      uint8   `cbor:"0,keyasint"`
	SourceSeq          uint64  `cbor:"1,keyasint"`
	Alpha              float32 `cbor:"2,keyasint"`
	DecrementOnFailure bool    `cbor:"3,keyasint,omitempty"`
	Skipped            bool    `cbor:"4,keyasint,omitempty"`
	PrevWR             float32 `cbor:"5,keyasint"`
	PrevWA             float32 `cbor:"6,keyasint"`
	PrevWC             float32 `cbor:"7,keyasint"`
	PrevWD             float32 `cbor:"8,keyasint"`
	PrevWV             float32 `cbor:"9,keyasint"`
	NewWR              float32 `cbor:"10,keyasint"`
	NewWA              float32 `cbor:"11,keyasint"`
	NewWC              float32 `cbor:"12,keyasint"`
	NewWD              float32 `cbor:"13,keyasint"`
	NewWV              float32 `cbor:"14,keyasint"`
}

// EncodeLearnWeightsPayload returns canonical deterministic CBOR for p.
func EncodeLearnWeightsPayload(p *LearnWeightsPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil LearnWeightsPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeLearnWeightsPayload parses canonical CBOR into out.
func DecodeLearnWeightsPayload(b []byte, out *LearnWeightsPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// LateBindingPayload is the canonical shape carried by KindFind entries
// (Phase 3 introduced the kind; Phase 11.5 extended the payload with
// AccessedIDs so the replay harness can re-apply salience.AccessCount
// bumps deterministically).
//
// LateBinding=true Finds are the only path that journals; compile-time
// Finds (issued during pre-resolution per D13) do NOT emit a payload at
// all. The audit posture matches research/04-cortex.md §12.3 anti-pattern
// detection: a journal scan can flag actors whose compile-time-to-late-
// binding ratio is unusual.
//
// Predicate is the canonical String() form of the Where AST (audit-only,
// not used by replay). Types is the byte-encoded memory.Type list. Limit
// and Tags mirror Query input. ResultCount is the post-trim returned
// count. AccessedIDs is the per-candidate memory IDs whose salience
// AccessCount was bumped in the same atomic batch as this journal entry;
// len(AccessedIDs) <= ResultCount because BudgetTokens trim can drop
// candidates from the rendered set before they're counted as accessed.
type LateBindingPayload struct {
	SchemaVersion uint8      `cbor:"0,keyasint"`
	Predicate     string     `cbor:"1,keyasint,omitempty"`
	Types         []byte     `cbor:"2,keyasint,omitempty"`
	Limit         int        `cbor:"3,keyasint,omitempty"`
	ResultCount   int        `cbor:"4,keyasint"`
	Tags          []string   `cbor:"5,keyasint,omitempty"`
	AccessedIDs   [][16]byte `cbor:"6,keyasint,omitempty"`
}

// EncodeLateBindingPayload returns canonical deterministic CBOR for p.
func EncodeLateBindingPayload(p *LateBindingPayload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("journal: nil LateBindingPayload")
	}
	return canonicalEnc.Marshal(p)
}

// DecodeLateBindingPayload parses canonical CBOR into out.
func DecodeLateBindingPayload(b []byte, out *LateBindingPayload) error {
	return canonicalDec.Unmarshal(b, out)
}

// Entry is one record in the per-actor append-only log.
//
// Field order in the CBOR map is fixed by the cbor library's canonical mode
// (keys sorted lexicographically by their encoded form). Struct tags use
// short keys to keep entries compact.
type Entry struct {
	Seq       uint64 `cbor:"1,keyasint"`
	Kind      Kind   `cbor:"2,keyasint"`
	CreatedAt int64  `cbor:"3,keyasint"` // unix nanoseconds
	CreatedBy []byte `cbor:"4,keyasint,omitempty"`
	Payload   []byte `cbor:"5,keyasint,omitempty"`
}

// canonicalEnc is the package-level canonical CBOR encoder.
var canonicalEnc cbor.EncMode

// canonicalDec is the matching decoder. Default decoder is fine; we keep it
// scoped so we can tune it later (e.g. max-array-size limits).
var canonicalDec cbor.DecMode

func init() {
	opts := cbor.CoreDetEncOptions() // RFC 8949 §4.2.1 deterministic
	em, err := opts.EncMode()
	if err != nil {
		panic(fmt.Errorf("journal: build EncMode: %w", err))
	}
	canonicalEnc = em

	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("journal: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// Encode returns the canonical deterministic CBOR encoding of e.
func (e *Entry) Encode() ([]byte, error) {
	if e.Kind == "" {
		return nil, fmt.Errorf("journal: Entry.Kind required")
	}
	return canonicalEnc.Marshal(e)
}

// Decode parses canonical CBOR into out.
func Decode(b []byte, out *Entry) error {
	return canonicalDec.Unmarshal(b, out)
}

// LeafHash returns sha256(LeafDomain || encoded). Inputs to this function are
// expected to be already canonical-encoded Entry bytes.
func LeafHash(encoded []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(LeafDomain))
	h.Write(encoded)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
