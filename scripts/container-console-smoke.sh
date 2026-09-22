#!/usr/bin/env bash
set -euo pipefail
image_name="${1:?usage: bash scripts/container-console-smoke.sh <image>}"
container_id=$(docker run --detach "$image_name")
trap 'docker rm --force "$container_id" >/dev/null' EXIT

# Exercise the real entrypoint and HTTP server without a backend or host port.
for attempt in {1..30}; do
  if docker exec "$container_id" node -e '
    fetch("http://127.0.0.1:3000/api/health", { signal: AbortSignal.timeout(2000) })
      .then(async r => {
        if (!r.ok || (await r.json()).status !== "ok") process.exit(1)
      })
      .catch(() => process.exit(1))
  '; then
    exit 0
  fi
  sleep 1
done
docker logs "$container_id"
echo "console health check failed" >&2
exit 1
