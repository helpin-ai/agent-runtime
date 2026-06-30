#!/usr/bin/env bash
# Boot agent-runtime locally with the Usermaven .env sourced.
set -euo pipefail
cd "$(dirname "$0")"
set -a
# shellcheck disable=SC1091
source .env
set +a
exec go run ./cmd/agent-runtime
