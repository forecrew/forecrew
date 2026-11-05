# syntax=docker/dockerfile:1
# The base image of the toolboxes: Node, git and the agent CLIs a job's
# runtime needs inside its container (docs/worker.md, Toolboxes). Toolbox
# Dockerfiles start from it:
#   ARG FORECREW_BASE
#   FROM ${FORECREW_BASE}
# The worker builds it first and passes the tag. Build context: this directory.

# The image is pinned by digest, like the control worker's; bump the tag and
# the digest together.
FROM node:24.22.2-bookworm-slim@sha256:c86ecead9e55a5210ac1727de38a4ca840bf03dcaf34dc9749addbf383b1920c
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# The CLI versions are exact in package.json and package-lock.json pins every
# transitive dependency with its integrity hash. Owned by root, so the agents
# cannot change it.
COPY package.json package-lock.json /opt/forecrew-cli/
RUN cd /opt/forecrew-cli && npm ci --omit=dev --no-audit --no-fund \
    && npm cache clean --force
ENV PATH=/opt/forecrew-cli/node_modules/.bin:$PATH
# Jobs run as the worker's uid with an empty in-memory home (HOME is set by
# the worker); nothing in the image is written to.
WORKDIR /work
