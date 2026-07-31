SHELL := /bin/sh

STATICCHECK_VERSION := v0.7.0
GOVULNCHECK_VERSION := v1.6.0

.PHONY: fmt-check tidy vet staticcheck test coverage race build vuln check

fmt-check:
	@test -z "$$(gofmt -l cmd internal)"

tidy:
	go mod tidy

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

test:
	go test ./...

coverage:
	go test ./... -cover

race:
	go test -race ./...

build:
	go build ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

check: fmt-check tidy vet staticcheck test coverage race build vuln
