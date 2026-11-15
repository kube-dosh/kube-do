# kube-do API

`kube-do daemon` 提供集群级 HTTP API。CLI 与 daemon 共用同一套资源下载、校验、渲染和 Ansible 安装实现，但 daemon 会把耗时操作转换成异步任务。

## 启动

```bash
sudo kube-do daemon \
  --addr 127.0.0.1:8090 \
  --artifact-addr :8080 \
  --cache-dir /var/cache/kube-do \
  --data-dir /var/lib/kube-do/daemon \
  --ansible-dir /opt/kube-do/ansible \
  --api-token "$KUBE_DO_API_TOKEN"
```

- API 默认只监听 `127.0.0.1:8090`。需要对管理网络开放时显式设置 `--addr`，并通过 `--api-token` 或 `KUBE_DO_API_TOKEN` 启用 Bearer Token。
- `--artifact-addr` 会额外启动与 `kube-do serve` 等价的资源下载服务，默认 `:8080`。集群配置中的 `installer.downloadPort` 必须与该端口一致；不需要时传 `--artifact-addr ""`。
- 集群配置持久化在 `--data-dir/clusters/<name>/cluster.yaml`。任务状态与任务日志保存在 daemon 内存中，daemon 重启后不会保留任务历史。
- `/healthz` 和 `/artifacts/` 不要求认证；`/api/` 在配置 Token 后要求 `Authorization: Bearer <token>`。

## 创建集群

请求体是完整的 `ClusterConfig` YAML。JSON 也可以发送，字段名与 YAML 相同。

```bash
curl -sS -X POST http://127.0.0.1:8090/api/v1/clusters \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary @cluster.yaml
```

返回 `201 Created`。重复的集群名称返回 `409 Conflict`。

## 异步安装集群

```bash
curl -sS -X POST http://127.0.0.1:8090/api/v1/clusters/demo/install \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "prepare": true,
    "check": true,
    "tags": [],
    "extraVars": []
  }'
```

- `prepare` 默认为 `true`：下载节点二进制、构建 Installer 镜像、部署或复用 Registry、搬运 Kubernetes 镜像。
- `check` 默认为 `true`：安装前执行配置、资源和 Registry 预检。
- 返回 `202 Accepted` 和任务对象。安装过程通过任务状态和日志查询。

## 向已有集群新增 Worker

```bash
curl -sS -X POST http://127.0.0.1:8090/api/v1/clusters/demo/nodes \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "role": "worker",
    "nodes": [
      {"name": "worker-3", "address": "10.0.0.23"},
      {"name": "worker-4", "address": "10.0.0.24"}
    ]
  }'
```

daemon 会生成只包含新 Worker 的 `new_workers` Inventory，执行 `ansible/add-worker.yml`：从现有第一个控制平面节点生成 join command，准备新节点，执行 `kubeadm join`，最后等待节点 Ready。成功后新节点才会写入持久化的集群配置。

当前仅支持 Worker 增量纳管。控制平面扩容涉及 etcd 成员、证书和 HA 配置，尚未开放 API。

## 查询

```bash
# 集群列表
curl -sS http://127.0.0.1:8090/api/v1/clusters \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"

# 集群详情（含完整 ClusterConfig）
curl -sS http://127.0.0.1:8090/api/v1/clusters/demo \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"

# 任务列表，可按集群过滤
curl -sS "http://127.0.0.1:8090/api/v1/jobs?cluster=demo" \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"

# 单个任务
curl -sS http://127.0.0.1:8090/api/v1/jobs/<job-id> \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"

# 任务日志（text/plain）
curl -sS http://127.0.0.1:8090/api/v1/jobs/<job-id>/logs \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"
```

任务状态为 `queued`、`running`、`succeeded` 或 `failed`。同一集群同时只允许一个安装或纳管任务，冲突时返回 `409 Conflict`。

## 当前边界

- 不提供集群卸载、节点删除、原地升级或控制平面扩容 API。
- daemon 不在重启后恢复正在执行的任务；任务历史也不持久化。
- API 默认不做 TLS 终止。跨主机访问时建议放在反向代理后，或额外配置网络访问控制。
- `prepare` 会操作安装机的 Registry 和 systemd 服务，通常需要 root 权限。
