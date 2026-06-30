# Agent Runtime dev tasks. Run `just` to list recipes.
# Bash so `source .env` and `set -a` work.
set shell := ["bash", "-uc"]

_default:
    @just --list

# ---------------------------------------------------------------- Go runtime --

# Hot-reload the agent-runtime service with air (loads .env). Ctrl-C to stop.
runtime:
    set -a; source .env; set +a; air

# Run the runtime once, no hot reload (plain go run).
runtime-once:
    ./run-local.sh

# Build the runtime binary to ./tmp.
runtime-build:
    go build -o ./tmp/agent-runtime ./cmd/agent-runtime

# Run the durable Temporal worker (loads .env).
worker:
    set -a; source .env; set +a; go run ./cmd/agent-runtime-worker

# Go tests.
test:
    go test ./...

# ------------------------------------------------------------------- Console --

# Console HMR dev server on :3101 (local iteration only — NOT the public URL).
console-dev:
    cd packages/console && npm run dev -- --port 3101

# Build the console production bundle.
console-build:
    cd packages/console && npm run build

# Typecheck the console.
console-check:
    cd packages/console && npm run typecheck

# Restart the systemd-served console.
console-restart:
    sudo systemctl restart agent-runtime-console

# Publish console changes to the served URL: build + restart.
console-deploy: console-build console-restart

# Follow the served console's logs.
console-logs:
    journalctl -u agent-runtime-console -f --no-hostname
