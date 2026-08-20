<p align="center">
  <img src="https://pub-cc719bc237f94810bec78e93e056bec4.r2.dev/centra.ai_wordmark_dark.png" alt="Centra AI" width="520" />
</p>

<p align="center">
  <strong>A stateful AI operating system for serious, long-horizon work.</strong>
</p>

<p align="center">
  Centra AI gives agents memory, tools, isolated computers, coordinated work,
  and a durable record of what they actually did.
</p>

<p align="center">
  <a href="https://github.com/Sidiora-Labs/centra-llm-agents"><img src="https://img.shields.io/badge/Project-Centra%20AI-0A0A0A?style=flat-square" alt="Centra AI" /></a>
  <a href="https://github.com/Sidiora-Labs"><img src="https://img.shields.io/badge/Built%20by-Sidiora%20Labs-0A0A0A?style=flat-square" alt="Built by Sidiora Labs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Centra%20AI%20Protocol-0A0A0A?style=flat-square" alt="Centra AI Protocol License" /></a>
  <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/Version-1.0.0-0A0A0A?style=flat-square" alt="Version 1.0.0" /></a>
</p>

## What is Centra AI?

Centra AI is a private, persistent agent platform built by Sidiora Labs. It is designed for work that cannot be completed reliably in a single prompt: building software, investigating complex questions, operating browsers and files, coordinating parallel specialists, producing artifacts, and continuing across sessions without losing the state of the job.

The system is centered on two agents:

- **Neo** is the primary agent. It researches, reasons, operates tools, creates artifacts, manages ongoing work, and stays with a task beyond the life of one model response.
- **Ion** is the technical agent and coding environment. It works directly with real projects, shells, files, tests, previews, and development toolchains inside a bounded workspace.

They are supported by **Workforce**, Centra's coordinated execution layer for decomposing larger objectives into governed parallel work, and **Neo Computer**, the unified place for sources, previews, artifacts, changes, and workspace evidence.

Centra is not organized around a chat transcript. The conversation is the control surface; the product is the work system behind it.

## What makes it different

### Persistent cognition

Centra maintains durable identity, memory, active goals, evidence, and unfinished work. A process restart or a new context window does not have to turn the agent into a stranger. Cortex and Neocortex preserve the state required to reconstruct what happened, what is still open, and why the system believes what it believes.

### Real execution environments

Agents can work with files, terminals, browsers, codebases, databases, local services, media tools, and external systems. Coding happens in an actual project workspace with package managers, test runners, preview servers, and downloadable outputs—not in a simulated code block.

### Evidence, not performance

Tool activity, source material, citations, artifacts, checkpoints, and verification results are part of the product surface. Centra is built to distinguish a convincing answer from completed work and to preserve the evidence needed to audit that difference.

### Long-horizon work

Tasks can be decomposed, queued, resumed, corrected, and continued. Agents can schedule follow-up work, recover after interruption, and coordinate bounded specialists without flattening the entire job into one oversized prompt.

### Human control at consequential boundaries

Routine reversible work can proceed quickly. Sensitive writes, external side effects, financial actions, and other consequential operations pass through explicit authority and approval boundaries. The system records the decision path instead of hiding it behind a generic confirmation dialog.

### Per-user isolation

The production architecture gives each user a dedicated agent environment and durable state boundary. Central services authenticate, route, meter, and wake those environments without turning user workspaces into a shared execution pool.

## Product system

```text
                                User
                                  |
                     Centra client and Neo Computer
                                  |
                    +-------------+-------------+
                    |                           |
                   Neo                         Ion
          research, operations,          software projects,
          artifacts, automation          shell, tests, preview
                    |                           |
                    +-------------+-------------+
                                  |
                   cognition, evidence, work ledger
                     Cortex / Neocortex / Vault
                                  |
              +-------------------+-------------------+
              |                   |                   |
          Workforce          native tools         external systems
       parallel governed   browser, files,       APIs, services,
             work          terminal, media       optional finance
```

## Core capabilities

| Area | What Centra provides |
| --- | --- |
| Software engineering | Repository-aware coding, shell execution, package installation, tests, services, diffs, previews, checkpoints, and project delivery. |
| Research | Multi-source investigation, exact citations, source excerpts, synthesis, and durable research artifacts. |
| Computer use | Browser and desktop operation with visible evidence, bounded authority, and recoverable state. |
| Knowledge and memory | Persistent identity, preferences, facts, goals, conversation continuity, temporal recall, and evidence-linked beliefs. |
| Coordinated work | Task decomposition, specialist dispatch, bounded parallelism, status receipts, supervision, and resumable work. |
| Artifacts and media | Documents, code, data, images, previews, versions, and workspace-native outputs surfaced through Neo Computer. |
| Automation | Scheduled work, wake-on-demand execution, proactive briefs, and durable queues with explicit user control. |
| Safety and authority | Per-user isolation, scoped tools, encrypted state, approval gates, audit trails, and fail-closed production controls. |

## Architecture

Centra is a monorepo of independently buildable services and clients. The important boundary is simple: agents may have broad authority inside their dedicated user environments, while daemon source, platform identity, host control, and credentials remain outside that authority.

| Component | Responsibility |
| --- | --- |
| `agents/neo/` | Primary agent runtime, streaming conversation loop, tools, memory integration, automation, swarms, and delivery. |
| `ion/` | Technical agent runtime, project intelligence, coding workspace, computer control, security policy, and operator interfaces. |
| `workforce/` | Governed multi-agent work decomposition, supervision, mission state, and coordinated execution. |
| `client/` | Next.js product client: chat, coding, Neo Computer, work surfaces, settings, and live state. |
| `core/neocortex/` | Deterministic evidence and memory engine with replay-built projections and exact recovery semantics. |
| `core/cortex/` | Durable Go memory substrate and compatibility path. |
| `core/cortexclient/` | Capability-scoped client and migration seam for Neocortex. |
| `packages/vault/` | Envelope encryption and per-user data protection. |
| `executor/` | Durable action lifecycle, tool dispatch, checkpoints, and structured high-consequence execution. |
| `core/mcl/` | Internal structured-action protocol used where free-form execution is not sufficient. |
| `router/` | Authentication, per-user routing, provisioning, wake-up, and reverse proxying. |
| `gateway/` | Model access, metering, policy, and provider routing. |
| `packages/chronos/` | Durable schedules, wake events, recurring work, and proactive delivery. |
| `packages/construct/` | Typed agent-rendered product surfaces and interaction back-channels. |
| `protocol/codegraph/` | Structural code intelligence and agent self-model data. |
| `packages/sandboxd/` | Bounded workspace and preview substrate. |
| `protocol/skills/` | Reusable capability definitions and execution guidance. |
| `protocol/tools/` | Native bridges for browser, files, shell, search, media, mail, finance, and other systems. |

The root Makefile currently drives fifteen Go modules. Compatibility-sensitive machine identifiers such as existing service names, environment variables, protocol headers, and image paths are documented in [BRANDING.md](BRANDING.md).

## Reliability model

Centra treats reliability as a systems problem rather than a prompting trick.

- **State is durable.** Conversations, work, memory, and checkpoints survive process boundaries.
- **Claims have provenance.** Sources, tool results, and artifacts remain connected to the answer that used them.
- **Incomplete work stays incomplete.** Exhaustion, cancellation, ambiguity, and failed verification are represented honestly.
- **Recovery is designed in.** Supervisors can reconstruct active work from durable state instead of guessing from the last visible message.
- **Authority is explicit.** Tools and side effects are scoped to the user, environment, operation, and approval state.
- **Derived state is rebuildable.** Evidence logs and deterministic projections make recovery and audit possible without trusting an opaque in-memory snapshot.

## Deployment model

Production uses one dedicated agent service per user, backed by durable storage and reached through authenticated central routing.

- The user environment contains the tools needed for real work, including language toolchains, browser automation, local services, and project workspaces.
- The router authenticates requests, resolves the user's service, wakes it when required, and proxies traffic over the private network.
- The gateway centralizes model-provider access and metering so provider credentials do not need to live in user daemons.
- Vault-backed encryption protects durable user state, while sandbox and capability policies bound what agent-controlled processes can see and change.
- Services can sleep while idle and recover their work on the next request.

## Quick start

### Requirements

- Go 1.26.5
- Node.js 22+
- pnpm 10.33+
- Python 3.11+
- GNU Make 4.x
- Docker with Buildx for container builds

### Clone and build the backend

```bash
git clone https://github.com/Sidiora-Labs/centra-llm-agents.git
cd centra-llm-agents

make build
make install
```

`make build` builds the fifteen Go modules listed by the root Makefile. `make install` writes the runnable binaries to `./bin`.

### Run the client

```bash
cd client
corepack enable
pnpm install
pnpm dev
```

The client starts on `http://localhost:3000` by default. Runtime services require the environment values documented by their module and deployment configuration.

### Run local verification

```bash
make ci

cd client
pnpm build
```

## Repository layout

```text
centra-llm-agents/
|-- client/          product client and Neo Computer
|-- agents/neo/             primary agent
|-- ion/             technical agent and coding environment
|-- workforce/       coordinated work system
|-- core/cortex/          durable memory substrate
|-- core/neocortex/       deterministic evidence engine
|-- core/cortexclient/    Neocortex protocol client
|-- executor/        durable action lifecycle
|-- router/          authentication and user routing
|-- gateway/         model gateway and metering
|-- packages/chronos/         scheduling and wake system
|-- packages/vault/           encryption and key boundaries
|-- packages/construct/       agent-rendered interfaces
|-- protocol/codegraph/       structural code intelligence
|-- packages/sandboxd/        bounded workspaces and previews
|-- protocol/skills/          reusable agent capabilities
|-- protocol/tools/           native tool bridges
|-- deploy/          service and container packaging
|-- docs/            architecture and operator documentation
|-- protocol/spec/            source-of-truth feature specifications
```

## Documentation

| Resource | Description |
| --- | --- |
| [Architecture](ARCHITECTURE.md) | System boundaries, runtime topology, and design invariants. |
| [Branding contract](BRANDING.md) | Canonical names and compatibility identifiers retained during the rebrand. |
| [Contributing](CONTRIBUTING.md) | Development setup, quality gates, and contribution policy. |
| [Security](SECURITY.md) | Vulnerability reporting and supported versions. |
| [How Centra AI is built](HOW_CENTRA_AI_WAS_BUILT.md) | The team's specification-led development methodology. |
| [Changelog](CHANGELOG.md) | Release history. |
| [Full documentation](docs/) | Module, runtime, deployment, and operator documentation. |

## License

Centra AI is source-available under the [Centra AI Protocol License](LICENSE.md).

```text
Copyright © 2026 Sidiora Labs. All rights reserved.
SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
```

## International READMEs

- [Spanish](README.es.md)
- [Japanese](README.ja.md)
- [Portuguese](README.pt-BR.md)
- [Russian](README.ru.md)
- [Chinese, Simplified](README.zh-CN.md)

<p align="center">
  Built by <a href="https://github.com/Sidiora-Labs"><strong>Sidiora Labs</strong></a>
</p>
