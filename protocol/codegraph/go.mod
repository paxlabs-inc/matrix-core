module centra/protocol/codegraph

go 1.25.0

require (
	golang.org/x/tools v0.47.0
	lukechampine.com/blake3 v1.3.0
	centra/core/cortex v0.0.0
	centra/core/mcl v0.0.0
)

require (
	github.com/klauspost/cpuid/v2 v2.0.9 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
)

replace centra/core/cortex => ../../core/cortex

replace centra/core/mcl => ../../core/mcl

replace centra/packages/vault => ../../packages/vault
