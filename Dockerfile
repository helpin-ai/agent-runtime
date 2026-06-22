FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agent-runtime ./cmd/agent-runtime
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-worker ./cmd/agent-runtime-worker
RUN CGO_ENABLED=0 go build -o /out/agent-runtime-mcp-bridge ./cmd/agent-runtime-mcp-bridge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent-runtime /agent-runtime
COPY --from=build /out/agent-runtime-worker /agent-runtime-worker
COPY --from=build /out/agent-runtime-mcp-bridge /agent-runtime-mcp-bridge
EXPOSE 8090
ENTRYPOINT ["/agent-runtime"]
