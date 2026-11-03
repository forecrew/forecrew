# syntax=docker/dockerfile:1
# The control worker: forecrew-worker with the runtime CLIs for the read-only
# "control" pool (docs/runbooks/install.md, Control worker). Build context: the
# repository root.
#   docker build -f deploy/worker/control/Dockerfile -t forecrew-worker-control .

# Base images are pinned by digest; bump the tag and the digest together.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# forecrew is the CLI the init step uses to register the worker.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/ ./cmd/forecrew-worker ./cmd/forecrew

# Node runs the runtime CLIs; no language toolchain is installed.
FROM node:24.22.2-bookworm-slim@sha256:c86ecead9e55a5210ac1727de38a4ca840bf03dcaf34dc9749addbf383b1920c
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# The CLI versions are exact in package.json; package-lock.json pins every
# transitive dependency with its integrity hash, and npm ci installs exactly
# that. Owned by root, so the agents cannot change it.
COPY deploy/worker/control/package.json deploy/worker/control/package-lock.json /opt/forecrew-cli/
RUN cd /opt/forecrew-cli && npm ci --omit=dev --no-audit --no-fund \
    && npm cache clean --force
ENV PATH=/opt/forecrew-cli/node_modules/.bin:$PATH
# The uid is fixed: the entrypoint drops to it (control.sh).
RUN groupadd --gid 10001 forecrew \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/forecrew --shell /usr/sbin/nologin forecrew \
    && install -d -o forecrew -g forecrew /var/lib/forecrew-worker/work
COPY --from=build /out/forecrew-worker /out/forecrew /usr/local/bin/
COPY --chmod=0755 deploy/worker/control/control.sh /usr/local/bin/forecrew-control
# Starts as root and runs the worker and its agents as forecrew (see control.sh).
ENTRYPOINT ["/usr/local/bin/forecrew-control"]
CMD ["run"]
