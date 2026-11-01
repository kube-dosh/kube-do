# kubedo 负责资源层，Ansible 负责集群层

kubedo CLI 只执行与集群无关的资源操作——下载 Artifact、提供 Download Server、准备 Registry、渲染 Ansible inventory 与 group_vars——并最终调用 `ansible-playbook`。集群内的一切操作（kubeadm init/join、keepalived+haproxy、Cilium 的 Helm 安装）全部由 Ansible 完成。这样 kubedo 不持有任何集群内状态，每条命令都能在集群外的 Installer Host 上独立运行；代价是安装必须经由 Ansible，且 kubedo 需保证与 playbook 的兼容性。

**Considered Options**: CLI 只驱动 Ansible（选中）；CLI 亲自执行集群内步骤；CLI 内嵌 Helm SDK 部署 Cilium。
