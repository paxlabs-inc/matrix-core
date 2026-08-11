# Third-Party Licenses

This directory contains the license texts for the open-source dependencies
used by the Centra AI monorepo. Sidiora Labs code is covered separately by
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

## Neocortex Native Sources

Neocortex builds without downloading packages: its exact sources and digests
are recorded in `neocortex/third_party/LOCK.json`, and the corresponding
license files remain beside each vendored source tree.

| Component | Version | License | Vendored path |
|-----------|---------|---------|---------------|
| liburing | 2.15 | MIT (library); GPL-2.0 material is separately identified upstream | `neocortex/third_party/liburing` |
| LMDB | 1.0.0 | OpenLDAP Public License 2.8 | `neocortex/third_party/lmdb` |
| BLAKE3 | 1.8.5 | CC0-1.0 / Apache-2.0 / Apache-2.0 with LLVM exceptions | `neocortex/third_party/blake3` |
| libsodium | 1.0.22 | ISC, with separately identified bundled material | `neocortex/third_party/libsodium` |
| FlatBuffers | 25.12.19 | Apache-2.0 | `neocortex/third_party/flatbuffers` |
| CRoaring | 4.7.2 | Apache-2.0 or MIT | `neocortex/third_party/croaring` |
| Highway | 1.4.0 | Apache-2.0 or BSD-3-Clause, with separately identified CC0 material | `neocortex/third_party/highway` |
| xxHash | 0.8.3 | BSD-2-Clause library; GPL-2.0 command-line/test material | `neocortex/third_party/hash/xxhash` |
| crc32c | 1.1.2 | BSD-3-Clause | `neocortex/third_party/hash/crc32c` |
| LLVM compiler-rt | 18.1.3 | Apache-2.0 with LLVM exceptions and component notices | `neocortex/toolchain/compiler-rt` |

The pinned cross-build sysroots under `neocortex/toolchain/sysroots/` retain
their package-level notices and applicable GNU runtime/library terms in-tree.

---

## License Texts

Full license texts are in this directory:

- [`Apache-2.0.txt`](./Apache-2.0.txt)
- [`MIT.txt`](./MIT.txt)
- [`BSD-3-Clause.txt`](./BSD-3-Clause.txt)
- [`BSD-2-Clause.txt`](./BSD-2-Clause.txt)
- [`Apache-2.0 with LLVM exceptions`](./Apache-2.0-LLVM.txt)
- [`CC0-1.0`](./CC0-1.0.txt)
- [`GPL-2.0`](./GPL-2.txt)
- [`LGPL-2.1`](./LGPL-2.1.txt)
- [`OpenLDAP Public License`](./OpenLDAP-Public-License.txt)
