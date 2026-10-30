VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin

.PHONY: build test test-integration lint cover web web-dev web-lint web-test web-cover e2e

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

test: web-test
	go test -race ./...

# Needs Postgres: set FORECREW_TEST_DATABASE_URL, for example
# postgres://postgres:postgres@localhost:5432/forecrew_test?sslmode=disable
test-integration:
	@test -n "$$FORECREW_TEST_DATABASE_URL" || { echo "FORECREW_TEST_DATABASE_URL is not set"; exit 1; }
	go test -race -tags integration ./...

lint: web-lint
	golangci-lint run ./...

# The Postgres store counts towards coverage only through its integration
# tests, so cover needs the same database as test-integration.
cover: web-cover
	@test -n "$$FORECREW_TEST_DATABASE_URL" || { echo "set FORECREW_TEST_DATABASE_URL; see make test-integration"; exit 1; }
	./scripts/cover.sh

# The web UI (docs/ui.md). Needs Node and pnpm; Go-only builds and tests do not.
WEB_DEPS := web/node_modules/.stamp

$(WEB_DEPS): web/package.json web/pnpm-lock.yaml
	cd web && pnpm install --frozen-lockfile
	@touch $@

# Builds the UI into internal/webui/dist, which forecrewd embeds.
web: $(WEB_DEPS)
	cd web && pnpm build

# Vite dev server on :5173 proxying /api to a forecrewd on localhost:8080
# (set FORECREW_URL for another address; see docs/ui.md).
web-dev: $(WEB_DEPS)
	cd web && pnpm dev

web-lint: $(WEB_DEPS)
	cd web && pnpm lint

web-test: $(WEB_DEPS)
	cd web && pnpm test

# Fails below 80% line coverage of web/src, without shadcn's components/ui.
web-cover: $(WEB_DEPS)
	cd web && pnpm cover

# Playwright against the production build served by vite preview. Set
# PLAYWRIGHT_CHROMIUM_PATH to use a Chromium you already have.
e2e: web
	cd web && $(if $(PLAYWRIGHT_CHROMIUM_PATH),,pnpm exec playwright install chromium &&) pnpm e2e
