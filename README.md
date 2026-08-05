# Milevia

Milevia 用于在一个网页中加载和管理 WSL 本地项目，并通过 Claude Code 进行连续的开发对话。

当前版本聚焦于一个可用闭环：选择项目目录、加载项目、创建 Claude Code 会话、下发提示词、查看消息和命令过程、确认命令权限，以及停止正在执行的任务。

## 当前能力

- 浏览并加载 WSL 本地目录，Git 仓库不是必需条件。
- 一个项目维护 Claude Code 会话；后续消息通过 Claude `session-id` 恢复上下文。
- 实时展示 Claude 消息、工具调用、命令输出、审批请求和运行结果；Claude 执行期间仍可继续发送提示词或快捷标签。
- 两种权限模式：默认模式下 Bash 命令需在网页确认；完全控制模式下 Claude 可直接执行命令。
- 支持停止任务、创建新会话、输入历史和 Markdown 对话展示。
- 支持通过“历史”或输入 `/resume` 搜索并恢复同一项目的历史 Claude 会话。
- 项目、会话、消息、运行和事件保存在本地 SQLite 数据库。

## 技术组成

- 前端：React、TypeScript、Vite
- 控制服务：Go、Chi、WebSocket
- 本地数据：SQLite
- AI 运行时：Claude Code CLI 的持久 `stream-json` 输入/输出会话

## Web 端前置条件

请在 WSL 环境中准备：

- Node.js 22 或更高版本
- pnpm 10 或更高版本
- Go 1.26 或更高版本，以及可用的 C/C++ 编译环境（SQLite 驱动需要 CGO）
- 已安装并登录的 Claude Code CLI
- `lsof` 或 `fuser` 命令，用于开发启动脚本清理占用端口

可先验证 Claude Code：

```bash
claude --version
claude auth status
```

安装前端依赖：

```bash
pnpm install
```

## 启动

以下命令均从仓库根目录执行。

使用一个命令启动前后端：

```bash
pnpm dev
```

根目录的 `dev.sh` 会先清理默认端口 `8080` 和 `5173` 上的监听进程，再启动控制服务和网页。按 `Ctrl+C` 会同时停止两个服务。

默认打开：`http://127.0.0.1:5173/`

### Windows 桌面端

桌面端使用 Tauri 2 承载现有 Web 页面，业务 API、页面结构和样式与 Web 端共用。桌面端启动时会自动拉起本地 Go control-server sidecar，并为本次启动生成独立会话令牌；Web 端仍可继续单独运行。

桌面端必须在 Windows 10/11 上构建和运行。请先准备：

- Node.js 22 或更高版本、pnpm 10 或更高版本
- Go 1.26 或更高版本，并配置可用的 CGO/SQLite 编译工具链
- 已安装并登录的 Claude Code CLI；如使用 Codex，也需准备 Codex CLI
- WebView2 Runtime（Windows 11 通常已预装）

首次安装依赖后，在仓库根目录启动桌面开发模式：

```powershell
pnpm install
pnpm --filter @milevia/desktop dev
```

桌面端构建脚本会在 Windows 上重新编译 `milevia-control.exe` 和 `milevia-approval.exe`，然后启动 Tauri 窗口。正式生成 NSIS/MSI 安装包：

```powershell
pnpm --filter @milevia/desktop build
```

Linux/WSL 环境不能直接生成可用的 Windows 安装包；应使用 Windows 实机或 Windows CI 完成最终构建和安装验证。

### 自定义端口

端口已被其他项目使用时，可以在启动时指定新端口。脚本会清理指定端口上的监听进程：

```bash
AUTO_CONTROL_PORT=8081 AUTO_WEB_PORT=5174 pnpm dev
```

打开：`http://127.0.0.1:5174/`

## 使用方式

1. 点击“加载项目”，在允许范围内浏览并选择 WSL 项目目录。
2. 点击“校验目录”，确认 Claude Code 可用后加载项目。
3. 在项目对话页选择默认权限或完全控制，输入开发要求后发送；执行期间可继续发送后续要求，它们会按提交顺序进入同一 Claude 会话。
4. 在时间线查看 Claude 的回复、命令和输出；默认权限下可允许或拒绝 Bash 命令。
5. 任务执行期间可点击“停止任务”。任务结束后，可继续当前会话或创建新会话。

## 配置项

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTO_HTTP_ADDR` | `127.0.0.1:8080` | 控制服务监听地址。 |
| `AUTO_CONTROL_URL` | `http://127.0.0.1:8080` | Claude 审批 Hook 回调的控制服务地址，应与实际端口一致。 |
| `AUTO_CONTROL_PORT` | `8080` | `pnpm dev` 使用的控制服务端口。 |
| `AUTO_WEB_PORT` | `5173` | `pnpm dev` 使用的网页端口。 |
| `AUTO_ALLOWED_ROOT` | 当前 WSL 用户的主目录 | 网页可浏览和加载项目的根目录。 |
| `AUTO_DATABASE_PATH` | `../../data/auto.db` | 从 `apps/control-server` 启动时使用的 SQLite 路径。建议在自定义部署时传入绝对路径。 |
| `AUTO_CLAUDE_PERMISSION_MODE` | `acceptEdits` | Claude 默认权限基础模式，可选 `acceptEdits` 或 `plan`。 |
| `AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT` | `5m` | 提示词发出后等待首个 Claude 模型响应的超时阈值。 |
| `AUTO_CLAUDE_TOOL_RESULT_TIMEOUT` | `5m` | Claude 收到工具结果后等待下一条模型响应的超时阈值，用于识别上游流未续传。 |
| `AUTO_CLAUDE_TURN_IDLE_TIMEOUT` | `30m` | Claude 工具执行中或普通流活动阶段的超时阈值。接受 Go 时长格式，如 `45m`、`1h`；超时后会停止会话并释放队列。 |
| `AUTO_APPROVAL_HOOK` | `../../scripts/claude-approval-hook.sh` | 默认权限下的 Bash 命令审批 Hook。 |
| `VITE_CONTROL_URL` | `http://127.0.0.1:8080` | Vite 代理目标地址，仅在启动网页时设置。 |

## 验证

后端测试与静态检查：

```bash
cd apps/control-server
go test -race ./...
go vet ./...
go build ./cmd/control-server
```

前端构建：

```bash
pnpm --filter @milevia/web build
```

## 文档

- [项目方向](docs/01-项目方向.md)
- [架构设计](docs/02-架构设计.md)
- [环境搭建和目录结构](docs/03-环境搭建和目录结构.md)
- [项目加载与 Claude 对话闭环](docs/04-项目加载与Claude对话闭环.md)
- [Claude 对话上下文与使用状态](docs/05-Claude对话上下文与使用状态.md)
- [快捷任务与命令系统](docs/06-快捷任务与命令系统.md)
- [任务编排与人工下发](docs/07-任务编排与人工下发.md)
- [Git 仓库管理与协作](docs/08-Git仓库管理与协作.md)
