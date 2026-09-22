#!/usr/bin/env bash
set -euo pipefail
: "${VERSION:?release version is required}"
: "${CHANNEL:?release channel is required}"
: "${GITHUB_SHA:?release commit is required}"

fail() { echo "$*" >&2; exit 1; }
[[ "$VERSION" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]] || fail "invalid release version"
[[ "$CHANNEL" == stage || "$CHANNEL" == prod ]] || fail "invalid release channel"
[[ "$GITHUB_SHA" =~ ^[0-9a-f]{40}$ ]] || fail "invalid release commit"
(( $# > 0 && $# % 2 == 0 )) || fail "expected image/digest pairs"
images=("$@")

# Validate every pair before publishing anything. Never fall back to mutable tags.
for ((i=0; i<${#images[@]}; i+=2)); do
  case "${images[i]}" in
    ghcr.io/helpin-ai/agent-runtime|ghcr.io/helpin-ai/agent-runtime-coding|ghcr.io/helpin-ai/agent-runtime-console) ;;
    *) echo "unexpected release image: ${images[i]}" >&2; exit 1 ;;
  esac
  [[ "${images[i+1]}" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "invalid digest for ${images[i]}"
done

# Copy manifests, never rebuild or download layers. Preserve single manifests
# as well as image indexes (including provenance).
for ((i=0; i<${#images[@]}; i+=2)); do
  image="${images[i]}"
  docker buildx imagetools create --prefer-index=false \
    --tag "$image:$VERSION" --tag "$image:sha-$GITHUB_SHA" \
    "$image@${images[i+1]}"
done

# Advance aliases only after all versioned images were published. Tag updates
# are not transactional across images; deployment jobs must wait for all of this.
for ((i=0; i<${#images[@]}; i+=2)); do
  docker buildx imagetools create --prefer-index=false \
    --tag "${images[i]}:$CHANNEL-latest" "${images[i]}@${images[i+1]}"
done
