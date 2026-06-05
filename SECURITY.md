# Security Policy

The Matrix project takes security seriously. This document explains what is in
scope, how to report a vulnerability, and what to expect after a report.

<p>
  <img src="https://img.shields.io/badge/Disclosure-Coordinated-004CED?style=for-the-badge&labelColor=000000" alt="Coordinated disclosure" />
  <img src="https://img.shields.io/badge/PGP-Available-004CED?style=for-the-badge&labelColor=000000" alt="PGP available" />
</p>

## Supported versions

Matrix is pre-1.0 and ships from `main`. Until v1.0 is tagged we **only**
provide fixes against the current `main` branch.

| Version  | Supported          |
| -------- | ------------------ |
| `main`   | ✅ active           |
| `< 1.0`  | ✅ rolling on `main`|
| tagged releases | ⚠️ best-effort backport at maintainer discretion |

## Scope

In scope:

- **`cortex/`** — replay determinism violations, journal gap conditions, Merkle
  root drift, scope-bypass in `cortex.Find` / `cortex.Context` / `ResolveScoped`,
  rate-limiter bypass.
- **`MCL/`** — compiler determinism breaks (same input ⇒ different IR),
  signature forgery in `MCL/envelope/`, prompt-injection paths that escape
  the closed verb / `obj_kind` vocab.
- **`bridge/`** — late-binding leak (compile-time `Find` journaling when it
  must not), scope-violation suppression bypass.
- **`executor/`** — MCP server escape (sandbox jail break), credential leakage
  from `$env:NAME` resolution, tool-URI version-pin bypass, plan walker
  capability-gate bypass, daemon auth bypass.
- **`deploy/`** — daemon container privilege escalation, Fly Machines isolation
  break, storage-box credential exposure, WireGuard mesh leak.
- Crypto primitives used by the project (ed25519 envelopes, sha256-domain-
  separated hashes, Merkle accumulator).

Out of scope:

- DoS via legitimate workload (covered by rate-limiting in `cortex/ratelimit.go`;
  reports must show a path that bypasses the documented buckets).
- Issues that require an attacker already in possession of an actor's signing
  key, Pebble store, or MCP server credentials (root-on-the-box).
- Issues in third-party MCP servers, in Pebble, `cbor/v2`, `usearch`, etc. —
  please report those upstream and link the upstream issue here.
- Findings against legacy code in `runs/`, `research/`, `knowledge/` (these
  are not executable).

## Reporting a vulnerability

**Please do not file public GitHub issues for security bugs.**

Send your report to one of the channels below:

- **GitHub Security Advisory** (preferred):
  [`paxlabs-inc/matrix` → Security → Report a vulnerability](https://github.com/paxlabs-inc/matrix/security/advisories/new)
- **Email**: `security@paxeer.app` (PGP optional; key fingerprint below)
- **Encrypted alternative**: if email is risky, mention "security" in a
  GitHub Discussions thread and a maintainer will provide a private channel.

A good report includes:

1. A clear description of the issue and its impact.
2. The affected module + file:line (we cite this way internally; same
   convention helps triage).
3. Reproduction steps or a proof-of-concept. Minimal repros land fastest.
4. Suggested severity (1–4 per the bug-report template) and any mitigation
   you have already verified.

## What to expect

| Stage              | Target turnaround                                  |
| ------------------ | -------------------------------------------------- |
| First acknowledgement | within **72 hours** of receipt                 |
| Triage + severity     | within **7 calendar days**                     |
| Fix planning          | within **14 days** of triage                   |
| Coordinated release   | by mutual agreement; typically 30–90 days      |

We will:

- Acknowledge receipt promptly.
- Keep you in the loop while we triage and patch.
- Credit you in the changelog and the GitHub Security Advisory (or honor
  your request for anonymity).
- Coordinate a disclosure window so a fix lands before public details do.

We ask that you:

- Give us reasonable time to fix the issue before public disclosure.
- Do not exploit the vulnerability beyond the minimum required to demonstrate
  the issue.
- Do not access data that is not yours.

## Hardening notes for operators

If you are running Matrix in production, the following are already enforced
in code; this list is for operator-side defence-in-depth.

- **Replay invariant** (`cortex` §13.4): run `cortex-shell rebuild
  -verify-only` after every disaster-recovery restore and as a periodic CI
  job. Pre/Post `OverallRoot` divergence indicates state corruption.
- **Atomic batches**: every cortex write must journal — `store.BeginWrite`
  enforces `ErrBatchNoJournal` on commit. If you fork the cortex layer,
  preserve this invariant.
- **URI version pinning** (D13): `cortex.Resolve` rejects `#latest`. Skill
  authors must pin to `@semver` or `@sha256:...`.
- **Closed vocabularies** (D7, v1): the 10-verb and 8-`obj_kind` enums are
  load-bearing. Extending them is a journaled migration, not a code-only
  edit.
- **Rate limits** (`cortex/ratelimit.go`): the `KindScopeViolation` and
  `cortex.Attest` token buckets default to `10/s burst 20` and `1/s burst 5`
  respectively. Tune via `WithRateLimits` only after measuring real
  workloads.
- **Daemon auth** (`executor/cmd/mcl-execute daemon`): always set
  `MATRIX_DAEMON_TOKEN` in non-loopback deployments. Empty token disables
  auth — local-dev only.
- **MCP credentials**: agent manifests reference secrets with `$env:NAME`;
  literal credentials in manifests are rejected by convention. Inject via
  `.env` or your secret store.

## PGP

```
TBD — request the current security@paxeer.app key via the email channel.
Rotation policy: yearly, or after any disclosure incident.
```
