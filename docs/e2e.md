# daemon API E2E

`hack/e2e-daemon.sh` 针对一个已存在的集群执行真实 daemon API 验证。脚本会启动临时 daemon、注册集群、调用 etcd 备份和证书检查接口，并重启 daemon 验证任务历史持久化。

## 前置条件

- 安装机可以访问目标集群节点，`ansible-playbook` 可直接执行现有 playbook。
- 提供一份与目标集群一致的 `ClusterConfig`，其中 `ssh.privateKeyFile` 等字段必须可由当前用户读取。
- 已安装 `curl`、`jq`、`go`、`sha256sum` 和 `ansible-playbook`。

## 默认验证范围

- `healthz`、Bearer Token 鉴权和错误 Token。
- 集群注册、重复注册冲突、列表与详情读取。
- 不支持的控制平面扩容请求返回 `400`。
- etcd 备份任务成功，快照通过 `etcdutl snapshot status` 校验，下载文件的 SHA-256 与 API 返回值一致。
- 同集群备份运行期间，证书检查返回 `409 Conflict`。
- 证书检查任务成功，返回结构化结果，日志非空。
- 已完成任务再次取消返回 `409`。
- daemon 重启后，已完成任务的状态、结果和日志仍然可读。

该脚本不默认执行安装或新增 Worker。它们会修改真实集群，需要显式开启：

```bash
# 新增 Worker；值是 /api/v1/clusters/<name>/nodes 的请求体
KUBE_DO_E2E_NEW_WORKERS='{"role":"worker","nodes":[{"name":"worker-9","address":"192.0.2.29"}]}' \
  hack/e2e-daemon.sh

# 通过 API 执行安装；可覆盖安装请求体
KUBE_DO_E2E_RUN_INSTALL=1 \
KUBE_DO_E2E_INSTALL_BODY='{"prepare":false,"check":false}' \
  hack/e2e-daemon.sh
```

## 运行

```bash
KUBE_DO_E2E_CONFIG=/path/to/cluster.yaml \
  hack/e2e-daemon.sh
```

常用环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `KUBE_DO_E2E_CONFIG` | 必填 | 已安装集群的 `ClusterConfig` |
| `KUBE_DO_E2E_ADDR` | `127.0.0.1:18090` | 临时 daemon 监听地址 |
| `KUBE_DO_E2E_WORK_DIR` | 自动创建 | 状态、缓存、日志和构建产物目录 |
| `KUBE_DO_E2E_KEEP` | `0` | 设为 `1` 时保留工作目录 |
| `KUBE_DO_E2E_RUN_OPS` | `1` | 设为 `0` 时只验证注册、鉴权和错误路径 |
| `KUBE_DO_E2E_REQUIRE_CONFLICT` | `1` | 要求同集群并发任务返回 `409` |

脚本不是故障注入套件。断电、网络分区、etcd 成员故障、控制平面重启、升级和恢复演练仍需在一次性环境中单独执行。
