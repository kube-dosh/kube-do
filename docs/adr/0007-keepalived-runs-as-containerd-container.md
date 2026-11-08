# HAProxy 与 keepalived 在 kubeadm 前以容器运行

HAProxy 与 keepalived 必须在 kubeadm 初始化之前提供 API 入口和 Control Plane VIP，因此不能作为 Kubernetes DaemonSet 运行。kube-do 在 `kube-do download` 的镜像阶段，把 Installer Host 上已构建并校验的 HAProxy/keepalived 二进制、`ldd` 收集的动态库、动态加载器和最小 glibc 配置文件分别组装成单架构镜像，并额外打入 glibc 在 `pthread_exit` 时运行期加载、不会出现在 `ldd` 输出中的 `libgcc_s.so.1`，再推送到私有 Registry；目标节点通过已有 containerd 拉取镜像，由 `kube-do-haproxy.service` 和 `kube-do-keepalived.service` 调用 `ctr` 启动容器。

两个容器都使用 hostNetwork 和 hostPID。hostNetwork 让 HAProxy 直接监听节点 API 入口端口，并让 VRRP 与 VIP 直接作用于节点网络；hostPID 让 keepalived 的 `vrrp_track_process` 能看到容器中的 HAProxy 进程。HAProxy 不需要额外 capability；keepalived 只增加 `CAP_NET_ADMIN`、`CAP_NET_RAW`、`CAP_NET_BROADCAST`。`/etc/haproxy` 与 `/etc/keepalived` 只读挂载，`/run/haproxy` 与 `/run/keepalived` 可写挂载。systemd 负责开机启动、重启、停止和清理容器，`RuntimeDirectory` 保证节点重启后 tmpfs 目录仍存在。角色会停止并禁用旧的宿主 `haproxy.service`/`keepalived.service`，删除 `/usr/local/sbin/haproxy`、`/usr/local/sbin/keepalived` 与旧 unit。

这样复用目标节点已经安装的 containerd，不引入 Docker 或 nerdctl；镜像自带 C 运行库，不需要在目标节点安装 HAProxy 或 keepalived 的 OS 包。代价是 Installer Host 仍必须与目标同架构且 glibc 不新于目标，节点启动 HA 容器时 Registry 必须可达，而且 hostPID 与网络特权容器扩大了 HA 组件的权限边界。镜像按平台分别打 tag（如 `haproxy:3.2.26-amd64` 与 `keepalived:2.4.3-amd64`），不使用跨架构 manifest index，因为每个镜像包含的是本机构建产出的二进制与库。

**Considered Options**: systemd + containerd 容器（选中）；Kubernetes DaemonSet（不可行，HA 入口必须早于集群）；宿主机二进制（原方案，但需要在目标机分发两个二进制及动态库）；第三方 HAProxy/keepalived 镜像（版本、架构和构建来源不受本工程控制）；Docker 容器（违反目标节点不依赖 Docker 的约束）。
