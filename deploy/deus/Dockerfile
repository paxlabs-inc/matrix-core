# syntax=docker/dockerfile:1
# Deus control plane (deusd) image. See deus/docs/13-deployment.md §13.2.
# Build context is the REPO ROOT: deus/go.mod carries
# `replace github.com/paxlabs-inc/layerx => ../layerx`, so the sibling layerx
# module must be present in the build tree.

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY layerx/go.mod layerx/go.sum ./layerx/
COPY deus/go.mod deus/go.sum ./deus/
WORKDIR /src/deus
RUN go mod download
WORKDIR /src
COPY layerx/ ./layerx/
COPY deus/ ./deus/
WORKDIR /src/deus
# CGO left enabled (default): go-ethereum pulls c-kzg-4844/blst (cgo) into the
# import graph; the golang image ships gcc. Runtime debian-slim is glibc-compatible.
RUN go build -trimpath -ldflags="-s -w" -o /out/deusd ./cmd/deusd \
 && go build -trimpath -ldflags="-s -w" -o /out/deusctl ./cmd/deusctl

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ENV DEUS_ROOT=/app
COPY --from=build /out/deusd /usr/local/bin/deusd
COPY --from=build /out/deusctl /usr/local/bin/deusctl
COPY deus/migrations /app/migrations
COPY deus/configs /app/configs
COPY deus/pkg/manifest/schema.json /app/pkg/manifest/schema.json
EXPOSE 9095
ENTRYPOINT ["deusd"]
