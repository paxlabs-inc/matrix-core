// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package scope is the cryptographic privacy boundary for cortex
// reads (and selected writes) issued by sub-agents on behalf of an
// actor. A Scope binds:
//
//   - actor identity (whose cortex)
//   - a pinned snapshot root (when the scope was issued)
//   - inclusion + exclusion selectors (what subset of memories may be
//     read)
//   - a multi-proof against the snapshot's memories namespace SMT (so
//     the sub-agent can verify its included keys against a Merkle root
//     it has independently checked, not against an API)
//   - granter + grantee agent refs
//   - an ed25519 signature by the granter over the unsigned bytes
//   - an expiry wall-clock and an optional budget cap
//   - a writable bit (default false; required for write-paths like
//     UpdateHead per research/04-cortex.md §10.3).
//
// Spec: research/06-agents.md §7 (the type, enforcement, "why merkle
// proofs not API trust"), research/04-cortex.md §7.5 (multi-proof
// shipping), §10 (visibility + scope enforcement choke point), §12
// (CortexScope shows up on Query and ResolveScoped).
//
// Design lock (Phase 10 Q1-Q8 in matrix.ctx phase10_locked_design):
//
//	Q1 wire format = canonical CBOR with SchemaVersion byte mixed in;
//	   mirrors snapshot.Manifest posture; HPS envelope is a tools/attest
//	   concern at the chain boundary (D4 keeps cortex chain-agnostic).
//
//	Q2 expires_at = wall-clock ISO-8601 per spec verbatim. Cortex takes
//	   `now` as a parameter so tests can drive deterministically; scope
//	   verification does NOT participate in replay determinism (the
//	   pinned snapshot_hash carries the state binding).
//
//	Q3 read API gating = single enforceScope choke point; Scope rides
//	   on Query / ContextOpts / ResolveScoped — NOT a wrapper type.
//
//	Q7 sub-agent UpdateHead requires Writable=true. Default-deny.
//
// Cortex never SIGNS a Scope (no key material at this layer per D4).
// Scope creation lives in the agent runtime / sub-dispatch executor;
// cortex only verifies. Sign / Encode helpers in this package are
// callable by the runtime and tests but they are NOT a cortex API
// surface.
package scope

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/memory"
	"matrix/cortex/snapshot"
)

// SchemaVersion is the wire-format schema for Scope. Bumped on any
// shape change that would alter encode bytes. Mixed into the canonical
// signed bytes so signatures from one schema cannot be replayed under
// another.
const SchemaVersion uint8 = 1

// Scope is the canonical CBOR value granted by a parent agent to a
// sub-agent. Field set follows research/06-agents.md §7.1 with the
// concrete additions documented in matrix.ctx phase10_locked_design.
//
// CBOR keys are integer keyasint (matches snapshot.Manifest +
// memory.Head). New fields land at unused integers; deletions are
// forbidden without a SchemaVersion bump.
type Scope struct {
	// SchemaVersion is mixed into the signed bytes so a schema bump
	// invalidates outstanding scopes.
	SchemaVersion uint8 `cbor:"0,keyasint"`

	// Actor names whose cortex this scope grants access into. Must
	// match the receiving cortex's store actor at enforcement time.
	Actor string `cbor:"1,keyasint"`

	// SnapshotHash pins the cortex_snapshot_hash (OverallRoot) at
	// scope creation. The verifier requires this snapshot to still be
	// resolvable on the receiving end (i.e. there exists a snap/<seq>
	// manifest with that OverallRoot).
	SnapshotHash [32]byte `cbor:"2,keyasint"`

	// Include selects what's allowed. Empty Include = nothing matches
	// = scope is empty (no reads allowed). Caller MUST populate at
	// least one criterion to grant access.
	Include Selector `cbor:"3,keyasint"`

	// Exclude is the belt-and-suspenders deny-list applied AFTER
	// Include matches. Empty Exclude = nothing extra denied.
	Exclude Selector `cbor:"4,keyasint,omitempty"`

	// Proofs is the multi-proof shipping the canonical Head bytes for
	// every memory id in Include.IDs against SnapshotHash's memories
	// SMT. The proof bundle MUST include exactly one proof per
	// Include.IDs entry, in matching order; the verifier cross-checks
	// each KeyHash against memory.ID-derived hashes. May be nil when
	// Include uses no IDs (Type/Tag/Frame-only scopes).
	Proofs *snapshot.MultiProof `cbor:"5,keyasint,omitempty"`

	// ExpiresAt is wall-clock per spec. Zero = never expires (rare;
	// production scopes always expire).
	ExpiresAt time.Time `cbor:"6,keyasint"`

	// BudgetTokens caps cortex.context budget for this scope. Zero =
	// no cap. Enforced at Context-call time.
	BudgetTokens int `cbor:"7,keyasint,omitempty"`

	// GrantedBy is the parent agent ref. Used by KeyResolver to look
	// up the public key for signature verification. Format mirrors
	// research/06-agents.md §7.1 (typically a DID like
	// "did:pax:0xabc..."). At v1 cortex treats it as opaque text.
	GrantedBy string `cbor:"8,keyasint"`

	// GrantedTo is the child agent ref. Carried for audit purposes;
	// cortex does not currently match the calling agent against this
	// field (no agent-identity authentication at the cortex layer).
	GrantedTo string `cbor:"9,keyasint"`

	// Writable gates non-read operations. Default false. Required for
	// UpdateHead/Write/Tombstone/AddEdge/RemoveEdge from a sub-agent.
	Writable bool `cbor:"10,keyasint,omitempty"`

	// Signature is the ed25519 sig by GrantedBy's pubkey over the
	// canonical CBOR encoding of the Scope with Signature=nil.
	// Ed25519 signatures are 64 bytes.
	Signature []byte `cbor:"11,keyasint"`
}

// Selector enumerates allow / deny criteria. A memory matches the
// selector if it satisfies ANY of the populated criteria (set union),
// EXCEPT empty selectors which match NOTHING.
//
// Empty-selector semantics is default-deny (the more secure default
// per research/06-agents.md §12 "deny by default, grant by signed
// scope"): to express "all types", caller passes the full type list
// rather than leaving Types empty. The primary-full template in §7.4
// is a literal enumeration of all 9 types.
type Selector struct {
	Types []memory.Type `cbor:"1,keyasint,omitempty"`
	Tags  []memory.Tag  `cbor:"2,keyasint,omitempty"`
	IDs   []memory.ID   `cbor:"3,keyasint,omitempty"`
	Frame *FrameFilter  `cbor:"4,keyasint,omitempty"`
}

// FrameFilter restricts the scope to memories whose Head.Frames
// includes a (Verb, ObjKind, ObjRef) tuple matching at least one of
// the (Verb, Objects[]) pairs encoded here. ObjKind is implied by the
// matching FrameRef on the memory side; the filter only pins Verb +
// ObjRef-derived ObjHash.
//
// At v1 we ship the simpler Verb + ObjHashes shape: the matcher walks
// h.Frames and matches when (frame.Verb == filter.Verb && frame.Hash()
// ∈ filter.ObjHashes). ObjKind is enforced upstream by the writer's
// FrameRef.Validate() so the caller can rely on the verb+ref tuple
// alone.
type FrameFilter struct {
	Verb      memory.Verb                `cbor:"1,keyasint"`
	ObjHashes [][memory.ObjHashSize]byte `cbor:"2,keyasint"`
}

// canonicalEnc + canonicalDec — same posture as snapshot/journal/memory.
var canonicalEnc cbor.EncMode
var canonicalDec cbor.DecMode

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("scope: build EncMode: %w", err))
	}
	canonicalEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("scope: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// EncodeScope returns canonical CBOR for s INCLUDING the Signature
// field. Used for wire transmission once the scope has been signed.
func EncodeScope(s *Scope) ([]byte, error) {
	if s == nil {
		return nil, errors.New("scope: nil Scope")
	}
	return canonicalEnc.Marshal(s)
}

// DecodeScope parses canonical CBOR into out. Out should be zero-valued
// before the call.
func DecodeScope(b []byte, out *Scope) error {
	return canonicalDec.Unmarshal(b, out)
}

// UnsignedBytes returns the canonical CBOR encoding of s with
// Signature explicitly cleared. This is the message that GrantedBy's
// ed25519 key signs (and that VerifySignature checks against).
//
// The returned bytes deliberately retain the SchemaVersion field so
// signature replays across schema versions are blocked.
func UnsignedBytes(s *Scope) ([]byte, error) {
	if s == nil {
		return nil, errors.New("scope: nil Scope")
	}
	c := *s
	c.Signature = nil
	return canonicalEnc.Marshal(&c)
}

// Sign sets s.Signature to the ed25519 signature of UnsignedBytes(s)
// under priv. Returns an error if the unsigned bytes cannot be
// produced. Used by tests + the agent runtime / CLI; cortex itself
// never calls this (no key material at the cortex layer per D4).
func Sign(s *Scope, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: bad private key length %d", ErrSignatureInvalid, len(priv))
	}
	msg, err := UnsignedBytes(s)
	if err != nil {
		return err
	}
	s.Signature = ed25519.Sign(priv, msg)
	return nil
}

// VerifySignature checks that s.Signature is a valid ed25519 signature
// from pub over UnsignedBytes(s). Returns ErrSignatureInvalid on any
// mismatch (length, key, message).
func VerifySignature(s *Scope, pub ed25519.PublicKey) error {
	if s == nil {
		return fmt.Errorf("%w: nil Scope", ErrSignatureInvalid)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key length %d", ErrSignatureInvalid, len(pub))
	}
	if len(s.Signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: bad signature length %d", ErrSignatureInvalid, len(s.Signature))
	}
	msg, err := UnsignedBytes(s)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, s.Signature) {
		return ErrSignatureInvalid
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
