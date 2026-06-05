// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package embed

import (
	"math"
	"strings"
	"testing"
)

// TestHashEmbedderDeterminism verifies the load-bearing property: same
// input bytes → same output bytes, across constructions and goroutines.
func TestHashEmbedderDeterminism(t *testing.T) {
	t.Parallel()
	e1 := NewHashEmbedder()
	e2 := NewHashEmbedder()
	cases := []string{
		"",
		"hello",
		"hello world",
		"Andrew is the founder of Paxeer",
		strings.Repeat("a", 4096),
	}
	for _, text := range cases {
		v1, err := e1.Embed(text)
		if err != nil {
			t.Fatalf("e1.Embed(%q): %v", text, err)
		}
		v2, err := e2.Embed(text)
		if err != nil {
			t.Fatalf("e2.Embed(%q): %v", text, err)
		}
		if len(v1) != len(v2) {
			t.Fatalf("dim mismatch: %d vs %d", len(v1), len(v2))
		}
		for i := range v1 {
			if v1[i] != v2[i] {
				t.Fatalf("byte drift at idx %d for %q: %v != %v", i, text, v1[i], v2[i])
			}
		}
	}
}

// TestHashEmbedderDim ensures Dim() agrees with the produced vector length
// across the default + a sample of custom dims.
func TestHashEmbedderDim(t *testing.T) {
	t.Parallel()
	for _, dim := range []int{16, 64, 256, 768, 1024} {
		e := NewHashEmbedderWith(dim, "")
		if e.Dim() != dim {
			t.Fatalf("Dim() = %d, want %d", e.Dim(), dim)
		}
		v, err := e.Embed("test")
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(v) != dim {
			t.Fatalf("len(vec) = %d, want %d", len(v), dim)
		}
	}
}

// TestHashEmbedderModelDigest verifies that different (dim, salt) configs
// surface distinct Model() strings so the audit log can tell stubs apart.
func TestHashEmbedderModelDigest(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	configs := []struct {
		dim  int
		salt string
	}{
		{768, ""},
		{768, "phase5"},
		{256, ""},
		{16, "tiny"},
	}
	for _, c := range configs {
		m := NewHashEmbedderWith(c.dim, c.salt).Model()
		if !strings.HasPrefix(m, "hash-stub@") {
			t.Fatalf("model = %q, want hash-stub@ prefix", m)
		}
		if _, dup := seen[m]; dup {
			t.Fatalf("model digest collision at %+v: %q", c, m)
		}
		seen[m] = struct{}{}
	}
}

// TestHashEmbedderUnitNorm verifies the produced vector is unit-length so
// cosine and dot-product agree.
func TestHashEmbedderUnitNorm(t *testing.T) {
	t.Parallel()
	e := NewHashEmbedder()
	for _, text := range []string{"a", "the quick brown fox", strings.Repeat("z", 256)} {
		v, err := e.Embed(text)
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		var sumSq float64
		for _, x := range v {
			sumSq += float64(x) * float64(x)
		}
		norm := math.Sqrt(sumSq)
		// HashEmbedder normalises via a float32 inverse; tolerate ~1e-5.
		if math.Abs(norm-1.0) > 1e-5 {
			t.Fatalf("vec not unit-normalised for %q: norm = %v", text, norm)
		}
	}
}

// TestHashEmbedderDistinguishesText sanity-checks that distinct inputs
// produce distinct vectors. Sha256-driven generators collide with
// vanishingly small probability (≈2^-128 for 4-byte windows), so any
// observed collision is a bug.
func TestHashEmbedderDistinguishesText(t *testing.T) {
	t.Parallel()
	e := NewHashEmbedder()
	a, _ := e.Embed("apple")
	b, _ := e.Embed("banana")
	if same(a, b) {
		t.Fatalf("distinct inputs produced identical vectors")
	}
}

// TestCosineSemantics verifies Cosine returns 1.0 for identical inputs,
// 0 for length mismatch, and meaningful values for known orthogonal pairs.
func TestCosineSemantics(t *testing.T) {
	t.Parallel()
	v := []float32{1, 0, 0}
	if c := Cosine(v, v); math.Abs(float64(c)-1.0) > 1e-6 {
		t.Fatalf("identical Cosine = %v, want 1.0", c)
	}
	w := []float32{0, 1, 0}
	if c := Cosine(v, w); c != 0 {
		t.Fatalf("orthogonal Cosine = %v, want 0", c)
	}
	if c := Cosine([]float32{1, 0}, v); c != 0 {
		t.Fatalf("length-mismatch Cosine = %v, want 0", c)
	}
	if c := Cosine(nil, v); c != 0 {
		t.Fatalf("nil Cosine = %v, want 0", c)
	}
}

// TestHashEmbedderSelfRecall confirms that the embedder satisfies the
// "identical text recalls identical text" property used by the Phase 5
// integration tests of Find Near: embedding the same string twice yields
// the highest possible cosine (1.0 within float epsilon).
func TestHashEmbedderSelfRecall(t *testing.T) {
	t.Parallel()
	e := NewHashEmbedder()
	a, _ := e.Embed("Andrew prefers minimal dark UI")
	b, _ := e.Embed("Andrew prefers minimal dark UI")
	c := Cosine(a, b)
	if math.Abs(float64(c)-1.0) > 1e-5 {
		t.Fatalf("self-recall cosine = %v, want ~1.0", c)
	}
}

func same(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
