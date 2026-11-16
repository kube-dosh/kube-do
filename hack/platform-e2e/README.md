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

## 测试专属超时

生产 Ansible 默认值保持不变。TCG 场景通过 `extraVars` 注入更长超时，并由 `install-in-vm.sh` 转成 `--extra-var`。示例文件位于 `overrides/tcg-kubeadm.yml` 与 `overrides/tcg-verify.yml`。

TCG 下 API Server 可能因宿主机调度抖动短暂拒连，导致 `helm upgrade --install --atomic` 在等待超时前提前退出。`kube_do_cilium_install_retries` 与 `kube_do_cilium_install_retry_delay` 只用于测试场景的安装重试，生产默认保持 `0` 次重试。

TCG 下单次 Kubernetes API 请求可能超过控制器默认的 5 秒租约续期窗口，使 `kube-controller-manager` 或 `kube-scheduler` 丢失领导权。TCG 场景通过 `kube_do_kubeadm_{controller_manager,scheduler}_leader_elect_*` 变量延长租约与重试窗口；这些变量默认为空，不改变生产 kubeadm 配置。

TCG 下 containerd 启动 HAProxy/keepalived 容器可能明显慢于 40 秒。角色默认仍只等待 20 次、每次 2 秒；TCG 场景通过 `kube_do_ha_{container,vip}_ready_{retries,delay}` 延长等待，不修改生产默认值。

若实测 etcd 在 TCG 下出现选举或磁盘超时，应先增加测试场景中的 etcd 时序参数，而不是修改生产默认配置。

TCG 慢速 udev 可能让 systemd 的 device unit 在 cloud-init 启动前超时。场景可通过 `kernelArgs` 给测试 VM 的引导项追加参数：Fedora 修改 BLS 条目，Debian 修改 GRUB 配置。当前两个 arm64 TCG 场景都使用 `systemd.default_device_timeout_sec=300`。该参数只写入本次临时 VM 的 overlay 镜像，不进入生产代码或目标节点配置。

## 清理与恢复

```bash
# 清理指定运行的 VM、网络和临时磁盘
hack/platform-e2e/cleanup-libvirt.sh \
  scenarios/fedora-arm64-tcg.yaml \
  hack/platform-e2e/results/<run-directory>
```

清理脚本只读取本次运行的 `libvirt/vms.txt` 和 `libvirt/state.env`，不会遍历或删除默认网络上的现有 VM。
