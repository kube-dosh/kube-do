# 一个 Build 多平台资源与 Debian/arm64 选择

一个 Build 使用 `spec.platforms` 同时声明多个 `os/arch`，而不是为每个架构复制整份 Build。每个平台必须提供同名、同版本且同数量的二进制集合与 Installer 生成镜像集合；节点镜像清单保持共享，因为节点镜像 index 本身携带多架构 manifest，搬运时由 `go-containerregistry` 原样保留。与节点镜像不同，`spec.installer.images` 是从本机构建产物生成的有架构差异的镜像，因此每个平台有独立 target tag。`cluster.yaml` 用 `platform.osFamily` 选择发行版行为、用 `platform.arch` 选择二进制与 Installer 镜像，渲染时只把目标平台的资源交给 Ansible。这样升级仍以 Build 为单位，避免 RHEL/amd64、Debian/arm64 等组合发生版本漂移。

发行版选择分为 `rhel` 与 `debian` 两个家族。`rhel` 覆盖 RHEL 系 9；`debian` 首批覆盖 Debian 12/13 与 Ubuntu 22.04/24.04。目标节点继续使用固定 Manifest 中的上游二进制，不从 apt/dnf 安装 Kubernetes、containerd 或 runc；Ansible 只负责发行版差异（SELinux 与 firewalld/ufw、内核模块与服务管理）。目标机必须预装 Python 3 供 Ansible 使用。

Installer Host 必须与目标同架构，因为 HAProxy/keepalived 是原生构建；因此 `download`、`registry` 与 `check` 会在架构不匹配时 fail-fast。一个 Build 虽同时携带 amd64/arm64 资源，但不能在 amd64 下载机上构建 arm64 的 HAProxy/keepalived 镜像。节点镜像下载不受架构选择影响，但因 Installer Host 约束仍与目标架构一致。

**Considered Options**: 一个 Build + `spec.platforms`（选中）；每个平台一个 Build（升级与镜像清单容易漂移）；把安装器二进制视为架构无关（错误，Registry 与 HA 构建产物均为原生程序）；跨架构构建 HAProxy/keepalived（本版不做）。
