#!/usr/bin/env bash

set -euo pipefail

PLATFORM_E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$PLATFORM_E2E_DIR/../.." && pwd)
SCENARIO_HELPER="$PLATFORM_E2E_DIR/scenario.py"
IMAGE_CACHE_DIR=${KUBE_DO_E2E_IMAGE_CACHE:-/var/lib/libvirt/images/kube-do-e2e/cache}

log() {
  printf '[platform-e2e] %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

scenario_helper() {
  python3 "$SCENARIO_HELPER" "$@"
}

load_scenario() {
  local scenario=$1
  local topology=${KUBE_DO_E2E_TOPOLOGY:-full}
  scenario_helper "$scenario" validate >/dev/null
  eval "$(scenario_helper "$scenario" shell-env --topology "$topology")"
  SCENARIO_FILE=$scenario
}

load_nodes() {
  local scenario=$1
  local topology=${KUBE_DO_E2E_TOPOLOGY:-full}
  NODE_ROWS=()
  while IFS=$'\t' read -r role vm_name node_name address vcpus memory_mib disk_gib mac; do
    [[ -n "$role" ]] || continue
    NODE_ROWS+=("$role"$'\t'"$vm_name"$'\t'"$node_name"$'\t'"$address"$'\t'"$vcpus"$'\t'"$memory_mib"$'\t'"$disk_gib"$'\t'"$mac")
  done < <(scenario_helper "$scenario" nodes --topology "$topology")
}

guest_proxy_url() {
  if [[ -n "${KUBE_DO_E2E_GUEST_PROXY:-}" ]]; then
    case "$KUBE_DO_E2E_GUEST_PROXY" in
      none|off|"")
        return 0
        ;;
      *)
        printf '%s' "$KUBE_DO_E2E_GUEST_PROXY"
        return 0
        ;;
    esac
  fi
  if [[ -n "${GUEST_PROXY_PORT:-}" ]]; then
    printf 'http://%s:%s' "$NET_GATEWAY" "$GUEST_PROXY_PORT"
  fi
}

host_proxy_url() {
  if [[ -n "${KUBE_DO_E2E_HOST_PROXY:-}" ]]; then
    printf '%s' "$KUBE_DO_E2E_HOST_PROXY"
  elif [[ -n "${HOST_PROXY:-}" ]]; then
    printf '%s' "$HOST_PROXY"
  fi
}

curl_with_optional_proxy() {
  local proxy
  proxy=$(host_proxy_url)
  if [[ -n "$proxy" ]]; then
    curl --proxy "$proxy" "$@"
  else
    curl "$@"
  fi
}

ssh_options() {
  SSH_OPTIONS=(
    -o BatchMode=yes
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o ConnectTimeout=10
    -o ServerAliveInterval=15
    -o ServerAliveCountMax=4
    -i "$SSH_KEY"
  )
}

vm_ssh() {
  local address=$1
  shift
  ssh -n "${SSH_OPTIONS[@]}" "root@$address" "$@"
}

vm_scp() {
  local source=$1
  local address=$2
  local destination=$3
  scp -q "${SSH_OPTIONS[@]}" "$source" "root@$address:$destination" </dev/null
}

wait_for_ssh() {
  local address=$1
  local timeout_seconds=$2
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if vm_ssh "$address" true >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  return 1
}

wait_for_cloud_init() {
  local address=$1
  local timeout_seconds=$2
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if vm_ssh "$address" 'test -f /var/lib/kube-do-e2e/ready' >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  return 1
}

scenario_timeout_seconds() {
  if [[ "$SCENARIO_ACCELERATION" == "tcg" ]]; then
    printf '3600'
  else
    printf '900'
  fi
}

result_dir() {
  local work_root=$1
  printf '%s/results/%s' "$work_root" "$SCENARIO_NAME"
}
