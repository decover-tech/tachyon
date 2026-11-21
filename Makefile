.PHONY: all build test lint proto check

all: check

build:
	go build -o bin/ ./cmd/...
	cd hostd && cargo build

test:
	go test -race ./...
	cd hostd && cargo test

lint:
	go vet ./...
	buf lint
	cd hostd && cargo fmt --check && cargo clippy --all-targets -- -D warnings

# Regenerates gen/ (Go). hostd generates its own code in build.rs.
proto:
	buf generate

check: lint test
