# 07 — Discovery (Search)

The promise: *"describe what you need in plain language and get the right service
back"* — not a catalog to dig through. Discovery blends **semantic similarity**,
**structured filters**, and **quality** into one ranked result.

## 7.1 Inputs

A discovery request has up to three parts (all optional but at least one
required):
- **`query`** — free text (the plain-language need).
- **`filters`** — structured constraints: `kind`, `max_price_wei`,
  `min_quality`, `min_uptime_bps`, `confidential`, `tags`.
- **`operation_shape`** — optional desired input/output schema hint (for agents
  that know the I/O they need).

## 7.2 Indexing pipeline (write path)

On listing create/update:
1. Build a **search document** from the manifest: `display_name + summary +
   description + tags + operation names + input/output field names`.
2. Compute an **embedding** of that document via the embedder
   (`cortex/embed` Fireworks client, or the Matrix gateway embedding route).
   Store in `embeddings.vec` (pgvector, HNSW, cosine).
3. Maintain a Postgres **full-text** index (`tsvector`) over the same document
   for lexical fallback and exact keyword matches.
4. Update structured columns (`kind`, price range, `quality_score`,
   `uptime_bps`) for filterable ranking.

## 7.3 Query pipeline (read path)

```text
parse request
  ├─ extract hard constraints from query when present
  │   ("under 0.001 PAX" -> max_price_wei; ">99% uptime" -> min_uptime_bps)
  │   via a small constraint extractor (regex + optional LLM normalizer)
  ├─ merge extracted + explicit filters (explicit wins)
embed(query) -> qvec                      (skip if no query: filter-only browse)
candidate retrieval:
  ├─ vector KNN: top-K by cosine(qvec, embeddings.vec) WHERE filters
  └─ lexical:    top-K by ts_rank WHERE filters       (union, dedup)
rank: blended score (7.4)
return top-N with per-result component scores
```

The constraint extractor turns natural phrasing into filters so
`"weather API with high uptime under 0.001 PAX/call"` becomes
`{ semantic: "weather api", filters: { max_price_wei, min_uptime_bps } }`.
Keep the LLM optional: regex handles common money/percent/latency phrasings;
the LLM normalizer is a fallback and is **never** on the critical path for
filter-only queries.

## 7.4 Ranking

Blended score per candidate (weights in `configs/ranking.yaml`, tunable):

```
score = w_sem * semantic_sim          // cosine, 0..1
      + w_qual * quality_norm          // quality_score / 1e18, 0..1
      + w_up   * uptime_bps/10000      // 0..1
      + w_price* price_affinity        // 1 when within budget, decays above
      + w_fresh* recency_decay         // small nudge for actively-used services
      - penalties                      // delisted/paused excluded; flapping uptime penalized
```

Defaults favor **delivery quality** so reliable services win visibility (this is
the explicit replacement for a comment section). Semantic relevance gates the
candidate set; quality/uptime order it.

## 7.5 Agent-optimized responses

When the caller is an agent (`Accept: application/json` + agent bearer), results
include everything needed to **act without a second round-trip**:
- `operations[]` with `price_wei`, `unit`, `input_schema` reference.
- `quote_hint` (indicative price for 1 unit).
- `quality_score`, `uptime_bps`, `cold_start_ms` hint.
- `invoke_url` (`/v1/invoke/{id}`) and `quote_url`.

So a planner can: discover → pick highest blended score within budget → quote →
invoke, in one pass.

## 7.6 Anti-gaming

- Quality is a PoFQ score with an **on-chain, tamper-evident reduction** over
  **operator-attested delivery samples** (success/latency/schema-validity), not
  reviews. A developer cannot fake their own score; the operator attests the
  inputs (honest-operator assumption, [`09-security.md`](./09-security.md) §9.1),
  which the **caller-co-signed receipt/voucher** ([`04-onchain.md`](./04-onchain.md)
  §4.3, [`08-payments-billing.md`](./08-payments-billing.md) §8.3) upgrades to
  bilateral attestation. It is hard to fake *without actually delivering*, but it
  is not "objective, unfakeable reputation."
- Embedding spam (keyword-stuffed manifests) is mitigated by: capping document
  length, weighting structured fields over free description, and demoting
  services with high impressions but low invoke-through / low quality.
- New services get a **bounded cold-start visibility** (a neutral prior quality)
  so they can earn samples, but cannot outrank proven services on quality alone.

## 7.7 Performance

- HNSW vector index + partial indexes on `(kind, status)` keep p95 search < 100ms
  at launch scale.
- Discovery degrades gracefully: if the embedder is unavailable, fall back to
  lexical + filters (log + metric, never 5xx the search).
- Read replicas serve discovery; the write path (indexing) is async off the
  listing transaction.
