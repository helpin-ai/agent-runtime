FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agent-runtime ./cmd/agent-runtime
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-mcp-bridge ./cmd/agent-runtime-mcp-bridge

FROM node:20-bookworm-slim
RUN npm i -g @openai/codex
COPY --from=build --chown=node:node /out/agent-runtime /agent-runtime
COPY --from=build --chown=node:node /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build --chown=node:node /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
USER node
WORKDIR /home/node
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]
