#!/usr/bin/env bash
set -euo pipefail
image_name="${1:?usage: bash scripts/container-default-smoke.sh <image>}"
docker run --rm --entrypoint /bin/sh "$image_name" -eu -c '
  test "$(node --version)" = "v24.21.0"
  agent-browser --version
  for command_name in codex opencode cargo rustc go gcc g++ make python python3; do
    if command -v "$command_name" >/dev/null 2>&1; then
      echo "unexpected support toolchain: $command_name" >&2
      exit 1
    fi
  done
  # Execution workers refuse a mounted (@path) app config before touching any
  # backend, so this check needs no database or Temporal.
  if AGENT_RUNTIME_APP_CONFIG="@/nonexistent" /agent-runtime-worker --coding >/tmp/coding-error 2>&1; then
    echo "worker unexpectedly started with a mounted app config" >&2
    exit 1
  fi
  if ! grep -q "require inline AGENT_RUNTIME_APP_CONFIG" /tmp/coding-error; then
    echo "worker did not reject the mounted app config:" >&2
    cat /tmp/coding-error >&2
    exit 1
  fi
  # The landlock-exec subcommand is dispatched before flag parsing and exits
  # 126 when it cannot confine and exec a program.
  set +e
  /agent-runtime-worker landlock-exec >/tmp/landlock-error 2>&1
  landlock_status=$?
  set -e
  if [ "$landlock_status" -ne 126 ]; then
    echo "landlock-exec without arguments exited $landlock_status, want 126:" >&2
    cat /tmp/landlock-error >&2
    exit 1
  fi
'
