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
  --job-timeout 2h \
  --max-job-log-bytes 8388608 \
  --backup-timeout 30m \
  --backup-retain 10 \
  --api-token "$KUBE_DO_API_TOKEN"
```

- API 默认只监听 `127.0.0.1:8090`。需要对管理网络开放时显式设置 `--addr`，并通过 `--api-token` 或 `KUBE_DO_API_TOKEN` 启用 Bearer Token。
- `--artifact-addr` 会额外启动与 `kube-do serve` 等价的资源下载服务，默认 `:8080`。集群配置中的 `installer.downloadPort` 必须与该端口一致；不需要时传 `--artifact-addr ""`。
- 集群配置持久化在 `--data-dir/clusters/<name>/cluster.yaml`。任务状态、结果和日志保存在 `--data-dir/jobs/<id>/`，daemon 重启后仍可查询。
- daemon 重启时，原先处于 `queued` 或 `running` 的任务会标记为 `interrupted`，不会自动重试。
- `--job-timeout` 控制单个任务的运行上限；`--max-job-log-bytes` 控制每个任务保留的日志上限。
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

## etcd 备份

```bash
curl -sS -X POST http://127.0.0.1:8090/api/v1/clusters/demo/etcd-backups \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"
```

任务成功后，任务对象的 `result` 包含快照路径、SHA-256、大小、创建时间以及 `etcdutl snapshot status` 的结构化结果：

```json
{
  "result": {
    "cluster": "demo",
    "filename": "demo-etcd-20261115T120000Z.db",
    "path": "/var/lib/kube-do/daemon/backups/demo/demo-etcd-20261115T120000Z.db",
    "metadataPath": "/var/lib/kube-do/daemon/backups/demo/demo-etcd-20261115T120000Z.db.json",
    "sha256": "...",
    "size": 12345678,
    "createdAt": "2026-11-15T12:00:00Z",
    "validated": true,
    "snapshot": {
      "hash": 123456789,
      "revision": 3549239,
      "totalKey": 466,
      "totalSize": 12345600
    }
  }
}
```

快照由第一个控制平面节点通过 `etcdctl snapshot save` 生成，在同一节点上通过 `etcdutl snapshot status` 校验，然后由 Ansible 拉取到 daemon 数据目录。下载完成后 daemon 会再次核对文件大小和 SHA-256。控制平面节点上的临时快照会在 playbook 结束时清理。

`--backup-min-free-bytes` 控制安装机本地最小可用空间；控制平面 `/var/lib/etcd` 默认要求至少 256 MiB 可用空间。`--backup-retain N` 会在成功后只保留最新 N 份快照，默认 `0` 表示不自动删除。对应 CLI 命令为：

```bash
kube-do etcd backup \
  --config cluster.yaml \
  --backup-dir ./etcd-backups \
  --retain 10 \
  --json
```

## 证书检查

```bash
curl -sS -X POST http://127.0.0.1:8090/api/v1/clusters/demo/certs/check \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"
```

任务会在每个控制平面节点执行 `kubeadm certs check-expiration`，并把结果解析为结构化数据。`result` 包含证书总数、警告/严重/过期/缺失数量，以及逐节点、逐证书的到期时间和剩余天数：

```json
{
  "result": {
    "cluster": "demo",
    "checkedAt": "2026-11-15T12:00:00Z",
    "warnDays": 90,
    "criticalDays": 30,
    "certificateCount": 40,
    "warningCount": 0,
    "criticalCount": 0,
    "expiredCount": 0,
    "missingCount": 2,
    "healthy": false,
    "nodes": [
      {
        "name": "cp-2",
        "address": "10.0.0.12",
        "certificates": [
          {
            "name": "admin.conf",
            "expiresAt": "2027-11-02T02:06:00Z",
            "daysRemaining": 351,
            "authority": "ca",
            "externallyManaged": false,
            "severity": "ok"
          }
        ],
        "missing": ["super-admin.conf"]
      }
    ]
  }
}
```

默认 90 天内到期为 `warning`，30 天内为 `critical`，已过期为 `expired`。缺失证书会进入 `missing` 列表。当前 kubeadm 版本中，由 `kubeadm join --control-plane` 加入的控制平面节点可能缺少 `super-admin.conf`，因此不能仅凭 `missingCount > 0` 判断集群异常，需要结合该节点的安装路径确认。

对应 CLI 命令为：

```bash
kube-do certs check --config cluster.yaml --json
```

## 取消任务

```bash
curl -sS -X POST http://127.0.0.1:8090/api/v1/jobs/<job-id>/cancel \
  -H "Authorization: Bearer $KUBE_DO_API_TOKEN"
```

运行中的任务返回 `202 Accepted` 并开始取消；已完成、已失败、已取消或已中断的任务返回 `409 Conflict`。取消会终止安装机上的 `ansible-playbook` 子进程，但不会自动回滚已经完成的远程变更。

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

任务状态为 `queued`、`running`、`succeeded`、`failed`、`cancelled` 或 `interrupted`。同一集群同时只允许一个任务，冲突时返回 `409 Conflict`。`interrupted` 表示 daemon 在任务完成前停止；重启后不会自动继续执行。

## 当前边界

- 不提供集群卸载、节点删除、原地升级或控制平面扩容 API。
- 不提供 etcd 自动恢复、证书自动轮换或控制平面静态 Pod 自动重启 API。
- daemon 重启后会保留任务历史，但不会恢复正在执行的任务，也不会自动重试中断任务。
- 任务取消依赖 `ansible-playbook` 进程响应上下文取消；已完成的远程操作不会回滚。
- API 默认不做 TLS 终止。跨主机访问时建议放在反向代理后，或额外配置网络访问控制。
- `prepare` 会操作安装机的 Registry 和 systemd 服务，通常需要 root 权限。

真实 API E2E 脚本和 opt-in 安装/扩容说明见 [e2e.md](e2e.md)。
