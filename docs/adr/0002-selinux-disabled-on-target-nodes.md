# 目标节点关闭 SELinux

被安装的 RHEL 系节点在安装前置阶段设置 `selinux=0`，SELinux 彻底关闭。原因是本工程只用上游官方二进制、不引入任何 OS 策略包（如 `container-selinux`），SELinux 启用时 kubelet/containerd 缺少必要策略；同时新版 k8s 的 `SELinuxMount` 默认启用在 SELinux 集群上可能破坏既有工作负载。代价是失去内核级强制访问控制，未来要恢复 enforcing 需补齐策略包并做验证。Debian 系目标通常没有启用 SELinux，Ansible 会跳过这些步骤。

**Considered Options**: 关闭并禁用（选中）；permissive；enforcing + 引入 `container-selinux` 策略包。
