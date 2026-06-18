// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
)

// snippetWith returns the first surfaced snippet whose text contains sub, or a
// zero Snippet when none match.
func snippetWith(snips []Snippet, sub string) (Snippet, bool) {
	for _, s := range snips {
		if strings.Contains(s.Text, sub) {
			return s, true
		}
	}
	return Snippet{}, false
}

// floatNear reports whether a and b are within tol of each other.
func floatNear(a, b, tol float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// A live EdgeSupersedes (new -> old) must drop the stale memory from the
// surfaced set while the superseding one survives — Neo sees the current
// version, not the one it corrects.
func TestRetrieveSupersessionFilter(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const stale = "the planner model is gpt-oss-120b"
	const fresh = "the planner model is deepseek-v4-pro"
	oldURI, err := p.RememberFact(ctx, stale)
	if err != nil || oldURI == "" {
		t.Fatalf("RememberFact stale: uri=%q err=%v", oldURI, err)
	}
	newURI, err := p.RememberFact(ctx, fresh)
	if err != nil || newURI == "" {
		t.Fatalf("RememberFact fresh: uri=%q err=%v", newURI, err)
	}
	drain(t, p)

	if err := p.relate(newURI, oldURI, RelationSupersedes, ""); err != nil {
		t.Fatalf("relate supersedes: %v", err)
	}

	snips, err := p.Retrieve(ctx, "")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if _, ok := snippetWith(snips, "deepseek-v4-pro"); !ok {
		t.Errorf("superseding fact should be surfaced; got %+v", snips)
	}
	if _, ok := snippetWith(snips, "gpt-oss-120b"); ok {
		t.Errorf("superseded fact must be filtered out; got %+v", snips)
	}
}

// Two surfaced memories joined by a live EdgeContradicts must BOTH carry the
// reconcile-first annotation so Neo doesn't silently trust one.
func TestRetrieveContradictionSurfacing(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const a = "the gateway listens on port 9090"
	const b = "the gateway listens on port 8443"
	aURI, err := p.RememberFact(ctx, a)
	if err != nil || aURI == "" {
		t.Fatalf("RememberFact a: %v", err)
	}
	bURI, err := p.RememberFact(ctx, b)
	if err != nil || bURI == "" {
		t.Fatalf("RememberFact b: %v", err)
	}
	drain(t, p)

	if err := p.relate(aURI, bURI, RelationContradicts, "port differs"); err != nil {
		t.Fatalf("relate contradicts: %v", err)
	}

	snips, err := p.Retrieve(ctx, "")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	sa, ok := snippetWith(snips, "9090")
	if !ok || sa.Note == "" {
		t.Errorf("contradicting fact A should carry a reconcile note; got %+v", sa)
	}
	sb, ok := snippetWith(snips, "8443")
	if !ok || sb.Note == "" {
		t.Errorf("contradicting fact B should carry a reconcile note; got %+v", sb)
	}
}

// A two-hop reference chain from a seed must surface both neighbors with the
// hop-decayed score: 0.7 at hop 1, 0.49 at hop 2.
func TestCascadeNeighborsHopDecay(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	aURI, _ := p.RememberFact(ctx, "fact A about the router")
	bURI, _ := p.RememberFact(ctx, "fact B about the executor")
	cURI, _ := p.RememberFact(ctx, "fact C about the compiler")
	if aURI == "" || bURI == "" || cURI == "" {
		t.Fatalf("seed writes failed: %q %q %q", aURI, bURI, cURI)
	}
	drain(t, p)

	if err := p.relate(aURI, bURI, RelationRelates, ""); err != nil {
		t.Fatalf("relate A->B: %v", err)
	}
	if err := p.relate(bURI, cURI, RelationRelates, ""); err != nil {
		t.Fatalf("relate B->C: %v", err)
	}

	ns := p.cascadeNeighbors(aURI)
	scores := map[string]float32{}
	for _, n := range ns {
		switch {
		case strings.Contains(n.snip.Text, "executor"):
			scores["B"] = n.score
		case strings.Contains(n.snip.Text, "compiler"):
			scores["C"] = n.score
		}
	}
	if s, ok := scores["B"]; !ok || !floatNear(s, 0.7, 0.001) {
		t.Errorf("hop-1 neighbor B score = %v, want ~0.7 (present=%v)", s, ok)
	}
	if s, ok := scores["C"]; !ok || !floatNear(s, 0.49, 0.001) {
		t.Errorf("hop-2 neighbor C score = %v, want ~0.49 (present=%v)", s, ok)
	}
}

// An exact duplicate must be skipped BEFORE the (costly) relation classifier
// is ever consulted — the cheap-model call is reserved for the relation band.
func TestDuplicateShortCircuitsBeforeClassifier(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const fact = "the dev box repo is at /root/matrix"
	if _, err := p.RememberFact(ctx, fact); err != nil {
		t.Fatalf("first RememberFact: %v", err)
	}
	drain(t, p)

	classify := func(context.Context, string, []Neighbor) (Relation, string, string) {
		t.Fatal("classifier must not run for an exact duplicate")
		return RelationNew, "", ""
	}
	uri, err := p.RememberFactRelated(ctx, fact, classify)
	if err != nil {
		t.Fatalf("RememberFactRelated dup: %v", err)
	}
	if uri != "" {
		t.Errorf("exact duplicate should be deduped (empty uri), got %q", uri)
	}
}

// ParseRelation is case-insensitive, defaults unknowns to the safe edge-free
// RelationNew, and each linking relation maps to its cortex edge byte.
func TestRelationParseAndEdgeMapping(t *testing.T) {
	cases := []struct {
		in   string
		want Relation
	}{
		{"supersedes", RelationSupersedes},
		{"  CONTRADICTS ", RelationContradicts},
		{"Relates", RelationRelates},
		{"duplicate", RelationDuplicate},
		{"new", RelationNew},
		{"nonsense", RelationNew},
		{"", RelationNew},
	}
	for _, c := range cases {
		if got := ParseRelation(c.in); got != c.want {
			t.Errorf("ParseRelation(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	if _, ok := RelationNew.edgeType(); ok {
		t.Error("RelationNew must not map to an edge")
	}
	if _, ok := RelationDuplicate.edgeType(); ok {
		t.Error("RelationDuplicate must not map to an edge")
	}
	for _, r := range []Relation{RelationSupersedes, RelationContradicts, RelationRelates} {
		if _, ok := r.edgeType(); !ok {
			t.Errorf("%q must map to a cortex edge", r)
		}
	}
}
