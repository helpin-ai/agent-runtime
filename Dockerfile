FROM golang:1.24.3-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GRAMMAR_TAGS selects which tree-sitter grammars the symbol tools link in.
# Without `grammar_subset` the runtime embeds all 206 grammars (~+30MB per
# binary) instead of only the ones we support (~+12MB). Keep this list in sync
# with extensionLanguages in internal/symbols/queries.go; a language listed
# there but missing here degrades to the legacy regex outline at runtime.
ARG GRAMMAR_TAGS="grammar_subset grammar_subset_go grammar_subset_typescript grammar_subset_tsx grammar_subset_javascript grammar_subset_python grammar_subset_rust grammar_subset_java"
RUN CGO_ENABLED=0 go build -tags "${GRAMMAR_TAGS}" -o /out/agent-runtime ./cmd/agent-runtime
RUN CGO_ENABLED=0 go build -tags "${GRAMMAR_TAGS}" -ldflags "-X main.codingSupported=false" -o /out/agent-runtime-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 go build -tags "${GRAMMAR_TAGS}" -o /out/agent-runtime-coding-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 go build -tags "${GRAMMAR_TAGS}" -o /out/agent-runtime-mcp-bridge ./cmd/agent-runtime-mcp-bridge

FROM node:20.20.2-bookworm-slim AS coding

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
COPY --from=build /usr/local/go /usr/local/go

# Repository-backed agents need the same general-purpose development
# toolchain that previously lived in Helpin's Temporal worker image. Security
# scanners are intentionally packaged separately and are not installed here.
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
		ripgrep \
		rustc \
		tzdata \
	&& python3 -m pip install --no-cache-dir --break-system-packages \
		"poetry==${POETRY_VERSION}" \
		"pytest==${PYTEST_VERSION}" \
		"uv==${UV_VERSION}" \
	&& npm install --global --no-audit --no-fund \
		"agent-browser@${AGENT_BROWSER_VERSION}" \
		"pnpm@${PNPM_VERSION}" \
	&& test "$(yarn --version)" = "${YARN_VERSION}" \
	&& npm cache clean --force \
	&& rm -rf /root/.cache /root/.npm /var/lib/apt/lists/*
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-coding-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]

# Default image: support execution and browser tools, without a coding toolchain.
FROM node:20.20.2-bookworm-slim AS default
ARG AGENT_BROWSER_VERSION=0.25.5
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg git ripgrep tzdata \
    && npm install --global --no-audit --no-fund "agent-browser@${AGENT_BROWSER_VERSION}" \
    && npm cache clean --force \
    && rm -rf /root/.cache /root/.npm /var/lib/apt/lists/*
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]
