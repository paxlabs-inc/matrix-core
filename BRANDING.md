# Centra AI Branding and Compatibility Contract

The public product brand is **Centra AI**, shortened to **Centra**. The canonical source repository is:

```text
github.com/Sidiora-Labs/centra-llm-agents
```

Use Centra AI in product copy, documentation, UI labels, release display names, and new private package names.

## Stable compatibility identifiers

The rebrand does not rename established machine-facing contracts. Keep these identifiers until each receives its own versioned migration plan:

- Environment variables beginning with `MATRIX_`
- HTTP headers beginning with `X-Matrix-`
- `matrix://` URIs and `did:matrix` identities
- Existing binary, Railway service, container image, and deployment service names
- Go module and import paths beginning with `matrix/`
- Existing filesystem paths such as `/root/matrix` and `/opt/matrix`

Do not silently alias or remove these identifiers during cosmetic branding work. A future rename must define compatibility duration, dual-read or dual-write behavior where appropriate, rollout order, and rollback criteria.

## License identity

The canonical copyright and SPDX header is:

```text
// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
```

The public license name is **Centra AI Protocol License**.

## Container registry namespace

Existing images remain in the established registry namespace by default:

```text
ghcr.io/paxlabs-inc/matrix-core
```

Renaming or transferring the source repository does not require moving those packages. Docker publishing uses `GHCR_NAMESPACE` so the image namespace stays explicit instead of following `github.repository`. Cross-organization writes require a scoped `GHCR_TOKEN` with package write access to the target organization; never store or print that token in source, logs, examples, or documentation.

Moving images to a new namespace is a separate release migration and must preserve old tags or publish compatibility tags until every deployment consumer has moved.
