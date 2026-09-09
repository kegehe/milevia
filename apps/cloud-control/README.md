# Milevia Cloud Control

云端 Control Plane 只保存电脑实例状态、远程命令和任务事件摘要，不保存项目源代码、AI 凭据或完整运行目录。

```powershell
$env:MILEVIA_CLOUD_DATABASE_URL = "postgres://milevia:password@127.0.0.1:5432/milevia"
$env:MILEVIA_CLOUD_AGENT_TOKENS = '{"instance-id":"replace-with-instance-agent-token"}'
$env:MILEVIA_CLOUD_ENROLLMENT_TOKEN = "replace-with-release-enrollment-token"
$env:MILEVIA_CLOUD_USER_TOKEN = "replace-with-mobile-token"
$env:MILEVIA_CLOUD_APP_URL = "https://milevia.example.com"
go run ./cmd/cloud-control
```

`MILEVIA_CLOUD_AGENT_TOKENS` is a JSON object mapping each instance ID to its own
agent token. Do not reuse one token across computers. Set `VITE_CLOUD_URL` to the
same public origin when building the mobile web app, or leave it empty when the
Web app and Cloud Control are served behind the same reverse proxy.

新安装包可使用 `MILEVIA_CLOUD_ENROLLMENT_TOKEN` 调用 `/v1/agent/register`，换取
每台电脑独立的实例 ID 和 Agent Token。注册令牌必须放在部署环境中并在发布后轮换；
新凭据保存在云端数据库，旧的 `MILEVIA_CLOUD_AGENT_TOKENS` 仅用于兼容既有设备。

发布桌面安装包时，在构建机设置同名的 `MILEVIA_AGENT_ENROLLMENT_TOKEN`，构建脚本会将
它写入仅含引导配置的资源文件；不会复制开发机 `.env.windows` 中的静态实例凭据。注册令牌
当前是共享部署令牌，适合受控分发；生产化多租户部署应进一步改为一次性、限时注册票据。

默认监听 `:8090`，可通过 `MILEVIA_CLOUD_ADDR` 修改。

服务器复用已有 PostgreSQL 时，可使用 `infrastructure/compose.server.yaml`。Cloud Control
只需要 PostgreSQL；当前实现不依赖 Redis 或 MinIO。数据库应使用独立数据库和账号，且
Cloud Control 容器需加入现有 PostgreSQL 所在的 Docker network。

接口：`/health`、`/v1/agent/register`、`/v1/instances`、`/v1/instances/{instanceId}/overview`、`/v1/instances/{instanceId}/snapshot`、`/v1/instances/{instanceId}/commands`、`/v1/instances/{instanceId}/revoke`、`/v1/commands/{commandId}`、`/v1/events?instanceId=...`、`/v1/agent/connect?instanceId=...`、`/v1/agent/events`。

移动端写操作必须携带 `Authorization: Bearer ...` 和 `Idempotency-Key`。扫码 claim 成功后会签发仅限该电脑实例的 90 天访问令牌；撤销实例会立即使该令牌失效。Agent 接口使用 `X-Milevia-Agent-Token`，事件批量上报还需携带 `X-Milevia-Instance-ID`。
