module matrix/neo

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	matrix/codegraph v0.0.0
	matrix/construct v0.0.0
	matrix/cortexclient v0.0.0
	matrix/executor v0.0.0-00010101000000-000000000000
	matrix/machine v0.0.0
	matrix/mcl v0.0.0
	matrix/vault v0.0.0
	modernc.org/sqlite v1.33.0
)

require (
	github.com/cyberphone/json-canonicalization v0.0.0-20241213102144-19d51d7fe467 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/klauspost/cpuid/v2 v2.2.8 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/paxlabs-inc/machine-genome v0.1.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.29.0 // indirect
	lukechampine.com/blake3 v1.3.0 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)

replace matrix/cassandra => ../cassandra

replace matrix/construct => ../construct

replace matrix/codegraph => ../codegraph

replace matrix/cortexclient => ../cortexclient

replace matrix/mcl => ../MCL

replace matrix/bridge => ../bridge

replace matrix/executor => ../executor

replace matrix/machine => ../machine

replace matrix/vault => ../vault
