#!/usr/bin/env bash
# Boot agent-runtime locally with the Usermaven .env sourced.
set -euo pipefail
cd "$(dirname "$0")"
set -a
# shellcheck disable=SC1091
source .env
set +a

# Usermaven runs locally on the host while Agent Runtime listens on 127.0.0.1.
# Register its app callback when no shared app configuration has been supplied.
# The callback credential remains owned by Usermaven; read it from that local
# environment instead of duplicating a secret in this repository.
if [[ -z "${AGENT_RUNTIME_APP_CONFIG:-}" ]]; then
  usermaven_env="/home/azhar/projects/usermaven/usermaven/.env"
  if [[ ! -r "$usermaven_env" ]]; then
    echo "Usermaven environment not found: $usermaven_env" >&2
    exit 1
  fi
  usermaven_callback_token="$(
    set -a
    # shellcheck disable=SC1090
    source "$usermaven_env"
    set +a
    printf '%s' "${AGENT_RUNTIME_CALLBACK_TOKEN:-}"
  )"
  if [[ -z "$usermaven_callback_token" ]]; then
    echo "AGENT_RUNTIME_CALLBACK_TOKEN is not configured in $usermaven_env" >&2
    exit 1
  fi
  export USERMAVEN_INTERNAL_API_SECRET="$usermaven_callback_token"
  export AGENT_RUNTIME_APP_CONFIG='{"apps":[{"app_id":"usermaven","event_callbacks":[{"url":"http://127.0.0.1:8000/agent-runtime/events","token_env":"USERMAVEN_INTERNAL_API_SECRET","event_types":["run.completed","run.failed","run.cancelled","run.paused"]}]}]}'
fi

exec go run ./cmd/agent-runtime
