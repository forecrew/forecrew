# syntax=docker/dockerfile:1
# Build context: the repository root.
#   docker build -f deploy/compose/Dockerfile -t forecrewd .

# T18 adds a Node stage here that builds the web UI before the Go build.

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/ ./cmd/forecrewd ./cmd/forecrew
# The certificate cache must exist and belong to the unprivileged user so a
# named volume mounted on it is writable.
RUN mkdir -p /out/certs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forecrewd /out/forecrew /usr/local/bin/
COPY --from=build --chown=nonroot:nonroot /out/certs /var/lib/forecrew/certs
USER nonroot
EXPOSE 80 443
ENTRYPOINT ["/usr/local/bin/forecrewd"]
CMD ["--config", "/etc/forecrew/forecrew.yaml"]
