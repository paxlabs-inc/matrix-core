package enrich

import "errors"

var (
	errNoSummarizer = errors.New("enrich: no Summarizer configured")
	errSummaryCount = errors.New("enrich: summarizer returned wrong number of summaries")
	errNoEmbedder   = errors.New("enrich: no Embedder configured")
	errNoVectors    = errors.New("enrich: no VectorIndex configured")
	errEmbedCount   = errors.New("enrich: embedder returned wrong number of vectors")
	errEmbedDim     = errors.New("enrich: embedder returned a vector of wrong dimension")
)
