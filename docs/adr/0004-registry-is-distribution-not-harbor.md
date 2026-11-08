# Registry 采用 CNCF Distribution 原生二进制，不部署 Harbor

安装机上的 Registry 使用 CNCF Distribution 3.1.2 的官方 `registry` 原生二进制，由 systemd 直接管理（HTTPS + 自签 CA、无认证、数据目录 `/var/lib/kube-do/registry`），运行在集群存在之前。这样安装机完全不需要容器运行时——既不用 Docker，也不用 containerd/nerdctl，比"用容器跑 Registry"少一层依赖。选择 Distribution 而不是 Harbor 的原因是：Harbor 官方只支持 Docker Engine + Compose，而本工程要求不使用 Docker；在 containerd/nerdctl 上跑 Harbor 属于非官方路径（要绕过 Harbor 的安装脚本、自己维护补丁、每次升级都要复现），复杂度远超收益。代价是放弃 Harbor 的认证/RBAC、多租户、Web UI、GC 界面与复制能力；本工程只需要一个能存、能拉、能被节点用 CA 信任的私有仓库。日后若确需 Harbor，退路是"先用 Distribution 引导集群、再用 Helm 把 Harbor 装进集群"。

**Considered Options**: Distribution 3.1.2 原生二进制（选中）；Distribution 容器镜像；Harbor + nerdctl compose（非官方）；Harbor + Docker Engine（违反"不用 Docker"）；先 Distribution 再 Helm 装 Harbor（保留为退路）。
