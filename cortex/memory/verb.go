// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Verb and ObjKind closed enums + FrameRef, used by Phase 8's cold-start
// composer (cortex.context). Spec:
//
//   - research/04-cortex.md §12.1 "Pinned/Frame-relevant/Outcomes tiers"
//     and the idx/frame + idx/actor_obj key shapes in §2.3.
//   - research/03-retrieval-patterns.md §2.1, §2.4 (call shape with
//     `verb` + `objects: {kind: ref}`).
//   - knowledge/matrix.ctx D7 (verb vocab CLOSED, exactly 10 verbs).
//
// Design discipline: both Verb and ObjKind are closed 1-byte enums. The
// spec pins `<verb:1>` and `<obj_kind:1>` byte widths in the idx/frame
// key shape, and D7 closes the verb vocabulary. ObjKind is closed at v1
// for parity (open-vocab would deviate from the literal spec width
// without an offsetting use case — phase-1 retrieval always queries by
// (verb, kind, ref) together, never by kind alone). Extending either
// vocabulary is a journaled migration (§14.1), not a code-only change.

package memory

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// Verb is the 1-byte verb discriminator from D7. Closed at 10 values.
//
// Spec: knowledge/matrix.ctx
//
//	D7 | verb vocab=CLOSED, 10 verbs:
//	     find acquire build modify deliver analyze negotiate schedule monitor delegate
//
// Order locked here so any future re-ordering breaks the build (it would
// also change idx/frame and idx/actor_obj byte keys, invalidating both
// snapshot roots and any persisted index — must go through a migration).
type Verb byte

const (
	VerbFind      Verb = 0x01
	VerbAcquire   Verb = 0x02
	VerbBuild     Verb = 0x03
	VerbModify    Verb = 0x04
	VerbDeliver   Verb = 0x05
	VerbAnalyze   Verb = 0x06
	VerbNegotiate Verb = 0x07
	VerbSchedule  Verb = 0x08
	VerbMonitor   Verb = 0x09
	VerbDelegate  Verb = 0x0A
)

// Valid reports whether v is one of the ten canonical verbs. Zero value
// (Verb(0)) is invalid by design — it doubles as a "no verb supplied"
// sentinel in the cortex.context composer (skips Frame/Outcomes tiers).
func (v Verb) Valid() bool { return v >= VerbFind && v <= VerbDelegate }

// String returns the lower-case verb name used in API surfaces and CLI
// flags. Matches the D7 spelling exactly.
func (v Verb) String() string {
	switch v {
	case VerbFind:
		return "find"
	case VerbAcquire:
		return "acquire"
	case VerbBuild:
		return "build"
	case VerbModify:
		return "modify"
	case VerbDeliver:
		return "deliver"
	case VerbAnalyze:
		return "analyze"
	case VerbNegotiate:
		return "negotiate"
	case VerbSchedule:
		return "schedule"
	case VerbMonitor:
		return "monitor"
	case VerbDelegate:
		return "delegate"
	}
	return "unknown"
}

// ParseVerb returns the Verb for name (lower-case D7 spelling). Returns
// the zero Verb and false on no match — callers can choose to treat that
// as either an error or "no verb supplied".
func ParseVerb(name string) (Verb, bool) {
	switch name {
	case "find":
		return VerbFind, true
	case "acquire":
		return VerbAcquire, true
	case "build":
		return VerbBuild, true
	case "modify":
		return VerbModify, true
	case "deliver":
		return VerbDeliver, true
	case "analyze":
		return VerbAnalyze, true
	case "negotiate":
		return VerbNegotiate, true
	case "schedule":
		return VerbSchedule, true
	case "monitor":
		return VerbMonitor, true
	case "delegate":
		return VerbDelegate, true
	}
	return 0, false
}

// ObjKind is the 1-byte object-kind discriminator inside idx/frame keys.
// Closed at v1 with the eight kinds the protocol layer references.
//
// Sources for the chosen kinds:
//   - "service" + "model": research/03-retrieval-patterns.md §2.4 + §9
//     ("objects: { service: gpu_inference, model: llama-405b }").
//   - "agent": research/02-protocol.md (matrix://agent/... refs are
//     central to dispatch/delegate semantics).
//   - "knowledge": knowledge/matrix.ctx layout — matrix://knowledge/* is
//     a top-level URI namespace.
//   - "intent": research/02-protocol.md INTENT_IR (intents are first-
//     class addressable objects in the protocol).
//   - "asset": Paxeer chain context (PAX + ERC20s are typed assets).
//   - "plan": research/02-protocol.md plan tree refs.
//   - "capability": memory.TypeCapability + research/04-cortex.md §4.2
//     CapabilityData (cortex itself models capabilities of subjects).
//
// Adding a kind is a journaled migration (§14.1). Both the enum value
// AND the lower-case string name participate in key bytes (the string
// is for parsing/debug; only the byte goes into idx/frame keys).
type ObjKind byte

const (
	KindService    ObjKind = 0x01
	KindModel      ObjKind = 0x02
	KindAgent      ObjKind = 0x03
	KindKnowledge  ObjKind = 0x04
	KindIntent     ObjKind = 0x05
	KindAsset      ObjKind = 0x06
	KindPlan       ObjKind = 0x07
	KindCapability ObjKind = 0x08
)

// Valid reports whether k is a registered ObjKind. Zero is invalid.
func (k ObjKind) Valid() bool { return k >= KindService && k <= KindCapability }

// String returns the lower-case kind name.
func (k ObjKind) String() string {
	switch k {
	case KindService:
		return "service"
	case KindModel:
		return "model"
	case KindAgent:
		return "agent"
	case KindKnowledge:
		return "knowledge"
	case KindIntent:
		return "intent"
	case KindAsset:
		return "asset"
	case KindPlan:
		return "plan"
	case KindCapability:
		return "capability"
	}
	return "unknown"
}

// ParseObjKind returns the ObjKind for name.
func ParseObjKind(name string) (ObjKind, bool) {
	switch name {
	case "service":
		return KindService, true
	case "model":
		return KindModel, true
	case "agent":
		return KindAgent, true
	case "knowledge":
		return KindKnowledge, true
	case "intent":
		return KindIntent, true
	case "asset":
		return KindAsset, true
	case "plan":
		return KindPlan, true
	case "capability":
		return KindCapability, true
	}
	return 0, false
}

// MaxObjRefLen caps the FrameRef.ObjRef string length so the canonical
// CBOR encoding of Head.Frames stays bounded and the (post-hash) idx
// key generation is finite-time. 256 chars accommodates full URIs
// (matrix://cortex/Event/<26-char-ulid>#<ver> ≈ 50 chars) with comfort
// for namespaced refs like "matrix://knowledge/models/llama-405b@digest".
const MaxObjRefLen = 256

// MaxFramesPerMemory caps Head.Frames cardinality to keep per-write
// idx/frame and idx/actor_obj fan-out bounded. Mirrors the
// MaxTagsPerMemory rationale (§4.3).
const MaxFramesPerMemory = 16

// ObjHashSize is the byte length of obj_id components in idx/frame and
// idx/actor_obj keys. Spec §2.3 pins 16 bytes ("<obj_id:16>").
const ObjHashSize = 16

// ObjHash returns sha256(ref)[:16] — the canonical 16-byte obj_id used
// in idx/frame and idx/actor_obj keys. Deterministic; the same ref
// string always hashes to the same obj_id across actors and processes,
// load-bearing for replay (drop indexes → walk j/ → re-derive identical
// idx/* keys from Head.Frames).
//
// Collision risk: 16 bytes = 128 bits. Birthday bound ≈ 2^64 distinct
// refs before a collision is likely — orders of magnitude above any
// realistic per-actor frame cardinality.
func ObjHash(ref string) [ObjHashSize]byte {
	sum := sha256.Sum256([]byte(ref))
	var out [ObjHashSize]byte
	copy(out[:], sum[:ObjHashSize])
	return out
}

// FrameRef is a single skill-authored "this memory is relevant for the
// (verb, kind, ref) frame" annotation. Stored as part of Head.Frames so
// canonical Head bytes carry frame relevance — replay reconstructs
// idx/frame and idx/actor_obj entries purely from Head.Frames at Write
// time, without consulting the journal payload.
//
// CBOR ordering (1=Verb, 2=ObjKind, 3=ObjRef) matches the writeMeta-on-
// head convention used elsewhere in this package.
type FrameRef struct {
	Verb    Verb    `cbor:"1,keyasint"`
	ObjKind ObjKind `cbor:"2,keyasint"`
	ObjRef  string  `cbor:"3,keyasint"`
}

// Hash returns the 16-byte obj_id derived from f.ObjRef. Shorthand for
// ObjHash(f.ObjRef); kept as a method so call sites read declaratively.
func (f FrameRef) Hash() [ObjHashSize]byte { return ObjHash(f.ObjRef) }

// Validate enforces the Phase 8 invariants for a single FrameRef:
//   - Verb in the closed D7 set
//   - ObjKind in the closed v1 set
//   - ObjRef non-empty and ≤ MaxObjRefLen
//
// Returns one of the ErrInvalidVerb / ErrInvalidObjKind / ErrEmptyObjRef
// sentinels so callers can switch on the error class.
func (f FrameRef) Validate() error {
	if !f.Verb.Valid() {
		return fmt.Errorf("%w: %d", ErrInvalidVerb, f.Verb)
	}
	if !f.ObjKind.Valid() {
		return fmt.Errorf("%w: %d", ErrInvalidObjKind, f.ObjKind)
	}
	if f.ObjRef == "" {
		return ErrEmptyObjRef
	}
	if len(f.ObjRef) > MaxObjRefLen {
		return fmt.Errorf("memory: FrameRef.ObjRef exceeds %d chars", MaxObjRefLen)
	}
	return nil
}

// Errors raised by FrameRef validation. Exported so the cortex package
// and tests can errors.Is against them.
var (
	ErrInvalidVerb    = errors.New("memory: invalid verb")
	ErrInvalidObjKind = errors.New("memory: invalid obj kind")
	ErrEmptyObjRef    = errors.New("memory: empty obj ref")
	ErrTooManyFrames  = errors.New("memory: frame count exceeds MaxFramesPerMemory")
)

// Copyright © 2026 Paxlabs Inc. All rights reserved.
