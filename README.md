# Milevia

<p align="center">
  <strong>多项目 AI 自动开发平台</strong><br>
  <em>统一管理本地与远程项目，通过 Claude Code / Codex CLI 进行持续 AI 开发对话</em>
</p>

---

Milevia 是一个多项目 AI 开发工作台，支持 Web 浏览器和 Windows 桌面双端使用。它聚合管理你的本地项目（Windows、WSL、SSH 远程），为每个项目维护独立的 AI 会话，实时展示 AI 的消息、工具调用、命令执行和代码变更过程，并提供文件浏览编辑、Git 工作台、任务编排等完整开发工具链。

## 核心能力

- **多项目管理** — 同时加载和监控 Windows 本地、WSL 发行版、SSH 远程服务器上的项目，仪表盘实时显示运行状态
- **AI 对话闭环** — 每个项目维护独立的 Claude Code / Codex 会话，支持会话恢复、历史搜索和多轮连续对话
- **实时过程展示** — 时间线界面完整呈现 AI 消息、工具调用、命令输出和审批请求，执行期间可继续发送提示词
- **双权限模式** — 默认模式需人工确认 Bash 命令才可执行；完全控制模式允许 AI 直接执行
- **任务编排** — 可视化任务看板，支持创建、下发、跟踪开发任务，串联 AI 对话与代码变更
- **定时任务** — 按单次、每日或每周计划自动执行提示词，可选择时区、Agent、权限与已安装 Skill；仅在 Milevia 运行时触发，详见 [定时任务说明](docs/27-定时任务使用与实现说明.md)
- **文件编辑** — 内置代码编辑器（CodeMirror，支持 JS/TS/Python/Go/CSS/HTML/JSON 语法高亮），文件树浏览
- **Git 工作台** — 项目内查看变更、暂存、提交，与 AI 修改无缝衔接
- **跨项目通知** — 统一通知中心，聚合所有项目的运行状态和事件
- **Windows 桌面应用** — 基于 Tauri 2 的原生桌面壳，系统托盘驻留，单实例管理，自动拉起本地 Go 控制服务
- **会话安全** — 桌面端每次启动生成独立会话令牌，WebView 与 sidecar 之间通过随机端口 + 令牌通信

## 架构概览

```
Milevia.exe (Tauri 2 桌面壳)
  ├── WebView — React / TypeScript 前端 (apps/web)
  │     └── @milevia/sdk — 平台适配层 (packages/sdk)
  │
  └── milevia-control.exe (Go sidecar，绑定 127.0.0.1 随机端口)
        ├── SQLite — 项目、会话、消息、运行记录
        ├── HTTP API — REST + WebSocket 实时推送
        ├── Windows Runner — 本地 C:\ D:\ 项目
        ├── WSL Runner — 各 WSL 发行版独立环境
        └── SSH Runner — 远程服务器项目

Milevia 网页版 (浏览器直接访问)
  └── React / TypeScript 前端 ← HTTP/WSS → Go 控制服务
```

**关键设计决策：**
- 前端代码不分叉 — Web 端和桌面端共用同一套 React 代码，`@milevia/sdk` 在运行时自动适配平台
- 桌面端不依赖 WSL — Windows 本地项目可直接使用 Windows Git / Claude Code / Codex
- Go 控制服务作为 sidecar 进程 — 由 Tauri 管理层负责启动、健康检查和优雅关闭

## 技术栈

| 层级 | 技术 |
|------|------|
| 前端 | React 19、TypeScript 5.9、Vite 8、React Router 7、CodeMirror 6 |
| 控制服务 | Go 1.26、Chi 路由、Gorilla WebSocket、mattn/go-sqlite3 (CGO) |
| 桌面壳 | Tauri 2.11 (Rust)、WebView2 |
| 数据 | SQLite（本地文件存储） |
| AI 运行时 | Claude Code CLI（持久 `stream-json` 会话）、Codex CLI |
| 测试 | Go 标准测试框架、Playwright (E2E) |

## 环境要求

### Web 端 (WSL / Linux)

- Node.js ≥ 22
- pnpm ≥ 10.17
- Go ≥ 1.26（需 CGO 支持，SQLite 驱动依赖）
- C/C++ 编译工具链（GCC）
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) 已安装并登录
- `lsof` 或 `fuser`（端口清理用）

### Windows 桌面端

- Windows 10/11
- Node.js ≥ 22、pnpm ≥ 10.17
- Go ≥ 1.26 + CGO/SQLite 编译工具链（如 MinGW-w64）
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) 已安装并登录
- WebView2 Runtime（Windows 11 已预装；Windows 10 需手动安装）
- 可选：Codex CLI，如需使用 Codex 运行时

## 快速开始

### Web 端

```bash
# 1. 克隆仓库
git clone https://github.com/kegehe/milevia.git
cd milevia

# 2. 安装依赖
pnpm install

# 3. 一键启动（控制服务 + 前端 dev server）
pnpm dev
```

浏览器打开 `http://127.0.0.1:5173/`。

按 `Ctrl+C` 同时停止两个服务。

### Windows 桌面端（开发模式）

```powershell
# 确保已安装 Node.js、pnpm、Go + CGO 工具链

# 1. 安装依赖
pnpm install

# 2. 启动桌面开发模式（自动编译 sidecar + 启动 Tauri dev）
pnpm --filter @milevia/desktop dev
```

### 构建桌面安装包

```powershell
pnpm --filter @milevia/desktop build
```

> **注意：** 桌面端必须在 Windows 实机上构建。Linux/WSL 环境无法生成可用的 Windows 安装包。

### 自定义端口

```bash
# 端口被占用时可指定自定义端口
AUTO_CONTROL_PORT=8081 AUTO_WEB_PORT=5174 pnpm dev

# 仅在确认占用端口的进程可以停止时，显式清理它们
AUTO_CLEAR_PORTS=1 pnpm dev
```

## 使用指南

1. **加载项目** — 点击 "加载项目"，浏览并选择项目目录（Windows 本地、WSL 或 SSH 远程）
2. **校验并加载** — 点击 "校验目录" 确认 AI 工具可用后加载项目
3. **创建对话** — 进入项目，选择默认权限或完全控制模式，输入开发需求并发给 AI
4. **查看过程** — 在时间线中查看 AI 的回复、命令和输出；默认权限下可允许或拒绝 Bash 命令
5. **管理任务** — 在任务看板中创建和跟踪开发任务，查看对应代码变更
6. **设置定时任务** — 在项目页打开“任务 → 定时任务”，设置提示词、执行时间、时区和权限；Milevia 退出期间不会执行或补跑，详见 [定时任务说明](docs/27-定时任务使用与实现说明.md)
7. **编辑文件** — 在文件面板浏览项目文件树，使用内置编辑器修改代码
8. **Git 操作** — 在 Git 工作台查看变更、暂存提交
9. **会话管理** — 任务执行期间可 "停止任务"；任务结束后可继续当前会话或创建新会话

## 配置项

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `AUTO_HTTP_ADDR` | `127.0.0.1:8080` | 控制服务监听地址 |
| `AUTO_CONTROL_URL` | `http://127.0.0.1:8080` | 审批 Hook 回调地址 |
| `AUTO_CONTROL_PORT` | `8080` | `pnpm dev` 使用的控制服务端口 |
| `AUTO_WEB_PORT` | `5173` | `pnpm dev` 使用的网页端口 |
| `AUTO_CLEAR_PORTS` | `0` | 设为 `1` 时允许 `pnpm dev` 停止占用开发端口的进程 |
| `AUTO_STARTUP_TIMEOUT` | `60` | `pnpm dev` 等待控制服务健康检查通过的最长秒数 |
| `AUTO_ALLOWED_ROOT` | 当前用户主目录 | 可浏览加载项目的根目录 |
| `AUTO_DATABASE_PATH` | `../../data/auto.db` | SQLite 数据库路径 |
| `AUTO_CLAUDE_PERMISSION_MODE` | `acceptEdits` | Claude 默认权限模式 (`acceptEdits` / `plan`) |
| `AUTO_AGENT_UPDATE_TIMEOUT` | `15m` | AI CLI 更新最长等待时间；更新期间不会提前终止 npm 安装 |
| `AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT` | `5m` | 等待首个 Claude 模型响应的超时 |
| `AUTO_CLAUDE_TOOL_RESULT_TIMEOUT` | `5m` | 等待 Claude 下一条响应（工具结果后）的超时 |
| `AUTO_CLAUDE_TURN_IDLE_TIMEOUT` | `30m` | Claude 空闲超时，超时后释放会话队列 |
| `AUTO_APPROVAL_HOOK` | `../../scripts/claude-approval-hook.sh` | 默认权限下的命令审批 Hook |
| `VITE_CONTROL_URL` | `http://127.0.0.1:8080` | Vite 代理目标地址 |

## 项目结构

```
milevia/
├── apps/
│   ├── web/                    # React 前端应用 (@milevia/web)
│   │   └── src/
│   │       ├── features/       # 功能模块：files, git, run, tasks
│   │       ├── pages/          # 页面组件
│   │       ├── components/     # 通用组件
│   │       ├── stores/         # 状态管理
│   │       └── lib/            # 工具、类型、API 封装
│   ├── control-server/         # Go 控制服务
│   │   ├── cmd/
│   │   │   ├── control-server/ # HTTP API + WebSocket + 任务编排
│   │   │   └── approval-helper/# 原生审批辅助程序 (Windows)
│   │   └── internal/app/       # 核心逻辑
│   └── desktop/                # Tauri 2 桌面应用 (@milevia/desktop)
│       ├── src-tauri/          # Rust 宿主 (窗口/托盘/sidecar 管理)
│       └── scripts/            # 构建脚本 (sidecar 增量编译)
├── packages/
│   └── sdk/                    # 平台运行时适配层 (@milevia/sdk)
├── docs/                       # 设计文档
├── infrastructure/             # Docker Compose（未来扩展用）
├── scripts/                    # Shell 脚本 (审批 Hook 等)
├── data/                       # SQLite 数据存储（运行时生成）
├── dev.sh                      # 开发启动脚本
└── pnpm-workspace.yaml
```

## 开发

### 后端测试

```bash
cd apps/control-server
go test -race ./...
go vet ./...
```

### 前端测试

```bash
pnpm --filter @milevia/web test
```

### 类型检查与构建

```bash
# 类型检查
pnpm --filter @milevia/web exec tsc --noEmit

# 生产构建
pnpm --filter @milevia/web build
```

### 桌面端开发

```powershell
# 启动桌面开发模式（增量编译 sidecar，仅源码变更时重编）
pnpm --filter @milevia/desktop dev

# 如需强制重新编译 sidecar（忽略缓存）：
node apps/desktop/scripts/build-sidecar.mjs --force
```

## 贡献

欢迎提交 Issue 和 Pull Request。

在提交 PR 之前，请确保：

1. 代码通过所有现有测试
2. 新增功能包含相应测试
3. Go 代码通过 `go vet` 检查
4. TypeScript 代码通过 `tsc --noEmit` 类型检查

## 许可证

[MIT](LICENSE) © 2026 Milevia

---

<p align="center">
  <sub>为多项目 AI 开发而生 · Built for multi-project AI development</sub>
</p>
