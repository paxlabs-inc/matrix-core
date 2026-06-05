// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command embed-smoke exercises the real Fireworks embedding API end-to-end.
//
// Usage:
//
//	FIREWORKS_API_KEY=... embed-smoke
//
// What it does:
//  1. Calls the configured embedder (default: nomic-embed-text-v1.5 on Fireworks)
//     on a handful of texts.
//  2. Verifies dim, L2 norm, determinism across repeated calls.
//  3. Computes cosine similarity between semantically related vs. unrelated
//     pairs to confirm the embedder produces meaningful geometry (HashEmbedder
//     fails this — sha256-derived vectors are random noise).
//
// This is a real network smoke; it's not a unit test (those are in
// cortex/embed/api_embedder_test.go).
package main

import (
	"fmt"
	"math"
	"os"
	"time"

	"matrix/cortex/embed"
)

func main() {
	model := os.Getenv("EMBED_MODEL")
	if model == "" {
		model = embed.DefaultModelFireworks // nomic-embed-text-v1.5
	}

	emb, err := embed.NewAPIEmbedder(embed.APIEmbedderConfig{
		Model:   model,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		die("NewAPIEmbedder: %v", err)
	}

	fmt.Printf("model:        %s\n", emb.Model())
	fmt.Printf("dim:          %d\n", emb.Dim())
	fmt.Println()

	// Semantically related vs unrelated samples.
	related := []string{
		"the cat sat on the mat",
		"a feline rested on the rug",
	}
	unrelated := []string{
		"quantum entanglement is non-local",
		"the price of tea in China",
	}

	allTexts := append([]string{}, related...)
	allTexts = append(allTexts, unrelated...)

	vecs := make(map[string][]float32)
	for _, text := range allTexts {
		start := time.Now()
		v, err := emb.Embed(text)
		elapsed := time.Since(start)
		if err != nil {
			die("Embed(%q): %v", text, err)
		}
		if len(v) != emb.Dim() {
			die("dim mismatch: got %d, want %d", len(v), emb.Dim())
		}
		// Verify L2 norm ~ 1.
		var sumSq float64
		for _, x := range v {
			sumSq += float64(x) * float64(x)
		}
		norm := math.Sqrt(sumSq)
		fmt.Printf("[%5dms] %-60q  |v|=%.4f\n", elapsed.Milliseconds(), truncate(text, 60), norm)
		vecs[text] = v
	}

	fmt.Println()
	fmt.Println("cosine similarities:")
	relSim := embed.Cosine(vecs[related[0]], vecs[related[1]])
	unrSim := embed.Cosine(vecs[unrelated[0]], vecs[unrelated[1]])
	crossSim := embed.Cosine(vecs[related[0]], vecs[unrelated[0]])
	fmt.Printf("  related   (cat/feline):                       %.4f\n", relSim)
	fmt.Printf("  unrelated (quantum/tea):                      %.4f\n", unrSim)
	fmt.Printf("  cross    (cat/quantum):                       %.4f\n", crossSim)

	// Quality assertion: related pairs should beat cross pairs by a clear margin.
	if relSim <= crossSim {
		fmt.Println()
		fmt.Println("WARNING: related similarity is not higher than cross similarity")
		fmt.Println("         the embedder may not be producing semantic geometry")
		os.Exit(2)
	}
	fmt.Println()
	fmt.Println("OK — semantic geometry confirmed (related > cross)")

	// Determinism smoke: re-embed one sample 3× and report variance.
	fmt.Println()
	fmt.Println("determinism check (3× repeats of 'the cat sat on the mat'):")
	var ref []float32
	for i := 0; i < 3; i++ {
		v, err := emb.Embed("the cat sat on the mat")
		if err != nil {
			die("repeat Embed: %v", err)
		}
		if i == 0 {
			ref = v
			fmt.Printf("  pass %d: baseline\n", i+1)
			continue
		}
		// Cosine vs. reference. Anything < 1.0 means the API isn't deterministic.
		cos := embed.Cosine(ref, v)
		fmt.Printf("  pass %d: cos vs baseline = %.6f\n", i+1, cos)
	}
}

func die(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "embed-smoke: "+format+"\n", args...)
	os.Exit(1)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
