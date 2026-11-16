#!/usr/bin/env python3
"""Scenario validation and rendering helpers for the kube-do platform matrix."""

from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
import shlex
import sys
from pathlib import Path
from typing import Any

import yaml


class ScenarioError(Exception):
    pass


def fail(message: str) -> None:
    raise ScenarioError(message)


def load(path: Path) -> dict[str, Any]:
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8"))
    except Exception as exc:
        fail(f"read {path}: {exc}")
    if not isinstance(data, dict):
        fail(f"{path}: scenario must be a YAML mapping")
    return data


def require(mapping: dict[str, Any], key: str, prefix: str = "") -> Any:
    value = mapping.get(key)
    if value is None or value == "":
        fail(f"{prefix}{key} is required")
    return value


def memory_bytes(value: Any) -> int:
    if not isinstance(value, int) or value <= 0:
        fail(f"invalid memory size {value!r}")
    return value * 1024 * 1024


def disk_bytes(value: Any) -> int:
    if not isinstance(value, int) or value <= 0:
        fail(f"invalid disk size {value!r}")
    return value * 1024 * 1024 * 1024


def node_list(scenario: dict[str, Any]) -> list[dict[str, Any]]:
    installer = dict(require(scenario, "installer", "nodes."))
    installer["role"] = "installer"
    installer["vmName"] = f"{scenario['name']}-installer"
    nodes = [installer]
    for role, key in (("control-plane", "controlPlane"), ("worker", "workers")):
        values = scenario.get(key) or []
        if not isinstance(values, list) or not values:
            fail(f"{key} must contain at least one node")
        for value in values:
            node = dict(value)
            node["role"] = role
            node["vmName"] = f"{scenario['name']}-{node['name']}"
            nodes.append(node)
    return nodes


def select_nodes(scenario: dict[str, Any], topology: str) -> tuple[dict[str, Any], list[dict[str, Any]], list[dict[str, Any]]]:
    nodes = node_list(scenario)
    installer = next(n for n in nodes if n["role"] == "installer")
    control_plane = [n for n in nodes if n["role"] == "control-plane"]
    workers = [n for n in nodes if n["role"] == "worker"]
    if topology == "minimal":
        control_plane = control_plane[:1]
        workers = workers[:1]
    elif topology != "full":
        fail(f"unknown topology {topology!r}; expected full or minimal")
    return installer, control_plane, workers


def validate(scenario: dict[str, Any]) -> None:
    name = require(scenario, "name")
    if not isinstance(name, str) or not name.replace("-", "").isalnum() or name != name.lower():
        fail(f"name must be a lowercase alphanumeric/hyphen string, got {name!r}")
    require(scenario, "description")
    if require(scenario, "arch") not in ("amd64", "arm64"):
        fail("arch must be amd64 or arm64")
    if require(scenario, "acceleration") not in ("kvm", "tcg"):
        fail("acceleration must be kvm or tcg")
    if require(scenario, "firmware") not in ("bios", "uefi"):
        fail("firmware must be bios or uefi")
    os_name = require(scenario, "os")
    if os_name not in ("fedora", "debian"):
        fail("os must be fedora or debian")
    if scenario["arch"] == "amd64" and scenario["acceleration"] != "kvm":
        fail("amd64 scenarios must use kvm")
    if scenario["arch"] == "arm64" and scenario["firmware"] != "uefi":
        fail("arm64 scenarios must use uefi firmware")

    image = require(scenario, "image")
    image_url = require(image, "url", "image.")
    if not str(image_url).startswith(("http://", "https://")):
        fail("image.url must be an http(s) URL")
    checksum = require(image, "sha256", "image.")
    if len(str(checksum)) != 64 or any(c not in "0123456789abcdef" for c in str(checksum)):
        fail("image.sha256 must be 64 lowercase hexadecimal characters")

    network = require(scenario, "network")
    cidr = ipaddress.ip_network(require(network, "cidr"), strict=True)
    if cidr.version != 4:
        fail("network.cidr must be IPv4")
    gateway = ipaddress.ip_address(require(network, "gateway"))
    if gateway not in cidr:
        fail("network.gateway must be inside network.cidr")
    require(network, "name")
    require(network, "bridge")

    cluster = require(scenario, "cluster")
    require(cluster, "name")
    require(cluster, "build")
    vip = ipaddress.ip_address(require(cluster, "vip"))
    if vip not in cidr:
        fail("cluster.vip must be inside network.cidr")
    api_port = require(cluster, "apiPort")
    if not isinstance(api_port, int) or not 1 <= api_port <= 65535:
        fail("cluster.apiPort must be between 1 and 65535")

    nodes = node_list(scenario)
    seen_names: set[str] = set()
    seen_addresses: set[str] = set()
    for node in nodes:
        node_name = require(node, "name", "node.")
        if node_name in seen_names:
            fail(f"duplicate node name {node_name!r}")
        seen_names.add(node_name)
        address = ipaddress.ip_address(require(node, "address", f"node {node_name}: "))
        if address not in cidr:
            fail(f"node {node_name} address {address} is outside {cidr}")
        if str(address) in seen_addresses:
            fail(f"duplicate node address {address}")
        seen_addresses.add(str(address))
        for key in ("vcpus", "memoryMiB", "diskGiB"):
            value = require(node, key, f"node {node_name}: ")
            if not isinstance(value, int) or value <= 0:
                fail(f"node {node_name}: {key} must be a positive integer")
        memory_bytes(node["memoryMiB"])
        disk_bytes(node["diskGiB"])
    if str(vip) in seen_addresses:
        fail(f"cluster VIP {vip} conflicts with a node address")
    if str(gateway) in seen_addresses:
        fail(f"network gateway {gateway} conflicts with a node address")

    extra = scenario.get("extraVars") or {}
    if not isinstance(extra, dict):
        fail("extraVars must be a mapping")

    kernel_args = scenario.get("kernelArgs") or []
    if not isinstance(kernel_args, list):
        fail("kernelArgs must be a list")
    for value in kernel_args:
        if not isinstance(value, str) or not value or any(character.isspace() for character in value):
            fail(f"kernelArgs entries must be non-empty strings without whitespace, got {value!r}")


def mac_for(scenario_name: str, vm_name: str) -> str:
    digest = hashlib.sha256(f"{scenario_name}/{vm_name}".encode()).hexdigest()
    return f"52:54:00:{digest[0:2]}:{digest[2:4]}:{digest[4:6]}"


def shell_env(scenario: dict[str, Any], topology: str) -> str:
    installer, control_plane, workers = select_nodes(scenario, topology)
    network = scenario["network"]
    cluster = scenario["cluster"]
    proxy = scenario.get("proxy") or {}
    values = {
        "SCENARIO_NAME": scenario["name"],
        "SCENARIO_OS": scenario["os"],
        "SCENARIO_OS_INFO": scenario.get("osInfo", ""),
        "SCENARIO_VERSION": scenario.get("version", ""),
        "SCENARIO_ARCH": scenario["arch"],
        "SCENARIO_ACCELERATION": scenario["acceleration"],
        "SCENARIO_FIRMWARE": scenario["firmware"],
        "SCENARIO_TOPOLOGY": topology,
        "SCENARIO_IMAGE_URL": scenario["image"]["url"],
        "SCENARIO_IMAGE_SHA256": scenario["image"]["sha256"],
        "NET_NAME": network["name"],
        "NET_BRIDGE": network["bridge"],
        "NET_CIDR": network["cidr"],
        "NET_GATEWAY": network["gateway"],
        "NET_DNS": ",".join(network.get("dns") or []),
        "HOST_PROXY": proxy.get("host", ""),
        "GUEST_PROXY_PORT": str(proxy.get("guestPort", "")),
        "CLUSTER_NAME": cluster["name"],
        "CLUSTER_BUILD": cluster["build"],
        "CLUSTER_VIP": cluster["vip"],
        "CLUSTER_API_PORT": str(cluster["apiPort"]),
        "CLUSTER_VRRP_ROUTER_ID": str((cluster.get("vrrp") or {}).get("routerId", 51)),
        "CLUSTER_VRRP_PASSWORD": str((cluster.get("vrrp") or {}).get("password", "kube-do")),
        "INSTALLER_NAME": installer["vmName"],
        "INSTALLER_IP": installer["address"],
        "INSTALLER_VCPUS": str(installer["vcpus"]),
        "INSTALLER_MEMORY_MIB": str(installer["memoryMiB"]),
        "INSTALLER_DISK_GIB": str(installer["diskGiB"]),
    }
    lines = [f"{key}={shlex.quote(value)}" for key, value in values.items()]
    lines.append(f"CP_NODES_JSON={shlex.quote(json.dumps(control_plane, separators=(',', ':')))}") 
    lines.append(f"WORKER_NODES_JSON={shlex.quote(json.dumps(workers, separators=(',', ':')))}") 
    return "\n".join(lines) + "\n"


def cluster_config(scenario: dict[str, Any], topology: str) -> dict[str, Any]:
    installer, control_plane, workers = select_nodes(scenario, topology)
    network = scenario["network"]
    cluster = scenario["cluster"]
    os_family = "rhel" if scenario["os"] == "fedora" else "debian"
    vrrp = cluster.get("vrrp") or {}
    return {
        "apiVersion": "kube-do.io/v1alpha1",
        "kind": "ClusterConfig",
        "metadata": {"name": cluster["name"]},
        "build": cluster["build"],
        "platform": {"osFamily": os_family, "arch": scenario["arch"]},
        "installer": {"address": installer["address"], "downloadPort": 8080},
        "registry": {
            "address": f"{installer['address']}:5000",
            "dataDir": "/var/lib/kube-do/registry",
            "tls": {"mode": "selfSigned"},
        },
        "ssh": {
            "user": "root",
            "port": 22,
            "privateKeyFile": "/root/.ssh/kube-do-e2e",
        },
        "controlPlane": {
            "vip": cluster["vip"],
            "apiPort": cluster["apiPort"],
            "vrrp": {
                "routerId": vrrp.get("routerId", 51),
                "password": vrrp.get("password", "kube-do"),
            },
            "nodes": [{"name": n["name"], "address": n["address"]} for n in control_plane],
        },
        "workers": [{"name": n["name"], "address": n["address"]} for n in workers],
        "cni": {
            "name": "cilium",
            "kubeProxyReplacement": True,
            "podCIDR": "10.244.0.0/16",
            "serviceCIDR": "10.96.0.0/12",
            "clusterDomain": "cluster.local",
        },
    }


def network_xml(scenario: dict[str, Any]) -> str:
    network = scenario["network"]
    cidr = ipaddress.ip_network(network["cidr"], strict=False)
    dns_xml = "    <dns enable='no'/>\n"
    return f"""<network>
  <name>{network['name']}</name>
  <forward mode='nat'/>
  <bridge name='{network['bridge']}' stp='on' delay='0'/>
{dns_xml}  <ip address='{network['gateway']}' netmask='{cidr.netmask}'>
  </ip>
</network>
"""


def network_config(scenario: dict[str, Any], node_name: str, topology: str) -> str:
    installer, control_plane, workers = select_nodes(scenario, topology)
    nodes = [installer, *control_plane, *workers]
    if node_name == "installer":
        node = installer
        mac = mac_for(scenario["name"], installer["vmName"])
    else:
        matches = [n for n in nodes if n["name"] == node_name]
        if not matches:
            fail(f"unknown node {node_name!r}")
        node = matches[0]
        mac = mac_for(scenario["name"], node["vmName"])
    network = scenario["network"]
    config: dict[str, Any] = {
        "version": 2,
        "ethernets": {
            "primary": {
                "match": {"macaddress": mac},
                "dhcp4": False,
                "dhcp6": False,
                "addresses": [f"{node['address']}/{ipaddress.ip_network(network['cidr']).prefixlen}"],
            }
        },
    }
    if node["role"] == "installer":
        config["ethernets"]["primary"]["routes"] = [{"to": "default", "via": network["gateway"]}]
        dns = network.get("dns") or []
        if dns:
            config["ethernets"]["primary"]["nameservers"] = {"addresses": dns}
    return yaml.safe_dump(config, sort_keys=False)


def user_data(scenario: dict[str, Any], role: str, node_name: str, ssh_public_key: Path, guest_proxy: str) -> str:
    if role not in ("installer", "target"):
        fail("role must be installer or target")
    key = ssh_public_key.read_text(encoding="utf-8").strip()
    hostname = f"{scenario['name']}-{node_name}" if role == "installer" else f"{scenario['name']}-{node_name}"
    network = scenario["network"]
    os_name = scenario["os"]
    write_files: list[dict[str, Any]] = [
        {
            "path": "/etc/ssh/sshd_config.d/00-kube-do-e2e.conf",
            "permissions": "0644",
            "content": "PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nPasswordAuthentication no\n",
        }
    ]
    runcmd = [
        "install -d -m 0755 /var/lib/kube-do-e2e",
        "usermod -U root 2>/dev/null || true",
        "systemctl enable --now sshd 2>/dev/null || systemctl enable --now ssh 2>/dev/null || true",
    ]
    if role == "installer":
        if guest_proxy:
            write_files.append(
                {
                    "path": "/etc/profile.d/90-kube-do-e2e-proxy.sh",
                    "permissions": "0644",
                    "content": (
                        f"export http_proxy={shlex.quote(guest_proxy)}\n"
                        f"export https_proxy={shlex.quote(guest_proxy)}\n"
                        f"export HTTP_PROXY={shlex.quote(guest_proxy)}\n"
                        f"export HTTPS_PROXY={shlex.quote(guest_proxy)}\n"
                        f"export no_proxy='127.0.0.1,localhost,{network['cidr']}'\n"
                    ),
                }
            )
        if "fedora" in os_name:
            if guest_proxy:
                write_files.append(
                    {
                        "path": "/etc/dnf/dnf.conf.d/99-kube-do-e2e-proxy.conf",
                        "permissions": "0644",
                        "content": f"[main]\nproxy={guest_proxy}\n",
                    }
                )
            if scenario.get("acceleration") == "tcg":
                write_files.append(
                    {
                        "path": "/etc/dnf/dnf.conf.d/98-kube-do-e2e-speed.conf",
                        "permissions": "0644",
                        "content": (
                            "[main]\n"
                            "minrate=0\n"
                            "timeout=60\n"
                            "retries=20\n"
                            "max_parallel_downloads=10\n"
                        ),
                    }
                )
            packages = "ansible-core gcc make tar gzip openssl-devel pkgconf-pkg-config libnl3-devel jq curl"
            package_cmd = f"dnf -y install {packages}"
        else:
            if guest_proxy:
                write_files.append(
                    {
                        "path": "/etc/apt/apt.conf.d/90-kube-do-e2e-proxy",
                        "permissions": "0644",
                        "content": (
                            f'Acquire::http::Proxy "{guest_proxy}";\n'
                            f'Acquire::https::Proxy "{guest_proxy}";\n'
                        ),
                    }
                )
            if scenario.get("acceleration") == "tcg":
                write_files.append(
                    {
                        "path": "/etc/apt/apt.conf.d/91-kube-do-e2e-timeouts",
                        "permissions": "0644",
                        "content": (
                            'Acquire::Retries "10";\n'
                            'Acquire::http::Timeout "60";\n'
                            'Acquire::https::Timeout "60";\n'
                        ),
                    }
                )
            packages = (
                "ansible-core gcc make tar gzip xz-utils libssl-dev pkg-config "
                "libnl-3-dev libnl-genl-3-dev jq curl ca-certificates"
            )
            package_cmd = (
                "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y " + packages
            )
        runcmd.extend(
            [
                package_cmd,
                "ansible-playbook --version >/dev/null",
            ]
        )
    else:
        runcmd.append("ip -4 route del default 2>/dev/null || true")
    runcmd.append("touch /var/lib/kube-do-e2e/ready")

    document = {
        "hostname": hostname,
        "manage_etc_hosts": True,
        "preserve_hostname": False,
        "disable_root": False,
        "ssh_pwauth": False,
        "users": [
            {
                "name": "root",
                "lock_passwd": False,
                "ssh_authorized_keys": [key],
            }
        ],
        "write_files": write_files,
        "runcmd": runcmd,
    }
    return "#cloud-config\n" + yaml.safe_dump(document, sort_keys=False)


def metadata(scenario: dict[str, Any], node_name: str, role: str) -> str:
    return f"instance-id: {scenario['name']}-{role}-{node_name}\nlocal-hostname: {scenario['name']}-{node_name}\n"


def nodes_tsv(scenario: dict[str, Any], topology: str) -> str:
    installer, control_plane, workers = select_nodes(scenario, topology)
    lines = []
    for node in (installer, *control_plane, *workers):
        mac = mac_for(scenario["name"], node["vmName"])
        lines.append(
            "\t".join(
                [
                    node["role"],
                    node["vmName"],
                    node["name"] if "name" in node else "installer",
                    node["address"],
                    str(node["vcpus"]),
                    str(node["memoryMiB"]),
                    str(node["diskGiB"]),
                    mac,
                ]
            )
        )
    return "\n".join(lines) + "\n"


def extra_vars(scenario: dict[str, Any]) -> str:
    values = scenario.get("extraVars") or {}
    return "".join(f"{key}={value}\n" for key, value in values.items())


def kernel_args(scenario: dict[str, Any]) -> str:
    values = scenario.get("kernelArgs") or []
    return "".join(f"{value}\n" for value in values)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("scenario", type=Path)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("validate")
    p_shell = sub.add_parser("shell-env")
    p_shell.add_argument("--topology", choices=("full", "minimal"), default="full")
    p_cluster = sub.add_parser("cluster-config")
    p_cluster.add_argument("--output", type=Path, required=True)
    p_cluster.add_argument("--topology", choices=("full", "minimal"), default="full")
    p_nodes = sub.add_parser("nodes")
    p_nodes.add_argument("--topology", choices=("full", "minimal"), default="full")
    sub.add_parser("network-xml")
    p_netcfg = sub.add_parser("network-config")
    p_netcfg.add_argument("--node", required=True)
    p_netcfg.add_argument("--topology", choices=("full", "minimal"), default="full")
    p_user = sub.add_parser("user-data")
    p_user.add_argument("--role", choices=("installer", "target"), required=True)
    p_user.add_argument("--node", required=True)
    p_user.add_argument("--ssh-public-key", type=Path, required=True)
    p_user.add_argument("--guest-proxy", default="")
    p_meta = sub.add_parser("meta-data")
    p_meta.add_argument("--node", required=True)
    p_meta.add_argument("--role", choices=("installer", "target"), required=True)
    sub.add_parser("extra-vars")
    sub.add_parser("kernel-args")
    args = parser.parse_args()

    scenario = load(args.scenario)
    validate(scenario)
    if args.command == "validate":
        print(f"{scenario['name']}: ok")
    elif args.command == "shell-env":
        sys.stdout.write(shell_env(scenario, args.topology))
    elif args.command == "cluster-config":
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(
            yaml.safe_dump(cluster_config(scenario, args.topology), sort_keys=False),
            encoding="utf-8",
        )
    elif args.command == "nodes":
        sys.stdout.write(nodes_tsv(scenario, args.topology))
    elif args.command == "network-xml":
        sys.stdout.write(network_xml(scenario))
    elif args.command == "network-config":
        sys.stdout.write(network_config(scenario, args.node, args.topology))
    elif args.command == "user-data":
        sys.stdout.write(user_data(scenario, args.role, args.node, args.ssh_public_key, args.guest_proxy))
    elif args.command == "meta-data":
        sys.stdout.write(metadata(scenario, args.node, args.role))
    elif args.command == "extra-vars":
        sys.stdout.write(extra_vars(scenario))
    elif args.command == "kernel-args":
        sys.stdout.write(kernel_args(scenario))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ScenarioError as exc:
        print(f"scenario error: {exc}", file=sys.stderr)
        raise SystemExit(2)
