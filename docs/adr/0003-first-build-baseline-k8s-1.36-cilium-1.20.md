# 首个 Build 的版本基线：Kubernetes 1.36.5 + Cilium 1.20.2

首个 Build `v1.36.5-kubedo.1` 钉 Kubernetes v1.36.5 与 Cilium 1.20.2。之所以不用最新的 1.37.x，是因为没有任何 GA 版 Cilium 覆盖 1.37：1.20.2 的兼容矩阵只到 1.36，1.37 仅出现在尚未 GA 的 1.21（`v1.21.0-pre.3`）。离线安装工程以可复现与官方支持为先，故取最近的一对 GA+GA；1.36 也尚未默认启用 `SELinuxMount`（1.37 才 GA），与本工程的 SELinux 策略一致。

伴随钉定：containerd 2.3.6、runc 1.5.2、cni-plugins 1.9.1、crictl v1.36.0、cilium-cli v0.20.1、Helm 3.22.0。kubeadm 默认镜像：etcd 3.6.8-0、coredns v1.14.2、pause 3.10.2。Helm 取 3.x 而非 4.x，是因为 Ansible 的 `kubernetes.core.helm` 面向 Helm 3。

**Considered Options**: 1.36.5 + Cilium 1.20.2（选中）；1.37.1 + Cilium 1.21.0-pre.3（CNI 用预发布版）；1.37.1 + Cilium 1.20.2（超出官方支持矩阵）。
