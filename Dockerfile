FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agent-runtime ./cmd/agent-runtime
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-mcp-bridge ./cmd/agent-runtime-mcp-bridge

FROM node:20-bookworm-slim
# The slim image omits both the CA bundle needed for outbound TLS and Git,
# which repository-backed runs invoke to prepare and persist workspaces.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates git \
	&& rm -rf /var/lib/apt/lists/* \
	&& npm i -g @openai/codex
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]
