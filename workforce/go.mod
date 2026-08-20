module centra/workforce

go 1.25.0

toolchain go1.25.12

require (
	github.com/jackc/pgx/v5 v5.9.2
	centra/packages/machine v0.0.0
	centra/agents/neo v0.0.0
	centra/packages/vault v0.0.0
)

require (
	github.com/cyberphone/json-canonicalization v0.0.0-20241213102144-19d51d7fe467 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/paxlabs-inc/machine-genome v0.1.1 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)

replace centra/packages/vault => ../packages/vault

replace centra/agents/neo => ../agents/neo

replace centra/packages/machine => ../packages/machine
