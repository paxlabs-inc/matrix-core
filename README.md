<h1 align="center">Agent Operating Framework</h1>

<p align="center">
  <img src="https://docs.matrixmcl.com/_jd/logo/wordmark.webp?v=mqi8nza6"
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxNiIgaGVpZ2h0PSIxNiIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIiBzdHJva2U9IiNmZmZmZmYiIHN0cm9rZS13aWR0aD0iMiIgc3Ryb2tlLWxpbmVjYXA9InJvdW5kIiBzdHJva2UtbGluZWpvaW49InJvdW5kIj48Y2lyY2xlIGN4PSIxMiIgY3k9IjEyIiByPSIxMCIvPjxwYXRoIGQ9Ik0xMiAxNnYtNCIvPjxwYXRoIGQ9Ik0xMiA4aC4wMSIvPjwvc3ZnPg==&logoColor=white" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square&logoColor=white" alt="Built by PaxLabs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Status-Active-0A0A0A?style=flat-square" alt="Status: Active" /></a>
  <a href="https://paxeer.app"><img src="https://img.shields.io/badge/Chain-HyperPaxeer%20125-0A0A0A?style=flat-square" alt="Chain: HyperPaxeer 125" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Block%20Time-400ms-0A0A0A?style=flat-square" alt="Block Time: 400ms" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Finality-400ms-0A0A0A?style=flat-square" alt="Finality: 400ms" /></a>
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/stargazers"><img src="https://img.shields.io/github/stars/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Stars" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/network/members"><img src="https://img.shields.io/github/forks/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Forks" /></a>
  <a href="https://docs.matrixmcl.com"><img src="https://img.shields.io/badge/Docs-docs.matrixmcl.com-0A0A0A?style=flat-square" alt="Documentation" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-38.7%25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/Solidity-26.3%25-363636?style=flat-square&logo=solidity&logoColor=white" alt="Solidity" />
  <img src="https://img.shields.io/badge/JavaScript-16.9%25-F7DF1E?style=flat-square&logo=javascript&logoColor=black" alt="JavaScript" />
  <img src="https://img.shields.io/badge/TypeScript-11.1%25-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" />
  <img src="https://img.shields.io/badge/HTML-5.5%25-E34F26?style=flat-square&logo=html5&logoColor=white" alt="HTML" />
  <img src="https://img.shields.io/badge/Python-0.5%25-3776AB?style=flat-square&logo=python&logoColor=white" alt="Python" />
</p>

<p align="center">
<img src="https://readme-stats-github.pages.dev/api/typing?lines=Building%20The%20Rails%20For%20The%20Machine%20Economy&theme=dark&color=%230565ff&particleColor=%23141414&background=%23141414"/>
</p>
---

## What is Matrix?

Matrix Core is the cognition and user-experience layer built on top of the [Paxeer Network](https://paxeer.app). It transforms natural-language requests from non-developers into a typed, inspectable, and correctable **Intent IR** that an autonomous agent can execute deterministically.

Most agent systems today lose intent between the user's words and the agent's actions. Prompts are fragile. Meaning evaporates across context windows. There is no shared vocabulary between human and machine, and no structured way to correct a plan once it has gone wrong. Matrix solves all four.

It is not a chatbot wrapper. It is a rigorously typed intent-to-execution compiler with replayable memory, deterministic walk semantics, and a closed vocabulary that ensures intent survives multi-step execution.

## The Four Failure Modes

Matrix was designed from first principles to eliminate the four classic failure modes that break every human-to-agent workflow:

| Failure Mode | The Problem | How Matrix Solves It |
|--------------|-------------|----------------------|
| **Prompt Fragility** | Slight rephrasing produces entirely different agent behavior | Closed vocabulary (10 verbs, 8 kinds) eliminates ambiguity at the AST level |
| **Intent Loss** | Original meaning drifts across context windows and tool calls | Typed Intent IR preserves semantics through every compiler stage; the IR is the contract |
| **No Shared Ontology** | Human and agent operate on different conceptual models | Canonical refs, SKILL manifests, and DID-bound agent manifests establish a common language |
| **No Structured Correction** | Users must start over or edit raw prompts to fix a plan | Walk-replay lets users inspect, correct, and resume execution at any step |

## Two Rails, One Substrate

Matrix ships two complementary agent rails over a single shared memory and execution substrate. You pick the rail based on the stakes of the work.

### Neo -- Conversational Rail

The default tool-calling agent. Familiar, robust, and fully permissive on reversible work.

- Shell, code, fetch, and web tools available out of the box
- Delegates monetary or irreversible work to MCL automatically
- Best for exploration, drafting, querying, and low-stakes automation
- Entry point: `POST /chat`

### MCL -- Rigorous Rail

Natural language becomes typed Intent IR becomes a plan becomes a replayable walk.

- Purpose-built for high-stakes, on-chain, and irreversible operations
- Deterministic compilation: same prose, same grammar, same IR
- Full audit trail: every step is journaled, attested, and replayable
- Entry point: `POST /messages`

Both rails read from and write to the same **cortex** memory graph and **executor** lifecycle engine. Switching between them is seamless -- a Neo conversation can hand off to MCL the moment stakes rise, and MCL can return context to Neo when rigor is no longer required.

## Architecture

```
user prose
      |
      v
+-----------------------+------------------------+
|                 MCL compiler                   |
|  lexer -> parser -> validator -> canonical     |
|    \                                  /        |
|     +-> interpreter <- LLM <- grammar          |
|              |                                 |
|              v                                 |
|          Intent IR  (closed verb, closed kind) |
+-----------------+------------------------------+
                  |
                  v
            +-----+------+
            |   bridge   |
            | MCL.Cortex |   (adapter)
            +-----+------+
                  |
                  v
+------------------+    +----+----+    +-----------------+
|  agent manifest  |--->| cortex  |<---| executor walker |
|  (DID-bound)     |    | (Pebble)|    | + MCP dispatch  |
+------------------+    +----+----+    +-----------------+
                            |                  |
                            |                  v
                            |          +---------------+
                            |          |  MCP servers  |
                            |          |  (subprocess) |
                            |          +-------+-------+
                            |                  |
                            +------ events -----+
                                      |
                                      v
                              attest + EMA loop
```

The root Makefile drives nine sibling Go modules -- each independently `go build` / `go test` able with its own `go.mod`:

<h1 align="center">
  <img src="https://www.readmecodegen.com/api/file-tree-embed?repo=paxlabs-inc%2Fmatrix-core&branch=main&maxDepth=1&foldersOnly=true&exclude=marketplace%2Cmarketplace%2F.dockerignore%2Cmarketplace%2F.gitignore%2Cmarketplace%2FBUILD_PLAN.md%2Cmarketplace%2FDockerfile%2Cmarketplace%2FOPS_RUNBOOK.md%2Cmarketplace%2FREADME.md%2Cmarketplace%2Fapp%2Cmarketplace%2Fapp%2Fapp.css%2Cmarketplace%2Fapp%2Fcomponents%2Cmarketplace%2Fapp%2Fcomponents%2Fdashboard-nav.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Ffeedback.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fpax.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fsearch-dock.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fservice-card.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fsite-footer.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fsite-header.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Ftry-it.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fturnstile.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fui.tsx%2Cmarketplace%2Fapp%2Fcomponents%2Fwallet.tsx%2Cmarketplace%2Fapp%2Fentry.server.tsx%2Cmarketplace%2Fapp%2Fenv.d.ts%2Cmarketplace%2Fapp%2Ffonts.css%2Cmarketplace%2Fapp%2Flib%2Cmarketplace%2Fapp%2Flib%2Fauth.server.test.ts%2Cmarketplace%2Fapp%2Flib%2Fauth.server.ts%2Cmarketplace%2Fapp%2Flib%2Fcache.server.test.ts%2Cmarketplace%2Fapp%2Flib%2Fcache.server.ts%2Cmarketplace%2Fapp%2Flib%2Fdeus.server.test.ts%2Cmarketplace%2Fapp%2Flib%2Fdeus.server.ts%2Cmarketplace%2Fapp%2Flib%2Fdeus.types.ts%2Cmarketplace%2Fapp%2Flib%2Fenv.ts%2Cmarketplace%2Fapp%2Flib%2Fformat.test.ts%2Cmarketplace%2Fapp%2Flib%2Fformat.ts%2Cmarketplace%2Fapp%2Flib%2Flimits.server.ts%2Cmarketplace%2Fapp%2Flib%2Fonce.server.ts%2Cmarketplace%2Fapp%2Flib%2Frequest-context.server.ts%2Cmarketplace%2Fapp%2Flib%2Fsentry.server.ts%2Cmarketplace%2Fapp%2Flib%2Fsiwe.server.ts%2Cmarketplace%2Fapp%2Flib%2Fsmoothui-data.ts%2Cmarketplace%2Fapp%2Flib%2Fturnstile.server.ts%2Cmarketplace%2Fapp%2Flib%2Futils.ts%2Cmarketplace%2Fapp%2Flib%2Fvalidate.server.test.ts%2Cmarketplace%2Fapp%2Flib%2Fvalidate.server.ts%2Cmarketplace%2Fapp%2Fmotion.css%2Cmarketplace%2Fapp%2Froot.tsx%2Cmarketplace%2Fapp%2Froutes.ts%2Cmarketplace%2Fapp%2Froutes%2Cmarketplace%2Fapp%2Froutes%2F_public.tsx%2Cmarketplace%2Fapp%2Froutes%2Fapi.wallet.tsx%2Cmarketplace%2Fapp%2Froutes%2Fauth.callback.tsx%2Cmarketplace%2Fapp%2Froutes%2Fcatalog.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2F_layout.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Faccount.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Fanalytics.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Fearnings.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Findex.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Fnew.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdashboard%2Fservice.tsx%2Cmarketplace%2Fapp%2Froutes%2Fdiscover.tsx%2Cmarketplace%2Fapp%2Froutes%2Fhealthz.tsx%2Cmarketplace%2Fapp%2Froutes%2Fhome.tsx%2Cmarketplace%2Fapp%2Froutes%2Flogin.tsx%2Cmarketplace%2Fapp%2Froutes%2Flogout.tsx%2Cmarketplace%2Fapp%2Froutes%2Frobots.tsx%2Cmarketplace%2Fapp%2Froutes%2Fservice.tsx%2Cmarketplace%2Fapp%2Froutes%2Fsitemap.tsx%2Cmarketplace%2Fapp%2Fsmoothui.css%2Cmarketplace%2Fapp%2Fwelcome%2Cmarketplace%2Fapp%2Fwelcome%2Flogo-dark.svg%2Cmarketplace%2Fapp%2Fwelcome%2Flogo-light.svg%2Cmarketplace%2Fapp%2Fwelcome%2Fwelcome.tsx%2Cmarketplace%2Fcomponents%2Cmarketplace%2Fcomponents%2Fui%2Cmarketplace%2Fcomponents%2Fui%2Flib%2Cmarketplace%2Fcomponents%2Fui%2Flib%2Fanimation.ts%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Falert-dialog.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Favatar.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Fcommand.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Fcontext-menu.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Fdialog.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Fdrawer.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Fdropdown-menu.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fshadcn%2Fpopover.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fagent-avatar%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fagent-avatar%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fai-branch%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fai-branch%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fai-input%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fai-input%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fai-input%2Fuse-click-outside.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-avatar-group%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-avatar-group%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-file-upload%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-file-upload%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-input%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-input%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-o-t-p-input%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-o-t-p-input%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-progress-bar%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-progress-bar%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-stepper%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-stepper%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-tabs%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-tabs%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-tags%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-tags%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-toggle%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-toggle%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-tooltip%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fanimated-tooltip%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fapp-download-stack%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fapp-download-stack%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fapple-invites%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fapple-invites%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-accordion%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-accordion%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-dropdown%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-dropdown%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-modal%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-modal%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-toast%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbasic-toast%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbook%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbook%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbreadcrumb%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbreadcrumb%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbutton-copy%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fbutton-copy%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcheckbox%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcheckbox%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fclip-corners-button%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fclip-corners-button%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcombobox%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcombobox%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcontext-menu%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcontext-menu%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcontribution-graph%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcontribution-graph%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcta-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcta-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcta-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcta-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcta-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcta-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcursor-follow%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcursor-follow%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fcursor-follow%2Fuse-cursor-position.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdialog%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdialog%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdot-morph-button%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdot-morph-button%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdrawer%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdrawer%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdropdown-menu%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdropdown-menu%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdynamic-island%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fdynamic-island%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fexpandable-cards%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fexpandable-cards%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fexposure-slider%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fexposure-slider%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-4%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffaq-4%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffeatures-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffeatures-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffeatures-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffeatures-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffeatures-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffeatures-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffigma-comment%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffigma-comment%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffigma-comment%2Fuse-click-outside.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-4%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ffooter-4%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fform%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fform%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgithub-stars-animation%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgithub-stars-animation%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fglow-hover-card%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fglow-hover-card%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgooey-popover%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgooey-popover%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgooey-popover%2Fuse-click-outside.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgrid-loader%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fgrid-loader%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-4%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-4%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-5%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-5%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-6%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fheader-6%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fimage-metadata-preview%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fimage-metadata-preview%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Finfinite-slider%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Finfinite-slider%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Finteractive-image-selector%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Finteractive-image-selector%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fjob-listing-component%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fjob-listing-component%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-4%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Flogo-cloud-4%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fmagnetic-button%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fmagnetic-button%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fnotification-badge%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fnotification-badge%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fnumber-flow%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fnumber-flow%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpagination%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpagination%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fphototab%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fphototab%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpower-off-slide%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpower-off-slide%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fprice-flow%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fprice-flow%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpricing-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpricing-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpricing-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpricing-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpricing-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fpricing-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fproduct-card%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fproduct-card%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fradio-group%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fradio-group%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Freveal-text%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Freveal-text%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Freviews-carousel%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Freviews-carousel%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Frich-popover%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Frich-popover%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscramble-hover%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscramble-hover%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscroll-reveal-paragraph%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscroll-reveal-paragraph%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscrollable-card-stack%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscrollable-card-stack%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscrubber%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fscrubber%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsearchable-dropdown%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsearchable-dropdown%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fselect%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fselect%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fshared%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fshared%2Fanimated-group.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fshared%2Fanimated-text.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fshared%2Fhero-header.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fshared%2Flogos.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fshared%2Fsmoothbutton.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsiri-orb%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsiri-orb%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fskeleton-loader%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fskeleton-loader%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsmooth-button%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsmooth-button%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsocial-selector%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fsocial-selector%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fstats-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fstats-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fstats-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fstats-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fswitchboard-card%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fswitchboard-card%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fteam-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fteam-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fteam-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fteam-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftestimonials-1%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftestimonials-1%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftestimonials-2%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftestimonials-2%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftestimonials-3%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftestimonials-3%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftweet-card%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftweet-card%2Fclient.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftweet-card%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftypewriter-text%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Ftypewriter-text%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fuser-account-avatar%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fuser-account-avatar%2Findex.tsx%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fwave-text%2Cmarketplace%2Fcomponents%2Fui%2Fsmoothui%2Fwave-text%2Findex.tsx%2Cmarketplace%2Fenv-extra.d.ts%2Cmarketplace%2Fmocks%2Cmarketplace%2Fmocks%2Fdeus-mock.mjs%2Cmarketplace%2Fpackage.json%2Cmarketplace%2Fpnpm-lock.yaml%2Cmarketplace%2Fpnpm-workspace.yaml%2Cmarketplace%2Fpublic%2Cmarketplace%2Fpublic%2F.well-known%2Cmarketplace%2Fpublic%2F.well-known%2Fsecurity.txt%2Cmarketplace%2Fpublic%2F_headers%2Cmarketplace%2Fpublic%2Ffavicon.ico%2Cmarketplace%2Fpublic%2Ffonts%2Cmarketplace%2Fpublic%2Ffonts%2FInter-Variable-Italic-latin.woff2%2Cmarketplace%2Fpublic%2Ffonts%2FInter-Variable-latin.woff2%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-Bold.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-CompactExtralightReclined.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-CompactRegular.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-CompactThinItalic.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-CompressedExtrabold.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-CompressedMediumReclined.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-CondensedThin.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-ExtraboldItalic.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-Light.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-Medium.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-NarrowExtraboldItalic.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-NarrowExtralightReclined.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-NarrowSemibold.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-NarrowSemiboldItalic.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-Semibold.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-SlimExtralight.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-SlimMediumReclined.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-SlimSemiboldItalic.otf%2Cmarketplace%2Fpublic%2Ffonts%2FPPPangramSansRounded-Thin.otf%2Cmarketplace%2Fpublic%2Ficon.svg%2Cmarketplace%2Fpublic%2Flegal%2Cmarketplace%2Fpublic%2Flegal%2FREADME.txt%2Cmarketplace%2Fpublic%2Flegal%2Facceptable-use-policy.html%2Cmarketplace%2Fpublic%2Flegal%2Fai-agent-responsible-use-policy.html%2Cmarketplace%2Fpublic%2Flegal%2Faml-kyc-policy.html%2Cmarketplace%2Fpublic%2Flegal%2Fapi-terms-of-use-license-agreement.html%2Cmarketplace%2Fpublic%2Flegal%2Fcompliance-statement.html%2Cmarketplace%2Fpublic%2Flegal%2Fconsolidated-governance-and-liability-framework.html%2Cmarketplace%2Fpublic%2Flegal%2Fcss%2Cmarketplace%2Fpublic%2Flegal%2Fcss%2Fpaxeer-legal.css%2Cmarketplace%2Fpublic%2Flegal%2Feu-ai-act-compliance.html%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FInter-Variable-Italic-latin.woff2%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FInter-Variable-latin.woff2%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-Bold.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-CompactExtralightReclined.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-CompactRegular.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-CompactThinItalic.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-CompressedExtrabold.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-CompressedMediumReclined.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-CondensedThin.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-ExtraboldItalic.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-Light.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-Medium.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-NarrowExtraboldItalic.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-NarrowExtralightReclined.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-NarrowSemibold.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-NarrowSemiboldItalic.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-Semibold.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-SlimExtralight.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-SlimMediumReclined.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-SlimSemiboldItalic.otf%2Cmarketplace%2Fpublic%2Flegal%2Ffonts%2FPPPangramSansRounded-Thin.otf%2Cmarketplace%2Fpublic%2Flegal%2Findex.html%2Cmarketplace%2Fpublic%2Flegal%2Fmachine-to-machine-m2m-agreement.html%2Cmarketplace%2Fpublic%2Flegal%2Fmarketplace-terms-and-conditions.html%2Cmarketplace%2Fpublic%2Flegal%2Fon-chain-data-privacy-notice.html%2Cmarketplace%2Fpublic%2Flegal%2Fprivacy-policy.html%2Cmarketplace%2Fpublic%2Flegal%2Fprivacy.html%2Cmarketplace%2Fpublic%2Flegal%2Frefunds.html%2Cmarketplace%2Fpublic%2Flegal%2Fregulatory-infrastructure-overview.html%2Cmarketplace%2Fpublic%2Flegal%2Frisk-disclosure.html%2Cmarketplace%2Fpublic%2Flegal%2Fsecurity-policy.html%2Cmarketplace%2Fpublic%2Flegal%2Fterms-of-service.html%2Cmarketplace%2Fpublic%2Flegal%2Fterms.html%2Cmarketplace%2Fpublic%2Flegal%2Fuser-facing-compliance-training-centre.html%2Cmarketplace%2Fpublic%2Fsite.webmanifest%2Cmarketplace%2Freact-router.config.ts%2Cmarketplace%2Ftsconfig.json%2Cmarketplace%2Fvite.config.ts%2Cmarketplace%2Fvitest.config.ts%2Cmarketplace%2Fworker-configuration.d.ts%2Cmarketplace%2Fworkers%2Cmarketplace%2Fworkers%2Fapp.ts%2Cmarketplace%2Fwrangler.jsonc&transparentBg=true&showHeader=true&showBorder=true" alt="Dynamic File Tree" />
</a>
</h1>

## The Stack

| Module | Role |
|--------|------|
| **MCL** | MatrixScript compiler -- lexer, parser, validator, canonicaliser, interpreter. Produces typed Intent IR and LLM-mediated plan envelopes. |
| **cortex** | Per-actor typed memory graph on Pebble. Append-only journal, Merkle-anchored snapshots, byte-deterministic replay. |
| **bridge** | MCL-to-cortex adapter. Separate Go module for clean interface boundaries. |
| **executor** | Plan walker, lifecycle state machine, MCP dispatch, per-user daemon, Liaison narrator, end-to-end test harness. |
| **neo** | Default conversational tool-calling agent with automatic MCL delegation for high-stakes operations. |
| **gateway** | Metered LLM proxy with PAX credit ledger, free-tier whitelist, and rate card enforcement. |
| **router** | Per-user Fly Machine provisioning with wake-then-reverse-proxy front door. |
| **deus** | Agent-service marketplace: registry, discovery, metered invocation, EIP-712 receipts, hosted execution. |
| **tachyon** | Agent-native Solidity/EVM engine -- compile, test, simulate, deploy. (git submodule) |
| **uwac** | Universal Web Agent Connector -- OAuth vault providing per-user MCP tools. |
| **layerx** | Settlement fabric and custody spine for agent balances. |
| **chronos** | Centralised agent scheduler and wake-up system. |
| **atlas** | Additional infrastructure orchestration layer. |
| **context** | Context management subsystem. |
| **journal** | Append-only journal subsystem for deterministic state replay. |
| **knowledge** | Canonical references: matrix.kvx project state, models, and schema definitions. |
| **skills** | SKILL.mtx capability manifests and SKILL.md prose capability descriptions. |
| **tools** | MCP servers: paxeer, browser, tachyon, deus, uwac, web-search, media, cortex. |
| **agents** | DID-bound agent manifests (default.json, neo.json) plus MCP server templates. |
| **protocol** | Protocol definitions and wire formats. |
| **marketplace** | Deus marketplace and developer dashboard (React Router on Cloudflare Workers). |
| **client** | Matrix consumer application (Next.js / React). |
| **deploy** | Daemon container image, Fly Machine deploy, shared-service images, box install scripts. |

## Key Design Decisions

- **10 closed verbs (D7)**: `find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate` -- every intent maps to one of these. No open-ended classification.

- **8 closed object kinds**: `service`, `model`, `agent`, `knowledge`, `intent`, `asset`, `plan`, `capability` -- every operand in an intent is one of these. No unstructured blobs.

- **Replay invariant (section 13.4)**: Derived state can always be rebuilt byte-identically from the journal. Enforced on every pull request via `make ci`.

- **Canonical AST hashing**: Comments and whitespace do not affect the digest. Two semantically identical programs produce the same hash. Content-addressed and reformat-safe.

- **Closed vocabularies**: Intent survives multi-step execution because meaning is typed at the compiler level, not inferred by the LLM at runtime.

## Quickstart

### Prerequisites

- Go 1.22+
- GNU Make 4.x
- Node.js 20+
- Python 3.11+
- Docker with Buildx

### Build

```bash
# Clone the repository
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core

# Build all nine Go modules
make build

# Install runnable CLIs into ./bin
make install

# Run tests (go test -count=1 -race ./... per module)
make test

# Full CI check (gofmt + vet + tests; mirrors GitHub Actions)
make ci
```

### Configure

```bash
# Copy the example environment file
cp .env.example .env

# Required for non-dry-run compilation:
#   FIREWORKS_API_KEY
#   TOGETHER_API_KEY
#
# Required for authenticated daemon mode:
#   MATRIX_DAEMON_TOKEN
```

### Compile Your First Intent

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb build
```

### Run an End-to-End Walk

```bash
./bin/mcl-execute walk \
  -prose "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

### Start the Daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

## API Reference

The executor daemon exposes a lightweight HTTP API for agent interaction.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/healthz` | Liveness probe + SSE broker statistics |
| `POST` | `/chat` | Converse with the agent via Neo (conversational rail) |
| `GET` | `/events` | Server-Sent Events stream for real-time transcript tailing |
| `POST` | `/messages` | Submit a prose message (rigorous MCL rail) |
| `GET` | `/intents/{id}` | Read intent envelope chain by intent ID |
| `GET` | `/me` | Per-user settings and identity |
| `POST` | `/shutdown` | Graceful drain and shutdown |

## Documentation

| Resource | Description |
|----------|-------------|
| [Architecture Guide](ARCHITECTURE.md) | System map, module boundaries, key invariants, and design rationale |
| [Contributing Guide](CONTRIBUTING.md) | Development setup, test discipline, commit style, and PR process |
| [Security Policy](SECURITY.md) | Vulnerability disclosure and responsible reporting |
| [Changelog](CHANGELOG.md) | Keep-a-Changelog format release notes |
| [MCL Documentation](docs/MCL-docs/index.md) | MatrixScript language reference, grammar, and compiler internals |
| [Daemon Deploy Guide](deploy/daemon/README.md) | Production deployment, Fly Machine configuration, and operations |
| [Full Documentation](https://docs.matrixmcl.com) | Complete documentation site at docs.matrixmcl.com |

## Contributing

Matrix Core is open source and you are free to **fork and modify it**. The `main` branch, however, is developed strictly by the core team: unsolicited pull requests are generally not merged, and outside changes are accepted only after we have worked directly with the contributor.

Before opening anything, read the contribution policy at the top of the [Contributing Guide](CONTRIBUTING.md). Issues, bug reports, and security disclosures are always welcome.

Contributors:
- dev-paxeer
- Andrew
- paxlabs-inc
- cursoragent
- Sidiora-Technologies

## License

Matrix Core is source-available under the [Matrix-Protocol License](LICENSE.md).

You may read, use, deploy, and integrate Matrix Core freely. If you modify and redistribute the software, you must release your changes under the same license. A commercial license from PaxLabs Inc. is required once you cross the commercial trigger thresholds:

- Charged fees exceeding **USD 100,000** in any 12-month period; or
- Liquidity under control exceeding **USD 10,000,000**.

See [LICENSE.md](LICENSE.md) for full terms.

## International READMEs

- [Espanol](README.es.md)
- [Nihongo / Japanese](README.ja.md)
- [Portugues](README.pt-BR.md)
- [Russkiy / Russian](README.ru.md)
- [Zhongwen / Chinese (Simplified)](README.zh-CN.md)

## Related

- [Paxeer Network](https://paxeer.app) -- The L1 blockchain Matrix Core is built on. 400ms blocks, 400ms finality, purpose-built for agentic workloads.
- [PaxLabs](https://labs.paxeer.app) -- Building the future of human-agent collaboration.

---

<p align="center">
  Built by <a href="https://labs.paxeer.app"><strong>PaxLabs Inc.</strong></a>
</p>

<p align="center">
  <sub>SPDX-License-Identifier: Matrix-Protocol</sub>
</p>