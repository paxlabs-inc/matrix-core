package enrich

import (
	"context"
	"testing"
)

// TestSummarize_CachesOnDigest proves the summary pass generates for every
// enrichable node once, reuses on an unchanged re-run, and regenerates only the
// node whose digest moved (Requirements 8.2, 8.4).
func TestSummarize_CachesOnDigest(t *testing.T) {
	ix := sampleGraph()
	sum := &FakeSummarizer{}
	e := &Enricher{Summarizer: sum}
	ctx := context.Background()

	// First pass: every enrichable node summarized, none reused.
	st, err := e.Summarize(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if st.Eligible != 3 || st.Generated != 3 || st.Reused != 0 {
		t.Fatalf("first pass stats = %+v, want eligible=3 generated=3 reused=0", st)
	}
	if sum.Calls != 3 {
		t.Fatalf("summarizer calls = %d, want 3", sum.Calls)
	}
	for _, id := range enrichableIDs() {
		n := ix.Node(id)
		if n.Enrich.Summary == "" {
			t.Fatalf("%s has no summary", id)
		}
		if n.Enrich.SummaryDigest != n.Digest {
			t.Fatalf("%s summary_digest %q != digest %q", id, n.Enrich.SummaryDigest, n.Digest)
		}
	}
	// The package node is not enrichable and must not be summarized.
	if ix.Node("p").Enrich.Summary != "" {
		t.Fatal("package node was summarized")
	}

	// Second pass over the unchanged graph: everything reused, zero new calls.
	sum.Calls = 0
	st, err = e.Summarize(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if st.Generated != 0 || st.Reused != 3 {
		t.Fatalf("no-change pass stats = %+v, want generated=0 reused=3", st)
	}
	if sum.Calls != 0 {
		t.Fatalf("summarizer called %d times on unchanged graph, want 0", sum.Calls)
	}

	// Move p.A's digest (body edit): only p.A re-summarizes.
	a := ix.Node("p.A")
	a.Digest = "b3:a2"
	sum.Calls = 0
	st, err = e.Summarize(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if st.Generated != 1 || st.Reused != 2 {
		t.Fatalf("post-edit stats = %+v, want generated=1 reused=2", st)
	}
	if sum.Calls != 1 {
		t.Fatalf("summarizer called %d times after single edit, want 1", sum.Calls)
	}
	if a.Enrich.SummaryDigest != "b3:a2" {
		t.Fatalf("p.A summary_digest not refreshed: %q", a.Enrich.SummaryDigest)
	}
}

func TestSummarize_NoSummarizer(t *testing.T) {
	e := &Enricher{}
	if _, err := e.Summarize(context.Background(), sampleGraph()); err == nil {
		t.Fatal("expected error with no summarizer")
	}
}
