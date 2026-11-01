# kubedo

kubedo 是一个面向离线（air-gapped）环境的 Kubernetes 安装工程：Go 编写的 `kubedo` 负责准备、版本化和分发安装资源，Ansible 负责目标节点与集群内编排。

> **发布状态：开发版，尚未达到生产发布标准。** Resource Manifest、资源下载与校验、HTTPS Registry、Download Server、Ansible 渲染及全部安装角色均已实现。2026-10-24 已从**空白 Rocky Linux 9.8 / amd64 cloud image** 重建目标节点并完成无公网端到端安装：目标机初始没有 `/etc/resolv.conf`、SELinux 为 `Enforcing`，安装后 3 Control Plane + 1 Worker 全部 `Ready`，Cilium kube-proxy replacement、CoreDNS 与 Hubble Relay 就绪；VIP 主动切主/恢复、整机重启后的 SELinux 持久禁用与集群恢复也已验证。2026-10-25 又在**空白 Debian 12 / amd64 cloud image** 上完成同类端到端安装，并补齐 Debian 12/13、Ubuntu 22.04/24.04 与 arm64 的资源清单、CLI 和 Ansible 支持。2026-10-26 将 keepalived 改为由 systemd 托管的 containerd 容器，并完成镜像构建、VIP 故障切换和节点重启回归；2026-11-01 进一步将 HAProxy 切换为相同的 Installer 镜像与 containerd 容器模型，HAProxy 容器化的端到端回归仍待执行。Debian 13、Ubuntu 22.04/24.04 与 arm64 的端到端安装仍待验证。**尚未在真实 RHEL 9 物理机、复杂网络和长期运行场景中验证**。本版只面向全新安装，不执行集群升级或节点扩容。

术语见 [GLOSSARY.md](./GLOSSARY.md)，架构取舍见 [docs/adr](./docs/adr/)。

## 目标与边界

- **离线安装**：Installer Host 联网下载全部资源；目标节点只访问 Installer Host，不需要公网，也不安装 OS 软件仓库中的 Kubernetes/容器运行时代码包。
- **资源版本化**：以 **Build** 为单位同时钉住 Kubernetes、二进制、Helm chart 与容器镜像；切换 Build 即切换整套资源。
- **单一配置入口**：用户只维护 `cluster.yaml`；inventory 与 group_vars 由 `kubedo` 生成，不写入版本库。
- **职责分离**：kubedo 处理集群外资源与命令编排，Ansible 处理主机前置配置、kubeadm、HA、etcd 与 Cilium。
- **支持平台**：目标节点支持 RHEL 系 9、Debian 12/13、Ubuntu 22.04/24.04，CPU 架构支持 `amd64` 与 `arm64`；同一 Build 可同时携带两个架构的资源。
- **显式选平台**：`cluster.yaml` 的 `platform.osFamily` 与 `platform.arch` 决定渲染到 Ansible 的二进制、Helm 归档路径与前置校验；目标节点实际发行版或架构不匹配时安装会立即失败。
- **构建 ABI**：HAProxy 与 keepalived 都在 Installer Host 上从固定源码原生构建，并分别连同运行库打包成按架构区分的 Installer 镜像。两者都不作为宿主机二进制分发，目标节点只拉取对应镜像。Installer Host 的架构必须与目标一致，glibc 不得新于目标节点；本版不做交叉编译。
- **本版范围**：全新安装、资源版本化与 Build 生成；**不包含**原地升级、节点扩容、Registry 认证，以及基于 scp 的内置分发路径。

## 架构

```text
                    Installer Host（联网机 / 安装机）
          ┌────────────────────────────────────────────────┐
          │ kubedo                                         │
          │   :8080  Download Server（HTTP）                │
          │   :5000  Registry（HTTPS + 自签 CA，无认证）    │
          │   cache  /var/cache/kubedo（可配置）             │
          └───────────────────┬────────────────────────────┘
                              │ HTTP 二进制 / HTTPS 镜像
          ┌───────────────────▼────────────────────────────┐
          │ Control Plane ×3                                │
          │   VIP:<apiPort> → HAProxy → 各节点 kube-apiserver:6443
          │   HAProxy 容器 + keepalived 容器 + VIP          │
          │   kubeadm + 本地 etcd（静态 Pod）               │
          └───────────────────┬────────────────────────────┘
                              │
          ┌───────────────────▼────────────────────────────┐
          │ Worker Node ×N                                  │
          │   containerd + Cilium（kube-proxy replacement） │
          └────────────────────────────────────────────────┘
```

### 节点角色

| 角色 | 说明 |
| --- | --- |
| Installer Host（安装机） | 位于被安装集群之外；兼任联网机、Download Server 与 Registry 宿主，执行 `kubedo install` |
| Control Plane（控制平面） | 3 台；HAProxy 容器转发 API 请求，keepalived 容器管理 VIP；etcd 由 kubeadm 以本地静态 Pod 运行 |
| Worker Node（工作节点） | 承载业务负载；容器运行时为 containerd，CNI 为 Cilium |

### 组件职责

- **kubedo（Go）**：下载并校验二进制、直接搬运镜像到私有 Registry、部署或复用 Registry、提供 HTTP 文件服务、生成 Ansible 输入并调用 `ansible-playbook`。
- **Ansible**：目标节点前置检查、containerd 与 kubelet 配置、kubeadm init/join、HAProxy/keepalived 容器、Cilium Helm 安装和安装后验证。
- **CNCF Distribution Registry**：以官方原生二进制和 systemd 运行，不需要 Docker/containerd 宿主；使用 HTTPS、自签 CA、无认证。
- **HAProxy / keepalived**：由上游固定版本源码在 Installer Host 上受控构建，并分别与动态库打包为单架构镜像，由目标机 containerd 在 kubeadm 初始化前启动。目标节点不安装这两个宿主机二进制。构建主机的 glibc 不得新于目标节点，详见 [ADR-0005](./docs/adr/0005-installer-host-build-abi.md) 与 [ADR-0007](./docs/adr/0007-keepalived-runs-as-containerd-container.md)。

## 首个 Build

内嵌 Build 为 `v1.36.5-kubedo.1`，同时提供 **linux/amd64** 与 **linux/arm64** 资源；目标发行版支持 RHEL 系 9、Debian 12/13 与 Ubuntu 22.04/24.04。

| 组件 | 版本 |
| --- | --- |
| Kubernetes（kubeadm / kubelet / kubectl） | v1.36.5 |
| containerd | 2.3.6 |
| runc | 1.5.2 |
| cni-plugins | v1.9.1 |
| crictl（cri-tools） | v1.36.0 |
| Helm | v3.22.0 |
| Cilium chart | 1.20.2 |
| cilium-cli | v0.20.1 |
| Registry（Installer Host） | 3.1.2 |
| HAProxy（Installer Host 构建，容器运行） | 3.2.26 |
| keepalived（Installer Host 构建，容器运行） | 2.4.3 |
| etcd 镜像 | 3.6.8-0 |
| CoreDNS 镜像 | v1.14.2 |
| pause 镜像 | 3.10.2 |

选择 kubeadm（而非 k3s、RKE2 等发行版）是为了保持 etcd、控制平面和证书链路的标准 kubeadm 行为。首个 Build 不采用 Kubernetes 1.37.x：Cilium 1.20.2 的官方兼容矩阵只到 1.36，而覆盖 1.37 的 Cilium 1.21 尚未 GA；本工程优先选择 GA + GA 组合。详见 [ADR-0003](./docs/adr/0003-first-build-baseline-k8s-1.36-cilium-1.20.md)。

## 安装机要求

### Installer Host

- 可访问公网，能访问上游二进制、Helm chart 与镜像仓库。
- 以 `root` 执行部署命令（Registry 的 systemd 部署、节点安装均需要 root 权限）。
- 已安装并可从 `PATH` 调用 `ansible-playbook`；当前使用 Rocky Linux 9 AppStream 提供的 ansible-core 2.14.18 验证。
- 构建 HAProxy 与 keepalived 需要 `gcc`、`make`、`pkg-config`、OpenSSL 开发头文件与可用的 C 工具链；keepalived 的 VRRP 认证需要 OpenSSL EVP 头文件。组装两个 HA 镜像都需要 `ldd` 来收集动态库。RHEL 系通常安装 `openssl-devel`，Debian 系通常安装 `libssl-dev`；kubedo 不会自动安装这些构建依赖。
- Installer Host 的 CPU 架构必须与 `cluster.yaml` 的 `platform.arch` 一致（`x86_64` 对应 `amd64`，`aarch64` 对应 `arm64`）。`kubedo download` 与 `kubedo registry` 在架构不一致时会直接失败，避免把本机架构的 HAProxy/keepalived 镜像或 Registry 二进制误发给目标节点。
- HAProxy/keepalived 必须在 **glibc 不新于目标节点** 的环境中原生构建。RHEL 目标应在 RHEL 系 9 兼容环境构建；Debian/Ubuntu 目标应在对应或更老的 Debian 系环境构建。不要用 Fedora 等更新 glibc 的环境构建。
- `kubedo download` 能比较 `GOOS`/`GOARCH`、校验源码与产物 SHA-256，但无法检测 glibc 符号版本不兼容。在更新版构建机上产出的 HAProxy 或 keepalived 镜像可能在目标节点报 `GLIBC_x.y not found`。
- `kubedo` CLI 本身使用 `CGO_ENABLED=0` 构建为静态二进制，避免安装机/构建机的 glibc 差异影响命令执行。
- 能通过 SSH 访问所有 Control Plane 与 Worker；推荐使用密钥认证。
- 安装机与所有目标节点应启用时间同步；本项目不配置 NTP，证书校验依赖一致的时钟。
- 防火墙开放或关闭：TCP `8080`（Download Server）、TCP `5000`（Registry）。
- 若使用 `selfSigned` Registry 证书，`registry.address` 中的主机名/IP 必须与节点访问地址一致，否则证书校验会失败。该模式不接受外部证书文件；复用已有 Registry 时改用 `provided`。

### 目标节点

- 支持 RHEL 系 9、Debian 12/13、Ubuntu 22.04/24.04；架构支持 `x86_64`/`amd64` 与 `aarch64`/`arm64`。Ansible 会同时校验发行版、主版本和 CPU 架构，必须与 `cluster.yaml` 的 `platform` 一致。
- `root` SSH 可登录。Debian/Ubuntu 目标需预先具备 Python 3 供 Ansible 连接；本项目不从 apt 安装 Python 或任何 Kubernetes/运行时组件。
- 不要求预装 Docker，也不从 OS 仓库安装 Kubernetes、containerd、runc 等组件。
- 基础系统需提供 Python 3、`ip`、`pgrep`、`mount`、`cp`、`modprobe`、`sysctl`、`swapoff`、`sha256sum`、`tar`、`gzip` 与 `systemctl` 等命令；RHEL 还需要 `getenforce`/`setenforce`。kubedo 不补装 OS 包，缺失会在对应阶段直接失败。
- 不要求节点配置可用的上游 DNS。若镜像没有 `/etc/resolv.conf`，前置阶段会创建一份**不含 nameserver** 的占位文件且不覆盖已有配置，使 kubelet 能创建 Pod sandbox；Ansible 随后读取其中的非 loopback nameserver 渲染 CoreDNS，没有上游时省略 `forward` 插件，集群内 DNS 仍可正常工作。
- Ansible 会关闭并禁用 RHEL 的 firewalld 或 Debian 系的 ufw，并完整关闭 swap：停止当前 swap、注释 `/etc/fstab` 中的 swap 条目、停止并屏蔽 systemd `.swap` unit，同时停止、禁用并屏蔽 `zram-generator`、`zram-tools`、`zram-config` 等 zram 服务；zram-generator 的全局配置会被替换为禁用配置。安装后会断言 `/proc/swaps` 为空、fstab 无活动 swap 条目且相关 systemd unit 均已屏蔽。RHEL 系会把 SELinux 设置为 `disabled`（当前启动先切换为 permissive）；Debian 系没有 SELinux 步骤。
- 所有节点需要访问安装机的 TCP `8080`（二进制）与 `5000`（镜像），并能访问 `<VIP>:<apiPort>`（默认 `16443`，用于加入集群和 kubelet 连接 API）。Control Plane 还需要本机 TCP `6443`（kube-apiserver）与 HAProxy 前端端口 `16443`；keepalived 之间还需要 VRRP（IP 协议 `112`）。
- **自动化不会重启任何节点**。SELinux 的持久化配置在下次启动才完全生效；需要合规时请在安装前完成必要的重启窗口。

## 快速开始

以下命令假设 `kubedo` 二进制在当前目录，`cluster.yaml` 已按示例准备好，并且以 root 执行。

### 1. 构建与检查配置

```bash
CGO_ENABLED=0 go build -trimpath -o kubedo ./cmd/kubedo
cp examples/cluster.yaml cluster.yaml
$EDITOR cluster.yaml

sudo ./kubedo check --config cluster.yaml --cache-dir /var/cache/kubedo
```

`check` 会校验 `cluster.yaml` 与 Resource Manifest 的一致性、目标平台是否由该 Build 提供、Installer Host 架构是否匹配，以及**当前目标平台**已缓存二进制的 SHA-256。首次运行时资源尚未下载，因此出现 missing warning 是正常的。加上 `--registry` 还会探测 Registry 的 `/v2/` 端点。

### 2. 下载并构建 Installer Host 侧资源

```bash
sudo ./kubedo download \
  --config cluster.yaml \
  --binaries-only \
  --cache-dir /var/cache/kubedo
```

该命令只下载 `cluster.yaml` 目标平台的节点二进制与 Installer Host 二进制，并在本机从固定源码构建 HAProxy/keepalived。下载结果带原子写入与上游 SHA-256 校验；普通二进制在内容校验通过后跳过。构建型二进制只有在产物、`.sha256` sidecar 和 build marker 三者一致时才复用，缺少 sidecar 或来源发生变化会重新构建。HAProxy 与 keepalived 的产物只作为 Installer 镜像内容，不通过 Download Server 分发到目标节点。

HAProxy 与 keepalived 产物都会连同 `ldd` 收集的动态库打成单架构 Installer 镜像（如 `haproxy:3.2.26-amd64` 与 `keepalived:2.4.3-amd64`），在镜像步骤推送到私有 Registry。目标节点不接收这两个宿主机二进制。请在架构一致且 ABI 兼容的环境中执行该步骤；例如安装 arm64 集群时，Installer Host 也必须是 arm64。本版不支持在 amd64 下载机为 arm64 目标构建 HA 产物，会直接报错。

### 3. 部署或复用 Registry

```bash
sudo ./kubedo registry --config cluster.yaml --cache-dir /var/cache/kubedo
```

如果 `registry.address` 指向本机且 `/v2/` 探测失败，`kubedo` 会安装 Distribution Registry、生成自签 CA/服务端证书并启动 systemd 服务；如果 Registry 已可访问，则直接复用。若端口被其他进程占用但 HTTPS 探测失败，命令会报错而不会覆盖现有服务。

首次部署启动 systemd 后，`kubedo` 会最多等待 15 秒轮询 `/v2/`；证书、端口或数据目录有问题时会直接失败，避免把坏 Registry 带到集群安装阶段。自签证书会校验 SAN、证书链和私钥，Registry 地址变更后会重新生成而不是继续复用旧证书。

生成的 systemd unit 显式设置 `OTEL_TRACES_EXPORTER=none`，避免 Distribution 3.1.2 在没有 OTLP collector 的环境中持续打印导出失败日志。

部署前可先检查将要写入的文件。`--dry-run` 会在 `--output-dir` 下生成 `bin/registry`、`config.yml`、`tls/` 和 `kubedo-registry.service`，但不探测在线服务、不写系统目录、也不操作 systemd；因此即使目标端口已被占用也能正常生成 bundle。相对 `--output-dir` 会先解析为绝对路径，生成的 `config.yml` 与 systemd unit 也使用该绝对路径，便于直接审阅或整体搬运：

```bash
./kubedo registry --config cluster.yaml --cache-dir /var/cache/kubedo --dry-run --output-dir registry-bundle
```

### 4. 搬运镜像

```bash
sudo ./kubedo download \
  --config cluster.yaml \
  --images-only \
  --cache-dir /var/cache/kubedo
```

节点镜像使用内嵌 `go-containerregistry` 直接拉取并推送，不落本地磁盘；原始 manifest index 会被保留，多架构镜像不会退化为单架构。Installer 镜像则由本机缓存的构建产物与动态库组装后直接推送。镜像统一推送为 `<registry>/<namespace>/<target>`，例如 `10.0.0.10:5000/kubedo/pause:3.10.2`、`10.0.0.10:5000/kubedo/haproxy:3.2.26-amd64` 与 `10.0.0.10:5000/kubedo/keepalived:2.4.3-amd64`。

### 5. 启动 Download Server

在 **另一个终端** 中保持进程运行：

```bash
sudo ./kubedo serve --dir /var/cache/kubedo --addr :8080
```

目标节点通过 `http://<installer.address>:<installer.downloadPort>` 拉取二进制。`serve` 必须持续运行到安装结束；当前 Ansible 角色使用 HTTP 下载，不内置 scp 分支。Download Server 没有认证与 TLS，只应暴露在受控管理网络中。

### 6. 执行安装

```bash
sudo ./kubedo check --config cluster.yaml --cache-dir /var/cache/kubedo --registry
sudo ./kubedo install --config cluster.yaml
```

`install` 默认先把 inventory 与 group_vars 渲染到 `rendered/`，再从 `ansible/` 目录执行 `site.yml`。常用选项：

```bash
sudo ./kubedo install --config cluster.yaml --check       # Ansible check mode
sudo ./kubedo install --config cluster.yaml -v            # 增加 Ansible 详细度，可重复
sudo ./kubedo install --config cluster.yaml --tags ha     # 只执行指定 tag
sudo ./kubedo install --config cluster.yaml --limit cp-1  # 限制目标主机
sudo ./kubedo install --config cluster.yaml --skip-render # 复用已有 rendered/
```

## 配置：cluster.yaml

完整示例见 [examples/cluster.yaml](./examples/cluster.yaml)。配置使用严格字段校验，拼错字段会直接报错。

```yaml
apiVersion: kubedo.io/v1alpha1
kind: ClusterConfig
metadata:
  name: demo
build: v1.36.5-kubedo.1
platform:
  osFamily: rhel   # rhel | debian
  arch: amd64      # amd64 | arm64

installer:
  address: 10.0.0.10
  downloadPort: 8080

registry:
  address: 10.0.0.10:5000
  dataDir: /var/lib/kubedo/registry
  tls:
    mode: selfSigned

ssh:
  user: root
  port: 22
  # privateKeyFile: /root/.ssh/kubedo

controlPlane:
  vip: 10.0.0.100
  apiPort: 16443
  vrrp:
    routerId: 51
    password: kubedo
  nodes:
    - name: cp-1
      address: 10.0.0.11
    - name: cp-2
      address: 10.0.0.12
    - name: cp-3
      address: 10.0.0.13

workers:
  - name: worker-1
    address: 10.0.0.21
  - name: worker-2
    address: 10.0.0.22

cni:
  name: cilium
  kubeProxyReplacement: true
  podCIDR: 10.244.0.0/16
  serviceCIDR: 10.96.0.0/12
  clusterDomain: cluster.local
```

### 默认值

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `platform.osFamily` | `rhel` | `rhel` 或 `debian`；后者覆盖 Debian 12/13 与 Ubuntu 22.04/24.04 |
| `platform.arch` | `amd64` | `amd64` 或 `arm64`；必须与 Manifest、目标节点和 Installer Host 一致 |
| `installer.downloadPort` | `8080` | Download Server 端口 |
| `ssh.user` | `root` | Ansible SSH 用户 |
| `ssh.port` | `22` | Ansible SSH 端口 |
| `registry.dataDir` | `/var/lib/kubedo/registry` | Registry 数据目录；必须是绝对路径 |
| `registry.tls.mode` | `selfSigned` | 也支持 `provided` |
| `controlPlane.apiPort` | `16443` | HAProxy 对外 API 端口；**禁止设为 6443** |
| `controlPlane.vrrp.routerId` | `51` | VRRP router ID，1–255 |
| `controlPlane.vrrp.password` | `kubedo` | VRRPv2 密码，1–8 个字符，仅限字母、数字、`.`、`_`、`-` |
| `cni.name` | `cilium` | 当前仅支持 Cilium |
| `cni.kubeProxyReplacement` | `true` | 当前必须为 true |
| `cni.podCIDR` | `10.244.0.0/16` | Pod 网段 |
| `cni.serviceCIDR` | `10.96.0.0/12` | Service 网段 |
| `cni.clusterDomain` | `cluster.local` | 集群 DNS 域 |

`controlPlane.apiPort` 不能是 `6443`：kube-apiserver 在每个 Control Plane 节点监听 `6443`，HAProxy 必须使用不同的前端端口，否则无法绑定。`controlPlaneEndpoint` 会渲染为 `https://<VIP>:16443`，kubeadm 使用不带 scheme 的 `<VIP>:16443`。

配置在渲染与安装前会做语义校验，常见约束包括：

- `metadata.name`、节点名与 `cni.clusterDomain` 必须是合法的小写 DNS 名称；节点地址必须是 IPv4，且节点名、地址不能重复，VIP 不能与节点地址冲突。
- `installer.address` 只填写主机名或 IP，不带 scheme 和端口；`registry.address` 必须是 `host:port`。
- `platform.osFamily` 只允许 `rhel`、`debian`；`platform.arch` 只允许 `amd64`、`arm64`。Manifest 必须声明对应平台，目标节点实际发行版/架构也必须匹配。
- `registry.dataDir` 必须是绝对路径；`provided` 模式下的 `registry.tls.caFile`、`certFile` 与 `keyFile` 也必须是绝对路径。
- `controlPlane.vrrp.password` 只能使用 `A-Z`、`a-z`、`0-9`、`.`、`_`、`-`，长度为 1–8。
- `cni.podCIDR` 与 `cni.serviceCIDR` 必须是 IPv4 网段且不能重叠。
- `build` 必须是安全的单段名称，不能包含路径分隔符或 `..`。

### Registry TLS

- `selfSigned`：`kubedo registry` 生成 CA、服务端证书和私钥，正常部署时固定位于 `/etc/kubedo/registry/tls/`；`--dry-run` 时写入 bundle 的 `tls/` 目录。证书使用 ECDSA P-256，SAN 包含 `registry.address` 的主机名/IP，并附带 localhost 与 loopback 地址。节点通过 `containerd` 的 `hosts.toml` 信任 `/etc/kubedo/registry/tls/ca.crt`。该模式**不接受** `caFile`、`certFile` 或 `keyFile`，配置了会直接报错。
- `provided`：必须同时提供 `registry.tls.caFile`、`certFile`、`keyFile`，三者都必须是绝对路径；适用于复用已有 Registry 的证书链，`caFile` 也是节点与 CLI 使用的信任锚。
- Registry 无认证。`registry.credentialsFile` 只用于访问需要认证的上游仓库或目标仓库，格式兼容 Docker `config.json`。

已有 Registry 的 TLS 配置示例：

```yaml
registry:
  address: registry.example.com:5000
  dataDir: /var/lib/kubedo/registry
  tls:
    mode: provided
    caFile: /etc/kubedo/certs/ca.crt
    certFile: /etc/kubedo/certs/registry.crt
    keyFile: /etc/kubedo/certs/registry.key
```

复用自签证书前会验证 CA、私钥、SAN、有效期和证书链；`registry.address` 变化或证书失效时，`kubedo` 会自动重新生成，而不是继续复用旧证书。`selfSigned` 与 `provided` 在配置语义上互斥，不能只覆盖其中一张证书。

由于 Registry 无认证，只应部署在受控管理网络中；任何能访问 `:5000` 的主体都可以推送镜像。

## CLI 命令

| 命令 | 作用 |
| --- | --- |
| `kubedo version` | 输出编译版本、commit 与构建时间 |
| `kubedo check` | 校验配置、Manifest、缓存二进制；可选探测 Registry |
| `kubedo download` | 下载二进制、构建 HA 二进制与 Installer 镜像、搬运节点镜像 |
| `kubedo registry` | 部署或复用私有 Registry |
| `kubedo serve` | 启动 HTTP Download Server |
| `kubedo render` | 生成 Ansible inventory 与 group_vars |
| `kubedo install` | 渲染输入并执行 `ansible-playbook` |
| `kubedo manifest bump` | 基于显式上游版本生成新的 Resource Manifest |
| `kubedo completion` | 生成 shell 自动补全脚本 |

### `check`

```bash
kubedo check --config cluster.yaml [--manifest build.yaml] \
  [--cache-dir DIR] [--registry] [--strict]
```

`--strict` 会把 warning 视为失败。`--registry` 只验证 Registry HTTPS `/v2/` 可达，不代表 Manifest 中的每个镜像都已存在。

### `download`

```bash
kubedo download --config cluster.yaml [--build BUILD] [--manifest build.yaml] \
  [--cache-dir DIR] [--binaries-only | --images-only] \
  [--insecure] [--ca-file CA] [--credentials-file config.json]
```

- `--build` 可临时选择与 `cluster.yaml` 不同的 Build；此时会提示配置仍钉住原 Build。
- 二进制下载范围由 `platform.osFamily`/`platform.arch` 决定；缓存仍按 `<build>/linux-<arch>` 隔离。Installer Host 架构与目标架构不一致时命令会失败，`--images-only` 也不例外。
- `--insecure` 仅用于本地或受控测试中的明文 HTTP Registry；生产建议使用 HTTPS。
- `--ca-file` 默认取 `registry.tls.caFile`；`selfSigned` 模式固定读取 `/etc/kubedo/registry/tls/ca.crt`，不接受配置覆盖。
- 二进制与镜像默认都会处理。镜像阶段会先为当前平台组装并推送 `spec.installer.images`（如 keepalived），再搬运 `spec.images` 的节点镜像；输出中的 `built_images` 与 `mirrored` 分别计数。
- 首次安装通常按上文顺序先执行 `--binaries-only`，再执行 `--images-only`。后者要求 Installer 镜像所需的构建型二进制及其 sidecar 已存在，但不会隐式重新构建。

### `registry`

```bash
kubedo registry --config cluster.yaml [--build BUILD] [--manifest build.yaml] \
  [--cache-dir DIR] [--dry-run] [--output-dir DIR]
```

非 dry-run 需要 root。固定部署路径为 `/usr/local/bin/registry`、`/etc/kubedo/registry/config.yml`、`/etc/kubedo/registry/tls/` 与 `/etc/systemd/system/kubedo-registry.service`。`--output-dir` 默认为 `registry-bundle`。

`--dry-run` 只渲染 bundle，不探测 Registry、不修改系统；可用于安装前审查配置、证书与 systemd unit。相对 `--output-dir` 会解析为绝对路径，bundle 内的 `config.yml`、TLS 路径和 systemd `ExecStart` 均使用绝对路径。

### `serve`

```bash
kubedo serve [--dir /var/cache/kubedo] [--addr :8080]
```

默认目录优先级为 `--dir` > `KUBEDO_CACHE_DIR` > `$XDG_CACHE_HOME/kubedo` > `~/.cache/kubedo`。服务以只读文件服务器方式暴露缓存根目录，Manifest 中二进制的 URL 位于 `artifacts/<build>/<os>-<arch>/` 下。

### `render`

```bash
kubedo render --config cluster.yaml [--manifest build.yaml] [--out rendered]
```

生成 `rendered/inventory.ini` 与 `rendered/group_vars/all.yml`。inventory 中的每个节点都包含 `ansible_host`、`ansible_user`、`ansible_port` 和 `kubedo_node_ip`；配置了 `ssh.privateKeyFile` 时还会写入 `ansible_ssh_private_key_file`。

### `install`

```bash
kubedo install --config cluster.yaml [--manifest build.yaml] \
  [--out rendered] [--ansible-dir ansible] [--playbook site.yml] \
  [--limit HOST] [--tags tag1,tag2] [--extra-var key=value] \
  [--check] [-v] [--skip-render]
```

`--ansible-dir` 默认为 `ansible`，也可用 `KUBEDO_ANSIBLE_DIR` 指定；playbook 的相对路径以该目录为基准。需要 Installer Host 的 `PATH` 中能找到 `ansible-playbook`。

## Ansible 安装流程

`ansible/site.yml` 按以下顺序执行：

1. **prerequisites**：平台断言、swap（fstab、systemd `.swap`、zram）、内核模块、sysctl、firewalld、SELinux；完成后逐节点验证 swap 已完全关闭，绝不自动重启。
2. **containerd**：安装 containerd/runc/cni-plugins/crictl；使用 containerd 2.x 原生 v4 配置，钉住私有 Registry 中的 pause sandbox 镜像，启用 `SystemdCgroup` 与 CRI runtime，配置 Registry CA 和 `hosts.toml` 后启动 containerd。归档按 `<name>-<version>` 解压到 `/var/cache/kubedo/` 下，避免切换 Build 后复用旧版本目录；containerd 与 CNI 插件逐文件安装并显式设置为 `0755`（对目录递归复制不会可靠保留可执行位）。
3. **kubernetes**：安装 kubeadm/kubelet/kubectl 与 kubelet systemd unit；kubelet 同时使用 bootstrap kubeconfig 和最终 kubeconfig，由 kubeadm 完成引导，并通过 `failSwapOn: true` 拒绝在残留 swap 的节点上启动。
4. **ha**：systemd 通过 `ctr` 分别拉起 HAProxy 与 keepalived 容器，两者都使用 hostNetwork 和 hostPID。HAProxy 挂载 `/etc/haproxy` 与 `/run/haproxy`；keepalived 额外增加 `CAP_NET_ADMIN`/`CAP_NET_RAW`，挂载 `/etc/keepalived` 和 `/run/keepalived`。共享 hostPID 后，keepalived 仍可用原生 `vrrp_track_process` 跟踪容器中的 HAProxy 进程。角色会按节点自身地址选择非 loopback 网卡、继承前缀，配置 VRRP unicast 与 VIP；VIP 自身不参与网卡探测，以免 VIP 已落在错误接口时污染后续运行。角色还会停止并禁用旧 `haproxy.service`/`keepalived.service`，删除 `/usr/local/sbin/haproxy`、`/usr/local/sbin/keepalived` 和旧 unit。容器必须在 kubeadm init 前启动，因此不是 Kubernetes 工作负载。
5. **init**：第一台 Control Plane 执行 `kubeadm init --upload-certs`，跳过 `addon/kube-proxy`；InitConfiguration 将 API、etcd 与组件健康检查超时放宽到 5–10 分钟，避免慢速机器冷启动被默认的 1 分钟超时误判。随后按本节点 `/etc/resolv.conf` 重建 CoreDNS Corefile：存在可用上游时转发，否则只提供集群内解析。
6. **join**：其余 Control Plane 与全部 Worker 加入集群。
7. **cilium**：解压 Helm 到版本化目录后，用 Helm 安装 Cilium；启用 kube-proxy replacement，所有镜像指向私有 Registry，`k8sServiceHost` 指向 VIP、`k8sServicePort` 指向 `apiPort`；`hubble.peerService.clusterDomain` 同步为 `cni.clusterDomain`，Hubble relay 启用、UI 关闭。
8. **verify**：等待节点 Ready、Cilium operator、CoreDNS 与 Hubble Relay 就绪，并断言 kube-proxy DaemonSet 不存在。

可用 tag：`prerequisites`、`containerd`、`kubernetes`、`ha`、`init`、`join`、`join-control-plane`、`join-workers`、`cilium`、`verify`。

## Build 与 Resource Manifest

- **Build**：整套离线资源的一次发布号，例如 `v1.36.5-kubedo.1`。
- **Resource Manifest**：某个 Build 的完整资源清单，包含平台、节点二进制、Installer Host 二进制、Installer 生成镜像与节点镜像。
- **Artifact**：清单中的单项二进制或容器镜像。

Manifest 位于 `manifests/builds/`，通过 `go:embed` 编入 `kubedo`；运行时可用 `--manifest` 指向本地文件。清单约束包括：

- 新清单使用 `spec.platforms` 声明一个 Build 支持的全部 `os/arch`；旧的单平台 `spec.platform` 仍可读取，但两者不能同时出现。多平台 Build 的每个平台必须提供**同名且同数量**的节点/Installer 二进制集合与 Installer 镜像集合。
- 二进制必须有 64 位小写 SHA-256；构建型二进制则必须固定 `sourceSha256`，且构建配方只允许 `haproxy` 与 `keepalived`。同一平台内二进制名不得重复，二进制所属平台必须已在 `spec.platforms` 中声明。
- `spec.installer.images` 描述由 Installer 二进制生成的单架构运行镜像；每项必须引用同平台、同版本的 `spec.installer.binaries`，target 必须带 `version` 或 `version-<suffix>` 形式的 tag 且在各平台间唯一，多平台 Build 的同名镜像版本必须一致。HAProxy 与 keepalived 都通过该机制从节点二进制清单移到 `haproxy:3.2.26-<arch>` 与 `keepalived:2.4.3-<arch>`。
- Build 名、Registry namespace、二进制名与版本、OS/arch 只能使用安全字符；Build 名同时作为缓存路径段，不能包含路径分隔符或 `..`。
- `spec.images` 的节点镜像必须带 tag；source/target 不能包含 scheme、反斜杠、`..` 或绝对路径，target 必须相对于 `<registry>/<namespace>`。节点镜像清单与架构无关，一个 Build 只维护一份；镜像 index 会原样搬运，保留其中的 amd64/arm64 等平台 manifest。

HAProxy 与 keepalived 的 Installer 镜像在清单中这样声明（每个平台各一项）：

```yaml
spec:
  installer:
    binaries:
      - name: keepalived
        version: 2.4.3
        os: linux
        arch: amd64
        # url/sourceSha256/archive/build: keepalived ...
      - name: haproxy
        version: 3.2.26
        os: linux
        arch: amd64
        # url/sourceSha256/archive/build: haproxy ...
    images:
      - name: keepalived
        version: 2.4.3
        os: linux
        arch: amd64
        target: keepalived:2.4.3-amd64
        binary: keepalived
      - name: haproxy
        version: 3.2.26
        os: linux
        arch: amd64
        target: haproxy:3.2.26-amd64
        binary: haproxy
```

内嵌 Build 名称按 Kubernetes 版本与末尾 `.N` 的数值排序，因此 `...kubedo.10` 会排在 `...kubedo.2` 之后；`bump` 默认从最新内嵌 Build 派生新清单。

### `manifest bump`

`bump` 从现有 Build 克隆并生成新 Build，永不原地修改基础清单：

```bash
# 预览，不写文件
./kubedo manifest bump --kubernetes 1.36.6 --cilium 1.20.2 --dry-run

# 写入 manifests/builds/<默认 Build 名>.yaml
./kubedo manifest bump --kubernetes 1.36.6 --cilium 1.20.2
```

支持显式更新的组件：`--kubernetes`、`--cilium`、`--containerd`、`--runc`、`--cni-plugins`、`--cri-tools`、`--helm`、`--cilium-cli`、`--registry`、`--haproxy`、`--keepalived`。默认命名规则：Kubernetes 版本不变时递增 `.N`，Kubernetes 版本变化时从 `.1` 开始；也可用 `--build` 指定。`--from`/`--from-build` 可选择内嵌或本地基础清单。

`bump` 会为清单中的**所有平台**重新计算二进制 checksum 与 URL；Kubernetes 依赖镜像只解析一次并在平台间共享，Cilium chart 的校验值会同步到每个平台的清单项。解析 kubeadm 镜像时要求 Installer Host 架构在清单中存在对应的 kubeadm。生成后仍应人工审查兼容性与镜像清单；写入的新 YAML 需要重新 `go build` 才会变成内嵌 Build。**本版 `bump` 只生成清单，不执行集群升级，也不会伪造异架构 HAProxy/keepalived 构建产物。**

维护 Cilium values 时，operator 的镜像仓库必须写成 `<registry>/<namespace>/cilium/operator`；Helm chart 会自动追加 `-generic`，最终指向 Manifest 中的 `.../operator-generic:<tag>`。不要提前写成 `operator-generic`，否则会生成 `operator-generic-generic`。

## 缓存与分发

缓存目录优先级为 `--cache-dir` > `KUBEDO_CACHE_DIR` > `$XDG_CACHE_HOME/kubedo` > `~/.cache/kubedo`。布局如下：

```text
<cache-dir>/artifacts/<build>/<os>-<arch>/<name>
<cache-dir>/artifacts/<build>/<os>-<arch>/<name>.sha256      # 构建型二进制
<cache-dir>/artifacts/<build>/<os>-<arch>/.<name>.build.json # 构建缓存标记
```

普通二进制按 Manifest 的 `sha256` 校验；HAProxy/keepalived 在上游源码校验通过后于本机构建，产出再计算 SHA-256 并写 sidecar，供镜像构建校验。构建型缓存只有在产物、sidecar 与 build marker 一致时才复用；sidecar 缺失会被重建，避免旧产物被错误当作可用缓存。`check` 发现构建型二进制缺少 sidecar 时只会给出 `unverified` warning。`download --images-only` 不会隐式构建二进制；若 HAProxy 或 keepalived 缓存及其 sidecar 缺失，会提示先运行 `--binaries-only`。

## 仓库布局

```text
kubedo/
├── cmd/kubedo/            # CLI 入口
├── internal/              # archive / manifest / bump / download / registry / check / serve / render / install
├── manifests/builds/      # Resource Manifest，一个 Build 一份 YAML
├── examples/cluster.yaml  # 用户配置示例
├── ansible/               # site.yml、roles、模板与 ansible.cfg
├── docs/adr/              # 架构决策记录
├── GLOSSARY.md
└── README.md
```

`rendered/`、`dist/`、`registry-bundle*/`、根目录的本地 `cluster*.yaml` 与缓存已在 `.gitignore` 中忽略。

## 开发与验证

```bash
CGO_ENABLED=0 go build -trimpath -o kubedo ./cmd/kubedo
gofmt -l ./cmd ./internal ./manifests
go vet ./...
go test ./...
go test -race ./...
```

Ansible 语法检查：

```bash
kubedo render --config cluster.yaml --out rendered
cd ansible
ansible-playbook -i ../rendered/inventory.ini --syntax-check site.yml
```

### 验证环境

本轮验收在自建 **libvirt/QEMU 虚拟机实验环境**中完成，不是 RHEL 物理机：

- 1 台 Installer Host（`192.168.124.10`）：双网卡，集群侧静态地址，另一侧提供联网与软件准备能力。
- Rocky Linux 回归：3 台 Control Plane（`192.168.124.11`–`.13`）和 1 台 Worker（`192.168.124.21`），连接隔离的 `kubedo-lab` libvirt 网络。
- Debian 回归：3 台 Control Plane（`192.168.124.31`–`.33`）和 1 台 Worker（`192.168.124.34`），使用同一隔离网络与 Installer Host，目标机无法访问公网。
- 基础镜像为 Rocky Linux 官方 `Rocky-9-GenericCloud-Base.latest.x86_64.qcow2`：<https://dl.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2>。
- 本机基础镜像 SHA-256 为 `92c206cc6f790c61583247eefe87890f8828420662c17cacf247cec78ab4eec8`，与 Rocky 官方 `CHECKSUM` 文件一致。
- Debian 回归使用 Debian 官方 `bookworm/latest/debian-12-genericcloud-amd64.qcow2`：<https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2>。本机 SHA-512 为 `9f181877006e8d9ca4f4554eba8729a829936f0f7d1db5e77f69b1ef0f6bc372b4d46f3dae39b10d84f9131496c2cc1d449264f26577fe934a79de5a5512a3dd`，与 Debian 官方 `SHA512SUMS` 一致。
- 每台目标机使用独立 qcow2 overlay 从空白磁盘启动；Rocky 最终回归使用 `run4`，Debian 回归使用 `run5`，均未复用旧集群磁盘。

因此，文中的“Rocky Linux 9.8 / amd64 已验证”指的是上述 Rocky 官方 cloud image 的虚拟机组合，不等同于真实 RHEL 9 物理机、真实交换机或生产网络验证；同样，“Debian 12 / amd64 已验证”指 Debian 官方 cloud image 虚拟机组合，不等同于 Ubuntu 或 Debian 13 已验证。arm64 目前只完成代码、资源与构建验证，不能据资源清单支持推断为已实机安装。

### 已验证

- **全新安装回归**：2026-10-24 从空白 Rocky Linux 9.8 cloud image 重建 4 台目标机；安装前确认 4 台节点均无 `/etc/resolv.conf`、无公网 DNS、公网地址不可达且 SELinux 为 `Enforcing`。完整执行 `kubedo install` 后 systemd `Result=success`、退出码为 0，没有在半安装节点上补跑。
- `gofmt`、`go vet`、`go test -race ./...` 与 `CGO_ENABLED=0` 静态 CLI 构建通过；归档解压测试覆盖安全符号链接、安全硬链接、越界链接拒绝与 mtime 保留。
- 2026-10-25 增加多平台 Manifest、平台选择与跨架构下载保护的单元测试；`CGO_ENABLED=0 GOARCH=arm64` 的静态 `kubedo` 可构建并在 QEMU binfmt 下执行 `version`。
- linux/arm64 的 Kubernetes、containerd、runc、CNI plugins、crictl、Helm、Cilium CLI、Cilium chart 与 arm64 Registry 共 11 个上游 Artifact 已下载，并逐项与上游 `.sha256`、`sha256sum` 或 Helm index digest 核对。arm64 的 HAProxy 3.2.26 与 keepalived 2.4.3 构建配方已在 Fedora 44 aarch64 容器中实际编译，生成 ELF aarch64 产物并成功执行 `--version`；该次构建仅验证构建路径，不用于 Debian 目标的 ABI 分发。
- Ansible `--syntax-check` 在 ansible-core 2.14.18 下通过；containerd 2.3.6 使用 v4 配置执行 `config dump`，无废弃字段迁移告警。
- Rocky Linux 9.8 / amd64 上完成 13/13 个二进制与 Helm chart 的准备；HAProxy 3.2.26 与 keepalived 2.4.3 从固定源码构建成功，所有普通资源均通过 Manifest SHA-256 校验，构建型资源生成并校验 sidecar。
- 目标节点保持无公网访问，只从 Installer Host 的 HTTP Download Server 拉取二进制、从自签 HTTPS Registry 拉取镜像；11 个 Manifest 镜像全部出现在 Registry catalog 中。
- containerd 2.3.6、kubelet v1.36.5 与三份本地 etcd 静态 Pod 正常运行；3 个 Control Plane 和 1 个 Worker 均 `Ready`，执行 `kubedo install` 与 `verify` 均成功。
- Cilium 1.20.2 DaemonSet 4/4、Cilium Envoy 4/4、Cilium Operator 2/2、CoreDNS 2/2、Hubble Relay 1/1 全部就绪；集群中没有 kube-proxy DaemonSet，operator 正确渲染为 `operator-generic`。
- 目标机初始没有 `/etc/resolv.conf` 时，前置阶段创建无 nameserver 的占位文件，kubelet 可正常创建 sandbox；生成的 CoreDNS Corefile 不包含 `forward`，CoreDNS 仍能启动并提供服务发现，Hubble Relay 可解析并连接 `hubble-peer`。
- VIP `192.168.124.100/24` 位于持有节点的 `eth0`；停止 cp-1 keepalived 后 VIP 自动漂移到 cp-2，`https://<VIP>:16443/readyz` 在切换期间持续返回 `ok`；恢复 keepalived 后 VIP 回到 cp-1。
- Registry 3.1.2 通过 Manifest SHA-256 校验；HTTPS `/v2/`、catalog 与 tag API 可查询，目标节点通过 `certs.d/<registry>/hosts.toml` 完整信任自签证书链。
- 4 台节点整机重启后 SELinux 均为 `Disabled`，containerd/kubelet 自动恢复，全部节点重新 `Ready`，VIP 与 `<VIP>:16443/readyz` 正常；firewalld 未运行，swap 已关闭。
- **Debian 12 / amd64 全新安装回归**：2026-10-25 从空白 Debian 12 cloud image 启动 4 台目标节点，`kubedo check` 预检为 `13/13 verified`，完整 `kubedo install` 退出码为 0。3 个 Control Plane 与 1 个 Worker 全部 `Ready`（Debian GNU/Linux 12、containerd 2.3.6、kubelet v1.36.5）；ufw 未安装或未启用、无 SELinux 路径、Helm 使用 `linux-amd64` 归档、etcd 静态 Pod 与 Cilium kube-proxy replacement 均正常，VIP `192.168.124.130:16443` 可用。
- **keepalived 容器化回归（Debian 12 / amd64，HAProxy 仍为旧宿主二进制）**：2026-10-26 由 `kubedo download --images-only` 生成并推送 `192.168.124.10:5000/kubedo/keepalived:2.4.3-amd64`；三台 Control Plane 均通过 `kubedo-keepalived.service` 启动 hostNetwork/hostPID 容器，旧的宿主机 keepalived 二进制与 unit 已移除。停止 cp-1 HAProxy 后 keepalived 将有效优先级从 200 降至 170，VIP 约 6 秒漂移到 cp-2；恢复 HAProxy 后约 4 秒回切，期间 VIP `/readyz` 返回 `ok`。cp-2 整机重启后 containerd、keepalived 容器和 HAProxy 自动恢复，4 节点保持 `Ready`。该轮回归尚未覆盖 HAProxy 容器。
- 实验环境的公网链路无法稳定直连 `registry.k8s.io` 与 GitHub Release，因此资源准备使用了实验清单：部分二进制经 GitHub 代理下载，Kubernetes 镜像从阿里云同步源拉取。二进制与 Helm chart 均按项目内嵌清单的官方 SHA-256 校验；镜像的 tag 与内嵌清单一致，但本轮**没有**将镜像 digest 与 `registry.k8s.io` 官方仓库逐项比对。项目内嵌 Manifest 的正式上游 URL 未被替换。

### 尚未验证

- 真实 RHEL 9 物理机、其他 RHEL 9.x 小版本，以及多网卡、非默认 MTU、真实交换机网络下的 VRRP 与 Cilium 行为。
- Debian 13、Ubuntu 22.04/24.04 与 arm64 目标的端到端集群安装。Debian 12/amd64 已完成实机验证，其余组合只完成代码、Ansible 分支和资源校验。
- arm64 Installer Host 到 arm64 目标节点的 glibc ABI 组合、HAProxy/keepalived 镜像启动与端到端集群安装；当前只证明 arm64 构建配方可产出并启动原生产物。
- 多 Worker 编排、长期运行稳定性、断网/断电恢复，以及安装进程中断后的自动续跑。若 Helm 在 `--wait` 阶段被中断，release 可能停留在 `pending-install`；本版不会自动清理，需人工确认后执行 `helm -n kube-system uninstall cilium` 再重跑 Cilium 阶段。
- Installer Host 与目标节点之间的所有 glibc ABI 组合；历史回归证明旧设计的 Rocky Linux 9.8 HAProxy 二进制可分发给 Rocky Linux 9.8 与 Debian 12 节点，并证明 keepalived 镜像可跨发行版运行在 Debian 12；HAProxy 容器镜像尚未完成端到端回归，也未覆盖 Debian 系构建机、arm64 或跨发行版的其他组合。

## 设计决策

- [ADR-0001：kubedo 负责资源层，Ansible 负责集群层](./docs/adr/0001-kubedo-resource-layer-ansible-cluster-layer.md)
- [ADR-0002：目标节点关闭 SELinux](./docs/adr/0002-selinux-disabled-on-target-nodes.md)
- [ADR-0003：首个 Build 的版本基线](./docs/adr/0003-first-build-baseline-k8s-1.36-cilium-1.20.md)
- [ADR-0004：Registry 采用 CNCF Distribution，不部署 Harbor](./docs/adr/0004-registry-is-distribution-not-harbor.md)
- [ADR-0005：Installer Host 构建 ABI 必须兼容目标平台](./docs/adr/0005-installer-host-build-abi.md)
- [ADR-0006：一个 Build 多平台资源与 Debian/arm64 选择](./docs/adr/0006-multi-platform-manifest-and-debian-arm64.md)
- [ADR-0007：HAProxy 与 keepalived 在 kubeadm 前以容器运行](./docs/adr/0007-keepalived-runs-as-containerd-container.md)

## 不在本版范围

- `kubeadm upgrade` 原地升级、Build 切换与回滚
- 节点扩容与节点移除
- Harbor 等重载镜像仓
- Registry 认证、RBAC、Web UI 与镜像复制
- 为异架构交叉构建 HAProxy/keepalived，以及非 Linux 目标
- 基于 scp 的内置二进制分发
