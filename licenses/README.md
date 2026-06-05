# Third-Party Licenses

This directory contains the license texts for the open-source dependencies
used by the Matrix Protocol monorepo. Paxlabs code is covered separately by
`LICENSE.md` at the repository root.

---

## Go Modules

| Module | Version | License | Used by |
|--------|---------|---------|---------|
| `github.com/cockroachdb/pebble` | v1.1.0 | Apache-2.0 | `cortex` |
| `github.com/oklog/ulid/v2` | v2.1.1 | Apache-2.0 | `cortex` |
| `github.com/fxamacker/cbor/v2` | v2.6.0 | MIT | `cortex`, `MCL`, `executor` |
| `github.com/lib/pq` | v1.12.3 | MIT | `gateway` |
| `github.com/jackc/pgx/v5` | v5.5.5 | MIT | `router` |
| `github.com/creack/pty` | v1.1.21 | MIT | `executor` |
| `github.com/gorilla/websocket` | v1.5.3 | BSD-2-Clause | `executor` |
| `golang.org/x/time` | v0.5.0 | BSD-3-Clause | `cortex` |
| `golang.org/x/crypto` | v0.17.0 | BSD-3-Clause | `router` |
| `golang.org/x/sys` | v0.13.0 | BSD-3-Clause | transitive |
| `golang.org/x/text` | v0.14.0 | BSD-3-Clause | transitive |
| `golang.org/x/exp` | various | BSD-3-Clause | transitive |
| `google.golang.org/protobuf` | v1.28.1 | BSD-3-Clause | transitive |
| `github.com/gogo/protobuf` | v1.3.2 | BSD-3-Clause | transitive |
| `github.com/DataDog/zstd` | v1.4.5 | BSD-3-Clause | transitive |
| `github.com/prometheus/client_golang` | v1.16.0 | Apache-2.0 | transitive |
| `github.com/prometheus/common` | v0.44.0 | Apache-2.0 | transitive |
| `github.com/prometheus/procfs` | v0.11.0 | Apache-2.0 | transitive |
| `github.com/golang/snappy` | v0.0.4 | BSD-3-Clause | transitive |
| `github.com/klauspost/compress` | v1.15.15 | MIT / Apache-2.0 | transitive |
| `github.com/pkg/errors` | v0.9.0 | BSD-2-Clause | transitive |
| `github.com/stretchr/testify` | v1.8.4 | MIT | test |
| `github.com/rogpeppe/go-internal` | v1.11.0 | BSD-3-Clause | test |

## Node.js / MCP

| Package | License | Used by |
|---------|---------|---------|
| `@modelcontextprotocol/sdk` | MIT | `tools/paxeer/paxeer-net.mjs` |

---

## License Texts

Full license texts are in this directory:

- [`Apache-2.0.txt`](./Apache-2.0.txt)
- [`MIT.txt`](./MIT.txt)
- [`BSD-3-Clause.txt`](./BSD-3-Clause.txt)
- [`BSD-2-Clause.txt`](./BSD-2-Clause.txt)
