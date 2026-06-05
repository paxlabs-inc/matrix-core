module matrix/gateway

go 1.21

// stdlib-only by deliberate choice (matches MCL/llm posture). The
// Postgres ledger writer is sketched against database/sql with a
// build-tag stub for environments where lib/pq isn't vendored; the
// stub is replaced by a pgx-backed implementation when the gateway
// is wired into the box (see internal/ledger/postgres.go header).
//
// No `replace` directives — the gateway is a leaf module, deliberately
// decoupled from cortex/MCL/bridge/executor. Daemon-side wiring talks
// to the gateway over HTTP, not Go imports. Cross-module refactors
// (e.g. shared header constants) are deferred to a future top-level
// `protocol/` shared module.

require github.com/lib/pq v1.12.3
