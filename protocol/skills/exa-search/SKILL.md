---
name: exa-search
description: Grounded semantic search and asynchronous research through Centra AI's router-owned Exa MCP. Use for controlled web evidence, company research, known-URL extraction, outbound briefs, or source-grounded drafts.
origin: Centra AI/Paxlabs
---

# Exa Search

Use Centra AI's router-owned Exa lane for semantic retrieval, extractive contents, and bounded multi-source research. Neo never receives the vendor API key.

## Runtime Boundary

Use only the shipped `exa__*` tools. The Centra router owns vendor credentials, user scoping, rate and spend limits, caching, run ownership, metering, and cancellation. Never ask the user for an Exa key, install a vendor MCP, or call Exa directly.

## Choosing the Evidence Lane

- Keep `web_search` / `web_news` plus `fetch` as the cheap default for ordinary discovery.
- Use `exa_search` when semantic matching, source/date controls, financial reports, or deeper synthesis materially improves retrieval.
- Use `exa_contents` for extractive evidence or bounded full text from 1-10 known URLs.
- Use asynchronous Agent research only for a genuinely multi-source question that benefits from structured synthesis.

## Tools

### `exa_search`

Supports `query`, `type`, `num_results`, `category`, domain include/exclude lists, publication-date bounds, and an extractive `highlight_query`.

```text
exa_search(query: "current WebAssembly component model adoption", num_results: 8, start_published_date: "2026-01-01")
```

### `exa_contents`

Returns highlights or bounded full text and a status for every requested URL. An HTTP-successful batch can still be partial; preserve and report every failed URL status.

```text
exa_contents(urls: ["https://example.com/report"], highlight_query: "revenue guidance")
```

### Asynchronous Research

Call `exa_research_start`, then poll the owned run with `exa_research_get`. Only completed terminal output and its grounding are authoritative. Use `exa_research_continue` for a bounded follow-up and `exa_research_cancel` when queued or running work is no longer needed.

### Bounded Workflows

- `exa_outbound_brief` returns sourced company/person context and talking points; it sends nothing.
- `exa_social_draft` returns a source-grounded draft; it publishes nothing.

## Grounding Rules

1. Exa highlights and Contents extracts are source evidence.
2. Exa answers, Agent research, outbound briefs, and social drafts are generated synthesis.
3. Only terminal Agent grounding authorizes generated claims; previews and intermediate source lists do not.
4. Preserve provider request IDs, retrieval times, costs, cache state, and partial URL failures when they matter to the answer.
5. If grounded research conflicts with canonical market data, retain both sourced values and timestamps and state the conflict.

## Related Skills

- `deep-research` for a complete multi-source workflow.
- `market-research` for decision-oriented business analysis.
