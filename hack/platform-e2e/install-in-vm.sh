#!/usr/bin/env bash

set -euo pipefail

if [[ $# -lt 4 ]]; then
  echo "usage: $0 <cluster.yaml> <ansible-dir> <cache-dir> <work-dir> [extra-vars-file] [ops-timeout]" >&2
  exit 2
fi

CONFIG=$(realpath "$1")
ANSIBLE_DIR=$(realpath "$2")
CACHE_DIR=$3
WORK_DIR=$4
EXTRA_VARS_FILE=${5:-}
OPS_TIMEOUT=${6:-15m}
KUBE_DO_BIN=${KUBE_DO_BIN:-/opt/kube-do/kube-do}
LOG_DIR="$WORK_DIR/logs"

mkdir -p "$CACHE_DIR" "$WORK_DIR" "$LOG_DIR"
exec > >(tee -a "$LOG_DIR/install.log") 2>&1

if [[ -n "${KUBE_DO_E2E_GUEST_PROXY:-}" ]]; then
  export http_proxy=$KUBE_DO_E2E_GUEST_PROXY
  export https_proxy=$KUBE_DO_E2E_GUEST_PROXY
  export HTTP_PROXY=$KUBE_DO_E2E_GUEST_PROXY
  export HTTPS_PROXY=$KUBE_DO_E2E_GUEST_PROXY
fi
export no_proxy="127.0.0.1,localhost,${KUBE_DO_E2E_NETWORK_CIDR:-}"
export NO_PROXY=$no_proxy
export KUBE_DO_CACHE_DIR=$CACHE_DIR

log() {
  printf '\n===== %s =====\n' "$*"
}

retry() {
  local attempts=$1
  local delay=$2
  shift 2
  local attempt=1
  while true; do
    if "$@"; then
      return 0
    fi
    if (( attempt >= attempts )); then
      return 1
    fi
    log "command failed; retrying in ${delay}s (${attempt}/${attempts}): $*"
    sleep "$delay"
    attempt=$((attempt + 1))
  done
}

log "Validating cluster configuration"
"$KUBE_DO_BIN" check --config "$CONFIG" --cache-dir "$CACHE_DIR"

log "Downloading node binaries and preparing Installer images"
retry 3 30 "$KUBE_DO_BIN" download --config "$CONFIG" --cache-dir "$CACHE_DIR" --binaries-only

log "Deploying or reusing the private Registry"
retry 3 30 "$KUBE_DO_BIN" registry --config "$CONFIG" --cache-dir "$CACHE_DIR"

log "Building and mirroring container images"
retry 3 30 "$KUBE_DO_BIN" download --config "$CONFIG" --cache-dir "$CACHE_DIR" --images-only

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
  log "Starting the artifact download server"
  nohup "$KUBE_DO_BIN" serve --dir "$CACHE_DIR" --addr 0.0.0.0:8080 \
    >"$LOG_DIR/serve.log" 2>&1 &
  echo $! >"$WORK_DIR/serve.pid"
  for _ in $(seq 1 60); do
    if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  curl -fsS http://127.0.0.1:8080/ >/dev/null
fi

log "Checking cached artifacts and Registry connectivity"
"$KUBE_DO_BIN" check --config "$CONFIG" --cache-dir "$CACHE_DIR" --registry

extra_args=()
if [[ -n "$EXTRA_VARS_FILE" && -f "$EXTRA_VARS_FILE" ]]; then
  while IFS= read -r value; do
    [[ -n "$value" ]] || continue
    extra_args+=(--extra-var "$value")
  done <"$EXTRA_VARS_FILE"
fi

log "Installing the cluster"
"$KUBE_DO_BIN" install \
  --config "$CONFIG" \
  --ansible-dir "$ANSIBLE_DIR" \
  --out "$WORK_DIR/rendered" \
  "${extra_args[@]}"

log "Collecting CLI etcd backup result"
mkdir -p "$WORK_DIR/etcd-backups"
"$KUBE_DO_BIN" etcd backup --config "$CONFIG" --ansible-dir "$ANSIBLE_DIR" \
  --out "$WORK_DIR/ops-rendered" --backup-dir "$WORK_DIR/etcd-backups" \
  --timeout "$OPS_TIMEOUT" --json >"$WORK_DIR/etcd-backup.json"

log "Collecting CLI certificate result"
"$KUBE_DO_BIN" certs check --config "$CONFIG" --ansible-dir "$ANSIBLE_DIR" \
  --out "$WORK_DIR/ops-rendered" --timeout "$OPS_TIMEOUT" --json \
  >"$WORK_DIR/certs-check.json"

touch /var/lib/kube-do-e2e/install-complete
log "Installation and baseline operations completed"
