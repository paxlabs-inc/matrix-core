---
name: deep-research
description: Multi-source deep research through Centra AI web search, fetch, and the router-owned Exa MCP. Searches, extracts, synthesizes, and delivers cited reports with explicit grounding.
origin: Centra AI/Paxlabs
---

# Deep Research

Produce a cited research answer from multiple web sources using ordinary discovery plus Centra AI's router-owned Exa evidence tools.

## Tool Boundary

Use the shipped `web_search`, `web_news`, `fetch`, and `exa__*` tools. Exa vendor credentials and run ownership stay in the Centra router. Do not install a direct vendor MCP or ask the user for an API key.

## Workflow

1. Turn the user's objective into 3-5 bounded research questions. Use reasonable defaults unless a missing choice materially changes the result.
2. Use `web_search` / `web_news` for cheap discovery. Add `exa_search` where semantic matching, domain/date controls, financial reports, or synthesis improves the evidence set.
3. Read selected ordinary-search URLs with `fetch`. Use `exa_contents` when extracting a bounded claim from known URLs; preserve every per-URL status and disclose partial evidence.
4. For genuinely multi-step synthesis, call `exa_research_start` with a bounded output schema and medium-or-lower effort by default, then poll with `exa_research_get`. Only terminal grounding authorizes the generated output.
5. Cross-reference claims, separate facts from inference, label uncertainty, and deliver the answer with citations near each supported claim.

## Quality Rules

1. Every material current claim needs a source.
2. Prefer official, primary, academic, and reputable reporting sources.
3. Flag claims supported by only one weak source.
4. Say when evidence was not found; never fill a gap with a similarly named entity or a different evidence class.
5. Exa highlights and Contents extracts are source evidence. Exa answers and Agent output are generated synthesis.
6. Never cite Agent previews or intermediate sources as final support; terminal grounding is authoritative.
7. Report partial Contents failures instead of silently dropping them.
8. Do not start expensive asynchronous research merely because a surface opened; it must be explicitly useful to the user request.

## Report Shape

- Lead with the answer and key findings.
- Organize evidence into the smallest useful set of themes.
- Put caveats beside the affected finding.
- End with actionable implications when the user is making a decision.
