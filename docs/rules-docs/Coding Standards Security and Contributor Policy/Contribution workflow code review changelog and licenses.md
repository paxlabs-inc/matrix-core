# Coding Standards, Security, and Contributor Policy

## Overview

This part of the repository defines how changes are proposed, reviewed, versioned, and licensed. It combines workflow rules, review gates, change-log conventions, a community code of conduct, and the project’s legal terms so contributors can move from idea to merged change with clear expectations.

The policy set is intentionally split between English source-of-truth files and Chinese-language mirrors under `rules/zh/`. The common workflow and code review documents define the baseline process, while the changelog, code of conduct, and license files govern release notes, community behavior, and distribution rights.

## Contribution Workflow

The workflow is split across the common rules and the Chinese mirror set. The English documents describe the canonical process; the Chinese documents restate the same workflow and review rules, and `rules/zh/README.md` explains how rule precedence works.

| File | Responsibility |
| --- | --- |
| `rules/common/development-workflow.md` | Defines the end-to-end feature workflow: research and reuse, planning, TDD, code review, commit and push, and pre-review checks. It explicitly requires GitHub code search first, then library documentation, then broader web research only when needed. |
| `rules/common/git-workflow.md` | Provides the git workflow reference that `rules/common/development-workflow.md` points contributors to for commit message format and pull request handling. |
| `rules/zh/development-workflow.md` | Chinese-language version of the development workflow with the same numbered stages and the same emphasis on research first, planning, TDD, review, commit, and pre-review checks. |
| `rules/zh/README.md` | Explains the rule-set structure, including the split between common rules and language-specific rules, and states that language-specific rules take precedence over common rules. |


### Required workflow sequence

1. Research and reuse first

Search for existing implementations before writing new code. The document requires GitHub code search first, then library documentation, then broader web research only if the first two are insufficient.

1. Plan before coding

Generate planning artifacts before implementation and break the work into phases.

1. Use TDD

Write tests first, implement to make them pass, then refactor.

1. Run code review immediately after coding

The workflow says to use the review agent right after writing code and to fix critical and high-priority findings.

1. Commit and push with disciplined history

The workflow requires detailed commit messages and directs contributors to the git workflow document for commit format and pull request process.

1. Complete pre-review checks before requesting review

The workflow requires passing automated checks, resolving merge conflicts, and syncing the branch with the target branch before asking for review.

## Code Review Policy

`rules/common/code-review.md` and `rules/zh/code-review.md` define the same review expectations in English and Chinese. They treat review as mandatory after code changes and before merges, with extra scrutiny for security-sensitive work.

| Area | Source-backed rule |
| --- | --- |
| When review is required | After writing or modifying code, before commits to shared branches, when security-sensitive code changes, when architecture changes, and before merging pull requests. |
| Pre-review conditions | Automated checks must pass, merge conflicts must be resolved, and the branch must be up to date with the target branch. |
| Review checklist | Readable and well-named code, focused functions, cohesive files, shallow nesting, explicit error handling, no hardcoded secrets or credentials, no console logging or debug statements, tests for new functionality, and at least 80% coverage. |
| Security review triggers | Authentication or authorization code, user input handling, database queries, file system operations, external API calls, cryptographic operations, and payment or financial code. |
| Severity handling | CRITICAL blocks merge, HIGH should be fixed before merge, MEDIUM is a maintainability concern, and LOW is optional. |
| Review agents | `code-reviewer`, `security-reviewer`, `typescript-reviewer`, `python-reviewer`, `go-reviewer`, `rust-reviewer`. |


### Review checklist details

The review policy explicitly escalates auth, payments, user data, database, filesystem, external API, cryptography, and financial changes to security review.

The checklist in `rules/common/code-review.md` and `rules/zh/code-review.md` is concrete, not advisory. It asks reviewers to verify:

- code readability and naming
- focused functions
- cohesive files
- shallow nesting
- explicit error handling
- no hardcoded secrets or credentials
- no console logging or debug statements
- tests for new functionality
- minimum test coverage

The Chinese file mirrors the same structure and review thresholds, including the same severity levels and agent list.

## Changelog Policy

`CHANGELOG.md` is the project’s release-history ledger. It states that all notable changes are documented there, that the format is based on Keep a Changelog, and that the project adheres to Semantic Versioning.

The file also records two important conventions:

- cross-references in parentheses point to either `knowledge/matrix.kvx` session entries such as `sess#NN`

or research decision IDs such as `D1` through `D18`

- the changelog is organized into versioned sections, including `[Unreleased]`, with release links at the bottom

### What the changelog captures

The visible entries show that the changelog records:

- added features
- removed items
- versioned releases with dates
- implementation notes that point back to sessions and decisions

This makes the changelog both a release note and a provenance trail for major repository changes.

## Code of Conduct

`CODE_OF_CONDUCT.md` adopts the Contributor Covenant Code of Conduct, version 2.1. It defines behavioral expectations, reporting channels, enforcement responsibilities, and escalation outcomes.

### Core obligations

- Be welcoming, respectful, and constructive.
- Avoid harassment, insults, personal attacks, or disclosure of private information.
- Respect community leaders’ authority to clarify and enforce standards.
- Follow the scope of the conduct policy in all community spaces and when representing the community publicly.

### Enforcement path

Reports are directed to `conduct@paxeer.app`. The file defines four enforcement outcomes:

- Correction
- Warning
- Temporary Ban
- Permanent Ban

The enforcement section also states that complaints are reviewed promptly and fairly, with privacy and security of reporters protected.

## Licensing and Third-Party Terms

`LICENSE.md` is the repository’s primary license for Sidiora Labs code. It is not a standard permissive license; it is the custom Centra AI Protocol License with explicit copyleft, commercial-trigger, audit, and termination terms.

### `LICENSE.md` summary

| Clause | Source-backed behavior |
| --- | --- |
| Grant | Allows unmodified source and object use, copy, and distribution under the license. |
| Pure Caller Use | Permits integration-only use that interacts through published ABIs, APIs, or RPC endpoints without distributing modified work. |
| Copyleft | Modifications and extensions must be released under the same license, with preserved notices, clear change marking, and reproducibility instructions. |
| Commercial Trigger | A commercial license is required if charged fees exceed USD 100,000 in any rolling year or if liquidity under control exceeds USD 10,000,000. |
| Contact duty | Crossing a trigger requires contacting `license@Paxeer.app` within 15 days to execute a commercial license. |
| Audit | Sidiora Labs may request a yearly audit, and may request an additional for-cause attestation. |
| Patent and trademark terms | The file includes limited patent rights, trademark restrictions, reservation of rights, and no-endorsement terms. |
| Warranty and liability | The work is provided as-is, without warranties, and liability is limited as stated. |
| Termination | Material breach not cured within 15 days terminates the license; prior compliant distributions survive. |
| Governing law | New York law applies, with venue in New York County, New York. |
| Notices and versioning | Notices are sent to `legal@paxeer.app`, and the license may be republished in updated versions. |


### Third-party license inventory

`licenses/README.md` explains that the directory contains the texts for open-source dependencies used by the monorepo, and that Sidiora Labs code is covered separately by `LICENSE.md`.

| File | Responsibility |
| --- | --- |
| `licenses/README.md` | Catalogs third-party dependencies by license family and usage, and points readers to the canonical license texts in the directory. |
| `licenses/MIT.txt` | Canonical MIT license text used for dependencies including `github.com/fxamacker/cbor/v2`, `github.com/lib/pq`, `github.com/jackc/pgx/v5`, `github.com/creack/pty`, `github.com/klauspost/compress`, `github.com/stretchr/testify`, and `@modelcontextprotocol/sdk`. |
| `licenses/Apache-2.0.txt` | Canonical Apache 2.0 text used for dependencies such as `github.com/cockroachdb/pebble`, `github.com/oklog/ulid/v2`, `github.com/prometheus/client_golang`, `github.com/prometheus/common`, and `github.com/prometheus/procfs`. |
| `licenses/BSD-2-Clause.txt` | Canonical BSD 2-Clause text used for dependencies including `github.com/pkg/errors`. |
| `licenses/BSD-3-Clause.txt` | Canonical BSD 3-Clause text used for dependencies including `golang.org/x/time`, `golang.org/x/crypto`, `golang.org/x/sys`, `golang.org/x/text`, `golang.org/x/exp`, `golang.org/x/sync`, `golang.org/x/net`, `golang.org/x/tools`, `google.golang.org/protobuf`, `github.com/gorilla/websocket`, `github.com/gogo/protobuf`, `github.com/DataDog/zstd`, `github.com/golang/snappy`, `github.com/rogpeppe/go-internal`, and `github.com/golang/protobuf`. |


### Practical reading order for contributors

1. Read `CODE_OF_CONDUCT.md` for expected behavior.
2. Read `rules/common/development-workflow.md` for the implementation process.
3. Read `rules/common/code-review.md` for review gates and escalation.
4. Read `CHANGELOG.md` to understand release-note practice and how changes are recorded.
5. Read `LICENSE.md` and `licenses/README.md` for legal terms and third-party license coverage.
6. Use the Chinese mirrors in `rules/zh/` when working in that language set.

## Key Files Reference

| File | Responsibility |
| --- | --- |
| `CODE_OF_CONDUCT.md` | Defines the repository’s community standards, reporting channel, and enforcement ladder under Contributor Covenant 2.1. |
| `CHANGELOG.md` | Records all notable changes, follows Keep a Changelog structure, and uses Semantic Versioning with session and decision cross-references. |
| `LICENSE.md` | Sets the repository’s custom Centra AI Protocol License terms, including copyleft, commercial triggers, audit rights, warranty limits, and termination. |
| `licenses/README.md` | Catalogs third-party licenses and explains which dependencies fall under each one. |
| `licenses/MIT.txt` | Canonical MIT text for listed dependencies. |
| `licenses/Apache-2.0.txt` | Canonical Apache 2.0 text for listed dependencies. |
| `licenses/BSD-2-Clause.txt` | Canonical BSD 2-Clause text for listed dependencies. |
| `licenses/BSD-3-Clause.txt` | Canonical BSD 3-Clause text for listed dependencies. |
| `rules/common/development-workflow.md` | Defines the common contribution workflow from research through pre-review checks. |
| `rules/common/git-workflow.md` | Provides the git workflow reference used for commit message format and PR process. |
| `rules/common/code-review.md` | Defines review triggers, checklist items, severity levels, and review-agent usage. |
| `rules/zh/development-workflow.md` | Chinese mirror of the development workflow. |
| `rules/zh/code-review.md` | Chinese mirror of the review policy. |
| `rules/zh/README.md` | Explains the rule-set hierarchy and language-specific precedence in Chinese. |
