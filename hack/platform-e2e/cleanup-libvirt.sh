#!/usr/bin/env bash

set -euo pipefail

PLATFORM_E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$PLATFORM_E2E_DIR/lib.sh"

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <scenario.yaml> <work-root>" >&2
  exit 2
fi

SCENARIO_FILE_INPUT=$(realpath "$1")
WORK_ROOT=$(realpath -m "$2")
if [[ -f "$WORK_ROOT/libvirt/state.env" ]]; then
  # shellcheck disable=SC1091
  source "$WORK_ROOT/libvirt/state.env"
  KUBE_DO_E2E_TOPOLOGY=${SCENARIO_TOPOLOGY:-full}
fi
load_scenario "$SCENARIO_FILE_INPUT"

if [[ "${KUBE_DO_E2E_KEEP_VMS:-0}" == "1" ]]; then
  log "KUBE_DO_E2E_KEEP_VMS=1; leaving VMs and network intact"
  exit 0
fi

vms_file="$WORK_ROOT/libvirt/vms.txt"
if [[ -f "$vms_file" ]]; then
  while IFS= read -r vm_name; do
    [[ -n "$vm_name" ]] || continue
    if virsh dominfo "$vm_name" >/dev/null 2>&1; then
      log "removing VM $vm_name"
      virsh destroy "$vm_name" >/dev/null 2>&1 || true
      virsh undefine "$vm_name" --nvram >/dev/null 2>&1 || virsh undefine "$vm_name" >/dev/null 2>&1 || true
    fi
  done <"$vms_file"
fi

if virsh net-info "$NET_NAME" >/dev/null 2>&1; then
  log "removing network $NET_NAME"
  virsh net-destroy "$NET_NAME" >/dev/null 2>&1 || true
  virsh net-undefine "$NET_NAME" >/dev/null 2>&1 || true
fi

if [[ -n "${DISK_DIR:-}" && -d "$DISK_DIR" ]]; then
  rm -rf -- "$DISK_DIR"
fi
rm -rf -- "$WORK_ROOT/libvirt/seeds"
log "cleanup complete"
