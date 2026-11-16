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
  SSH_KEY=$WORK_ROOT/ssh/id_ed25519
fi
load_scenario "$SCENARIO_FILE_INPUT"
load_nodes "$SCENARIO_FILE_INPUT"
SSH_KEY=${SSH_KEY:-$WORK_ROOT/ssh/id_ed25519}
ssh_options

OUT=$(result_dir "$WORK_ROOT")
mkdir -p "$OUT"
cp "$SCENARIO_FILE_INPUT" "$OUT/scenario.yaml"
if [[ -f "$WORK_ROOT/cluster.yaml" ]]; then
  cp "$WORK_ROOT/cluster.yaml" "$OUT/cluster.yaml"
fi

collect_node() {
  local role=$1 address=$2 prefix=$3
  {
    printf 'role=%s\naddress=%s\n' "$role" "$address"
    vm_ssh "$address" 'cat /etc/os-release; echo; uname -a; echo; nproc; free -m; echo; ip -brief addr; echo; ip route; echo; cloud-init status --long || true' || true
  } >"$OUT/$prefix-system.txt" 2>&1
}

first_cp=""
for row in "${NODE_ROWS[@]}"; do
  IFS=$'\t' read -r role vm_name node_name address vcpus memory_mib disk_gib mac <<<"$row"
  collect_node "$role" "$address" "$vm_name"
  if [[ "$role" == "control-plane" && -z "$first_cp" ]]; then
    first_cp=$address
  fi
done

if [[ -n "$first_cp" ]]; then
  {
    vm_ssh "$first_cp" '/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide'
    echo
    vm_ssh "$first_cp" '/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get pods -A -o wide'
    echo
    vm_ssh "$first_cp" '/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get events -A --sort-by=.lastTimestamp'
    echo
    vm_ssh "$first_cp" '/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system get deployment,daemonset,service -o wide'
  } >"$OUT/cluster.txt" 2>&1 || true
  vm_ssh "$first_cp" 'command -v cilium >/dev/null && cilium status --kubeconfig /etc/kubernetes/admin.conf' \
    >"$OUT/cilium-status.txt" 2>&1 || true
  vm_ssh "$first_cp" 'pod=$(/usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system get pods -l component=etcd -o jsonpath="{.items[0].metadata.name}"); /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system exec "$pod" -- etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key endpoint status -w table' \
    >"$OUT/etcd-status.txt" 2>&1 || true
  vm_ssh "$first_cp" 'kubeadm certs check-expiration' >"$OUT/certs-expiration.txt" 2>&1 || true
fi

{
  curl -ksS "https://$CLUSTER_VIP:$CLUSTER_API_PORT/healthz"
  echo
} >"$OUT/vip-healthz.txt" 2>&1 || true

if vm_ssh "$INSTALLER_IP" 'test -d /var/lib/kube-do/e2e' >/dev/null 2>&1; then
  ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" \
    'tar -C /var/lib/kube-do/e2e -cf - .' | tar -C "$OUT" -xf - || true
fi

cat >"$OUT/summary.txt" <<EOF
scenario=$SCENARIO_NAME
os=$SCENARIO_OS
version=$SCENARIO_VERSION
arch=$SCENARIO_ARCH
acceleration=$SCENARIO_ACCELERATION
topology=${KUBE_DO_E2E_TOPOLOGY:-full}
validation_kind=$([[ "$SCENARIO_ACCELERATION" == "tcg" ]] && printf 'functional-prevalidation-tcg' || printf 'native-kvm-e2e')
cluster=$CLUSTER_NAME
vip=$CLUSTER_VIP
api_port=$CLUSTER_API_PORT
collected_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

log "collected results in $OUT"
