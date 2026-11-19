# 平台矩阵 E2E

该目录提供可重复的 libvirt/QEMU 平台矩阵验证。它不会复用默认的 `192.168.122.0/24` 实验网络，也不会清理任何未由本工具创建的 VM 或网络。

## 场景矩阵

| 场景 | 架构 | 加速 | 验证等级 |
| --- | --- | --- | --- |
| `scenarios/fedora-amd64.yaml` | `amd64` | KVM | 原生完整 E2E |
| `scenarios/debian-amd64.yaml` | `amd64` | KVM | 原生完整 E2E |
| `scenarios/fedora-arm64-tcg.yaml` | `arm64` | QEMU TCG | 功能预验证，不代表原生性能验收 |
| `scenarios/debian-arm64-tcg.yaml` | `arm64` | QEMU TCG | 功能预验证，不代表原生性能验收 |

每个场景使用相同发行版、相同架构的独立安装机，以及三个控制平面和一个 Worker。所有 VM 都是全新 Cloud Image，测试结束后删除。`--topology minimal` 可用于 TCG 测速，只创建首个控制平面和首个 Worker。

## 前置条件

- 宿主机运行 64 位 Linux，具有 libvirt、`virt-install`、`cloud-localds`、`qemu-img`、`ssh-keygen`、`go`、`jq`、`curl` 和 `tar`。
- amd64/KVM 场景需要 `/dev/kvm`。
- arm64 场景需要安装 `qemu-system-aarch64` 与 `edk2-aarch64`。
- 磁盘放在 `/var/lib/libvirt/images/kube-do-e2e`，不使用 `/tmp`。
- 安装机需要访问上游网络。若使用代理，给安装机配置网段网关上的代理；不能让 Guest 使用 `127.0.0.1:1080`。

## 运行

```bash
# 先做不创建 VM 的渲染校验
hack/platform-e2e/run-scenario.sh --dry-run scenarios/fedora-amd64.yaml

# 完整 3 控制平面 + 1 Worker
hack/platform-e2e/run-scenario.sh scenarios/fedora-amd64.yaml

# TCG 最小拓扑测速
hack/platform-e2e/run-scenario.sh --topology minimal \
  scenarios/fedora-arm64-tcg.yaml
```

成功运行后，结果写入 `hack/platform-e2e/results/<scenario>-<topology>-<timestamp>/`，包括安装日志、节点与 Pod 状态、Cilium、etcd endpoint、VIP healthz、证书检查、etcd 备份以及 daemon API E2E 日志。失败时默认保留 VM 和网络，便于排查；成功后自动清理。使用 `--keep` 可保留成功环境。

## 网络与离线边界

- Fedora 场景使用 `192.168.123.0/24`，Debian 场景使用 `192.168.124.0/24`。
- 安装机有默认路由，目标节点只有同网段静态地址，没有默认路由；脚本会在安装前断言这点，避免把“仍可访问公网”误当成离线验证。
- 目标节点只访问安装机的 Download Server `:8080` 和 Registry `:5000`。
- 宿主机代理可通过 `KUBE_DO_E2E_HOST_PROXY` 指定；默认读取场景中的 `proxy.host`。
- 安装机代理可使用 `KUBE_DO_E2E_GUEST_PROXY` 覆盖；设置为 `none` 可禁用。默认使用 `<网关>:1080`。
- 当前代理链路访问 `cdn.registry.k8s.io` 的大层会出现 TLS EOF。测试时可设置 `KUBE_DO_E2E_PRELOAD_IMAGES=1`，由宿主机 Podman 从上游拉取节点镜像并推送到安装机 Registry；安装器只构建 keepalived/HAProxy 镜像，不重复拉取节点镜像。该绕过仅存在于 E2E 工具，不改变生产 `kube-do download` 行为。
- 宿主机镜像分发默认串行；可设置 `KUBE_DO_E2E_IMAGE_JOBS=4`，为每个架构启动多个独立 Podman 存储并行拉取和推送。镜像已存在且架构匹配时会跳过。

## 测试专属超时

生产 Ansible 默认值保持不变。TCG 场景通过 `extraVars` 注入更长超时，并由 `install-in-vm.sh` 转成 `--extra-var`。示例文件位于 `overrides/tcg-kubeadm.yml` 与 `overrides/tcg-verify.yml`。

TCG 下 API Server 可能因宿主机调度抖动短暂拒连，导致 `helm upgrade --install --atomic` 在等待超时前提前退出。`kube_do_cilium_install_retries` 与 `kube_do_cilium_install_retry_delay` 只用于测试场景的安装重试，生产默认保持 `0` 次重试。

TCG 下单次 Kubernetes API 请求可能超过控制器默认的 5 秒租约续期窗口，使 `kube-controller-manager` 或 `kube-scheduler` 丢失领导权。TCG 场景通过 `kube_do_kubeadm_{controller_manager,scheduler}_leader_elect_*` 变量延长租约与重试窗口；这些变量默认为空，不改变生产 kubeadm 配置。

TCG 下 containerd 启动 HAProxy/keepalived 容器可能明显慢于 40 秒。角色默认仍只等待 20 次、每次 2 秒；TCG 场景通过 `kube_do_ha_{container,vip}_ready_{retries,delay}` 延长等待，不修改生产默认值。

TCG 下 systemd 的 `daemon-reload` 也可能超时。HA 角色的单元加载和重启默认重试 3 次；TCG 场景通过 `kube_do_ha_systemd_{retries,delay}` 延长重试窗口。

Fedora TCG 上 `daemon-reload` 的 D-Bus 调用可能持续超过 100 秒，重试无法自愈。两个 TCG 场景因此通过 `kube_do_systemd_daemon_reload: false` 让新增单元按需加载；生产默认仍为 `true`，不改变正常节点的 systemd 重载语义。

`kubeadm init` 无法通过 Ansible 变量关闭内部的 `systemctl daemon-reload`。为避免 TCG 下该调用把初始化卡死，TCG Cloud Image 的 cloud-init 会在测试节点安装 `/usr/local/bin/systemctl` 包装器：它只跳过 `daemon-reload`，其他子命令仍转发给 `/usr/bin/systemctl`。包装器只存在于临时 TCG 测试节点，不进入生产安装路径；跳过日志写入 `/var/log/kube-do-e2e-systemctl.log`。

kubelet 单元带有 `ConditionPathExists=/var/lib/kubelet/config.yaml`。即使 systemd 在配置生成前尝试启动 kubelet，也不会形成崩溃重启循环；kubeadm 写入配置后，正常的 `systemctl start kubelet` 会立即生效。

Fedora Cloud Image 的 `dnf-makecache` 在 TCG 下可能长期占用 CPU 并拖慢后续任务。测试场景的 cloud-init 会在 Fedora TCG 虚拟机中屏蔽该服务和 timer；该处理只存在于临时测试节点，不进入生产默认配置。

Fedora TCG 的 `systemd-logind` 也可能因 D-Bus 超时阻塞新的 SSH session。测试 cloud-init 会仅在该场景停用 `pam_systemd` 的 SSH session 注册，使 Ansible 连接可以建立；目标节点的服务管理仍由 systemd 负责。

Fedora 场景通过 `rpmMirror` 将 Guest 的 `fedora` 与 `updates` RPM 仓库切换到清华 TUNA，并禁用 Cloud Image 自带的上游仓库定义。该配置只存在于临时测试 VM，不改变生产节点。`dl.k8s.io` 在本测试代理链路上可能发生 TLS EOF，因此安装机的测试环境将该域名及其子域加入 `no_proxy`，直接下载 Kubernetes 二进制；容器镜像则在测试环境中由宿主机预分发。这些处理不改变生产安装机的代理配置。

迭代排查时，可以设置 `KUBE_DO_E2E_INSTALL_TAGS=ha,init,join,cilium,verify`，只重跑安装阶段，不重复已经完成的主机前置配置。该变量仅由测试工具转发，不改变 `kube-do install` 的生产默认值。

若实测 etcd 在 TCG 下出现选举或磁盘超时，应先增加测试场景中的 etcd 时序参数，而不是修改生产默认配置。

TCG 会把每次 `fsync` 放大到不可接受的程度，导致 etcd 长时间无法完成 ReadIndex，并连带使 apiserver 在 20 秒硬编码 etcd 拨号窗口内失败。`provision-libvirt.sh` 因此只对 `acceleration: tcg` 的临时 VM 使用 `cache=unsafe`，牺牲掉电持久性换取可完成的功能预验证；原生 KVM 场景仍使用 libvirt 默认缓存策略。

TCG 慢速 udev 可能让 systemd 的 device unit 在 cloud-init 启动前超时。场景可通过 `kernelArgs` 给测试 VM 的引导项追加参数：Fedora 修改 BLS 条目，Debian 修改 GRUB 配置。当前两个 arm64 TCG 场景都使用 `systemd.default_device_timeout_sec=300`，并禁用 IPv6，避免 apiserver 的 loopback 启动 Hook 长时间卡在 `[::1]:6443`。这些参数只写入本次临时 VM 的 overlay 镜像，不进入生产代码或目标节点配置。

TCG 场景还会为 QEMU 配置 1 GiB translation block cache（`tb-cache`），降低控制面启动期间的 JIT 抖动。该设置只由 `provision-libvirt.sh` 应用于 `acceleration: tcg` 的临时 VM，原生 KVM 场景不受影响。

## 清理与恢复

```bash
# 清理指定运行的 VM、网络和临时磁盘
hack/platform-e2e/cleanup-libvirt.sh \
  scenarios/fedora-arm64-tcg.yaml \
  hack/platform-e2e/results/<run-directory>
```

清理脚本只读取本次运行的 `libvirt/vms.txt` 和 `libvirt/state.env`，不会遍历或删除默认网络上的现有 VM。
