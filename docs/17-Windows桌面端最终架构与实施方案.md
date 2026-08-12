# Windows 桌面端最终架构与实施方案

> 日期：2026-08-05
>
> 目标：将 Milevia 发布为原生 Windows 桌面应用，同时保持当前 Web 页面、功能、数据模型和实时交互不分叉。
>
> 最终决策：采用 **Tauri 2 桌面壳 + 原生 Windows Go Control Server sidecar + 多环境 Runner**。React/Vite 前端继续是唯一的界面实现；桌面页面由 Tauri 使用稳定 Origin 托管，Go sidecar 仅提供随机端口的本机 API/实时服务；Windows、WSL 与 SSH 是同等的一类执行环境；Windows 桌面版不以 WSL 为必需依赖。

> 实施状态（2026-08-05）：桌面宿主、安全连接、原生审批 helper、Windows 本地 Runner 的基础文件/Git/CLI/项目运行路径已实现并在 Linux 开发环境完成单元和 sidecar 冒烟验证。多发行版 WSL Adapter、WebView2 实机协议验证、Windows SQLite/安装包端到端验证、代码签名与自动更新仍是发布前必需工作，不能视为已完成。

## 1. 决策摘要

### 1.1 采用方案

```text
Milevia.exe (Tauri 2)
  |
  +-- 使用稳定的 Tauri 页面 Origin 托管 apps/web/dist
  +-- 单实例、窗口/托盘生命周期、安装、升级
  +-- 向当前 WebView 提供最小运行时连接配置
  |
  v
milevia-control.exe (Go sidecar，绑定 127.0.0.1 随机端口)
  |
  +-- SQLite、HTTP API、WebSocket、任务编排、通知
  +-- Windows Local Runner: C:\\、D:\\，Windows Git / Claude Code / Codex
  +-- WSL Runner: 每个已配置 WSL 发行版各自独立
  +-- SSH Runner: 远程服务器项目
```

- 桌面窗口不加载随机 localhost 页面。Tauri 托管页面以保持固定 Origin，从而让 `localStorage`、草稿和界面偏好在应用重启后继续可用。
- Go 服务仅监听随机 localhost API/WS 端口。前端通过运行时地址层连接服务；普通 Web 模式仍使用相对 API 与同源 WebSocket。
- 生产运行时只保留一个 React 前端来源：`apps/web`；不得复制页面到 `apps/desktop`。
- 首发仅支持 Windows x64；ARM64 在构建、CLI 与测试条件成熟后再加入。

### 1.2 不采用方案

| 方案 | 不采用原因 |
| --- | --- |
| Electron | 能实现，但需要随应用分发 Chromium，包体和内存开销显著高于使用系统 WebView2 的 Tauri；本项目没有 Node 主进程特有需求。 |
| PWA | 无法可靠启动本地 Go 服务、AI CLI、Git、长驻进程和审批 Hook，不满足本产品核心能力。 |
| 仅套一层 Windows 壳并继续运行 WSL 服务 | 可作为过渡版本，但用户必须安装 WSL，Windows 项目存在 `/mnt` 跨文件系统性能和运行时差异，不是最终产品。 |
| Wails 直接替代 Control Server | 会引入新的 Go-JS RPC 边界，而当前 HTTP/WebSocket 服务已完整承载业务和实时通信；收益不足以覆盖重构成本。 |

## 2. 当前项目基础与缺口

### 2.1 可直接复用的部分

- `apps/web` 是 React + TypeScript + Vite 单页应用，当前 API 请求使用相对地址，实时事件使用 WebSocket。
- 页面、样式、文件编辑、Git 工作台、任务、项目运行、通知和 SSH 管理均已在浏览器标准能力内实现。
- Go Control Server 已包含 SQLite、项目/会话/任务/运行持久化、WebSocket、SSH Runner、文件系统、Git 和 Agent Runner 边界。
- Go 已有 Windows 进程树终止实现，可作为 Windows 本地运行管理的基础。

### 2.2 不能直接作为原生 Windows 版发布的部分

- 默认本地 Runner 为 `wsl-local`，目录根、项目路径和 CLI 配置以 WSL 环境为中心。
- 项目启动的 `wsl` 目标使用 `sh -c`；现有 `windows` 目标是由 WSL 将 `/mnt/<盘符>` 路径转换后调用 PowerShell，不适用于原生 Windows 服务。
- Claude 的审批 Hook 为 Shell 脚本，且 Runner 将其硬编码为 `sh <hook>`。
- 数据库默认相对当前工作目录存放，安装后不应继续依赖安装目录或启动目录。
- `mattn/go-sqlite3` 依赖 CGO。将 Go 服务打包为 Windows sidecar 时，构建链路需要 C/C++ 工具链，增加 CI 和本地发布复杂度。
- 服务当前只注册 API 与 WebSocket 路由，尚未区分桌面 API-only 模式和浏览器 Web 静态托管模式。

## 3. 最终职责边界

### 3.1 Tauri 桌面壳

Tauri 只承担桌面宿主职责，不承载业务逻辑，也不向页面暴露任意系统调用能力。

- 创建原生窗口、应用图标、菜单和单实例行为。
- 先启动 `milevia-control.exe`，读取其就绪地址并轮询 `/api/health`，再创建业务窗口。
- 使用 Tauri 资源协议托管 `apps/web/dist`，页面 Origin 固定；不得为了复用相对 API 而让窗口直接加载随机端口地址。
- 仅向当前业务窗口提供 `desktopRuntimeConfig()`，内容为 API 地址、WebSocket 地址和本次启动会话令牌；该能力不提供文件、Shell、进程或任意网络访问。
- 应用退出时通知 sidecar 优雅退出，超时后终止其进程树。
- sidecar 异常退出时显示错误状态并保留诊断日志。
- 禁止窗口导航至非应用资源或非受控外部链接；禁止打开不受控的新窗口。
- 不向页面开放文件系统、Shell 或进程执行接口。Tauri 不使用 Electron 的 Node.js 集成模型。

### 3.2 Go Control Server

Go 服务仍然是唯一的业务后端，并有两种明确的启动模式。

- 使用 `127.0.0.1:0` 监听，由操作系统分配端口；启动后输出结构化就绪行，例如 `MILEVIA_READY=http://127.0.0.1:43127`。
- `desktop-api` 模式只提供 API、WebSocket、健康检查和受会话令牌保护的退出端点。
- `web` 模式以 `--web-root` 托管 Vite `dist`，并对非 `/api`、非 `/ws` 的未知路径返回 `index.html`，继续支持浏览器 Web 端的 `BrowserRouter` 刷新和深链接。
- 以 `--data-dir` 指定用户数据目录，不从当前工作目录推导数据库、日志或配置位置。
- 服务启动前必须取得 `data-dir` 的独占进程锁（例如 `milevia.lock`），锁内容记录 PID、启动时间、API 地址和版本。已有健康服务持锁时，第二个启动请求只聚焦现有桌面实例，不得再次打开同一 SQLite 文件。
- `web` 模式首期仅指本机单用户浏览器入口。若要绑定局域网或公网地址，必须另行实现身份认证、TLS、角色权限、会话管理、CSRF 防护与审计；在此之前服务不得监听非 loopback 地址。
- 保持业务 HTTP API 和 WebSocket 事件契约不变；仅在前端服务层增加地址与认证适配，不创建第二套页面或业务服务层。
- 提供由 Tauri 调用的健康检查、版本查询、优雅退出与诊断信息。

### 3.3 Runner 模型

`Runner` 描述执行地点，`Agent` 描述 Claude Code 或 Codex，两者保持正交。项目绑定 Runner，Conversation 选择 Agent。

| Runner 类型 | 项目路径形式 | CLI 与命令执行 | 首发状态 |
| --- | --- | --- | --- |
| `windows-local` | `C:\\...`、`D:\\...` | Windows Git、Claude Code、Codex、PowerShell/cmd | 必须实现 |
| `wsl` | 对应发行版内 Linux 路径 | 对应发行版内 Git、Claude Code、Codex、Shell | 必须兼容，不再是前提 |
| `ssh-*` | 远程 Linux 路径 | 现有 SSH/SFTP 与远端 CLI | 继续复用 |

Windows 项目不得保存为 `/mnt/c/...` 伪路径；原生服务中保存 Windows 绝对路径。WSL 项目保存发行版 ID 与该发行版内的 Linux 路径，并由相应 WSL Runner 处理目录、Git、文件和长驻 Agent 会话。桌面端从 `wsl.exe --list --quiet` 发现发行版；一个默认发行版不是架构前提。

### 3.4 Runner 持久化与适配器

Runner 不能只以项目表中的字符串表达。新增 `runners` 表：

| 字段 | 说明 |
| --- | --- |
| `id` | 稳定 UUID，项目只引用此 ID。 |
| `kind` | `windows-local`、`wsl` 或 `ssh`。 |
| `display_name` | 面向用户的名称。 |
| `config_json` | 环境专属配置；WSL 保存发行版标识，SSH 保存连接引用。 |
| `enabled` / `last_error` | 当前可用性与最近诊断。 |

迁移期间，已有 `projects.runner` 值映射到对应 Runner 记录；迁移完成后项目保存 `runner_id`。Runner 注册表从数据库恢复，而不是仅在内存中构造 `wsl-local`。

`wslRunner` 是独立 Adapter，不是将 Linux 路径交给 Windows 本地实现：

- Agent、Git 和项目运行通过 `wsl.exe --distribution <发行版> --cd <Linux 路径> --exec ...` 执行，所有参数使用结构化参数传递。
- 文件 Adapter 维护 Linux 路径与 Windows 可访问 UNC 路径的映射，仅将 UNC 用作文件传输层；不把 UNC 路径持久化为项目路径。
- `wslRunner` 分别实现 Agent、文件系统、Git 与项目运行接口，并统一返回现有 API 所需的事件、日志和错误语义。
- 长驻 Claude stdin/stdout、停止传播、UNC 映射、发行版离线和恢复必须先通过 Windows POC，未通过则不作为首发承诺。

## 4. 前端复用策略

### 4.1 单一前端来源

`apps/web` 保持为唯一前端工程，现有页面、CSS、组件、测试和 Vite 配置继续在该目录维护。

生产构建流程：

```text
pnpm --filter @milevia/web build
  -> apps/web/dist
  -> 作为桌面安装包 resource 随 Tauri 分发（桌面模式）
  -> Tauri 稳定页面 Origin 加载资源
  -> 调用 desktopRuntimeConfig() 获取本机 sidecar 连接配置
  -> 或由 Go 服务以 --web-root 托管（浏览器 Web 模式）
```

前端新增唯一的运行时服务地址层：Web 模式默认使用当前 Origin；桌面模式读取 `apiBase`、`wsBase` 与会话令牌。所有 HTTP 请求统一经 `api()` 封装，所有 WebSocket 统一经同一个连接工厂创建，避免页面自行拼接 `location.host`。页面的 `localStorage`、`sessionStorage`、剪贴板、通知权限和响应式 CSS 继续由 WebView2 支持，且稳定页面 Origin 保证已有本地偏好不会因随机 API 端口变化而丢失。

### 4.2 开发模式

- 前端继续使用 Vite 开发服务器和热更新。
- 桌面开发窗口可加载 Vite 地址，Go 服务继续由开发脚本启动。
- 生产模式必须验证由 Go 服务托管 `dist` 的路径、刷新、深链接、资源缓存和 WebSocket。

## 5. 原生 Windows 后端改造

### 5.1 配置、数据与路径

桌面端启动器显式传入以下参数或等价环境变量：

| 项目 | Windows 最终位置/行为 |
| --- | --- |
| 用户数据根目录 | `%LOCALAPPDATA%\\Milevia` |
| SQLite | `%LOCALAPPDATA%\\Milevia\\data\\milevia.db` |
| 日志 | `%LOCALAPPDATA%\\Milevia\\logs` |
| 配置与 SSH 元数据 | `%LOCALAPPDATA%\\Milevia\\config` |
| 安装资源 | Tauri resource 目录，只读，不写入用户数据 |
| 允许浏览根目录 | 用户目录与经用户选择/授权的磁盘根目录 |

项目、文件、Git 和运行配置必须使用 `filepath` 与平台本地路径。界面只显示后端提供的 `pathDisplay`，不自行假定斜杠格式。

### 5.2 SQLite 发布决策门

Windows 打包前必须完成 SQLite 发布 POC，再决定驱动；不得在未验证前直接迁移生产数据库驱动。

- 优先验证纯 Go SQLite 驱动，以减少 Windows 发布对 CGO、MinGW/MSVC/SQLite C 编译环境的依赖。
- 若纯 Go 驱动在 WAL、外键、`busy_timeout`、并发写入、已有数据库升级或大事件流测试中不达标，则保留当前驱动并在 Windows CI 固定可复现的 C 编译链路。
- 两条路径都必须验证已有 SQLite 文件可升级、可回滚且不会破坏迁移 SQL。
- 不将数据库嵌入安装包，不在卸载时默认删除用户数据。

### 5.3 Agent CLI 与审批

- Claude Code 与 Codex 由用户在对应 Runner 环境安装、登录和升级；应用只检测版本和状态，不打包用户凭据。
- Windows Runner 使用 Windows 可执行路径与 `exec.Command` 直接启动 CLI，兼容 `.exe`、`.cmd` 与 `PATHEXT`。
- 用独立的 `milevia-approval.exe` 替代 `claude-approval-hook.sh`。该 helper 从标准输入读取 Hook 数据，通过一次性令牌向本机 Control Server 请求审批；不依赖 `sh`、`curl`、Git Bash 或 PowerShell 脚本拼接。
- WSL 和 SSH Runner 保持各自的 Linux/远端 Hook 适配，不让 Windows Hook 逻辑泄漏到其他 Runner。

### 5.4 项目运行

将当前 `auto`、`wsl`、`windows` 的项目运行目标替换为按 Runner 分派。此重构必须覆盖项目运行、文件系统、Git、任务编排、CLI 检测、更新和错误提示中对 `wsl-local` 的所有硬编码：

- `windows-local`：使用受控的 PowerShell 或 `cmd.exe` 启动项目命令，记录并终止完整子进程树。
- `wsl`：通过 Runner 配置中指定的发行版启动 Shell 命令，支持长驻进程与日志流。
- `ssh-*`：沿用远端执行语义。

命令必须作为参数或编码后的数据传递，不能把用户输入直接拼进 PowerShell 源码。停止、重启、退出码、日志截断和状态机必须在三个 Runner 上保持相同 API 语义。WSL 长驻 Claude stdin/stdout、停止传播和路径行为须在 POC 中验证后才进入正式实现。

### 5.5 命令解释器与迁移语义

> **实现说明**：本节原设计的 `shell` 字段（`cmd` / `powershell` / `wsl-shell`）**未落地**。实际采用更简单的 `execution_target` 方案（`auto` / `wsl` / `windows`，见 `docs/12` §3.2.1）。`project_runner.go` 的 `newProjectRunCommand()` 按 `RunExecutionTarget` 分派命令解释器：`windows` 目标在 Windows 上用 `cmd.exe /d /s /c`；`wsl` 目标用 `sh -c`；WSL 服务端跑 windows 目标用 PowerShell 桥接。下方保留原 `shell` 设计作参考。

项目运行配置新增 `shell` 字段，取值为 `cmd`、`powershell` 或 `wsl-shell`，由 Runner 校验可用值：

- `windows-local` 新项目默认 `cmd`，可显式选择 PowerShell；`envVars` 由后端以进程环境注入，不依赖 Shell 语法。
- `wsl` Runner 固定使用 `wsl-shell`。
- 不自动翻译既有命令。迁移发现旧 `auto/wsl/windows` 配置时，根据原项目 Runner 显式写入等价 Shell；无法可靠判定时要求用户确认后启动。

这样可避免 `FOO=bar pnpm dev`、`$env:FOO='bar'`、`set FOO=bar` 被误认为可跨 Shell 运行。

## 6. 本机服务安全

桌面端的本机 Control Server 拥有文件、Git、CLI 与 SSH 密钥访问能力，不能把 localhost 当作天然可信边界。

- 仅监听 `127.0.0.1`，不监听 `0.0.0.0`、局域网地址或 IPv6 全局地址。
- 每次启动生成随机会话密钥。HTTP 使用 `X-Milevia-Session`；浏览器 WebSocket 无法自定义 Header，使用经服务端校验的 `Sec-WebSocket-Protocol` 子协议令牌，不使用会被日志记录的 URL 查询参数。
- CORS 与 WebSocket Origin 必须精确匹配桌面页面 Origin，或在 Web 模式下精确匹配实际 Web 服务 Origin；不得接受任意 `localhost` 来源。
- Tauri 仅向自身业务窗口暴露运行时配置。外部页面既无法读取令牌，也不能通过 CORS 或 WebSocket Origin 校验访问本机服务。
- WebSocket 子协议认证必须在 Windows WebView2 POC 中验证服务端动态协商、重连、令牌编码和日志脱敏；未通过前不得把该协议视为发布完成。
- 仅由 Tauri 托管自身构建的桌面资源；外部导航、弹窗与下载须显式审核。
- 安装包中的 sidecar、前端资源与审批 helper 必须在发布链路中校验版本和完整性。
- SSH 私钥与凭据继续只保存在 Control Server 用户数据目录，禁止传入浏览器 JavaScript、日志或诊断包。

### 6.1 活跃运行与退出策略

- 有 AI 会话、任务编排或项目运行进程时，关闭窗口默认最小化到托盘；用户选择“退出并停止”后才结束 sidecar 与子进程树。
- 无活跃运行时，Tauri 调用受会话令牌保护的退出端点，等待 Go 服务关闭 SQLite、WebSocket、SSH 连接和子进程；超时才由宿主强制终止。
- 自动更新仅在无活跃运行时下载和安装。升级前创建 SQLite 备份，升级失败时保留可回滚的应用版本和数据。

## 7. 工程组织与构建

```text
apps/
  web/                         # 唯一的 React/Vite UI
  control-server/              # Go 业务服务与 Runner
  desktop/
    package.json               # Tauri 开发/打包命令
    src-tauri/                 # Rust 宿主、窗口和 sidecar 生命周期
    binaries/                  # 构建期放入目标三元组命名的 Go sidecar
    scripts/                   # 构建 web、Go sidecar、组装资源
```

发布流程：

1. 构建并测试 `apps/web/dist`。
2. 构建 Windows x64 的 `milevia-control.exe` 与 `milevia-approval.exe`。
3. 将 Web 构建产物、Go 二进制与必要资源纳入 Tauri bundle。
4. 生成 NSIS 安装包和 MSI，检测或引导安装 WebView2 Evergreen Runtime。
5. 在对外发布前完成 Windows 代码签名；自动更新必须具备签名校验、活跃运行拦截和回滚策略。
6. 在干净的 Windows 虚拟机执行安装、首次启动、升级、卸载和数据保留验证。

## 8. 实施阶段

### 阶段 A：共享边界重构与 POC

- 前端建立运行时服务地址层，收敛现有 API 与全部 WebSocket 创建点；Web 模式保持兼容。
- 引入 `runners` 持久化模型、项目 `runner_id` 迁移和 `data-dir` 独占锁；先以现有 WSL/SSH 行为回归验证。
- 完成四个发布 POC：Windows 原生 Claude 审批 helper、WSL Adapter（含长驻 CLI/UNC 文件映射）、SQLite 驱动/构建链路、WebView2 WebSocket 子协议认证。

### 阶段 B：桌面宿主与安全连接

- 新建 `apps/desktop`，接入 Tauri 2 与单实例管理。
- Tauri 托管稳定页面资源；Go 服务实现随机端口、就绪通知、健康检查和受认证的优雅退出。
- 实现运行时配置、HTTP/WS 令牌认证、精确 CORS/Origin 校验和活跃运行托盘策略。
- 验证现有仪表盘、对话、任务、文件、Git、运行、通知和 SSH 页面无需视觉或功能分叉。

### 阶段 C：原生 Windows 与多发行版 WSL Runner

- 迁移用户数据目录，并采用阶段 A 验证通过的 SQLite 发布路径。
- 增加 `windows-local` Runner、Windows 文件浏览根、原生路径展示、Git、CLI、项目运行和审批 helper。
- 支持选择 WSL 发行版、创建持久化的 `wsl` Runner 记录、浏览 Linux 路径、运行长驻 CLI 会话和项目命令。
- 回归 SSH 项目的目录、文件、Git、Claude、Codex、审批与通知链路。

### 阶段 D：发布质量

- 单实例、服务崩溃恢复、日志导出、版本显示和升级前检查。
- WebView2 前置条件、Windows 代码签名、自动更新渠道、SQLite 备份与回滚策略。
- Windows x64 CI 构建、安装包冒烟测试与端到端测试。

## 9. 验收标准

桌面版达到以下条件才可替代当前 Web 启动方式：

- 安装后不需要手动启动 Vite 或 Go 服务；启动 `Milevia.exe` 即可进入当前界面。
- 页面风格、路由、项目列表、对话、任务、文件、Git、运行日志和通知与 Web 版本一致。
- Windows 本地项目可从 `C:\\`、`D:\\` 浏览、加载、编辑、Git 操作、启动和停止。
- Windows Runner 上的 Claude Code、Codex 和审批流程可用，不依赖 Git Bash 或 WSL。
- WSL 项目与 SSH 项目仍可运行，且不因 Windows 适配回退功能；WSL 文件、Git、项目运行和长驻 Agent 均在所选发行版内执行。
- 应用重启后项目、会话、任务、事件、通知和运行配置可从 SQLite 恢复。
- 应用重启后项目卡片排序、对话草稿、代码字号等 `localStorage` 偏好仍存在，不受随机 API 端口影响。
- 端口冲突不会阻止启动；第二个应用实例会聚焦第一个实例。
- 同一 `data-dir` 不能被两个 Control Server 同时打开；第二个启动请求必须发现并复用已有服务或明确提示其不可用。
- 本机非本应用页面无法调用 Control Server 的敏感 API 或 WebSocket。
- 关闭有活跃运行的窗口会进入托盘或要求明确确认；正式退出会停止服务并保持 SQLite 一致性。
- 浏览器 Web 模式仍可通过同一份 `apps/web/dist` 和 Go 的 `web` 模式作为本机单用户入口运行，不需要桌面应用；远程 Web 访问不在首发范围。
- 卸载默认保留用户数据；升级不会破坏已有 SQLite 数据库。

## 10. 后续扩展

该方案保留跨平台方向：Tauri 窗口和 React 页面可以复用到 macOS/Linux；Go Control Server 与 Runner 模型不变，只需新增平台本地运行器和安装包。Windows 桌面版完成后，下一步不应复制业务页面，而应继续完善 Runner 能力、自动更新、签名与多机器控制平面。

## 11. 参考

- [02-架构设计](02-架构设计.md)：已有的客户端、控制平面与 Runner 分层目标。
- [10-Windows项目目录支持](10-Windows项目目录支持.md)：当前从 WSL 访问 Windows 项目的过渡方案。
- [11-SSH远程项目支持](11-SSH远程项目支持.md)：SSH Runner 的现有边界与演进路径。
- [12-项目启动停止功能](12-项目启动停止功能.md)：项目运行进程管理与 Windows 互操作实现。
- [Tauri Sidecar 官方文档](https://v2.tauri.app/develop/sidecar/)
- [Tauri Security 官方文档](https://v2.tauri.app/security/)
