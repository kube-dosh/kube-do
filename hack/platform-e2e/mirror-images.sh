#!/usr/bin/env bash

set -euo pipefail

PLATFORM_E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$PLATFORM_E2E_DIR/lib.sh"

usage() {
  echo "usage: $0 <manifest.yaml> <arch> <registry-address>" >&2
  exit 2
}

[[ $# -eq 3 ]] || usage
MANIFEST_FILE=$(realpath "$1")
ARCH=$2
REGISTRY_ADDRESS=$3
[[ -f "$MANIFEST_FILE" ]] || die "manifest not found: $MANIFEST_FILE"
require_command podman

WORKERS=${KUBE_DO_E2E_IMAGE_JOBS:-1}
WORKER=${KUBE_DO_E2E_MIRROR_WORKER:-}
if [[ -z "$WORKER" && "$WORKERS" -gt 1 ]]; then
  pids=()
  for worker in $(seq 0 $((WORKERS - 1))); do
    KUBE_DO_E2E_MIRROR_WORKER=$worker KUBE_DO_E2E_MIRROR_WORKERS=$WORKERS \
      "$0" "$@" &
    pids+=("$!")
  done
  status=0
  for pid in "${pids[@]}"; do
    wait "$pid" || status=1
  done
  exit "$status"
fi
WORKER=${WORKER:-0}
WORKER_COUNT=${KUBE_DO_E2E_MIRROR_WORKERS:-1}

NAMESPACE=$(python3 - "$MANIFEST_FILE" <<'PY'
import sys, yaml
print(yaml.safe_load(open(sys.argv[1], encoding="utf-8"))["spec"]["registry"]["namespace"])
PY
)
ROWS=$(python3 - "$MANIFEST_FILE" "$WORKER" "$WORKER_COUNT" <<'PY'
import sys, yaml
data = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
worker, count = int(sys.argv[2]), int(sys.argv[3])
for index, image in enumerate(data["spec"].get("images", [])):
    if index % count != worker:
        continue
    print("\t".join((image["source"], image["target"])))
PY
)

CONTAINER_ROOT="/var/lib/kube-do-e2e/containers-$ARCH-$WORKER"
CONTAINER_RUNROOT="/run/kube-do-e2e-containers-$ARCH-$WORKER"
install -d -m 0755 "$CONTAINER_ROOT" "$CONTAINER_RUNROOT"

PROXY=$(host_proxy_url)
if [[ -n "$PROXY" ]]; then
  export HTTPS_PROXY=$PROXY
  export HTTP_PROXY=$PROXY
  export https_proxy=$PROXY
  export http_proxy=$PROXY
  export NO_PROXY="127.0.0.1,localhost,cdn.registry.k8s.io,${REGISTRY_ADDRESS%:*}"
  export no_proxy=$NO_PROXY
fi

PODMAN=(podman --root "$CONTAINER_ROOT" --runroot "$CONTAINER_RUNROOT")

image_is_preloaded() {
  local target=$1
  python3 - "$REGISTRY_ADDRESS" "$NAMESPACE" "$target" "$ARCH" <<'PY'
import json
import ssl
import sys
import urllib.request

registry, namespace, target, arch = sys.argv[1:]
repository, tag = target.rsplit(":", 1)
context = ssl._create_unverified_context()
try:
    manifest_url = f"https://{registry}/v2/{namespace}/{repository}/manifests/{tag}"
    request = urllib.request.Request(
        manifest_url,
        headers={"Accept": "application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json"},
    )
    manifest = json.load(urllib.request.urlopen(request, context=context, timeout=15))
    config_digest = manifest["config"]["digest"]
    config_url = f"https://{registry}/v2/{namespace}/{repository}/blobs/{config_digest}"
    config = json.load(urllib.request.urlopen(config_url, context=context, timeout=15))
except Exception:
    sys.exit(1)
sys.exit(0 if config.get("architecture") == arch else 1)
PY
}

while IFS=$'\t' read -r source target; do
  [[ -n "$source" ]] || continue
  target_ref="$REGISTRY_ADDRESS/$NAMESPACE/$target"
  if image_is_preloaded "$target"; then
    log "reusing preloaded $target_ref"
    continue
  fi
  log "pulling $source (linux/$ARCH)"
  "${PODMAN[@]}" pull --quiet --platform "linux/$ARCH" --retry 5 --retry-delay 3s "$source"
  log "pushing $target_ref"
  "${PODMAN[@]}" push --quiet --tls-verify=false --format v2s2 "$source" "docker://$target_ref"
done <<<"$ROWS"
