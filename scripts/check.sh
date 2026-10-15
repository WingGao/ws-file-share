#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")/.."
export GOMODCACHE="$PWD/.cache/gomod"
export GOCACHE="$PWD/.cache/gobuild"
go test ./... -count=1
go vet ./...
go build -o .cache/ws-test ./cmd/ws-downloader
node --test client/client.test.cjs
