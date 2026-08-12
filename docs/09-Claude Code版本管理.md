# Claude Code 版本管理

> 日期：2026-07-22  
> 阶段目标：让平台用户能够在页面中查看 Runner 的 Claude Code 版本号与可用状态，主动检查更新并在确认后执行更新；为未来远程 Runner 的版本管理保留统一接口。

## 1. 结论

当前平台只在项目创建时检测一次 Claude Code 是否可用（`claudeReady` 布尔值），用户看不到具体的版本号，也无法知道是否有新版本可更新。随着 Claude Code 快速迭代（周级发版），Runner 版本可能落后，导致新参数不支持、行为差异或已知 bug 未修复。

建议首期落地三个能力：

| 能力 | 说明 | 交互 |
| --- | --- | --- |
| 版本状态展示 | 前端展示当前 Runner 的 Claude Code 版本号和可用状态 | 纯展示，runner 信息区 |
| 检查更新 | 查询最新版本，对比当前版本，提示是否有更新可用 | 用户主动触发 |
| 执行更新 | 用户在确认风险后触发更新，返回更新结果 | 二次确认弹窗 |

明确不做：

- 不自动更新 Claude Code（即使全局设置了 `autoUpdatesChannel`，平台不替用户决策）
- 不锁定特定版本（平台不维护最低版本要求，仅做提示）
- 不在平台内安装 Claude Code（安装属于 Runner 部署层面）
- 不跨多个 Runner 批量更新

## 2. 现状与缺口

### 2.1 当前已有的能力

- `Config.ClaudePath` 指定 claude 二进制路径（默认 `"claude"`，通过 `PATH` 查找）
- `AgentRunner.Ready()` 检测 claude 是否存在 + `claude auth status` 是否通过
- 项目创建时调用 `Ready()`，结果存入 `projects.claude_ready`（布尔值，静态快照）
- 前端 `/api/runners` 返回 runner 基本信息（id、name、environment、root）
- 前端项目卡片上根据 `claudeReady` 展示可用状态

### 2.2 缺口

- 没有版本号：`Ready()` 只返回 bool，不返回版本字符串
- 版本快照过期：`projects.claude_ready` 写入后不再刷新，与实际可用性可能不一致
- 没有更新通道：用户不知道是否有新版本，也无法触发更新
- Runner 接口不暴露版本信息：`/api/runners` 返回静态固定数据，不含版本信息
- 没有更新操作的并发保护：更新期间若有人同时发起 Claude 对话，可能因进程被替换而出错

### 2.3 Claude Code CLI 提供的版本管理能力

Claude Code CLI 已经提供三个相关命令：

```bash
claude --version          # 输出当前版本，如 "2.1.216 (Claude Code)"
claude update             # 检查更新，如有新版本则安装
claude install [target]   # 安装指定版本（stable/latest/具体版本号），--force 强制重装
```

`update` 命令的行为特点：
- 检查远端最新版本，与当前版本比对
- 如有新版本，自动下载并替换当前二进制
- 无新版本时正常退出
- 更新是就地替换二进制文件，更新期间不应有活跃的 Claude 进程

### 2.4 已知限制：自定义 API 端点下的更新

本项目使用自定义 API 端点（`ANTHROPIC_BASE_URL` 指向 `maas-coding-api`），不是标准的 Anthropic 官方 API 环境。在这种环境下：

- `claude update` 依赖 npm registry 查询最新版本，可能在受限网络环境下失败
- `claude --version` 不受影响，只读取本地二进制版本
- 最新版本查询可降级为 `npm view @anthropic-ai/claude-code version`，但同样需要网络可达
- 实际更新行为等价于 `npm install -g @anthropic-ai/claude-code@latest`

平台设计应以 `claude update` 为首选更新方式，但当其失败时，应向用户展示具体错误信息，不做静默降级。

## 3. 产品边界与术语

### 3.1 术语

| 术语 | 定义 |
| --- | --- |
| Runner | 执行 AI CLI 的运行时环境。当前为 WSL2 本地 Runner（`wsl-local`），未来可能是远程 Runner |
| Claude Code 版本 | `claude --version` 输出的版本字符串，如 `2.1.216 (Claude Code)` |
| 可用状态 | Claude Code 二进制是否存在 + 认证是否有效 |
| 更新检查 | 查询是否有比当前版本更新的版本可用 |
| 更新执行 | 调用 `claude update` 替换当前二进制的过程 |

### 3.2 用户能做什么

- 在 Runner 信息区域看到当前 Claude Code 版本和状态（"Claude Code 2.1.216 ✓" 或 "Claude Code 不可用"）
- 点击"检查更新"按钮，看到是否有新版本可用
- 当有更新时，点击"更新"按钮，在确认风险后触发更新
- 更新完成后看到最新版本号

### 3.3 用户不能做什么

- 不能选择更新到特定版本（始终更新到 latest）
- 不能回退版本
- 不能安装 Claude Code 到一个未安装的 Runner
- 不能让平台自动更新

## 4. 方案比较与推荐

### 4.1 方案 A：前端直接读取静态配置

在 control-server 启动时获取一次版本号，作为静态信息返回给前端。不支持运行时检查更新和触发更新。

- 优点：实现极简，无并发问题
- 缺点：版本可能过时，不能更新，离用户需求太远
- 结论：不采用

### 4.2 方案 B：后端实时命令 + REST API

每次前端请求版本信息时，control-server 实时执行 `claude --version` 获取版本。检查更新和执行更新也走单独的 REST 端点，后端执行对应 CLI 命令。

- 优点：版本信息始终准确，可支持全部操作，实现直接
- 缺点：频繁请求会有 CLI 调用开销；更新操作需要并发控制
- 结论：采用。版本信息的读取频率不高（Runner 信息区域不是每秒刷新），实时命令开销可接受。

### 4.3 方案 C：后端缓存 + 定时轮询

control-server 定期（如每小时）自动执行版本检测和更新检查，结果缓存在内存中，前端读取缓存。

- 优点：前端零延迟，不产生重复 CLI 调用
- 缺点：版本变化有延迟；引入定时器增加复杂度；用户主动刷新时不能即时反映最新状态
- 结论：不采用。Claude Code 版本变化本身不频繁，用户操作驱动的实时查询更直观。

### 4.4 推荐：方案 B，但版本信息启动时缓存 + 按需刷新

在方案 B 的基础上做一处优化：control-server 启动时执行一次版本检测并缓存，后续前端请求直接返回缓存。用户主动"检查更新"时实时执行 CLI 命令并刷新缓存。这样既保证日常展示零延迟，又保证更新操作后的数据准确。

## 5. 领域模型

### 5.1 AgentRunner 接口扩展

现有 `AgentRunner` 接口定义在 `claude_runner.go`：

```go
type AgentRunner interface {
    Ready(context.Context) bool
    Run(context.Context, AgentRunRequest, AgentRunSink) error
}
```

为支持版本管理，增加三个方法：

```go
type AgentRunner interface {
    Ready(context.Context) bool
    Run(context.Context, AgentRunRequest, AgentRunSink) error

    // Version 返回当前安装的 Claude Code 版本字符串。如未安装，返回空字符串。
    Version(context.Context) string

    // CheckUpdate 检查是否有可用更新。返回是否有更新、最新版本号。
    // 查询失败时返回 error。
    CheckUpdate(context.Context) (updateAvailable bool, latestVersion string, err error)

    // Update 执行更新。返回更新前版本和更新后版本。
    // 更新期间不应有活跃的 Claude 进程，调用方需先做门禁检查。
    Update(context.Context) (previousVersion, currentVersion string, err error)
}
```

`claudeCLIRunner` 作为唯一实现者，对应底层命令：

| 接口方法 | 底层命令 | 说明 |
| --- | --- | --- |
| `Version()` | `claude --version` | 解析输出，提取纯版本号 |
| `CheckUpdate()` | `npm view @anthropic-ai/claude-code version` | 对比本地版本与远端最新版本 |
| `Update()` | `claude update` | 直接调用 CLI 更新命令 |

> **更新：实际接口已超出本节描述**。代码中除 `AgentRunner` 外，还定义了两个相关接口（`claude_runner.go`）：
>
> ```go
> // StreamingAgentRunner 支持流式会话（StartSession），用于对话页实时过程
> type StreamingAgentRunner interface {
>     AgentRunner
>     StartSession(context.Context, AgentSessionRequest) (AgentSession, error)
> }
>
> // CodexCapableRunner 支持 Codex CLI 的版本管理（对称的四个方法）
> type CodexCapableRunner interface {
>     CodexReady(context.Context) bool
>     CodexVersion(context.Context) string
>     CodexCheckUpdate(context.Context) (bool, string, error)
>     CodexUpdate(context.Context) (string, string, error)
> }
> ```
>
> **Codex CLI 的版本管理已完整实现**（`codex_runner.go`），与本节 Claude 的三能力对称：`codex --version` / `npm view` / `codex update`，对应 API `POST /api/runners/{runnerId}/codex/check-update` 与 `POST /api/runners/{runnerId}/codex/update`。本节原仅描述 Claude，实际应理解为"Claude 与 Codex 共用同一套版本管理接口模式"。

### 5.2 RunnerInfo（扩展）

当前 `/api/runners` 返回静态列表。扩展为包含动态版本信息：

```jsonc
{
  "id": "wsl-local",
  "name": "WSL Local Runner",
  "environment": "wsl",
  "root": "/home/tangmaoke",
  // 以下为新增字段
  "claude": {
    "status": "ready",          // "ready" | "unavailable" | "needs_auth" | "updating"
    "version": "2.1.216"        // 版本号字符串，status 为 ready 时有效
  }
}
```

`status` 的取值与含义：

| 值 | 含义 | 条件 |
| --- | --- | --- |
| `ready` | 正常可用 | `claude` 在 PATH 中 + `claude auth status` 成功 |
| `unavailable` | 未安装或不可执行 | `claude` 不在 PATH 中 |
| `needs_auth` | 已安装但未认证 | `claude` 可执行但 `auth status` 失败 |
| `updating` | 正在更新中 | `claude update` 正在执行 |

`updateAvailable` 和 `latestVersion` 不放在 `RunnerInfo` 中，而是通过独立的 `check-update` API 按需获取。原因：
- 更新检查需要网络请求，不应在每次加载 Runner 信息时触发
- 未做过检查时这两个字段无意义
- 避免将"版本快照"和"更新状态"两类不同生命周期的数据混在一起

### 5.3 不与 project 绑定

版本信息属于 Runner 层级，不属于 project。当前 project 的 `claude_ready` 字段保持不变（用于项目创建时的快照），但前端 Runner 状态展示以 Runner 级实时信息为准。

## 6. 并发、更新与安全规则

### 6.1 更新前的门禁

执行更新前，后端必须检查：
1. Runner 上是否有正在运行的 Claude 对话（`conversations.status = 'running'`）
2. Runner 上是否有正在排队的 Run（`runs.status = 'queued'`）

如果有活跃的 Claude 进程，拒绝更新并提示用户先停止所有运行中的对话。

### 6.2 更新期间的隔离

更新操作使用互斥锁（内存级别），同一 Runner 同时只能有一个更新操作。更新执行期间，前端展示 `status: "updating"`，该 Runner 上的新对话创建请求应被拒绝。

### 6.3 更新失败处理

如果 `claude update` 执行失败（网络错误、权限不足等），后端记录错误信息并返回给前端。Runner 状态从 `updating` 恢复为更新前的状态。不保留中间状态。

### 6.4 更新后的恢复

更新成功后，刷新版本缓存，Runner 状态恢复为 `ready`。更新期间被阻塞的对话可以重新创建。不需要重启 control-server。

## 7. API 设计

### 7.1 GET `/api/runners`（扩展）

在现有响应基础上，为每个 runner 增加 `claude` 字段。

**响应**：

```jsonc
[
  {
    "id": "wsl-local",
    "name": "WSL Local Runner",
    "environment": "wsl",
    "root": "/home/tangmaoke",
    "claude": {
      "status": "ready",
      "version": "2.1.216"
    }
  }
]
```

`version` 字段取自 control-server 启动时的缓存。`status` 取值为 `"ready"` / `"unavailable"` / `"needs_auth"` / `"updating"`。不包含 `updateAvailable` 和 `latestVersion`（通过独立端点按需查询）。

### 7.2 POST `/api/runners/{runnerId}/claude/check-update`

检查是否有可用更新。后端调用 `CheckUpdate()` 接口方法，底层对比本地版本与远端最新版本。

**响应**（有更新）：

```jsonc
{
  "updateAvailable": true,
  "currentVersion": "2.1.216",
  "latestVersion": "2.1.217"
}
```

**响应**（无更新）：

```jsonc
{
  "updateAvailable": false,
  "currentVersion": "2.1.216",
  "latestVersion": "2.1.216"
}
```

**失败响应**（网络不可达等）：

```jsonc
{
  "updateAvailable": false,
  "currentVersion": "2.1.216",
  "error": "无法查询最新版本：npm registry 不可达"
}
```

此端点不触发更新，仅做版本比对。不会改变 Runner 状态。

### 7.3 POST `/api/runners/{runnerId}/claude/update`

执行 Claude Code 更新。

**前置条件**：
- Runner 上无运行中的对话

**成功响应**：

```jsonc
{
  "success": true,
  "previousVersion": "2.1.216",
  "currentVersion": "2.1.217"
}
```

**失败响应**：

```jsonc
{
  "success": false,
  "error": "更新过程中无法连接远端：..."
}
```

**冲突响应**（有运行中对话）：

```jsonc
{
  "success": false,
  "error": "有 2 个对话正在运行，请先停止所有对话后再更新",
  "activeConversations": 2
}
```

### 7.4 Codex CLI 版本管理 API（已实现）

Codex 与 Claude 对称，复用同一套模式：

- `POST /api/runners/{runnerId}/codex/check-update` — 检查 Codex CLI 更新
- `POST /api/runners/{runnerId}/codex/update` — 执行 Codex CLI 更新

请求/响应结构与 §7.2、§7.3 的 Claude 端点一致，底层命令为 `codex --version` / `npm view @openai/codex version` / `codex update`。

### 7.5 兼容性说明

以上 API 路径使用 `{runnerId}` 作为路径参数。当前 `runnerId` 已不限于 `wsl-local`，还包括 `windows-local` 和 `ssh-{id}`（见 `docs/11`、`docs/17`、`docs/20`）。各 Runner 通过 `CodexCapableRunner` 接口决定是否支持 Codex 版本管理；不支持的 Runner 对 Codex 端点返回不可用。

> 注：本节原提及"gRPC 接口"——当前无 gRPC，Runner 是进程内接口实现，见 `docs/02` 当前架构。

## 8. 前端交互

### 8.1 对话页面中的 Runner 状态栏

进入项目对话页面后，在对话框（composer）下方展示 Runner 状态栏，显示：

- 当前项目所关联 Runner 的 Claude Code 版本号 + 状态指示灯（绿色=ready，黄色=unavailable/needs_auth，闪烁=updating）
- 不同项目可能使用不同 Runner（WSL 本地、SSH 远程等），每个项目看到的 Runner 版本状态独立
- "检查更新" 按钮 — 仅在状态为 `ready` 时可用
- 有可用更新时显示新版本号 + "更新" 按钮

### 8.2 更新确认弹窗

### 8.2 更新确认弹窗

点击"更新"按钮时弹出确认弹窗：

- 标题：确认更新 Claude Code
- 内容：当前版本 → 最新版本，更新期间将无法使用 AI 对话功能，更新预计需要数十秒
- 按钮：取消 / 确认更新
- 如果存在运行中的对话，直接显示错误提示而不弹出确认弹窗

### 8.3 更新过程

更新执行期间：
- 版本区域显示 "更新中..." + 加载中指示器
- "检查更新"和"更新"按钮禁用
- 更新完成后自动刷新版本号并显示结果

## 9. 实施顺序

### 阶段 A：版本信息展示

- control-server 启动时获取并缓存版本号
- 扩展 `AgentRunner` 接口，增加 `Version()` 方法
- 扩展 `/api/runners` 响应，增加 `claude` 字段
- 前端展示版本号和状态

### 阶段 B：检查更新

- 扩展 `AgentRunner` 接口，增加 `CheckUpdate()` 方法
- 添加 `POST /api/runners/{runnerId}/claude/check-update` 端点
- 前端增加"检查更新"按钮和结果展示

### 阶段 C：执行更新

- 扩展 `AgentRunner` 接口，增加 `Update()` 方法
- 实现更新前的门禁检查（无运行中对话）
- 添加 `POST /api/runners/{runnerId}/claude/update` 端点
- 前端增加"更新"按钮和确认弹窗
- 更新过程中的状态保护

## 10. 验收条件

### 基础展示

- [ ] `/api/runners` 返回 `claude.status` 和 `claude.version`
- [ ] 对话页面 composer 下方展示 Runner 状态栏（版本号 + 状态指示灯）
- [ ] 不同项目按 `project.runner` 匹配对应 Runner 的版本状态
- [ ] Claude Code 未安装时显示 `unavailable` 状态
- [ ] Claude Code 未认证时显示 `needs_auth` 状态

### 检查更新

- [ ] 点击"检查更新"后能看到是否有新版本
- [ ] 无新版本时提示"已是最新版本"
- [ ] 检查更新不影响正在运行的对话

### 执行更新

- [ ] 点击"更新"后弹出确认弹窗
- [ ] 有运行中对话时拒绝更新并提示
- [ ] 更新成功后版本号自动刷新
- [ ] 更新失败后有明确错误提示
- [ ] 更新期间无法发起新的 AI 对话
