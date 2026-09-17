FROM --platform=$BUILDPLATFORM golang:1.26.7-bookworm AS build
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY scripts/go-mod-download.sh scripts/go-mod-download.sh
RUN sh scripts/go-mod-download.sh
COPY . .
# GRAMMAR_TAGS selects which tree-sitter grammars the symbol tools link in.
# Without `grammar_subset` the runtime embeds all 206 grammars (~+30MB per
# binary) instead of only the ones we support (~+12MB). Keep this list in sync
# with extensionLanguages in internal/symbols/queries.go; a language listed
# there but missing here degrades to the legacy regex outline at runtime.
ARG GRAMMAR_TAGS="grammar_subset grammar_subset_go grammar_subset_typescript grammar_subset_tsx grammar_subset_javascript grammar_subset_python grammar_subset_rust grammar_subset_java"
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -tags "${GRAMMAR_TAGS}" -o /out/agent-runtime ./cmd/agent-runtime
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -tags "${GRAMMAR_TAGS}" -o /out/agent-runtime-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -tags "${GRAMMAR_TAGS}" -o /out/agent-runtime-mcp-bridge ./cmd/agent-runtime-mcp-bridge

FROM golang:1.26.7-bookworm AS coding-toolchain

FROM node:24.21.0-bookworm-slim@sha256:2fe369e969550cde8e867afc3fe370b260140cab4a23d467074295b42163d553 AS coding
COPY LICENSE NOTICE /usr/share/licenses/agent-runtime/

ARG DEBIAN_FRONTEND=noninteractive
ARG AGENT_BROWSER_VERSION=0.25.5
ARG PNPM_VERSION=10.32.1
ARG YARN_VERSION=1.22.22
ARG PYTEST_VERSION=9.1.1
ARG UV_VERSION=0.12.0
ARG POETRY_VERSION=2.4.1

ENV PATH="/usr/local/go/bin:${PATH}" \
	PIP_DISABLE_PIP_VERSION_CHECK=1 \
	PYTHONDONTWRITEBYTECODE=1

# Keep the build and execution Go versions identical without carrying the Go
# build cache or source tree into the runtime image.
COPY --from=coding-toolchain /usr/local/go /usr/local/go

# Repository-backed agents need the same general-purpose development
# toolchain that previously lived in Helpin's Temporal worker image. Security
# scanners are intentionally packaged separately and are not installed here.
# npm requires an explicit allowlist for the pinned browser binary's postinstall.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		build-essential \
		ca-certificates \
		cargo \
		curl \
		ffmpeg \
		git \
		python-is-python3 \
		python3 \
		python3-pip \
		python3-venv \
		ripgrep \
		rustc \
		tzdata \
	&& python3 -m pip install --no-cache-dir --break-system-packages \
		"poetry==${POETRY_VERSION}" \
		"pytest==${PYTEST_VERSION}" \
		"uv==${UV_VERSION}" \
	&& npm install --global --no-audit --no-fund --allow-scripts=agent-browser \
		"agent-browser@${AGENT_BROWSER_VERSION}" \
		"pnpm@${PNPM_VERSION}" \
	&& test "$(yarn --version)" = "${YARN_VERSION}" \
	&& npm cache clean --force \
	&& rm -rf /root/.cache /root/.npm /var/lib/apt/lists/*
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
RUN mkdir -p /tmp/agent-runtime-workspaces && chown node:node /tmp/agent-runtime-workspaces
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]

# Support bundle: app tools and durable ordinary workers. Browser/coding remain
# opt-in images; no Node package manager or global browser package is shipped.
FROM debian:bookworm-slim AS community
COPY LICENSE NOTICE /usr/share/licenses/agent-runtime/
RUN apt-get update && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends ca-certificates curl git python3 python3-pip python3-venv ripgrep tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home runtime
COPY --from=build --chown=runtime:runtime /out/agent-runtime /agent-runtime
COPY --from=build --chown=runtime:runtime /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=runtime:runtime /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
RUN mkdir -p /tmp/agent-runtime-workspaces && chown runtime:runtime /tmp/agent-runtime-workspaces
USER runtime
WORKDIR /home/runtime
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]

# Default image: support execution and browser tools, without a coding toolchain.
FROM node:24.21.0-bookworm-slim@sha256:2fe369e969550cde8e867afc3fe370b260140cab4a23d467074295b42163d553 AS default
COPY LICENSE NOTICE /usr/share/licenses/agent-runtime/
ARG AGENT_BROWSER_VERSION=0.25.5
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg git ripgrep tzdata \
    && npm install --global --no-audit --no-fund --allow-scripts=agent-browser "agent-browser@${AGENT_BROWSER_VERSION}" \
    && npm cache clean --force \
    && rm -rf /root/.cache /root/.npm /var/lib/apt/lists/*
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]
