#!/usr/bin/env bash

set -euo pipefail

PLATFORM_E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$PLATFORM_E2E_DIR/lib.sh"

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <scenario.yaml> <work-root>" >&2
  exit 2
fi

SCENARIO_FILE_INPUT=$(realpath "$1")
WORK_ROOT=$(realpath -m "$2")
load_scenario "$SCENARIO_FILE_INPUT"
load_nodes "$SCENARIO_FILE_INPUT"

KERNEL_ARGS=()
while IFS= read -r kernel_arg; do
  [[ -n "$kernel_arg" ]] || continue
  KERNEL_ARGS+=("$kernel_arg")
done < <(scenario_helper "$SCENARIO_FILE_INPUT" kernel-args)

[[ $(id -u) -eq 0 ]] || die "provision-libvirt.sh must run as root"
for command in virsh virt-install qemu-img cloud-localds ssh-keygen sha256sum curl tar; do
  require_command "$command"
done
if [[ "$SCENARIO_ARCH" == "arm64" ]]; then
  require_command qemu-system-aarch64
fi
if (( ${#KERNEL_ARGS[@]} > 0 )); then
  require_command qemu-nbd
  require_command partprobe
fi
if [[ "$SCENARIO_ACCELERATION" == "kvm" && ! -e /dev/kvm ]]; then
  die "KVM acceleration requested but /dev/kvm is unavailable"
fi

LIBVIRT_DIR="$WORK_ROOT/libvirt"
DISK_DIR=${KUBE_DO_E2E_DISK_DIR:-/var/lib/libvirt/images/kube-do-e2e/runs/$(basename "$WORK_ROOT")}
SSH_DIR="$WORK_ROOT/ssh"
SSH_KEY="$SSH_DIR/id_ed25519"
SEED_DIR="$DISK_DIR/seeds"
NET_XML="$LIBVIRT_DIR/network.xml"
STATE_FILE="$LIBVIRT_DIR/state.env"

mkdir -p "$LIBVIRT_DIR" "$DISK_DIR" "$SSH_DIR" "$SEED_DIR" "$IMAGE_CACHE_DIR"
if [[ ! -f "$SSH_KEY" ]]; then
  ssh-keygen -q -t ed25519 -N '' -C 'kube-do-platform-e2e' -f "$SSH_KEY"
fi

if virsh net-info "$NET_NAME" >/dev/null 2>&1; then
  die "libvirt network $NET_NAME already exists; run cleanup-libvirt.sh for the stale run or choose a clean host"
fi

scenario_helper "$SCENARIO_FILE_INPUT" network-xml >"$NET_XML"
virsh net-define "$NET_XML" >/dev/null
virsh net-start "$NET_NAME" >/dev/null
virsh net-autostart "$NET_NAME" >/dev/null

image_path="$IMAGE_CACHE_DIR/$(basename "${SCENARIO_IMAGE_URL%%\?*}")"
if [[ -f "$image_path" && "$(sha256sum "$image_path" | awk '{print $1}')" == "$SCENARIO_IMAGE_SHA256" ]]; then
  log "using cached image $image_path"
else
  log "downloading $SCENARIO_IMAGE_URL"
  rm -f "$image_path.part"
  curl_with_optional_proxy -fL --retry 5 --retry-delay 5 --connect-timeout 30 \
    -o "$image_path.part" "$SCENARIO_IMAGE_URL"
  actual_sha=$(sha256sum "$image_path.part" | awk '{print $1}')
  [[ "$actual_sha" == "$SCENARIO_IMAGE_SHA256" ]] || die "image SHA-256 mismatch: got $actual_sha, want $SCENARIO_IMAGE_SHA256"
  mv "$image_path.part" "$image_path"
fi

guest_proxy=$(guest_proxy_url)
: >"$LIBVIRT_DIR/vms.txt"

inject_fedora_kernel_args() {
  local overlay=$1
  shift
  local nbd=${KUBE_DO_E2E_NBD_DEVICE:-/dev/nbd0}
  local mount_dir="$DISK_DIR/.kernel-args-mnt"
  local partition
  modprobe nbd max_part=16
  [[ ! -e "/sys/block/$(basename "$nbd")/pid" ]] || die "NBD device $nbd is already in use"
  mkdir -p "$mount_dir"
  (
    set -euo pipefail
    trap 'umount "$mount_dir" >/dev/null 2>&1 || true; qemu-nbd --disconnect "$nbd" >/dev/null 2>&1 || true; rmdir "$mount_dir" >/dev/null 2>&1 || true' EXIT
    qemu-nbd --connect="$nbd" "$overlay"
    partprobe "$nbd" >/dev/null 2>&1 || true
    udevadm settle >/dev/null 2>&1 || true
    for _ in $(seq 1 20); do
      partition=$(lsblk -lnpo NAME,FSTYPE "$nbd" | awk '$2 == "btrfs" { print $1; exit }')
      [[ -n "$partition" ]] && break
      sleep 0.5
    done
    [[ -n "$partition" ]] || die "no btrfs partition found for kernel argument injection in $overlay"
    mount -o rw,subvol=boot "$partition" "$mount_dir"
    for entry in "$mount_dir"/loader/entries/*.conf; do
      [[ -f "$entry" ]] || continue
      for kernel_arg in "$@"; do
        grep -q " ${kernel_arg} \| ${kernel_arg}$" "$entry" || sed -i "/^options / s/$/ ${kernel_arg}/" "$entry"
      done
    done
  )
}

inject_debian_kernel_args() {
  local overlay=$1
  shift
  local nbd=${KUBE_DO_E2E_NBD_DEVICE:-/dev/nbd0}
  local mount_dir="$DISK_DIR/.kernel-args-mnt"
  local partition
  local grub_default
  local grub_cfg
  modprobe nbd max_part=16
  [[ ! -e "/sys/block/$(basename "$nbd")/pid" ]] || die "NBD device $nbd is already in use"
  mkdir -p "$mount_dir"
  (
    set -euo pipefail
    trap 'umount "$mount_dir" >/dev/null 2>&1 || true; qemu-nbd --disconnect "$nbd" >/dev/null 2>&1 || true; rmdir "$mount_dir" >/dev/null 2>&1 || true' EXIT
    qemu-nbd --connect="$nbd" "$overlay"
    partprobe "$nbd" >/dev/null 2>&1 || true
    udevadm settle >/dev/null 2>&1 || true
    for _ in $(seq 1 20); do
      partition=$(lsblk -lnpo NAME,FSTYPE "$nbd" | awk '$2 == "ext4" { print $1; exit }')
      [[ -n "$partition" ]] && break
      sleep 0.5
    done
    [[ -n "$partition" ]] || die "no ext4 partition found for kernel argument injection in $overlay"
    mount -o rw "$partition" "$mount_dir"
    grub_default="$mount_dir/etc/default/grub"
    grub_cfg="$mount_dir/boot/grub/grub.cfg"
    [[ -f "$grub_default" ]] || die "no /etc/default/grub found in $overlay"
    [[ -f "$grub_cfg" ]] || die "no /boot/grub/grub.cfg found in $overlay"
    for kernel_arg in "$@"; do
      if ! grep -qF -- "$kernel_arg" "$grub_default"; then
        sed -i "/^GRUB_CMDLINE_LINUX=/ s/\"$/ ${kernel_arg}\"/" "$grub_default"
      fi
      if ! grep -qF -- "$kernel_arg" "$grub_cfg"; then
        sed -i "/^[[:space:]]*linux/ s/$/ ${kernel_arg}/" "$grub_cfg"
      fi
    done
  )
}

for row in "${NODE_ROWS[@]}"; do
  IFS=$'\t' read -r role vm_name node_name address vcpus memory_mib disk_gib mac <<<"$row"
  vm_dir="$DISK_DIR/$vm_name"
  overlay="$vm_dir/root.qcow2"
  seed="$SEED_DIR/$vm_name.iso"
  mkdir -p "$vm_dir"
  qemu-img create -q -f qcow2 -F qcow2 -b "$image_path" "$overlay" "${disk_gib}G"

  if (( ${#KERNEL_ARGS[@]} > 0 )); then
    case "$SCENARIO_OS" in
      fedora) inject_fedora_kernel_args "$overlay" "${KERNEL_ARGS[@]}" ;;
      debian) inject_debian_kernel_args "$overlay" "${KERNEL_ARGS[@]}" ;;
      *) die "kernelArgs injection is not implemented for $SCENARIO_OS" ;;
    esac
  fi

  if [[ "$role" == "installer" ]]; then
    cp_role=installer
    guest_proxy_value=$guest_proxy
  else
    cp_role=target
    guest_proxy_value=""
  fi
  scenario_helper "$SCENARIO_FILE_INPUT" user-data \
    --role "$cp_role" --node "$node_name" --ssh-public-key "$SSH_KEY.pub" --guest-proxy "$guest_proxy_value" \
    >"$SEED_DIR/$vm_name-user-data.yaml"
  scenario_helper "$SCENARIO_FILE_INPUT" network-config --node "$node_name" \
    --topology "${KUBE_DO_E2E_TOPOLOGY:-full}" >"$SEED_DIR/$vm_name-network-config.yaml"
  scenario_helper "$SCENARIO_FILE_INPUT" meta-data --node "$node_name" --role "$cp_role" \
    >"$SEED_DIR/$vm_name-meta-data.yaml"
  cloud-localds --network-config="$SEED_DIR/$vm_name-network-config.yaml" \
    "$seed" "$SEED_DIR/$vm_name-user-data.yaml" "$SEED_DIR/$vm_name-meta-data.yaml"

  virt_args=(
    --connect qemu:///system
    --name "$vm_name"
    --memory "$memory_mib"
    --vcpus "$vcpus"
    --import
    --disk "path=$overlay,format=qcow2,bus=virtio"
    --disk "path=$seed,device=cdrom"
    --network "network=$NET_NAME,model=virtio,mac=$mac"
    --graphics none
    --console pty,target_type=serial
    --noautoconsole
    --wait 0
    --autostart
  )
  if [[ "$SCENARIO_ARCH" == "arm64" ]]; then
    virt_args+=(
      --arch aarch64
      --machine virt
      --virt-type qemu
      --cpu max
      --boot uefi
    )
  elif [[ "$SCENARIO_ACCELERATION" == "kvm" ]]; then
    virt_args+=(--virt-type kvm --cpu host-passthrough)
  else
    virt_args+=(--virt-type qemu --cpu max)
  fi
  if [[ -n "$SCENARIO_OS_INFO" ]]; then
    virt_args+=(--osinfo "$SCENARIO_OS_INFO")
  fi
  log "creating $vm_name ($role, $address)"
  virt-install "${virt_args[@]}" >/dev/null
  printf '%s\n' "$vm_name" >>"$LIBVIRT_DIR/vms.txt"
done

cat >"$STATE_FILE" <<EOF
SCENARIO_NAME=$SCENARIO_NAME
SCENARIO_FILE=$SCENARIO_FILE_INPUT
SCENARIO_TOPOLOGY=${KUBE_DO_E2E_TOPOLOGY:-full}
NET_NAME=$NET_NAME
WORK_ROOT=$WORK_ROOT
LIBVIRT_DIR=$LIBVIRT_DIR
DISK_DIR=$DISK_DIR
SSH_KEY=$SSH_KEY
EOF

log "provisioned ${#NODE_ROWS[@]} VMs on $NET_NAME"
log "state: $STATE_FILE"
