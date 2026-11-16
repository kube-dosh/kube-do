# HA 运行时镜像由 CI 预构建并按 Digest 固定

HAProxy 与 keepalived 是 kube-do 的运行时依赖，不是用户的业务负载。Installer Host 不应为了安装集群而安装 C 工具链、重复编译这两个组件，更不应让构建机的 glibc 版本决定目标节点能否启动 HA 容器。默认路径改为：kube-do CI 从固定源码在 AlmaLinux 9 基线中构建 `linux/amd64` 与 `linux/arm64` 镜像，推送到 GHCR，并将每个单架构镜像的 `sha256` digest 写回 Resource Manifest；`kube-do download --images-only` 只做 OCI 镜像搬运和 digest 校验，不执行编译。

Manifest 的 `spec.installer.images[]` 支持两类条目。预构建条目使用 `source`、`digest` 与 `target`，其中 `source` 必须是带 tag 的 OCI 引用，`digest` 必须是 `sha256:<64 hex>`，搬运前后都要校验 digest。旧条目使用 `binary` 指向 Installer Host 二进制，仍可用于离线应急、尚未发布镜像的开发 Build，或在 CI 不可用时手工构建；两类字段互斥。预构建路径不会读取或编译 `spec.installer.binaries` 中只服务于预构建镜像的 HAProxy/keepalived 源码；`download` 与 `check` 会跳过这些未使用的回退二进制。

CI 使用 AlmaLinux 9 作为构建基线，因为它的 glibc 2.34 不新于当前支持的 RHEL 系 9 与 Debian/Ubuntu/Fedora 目标。镜像只包含二进制、`ldd` 收集的动态库、动态加载器和最小 glibc 配置，不包含 shell 或包管理器。镜像按组件和架构分别打 tag，例如 `kube-do-haproxy:3.2.26-amd64` 与 `kube-do-keepalived:2.4.3-arm64`，不使用跨架构 manifest index；这样 Manifest 可以为每个目标平台固定独立 digest。

预构建镜像的升级必须完成“重建、发布、取得 digest、写回 Manifest”四步。`kube-do manifest bump` 不能可靠猜测新镜像 digest，因此遇到已固定 digest 的 HA 镜像时会拒绝自动 bump HAProxy/keepalived，而不是保留旧镜像或写入未固定的 tag。Kubernetes、Cilium 等不涉及 HA 镜像版本的 bump 不受影响。

在 HA 镜像工作流首次发布产物并取得 digest 之前，仓库内嵌的现有 Build 暂时保留 `binary` 回退，以保证离线安装和测试不依赖尚未存在的 GHCR 包。新 Build 与后续发布应迁移到 `source` + `digest`。

**Considered Options**: CI 预构建并固定 digest（选中）；Installer Host 现场源码构建（保留为离线回退，但不再是默认路径）；使用发行版 `.rpm`/`.deb`（与各发行版 ABI 和包管理器耦合，无法形成统一离线镜像）；直接使用第三方 keepalived 镜像（缺少与 HAProxy 一致的来源、版本和构建策略）；在目标节点安装 HA 软件包（违反目标节点不依赖 OS 仓库的原则）。
