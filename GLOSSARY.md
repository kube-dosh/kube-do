# kube-do

kube-do 是一个在离线（air-gapped）环境中安装 Kubernetes 集群的工程：由 `kube-do` 程序准备并分发资源，由 Ansible 编排安装。

## Language

**Build（构建版本）**:
整套离线资源的一次发布号（如 `v1.36.5-kube-do.1`）。切换 Build 意味着整套资源版本的升级。
_Avoid_: 版本号、release、tag

**Resource Manifest（资源清单）**:
某个 Build 下所有 Artifact 的版本化清单。一个 Build 可通过 `spec.platforms` 同时携带多个 `os`/`arch` 平台。
_Avoid_: 依赖清单、物料表、BOM

**Platform（平台）**:
目标节点的发行版家族与 CPU 架构组合，在 `cluster.yaml` 中写作 `platform.osFamily` + `platform.arch`（如 `debian` + `arm64`）。Manifest 使用 `linux` + `arch` 选择二进制。
_Avoid_: 架构、OS（单独指代整个组合时）

**Artifact（资源项）**:
Resource Manifest 中的单项资源，是一个二进制文件或一个容器镜像。
_Avoid_: 依赖、包、asset

**Installer Image（安装器镜像）**:
由 Installer Host 上构建的二进制及其运行库组装、推送到私有 Registry 的单架构运行镜像。HAProxy 与 keepalived 都使用该机制在 kubeadm 之前运行。
_Avoid_: 节点镜像、系统镜像

**Installer Host（安装机）**:
位于被安装集群之外、执行安装命令的机器。默认同时承担联网下载、Download Server 与 Registry 宿主三个职责。
_Avoid_: 工作机、工具节点、跳板机、bastion

**Download Server（下载服务器）**:
在 Installer Host 上提供 Artifact 文件下载的服务。
_Avoid_: 文件服务器、资源服务器

**Registry（镜像仓）**:
存放 Resource Manifest 中全部容器镜像的私有镜像仓库。
_Avoid_: 仓库、harbor（指代该概念时）

**Control Plane（控制平面）**:
由 kubeadm 管理、承载 API Server 与 etcd 的节点集合。
_Avoid_: master、主节点

**Control Plane Endpoint（控制平面入口）**:
指向 Control Plane API Server 的稳定访问入口。
_Avoid_: LB、vip 地址、apiserver 地址

**Worker Node（工作节点）**:
承载业务负载的集群节点。
_Avoid_: 工作机、从节点
