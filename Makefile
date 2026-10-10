VERSION ?= dev

.PHONY: build test lint bench

build:
	go build -ldflags "-X skald/internal/cli.Version=$(VERSION)" -o skald ./cmd/skald

test:
	go test ./...

lint:
	go vet ./...

bench:
	go run ./cmd/skald-bench -procs 4
	go run ./cmd/skald-bench -procs 8
