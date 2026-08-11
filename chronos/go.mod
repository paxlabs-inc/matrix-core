module github.com/Sidiora-Labs/centra-llm-agents/chronos

go 1.25.0

toolchain go1.25.12

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/robfig/cron/v3 v3.0.1
	matrix/vault v0.0.0
	matrix/workforce v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)

replace matrix/vault => ../vault

replace matrix/workforce => ../workforce
