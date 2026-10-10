VERSION ?= dev

.PHONY: build test lint bench conc release release-check

build:
	go build -ldflags "-X skald/internal/cli.Version=$(VERSION)" -o skald ./cmd/skald

test:
	go test ./...

lint:
	go vet ./...

bench:
	go run ./cmd/skald-bench -procs 4
	go run ./cmd/skald-bench -procs 8

# Multi-process concurrency test. SKALD_CONC_RACE=1 builds the binary with
# -race; SKALD_CONC_CLI and SKALD_CONC_CYCLES scale the workload.
conc:
	go test -count=1 -timeout 600s -run TestConcurrency ./internal/conctest/

# Cross-compile and package release archives into dist/. Pass VERSION=... to
# override the git-derived version and ALLOW_DIRTY=1 for a throwaway build.
release:
	./scripts/release.sh build

# Verify dist/ (checksums, binary types, version) and smoke-test the native binary.
release-check:
	./scripts/release.sh check
