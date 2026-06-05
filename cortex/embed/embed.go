// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package embed defines the Embedder interface and a deterministic
// hash-based stub used by Phase 5 of the cortex.
//
// Production wiring of a real embedding model (lean: nomic-embed-text-v1.5
// via local llama.cpp / ollama HTTP, 768-dim Matryoshka) lives in a later
// phase alongside the executor model registry. Phase 5 ships the contract
// + a deterministic stub so the vector worker, HNSW index, and Find Near
// path can be implemented and tested hermetically.
//
// Spec: research/04-cortex.md §11.2 (async embedding), §13.1 (vector
// engine + per-actor model pin), §19 (model lean).
//
// Contract:
//   - Same (Embedder, text) → same []float32 bytes. Required for the
//     replay invariant: the embedding worker is deterministic, so dropping
//     indexes/vector/<actor>/ and re-running the journal produces a
//     byte-identical vec/meta namespace.
//   - len(out) == Dim() for every text the embedder accepts.
//   - The embedder MUST NOT panic on empty text or text exceeding any
//     model-specific max-token cap; it should either truncate-and-embed
//     deterministically or return an error.
//
// Model identifier shape: "<name>@<digest>" where the digest is a
// content-addressed fingerprint of the model weights (sha256 of the
// gguf/safetensors file is conventional). The Phase 5 HashEmbedder uses
// "hash-stub@<algorithm-digest>" so audit logs never confuse a real
// embedding for a test stub.
package embed

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

// Errors returned by Embedder implementations.
var (
	// ErrEmptyInput indicates an embedder rejected zero-length text. The
	// stub accepts empty strings (returns a zero-mean vector); HTTP-backed
	// embedders may differ.
	ErrEmptyInput = errors.New("embed: empty input")

	// ErrDimMismatch is returned when an embedder produces a vector whose
	// length does not equal the Dim() it advertises. Callers (the vector
	// worker, the Find Near path) MUST treat this as a hard error rather
	// than silently truncating, since it indicates either an embedder bug
	// or a model-version mismatch.
	ErrDimMismatch = errors.New("embed: produced vector dim does not match advertised Dim")
)

// Embedder maps text to a fixed-dimensional vector. Implementations must be
// safe for concurrent use; the worker fans out batches across goroutines.
type Embedder interface {
	// Embed returns the vector for text. The slice length equals Dim().
	// Implementations MUST be deterministic: two calls with identical
	// (this Embedder, text) return byte-identical slices.
	Embed(text string) ([]float32, error)

	// Dim returns the vector dimensionality advertised by this embedder.
	// Constant for the lifetime of the Embedder value.
	Dim() int

	// Model returns the canonical "<name>@<digest>" identifier persisted
	// alongside every vector. Embedding-model migrations key off this
	// string to detect stale vec/meta entries (§14.3).
	Model() string
}

// DefaultDim is the embedding dimensionality used by the Phase 5 stub. It
// matches the spec lean (nomic-embed-text-v1.5 at 768 dims) so swapping in
// a real embedder does not force a re-index. Lower-dim Matryoshka slices
// (e.g. 256) remain available by composing a custom Embedder.
const DefaultDim = 768

// HashEmbedder is a deterministic embedder that derives a pseudo-vector
// from sha256(text) seed material. It is NOT a real embedding model — the
// geometry of its output reflects sha256 chaos, not text semantics — so
// nearest-neighbour queries against it are only meaningful for tests where
// "identical text recalls identical text" is the property under test.
//
// Why this exists: lets every Phase 5 test (vector worker, HNSW index,
// Find Near integration) run hermetically with bit-stable outputs. When
// the real embedder lands, callers swap the implementation; nothing else
// changes.
//
// Construction:
//   - NewHashEmbedder() — DefaultDim (768), salt "" (matches "@v1" digest)
//   - NewHashEmbedderWith(dim, salt) — explicit dim and salt for testing
//     edge cases (small dim collapses, salted variants distinct, etc.)
type HashEmbedder struct {
	dim   int
	salt  string
	model string
}

// NewHashEmbedder returns a HashEmbedder at DefaultDim with no salt. Its
// Model() returns "hash-stub@v1".
func NewHashEmbedder() *HashEmbedder {
	return NewHashEmbedderWith(DefaultDim, "")
}

// NewHashEmbedderWith returns a HashEmbedder at the given dim and salt.
// Model() returns "hash-stub@<digest>" where the digest captures both dim
// and salt so two stubs configured differently are never confused at the
// audit boundary.
func NewHashEmbedderWith(dim int, salt string) *HashEmbedder {
	if dim <= 0 {
		dim = DefaultDim
	}
	h := sha256.New()
	h.Write([]byte("hash-stub.v1"))
	var d [8]byte
	binary.BigEndian.PutUint64(d[:], uint64(dim))
	h.Write(d[:])
	h.Write([]byte(salt))
	digest := h.Sum(nil)
	return &HashEmbedder{
		dim:   dim,
		salt:  salt,
		model: "hash-stub@" + hex8(digest),
	}
}

// hex8 renders the first 4 bytes of digest as 8 lowercase hex characters.
// Short enough to fit in audit logs, distinctive enough to spot
// salted/unsalted variants by eye.
func hex8(b []byte) string {
	const hexAlphabet = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i*2] = hexAlphabet[b[i]>>4]
		out[i*2+1] = hexAlphabet[b[i]&0x0f]
	}
	return string(out)
}

// Dim implements Embedder.
func (h *HashEmbedder) Dim() int { return h.dim }

// Model implements Embedder.
func (h *HashEmbedder) Model() string { return h.model }

// Embed returns a unit-normalized pseudo-vector derived from sha256 of the
// (salt, text) pair. Algorithm:
//
//  1. seed = sha256("hash-stub.v1" || salt || text)
//  2. Stream a sha256-based xorshift PRNG keyed by seed to produce dim
//     float32 components in [-1, 1].
//  3. L2-normalize the vector so dot-products match cosine similarity
//     (HNSW search and Find Near use dot-product as their distance proxy).
//
// Determinism: byte-identical for byte-identical (salt, text). No
// system-clock, no PRNG state outside the seeded chain.
//
// Empty-text handling: returns a deterministic vector (the seed expansion
// of just salt). This differs from production embedders but keeps tests
// clean — they can write a memory with empty rendered text without
// crashing the worker.
func (h *HashEmbedder) Embed(text string) ([]float32, error) {
	// Seed the PRNG chain with sha256(domain || salt || text).
	seedHasher := sha256.New()
	seedHasher.Write([]byte("hash-stub.v1"))
	seedHasher.Write([]byte(h.salt))
	seedHasher.Write([]byte(text))
	seed := seedHasher.Sum(nil)

	out := make([]float32, h.dim)
	// We pull 4-byte chunks out of a chained sha256 keystream: each block
	// produces 32 bytes = 8 floats. Chain step k := sha256(k-1) keeps the
	// chain deterministic and avoids storing 8 KiB of state for 768-dim.
	block := seed
	for i := 0; i < h.dim; {
		// Convert the current 32-byte block into up to 8 float32s.
		for j := 0; j < 8 && i < h.dim; j, i = j+1, i+1 {
			bits := binary.BigEndian.Uint32(block[j*4 : j*4+4])
			// Map uint32 → [-1, 1) via (bits / 2^31) - 1 keeping
			// determinism and a roughly uniform distribution.
			f := float32(float64(bits)/float64(1<<31) - 1)
			out[i] = f
		}
		if i >= h.dim {
			break
		}
		next := sha256.Sum256(block)
		block = next[:]
	}

	// L2 normalize so cosine and dot-product agree.
	var sumSq float64
	for _, x := range out {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		// Degenerate (won't happen for sha256 output, but guard for
		// safety so callers never see a NaN-laden vector). Return a
		// canonical unit vector along axis 0.
		out[0] = 1
		return out, nil
	}
	inv := float32(1.0 / math.Sqrt(sumSq))
	for i := range out {
		out[i] *= inv
	}

	return out, nil
}

// Cosine returns the cosine similarity of a and b. Both slices must be the
// same length; returns 0 on a length mismatch rather than erroring so
// callers in tight loops don't have to plumb errors. The HNSW index uses
// Cosine via the negative-cosine distance to keep min-heap semantics
// (lower distance = closer).
//
// Implementation note: a and b are expected to be unit-normalized (the
// HashEmbedder and the planned production embedder both normalize) so
// Cosine reduces to a plain dot product. We still divide by the norms to
// stay correct under non-normalized inputs.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		ax := float64(a[i])
		bx := float64(b[i])
		dot += ax * bx
		na += ax * ax
		nb += bx * bx
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(na*nb))
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
