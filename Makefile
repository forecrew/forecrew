VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin

.PHONY: build build-enterprise test test-integration test-docker test-toolbox-browser test-kvm test-microvm images-validate lint cover web web-dev web-lint web-test web-cover e2e test-e2e smoke-live smoke-live-jira

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

# forecrewd with ee/ linked in (docs/enterprise.md).
build-enterprise:
	go build -trimpath -tags enterprise -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

# Both builds: open core, and with the enterprise tag, which adds ee/.
test: web-test
	go test -race ./...
	go test -race -tags enterprise ./...

# Needs Postgres: set FORECREW_TEST_DATABASE_URL, for example
# postgres://postgres:postgres@localhost:5432/forecrew_test?sslmode=disable
test-integration:
	@test -n "$$FORECREW_TEST_DATABASE_URL" || { echo "FORECREW_TEST_DATABASE_URL is not set"; exit 1; }
	go test -race -tags integration ./...
	go test -race -tags 'integration enterprise' ./ee/...

# Needs a Docker engine: the worker's environments run in containers.
test-docker:
	go test -race -count=1 -tags docker ./internal/worker/

# Needs a Docker engine and downloads Chromium (a few hundred MB): the smoke
# test of the browser toolbox, not part of test-docker.
test-toolbox-browser:
	go test -count=1 -tags 'docker browser' -run Browser ./internal/worker/

# Needs /dev/kvm, qemu, Docker with alpine:latest, mke2fs 1.47.1 or newer with libarchive and a
# kernel (FORECREW_TEST_KERNEL, see internal/images/boot_kvm_test.go): boots a
# converted root filesystem and checks that the machine spends its token before
# the image's code runs, and that its worker reaches a fake server.
test-kvm:
	go test -count=1 -tags kvm -run Boots ./internal/images/

# As root, with firecracker and jailer on PATH and FORECREW_TEST_KERNEL (and
# FORECREW_TEST_DOCKER_IMAGE for Docker inside): runs microVMs under the real
# Firecracker and checks isolation and clean-up (internal/fleetagent/microvm).
test-microvm:
	go test -c -tags kvm -o microvm.test ./internal/fleetagent/microvm/
	sudo --preserve-env=FORECREW_TEST_KERNEL,FORECREW_TEST_DOCKER_IMAGE,PATH ./microvm.test -test.run TestMicroVMs -test.v
	rm -f microvm.test

# packer validate on every image template (deploy/images/README.md).
images-validate:
	./deploy/images/validate.sh

# actionlint checks the workflow Forecrew installs in projects; it is skipped
# where it is not installed (CI installs it).
lint: web-lint
	golangci-lint run ./...
	golangci-lint run --build-tags enterprise ./ee/... ./cmd/...
	@if command -v actionlint >/dev/null; then actionlint packs/workflows/*.yml; else echo "actionlint not installed, workflow template not linted"; fi

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

# The in-process end-to-end suite (internal/e2e): forecrewd, real workers and
# the embedded playbooks against Postgres, with a fake GitHub and a scripted
# runtime. Needs FORECREW_TEST_DATABASE_URL and git.
test-e2e:
	@test -n "$$FORECREW_TEST_DATABASE_URL" || { echo "FORECREW_TEST_DATABASE_URL is not set"; exit 1; }
	go test -race -count=1 -tags 'integration e2e' ./internal/e2e/...

# The live smoke test against a real instance and a sandbox repo. It spends
# real model money and is never run in CI; see docs/runbooks/smoke-live.md.
# Needs FORECREW_SMOKE_URL, FORECREW_SMOKE_TOKEN, FORECREW_SMOKE_GITHUB_TOKEN
# and FORECREW_SMOKE_REPO; the report goes to FORECREW_SMOKE_REPORT (relative
# to the repo root), default ./smoke-report.md.
FORECREW_SMOKE_REPORT ?= smoke-report.md

smoke-live:
	@test -n "$$FORECREW_SMOKE_URL" -a -n "$$FORECREW_SMOKE_TOKEN" -a -n "$$FORECREW_SMOKE_GITHUB_TOKEN" -a -n "$$FORECREW_SMOKE_REPO" || { echo "set FORECREW_SMOKE_URL, FORECREW_SMOKE_TOKEN, FORECREW_SMOKE_GITHUB_TOKEN and FORECREW_SMOKE_REPO; see docs/runbooks/smoke-live.md"; exit 1; }
	FORECREW_SMOKE_REPORT="$(abspath $(FORECREW_SMOKE_REPORT))" go test -tags smoke -count=1 -timeout 60m -run TestLive -v ./internal/smoke/

# The Jira section of the live smoke test: the tracker contract against a real
# Jira Data Center and a real Jira Cloud site, each run when its
# FORECREW_SMOKE_JIRA_DC_* or FORECREW_SMOKE_JIRA_CLOUD_* variables are set.
# It creates issues: use a sandbox project. See docs/runbooks/smoke-live.md.
smoke-live-jira:
	@test -n "$$FORECREW_SMOKE_JIRA_DC_URL$$FORECREW_SMOKE_JIRA_CLOUD_URL" || { echo "set FORECREW_SMOKE_JIRA_DC_* or FORECREW_SMOKE_JIRA_CLOUD_*; see docs/runbooks/smoke-live.md"; exit 1; }
	go test -tags smoke -count=1 -timeout 10m -run TestLive -v ./internal/tracker/jira/
