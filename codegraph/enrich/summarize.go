package enrich

import (
	"context"

	"matrix/codegraph/model"
)

// SummaryStats reports what a Summarize pass did.
type SummaryStats struct {
	Eligible  int // enrichable nodes considered
	Generated int // nodes summarized this pass (digest was stale)
	Reused    int // nodes whose cached summary was still valid (digest unchanged)
}

// summaryStale reports whether a node needs a fresh summary: it has no summary
// yet, or its cached summary was derived from a different digest (its body
// moved). A node whose summary_digest equals its current digest is reused
// (Requirement 8.2); a mismatch is treated as stale (Requirement 8.4).
func summaryStale(n *model.Node) bool {
	return n.Enrich.Summary == "" || n.Enrich.SummaryDigest != n.Digest
}

// Summarize generates one-line summaries for every enrichable node whose
// summary is stale, batching them through the Summarizer keyed on node digest,
// and reusing cached summaries for unchanged digests. It writes only
// Enrich.Summary and Enrich.SummaryDigest — never a structural field
// (Requirements 8.1, 8.2, 8.3, 8.4). Nodes are processed in id-sorted order so
// the pass is deterministic.
func (e *Enricher) Summarize(ctx context.Context, ix *model.Index) (SummaryStats, error) {
	if e.Summarizer == nil {
		return SummaryStats{}, errNoSummarizer
	}
	var stats SummaryStats
	var batch []*model.Node
	batchN := e.batchSize()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		reqs := make([]Request, len(batch))
		for i, n := range batch {
			reqs[i] = requestFor(n)
		}
		summaries, err := e.Summarizer.Summarize(ctx, reqs)
		if err != nil {
			return err
		}
		if len(summaries) != len(batch) {
			return errSummaryCount
		}
		for i, n := range batch {
			setSummary(n, summaries[i], n.Digest)
			stats.Generated++
		}
		batch = batch[:0]
		return nil
	}

	for _, n := range ix.Nodes() {
		if !Enrichable(n) {
			continue
		}
		stats.Eligible++
		if !summaryStale(n) {
			stats.Reused++
			continue
		}
		batch = append(batch, n)
		if len(batch) >= batchN {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	return stats, nil
}
