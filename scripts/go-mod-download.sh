#!/bin/sh
set -eu

# Retry transient proxy failures before compiling. The last attempt avoids
# HTTP/2 stream errors while retaining the configured proxy and checksum checks.
attempt=1
while [ "$attempt" -le 3 ]; do
  if [ "$attempt" -eq 3 ]; then
    export GODEBUG="${GODEBUG:+$GODEBUG,}http2client=0"
  fi
  if go mod download; then
    exit 0
  fi
  if [ "$attempt" -eq 3 ]; then
    echo "Go module download failed after $attempt attempts" >&2
    exit 1
  fi
  delay=$((attempt * 5))
  echo "Go module download failed; retrying in ${delay}s ($attempt/3)" >&2
  sleep "$delay"
  attempt=$((attempt + 1))
done
