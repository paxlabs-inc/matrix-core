// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Canonical CBOR encoding for memory records and per-type Data, plus the
// Hash computation used at write time (§4.3 step 5).
//
// Spec: research/04-cortex.md §4.3 (validation pipeline produces Hash).
//
// Hash domain: sha256("matrix.cortex.memory.v1" || Type byte || canonical CBOR
// of the body). Domain separation prevents collisions with the journal leaf
// hash and with future Merkle leaves over different namespaces (§7).

package memory

import (
	"crypto/sha256"
	"fmt"
	"math"

	"github.com/fxamacker/cbor/v2"
)

// HashDomain is prepended to canonical CBOR before SHA-256.
const HashDomain = "matrix.cortex.memory.v1"

var canonicalEnc cbor.EncMode
var canonicalDec cbor.DecMode

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("memory: build EncMode: %w", err))
	}
	canonicalEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("memory: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// EncodeData canonical-CBOR-encodes a typed Data struct. Returns the bytes
// to be stored in Version.Data.
func EncodeData(d TypedData) ([]byte, error) {
	if d == nil {
		return nil, ErrEmptyData
	}
	return canonicalEnc.Marshal(d)
}

// DecodeData decodes raw canonical CBOR bytes into the Go struct that
// matches the given Type. Returns a TypedData. Mirrors the type table in
// data.go.
func DecodeData(t Type, raw []byte) (TypedData, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyData
	}
	switch t {
	case TypeIdentity:
		var d IdentityData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypeFact:
		var d FactData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypePreference:
		var d PreferenceData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypeBelief:
		var d BeliefData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypeEvent:
		var d EventData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypeGoal:
		var d GoalData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypeConstraint:
		var d ConstraintData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypeCapability:
		var d CapabilityData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	case TypePattern:
		var d PatternData
		if err := canonicalDec.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil
	default:
		return nil, ErrUnknownDataKind
	}
}

// EncodeHead returns canonical CBOR of h.
func EncodeHead(h *Head) ([]byte, error) { return canonicalEnc.Marshal(h) }

// DecodeHead decodes canonical CBOR into h.
func DecodeHead(b []byte, h *Head) error { return canonicalDec.Unmarshal(b, h) }

// EncodeVersion returns canonical CBOR of v.
func EncodeVersion(v *Version) ([]byte, error) { return canonicalEnc.Marshal(v) }

// DecodeVersion decodes canonical CBOR into v.
func DecodeVersion(b []byte, v *Version) error { return canonicalDec.Unmarshal(b, v) }

// EncodeVectorMeta returns canonical CBOR of m. Used by the embedding
// worker when persisting to vec/meta/<id> (Phase 5).
func EncodeVectorMeta(m *VectorMeta) ([]byte, error) { return canonicalEnc.Marshal(m) }

// DecodeVectorMeta decodes canonical CBOR into m.
func DecodeVectorMeta(b []byte, m *VectorMeta) error { return canonicalDec.Unmarshal(b, m) }

// VectorHashDomain is prepended to vector bytes before SHA-256 so vector
// hashes can't collide with memory.Hash, journal LeafHash, or any future
// Merkle domain. Bump the version suffix on any change to the canonical
// vector byte layout.
const VectorHashDomain = "matrix.cortex.vector.v1"

// HashVector returns sha256(VectorHashDomain || big-endian float32
// components). Independent of CBOR map ordering so the hash is stable
// across encoder versions. Carried in EmbedPayload.VectorHash and
// VectorMeta.VectorHash for replay-time integrity checking (Phase 11).
func HashVector(v []float32) [32]byte {
	h := sha256.New()
	h.Write([]byte(VectorHashDomain))
	var buf [4]byte
	for _, x := range v {
		// math.Float32bits keeps the bit pattern; NaN payloads survive.
		// Big-endian so byte-stream is deterministic across architectures.
		bits := math.Float32bits(x)
		buf[0] = byte(bits >> 24)
		buf[1] = byte(bits >> 16)
		buf[2] = byte(bits >> 8)
		buf[3] = byte(bits)
		h.Write(buf[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// HashVersion computes the version's content hash. The hash inputs are:
//   - HashDomain string
//   - the Type byte
//   - the canonical CBOR of a "hashable view" of the Version: every field
//     EXCEPT Hash itself (otherwise we'd need to hash a struct that contains
//     its own hash — chicken-and-egg).
//
// We achieve "every field except Hash" by zeroing Hash before encode and
// restoring it after.
func HashVersion(v *Version) ([32]byte, error) {
	if v == nil {
		return [32]byte{}, fmt.Errorf("memory: HashVersion: nil version")
	}
	saved := v.Hash
	v.Hash = [32]byte{}
	enc, err := canonicalEnc.Marshal(v)
	v.Hash = saved
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	h.Write([]byte(HashDomain))
	h.Write([]byte{byte(v.Type)})
	h.Write(enc)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
