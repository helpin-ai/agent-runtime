#!/usr/bin/env bash
set -euo pipefail

SOURCE_NAMESPACE="${SOURCE_NAMESPACE:-helpin}"
TARGET_NAMESPACE="${TARGET_NAMESPACE:-agent-runtime}"

for command in kubectl jq; do
  if ! command -v "${command}" >/dev/null 2>&1; then
    echo "${command} is required" >&2
    exit 1
  fi
done

kubectl get namespace "${TARGET_NAMESPACE}" >/dev/null

if (( $# > 0 )); then
  secrets=("$@")
else
  secrets=(cert-stage-helpin-wildcard cert-prod-helpin-wildcard)
fi

for secret in "${secrets[@]}"; do
  echo "Syncing ${SOURCE_NAMESPACE}/${secret} to ${TARGET_NAMESPACE}/${secret}"
  kubectl --namespace "${SOURCE_NAMESPACE}" get secret "${secret}" --output json \
    | jq --arg namespace "${TARGET_NAMESPACE}" '
        .metadata = {
          name: .metadata.name,
          namespace: $namespace,
          labels: (.metadata.labels // {})
        }
      ' \
    | kubectl apply --server-side --field-manager=agent-runtime-tls-sync --filename -
done

echo "TLS secrets are present in ${TARGET_NAMESPACE}."
