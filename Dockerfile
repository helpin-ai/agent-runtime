FROM golang:1.24.3-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agent-runtime ./cmd/agent-runtime
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-mcp-bridge ./cmd/agent-runtime-mcp-bridge

FROM node:20.20.2-bookworm-slim

ARG DEBIAN_FRONTEND=noninteractive
ARG CODEX_VERSION=0.146.0
ARG OPENCODE_VERSION=1.18.9
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
		"@openai/codex@${CODEX_VERSION}" \
		"agent-browser@${AGENT_BROWSER_VERSION}" \
		"opencode-ai@${OPENCODE_VERSION}" \
		"pnpm@${PNPM_VERSION}" \
	&& test "$(yarn --version)" = "${YARN_VERSION}" \
	&& npm cache clean --force \
	&& rm -rf /root/.cache /root/.npm /var/lib/apt/lists/*
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]
