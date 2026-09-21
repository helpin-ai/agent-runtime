#!/usr/bin/env bash
set -euo pipefail

image_name="${1:?usage: bash scripts/container-toolchain-smoke.sh <image>}"

docker run --rm \
  --read-only \
  --tmpfs /tmp:rw,nosuid,nodev,size=512m \
  --entrypoint /bin/sh \
  "$image_name" -eu -c '
    export HOME=/tmp/agent-runtime-smoke/home
    export XDG_CACHE_HOME=/tmp/agent-runtime-smoke/cache
    export XDG_CONFIG_HOME=/tmp/agent-runtime-smoke/config
    export XDG_DATA_HOME=/tmp/agent-runtime-smoke/data
    mkdir -p "$HOME" "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME"

    test "$(id -u)" = "1000"
    test "$(node --version)" = "v24.21.0"
    agent-browser --version
    test -f /usr/share/zoneinfo/UTC
    export AGENT_BROWSER_SOCKET_DIR=/tmp/agent-browser-smoke
    agent-browser --session image-smoke --json open about:blank >/tmp/browser-open.json
    agent-browser --session image-smoke --json close >/tmp/browser-close.json

    for command_name in \
      cargo chromium curl git go make node npm npx pip pip3 pnpm \
      poetry pytest python python3 rg rustc uv yarn
    do
      command -v "$command_name" >/dev/null
    done

    for excluded_scanner in codex opencode gitleaks semgrep trivy
    do
      if command -v "$excluded_scanner" >/dev/null 2>&1; then
        echo "excluded scanner unexpectedly present: $excluded_scanner" >&2
        exit 1
      fi
    done

    cargo --version
    curl --version
    git --version
    go version
    make --version
    node --version
    npm --version
    npx --version
    pip --version
    pip3 --version
    pnpm --version
    poetry --version
    pytest --version
    python --version
    python3 --version
    rg --version
    rustc --version
    uv --version
    yarn --version
  '
