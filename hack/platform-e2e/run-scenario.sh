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
  --skip-install           reuse an already installed cluster in an existing work root
  --work-root PATH         explicit work/result directory
  --no-daemon-e2e          skip the daemon API E2E on the installer VM
  --dry-run                validate and render inputs without creating VMs
EOF
  exit 2
}

TOPOLOGY=full
KEEP=0
SKIP_PROVISION=0
SKIP_INSTALL=0
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
    --skip-install)
      SKIP_INSTALL=1
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

bootstrap_fedora_tcg_installer() {
  if [[ "$SCENARIO_OS" != "fedora" || "$SCENARIO_ACCELERATION" != "tcg" ]]; then
    return 0
  fi
  require_command dnf
  if ! command -v createrepo_c >/dev/null 2>&1; then
    log "installing host createrepo_c for the offline Fedora TCG bootstrap"
    dnf -y install createrepo_c
  fi

  local packages="ansible-core gcc make tar gzip openssl-devel pkgconf-pkg-config libnl3-devel jq curl"
  local installroot rpm_dir
  installroot=$(mktemp -d /tmp/kube-do-e2e-dnfroot.XXXXXX)
  rpm_dir=$(mktemp -d /tmp/kube-do-e2e-rpms.XXXXXX)
  log "resolving Fedora $SCENARIO_VERSION/aarch64 installer dependencies on the host"
  dnf -y --installroot="$installroot" --releasever="$SCENARIO_VERSION" --forcearch=aarch64 \
    --setopt=install_weak_deps=False --setopt=reposdir=/etc/yum.repos.d \
    download --resolve --destdir="$rpm_dir" $packages
  createrepo_c "$rpm_dir" >/dev/null

  log "installing Fedora TCG installer dependencies from the offline local repository"
  tar -C "$rpm_dir" -cf - . \
    | ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" \
      'install -d -m 0755 /var/tmp/kube-do-e2e-rpms && tar -C /var/tmp/kube-do-e2e-rpms -xf -'
  vm_ssh "$INSTALLER_IP" \
    "printf '[kube-do-e2e-local]\\nname=kube-do e2e local\\nbaseurl=file:///var/tmp/kube-do-e2e-rpms\\nenabled=1\\ngpgcheck=0\\n' >/etc/yum.repos.d/kube-do-e2e-local.repo
     dnf -y --disablerepo='*' --enablerepo=kube-do-e2e-local --setopt=install_weak_deps=False install $packages
     ansible-playbook --version >/dev/null
     rm -f /etc/yum.repos.d/kube-do-e2e-local.repo
     rm -rf /var/tmp/kube-do-e2e-rpms"
  rm -rf -- "$installroot" "$rpm_dir"
}

prefetch_curl() {
  local url=$1
  shift
  local proxy
  proxy=$(host_proxy_url)
  if [[ -n "$proxy" && "$url" != https://dl.k8s.io/* ]]; then
    curl --proxy "$proxy" "$@" "$url" && return 0
    curl "$@" "$url"
    return
  fi
  curl "$@" "$url" && return 0
  [[ -n "$proxy" ]] || return 1
  curl --proxy "$proxy" "$@" "$url"
}

prefetch_content_length() {
  local url=$1 len attempt
  for attempt in 1 2 3 4 5; do
    len=$(prefetch_curl "$url" -fsSIL --connect-timeout 15 --max-time 60 2>/dev/null \
      | awk 'tolower($1)=="content-length:" {gsub("\r",""); v=$2} END{print v}' || true)
    if [[ -n "$len" ]]; then
      printf '%s' "$len"
      return 0
    fi
    sleep 2
  done
  return 1
}

prefetch_artifacts() {
  local manifest="$REPO_ROOT/manifests/builds/$CLUSTER_BUILD.yaml"
  [[ -f "$manifest" ]] || die "manifest not found: $manifest"
  local cache_dir="/var/cache/kube-do/artifacts/$CLUSTER_BUILD/linux-$SCENARIO_ARCH"
  local host_cache_dir="/var/cache/kube-do-e2e/artifacts/$CLUSTER_BUILD/linux-$SCENARIO_ARCH"
  local rows name url sha len chunk tmp start end i pids host_file chunks
  rows=$(python3 - "$manifest" "$SCENARIO_ARCH" <<'PY'
import sys
import yaml

manifest, arch = sys.argv[1], sys.argv[2]
data = yaml.safe_load(open(manifest, encoding="utf-8"))
groups = (data["spec"].get("binaries", []), data["spec"].get("installer", {}).get("binaries", []))
seen = set()
for binaries in groups:
    for binary in binaries:
        if binary.get("os") != "linux" or binary.get("arch") != arch:
            continue
        if binary.get("build") or not binary.get("sha256"):
            continue
        if binary["name"] in seen:
            continue
        seen.add(binary["name"])
        print("\t".join((binary["name"], binary["url"], binary["sha256"])))
PY
)
  vm_ssh "$INSTALLER_IP" "install -d -m 0755 $cache_dir"
  install -d -m 0755 "$host_cache_dir"
  while IFS=$'\t' read -r name url sha; do
    [[ -n "$name" ]] || continue
    if vm_ssh "$INSTALLER_IP" "test -f $cache_dir/$name && echo '$sha  $cache_dir/$name' | sha256sum -c - >/dev/null 2>&1"; then
      log "reusing prefetched $name"
      continue
    fi
    host_file="$host_cache_dir/$name"
    if printf '%s  %s\n' "$sha" "$host_file" | sha256sum -c - >/dev/null 2>&1; then
      log "reusing host-cached $name"
    else
      log "prefetching $name"
      tmp=$(mktemp -d "/tmp/kube-do-e2e-$name.XXXXXX")
      if len=$(prefetch_content_length "$url"); then
        chunks=8
        chunk=$(((len + chunks - 1) / chunks))
        pids=()
        for i in $(seq 0 $((chunks - 1))); do
          start=$((i * chunk))
          end=$((start + chunk - 1))
          (( end < len )) || end=$((len - 1))
          prefetch_curl "$url" -fsSL --retry 5 --retry-delay 3 --connect-timeout 15 \
            --max-time 900 -r "$start-$end" -o "$tmp/part.$i" &
          pids+=("$!")
        done
        for i in "${!pids[@]}"; do
          wait "${pids[$i]}" || die "failed to prefetch $name range $i"
        done
        for i in $(seq 0 $((chunks - 1))); do
          cat "$tmp/part.$i"
        done >"$tmp/$name"
      else
        prefetch_curl "$url" -fsSL --retry 5 --retry-delay 3 --connect-timeout 15 \
          --max-time 1800 -o "$tmp/$name"
      fi
      printf '%s  %s\n' "$sha" "$tmp/$name" | sha256sum -c - >/dev/null
      mv "$tmp/$name" "$host_file"
      rm -rf -- "$tmp"
    fi
    vm_scp "$host_file" "$INSTALLER_IP" "$cache_dir/$name"
    vm_ssh "$INSTALLER_IP" "chmod 0600 $cache_dir/$name"
  done <<<"$rows"
}

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

bootstrap_fedora_tcg_installer

log "copying kube-do, playbooks, configuration and SSH key to the installer"
vm_ssh "$INSTALLER_IP" 'install -d -m 0755 /opt/kube-do /root/.ssh /var/lib/kube-do/e2e'
tar -C "$REPO_ROOT" -cf - ansible hack/platform-e2e/install-in-vm.sh hack/e2e-daemon.sh \
  | ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" 'tar -C /opt/kube-do -xf -'
vm_scp "$WORK_ROOT/bin/kube-do" "$INSTALLER_IP" /opt/kube-do/kube-do.new
vm_scp "$WORK_ROOT/cluster.yaml" "$INSTALLER_IP" /opt/kube-do/cluster.yaml
vm_scp "$WORK_ROOT/extra-vars.txt" "$INSTALLER_IP" /opt/kube-do/extra-vars.txt
vm_scp "$SSH_KEY" "$INSTALLER_IP" /root/.ssh/kube-do-e2e
vm_ssh "$INSTALLER_IP" 'chmod 0600 /root/.ssh/kube-do-e2e; chmod 0755 /opt/kube-do/kube-do.new; mv /opt/kube-do/kube-do.new /opt/kube-do/kube-do'

prefetch_artifacts

guest_proxy=$(guest_proxy_url)
ops_timeout=${KUBE_DO_E2E_OPS_TIMEOUT:-15m}
if [[ "$SCENARIO_ACCELERATION" == "tcg" && -z "${KUBE_DO_E2E_OPS_TIMEOUT:-}" ]]; then
  ops_timeout=45m
fi
preload_images=0
images_manifest=""
if [[ "${KUBE_DO_E2E_PRELOAD_IMAGES:-0}" == "1" && "$SKIP_INSTALL" != "1" ]]; then
  require_command podman
  preload_images=1
  images_manifest=/opt/kube-do/images-manifest.yaml
  python3 - "$REPO_ROOT/manifests/builds/$CLUSTER_BUILD.yaml" "$WORK_ROOT/images-manifest.yaml" "$INSTALLER_IP" <<'PY'
import sys
import yaml

data = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
namespace = data["spec"]["registry"]["namespace"]
data["spec"]["images"] = [{
    "name": "pause",
    "source": f"{sys.argv[3]}:5000/{namespace}/pause:3.10.2",
    "target": "pause:3.10.2",
}]
with open(sys.argv[2], "w", encoding="utf-8") as fh:
    yaml.safe_dump(data, fh, sort_keys=False)
PY
fi
cat >"$WORK_ROOT/platform.env" <<EOF
export KUBE_DO_E2E_GUEST_PROXY=$(printf '%q' "$guest_proxy")
export KUBE_DO_E2E_NETWORK_CIDR=$(printf '%q' "$NET_CIDR")
export KUBE_DO_E2E_INSTALL_TAGS=$(printf '%q' "${KUBE_DO_E2E_INSTALL_TAGS:-}")
EOF
if [[ -n "$images_manifest" ]]; then
  printf 'export KUBE_DO_E2E_IMAGES_MANIFEST=%q\n' "$images_manifest" >>"$WORK_ROOT/platform.env"
fi
vm_scp "$WORK_ROOT/platform.env" "$INSTALLER_IP" /opt/kube-do/platform.env

if [[ "$preload_images" == "1" ]]; then
  vm_scp "$WORK_ROOT/images-manifest.yaml" "$INSTALLER_IP" "$images_manifest"
  log "deploying the private Registry before host-side image preload"
  vm_ssh "$INSTALLER_IP" \
    'set -a; . /opt/kube-do/platform.env; set +a; /opt/kube-do/kube-do registry --config /opt/kube-do/cluster.yaml --cache-dir /var/cache/kube-do'
  "$PLATFORM_E2E_DIR/mirror-images.sh" "$REPO_ROOT/manifests/builds/$CLUSTER_BUILD.yaml" \
    "$SCENARIO_ARCH" "$INSTALLER_IP:5000"
fi

if [[ "$SKIP_INSTALL" == "1" ]]; then
  vm_ssh "$INSTALLER_IP" 'test -f /var/lib/kube-do-e2e/install-complete' \
    || die "--skip-install requires a completed installation in $WORK_ROOT"
  log "reusing the completed installation recorded in $INSTALL_LOG"
else
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
fi

if [[ "$RUN_DAEMON" == "1" ]]; then
  log "running daemon API E2E against the installed cluster"
  daemon_work_dir="/var/lib/kube-do/e2e/daemon-$(date -u +%Y%m%dT%H%M%SZ)"
  job_timeout=30m
  backup_timeout=15m
  cert_timeout=10m
  backup_wait_seconds=900
  cert_wait_seconds=600
  if [[ "$SCENARIO_ACCELERATION" == "tcg" ]]; then
    job_timeout=90m
    backup_timeout=45m
    cert_timeout=45m
    backup_wait_seconds=2700
    cert_wait_seconds=2700
  fi
  set +e
  ssh "${SSH_OPTIONS[@]}" "root@$INSTALLER_IP" \
    "KUBE_DO_E2E_CONFIG=/opt/kube-do/cluster.yaml \
     KUBE_DO_E2E_BIN=/opt/kube-do/kube-do \
     KUBE_DO_E2E_ANSIBLE_DIR=/opt/kube-do/ansible \
     KUBE_DO_E2E_WORK_DIR=$daemon_work_dir \
     KUBE_DO_E2E_KEEP=1 \
     KUBE_DO_E2E_JOB_TIMEOUT=$job_timeout \
     KUBE_DO_E2E_BACKUP_TIMEOUT=$backup_timeout \
     KUBE_DO_E2E_CERT_TIMEOUT=$cert_timeout \
     KUBE_DO_E2E_BACKUP_WAIT_SECONDS=$backup_wait_seconds \
     KUBE_DO_E2E_CERT_WAIT_SECONDS=$cert_wait_seconds \
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
