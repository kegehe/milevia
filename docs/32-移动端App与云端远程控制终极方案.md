# 移动端 App 与云端远程控制终极方案

> 日期：2026-08-31
> 阶段：架构调研与目标方案
> 目标：通过手机随时查看电脑 Milevia 的项目、任务和执行进度，并创建、管理、下发任务

## 1. 结论

该需求可以在现有 Milevia 基础上实现。当前项目已经具备项目、任务、TaskRun、TaskEvent、Runner 和 WebSocket 实时状态等核心业务能力，主要缺口是：

- 电脑端只监听 `127.0.0.1`，不能接受公网连接；
- 桌面 sidecar 与 Tauri 主进程绑定，关闭桌面窗口后无法继续远程执行；
- 当前 Session Token 是桌面启动级凭证，不适合作为手机设备凭证；
- 没有云端用户、设备、配对、命令路由和事件同步系统；
- WebSocket 主要面向当前浏览器页面，缺少移动端断线补偿和统一事件游标。

最终方案采用：

```text
手机 App
    | HTTPS / WSS
    v
云端 Milevia Control Plane
    | Redis 命令路由与事件广播
    v
电脑 Milevia Agent（主动出站 WSS）
    | 本机回环连接
    v
现有 control-server
    |
项目、任务、Runner、Claude/Codex
```

电脑不开放公网入站端口，只向云端建立出站连接。云端负责身份认证、设备管理、命令路由和事件同步；电脑负责实际项目访问和 AI 执行。

## 2. 产品边界

### 2.1 移动端首要能力

1. 登录账号并管理已配对的电脑。
2. 查看电脑在线状态和最后连接时间。
3. 查看电脑上已加载的项目。
4. 查看项目运行状态、运行时长和当前执行任务。
5. 查看任务列表、任务详情、依赖关系和最近运行记录。
6. 创建、编辑、删除和重新打开任务。
7. 下发、停止任务。
8. 查看执行事件、失败原因和日志摘要。
9. 对待验收任务执行“确认完成”或“要求修改”。
10. 接收任务完成、失败、待验收和需要处理通知。

### 2.2 首期不做

- 移动端完整文件编辑器；
- 移动端全功能 Git 工作台；
- 交互式终端和任意 Shell 执行；
- 移动端修改 API Key、SSH 私钥等敏感凭据；
- 将项目源码和完整运行环境上传到云端；
- 把手机做成远程桌面客户端。

手机端的定位是任务控制台，而不是电脑桌面替代品。

## 3. 当前实现基础与限制

### 3.1 可复用能力

当前 `control-server` 已提供：

- `GET /api/projects`：项目列表；
- `GET /api/projects/statuses`：项目业务状态；
- `GET /api/projects/processes/statuses`：项目进程状态；
- `/api/projects/{projectID}/tasks`：任务列表和创建；
- `/api/tasks/{taskID}`：任务详情和编辑；
- `/api/tasks/{taskID}/dispatch`：下发任务；
- `/api/tasks/{taskID}/stop`：停止任务；
- `/api/tasks/{taskID}/review`：验收或要求修改；
- `/api/tasks/{taskID}/runs`、`/events`：运行和事件历史；
- `/ws/processes`：全局项目进程状态；
- `/ws/projects/{projectID}/run`：项目运行日志；
- `/ws/notifications`：通知；
- `/ws/conversations/{conversationID}`：会话事件。

任务模型已经将计划和执行分离为 `Project -> Task -> TaskRun -> TaskEvent`，移动端应复用这套语义，不再设计第二套任务状态机。

### 3.2 必须解决的限制

桌面端当前通过随机端口、随机 Session Token 和 `127.0.0.1` 保护本地 API。该机制适合桌面 WebView，不适合远程设备。远程能力必须新增独立认证，不应简单把监听地址改成 `0.0.0.0`。

Tauri 当前会监控父进程并在桌面主进程退出后关闭 sidecar。要支持手机在桌面窗口关闭后继续查看和下发任务，必须增加后台 Agent 或服务模式。

### 3.3 数据权威与一致性原则

- 电脑本地 SQLite 是项目、任务、TaskRun 和本地运行状态的唯一权威来源；
- 云端保存的是按电脑实例划分的只读镜像、命令记录、事件记录和在线状态；
- 云端不能直接修改任务镜像，也不能在电脑离线时伪造任务已经创建或执行；
- 手机写操作必须经云端转发给 Agent，由 Agent 调用现有 control-server 的业务服务完成；
- 本地业务状态变更、TaskEvent 和远程 Outbox 必须由 control-server 在同一个 SQLite 事务中写入；Agent 只负责投递命令和异步上传 Outbox，不能在业务事务提交后自行补写 Outbox；
- 手机读取镜像时必须返回 `observedAt`、`sourceRevision` 和 `stale` 标记。

## 4. 云端总体架构

第一阶段采用模块化单体，保持 Go 技术栈一致；连接数和团队规模增长后再拆分服务。

### 4.1 云端模块

| 模块 | 职责 |
| --- | --- |
| API Server | 登录、实例、项目、任务、设备和审计 API |
| Agent Gateway | 接收电脑 Agent 的长期 WSS 连接 |
| Command Router | 按 `instanceId` 路由手机命令 |
| Event Service | 接收 Agent 事件、持久化并向手机广播 |
| Notification Worker | 按订阅类型发送 Web Push/VAPID、APNs 或 FCM 推送 |
| Audit Service | 记录远程操作和安全事件 |

### 4.2 基础设施

- PostgreSQL：账号、设备、电脑实例、命令、事件、审计数据；
- Redis：在线连接表、命令路由、Pub/Sub、短期缓存和限流；
- S3/MinIO：可选，保存压缩后的运行日志、构建产物和其他明确授权的 Artifact；
- Caddy 或 Nginx：TLS 终止、反向代理和限流；
- Prometheus/Grafana：连接数、在线率、命令延迟、失败率和推送指标。

现有 `infrastructure/compose.yaml` 已包含 PostgreSQL、Redis 和 MinIO，可作为单机部署基础。

### 4.3 域名和入口

初期采用一个统一入口，降低备案、证书和部署复杂度：

```text
milevia.example.com                  移动 Web/PWA 静态资源
milevia.example.com/health           Cloud Control 健康检查
milevia.example.com/v1/*             移动端 HTTPS API
wss://milevia.example.com/v1/agent/connect  Agent WSS 长连接
```

由 Caddy 或 Nginx 按路径反向代理：根路径提供 `apps/web/dist`，`/health` 和 `/v1/*` 转发到 Cloud Control。反向代理必须支持 WebSocket Upgrade、HTTPS 和基本限流；API 鉴权与 Agent Token 鉴权仍保持独立。后续需要独立扩容时，再拆分为多个子域名不影响协议设计。

## 5. 电脑 Agent 设计

### 5.1 生命周期

电脑端新增 `milevia-agent` 后台模式，默认采用当前 Windows 用户会话中的后台进程：

- 通过计划任务或托盘启动器在用户登录后启动；
- 使用当前用户的项目目录、CLI 登录状态和凭据；
- 与云端保持 WSS 长连接；
- 连接本机 control-server；
- 在远程后台模式下，桌面窗口关闭或隐藏到托盘后仍保持运行；
- 网络恢复后自动重连；
- Agent 重启后上报完整状态快照。

Windows Service 仅作为后续的无界面执行模式，不作为默认实现。Windows 服务不能直接与用户桌面交互；如果未来使用服务，必须通过受 ACL 保护的命名管道与用户会话中的 UI/审批辅助进程通信。第一版可以先将远程连接器放入现有 Go control-server 的独立后台启动模式（不带 `--parent-pid`），稳定后再拆成独立 Agent 二进制。

进程归属必须明确：桌面开发模式继续使用带 `--parent-pid` 的 sidecar；远程模式由不带 `--parent-pid` 的后台 control-server 作为本地 SQLite 的唯一拥有者。第一版 Agent 可作为该进程内的模块运行；拆分成独立 Agent 后，只能通过带认证的回环 HTTP/IPC 调用业务服务，不能直接打开 Milevia 主 SQLite。两种模式不能同时使用同一数据目录，避免数据目录锁冲突。关闭窗口或隐藏到托盘不等于退出远程模式；用户注销时标记为 `user_session_unavailable`，机器关机、睡眠或网络不可达时标记为 `machine_offline`，Agent 或 control-server 自身停止时标记为 `agent_unavailable`，不得继续报告在线。

### 5.2 本地连接

Agent 与 control-server 之间继续使用本机回环连接或受控 IPC：

- 本地 API 仍只监听 `127.0.0.1`；
- Agent 持有独立于桌面 WebView Session Token 的持久本地服务令牌；令牌首次生成时写入 Windows DPAPI，轮换和撤销由本地 control-server 管理；
- 不允许通过 Agent 转发任意 URL 或任意 Shell；
- 所有可执行操作映射到明确的业务命令。

### 5.3 Agent 身份

每台电脑需要稳定身份：

```text
instance_id
device_id
device_keypair
device_certificate
last_agent_sequence
```

桌面启动级 Session Token 只能继续用于本地 WebView，不得作为云端长期凭证。

## 6. 配对与认证

### 6.1 配对流程

1. Agent 在本地生成设备密钥对，并将私钥保存到 Windows DPAPI。
2. 用户在电脑端打开“远程访问”，电脑生成一次性二维码（带配对句柄 + 本次的 6 位校验码）或单独的 6 位配对码。
3. 手机登录云端账号并扫码 —— **扫码即提交**（手机把二维码里的句柄与校验码一起交给云端），云端创建短时待确认配对请求；二维码被截断/来自旧版本时退回“手动输入 6 位校验码”，两条路汇到同一个 claim 接口。
4. 电脑端明确确认（点击「确认绑定」）后令牌才被激活，云端同时校验手机账号和配对句柄。**二维码/校验码都不是授权凭据，电脑端那一次点击才是。**
5. Agent 使用一次性 bootstrap token 或签名请求完成首次注册；云端签发实例凭证和设备证书。
6. bootstrap 凭证成功、过期或撤销后立即失效，之后 Agent 只能使用 mTLS + WSS 建立长期连接。

配对码应限制有效期、尝试次数和使用次数，成功或过期后立即失效。

### 6.2 手机认证

- Access Token 短期有效；
- Refresh Token 存储在系统安全存储；
- 支持退出登录和刷新令牌轮换；
- 支持查看和撤销手机设备；
- 后续可以增加 Passkey 或 OIDC。

### 6.3 权限

```text
read:projects
read:tasks
read:runs
write:tasks
dispatch:tasks
stop:runs
review:tasks
manage:devices
```

即使当前只有单用户，也应保留实例级和项目级权限字段，为未来多账号做准备。

## 7. 命令和事件协议

### 7.1 手机 API

```text
POST /v1/auth/login
POST /v1/auth/refresh
GET  /v1/instances
POST /v1/instances/{instanceId}/revoke
GET  /v1/instances/{instanceId}/overview
GET  /v1/instances/{instanceId}/snapshot
GET  /v1/instances/{instanceId}/projects
GET  /v1/instances/{instanceId}/projects/{projectId}/tasks
POST /v1/instances/{instanceId}/projects/{projectId}/tasks
GET  /v1/tasks/{taskId}
PATCH /v1/tasks/{taskId}
POST /v1/tasks/{taskId}/dispatch
POST /v1/tasks/{taskId}/stop
POST /v1/tasks/{taskId}/review
POST /v1/tasks/{taskId}/reopen
GET  /v1/commands/{commandId}
```

移动端 API 是云端 API，不直接暴露本机 `control-server` 路由。云端负责权限检查、命令落库和转发。

所有移动端写操作默认返回 `202 Accepted`，响应至少包含 `commandId`、`status` 和幂等键。`accepted` 只表示云端已接收命令，不表示电脑已执行；`started` 表示电脑已开始处理，`completed` 表示本次远程命令已有明确业务结果，不等于任务已经验收完成。手机通过 `GET /v1/commands/{commandId}` 或事件流查询最终结果；在线状态在提交和执行之间发生变化时，仍以命令结果为准。

### 7.2 Agent 通道

```text
WSS /v1/agent/connect
```

命令至少包含：

```json
{
  "commandId": "cmd-123",
  "instanceId": "pc-001",
  "type": "task.dispatch",
  "taskId": "task-001",
  "idempotencyKey": "mobile-request-001",
  "createdAt": "2026-08-31T10:00:00Z"
}
```

命令状态包括 `queued`、`accepted`、`received`、`executing`、`completed`、`failed`、`expired`、`cancelled`、`indeterminate`。

远程命令采用“至少一次投递、业务结果幂等”语义：

- 云端先持久化命令，再尝试通过 Agent WSS 投递；
- Agent 在 `processed_commands` 中以 `received -> executing -> completed/failed/expired` 记录状态后执行；重启恢复时，超时的 `received` 或 `executing` 命令必须按命令类型决定重试、重新校验或标记 `indeterminate`；
- 重复收到相同 `commandId` 时只返回已记录结果，不创建新的 TaskRun；结果未知时不得伪造成功或失败；
- 每条命令必须有 TTL，过期命令不得在 Agent 恢复连接后自动执行；
- 电脑离线时，`dispatch`、`stop`、`delete`、`review` 等危险操作默认返回 `instance_offline`，不排队执行；在线检查只是提交时检查，不能替代 Agent 执行时的状态校验；
- 手机可以保存本地草稿，但必须在电脑在线后由用户显式提交。

`Idempotency-Key` 的唯一范围至少为“用户 + 实例 + 操作类型”，并设置有效期；相同幂等键和不同请求体必须返回冲突，不能复用旧结果。Redis 只用于在线连接路由和广播，命令以 PostgreSQL 记录为准；Agent Gateway 重启或投递失败后必须由重试 worker 根据命令状态继续投递或过期处理。

### 7.3 统一事件

```json
{
  "eventId": "event-123",
  "agentSequence": 10086,
  "type": "task.status_changed",
  "instanceId": "pc-001",
  "projectId": "project-001",
  "taskId": "task-001",
  "createdAt": "2026-08-31T10:01:00Z",
  "payload": {}
}
```

手机使用统一事件流：

```text
WSS /v1/events
```

Agent 为每个 `instanceId` 在本地事务中分配持久化的 `agentSequence`，事件同时携带 `agentEventId`。云端对 `(instanceId, agentSequence)` 和 `(instanceId, agentEventId)` 建唯一约束，检测重复和序号 gap，不按网络到达顺序重排业务事件。云端可以另设内部数据库 ID，但不能用它替代 Agent 业务顺序。断线重连时手机携带每台电脑最后消费的 Agent 序号；云端根据游标补发，无法补发时发送带 `snapshotRevision` 的快照，手机重新收敛。

状态变更、TaskEvent 和本地 Outbox 写入必须在同一个 SQLite 事务中完成。Outbox 上传采用异步重试，不得阻塞本地任务执行；云端事件接收必须可重复提交，不能因为网络重试生成重复通知或重复任务结果。

Agent 批量上传 Outbox 后，云端返回明确 ACK（至少包含 `acceptedThrough`、重复事件列表和 `expectedNextSequence`）。发现序号 gap 时优先请求缺失事件重传；仅当事件已超出保留期或无法补发时才要求全量快照。上传失败使用指数退避和抖动，超过重试次数或本地容量上限后标记 `sync_error` 并告警。

## 8. 任务和进度语义

继续复用当前任务状态机：

```text
todo / action_required
    -> running
    -> awaiting_review
    -> done

running
    -> action_required
```

`failed`、`stopped`、`interrupted` 不是 Task 状态，而是 TaskRun 的终态；TaskRun 进入这些终态后，所属 Task 统一进入 `action_required`。Task 的取消状态使用现有 `cancelled` 语义，不能由移动端自行增加新的状态值。

第一版不伪造百分比进度，移动端显示：当前状态、已运行时长、当前 Agent、最近事件、最后一段输出、失败原因和是否可以验收。

以后可以让 Agent 上报阶段，例如“分析需求、修改代码、执行测试、等待验收”，再增加阶段进度。

任务状态与执行状态必须分开处理。当前代码的映射为：`Run completed -> TaskRun succeeded -> Task awaiting_review`；`Run failed/stopped/interrupted -> 对应 TaskRun 终态 -> Task action_required`。远程命令的 `completed` 仅表示命令处理完成，不能表示任务已验收。

## 9. 数据模型

### 9.1 云端表

```text
users
user_sessions
mobile_devices
desktop_instances
instance_members
pairing_requests
device_credentials
remote_commands
remote_events
event_cursors
push_subscriptions
audit_events
```

电脑本地还需要增加：

```text
remote_outbox
processed_commands
agent_state
```

`remote_outbox` 保存待上传事件、Agent 序号、重试次数、下次重试时间和上传状态；云端成功 ACK 后才允许删除，长期失败达到容量上限时进入 `sync_error` 并告警。Outbox 上传失败不能阻塞本地任务执行。`processed_commands` 保存命令状态、结果引用、过期时间和最后更新时间；`agent_state` 保存实例身份、凭证版本、最后上传序号和协议版本。`push_subscriptions` 按 `webpush`、`apns`、`fcm` 分别保存订阅或设备令牌，不能共用令牌字段。

### 9.2 数据归属

云端保存账号、设备、实例、任务摘要、命令、事件和审计数据。项目源码、AI 凭据、SSH 私钥和完整工作目录继续保存在电脑本地，默认不上传云端。项目或任务删除必须通过带 `deletedAt` 的 tombstone 和 `snapshotRevision` 同步，云端镜像可由电脑重新发送全量快照重建，不能把镜像当作事实来源。

运行日志默认只同步摘要；完整日志或 Artifact 必须由用户明确开启，并设置大小限制、保留期限和删除能力。

## 10. 安全要求

必须满足以下要求：

1. 全部公网通信使用 HTTPS/WSS。
2. Agent 使用 mTLS 或设备签名认证。
3. 手机令牌可撤销、可轮换、可过期。
4. 配对码一次性且需要电脑端确认。
5. 写操作进行服务端状态检查，不能信任手机传来的最终状态。
6. 下发、停止、验收等操作写入审计日志。
7. 所有命令支持 `Idempotency-Key`，避免重复执行。
8. API 按用户、实例、设备和 IP 限流。
9. 不提供通用 Shell、通用代理或任意文件读取接口。
10. API Key、SSH 私钥和环境变量不进入手机和云端。

补充要求：

- Agent 私钥使用 Windows DPAPI 或系统安全存储保护；
- Agent 证书设置短期有效期并支持轮换，撤销状态在每次连接和敏感命令前检查；
- 二维码只包含短时一次性配对句柄，不包含长期 Token 或私钥；其中携带的 6 位校验码同样是一次性短时凭据（5 分钟、单次有效），且**令牌必须经电脑端点击「确认绑定」才被激活**（未激活的令牌一律鉴权失败），因此二维码本身不是可用的授权凭据；
- 手机 Access Token 短期有效，Refresh Token 轮换并检测重复使用；
- 日志和事件采用字段白名单，脱敏 Token、Key、Cookie、Authorization、环境变量和疑似密钥；
- 所有实例、项目和任务端点都校验用户成员关系，防止跨实例越权；
  - 远程审批默认只展示“需要在电脑端处理”，移动端可接收 `approval.pending` 状态但不能批准；若未来允许远程批准 Shell 命令，必须使用独立高风险权限、二次确认、超时、撤销和审计。

禁止将现有未认证 Web 模式直接绑定公网地址，也禁止通过路由器端口映射暴露本地 control-server。

## 11. 移动端技术路线

建议分阶段：

1. 复用现有 React + TypeScript，增加移动优先布局；
2. 增加 PWA manifest、Service Worker 和离线只读缓存；
3. PWA 使用标准 Web Push + VAPID；
4. 使用 Capacitor 打包 Android/iOS 后，原生版本另行接入 FCM/APNs；
5. 后续再评估 React Native/Expo。

PWA 适合快速验证任务闭环，但 iOS 推送要求用户将 Web App 加入主屏幕并明确授权。PWA 与 Capacitor 是两套推送注册和令牌管理流程，不能共用同一套设备令牌。Tauri Mobile 不作为首选，因为手机端不运行 Go sidecar，且需要重新处理移动端原生能力和发布链路。

## 12. 部署方案

现有 `infrastructure/compose.yaml` 是开发基础设施骨架，不直接作为生产配置。单机生产部署应另建生产 Compose 或编排配置：

```text
Caddy/Nginx
  -> milevia-cloud
  -> PostgreSQL
  -> Redis
  -> MinIO/S3
```

必须配置 HTTPS 证书自动续期、PostgreSQL 定期备份和恢复演练、Redis 密码和网络隔离、管理端与 Agent 入口隔离、容器非 root 运行、结构化日志、健康检查和故障告警。

生产配置还必须包含密钥注入、备份加密、异地备份、恢复演练、WebSocket 代理超时设置、数据库迁移锁和最小网络暴露面。PostgreSQL、Redis、MinIO 不应暴露到公网；示例密码只能存在于开发环境。

后续需要多实例扩容时，Agent Gateway 使用 Redis 维护连接归属，API Server 使用共享数据库，避免把连接状态只放在单进程内存中。

## 13. 实施阶段

### 阶段一：云端基础设施

- PostgreSQL/Redis 初始化；
- 用户登录和会话；
- 电脑实例和设备模型；
- 二维码配对和设备撤销；
- Agent WSS Gateway。

### 阶段二：只读远程监控

- 项目列表；
- 项目在线状态；
- 当前运行状态；
- 任务列表和详情；
- 事件同步和断线恢复；
- 移动端总览接口。

### 阶段三：任务控制

- 创建、编辑、删除任务；
- 任务依赖；
- 下发、停止、重新打开；
- 验收和要求修改；
- 幂等控制和审计日志。

### 阶段四：移动 App

- 移动优先 PWA；
- Android/iOS 安装；
- 推送通知；
- 深度链接；
- 生物识别解锁；
- 多台电脑切换。

### 阶段五：生产化

- Agent 自动升级；
- 事件保留和归档；
- 备份恢复；
- 指标和告警；
- 多账号、成员和角色权限。

## 14. MVP 验收标准

- 在用户会话仍可用且远程后台模式运行时，关闭 Milevia 主窗口后后台 Agent 仍在线；
- 手机扫码后可以看到该电脑的项目；
- 手机可以看到项目运行状态和任务状态；
- 手机创建任务后，电脑端立即可见；
- 手机下发任务后，电脑开始执行；
- 手机可以看到状态变化和失败原因；
- 手机断网重连后，使用事件游标或快照收敛，状态不会错乱；
- 重复点击下发不会创建重复 TaskRun；
- 未配对设备无法访问；
- 用户可以撤销手机访问权限；
- 云端不保存项目源码和 AI 私钥；
- 手机端不能执行任意 Shell。

## 15. 调研依据

- Microsoft Learn：Windows 服务不能直接与用户桌面交互，推荐服务与用户会话程序通过受控 IPC 配合：<https://learn.microsoft.com/en-us/windows/win32/services/interactive-services>
- OWASP REST Security Cheat Sheet：非公开 REST 服务必须使用 HTTPS，并对每个端点执行访问控制：<https://cheatsheetseries.owasp.org/cheatsheets/REST_Security_Cheat_Sheet.html>
- OWASP MASVS：移动端安全存储、认证、网络和隐私的安全验证基线：<https://mas.owasp.org/MASVS/>
- RFC 6455：WebSocket Ping/Pong 只提供连接保活，不提供业务事件恢复：<https://www.rfc-editor.org/rfc/rfc6455>
- WebKit：iOS/iPadOS 16.4 支持加入主屏幕的 Web App 使用 Web Push：<https://webkit.org/blog/13966/webkit-features-in-safari-16-4/>
- Capacitor：原生 Android/iOS 推送需要分别配置 FCM/APNs：<https://capacitorjs.com/docs/guides/push-notifications-firebase>

## 16. 后续开发原则

云端负责身份、连接、路由和同步，电脑负责执行，手机负责查看和决策。移动端新增能力都应优先复用现有任务服务和状态机，避免在云端、桌面端和手机端分别维护不同的任务事实来源。
