# syntax=docker/dockerfile:1
# The security toolbox (security.yaml): semgrep, gitleaks, osv-scanner and
# trivy on top of the agent base image. The scanners are pinned by version, and
# the downloaded binaries by checksum. Build context: packs/toolboxes.
ARG FORECREW_BASE
FROM ${FORECREW_BASE}

ARG TARGETARCH
ARG SEMGREP_VERSION=1.183.0
ARG GITLEAKS_VERSION=8.30.1
ARG OSV_SCANNER_VERSION=2.7.0
ARG TRIVY_VERSION=0.76.2

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv /opt/semgrep \
    && /opt/semgrep/bin/pip install --no-cache-dir "semgrep==${SEMGREP_VERSION}" \
    && ln -s /opt/semgrep/bin/semgrep /usr/local/bin/semgrep

RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) gl=x64;   gl_sha=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb; osv=amd64; osv_sha=160c6253e2922e9e0a6a9771c5c4c40a4b3c6090b34a3cdd5e80c96cc2e4218b; tv=64bit; tv_sha=bc0fc726e017b8927a62b2d3531d7a84ee01769daf5c99ed866dced54089fdbf ;; \
      arm64) gl=arm64; gl_sha=e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080; osv=arm64; osv_sha=4061ef4c021ed3c47f203b818f337e351336cc5cd67c0274fc36bd1dfc728dd7; tv=ARM64; tv_sha=ab9d51f86e6a5fb936f605faff6b593ac51bc378fff4d3e8b9d58358b9c367c3 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    tmp=$(mktemp -d); cd "$tmp"; \
    curl -fsSLO "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_${gl}.tar.gz"; \
    echo "${gl_sha}  gitleaks_${GITLEAKS_VERSION}_linux_${gl}.tar.gz" | sha256sum -c -; \
    tar -xzf "gitleaks_${GITLEAKS_VERSION}_linux_${gl}.tar.gz" gitleaks; \
    install -m 0755 gitleaks /usr/local/bin/gitleaks; \
    curl -fsSLo osv-scanner "https://github.com/google/osv-scanner/releases/download/v${OSV_SCANNER_VERSION}/osv-scanner_linux_${osv}"; \
    echo "${osv_sha}  osv-scanner" | sha256sum -c -; \
    install -m 0755 osv-scanner /usr/local/bin/osv-scanner; \
    curl -fsSLO "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-${tv}.tar.gz"; \
    echo "${tv_sha}  trivy_${TRIVY_VERSION}_Linux-${tv}.tar.gz" | sha256sum -c -; \
    tar -xzf "trivy_${TRIVY_VERSION}_Linux-${tv}.tar.gz" trivy; \
    install -m 0755 trivy /usr/local/bin/trivy; \
    cd /; rm -rf "$tmp"
