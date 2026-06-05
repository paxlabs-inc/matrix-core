// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package envelope is the canonical wire codec for all 15 MCL message
// kinds. Every message Matrix sends rides inside an Envelope: typed
// header, opaque CBOR body, ed25519 signature over the canonical
// CBOR encoding with Signature cleared.
//
// Spec: research/02-protocol.md §4 (envelope shape), §5 (15 kinds),
// research/01-foundations.md §4.10 (journal/logs/ persistence).
//
// Encoding posture (locked Session 21 — matches cortex/scope/scope.go):
//
//   - Canonical CBOR via github.com/fxamacker/cbor/v2 CoreDetEncOptions.
//   - Integer keyasint tags on every field so adding a new field at an
//     unused tag is non-breaking.
//   - SchemaVersion mixed into UnsignedBytes so a schema bump invalidates
//     outstanding signatures (replay protection).
//   - On-wire: CBOR. On-disk: JSON (provided by EnvelopeJSON for
//     journal/logs/ readability per research/01 §4.10).
//
// The Body is held as cbor.RawMessage so a single round-trip preserves
// byte equality (required for replay determinism) and so the codec
// doesn't have to know about every body type at compile time. Callers
// use NewEnvelope + DecodeBody to round-trip typed body structs.
//
// Sign / Verify mirror cortex/scope/scope.go: callers provide a
// KeyResolver that maps the From principal (typically a DID) to an
// ed25519.PublicKey. Cortex does not sign or verify envelopes — that
// is agent-runtime / executor work.
package envelope

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// SchemaVersion is the wire-format schema for Envelope. Bumped on any
// shape change that would alter encode bytes. Mixed into the canonical
// signed bytes so signatures from one schema cannot be replayed under
// another.
const SchemaVersion uint8 = 1

// ProtocolVersion is the MCL protocol version string carried in the
// envelope header. Pinned to "mcl/0.1" per research/02-protocol.md §4.
const ProtocolVersion = "mcl/0.1"

// Envelope is the canonical message wrapper for all 15 MCL message kinds.
// Body is opaque CBOR bytes; the caller knows the kind (Kind field) and
// decodes Body into the matching typed struct via DecodeBody.
//
// CBOR field tags are integer keyasint. New fields land at the next
// unused integer; deletions require a SchemaVersion bump.
type Envelope struct {
	// SchemaVersion mixes into UnsignedBytes for replay protection.
	SchemaVersion uint8 `cbor:"0,keyasint"`

	// ProtocolVersion is the MCL spec version ("mcl/0.1").
	ProtocolVersion string `cbor:"1,keyasint"`

	// Kind discriminates the body. One of the Kind* constants below.
	Kind string `cbor:"2,keyasint"`

	// ID is a ULID identifying this specific message (26 chars).
	ID string `cbor:"3,keyasint"`

	// At is the wall-clock timestamp at envelope creation (ISO-8601).
	At string `cbor:"4,keyasint"`

	// From is the sender principal: matrix://agent/<did> or
	// matrix://user/<did>.
	From string `cbor:"5,keyasint"`

	// To is the recipient principal (optional; broadcast messages
	// like intent.attest may omit it for the chain-anchored leg).
	To string `cbor:"6,keyasint,omitempty"`

	// Intent is the matrix://intent/<id> URI this message belongs to.
	// Every message belongs to exactly one Intent (research/02 §2.2).
	Intent string `cbor:"7,keyasint"`

	// CorrelationID matches request → response pairs (e.g.
	// intent.clarify → intent.answer). Optional.
	CorrelationID string `cbor:"8,keyasint,omitempty"`

	// CausationID points to the message that directly caused this one,
	// for tracing across multi-hop flows. Optional.
	CausationID string `cbor:"9,keyasint,omitempty"`

	// Body holds the canonical CBOR encoding of the typed body for Kind.
	// Use NewEnvelope to populate from a typed struct; DecodeBody to
	// read back. Body MUST be canonical CBOR for signature stability.
	Body cbor.RawMessage `cbor:"10,keyasint"`

	// Signature is the ed25519 sig by From's pubkey over UnsignedBytes.
	// 64 bytes when populated; nil/empty before signing.
	Signature []byte `cbor:"11,keyasint,omitempty"`
}

// canonicalEnc / canonicalDec — same posture as cortex/scope.
var canonicalEnc cbor.EncMode
var canonicalDec cbor.DecMode

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("envelope: build EncMode: %w", err))
	}
	canonicalEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("envelope: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// Common errors.
var (
	ErrUnknownKind      = errors.New("envelope: unknown message kind")
	ErrSchemaVersion    = errors.New("envelope: SchemaVersion mismatch")
	ErrSignatureInvalid = errors.New("envelope: signature invalid")
	ErrSignatureMissing = errors.New("envelope: no signature present")
	ErrFromMissing      = errors.New("envelope: From principal required")
	ErrIntentMissing    = errors.New("envelope: Intent ref required")
	ErrIDMissing        = errors.New("envelope: ID required")
	ErrAtMissing        = errors.New("envelope: At timestamp required")
	ErrUnknownPrincipal = errors.New("envelope: KeyResolver does not know principal")
	ErrBodyTypeMismatch = errors.New("envelope: body type does not match Kind")
	ErrBodyEmpty        = errors.New("envelope: Body bytes empty")
	ErrSelfHashMismatch = errors.New("envelope: self-hash mismatch")
)

// NewEnvelope constructs an unsigned Envelope from a typed body struct.
// The kind argument MUST match the body's expected kind (validated against
// kindForBody). Body is canonical-CBOR-encoded into env.Body.
//
// The caller is responsible for populating ID, At, From, To, Intent,
// CorrelationID, CausationID before calling Sign.
func NewEnvelope(kind string, body interface{}) (*Envelope, error) {
	if !ValidKind(kind) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if err := checkBodyKind(kind, body); err != nil {
		return nil, err
	}

	raw, err := canonicalEnc.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("envelope: marshal body: %w", err)
	}

	return &Envelope{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: ProtocolVersion,
		Kind:            kind,
		Body:            raw,
	}, nil
}

// DecodeBody parses env.Body into out. out must be a pointer to the
// typed struct matching env.Kind. Mismatched out types are still
// decoded if the field tags align — DecodeBody does NOT enforce the
// expected Go type, only that env.Kind is a valid kind. Use ValidateBody
// for strict kind↔type matching.
func (env *Envelope) DecodeBody(out interface{}) error {
	if env == nil {
		return errors.New("envelope: nil Envelope")
	}
	if len(env.Body) == 0 {
		return ErrBodyEmpty
	}
	return canonicalDec.Unmarshal(env.Body, out)
}

// Encode returns canonical CBOR for the envelope INCLUDING Signature.
// This is the wire form.
func Encode(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, errors.New("envelope: nil Envelope")
	}
	return canonicalEnc.Marshal(env)
}

// Decode parses canonical CBOR wire bytes into out. out should be zero.
func Decode(b []byte, out *Envelope) error {
	if out == nil {
		return errors.New("envelope: nil out")
	}
	return canonicalDec.Unmarshal(b, out)
}

// UnsignedBytes returns the canonical CBOR encoding with Signature
// explicitly cleared. This is what From's ed25519 key signs (and what
// VerifySignature checks against).
//
// SchemaVersion is retained in the unsigned bytes so signature replays
// across schema versions are blocked.
func UnsignedBytes(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, errors.New("envelope: nil Envelope")
	}
	c := *env
	c.Signature = nil
	return canonicalEnc.Marshal(&c)
}

// Sign sets env.Signature to ed25519.Sign(priv, UnsignedBytes(env)).
// Cortex never calls this — signing lives in the agent runtime, the
// executor (for agent → user messages), and tooling that emits
// intent.draft / intent.accept from the user side.
func Sign(env *Envelope, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: bad private key length %d", ErrSignatureInvalid, len(priv))
	}
	if err := requireRequiredHeaderFields(env); err != nil {
		return err
	}
	msg, err := UnsignedBytes(env)
	if err != nil {
		return err
	}
	env.Signature = ed25519.Sign(priv, msg)
	return nil
}

// VerifySignature checks env.Signature against pub over UnsignedBytes(env).
// Returns ErrSignatureInvalid on any failure.
func VerifySignature(env *Envelope, pub ed25519.PublicKey) error {
	if env == nil {
		return fmt.Errorf("%w: nil Envelope", ErrSignatureInvalid)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key length %d", ErrSignatureInvalid, len(pub))
	}
	if len(env.Signature) != ed25519.SignatureSize {
		if len(env.Signature) == 0 {
			return ErrSignatureMissing
		}
		return fmt.Errorf("%w: bad signature length %d", ErrSignatureInvalid, len(env.Signature))
	}
	msg, err := UnsignedBytes(env)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, env.Signature) {
		return ErrSignatureInvalid
	}
	return nil
}

// Verify runs the full envelope verification chain:
//
//  1. SchemaVersion matches package constant.
//  2. Required header fields populated (Kind, ID, At, From, Intent).
//  3. Kind is in the closed set.
//  4. KeyResolver returns a pubkey for env.From.
//  5. Signature is valid ed25519 sig over UnsignedBytes.
//
// Body shape is NOT validated here (that is ValidateBody's job, called
// after the receiver has decoded into a typed struct). Verify only
// checks "this envelope was actually sent by env.From and hasn't been
// tampered with."
func Verify(env *Envelope, resolver KeyResolver) error {
	if env == nil {
		return errors.New("envelope: nil Envelope")
	}
	if env.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: got %d want %d", ErrSchemaVersion, env.SchemaVersion, SchemaVersion)
	}
	if err := requireRequiredHeaderFields(env); err != nil {
		return err
	}
	if !ValidKind(env.Kind) {
		return fmt.Errorf("%w: %q", ErrUnknownKind, env.Kind)
	}
	if resolver == nil {
		return errors.New("envelope: nil KeyResolver")
	}
	pub, err := resolver.ResolveKey(env.From)
	if err != nil {
		return err
	}
	return VerifySignature(env, pub)
}

// SelfHash returns the sha256 hex digest of UnsignedBytes(env). Used as
// a content-address for journal storage and as the input to Merkle
// anchoring (when an agent posts an attest envelope on-chain). Does NOT
// require the envelope to be signed.
func SelfHash(env *Envelope) (string, error) {
	b, err := UnsignedBytes(env)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func requireRequiredHeaderFields(env *Envelope) error {
	if env.ID == "" {
		return ErrIDMissing
	}
	if env.At == "" {
		return ErrAtMissing
	}
	if env.From == "" {
		return ErrFromMissing
	}
	if env.Intent == "" {
		return ErrIntentMissing
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
