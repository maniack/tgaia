.DEFAULT_GOAL := help

GO ?= go
BINARY ?= bin/tgaia
TEST_ARGS ?=

.PHONY: help build test test-race vet check

help:
	@printf '%s\n' \
	  'make build      Build the binary (default: bin/tgaia, CGO disabled)' \
	  'make test       Run all tests (CGO disabled)' \
	  'make test-race  Run all tests with the race detector (requires C compiler)' \
	  'make vet        Run go vet' \
	  'make check      Run tests and go vet' \
	  'Overrides: GO, BINARY, TEST_ARGS; standard Go environment variables are respected.'

build:
	@mkdir -p "$(dir $(BINARY))"
	CGO_ENABLED=0 $(GO) build -trimpath -o "$(BINARY)" ./cmd/tgaia

test:
	CGO_ENABLED=0 $(GO) test $(TEST_ARGS) ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race $(TEST_ARGS) ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

check: test vet
