# 20-按项目环境分发的 Agent Runner 一次性实现方案

> 日期：2026-08-07
> 目标：让 AI Agent（Claude Code / Codex）的运行环境严格跟随项目所在环境——项目在哪个环境，就在那个环境校验并启动 Agent，加载时校验的 CLI 与运行时执行的是同一个环境，消除"展示为 Windows 却跑在 WSL"的错位。一次性完整实现，不拆分阶段。
> 关联文档：[17-Windows桌面端最终架构与实施方案.md](17-Windows桌面端最终架构与实施方案.md)（Runner 模型与 Windows 后端改造）、[10-Windows项目目录支持.md](10-Windows项目目录支持.md)（早期 /mnt 复用 WSL Runner 的过渡方案）。

---

## 1. 问题

### 1.1 用户诉求

三个形态要统一到一个原则：

> **项目是哪个环境的，Agent 就在哪个环境跑，加载时校验的也就是那个环境。**

- 部署在 **WSL**：加载 WSL 项目 → WSL 的 claude/codex；加载 Windows（/mnt/c、/mnt/d）项目 → 应驱动 Windows 侧的 claude/codex。
- 部署在 **Windows 本机（桌面版）**：加载 Windows（C:\、D:\）项目 → 本机 Windows 原生 claude/codex；加载 SSH 项目 → 远端。
- **SSH**：远程 Linux 项目 → 远端 claude/codex（已实现，本期不动）。

> **范围的诚实承诺**：WSL 与 Windows 两个本机 runner 在**同一服务端进程**内二选一注册（`app.go:540` 按 `runtime.GOOS`），不能同时持两套本机 claude。因此"Windows 本机部署下加载 WSL 发行版项目 → 该发行版内的 claude"，依赖文档17 的**多发行版 WSL Adapter**——本期非目标（见 §5），前端在 Windows 部署下只会列出本机和 SSH runner，不会把 wsl 项目导到本机 Windows claude 上；SSH 远端则不限部署形态、始终可用。

当前行为与这一原则存在错位（详见 1.2、1.3）。

### 1.2 现状诊断（已核实）

代码里已经存在"按环境"的*元数据层*（id、meta、Environment 字段、runners 表种子），但**没有接到 Agent 进程的 spawn 上**：

1. **本机 Agent Runner 是单实例、按服务端 OS 定死的。** `NewWithRunner`（`app.go:500-502`）固定创建 `s.runner = newClaudeCLIRunner(config)` 与 `s.codexRunner = newCodexCLIRunner(config)`，在服务端进程所在 OS 直接 `exec.Command(ClaudePath/CodexPath)`（`claude_runner.go:357/420`、`codex_runner.go:236`）。它是 WSL 里的 claude，还是 Windows 里的 claude，完全取决于*服务端跑在哪*，与*项目在哪*无关。

2. **Ready 校验不看项目。** `createProject`（`app.go:1733-1734`）、`listProjects`（`app.go:1979`）对本地项目一律用 `s.runner.Ready()` / `s.codexRunner.Ready()`——无论项目是 WSL 还是 Windows 路径都查同一套本机 CLI。`isLocalRunnerID`（`app.go:4723`）只按 `runtime.GOOS` 区分 wsl-local/windows-local，等于服务端是 WSL 就永远查 WSL、永远不查 Windows。

3. **发消息选 runner 也不看项目。** 入队逻辑（`app.go:3420-3445`）`runnerObj` 默认 `s.runner`，codex 非 ssh 走 `s.codexRunner`，ssh 走 registry。唯一的环境相关分支是 `app.go:3413-3416` 的硬编码拦截：

   ```go
   if runtime.GOOS == "windows" && projectRunner == "wsl-local" {
       return ..., errors.New("该项目使用旧 WSL Runner；请先配置对应的 WSL 发行版 Runner")
   }
   ```

4. **`decorateProjectPresentation`（`app.go:2128-2135`）用 `/mnt/` 前缀猜 Environment。** Windows 部署下 path 是 `C:\`，该函数会让 `Environment` 恒为 `"wsl"`，前端展示错误。

5. **Windows 侧缺两条执行能力**（`claude_runner.go` 是全平台同一份源码，无 build tag 分支）：
   - **审批 Hook**：唯一实现是 `sh <claude-approval-hook.sh>`（`claude_runner.go:567`）。Windows 没有 sh，缺 Windows 审批 helper（文档17 §5.3 规划了 `milevia-approval.exe`）。
   - **CLI 路径解析**：`exec.Command("claude")` 在 Windows 对本机 `.cmd` shim 不可直接 spawn，需兼容 `PATHEXT`（文档17 §5.3 已明确）。

6. **运行目标分派仍带 wsl 中心痕迹。** `resolveRunExecutionTarget`（`project_runner.go:375`）与 `newProjectRunCommand` 已是按 Runner 派发的样板（windows+GOOS=windows → `cmd.exe /c`；WSL → `sh -c`；WSL 跨到 Windows → PowerShell），但**这套只用于项目启动命令，Agent 会话没走它**。

### 1.3 核心矛盾一句话

> 元数据层知道项目是什么环境（`Environment`/`runner_id`），Agent Runner 却只认服务端自己的 OS。两者从未被接起来，于是"校验 WSL、运行 WSL"，而前端却把 `/mnt/` 项目标成 windows。

---

## 2. 设计原则（与文档17对齐）

遵循文档17 §3.3 的 Runner 模型：**Runner 描述执行地点，Agent 描述 CLI 类型，两者正交。**

本期落地的关键原则：

1. **项目环境唯一决定 Agent 的执行环境。** 依据订单 `runner_id`（`projects.runner_id`，兼容旧 `runner` 字段）解析出项目 Runner → 得到项目的执行环境 → 选择对应环境的 Agent Runner 做 `Ready` 校验和 `Run/StartSession`。
2. **校验即运行环境。** 加载项目时对 Agent 的就绪校验，必须与被选中的 Runner 同源：`/mnt/`（WSL 部署下的 windows 项目）校验 Windows 侧 CLI；`~` 下项目校验 WSL 侧 CLI；Windows 部署下 Windows 项目校验本机 Windows CLI。
3. **环境推导不靠猜。** 弃用"以 `/mnt/` 前缀猜 Environment"的做法，改为以项目 Runner 的环境属性为准；`/mnt/` 前缀仅作为 WSL 部署下路径→环境的便捷映射保留。
4. **Windows 会话能力补齐（分承诺等级）。** Windows 环境要有可在 Windows 侧跑长驻 claude/codex 会话、执行审批 Hook、管理进程树的能力——保证"Windows 环境 → Windows 侧 Agent"语义真实，而不是只把 UI 标成 windows。承诺等级：**Windows 本机部署**（`exec.Command` 本机跑）为必须实现；**WSL 部署跨到 Windows 侧长驻 claude**（§3.4-2）为可回退边界，未通过 POC 时如实报错降级，不静默回退 WSL。
5. **向前兼容。** 单发行版 WSL 仍是首期目标；`runners` 表多发行版 WSL Adapter（文档17 §3.4）不阻塞本期，本期把"环境分发"先做成可插拔，后续加发行版只增 Adapter。

---

## 3. 一次性完整实现方案

不再分期。下面所有改动一次合入，共同构成"按项目环境选择 Agent Runner"的闭环。

### 3.1 引入"项目环境 → Agent Runner"分发

新增一个纯函数，输入项目 Runner / 项目路径，输出目标 Agent 环境，作为所有 Ready 校验、入队选 runner、环境推导的统一入口：

```go
// agentTargetEnv 由项目 Runner 解析出的"Agent 应运行的执行环境"。
type agentTargetEnv string

const (
    agentTargetEnvWSL     agentTargetEnv = "wsl"      // 本机 WSL 发行版
    agentTargetEnvWindows agentTargetEnv = "windows"  // 本机 Windows（原生 sidecar 或 WSL 跨界）
    agentTargetEnvRemote  agentTargetEnv = "remote-linux" // SSH 远端
)

// resolveAgentTargetEnv 依据项目 Runner 与路径判定 Agent 目标环境。
//
// 优先级（单发行版实现，见 §1.1 诚实承诺 + 本段 §3.1 要点）：
//  1. runner_id 明确 ssh-* → remote-linux（远端由 sshRunner 驱动）
//  2. runner_id 明确 windows-local，或服务端在 Windows 时的本机 runner → windows
//  3. WSL 服务端下 /mnt/<盘符>/ 路径 → windows（驱动 Windows 侧 CLI）
//  4. 其余 → wsl
func (s *Server) resolveAgentTargetEnv(runnerID, projectPath string) agentTargetEnv
```

> **落地偏差（比初稿更诚实）**：初稿第 3 条本写"runner_id 明确 wsl-local → wsl"。但在单服务端单发行版下，前端对 `/mnt/` 项目也传 `runner: wsl-local`（wsl-local 是本机唯一 runner，其 meta 把 `/mnt/<盘>` 挂成 root）。若第 3 条照初稿，`wsl-local + /mnt/` 会被判成 wsl，正是用户原始抱怨"加载 Windows 项目仍跑 WSL"的根因。故落地改为**以路径为准**：WSL 服务端下 `/mnt/` 项目恒为 windows（驱动 Windows 侧 CLI），`~` 项目恒为 wsl。这与 §1.1"单发行版本机 runner 由 runtime.GOOS 定"一致——`wsl-local`/`windows-local` 只是服务端本机 runner 的 id，不再额外承担"指定环境"语义（多发行版 WSL Adapter 仍是 §5 非目标）。

要点：

- Windows 本机部署下 `windows-local` 原生 runner 就是"目标 windows"；在 **WSL 部署**下"目标 windows"意味着要跨到 Windows 侧起 Agent（见 3.4）。
- SSH 项目保持现状：远端 CLI 由 sshRunner 驱动，本期只把 dispatch 归一化到同一入口，不改变 SSH 行为。

### 3.2 创建项目：按目标环境做 CLI 就绪校验

`createProject`（`app.go:1727-1738`）与冲突返回路径（`1739-1762`）不再固定 `s.runner.Ready()`：

```go
target := s.resolveAgentTargetEnv(runnerID, path)
claudeReady, codexReady := s.agentCLIReady(ctx, target)
if !claudeReady && !codexReady {
    writeError(w, http.StatusBadRequest,
        errors.New("该 Runner 环境中 Claude Code 与 Codex CLI 均不可用或未登录"))
    return
}
```

`agentCLIReady(ctx, target)` 内部按环境路由到对应 runner 的 `Ready()`：
- `wsl` → 本机 `claudeCLIRunner` / `codexCLIRunner`
- `windows` → 本机 Windows Agent Runner（3.4）或无 Windows 安装时正确降级
- `remote` → sshRunner 的 `Ready()`/`CodexReady()`

这样"加载 window 项目，校验的就是 Windows 侧 CLI"。若 Windows 侧未装 CLI，报错而非静默回退 WSL。

> **时序提示**：`createProject` 在**保存项目之前**就校验目标环境 CLI（`app.go:1727-1738`）。因此"能创建项目"天然以"目标环境已安装 claude 或 codex"为前提——`/mnt/` 项目要能创建，Windows 侧就得装好 CLI 并登录。这与现状（WSL 项目须有 WSL 侧 claude）语义一致，只是把"校验哪端"纠正为目标环境那端。前端若需要对"仅浏览目录、不建项目"的 Windows 路径放开，是独立于本次的交互增强，不在本文案范围。

### 3.3 发消息：按目标环境选 runner 与 ready

`message` 入队（`app.go:3420-3445`）的 `runnerObj` 选择重构为：

```go
target := s.resolveAgentTargetEnv(projectRunner, projectPath)

var runnerObj AgentRunner
switch {
case strings.HasPrefix(projectRunner, "ssh-"):
    r, ok := s.runnerRegistry.get(projectRunner)          // 现状不变
    ...
    runnerObj = r
case conversation.AgentID == "codex":
    // 注意：CodexReady 属于 CodexCapableRunner 接口（AgentRunner 上没有）。
    // 校验用独立的辅助函数，返回值类型为 CodexCapableRunner，不能是裸 AgentRunner。
    codex := s.codexRunnerFor(target)                      // wsl→本机 codex；windows→win codex；remote→ssh codex
    if r, ok := codex.(CodexCapableRunner); !ok || !r.CodexReady(ctx) { ...503... }
    runnerObj = codex
default: // claude-code
    claude := s.agentClaudeRunnerFor(target)               // wsl→本机 claude；windows→win claude；remote→ssh claude
    if !claude.Ready(ctx) { ...503 "Claude Code 不可用或未登录"（按环境报错）... }
    runnerObj = claude
}
```

删除 `app.go:3413-3416` 的硬编码拦截（其作用由"按 target 路由到对应 runner"的自然覆盖取代）。

`agentClaudeRunnerFor(target)` / `codexRunnerFor(target)` 是本模块的核心注册表，返回类型是 `AgentRunner`（claude 侧）与 `AgentRunner`（codex 侧，调用处按需断言 `CodexCapableRunner` 取 `CodexReady`）：

- 返回**本机** `claudeCLIRunner`/`codexCLIRunner`（环境 = wsl，服务端在 WSL 时即 WSL 侧；服务端在 Windows 时即本机 Windows CLI）。
- 返回 **Windows Agent Runner**（环境 = windows，见 3.4）——在 WSL 部署下负责跨到 Windows 侧执行。
- 未知 → 报清晰错误。

### 3.4 Windows Agent Runner（三种执行形态）

Windows 环境（`agentTargetEnvWindows`）的 Agent 执行，按服务端所处部署形态区分三种情况：

| 服务端部署 | 触发条件 | 执行方式 | 实施优先级 |
|---|---|---|---|
| **Windows 本机**（桌面版/Windows 部署） | Windows 项目（`C:\`、`D:\`） | 直接本机 `exec.Command` 跑 Windows 版 claude/codex | ✅ 主路径，先做 |
| **WSL** | `/mnt/` 项目（需驱动 Windows 侧 Agent） | PowerShell 起 Windows 侧 claude/codex，PID marker 回传 | ⚠️ 跨界难点，需 POC |
| WSL/Windows | 已装原生 `.exe` | `exec.LookPath` 直接起（兼容 `.cmd`/`PATHEXT`） | ✅ 随上述一并实现 |

实现要点（忠实文档17 §5.3，并复用 `project_runner.go` 的跨端样板）：

1. **CLI 路径解析**：在 Windows 侧用 `exec.LookPath` 兼容 `.exe` / `.cmd` / `PATHEXT`；取值来自本机配置的 `AUTO_CLAUDE_PATH` / `AUTO_CODEX_PATH`（见 3.6），缺省时为 `claude` / `codex`，交由 Windows `cmd.exe` 按 `PATHEXT` 解析。
2. **WSL 部署 + /mnt/ 项目的会话拉起（long-running streaming）——本期重点，也是唯一需 POC 处**：claude 的主工作方式是长驻 `StreamingAgentRunner`（持续 stdin 写入、流式读 stdout/stderr），这与一次性 Run 命令不同。本期首取**本机对称路径**（Windows 部署→本机 spawn；WSL 部署→`/mnt/` 项目如实标 windows 并如实降级/报错，不假装跑在 Windows 侧）。真正的"WSL 跨界长驻 claude"（PowerShell stdin/stdout 双向管道转发 + `newWindowsPIDMarker` PID 回传 + `taskkill /T /F` 清理）实现复杂、风险高，文档17 §3.4 已将其列为"须 POC、未通过不承诺"。本期将其作为独立、可回退的边界能力，**不并入本机对称主干**——确需时单独立项 POC。
3. **审批 Hook**：Windows 侧的 claude 走 `--native-approval-hook`（`claude_runner.go:565` 的 `NativeApprovalHook` 分支已有），命令指向新增的 **`milevia-approval.exe`**，从 stdin 读 Hook 事件、带一次性令牌向本机 Control Server 请求审批，不依赖 sh/curl/Git Bash。
4. **环境注入**：`AUTO_CONTROL_URL`、`AUTO_APPROVAL_RUN_ID/CONVERSATION_ID`、`AUTO_APPROVAL_TOKEN` 等（`profileLaunch` 已注入本机进程）对 Windows 子进程同样生效（`managedCLIEnvironment` 以 `os.Environ()` 为基，Windows 进程继承，无需跨界）。

> **诚实降级原则**：在"WSL 部署 + /mnt/ 项目"且未确认 Windows 侧可长驻运行 claude 时，`AgentReady` 与对话启动会**明确报中文错误**（指出需在 Windows 侧安装/配置 CLI，或改用本机对称环境），绝不静默回退到 WSL 侧——这正是消除"标成 windows 却跑 wsl"错位的核心。Windows 部署下 `agentTargetEnvWindows` 直接走本机 `exec.Command`（文档17 §5.3），连跨界转发器都不需要。

### 3.5 环境推导修正

`decorateProjectPresentation`（`app.go:2125-2142`）改为：

```go
func (s *Server) decorateProjectPresentation(project *Project) {
    project.FullPath = project.Path
    project.PathDisplay = projectPathDisplay(project.Path)   // /mnt 转 C:\…，其余用 base（复用文档10 方案）
    project.Environment = string(s.resolveAgentTargetEnv(project.Runner, project.Path))
    ...
}
```

删除 `project.Environment = "wsl"` / `/mnt/` 猜分支的叠加，统一走 3.1 的 resolver（仍含 `/mnt/` → windows 映射）。前端交互不变，`environment` 字段现在如实反映 Agent 将跑的环境。

### 3.6 配置与钩子落地

| 项 | 现状 | 本期改动 |
|---|---|---|
| `AUTO_CLAUDE_PATH` | 无（`ClaudePath` 硬编码 `"claude"`，`app.go:99`） | 新增环境变量，Windows 侧可配成 `.exe`/`.cmd` 路径；`CodexPath` 已有 `AUTO_CODEX_PATH` |
| Windows 审批 helper | 无 | 新增 `apps/control-server/cmd/milevia-approval/`（Go 单文件 exe，见 3.4-3） |
| `projects.runner_id` | 已有列与迁移 | 不动；分发以此为准 |
| 前端 environment 消费 | 项目卡片标的 `environment` | 不变（值语义改对即可） |

### 3.7 改动文件清单

**按 agent 环境分发的消费点（本方案要改成"环境决定 runner"的位置）：**

| 文件:行 | 现在的写法 | 本期改为 |
|---|---|---|
| `app.go:1639-1640`（createProject 本地分支 `s.runner.Ready`） | 固定本机 runner | `agentCLIReady(target)` |
| `app.go:1733-1734`（createProject `s.runner.Ready`） | 固定本机 runner | `agentCLIReady(target)` |
| `app.go:1717/1755`（createProject 冲突返回 `s.codexRunner.Ready`） | 固定本机 codex | `agentCLIReady(target)` |
| `app.go:1979`（listProjects `localCodexReady`） | 固定本机 codex | 按 target 环境 |
| `app.go:3420-3445`（message 入队 `runnerObj` 选择） | 默认 `s.runner`/`s.codexRunner` | 按 `resolveAgentTargetEnv` 分发 |
| `app.go:2128-2135`（`decorateProjectPresentation` 环境推导） | `/mnt/` 猜 | `resolveAgentTargetEnv` |
| `app.go:4708-4713` / `4808-4814` / `4858-4859` / `4905-4906`（codex 可用性/列表/更新入口） | 固定 `s.codexRunner` | 按 runner id 路由（本机 or win/ssh） |
| `orchestration.go:1240-1245`（`runIndependentReview` 的 `runner`） | 固定 `s.runner`/`s.codexRunner` | 按 project runner 解析目标 runner |

源文件（已落地）：

| 文件 | 改动 |
|---|---|
| `apps/control-server/internal/app/app.go` | 新增 `Server.windowsRunner` 字段；改 §3.7 上表各消费点；删原 `app.go:3413` 硬编码拦截；`ConfigFromEnv` 的 `ClaudePath` 支持 `AUTO_CLAUDE_PATH` |
| `apps/control-server/internal/app/agent_target_env.go`（新增） | `agentTargetEnv` 类型、`resolveAgentTargetEnv`、`agentCLIReady`/`codexReadyForTarget`/`agentClaudeRunnerFor`/`codexRunnerFor`/`windowsAgentRunner`、`agentTargetEnvUnavailable` |
| `apps/control-server/internal/app/win_agent_runner.go`（新增） | `windowsAgentRunner`：WSL 部署跨端就绪/版本探测（PowerShell bridge），Claude 会话按需降级如实报错 |
| `apps/control-server/internal/app/orchestration.go` | `runIndependentReview` 按 `resolveAgentTargetEnv` 路由 runner |
| `apps/control-server/internal/app/agent_target_env_test.go`（新增） | resolver 矩阵、`agentCLIReady`/`agentClaudeRunnerFor` 分发单测 |

> **未落地项（与文档 §3.4 分级一致）**：`win_agent_runner_windows.go`/`_unix.go` 平台分派未建——`windowsAgentRunner` 只在 WSL 服务端构造、经 PowerShell 单实现即可覆盖，Windows 服务端直接复用本机 `s.runner`/`s.codexRunner`，无需平台 tag。`cmd/milevia-approval/`（Windows 审批 helper）未在本文案落地：`claudeCLIRunner` 已通过 `--native-approval-hook`（`claude_runner.go:563`）与 `NativeApprovalHook` 处理 Windows 侧审批 helper，独立于本环境分发。

> **边界：本方案只改"agent 环境决定 runner"这一层。以下消费点保持"本机 vs 远程"二分不变，不要误改成按 agent 环境分发**（它们对 Windows 项目本就该在服务端本机操作，无论 claude 跑在 Windows 侧还是 WSL 侧）：`fs_handler.go:41`、`git_operations.go:1013/1040`、`orchestration.go:947`（automatic orchestration 校验命令须本机跑）、`app.go:1991/2098/5042/5094/5194/5223/5312`。这些是"文件/Git/编排命令在服务端本机执行"的正确语义，详见 §5。

---

## 4. 为什么不选"只修校验、Agent 仍跑 WSL"

用户的诉求是"环境即运行环境"。若只把校验对齐到 Windows、仍在 WSL 跑：

- 项目标成 Windows、入口校验 Windows CLI，但实际 Agent 仍用 WSL bash + WSL node 操作 `/mnt/`（9P 跨文件系统性能、权限语义差异），与 UI 语义不符；
- 加载一个"Windows 可跑、WSL 未装 claude"的项目时，会出现"校验通过但跑不起来"或"校验报错但明明装了 Windows claude"的割裂。

因此本期把"运行环境 = 项目环境"作为边界，与校验同步完成，才满足"加载哪个环境、就用哪个环境"：Windows 本机部署下 Windows 项目确实跑 Windows 侧 Agent；WSL 部署下 `/mnt/` 项目的跨界长驻按 §3.4 分级落地，未通过则如实报错降级，绝不让"标 Windows 却跑 WSL"发生。

---

## 5. 非目标 / 显式排除

- **多发行版 WSL Adapter**（文档17 §3.4）：本期仍单发行版，分发留出扩展点，纳入后续。
- **SSH Agent 行为变更**：SSH 远端已按项目 runner 正确驱动，本期只统一入口、不改语义。
- **"本机 vs 远程"二分的消费点不做环境分发**：`fs_handler.go:41`、`git_operations.go:1013/1040`、`orchestration.go:947`、`app.go:1991/2098/5042/5094/5194/5223/5312` 用 `isLocalRunnerID(project.Runner)` 做"本机执行 vs SSH 远程执行"判定。这些是文件/Git/编排校验命令在**服务端本机**执行的正确语义——Windows 项目对这些能力同样是"本机 runner"（服务端在 Windows 就本机 exec、在 WSL 就 WSL 侧操作 `/mnt/`），与"claude 跑在 Windows 侧还是 WSL 侧"无关。**不得将这些点误改成按 agent 环境分发。**
- **Agent profile 跨 runner 的 secret 管理**：`canManageProfileRunner`（`agent_profiles.go:756`，`runnerID == s.localRunnerID()`）当前只允许本机 runner 管理 profile。对 **Windows 本机部署**它已正确（本机 runner 即 windows-local）；对 **WSL 部署 + /mnt/ 项目（跨界 runner）**它保持本机-only，即跨界 runner 的 profile 托管暂不支持——符合文档17 §5.3"凭据由用户在对应 Runner 环境自行安装/登录"的设计，非本方案遗漏。
- **任务编排（一键验收）的独立审查跟随项目环境（落地行为变更）**：`runIndependentReview`（`orchestration.go:1240`）现按 `resolveAgentTargetEnv` 路由。WSL 部署 + home 项目、以及 Windows 本机部署均走本机 claude/codex，无回归；唯独 **WSL 部署 + /mnt/ 项目**的独立审查会命中 §3.4 跨界 POC 边界而如实报错（不再用 WSL claude 审视 Windows 项目，守住"不使用 WSL claude 操作 Windows 项目"的原则）。这是环境跟随原则在后台审查上的诚实落地，非回退——确需对 /mnt/ 项目跑编排时，用 Windows 本机部署或待跨界 POC。
- **Windows 打包/签名/自动更新、SQLite 驱动迁移**：属于文档17 发布路径，非本环境分发范畴。

---

## 6. 验证方式

1. **单测**：resolver 对 runner 值 × 路径 × GOOS 的矩阵单测；`agentCLIReady`/`agentClaudeRunnerFor`/`codexRunnerFor` 分派单测；新增 Windows Agent Runner 的 event/termination 单测。
2. **Windows 本机部署（主路径，必验）**：Windows 项目走本机 `exec.Command`，断言**无 PowerShell 跨界**；`createProject` 校验命中本机 Windows CLI；对话在 Windows 侧用本机 claude + `milevia-approval.exe` 审批；`project.environment === "windows"` 且实际进程是本机 Windows 侧。
3. **WSL 部署 + /mnt/ 项目（跨界，按分级）**：环境推导正确（`environment === "windows"`）；`createProject` 校验命中 Windows CLI 配置；长驻 claude 若未通过 POC，对话启动**如实报中文错误**（指示 Windows 侧安装/配置或改用本机对称环境），验证**绝不静默回退 WSL**。跨界长驻 POC（stdin/stdout 双向、停止传播、PID 清理）单独立项验收，不阻塞本机对称为主的交付。
4. **WSL 部署 + ~ 项目（回归）**：纯 WSL 项目 / SSH 项目的既有人工下发、审批、Git、任务编排不受影响。
