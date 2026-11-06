#!/bin/sh
# A mke2fs that runs in a Debian 13 container, for hosts whose own is older than
# 1.47.1 or lacks libarchive (Ubuntu 24.04, for one). Use it as
# MKE2FS=deploy/images/mke2fs-docker.sh deploy/images/build.sh rootfs ...
# or `forecrew image rootfs --mke2fs deploy/images/mke2fs-docker.sh`. The paths
# mke2fs is given are mounted at the same place in the container.
set -eu

image=forecrew-mke2fs:debian13
if ! docker image inspect "$image" >/dev/null 2>&1; then
	docker build -q -t "$image" - >/dev/null <<'DOCKERFILE'
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update && apt-get install -y --no-install-recommends e2fsprogs libarchive13t64 && rm -rf /var/lib/apt/lists/*
DOCKERFILE
fi
tmp=${TMPDIR:-/tmp}
exec docker run --rm -i --network none --user "$(id -u):$(id -g)" -v "$tmp:$tmp" -v "$PWD:$PWD" -w "$PWD" "$image" mke2fs "$@"
