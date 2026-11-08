# Installer Host 构建 ABI 必须兼容目标平台

HAProxy 与 keepalived 没有本工程可直接使用的官方发布二进制，因此 kube-do 在 Installer Host 上从固定源码原生构建这两个程序。两个产物分别与构建机上的动态库一起打成单架构 Installer 镜像，再推送到私有 Registry；目标节点不接收这两个宿主机二进制。这意味着 Installer Host 不只是“下载机”：它的 CPU 架构、C 运行库和工具链 ABI 会直接决定镜像能否在目标节点运行。

当前约束是 Installer Host 架构与目标一致（`amd64` 或 `arm64`），并且构建环境的 glibc 不新于目标节点：RHEL 系 9 目标使用 RHEL 系 9 兼容环境，Debian/Ubuntu 目标使用对应或更老的 Debian 系环境。Fedora 44 是受支持的例外，但仅当 Installer Host 与全部目标节点都是 Fedora 44；把 Fedora 44 构建的产物分发到 RHEL 9 目标不在支持范围内，常见故障是目标节点报 `GLIBC_x.y not found`。本版不做 HAProxy/keepalived 交叉编译。两个 HA 镜像都自带构建机的运行库，因此镜像运行仍受同一 ABI 约束，详见 [ADR-0007](./0007-keepalived-runs-as-containerd-container.md)。

`kube-do download` 只能校验 `GOOS`/`GOARCH` 以及源码和产物的 SHA-256，无法验证 glibc 符号版本；因此发布 Build 前必须在兼容环境构建，或在 RHEL 9 节点上做实际启动验证。

**Considered Options**: 在与目标同架构、glibc 兼容的环境原生构建（选中）；引入跨架构编译器或容器化构建链（后续可做，但会增加维护面）；在目标节点安装编译工具链（违反目标节点不依赖 OS 包的原则）；使用来源不可验证的第三方二进制（不可接受）。
