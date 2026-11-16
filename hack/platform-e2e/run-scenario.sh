#!/usr/bin/env bash

set -euo pipefail

PLATFORM_E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$PLATFORM_E2E_DIR/lib.sh"

usage() {
  cat >&2 <<'EOF'
usage: run-scenario.sh [options] <scenario.yaml>

options:
  --topology full|minimal  VM topology (default: full)
  --keep                   keep VMs and network after a successful run
  --skip-provision         reuse an existing work root and VMs
  --work-root PATH         explicit work/result directory
  --no-daemon-e2e          skip the daemon API E2E on the installer VM
  --dry-run                validate and render inputs without creating VMs
EOF
  exit 2
}

TOPOLOGY=full
KEEP=0
SKIP_PROVISION=0
RUN_DAEMON=1
DRY_RUN=0
WORK_ROOT=""
SCENARIO_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --topology)
      TOPOLOGY=$2
      shift 2
      ;;
    --keep)
      KEEP=1
      shift
      ;;
    --skip-provision)
      SKIP_PROVISION=1
      shift
      ;;
    --work-root)
      WORK_ROOT=$2
      shift 2
      ;;
    --no-daemon-e2e)
      RUN_DAEMON=0
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -*)
      usage
      ;;
    *)
      [[ -z "$SCENARIO_FILE" ]] || usage
      SCENARIO_FILE=$1
      shift
      ;;
  esac
done
[[ -n "$SCENARIO_FILE" ]] || usage
[[ "$TOPOLOGY" == "full" || "$TOPOLOGY" == "minimal" ]] || die "--topology must be full or minimal"
export KUBE_DO_E2E_TOPOLOGY=$TOPOLOGY
SCENARIO_FILE=$(realpath "$SCENARIO_FILE")
load_scenario "$SCENARIO_FILE"
load_nodes "$SCENARIO_FILE"

if [[ -z "$WORK_ROOT" ]]; then
  WORK_ROOT="$PLATFORM_E2E_DIR/results/$SCENARIO_NAME-$TOPOLOGY-$(date -u +%Y%m%dT%H%M%SZ)"
fi
WORK_ROOT=$(realpath -m "$WORK_ROOT")
mkdir -p "$WORK_ROOT"

if [[ "$DRY_RUN" == "1" ]]; then
  scenario_helper "$SCENARIO_FILE" cluster-config --output "$WORK_ROOT/cluster.yaml" --topology "$TOPOLOGY"
  scenario_helper "$SCENARIO_FILE" network-xml >"$WORK_ROOT/network.xml"
  scenario_helper "$SCENARIO_FILE" extra-vars >"$WORK_ROOT/extra-vars.txt"
  log "dry run validated $SCENARIO_NAME ($TOPOLOGY)"
  printf 'scenario=%s\narch=%s\nacceleration=%s\ntopology=%s\ncluster=%s\nwork_root=%s\n' \
    "$SCENARIO_NAME" "$SCENARIO_ARCH" "$SCENARIO_ACCELERATION" "$TOPOLOGY" "$CLUSTER_NAME" "$WORK_ROOT"
  exit 0
fi

[[ $(id -u) -eq 0 ]] || die "run-scenario.sh must run as root"
for command in go virsh virt-install qemu-img cloud-localds ssh ssh-keygen scp tar sha256sum curl jq; do
  require_command "$command"
done

RUN_STARTED=$SECONDS
SSH_KEY="$WORK_ROOT/ssh/id_ed25519"
SSH_OPTIONS=()
INSTALL_LOG="$WORK_ROOT/install.log"
DAEMON_LOG="$WORK_ROOT/daemon-api.log"

fail_and_preserve() {
  local code=$?
  if (( code != 0 )); then
    log "scenario failed; collecting partial diagnostics"
    "$PLATFORM_E2E_DIR/collect-results.sh" "$SCENARIO_FILE" "$WORK_ROOT" || true
    printf 'status=failed\nexit_code=%d\nelapsed_seconds=%d\n' "$code" "$((SECONDS - RUN_STARTED))" \
      >"$WORK_ROOT/result.txt"
    log "VMs are preserved for debugging. Clean up with: $PLATFORM_E2E_DIR/cleanup-libvirt.sh $SCENARIO_FILE $WORK_ROOT"
  fi
}
trap fail_and_preserve EXIT

if [[ "$SKIP_PROVISION" != "1" ]]; then
  "$PLATFORM_E2E_DIR/provision-libvirt.sh" "$SCENARIO_FILE" "$WORK_ROOT"
fi
[[ -f "$WORK_ROOT/libvirt/state.env" ]] || die "missing libvirt state; provision the scenario first"
# shellcheck disable=SC1091
source "$WORK_ROOT/libvirt/state.env"
SSH_KEY=${SSH_KEY:-$WORK_ROOT/ssh/id_ed25519}
ssh_options
[[ -f "$SSH_KEY" ]] || die "SSH key not found: $SSH_KEY"

log "Building kube-do for $SCENARIO_ARCH"
mkdir -p "$WORK_ROOT/bin"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$SCENARIO_ARCH" go build -trimpath -o "$WORK_ROOT/bin/kube-do" ./cmd/kube-do)

scenario_helper "$SCENARIO_FILE" cluster-config --output "$WORK_ROOT/cluster.yaml" --topology "$TOPOLOGY"
scenario_helper "$SCENARIO_FILE" extra-vars >"$WORK_ROOT/extra-vars.txt"

timeout_seconds=$(scenario_timeout_seconds)
log "waiting for VMs and cloud-init (timeout ${timeout_seconds}s per VM)"
installer_ready=0
for row in "${NODE_ROWS[@]}"; do
  IFS=$'\t' read -r role vm_name node_name address vcpus memory_mib disk_gib mac <<<"$row"
  if ! wait_for_ssh "$address" "$timeout_seconds"; then
    die "SSH did not become ready for $vm_name ($address)"
  fi
  if ! wait_for_cloud_init "$address" "$timeout_seconds"; then
    die "cloud-init did not finish for $vm_name ($address)"
  fi
  if [[ "$role" == "installer" ]]; then
    installer_ready=1
  else
    default_route=$(vm_ssh "$address" 'ip -4 route show default' || true)
    [[ -z "$default_route" ]] || die "$vm_name ($address) has an unexpected default route: $default_route"
  fi
done
[[ "$installer_ready" == "1" ]] || die "installer VM was not provisioned"

log "copying kube-do, playbooks, configuration and SSH key to the installer"
vm_ssh "$INSTALLER_IP" 'install -d -m 0755 /opt/kube-do /root/.ssh /var/lib/kube-do/e2e'
tar -C "$REPO_ROOT" -cf - ansible hack/platform-e2e/install-in-vm.sh hack/e2e-daemon.sh \
  | ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" 'tar -C /opt/kube-do -xf -'
vm_scp "$WORK_ROOT/bin/kube-do" "$INSTALLER_IP" /opt/kube-do/kube-do.new
vm_scp "$WORK_ROOT/cluster.yaml" "$INSTALLER_IP" /opt/kube-do/cluster.yaml
vm_scp "$WORK_ROOT/extra-vars.txt" "$INSTALLER_IP" /opt/kube-do/extra-vars.txt
vm_scp "$SSH_KEY" "$INSTALLER_IP" /root/.ssh/kube-do-e2e
vm_ssh "$INSTALLER_IP" 'chmod 0600 /root/.ssh/kube-do-e2e; chmod 0755 /opt/kube-do/kube-do.new; mv /opt/kube-do/kube-do.new /opt/kube-do/kube-do'

guest_proxy=$(guest_proxy_url)
ops_timeout=${KUBE_DO_E2E_OPS_TIMEOUT:-15m}
if [[ "$SCENARIO_ACCELERATION" == "tcg" && -z "${KUBE_DO_E2E_OPS_TIMEOUT:-}" ]]; then
  ops_timeout=45m
fi
cat >"$WORK_ROOT/platform.env" <<EOF
export KUBE_DO_E2E_GUEST_PROXY=$(printf '%q' "$guest_proxy")
export KUBE_DO_E2E_NETWORK_CIDR=$(printf '%q' "$NET_CIDR")
EOF
vm_scp "$WORK_ROOT/platform.env" "$INSTALLER_IP" /opt/kube-do/platform.env

log "running the offline installation in the installer VM"
set +e
ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" \
  'set -a; . /opt/kube-do/platform.env; set +a; /opt/kube-do/hack/platform-e2e/install-in-vm.sh /opt/kube-do/cluster.yaml /opt/kube-do/ansible /var/cache/kube-do /var/lib/kube-do/e2e /opt/kube-do/extra-vars.txt '"$(printf '%q' "$ops_timeout")" \
  2>&1 | tee "$INSTALL_LOG"
install_status=${PIPESTATUS[0]}
set -e
if (( install_status != 0 )); then
  die "installation failed with exit code $install_status"
fi

if [[ "$RUN_DAEMON" == "1" ]]; then
  log "running daemon API E2E against the installed cluster"
  job_timeout=30m
  backup_timeout=15m
  cert_timeout=10m
  if [[ "$SCENARIO_ACCELERATION" == "tcg" ]]; then
    job_timeout=90m
    backup_timeout=45m
    cert_timeout=45m
  fi
  set +e
  ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" \
    "KUBE_DO_E2E_CONFIG=/opt/kube-do/cluster.yaml \
     KUBE_DO_E2E_BIN=/opt/kube-do/kube-do \
     KUBE_DO_E2E_ANSIBLE_DIR=/opt/kube-do/ansible \
     KUBE_DO_E2E_WORK_DIR=/var/lib/kube-do/e2e/daemon \
     KUBE_DO_E2E_KEEP=1 \
     KUBE_DO_E2E_JOB_TIMEOUT=$job_timeout \
     KUBE_DO_E2E_BACKUP_TIMEOUT=$backup_timeout \
     KUBE_DO_E2E_CERT_TIMEOUT=$cert_timeout \
     KUBE_DO_E2E_RUN_OPS=1 \
     /opt/kube-do/hack/e2e-daemon.sh" 2>&1 | tee "$DAEMON_LOG"
  daemon_status=${PIPESTATUS[0]}
  set -e
  if (( daemon_status != 0 )); then
    die "daemon API E2E failed with exit code $daemon_status"
  fi
fi

"$PLATFORM_E2E_DIR/collect-results.sh" "$SCENARIO_FILE" "$WORK_ROOT"

printf 'status=succeeded\nelapsed_seconds=%d\nvalidation_kind=%s\ntopology=%s\n' \
  "$((SECONDS - RUN_STARTED))" \
  "$([[ "$SCENARIO_ACCELERATION" == "tcg" ]] && printf 'functional-prevalidation-tcg' || printf 'native-kvm-e2e')" \
  "$TOPOLOGY" >"$WORK_ROOT/result.txt"

trap - EXIT
if [[ "$KEEP" == "1" ]]; then
  log "keeping VMs and network (--keep); work root: $WORK_ROOT"
else
  "$PLATFORM_E2E_DIR/cleanup-libvirt.sh" "$SCENARIO_FILE" "$WORK_ROOT"
fi
log "scenario succeeded: $SCENARIO_NAME ($WORK_ROOT)"
