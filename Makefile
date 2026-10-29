VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin

.PHONY: build test test-integration lint cover

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

test:
	go test -race ./...

# Needs Postgres: set FORECREW_TEST_DATABASE_URL, for example
# postgres://postgres:postgres@localhost:5432/forecrew_test?sslmode=disable
test-integration:
	@test -n "$$FORECREW_TEST_DATABASE_URL" || { echo "FORECREW_TEST_DATABASE_URL is not set"; exit 1; }
	go test -race -tags integration ./...

lint:
	golangci-lint run ./...

# The Postgres store counts towards coverage only through its integration
# tests, so cover needs the same database as test-integration.
cover:
	@test -n "$$FORECREW_TEST_DATABASE_URL" || { echo "set FORECREW_TEST_DATABASE_URL; see make test-integration"; exit 1; }
	./scripts/cover.sh
