#!/usr/bin/env bash
set -euo pipefail
image_name="${1:?usage: bash scripts/container-support-smoke.sh <image>}"
docker run --rm --entrypoint /bin/sh "$image_name" -eu -c '
  for command_name in codex opencode cargo rustc go gcc g++ make python python3; do
    if command -v "$command_name" >/dev/null 2>&1; then
      echo "unexpected support toolchain: $command_name" >&2
      exit 1
    fi
  done
  if /agent-runtime-worker --coding >/tmp/coding-error 2>&1; then
    echo "support worker accepted coding" >&2
    exit 1
  fi
  grep -q "coding" /tmp/coding-error
'
