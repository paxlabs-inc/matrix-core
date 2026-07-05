package enrich

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"lukechampine.com/blake3"
)

// FakeSummarizer is a deterministic Summarizer for tests: it derives a stable
// one-line summary purely from each request's structural fields, so a digest-
// keyed cache and the enrichment-non-perturbation property are testable without
// a live model. It is a real interface implementation, not a canned-answer stub:
// distinct inputs yield distinct outputs.
type FakeSummarizer struct {
	// Calls counts how many requests were actually summarized, so tests can
	// assert the cache skipped unchanged nodes.
	Calls int
}

func (f *FakeSummarizer) Summarize(_ context.Context, reqs []Request) ([]string, error) {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		f.Calls++
		desc := firstLine(r.Doc)
		if desc == "" {
			desc = r.Sig
		}
		desc = oneLine(desc)
		if desc == "" {
			out[i] = fmt.Sprintf("%s %s", r.Kind, r.Name)
		} else {
			out[i] = fmt.Sprintf("%s %s: %s", r.Kind, r.Name, desc)
		}
	}
	return out, nil
}

// FakeEmbedder is a deterministic Embedder for tests: each text maps to a stable
// L2-normalized vector derived from its blake3 hash, so identical text embeds
// identically and cosine similarity is meaningful across runs.
type FakeEmbedder struct {
	// Dimension is the vector width; 0 defaults to 32.
	Dimension int
	// Calls counts embedded texts, so tests can assert caching.
	Calls int
}

func (f *FakeEmbedder) Dim() int {
	if f.Dimension > 0 {
		return f.Dimension
	}
	return 32
}

func (f *FakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dim := f.Dim()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		f.Calls++
		out[i] = deterministicVector(t, dim)
	}
	return out, nil
}

// deterministicVector expands blake3(text) into a dim-wide unit vector. Bytes
// are drawn from a counter-extended hash stream so any dimension is supported.
func deterministicVector(text string, dim int) []float32 {
	vec := make([]float32, dim)
	var norm float64
	for i := 0; i < dim; i++ {
		var buf [12]byte
		copy(buf[:], "cg")
		binary.LittleEndian.PutUint32(buf[8:], uint32(i))
		h := blake3.New(8, nil)
		h.Write([]byte(text))
		h.Write(buf[:])
		var sum [8]byte
		h.Sum(sum[:0])
		u := binary.LittleEndian.Uint64(sum[:])
		// Map to [-1, 1).
		v := float64(u)/float64(math.MaxUint64)*2 - 1
		vec[i] = float32(v)
		norm += v * v
	}
	if norm == 0 {
		return vec
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
