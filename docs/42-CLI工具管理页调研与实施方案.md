# CLI 工具管理页调研与实施方案

> 日期：2026-09-21（决策已拍板见 §11；自复查修订见 §14）
> 目标：提供一个「CLI 工具管理」页面，列出平台支持的所有 AI CLI 工具，自动检测版本与就绪状态，
> 未安装的可安装、已安装的可升级；目标环境缺 Node 运行时的一并处理。
> 结论：**可以落地，但不要把它做成「再加一个安装按钮」**。真正的缺口有两个：
> ① 「支持哪些工具」没有单一来源（Go 侧硬编码 8 处、前端 2 处清单 + **约 20 处二元三元式**）；
> ② 平台**没有托管的 Node 运行时**，而"安装 CLI"的真实前置是"先有 npm"。
> 先补这两块，检测 / 安装 / 升级就退化成"对目录里的条目做循环"。
>
> ⚠️ **§14 自复查推翻了初版的两条建议，并在 §4.2 / §6.3 / §7 补了四处漏项。看结论请以 §14 为准。**

## 1. 结论与目标能力

### 1.1 目标能力

| 能力 | 说明 | 现状 |
| --- | --- | --- |
| 列出所有支持的 CLI 工具 | 目录由服务端给出，前端不自己维护名单 | ❌ 名单散落在 8+ 处 |
| 自动检测版本 | 每个 Runner 上每个工具的「已装 / 版本号 / 就绪」 | ✅ 已有，但按 claude / codex 写死两套 |
| 检测运行前置（node / npm） | 安装与升级都依赖 npm | ❌ 无 |
| **安装 Node 运行时** | 目标环境没有 npm 时，平台内下载并托管（§7） | ❌ 无（本机也没有内置 Node） |
| 安装未安装的工具 | 走托管 npm + 托管 prefix | ❌ 完全没有（见 docs/09 §1 明确排除） |
| 升级已安装的工具 | 托管安装 → npm 重装；用户自装 → CLI 自带 update | ✅ 已有（仅后者） |
| 展示安装位置 | 二进制真实路径，可复制 | ❌ 只有环境变量，无 UI |
| 长任务进度 | 安装/升级耗时长，要有进行中与结果 | ⚠️ 只有 `updating` 一个布尔状态 |
| 远端安装 | 本机 / WSL / SSH 三端都可装（§9） | ❌ 无 |

### 1.2 关键判断

1. **`docs/09-Claude Code版本管理.md` §1 明确写了「不在平台内安装 Claude Code（安装属于 Runner 部署层面）」** —— 本次要做的正是突破这条边界，需要显式承认并重新定义安全前提（见 §9）。
2. **安装不是新机制**：`npmCLIInstall`（`npm_cli_install.go:16`）已经把「npm 全局包 → 命令 shim → 二进制路径 → 中断回滚」全部实现好了，而且 `claudeNpmCLIInstall`（`claude_runner.go:417`）与 `codexNpmCLIInstall`（`codex_runner.go:283`）已经是它的两个实例。**缺的是"安装"这个动作本身，不是基础设施。**
3. **并发闸门也已经就位**：`runnerMaintenanceMu` + `runnerUpdating map[runnerAgentKey]bool`（`app.go:6845`）已经是「按 runner + agent 粒度」的维护锁，`runnerAgentKey{runnerID, agentID}` 的形状天然支持扩展到任意工具。**不需要新造锁。**
4. **版本比较有一处真实缺陷**：Claude 用字符串不等判断（`claude_runner.go:347` `return latest != local, latest, nil`），Codex 用完整 semver（`parseCodexSemver` / `compareCodexSemver`，`codex_runner.go:152/194`）。用户若装了比 npm `latest` 更新的预发布版，管理页会把 Claude 显示成「有更新可用」，点下去是降级。做管理页前必须先统一成 semver。
5. **"装 CLI"的前置其实是"有 npm"**：本机（Windows 桌面端）没有内置 Node（`apps/desktop` 只拉起 Go sidecar），`npm` 是纯系统依赖；WSL / SSH 远端同样不保证有，而 `apt install nodejs` 需要 sudo 且版本常过旧。所以运行时必须是**平台托管**的（§7），否则"安装"按钮在干净机器上点了必失败。
6. **路径解析层是必须先动的地方**（§14.B）：`Config.ClaudePath` / `CodexPath` 被直接读 **14 处**，且 `Config` 按值传进 runner、runner 只在启动时构造一次。装到托管 prefix 之后若不改解析层，`Ready()` / `Version()` / `Run()` / 命令目录探测 / MCP 导入会**全部继续用旧路径**。

## 2. 现状盘点

### 2.1 已经有的正确基础

| 位置 | 内容 |
| --- | --- |
| `claude_runner.go:309` `Ready()` | `exec.LookPath` + `claude auth status`（存在性 + 认证） |
| `claude_runner.go:320` `Version()` | `claude --version`，剥 `" (Claude Code)"` |
| `codex_runner.go:92` `BinaryReady()` | `exec.LookPath`（只看二进制，不看登录态 —— 因为 api_key 档案自带凭据） |
| `codex_runner.go:97` `Version()` | `codex --version`，剥 `"codex-cli "` |
| `claude_runner.go:333` / `codex_runner.go:109` `CheckUpdate()` | `npm view <pkg> version` 对比本地 |
| `claude_runner.go:370` / `codex_runner.go:243` `Update()` | 调 CLI 自带 `update` 子命令 + 失败回滚 |
| `npm_cli_install.go` | npm 全局前缀解析、shim 生成、中断回滚（**可复用于安装**） |
| `app.go:6845` `runnerUpdating` | 按 `runnerAgentKey{runnerID, agentID}` 的维护锁 |
| `mcp_client.go:1283` `/api/mcp/runtime-check` | **目标环境探测命令是否存在的现成范式**（`command -v X \|\| echo <marker>`，区分"没装"与"通道坏了"） |
| `filesystem.go:721` `SFTPFilesystem.WriteFile` | SFTP 写远端文件能力（§14.F 的兜底要用它） |
| `claude_runner.go:60` `autoUpdateSupportedRunner` | 「有新版但不能应用内升级」的三态语义（跨端 runner） |

### 2.2 「支持哪些工具」散落在哪些地方

**Go 侧 8 处**

| # | 位置 | 形态 |
| --- | --- | --- |
| 1 | `agent_profiles.go:353` | `validProfileAgent()` 硬编码两值 |
| 2 | `agent_profiles.go:644` | `for _, agentID := range []string{"claude-code", "codex"}` |
| 3 | `agent_profiles.go:570/667/668` | 视图结构体里 `Claude` / `Codex` **两个具体字段** |
| 4 | `mcp_config.go:119` | `agentID != "claude-code" && agentID != "codex"` |
| 5 | `mcp_servers.go:221` | 同上，校验循环 |
| 6 | `git_conflict_suggest.go:159` | 同上 |
| 7 | `insights.go:230` / `958` / `2176` | 默认值与分支 |
| 8 | `app.go:6846/6879/6985/6993` | `listRunners` / `runnerStatus` 里 **两段几乎相同的 if 块** |

**前端：2 处清单 + 约 20 处二元三元式**

| 位置 | 形态 |
| --- | --- |
| `types.ts:21` | `type AgentID = "claude-code" \| "codex"` |
| `types.ts:37` | `type SkillAgent = "claude-code" \| "codex"` |
| `ProjectAiConfigDialog.tsx:34` | `AGENT_IDS` 常量数组 |
| `ConversationPage.tsx:818` | `(["claude-code","codex"] as AgentID[])` |
| 三元式 | `ConversationPage.tsx:476/477/478/553/610/737/775/797/802/853/990/1129/1143`、`AgentProfilesPage.tsx:242`、`ScheduledTasksPage.tsx:91/243/281`、`OrchestrationPage.tsx:33/89`、`ProjectAiConfigDialog.tsx:79/137/154/169` |

### 2.3 结构性症状

- **`CodexCapableRunner` 是耦合的证据**：为了把 Codex 塞进 Claude 形状的 `updateAgent`，专门写了一个 `codexRunnerAdapter`（`app.go:7090`），把 `CodexVersion` 映射成 `Version`、把 `Run` 映射成"报错"（`:7094`）。
- **`app.go:6846` 与 `:6985` 是两段近乎逐行重复的代码**，只差 `"claude-code"` / `"codex"` 两个字面量与 `runner.Version` / `codexR.CodexVersion` 两个取值点。加第三个工具就是加第三段。
- **本地 Codex 与 registry 中的 Codex 是两条路**：`isLocalRunnerID(m.ID)` 分支走 `s.codexRunner`，否则走 `runner.(CodexCapableRunner)`（`app.go:6859-6877`、`7013-7027`）。这个特殊分支本身就是"目录缺失"的代价。
- **耦合一直贯穿到线格式**：`app.go:6854/6884` 输出 `entry["claude"]` / `entry["codex"]` 两个具名字段，前端 `ConversationPage.tsx:476` 就靠 `agentID === "codex" ? runner?.codex : runner?.claude` 取值。**`docs/13 §8.1` 早就提出应改为 `agents[]`，一直没做。**

## 3. 方案比较与推荐

| 方案 | 做法 | 评价 |
| --- | --- | --- |
| A. 只加安装按钮 | 在现有对话框的版本区加一个「安装」按钮，后端加 `claude/install`、`codex/install` 两条路由 | 能满足字面需求，但复制第 3 套平行代码；第三个 CLI 到来时再复制第 4 套。**不采用** |
| B. 工具目录 + 托管运行时 + 泛化 API | 后端一份 `agentCatalog`；一份托管 Node 工具链；API 泛化为 `/api/runners/{id}/agents/{agentID}/...`；检测/安装/升级都是对目录条目的循环 | 一次性解决清单散落、平行代码、以及"干净机器上装不了"。**推荐** |
| C. 顺带做插件化（第三方工具可注册） | B + 外部清单文件 / 插件目录 | 收益要到真接入第三个（如 Gemini CLI、Aider）时才体现；且外部清单会新增一条"谁定义工具"的信任链。**本期不做，但 B 的目录结构要留出扩展位** |

**推荐 B**，执行范围按已拍板决策全放开到本机 / WSL / SSH（见 §9）。

## 4. 领域模型

### 4.1 `AgentCatalogEntry`（工具目录，唯一事实来源）

新建 `apps/control-server/internal/app/agent_catalog.go`：

```go
// AgentCatalogEntry 描述一个平台支持的 AI CLI 工具。所有"支持哪些工具"的判断
// 都必须从这里读 —— 这是本方案的核心不变量。
type AgentCatalogEntry struct {
	ID       string // "claude-code"，持久化标识，不可改
	Name     string // "Claude Code"，界面名
	Vendor   string // "Anthropic"
	Homepage string
	DocsURL  string

	// 安装通道与命令解析
	InstallKind string   // "npm-global"（当前唯一实现）；"native" 见 §11 开放项
	NpmPackage  string   // "@anthropic-ai/claude-code"
	CommandName string   // "claude"
	BinFiles    []string // ["claude.exe"] / ["codex.js"]
	// MinRuntimeVersion 是该 CLI 要求的 Node 最低版本（Claude Code 要求 >=18）。
	// 光"有 npm 就用"不够：Node 16 上装了也跑不起来。
	MinRuntimeVersion string

	// 版本探测
	VersionArgs     []string // ["--version"]
	VersionTrim     string   // " (Claude Code)" / "codex-cli "
	VersionIsSemver bool

	// 升级：SupportsManagedUpgrade 决定"升级"走 npm 重装还是 CLI 自带 update（§6.3）
	SupportsManagedUpgrade bool
	UpdateArgs             []string // ["update"]，仅用于 discovered 安装

	SupportsInstall bool
	Requires        []Requirement // [{Command:"npm", Label:"Node.js（含 npm）", Kind:"managed-runtime"}]
	Capabilities    AgentCapabilities
}
```

```go
func agentCatalog() []AgentCatalogEntry // 唯一清单
func agentByID(id string) (AgentCatalogEntry, bool)
```

**收口规则**：`validProfileAgent`、`agent_profiles.go:644` 的循环、`mcp_config.go:119`、`mcp_servers.go:221`、`git_conflict_suggest.go:159`、`insights.go:230` 一律改为查目录；`app.go:6846/6985` 两段重复块合并成一个 `probeAgents(ctx, runnerID) []AgentStatus`。

> ### ⚠️ 关于前端 `AgentID`：**不要**改成 `string`（§14.A 已推翻初版建议）
>
> 初版主张把 `types.ts:21` 的联合类型放宽为 `string`，理由是"目录新增工具时前端会编译期拒绝"。**这条是错的**，因为：
>
> 1. 前端真正的问题不是类型太窄，而是 **约 20 处二元三元式** `agentID === "codex" ? X : Y`（清单见 §2.2）。它们**默认落到 Claude**，第三个工具进来会被静默标成 "Claude Code"；放宽类型**一处都修不好**，只会让它们从"编译报错"变成"编译通过但标签是错的" —— 即**假装修好了**。
> 2. 这些三元式还被测试**逐字钉住**：`conversation-layout.test.mjs:327` 断言 `const agentPath = agentID === "codex" ? "codex" : "claude";`、`race-guards.test.mjs:79/95/96/97`、`command-picker.test.mjs:49/60`。改它们必须同步改锚点。
> 3. 联合类型是**安全网**，不是病因 —— 它让"这里还没适配第三个工具"变成编译错误，正是我们想要的信号。
>
> **正确做法**：union 保留；真修的是两件事 ——
> ① **线格式**改为 `agents: AgentStatus[]`，取代 `runner.claude` / `runner.codex` 两个具名字段（`app.go:6854/6884`、`types.ts` 的 `RunnerInfo`、`ConversationPage.tsx:476`）；
> ② 那约 20 处二元三元式**收敛到一处查表**（显示名 / 权限模式 / 是否支持斜杠命令 / 能力），查表数据来自 `GET /api/agents`。
>
> 这样加第三个工具时，编译器会指着剩下的每一处未适配点。

### 4.2 `AgentInstallation`（安装登记）

`Config.ClaudePath` / `CodexPath` 目前**只来自环境变量**（`app.go:113-133`），且 **`Config` 是按值传进 runner 的**（`newClaudeCLIRunner(config Config)`，`claude_runner.go:305`），runner 只在启动时构造一次。

新增 SQLite 表（走既有幂等迁移范式，见 docs/13 §6.1 的告警）：

```sql
create table if not exists agent_installations (
  runner_id    text not null,
  agent_id     text not null,           -- "claude-code" / "codex" / "node"
  binary_path  text not null,           -- 实测可用的绝对路径
  install_kind text not null,           -- npm-global-system | npm-global-managed | native
  prefix       text not null default '',-- npm 全局 prefix（升级时要用它找对位置）
  version      text not null default '',
  source       text not null,           -- managed-install | discovered | env-override
  installed_at text not null,
  updated_at   text not null,
  primary key (runner_id, agent_id)
);
```

> ### ⚠️ 这张表真正必需的理由（§14.B 修正）
>
> 初版给的理由是"GUI 继承到陈旧 PATH" —— 那是次要的，因为系统级安装的路径可以用固定候选算出来（`app.go:118-128` 那段 Windows 兜底就是干这个）。
>
> **真正必需的是**：托管 prefix **根本不在 PATH 上**，`exec.LookPath` 永远找不到它。所以：
> - **路径解析必须变成一个可查询的解析器**，而不是 `Config` 里的一个不可变字符串 —— 否则 `Ready()`(`claude_runner.go:310/315`)、`Version()`(`:323`)、`Run()`(`:527`/`:613`)、`codex_runner.go:93/100/340/740`、`project_commands.go:280`（命令目录探测）、`mcp_import.go:348/612`（MCP 导入）**这 14 处的行为不会跟着安装结果改变**，装完等于没装；
> - 连带 `project_availability.go` 与 `agent_target_env.go` 的项目可用性判定也会一起变红。
> - `Config` 按值复制 ⇒ 改路径要么重建 runner，要么把"路径"换成一个可变的 resolver（建议后者：`type binaryResolver interface{ Path(agentID string) (string, bool) }`，注入 runner）。
>
> 解析顺序固定为 **env 覆盖 > 登记表 > PATH 查找 > 平台兜底路径**，并在**每次安装/升级成功后把实测到的绝对路径与 prefix 一起写回**（`binary_path` 与 `install_kind` / `prefix` 必须成对，否则"升级"会去找错文件）。

### 4.3 `RuntimeInstallation`（托管运行时登记）

复用同一张表：`agent_id = "node"`，`install_kind = "managed-toolchain"`，`binary_path` = `node` 可执行文件绝对路径，`prefix` = 该工具链下给 CLI 用的 npm 全局 prefix，`version` = 实测 `node --version`。

### 4.4 `AgentOperation`（长任务）

安装/升级是数十秒到数分钟级操作（下载 Node 约 30 MB），且需要"进行中"与"结果"两态。现状只有 `runnerUpdating` 一个布尔。

```go
type AgentOperation struct {
	ID            string // op_xxx
	RunnerID      string
	AgentID       string // 也可能是 "node"（运行时安装）
	Kind          string // install-runtime | install | update
	Status        string // running | succeeded | failed | cancelled
	Stage         string // resolving | downloading | verifying | extracting | installing | verifying-install
	TargetVersion string
	FromVersion   string
	ToVersion     string
	Progress      int    // 0-100，只有下载阶段能给准确值，其余为阶段推进
	Output        string // 清洗后的尾部输出（复用 stripAnsi/redactAgentText/tailUpdateOutput）
	StartedAt     time.Time
	FinishedAt    time.Time
}
```

`runnerUpdating` 的语义泛化为「该 (runner, agent) 有 operation 在跑」，键类型不变；运行时安装用 `agentID="node"` 占位，从而**天然与 CLI 操作互斥**（同一台机器上不会两个 npm 同时写同一个 prefix）。**不要**为进度另起一套轮询机制 —— `ProcessStatusProvider` 的 REST + WS 模式已是项目既有答案。

## 5. API 设计

### 5.1 新增：工具目录

```
GET /api/agents
```
返回 §4.1 的目录（含 `name` / `vendor` / `npmPackage` / `requires` / `capabilities` / `minRuntimeVersion`）。**这是前端唯一可用的工具名单来源**，也是那约 20 处二元三元式收敛后的查表数据。

### 5.2 新增：某个 Runner 上全部工具 + 运行时的状态

```
GET /api/runners/{runnerID}/agents
```
```jsonc
{
  "runnerId": "wsl-local",
  "environment": "wsl",
  "probeOk": true,                    // false = 通道级失败，下面的 items 一律不可信
  "probeError": "",                   // "WSL 未安装" / "SSH 未连接"
  "runtime": {
    "id": "node",
    "installed": true,
    "version": "24.5.0",
    "npmVersion": "10.8.2",
    "npmPath": "/home/u/.local/share/milevia/toolchain/node/bin/npm",
    "origin": "managed",              // "system" | "managed" | "none"
    "managedPath": "/home/u/.local/share/milevia/toolchain",
    "latestVersion": "24.9.0",        // 来自 runtimes/catalog（§14.D）
    "updateAvailable": true,
    "installSupported": true,
    "installBlockedReason": "",       // "远端未授权" / "该架构无可用的官方分发包"
    "meetsMinVersionFor": ["claude-code"]
  },
  "remoteInstallAllowed": true,
  "items": [
    {
      "id": "claude-code",
      "installed": true,
      "version": "2.1.216",
      "binaryPath": "/home/u/.local/share/milevia/npm-global/bin/claude",
      "installKindUsed": "npm-global-managed",
      "ready": true,
      "reason": "",
      "installSupported": true,
      "updateSupported": true,
      "autoUpdatable": true,
      "operation": null
    },
    { "id": "codex", "installed": false, "installSupported": true, "reason": "未安装" }
  ]
}
```

**必须区分三种"空"**（本项目红线，见 MEMORY「空列表有三种真相」）：
1. `probeOk=false` → **通道坏了**，界面显示「无法检测：WSL 未安装 / SSH 未连接」，**绝不渲染成"未安装"**；
2. `probeOk=true` 且 `installed=false` → **真的未安装**，给出「安装」按钮；
3. 请求进行中 → 骨架/加载态。

另需区分三档**不可安装**，文案各不相同：远端未授权（§9.1）／运行时缺失或版本过低（§7.3）／该架构或 libc 无官方分发包（§7.4）。三者都不能渲染成灰按钮了事。

> ⚠️ **运行时探测只放在这个端点**，不要加进 `/api/runners`（§14.G）：`listRunners`（`app.go:6839`）已经对每个 runner 起一次 `claude --version`，而 `ConversationPage.tsx:543` 在更新中每 5s 轮询 `/api/runners`；再塞 node/npm 探测会变成每次多两个子进程。

### 5.3 新增：运行时与工具操作

```
GET  /api/runtimes/catalog                            # 可安装的 Node 版本（来自 nodejs.org/dist/index.json，§14.D）
POST /api/runners/{runnerID}/runtime/install          { "version": "lts" | "24.9.0" }
POST /api/runners/{runnerID}/remote-install/grant     # 授权该 runner 允许远端安装（§9.1）
DELETE /api/runners/{runnerID}/remote-install/grant

POST /api/runners/{runnerID}/agents/{agentID}/check-update
POST /api/runners/{runnerID}/agents/{agentID}/install
POST /api/runners/{runnerID}/agents/{agentID}/update
GET  /api/runners/{runnerID}/agents/{agentID}/operations/{opID}
```

工具操作的响应沿用现有 `checkAgentUpdate` 的形状（`updateAvailable` / `autoUpdatable` / `currentVersion` / `latestVersion` / `error`），只把 `{agentID}` 参数化。

### 5.4 旧路由处理

`/claude/check-update|update`、`/codex/check-update|update`（`app.go:1119-1122`）**保留为薄适配**（内部转调泛化 handler），并在文档与注释里标注为 deprecated。理由：`ConversationPage.tsx:509/555` 正在用，一次性改会同时动到对话页回归面。

### 5.5 明确不新增

**不把 install / update / runtime 暴露到 `/api/remote/*`。** 手机端只读（§9.3）。

## 6. 后端执行设计

### 6.1 检测（probe）

`probeAgents(ctx, runnerID)` 对目录做循环，每个工具：

1. 解析二进制路径（§4.2 的四级顺序，**经 resolver**，不是读 `Config` 字段）；
2. 未找到 → `installed=false`；
3. 找到 → 跑 `<bin> VersionArgs` 取版本（5s 超时，沿用 `configureProcessGroup`）；
4. 就绪判定按工具区分：Claude 要额外 `auth status`，Codex 只要二进制（**沿用既有正确判断，不要统一成一种** —— `codex_runner.go:80-87` 的注释解释了为什么 Codex 不能查登录态）。

运行时（`node` / `npm`）探测同样走 resolver。探测整体并发跑（现有 `runnerStatus` 已用 `sync.WaitGroup`，`app.go:6941`），并保留 `probeOk` 与 items 的区分。

### 6.2 安装 CLI

```
npm --prefix <托管 prefix> install -g <NpmPackage>@<version>     # version 缺省 latest
```
（走系统 npm 时则不带 `--prefix`。选择逻辑见 §7.3。）

- **必须在目标环境执行**（本机 / `wsl.exe -- sh -c` / SSH），不是 control-server 所在侧 —— 与 MCP 的 `runtime-check` 同一条纪律（`mcp_client.go:1291` 的哨兵注释解释了为什么要把"没装"与"通道坏了"分开）。例外：npm registry 的**版本号查询**可以留在本机（`claude_runner.go:350` 的注释已论证）。
- **安装后自检**：跑一次 `--version`；成功后把**实测的绝对路径 + prefix + install_kind** 一起写回 `agent_installations`（缺 prefix 则升级会找错位置）。
- **失败清理**：安装没有"上一版本"可回滚，失败时清理半成品包目录并**保留输出尾部**给用户（复用 `tailUpdateOutput` / `redactAgentText`，`claude_runner.go:435`）。

### 6.3 升级（§14.E 已简化）

**按 `install_kind` 分两条路，不再是"总是调 CLI 自带的 update"**：

| 该工具怎么装上的 | 升级怎么做 | 为什么 |
| --- | --- | --- |
| `npm-global-managed`（平台装的，落在托管 prefix） | **`npm install -g <pkg>@latest` + 自检** | 布局是我们自己定的，确定性最高；且 **install 与 update 走同一条代码路径** |
| `npm-global-system`（用户已有 npm，装到系统全局） | 同上（不带 `--prefix`） | 同上 |
| `native` / `discovered`（用户自己装的） | 调目录里的 `UpdateArgs`（`claude update` / `codex update`）；失败即如实报错 | 我们不知道它的布局，交给 CLI 自己的更新器 |

这条改动同时**消掉了初版最大的不确定性**：初版担心"托管 prefix 装出来的包，`claude update` 行为未知"，并要求先做三端 POC。现在我们**根本不在托管安装上调 CLI 的 update**，那条 POC 在托管路径上不存在了；只剩 `discovered` 那条降级路，失败也不影响主流程。

仍保留的改造：删掉 `codexRunnerAdapter`（`app.go:7090`）这类"把工具塞进另一个工具形状"的适配器；把 `parseCodexSemver` / `compareCodexSemver` 提取为共享 `semver.go`，替换 `claude_runner.go:347` 的 `latest != local`。

### 6.4 并发与闸门

- 复用 `runnerMaintenanceMu` + `runnerUpdating[runnerAgentKey]`：同一 (runner, agent) 同时只允许一个 install/update；运行时安装占 `agentID="node"`，因此与同机的 CLI 安装/升级天然互斥。
- `updateAgent`（`app.go:7113`）已实现"与 run admission 串行 + 有活跃对话则拒绝"，install 走同一条闸门。
- 更新/安装期间该工具在界面显示「进行中」，且**新会话创建被拒绝或降级**（现有 `updating` 语义）。

## 7. Node.js 运行时：托管工具链

已拍板"平台内一并安装 Node"，这是本方案里体量最大、也最容易做错的一块。

### 7.1 核心选择：托管工具链，而不是动系统

**不采用**下面这些，理由是逐条的：

| 做法 | 为什么不行 |
| --- | --- |
| `apt install nodejs` | 需要 sudo（SSH 远端多数没有免密 sudo）；发行版自带的 Node 版本常年过旧，CLI 兼容性差 |
| `winget install OpenJS.NodeJS.LTS` | 需要 winget 可用（Win10 1809+ 才有 App Installer）；MSI 安装要 UAC 提权 |
| 官方 MSI / 安装脚本（`curl \| bash`） | 同样要提权；且在远端**执行一句来自网络的 shell**，供应链风险与本方案"解压即用"不是一个量级 |
| `nvm` / `fnm` | 引入**第二套状态**（"当前用哪个版本"）—— 而 CLI 本身就是状态，立刻变成"两处真相"，正是本项目一直在防的错 |

**采用**：下载官方 Node.js **分发包**（Windows `.zip`，Linux `.tar.xz`），解压到平台自己的目录，用它的 `node` / `npm` 工作。三个好处：
1. **不要 sudo、不要 UAC**；
2. **不污染系统**：删掉一个目录就完全回到原状；
3. **不和用户已有的 Node 打架**：不动 PATH、不动 `npm prefix -g`。

### 7.2 落点与 npm prefix

| 环境 | 工具链目录 | 分发包 |
| --- | --- | --- |
| Windows 本机 | `%LOCALAPPDATA%\Milevia\toolchain\node\` | `node-vX-win-x64.zip` |
| WSL | `~/.local/share/milevia/toolchain/node/` | `node-vX-linux-x64.tar.xz` |
| SSH 远端 | 同上（远端家目录下） | 同上 |

**CLI 的全局包也落在托管目录**，于是每个 CLI 的二进制路径可预测、可登记：
- Windows：包体 `toolchain/npm-global/node_modules/<scope>/<pkg>/bin/<binFile>`，shim `toolchain/npm-global/<cmd>.cmd`
- Linux：包体 `toolchain/npm-global/lib/node_modules/<scope>/<pkg>/bin/<binFile>`，bin 链接在 `toolchain/npm-global/bin/<cmd>`

**`npm_cli_install.go` 零重写**：`packageRoot` / `binaryPath` / `commandPath`（`:23` / `:30` / `:34`）已经按这个形状写好，只是把 `prefix` 从 `npm prefix -g` 的结果换成托管目录即可。

### 7.3 解析顺序（三级，顺序不许换）

1. 用户已有 `npm`（`exec.LookPath`）→ 用它的 `npm prefix -g`，装到**系统全局**；
2. 托管工具链已存在 → 用托管 `npm` + 托管 prefix；
3. 都没有 → 提示「将下载 Node.js 运行时（约 xx MB）」，用户确认后安装为托管工具链，再按 2 执行。

> 注意 1 与 2 的**安装结果位置不同**，所以 `binary_path` 必须与 `install_kind` + `prefix` **成对记录**，否则"升级"会去找错文件。
>
> 另需一道**最低版本闸门**（§14.D）：光"有 npm 就用"不够 —— 用户有 Node 16 时，Claude Code 装了也跑不起来。判据用 `agentCatalog().MinRuntimeVersion`，不满足时如实报「运行时版本过低（当前 16.x，需要 ≥18）」，**不要**静默回落到托管安装（那会让用户机器上出现两套 Node 而没人知道）。

### 7.4 下载、校验与目标平台判据

- **必须先判目标平台的 arch 与 libc**（§14.C）：
  - `uname -m` → `x86_64` / `aarch64` / …，据此选 `linux-x64` / `linux-arm64` / `win-x64` / `win-arm64`；
  - **官方 Node 的 Linux 包是 glibc-only**。Alpine/musl 上装下去会在运行时报 `not found`，所以要在**下载前**判掉（探 `/etc/alpine-release` 或 `ldd --version`），如实报「该环境使用 musl libc，暂不支持托管运行时」。
  - 这一步不能省：ARM 云主机（Graviton 等）与 Alpine 容器都常见，猜错的表现是"装完了但用不了"。
- **源地址可配置**（默认官方 `nodejs.org`，可切 `npmmirror`）。这不是偏好而是可用性问题：下载走**目标环境自己的网络**，受限网络下官方源常不可达。
- **必须校验 SHA256**：官方 `SHASUMS256.txt` 与分发包一同取回并比对；不匹配即中止并删除。**不允许跳过校验的开关。**
- **下载地址白名单**：只允许 `https` + 配置里的 host，且分发文件名必须匹配官方命名形态。
- **不执行任何安装脚本**：解压即用。这使本方案与"`curl | bash`"在供应链风险上完全不同，也是能在 SSH 上放开的前提。
- **版本来源与陈旧策略**（§14.D）：版本列表取自 `nodejs.org/dist/index.json`（可配镜像）并缓存；**托管运行时自己也要有"有新版"这一档**（复用同一套三态语义），否则用户的 Node 会永久停在装的那天 —— 那正是我们在 CLI 上要消灭的问题。
- 体积以实测为准并写进代码注释（压缩包与解压后各记一个数），用于进度条与磁盘占用提示。

### 7.5 远端下载失败时的兜底（§14.F）

远端可能既没有 `curl` 也没有 `wget`，或者**根本到不了 `nodejs.org`**（受限网络，镜像也未必配得对）。这时最根本的兜底是**本机下载 → SFTP 上传**：

- 项目已有 `SFTPFilesystem.WriteFile`（`filesystem.go:721`）与完整的 SSH 连接管理，机制现成；
- 比"只让用户去配镜像"更可靠，因为走的是已经建立的 SSH 通道，不依赖远端出网；
- ⚠️ 实现时要确认一处：`WriteFile` 的签名是 `content []byte`，一个 ~30 MB 的分发包会被整个读进内存。要么接受这个占用，要么给 SFTP 通道加一条流式写入 —— **这一处必须先验证再选，不要凭猜**。

## 8. 前端设计

### 8.1 页面

新建 `apps/web/src/pages/CliToolsPage.tsx` + `cli-tools.css`，路由 `/cli-tools`。

落点（4 处改动，与既有页面一致）：
| 文件 | 改动 |
| --- | --- |
| `pages/CliToolsPage.tsx` + `pages/cli-tools.css` | 新建页面与样式 |
| `App.tsx:11-27` | 加 import |
| `App.tsx:76-102` | 加 `<Route path="/cli-tools" element={<CliToolsPage />} />` |
| `pages/DashboardPage.tsx:162-169` | 加「CLI 工具」入口按钮（仿 SSH/MCP） |

样式与交互**照抄 `McpManagerPage.tsx`**：它以 `DashboardPage` 为底 + overlay 弹窗（`.ssh-manager-backdrop` / `.ssh-manager-dialog` / `.ssh-manager-toolbar`），是项目里最接近「环境依赖检查」的现成页面。Toast 用 `sonner`，确认弹窗用既有 `.backdrop` / `.modal`（`ConversationPage.tsx:596` 的更新确认弹窗可直接复用文案结构）。

### 8.2 页面结构

```
┌ CLI 工具管理 ────────────────────────────────────┐
│ Runner: [本机 Windows ▾] [WSL ▾] [SSH:prod ▾]    │   ← 多环境必须可选，不能只看本机
│ 运行时：Node.js 24.5.0 ✓  npm 10.8.2 ✓            │
│           托管（~/.local/share/milevia/toolchain）│
│           [有新版本 24.9.0]                       │
├──────────────────────────────────────────────────┤
│ ┌ Claude Code ──────────────── [已安装 2.1.216] ┐ │
│ │ Anthropic · 安装位置 .../npm-global/bin/claude│ │
│ │ [检查更新] [升级到 2.1.217]                    │ │
│ └──────────────────────────────────────────────┘ │
│ ┌ Codex ──────────────────────────── [未安装] ──┐ │
│ │ OpenAI · npm @openai/codex                    │ │
│ │ [安装]                                        │ │
│ └──────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────┘
```

必需状态（缺一个就是本项目反复踩过的错）：
- 加载中 / 通道失败（`probeOk=false`，显示具体原因）/ 真的没有 → **三态分开渲染**；
- 「未安装」与「检测不到」**绝不合并**；
- 安装/升级进行中：按钮禁用 + 阶段进度（下载 / 校验 / 解压 / 安装）+ 可查看输出尾部；
- 有新版但不能应用内升级（`autoUpdatable=false`）→ 显示「需在<环境>手动执行 `<cmd>`」，**不给必失败的按钮**（现有三态语义，`ConversationPage.tsx:591`）。

**三档"不可安装"要各给各的说法**（`installBlockedReason`）：远端未授权 → 授权说明 + 授权按钮（不是灰按钮）；运行时缺失/过低 → 「先安装 Node.js 运行时」并显示所需最低版本；架构或 libc 不支持 → 如实说明，且**不给兜底假希望**。

### 8.3 数据来源

页面数据**全部来自** `GET /api/agents` + `GET /api/runtimes/catalog` + `GET /api/runners/{id}/agents`。前端不维护任何工具名单、版本解析规则或状态优先级文案。

## 9. 安全边界

### 9.1 SSH 远端安装（已拍板全放开，因此这三件事缺一不可）

1. **逐主机显式授权**：默认 `remote_install_allowed = false`。第一次在某台 SSH runner 上装任何东西时，弹一个**指名道姓**的确认（"将在 prod-server（user@host:22）的 /home/user/.local/share/milevia 下下载并安装 Node.js 与 Claude Code"），确认后持久化，可随时撤销。
2. **一律不用 sudo**：全部落在远端家目录（托管工具链）。若远端家目录不可写 → **如实报错，不回退到 sudo**。
3. **审计**：记录 runner / 主机 / 工具 / 动作 / from→to / 结果 / 时间。这条链路会往用户的生产机器写东西，必须可回溯。

另需如实体面的限制：远端下载走远端自己的网络（§7.5 的 SFTP 兜底）、需要能解压 `.tar.xz`（缺 `tar` / `xz` 时明确报出缺的是哪一个）。

### 9.2 本机与 WSL

同样不需要提权（托管在用户目录）。WSL 侧复用现有 `wslAgentRunner` 的跨端执行通道；若 WSL 未安装则 `probeOk=false`，页面显示通道失败而不是"所有工具都未安装"。

### 9.3 手机端只读

`/api/remote/*` 这条边界已被改写三次（见 MEMORY「跨端契约」），而"安装"意味着**在目标环境执行任意 npm 包代码 + 下载解压运行时**，与现有的"项目沙箱内读写 + 该仓库 Git 操作"完全不是一个量级。**不新增任何 install / runtime 相关的远程端点**；手机端可以只读展示工具版本与运行时版本。

### 9.4 输入校验

工具 ID、Runner ID 全部查目录/注册表；版本号必须过白名单（`^[0-9A-Za-z.\-+]+$`）后才进入命令拼装 —— **禁止把请求体直接拼进 shell**（对标 `mcpRuntimeCommandPattern`，`mcp_client.go:1301`）；下载 URL 只由服务端从 `runtimes/catalog` 生成，不接受请求体传入。

### 9.5 不落任何凭据

安装过程只跑 npm 与解压；输出经 `redactAgentText` 清洗后再入库与展示。

## 10. 实施顺序

### 阶段 1：目录收口 + 前端收敛（不改行为，纯重构）
- 新增 `agent_catalog.go` + `GET /api/agents`；
- 把 §2.2 的 8 处 Go 硬编码改为查目录；
- `app.go:6846/6985` 两段重复块合并为 `probeAgents`；
- **线格式改为 `agents: AgentStatus[]`**（取代 `runner.claude` / `runner.codex`），前端约 20 处二元三元式收敛到一处查表 —— 顺序上这一步要早做，否则目录建好了前端还是一片二元三元式（§14.H）；
- 提取共享 `semver.go`，**修掉 Claude 的字符串比较**；
- 回归：既有 Claude/Codex 检测、检查更新、升级、对话链路测试全绿；同步更新被钉住的测试锚点（`conversation-layout.test.mjs:327` 等）。

### 阶段 2：路径解析器 + 托管运行时
- **把 `Config` 里的不可变路径换成注入的 resolver**（§14.B）—— 这是"装完能用"的前提，必须早于安装能力；
- `GET /api/runtimes/catalog`（版本来源 + 镜像 + 缓存）；
- arch / libc 判据（§14.C）、下载 / SHA256 校验 / 解压 / 登记；
- 三级解析顺序 + 最低版本闸门；
- SFTP 兜底与 `WriteFile` 内存占用那一处先验证（§7.5）。

### 阶段 3：安装能力 + 泛化 API
- 新增 `/api/runners/{id}/agents[...]` 与 `runtime/install` 路由；旧路由转薄适配；
- 实现 CLI `install`（复用 `npmCLIInstall` 的 prefix/shim/回滚机制）；升级按 §6.3 的两条路分流；
- 并发闸门复用 `runnerUpdating`，install 与 run admission 串行；
- 执行范围：本机 → WSL → SSH（SSH 连带 §9.1 的授权与审计）。

### 阶段 4：管理页
- `CliToolsPage` + 路由 + 入口；
- 三态列表、运行时卡（含"有新版"）、安装/升级确认弹窗与阶段进度、安装位置可复制、三档"不可安装"文案。

## 11. 已拍板决策与仍开放项

### 已拍板（2026-09-21）

| # | 问题 | 决策 |
| --- | --- | --- |
| 1 | 管理范围 | **本机 + WSL + SSH 全放开** → 连带 §9.1 的逐主机授权与审计为必做项 |
| 2 | 操作面 | **安装 + 升级**（不做卸载、不做指定版本） |
| 3 | 缺 Node / npm | **平台内一并安装** → §7 整章；形态为托管工具链（免提权、可完全撤销） |
| 4 | 手机端 | **只读展示版本**，不提供安装/升级入口 |

### 仍开放（实现时按此默认，除非另行指定）

| # | 问题 | 建议默认 |
| --- | --- | --- |
| 1 | CLI 安装通道：npm 全局 vs 官方原生安装脚本 | 先做 `npm-global`（可控、可校验、可回滚）；`native` 只在"用户已自装"时识别、不主动改用 |
| 2 | 镜像源默认值 | 默认官方源，镜像作为可选配置项 |
| 3 | Node 版本选择 | 默认 LTS，允许在页面上选（列表来自 `runtimes/catalog`） |
| 4 | 托管 Node 是否自动升级 | **不自动**，但在运行时卡上显示"有新版"并提供升级按钮（与 §7.4 一致） |

## 12. 验收与测试矩阵

### 后端（Go 测试，契约留在 Go 侧）
1. `agentCatalog()` 覆盖全部已支持工具，且**不存在第二份清单**：给校验函数一个"目录里存在、旧硬编码里没有"的 ID，应返回 true。
2. semver：新版本 > 本地 → `updateAvailable=true`；新版本 < 本地（预发布）→ **false**（这条就是当前 Claude 的 bug）。
3. **解析器顺序**：有系统 npm → 用系统 prefix；无系统 npm 有托管 → 用托管 prefix；都没有 → 返回"需要安装运行时"而**不去执行 npm**。三种情形下 `binary_path` 与 `install_kind` / `prefix` **成对**正确。
4. **解析器真的被 14 处消费**：把一个工具的 `binary_path` 写成托管路径（不在 PATH 上），断言 `Ready()` / `Version()` / `Run()` 参数 / `project_commands.go` 的命令目录探测都走它 —— 这条防的是"解析器写了但没人用"（本项目反复出现的错）。
5. 最低版本闸门：`node --version` 为 16.x 且目录要求 ≥18 → 报"运行时版本过低"，**不**静默去装托管 Node。
6. arch / libc：模拟 `aarch64` 与 musl 两种探测结果，断言选到正确分发包 / 如实拒绝。
7. 运行时安装：SHA256 不匹配 → 中止并删除已下载文件，**不进入解压**；下载中途失败 → 残留目录被清理。
8. `install`：安装成功后自检失败 → 状态 failed 且保留输出尾部。
9. 通道与状态：`probeOk=false`（WSL 不可用 / SSH 未连接）时，响应里 items 不被解读为"未安装"。
10. 远端授权：未授权时 install / runtime install 返回明确拒绝，且**不发起任何远端命令**；授权后放行；撤销后再次拒绝。
11. 并发：同一 (runner, agent) 同时发起 install 与 update → 第二个被拒；运行时安装与 CLI 安装在同机互斥；有活跃对话时 install 被拒。
12. 旧路由 `/claude/update` 与新 `/agents/claude-code/update` 行为一致（成对用例）。
13. 审计：每次远端 install/update 落一条记录，字段完整。

### 前端（node 侧结构断言 + 探针真实行为断言，两套都要）
1. 页面清单来自服务端（断言源码里**不存在**硬编码工具名单）。
2. **二元三元式收敛**：断言 `ConversationPage.tsx` 等文件里不再出现 `agentID === "codex" ?` 这种形态（**剥离注释后**判断 —— 注释里原样出现会让断言永远不可能红，本项目踩过两次），并同步更新 `conversation-layout.test.mjs:327` / `race-guards.test.mjs:79/95/96/97` / `command-picker.test.mjs:49/60` 这些钉住旧写法的锚点。
3. 三态渲染分支都存在且互斥（加载 / 通道失败 / 真空）。
4. 三档"不可安装"文案互不相同（未授权 / 运行时缺失或过低 / 架构不支持）—— 探针分别模拟，断言三句话不一样。
5. `autoUpdatable=false` 时不渲染更新按钮，渲染手动命令提示。
6. 探针：真实拉起页面，分别模拟 `probeOk=false` / `installed=false` / `runtime.installed=false` 三种响应，断言文案互不相同。

> 断言纪律（自证断言 / 成对用例 / 夹具覆盖面 / SKIPPED 与 ESCAPED 必须为 0）见 `TOOLING.md`。

## 13. 本期不做

- 不做插件化 / 第三方工具注册（方案 C 顺延）；
- 不做卸载与"安装到指定版本"（已拍板）；
- 不做镜像源自动探测（只提供配置项）；
- 不做托管 Node 的自动升级（只提示 + 手动）；
- 不做跨 Runner 批量安装/升级；
- 不在 `/api/remote/*` 暴露 install / update / runtime。

## 14. 自复查（回答"这是最佳方案吗"）

初版发出后重新逐处对源码，**推翻了 2 条自己的建议，补了 4 处漏项，简化了 1 处，调整了 1 处顺序**。
共同特征：**都是我"没读源码就下结论"造成的**（与 `docs/41 §13` 同一类错）。

### A. 推翻：`AgentID` 改成 `string` 是错的（初版 §4.1 的建议）

初版理由："目录新增工具时前端会编译期拒绝"。读了前端才发现，真正的问题不是类型太窄，而是 **约 20 处二元三元式** `agentID === "codex" ? X : Y`（清单见 §2.2），它们**默认落到 Claude**，第三个工具进来会被静默标成 "Claude Code"。

放宽成 `string` **一处都修不好**，只会把"编译报错"变成"编译通过但标签是错的" —— 把问题藏起来。而且这些三元式被测试逐字钉住（`conversation-layout.test.mjs:327` 等），联合类型反而是"这里还没适配"的信号。

**改成**：union 保留；修的是线格式（`agents[]` 取代 `runner.claude` / `runner.codex` —— `docs/13 §8.1` 早就提过、一直没做）与那约 20 处的收敛。

> 教训：**"这个类型太窄了"要先确认病因是类型还是用法。** 放宽类型永远能让编译通过，所以它天然是个"看起来解决了"的假动作。

### B. 补漏（最实的一条）：`Config` 按值复制，路径运行时改不了

初版写了"三级解析顺序"，但没说它要落在哪里。读了调用点才发现：`Config.ClaudePath` / `CodexPath` 被**直接读 14 处**（`claude_runner.go:310/315/323/375/378/527/613`、`codex_runner.go:93/100/248/252/340/740`、`mcp_import.go:348/612`、`project_commands.go:280`、`wsl_agent_run.go:134/347`），而 `newClaudeCLIRunner(config Config)` 是**按值传入**、runner 只在启动时构造一次。

⇒ 照初版实施，装到托管 prefix 之后 `Ready()` / `Version()` / `Run()` / 命令目录 / MCP 导入**全都还在用旧路径**，装完等于没装，项目可用性判定（`project_availability.go`、`agent_target_env.go`）也会一起变红。

**改成**：把不可变字段换成注入的 resolver（`Path(agentID string) (string, bool)`），并把"解析器真的被那 14 处消费"写成一条测试（§12.3 / §12.4）。这也是把解析器提到阶段 2 的原因。

### C. 补漏：远端分发包要按 arch / libc 选

初版只写了"各平台分发包"。实际：**官方 Node 的 Linux 包是 glibc-only**，Alpine/musl 装下去会在运行时报 `not found`；ARM 云主机（Graviton 等）要 `linux-arm64`。判错的表现是"装完了但用不了"，而用户拿到的是成功提示。

**改成**：下载前先取 `uname -m` 与 libc 判据，选不到就**如实拒绝**（§7.4）。

### D. 补漏：托管 Node 自己也会过期 + 缺最低版本闸门

初版说"默认 LTS"，但没定**版本来源**与**陈旧策略** —— 硬编码一个版本，等于复制我们正要消灭的"版本停在装的那天"这个问题。另有一处：初版写"有 npm 就用"，但 Claude Code 要求 Node ≥18，用户机器上是 Node 16 时装了也跑不起来。

**改成**：版本列表取自 `nodejs.org/dist/index.json`（可配镜像 + 缓存）；托管运行时同样有"有新版"这一档；加 `MinRuntimeVersion` 闸门，且**不满足时不静默回落**到托管安装（那会让用户机器上出现两套 Node 而没人知道）。

### E. 简化：升级不必依赖 CLI 自带的 `update`（值得做的减法）

初版最大的不确定性是"托管 prefix 装出来的包，`claude update` 行为未知"，为此安排了三端 POC。

关键在于：**这个不确定性是我们自己造出来的** —— 布局是我们定的，为什么还要让 CLI 自己去猜它装在哪？按 `install_kind` 分流后（§6.3），托管安装的升级就是 `npm install -g @latest` 再自检，**install 与 update 合成同一条代码路径**，那条 POC 在托管路径上直接不存在了。`claude update` 只留给用户自装的情况，失败也只是降级。

> 教训：**遇到"行为未知、要 POC"时，先问一句"这个未知是我引入的吗"。** 如果是自己引入的，往往有一条直接把未知消掉的路。

### F. 补漏：远端到不了 nodejs.org 时的兜底

初版只给了"配镜像源"。但远端可能连 `curl` / `wget` 都没有，或者有却出不了网。项目已有 `SFTPFilesystem.WriteFile`（`filesystem.go:721`）⇒ **本机下载 + SFTP 上传**是现成可用的兜底，且不依赖远端出网，比镜像更根本。

⚠️ 但有一处**不许凭猜**：`WriteFile` 的签名是 `content []byte`，~30 MB 的分发包会被整块读进内存。要么接受，要么给 SFTP 加流式写入 —— 先验证再定。

### G. 一处判断：运行时探测不能进 `/api/runners` 热路径

`listRunners`（`app.go:6839`）已经对每个 runner 起一次 `claude --version`，而 `ConversationPage.tsx:543` 在更新中每 5s 轮询 `/api/runners`。再塞 node/npm 探测 = 每次多两个子进程。**改成**：运行时探测只放在 `GET /api/runners/{id}/agents`（按需）。

### H. 顺序调整：前端收敛要提到阶段 1

初版把前端收敛放在阶段 4（管理页）。但阶段 1 的目标是"让加第三个工具有地方可加"，而前端那约 20 处二元三元式如果不先收敛，目录建好了也还是加不进去。**改成**：线格式 + 查表收敛放进阶段 1。

### 结论

**不是最佳方案的那一版已经修掉了。** 骨架（工具目录 + 托管运行时 + 复用既有闸门与 npm 回滚机制）经复查仍然成立；改动集中在四处：解析层（B）、平台判据（C）、运行时版本策略（D）、升级路径简化（E）；另撤回一条错误建议（A）、调整一处顺序（H）、加一处边界（G）、补一条兜底（F）。

## 15. 实施状态（2026-09-21）

### 15.1 已完成：阶段 1（目录收口 + 前端收敛）

**这一阶段不改任何行为**（除了下面记的两处有意变更），目标是把"支持哪些工具"这个事实
收成一份，让后续阶段有地方可加。

**新增文件**

| 文件 | 作用 |
| --- | --- |
| `internal/app/agent_catalog.go` | **唯一工具目录** + `GET /api/agents` |
| `internal/app/semver.go` | 共享语义版本比较（从 codex_runner.go 提取） |
| `internal/app/agent_probe.go` | 统一的逐工具探测（合并两段重复块） |
| `internal/app/agent_routes.go` | 泛化 per-agent 路由 + 四个旧 handler 的薄委托 |
| `web/src/lib/agent-registry.ts` | 前端唯一的工具目录来源与取值入口 |

**收口掉的重复**

- Go 侧 8 处硬编码清单 → 全部改查目录（`validProfileAgent`、`agent_profiles.go` 的循环、
  `mcp_config.go`、`mcp_servers.go`、`git_conflict_suggest.go`、`insights.go`、`app.go` 两段探测块）。
- `listRunners` 与 `runnerStatus` 里 **123 行近乎逐行重复的 claude/codex 探测块 → 16 行**
  （两处都改为调用 `probeAgents` + 由 `agents[]` 派生过渡字段）。顺带消灭了一处已经漂移的文案
  （同一条件在两处分别写着"本机 Codex CLI 未安装或未登录"与"本地…"）。
- 前端 **约 20 处 `agentID === "codex" ? X : Y` 二元三元式 → 0**，覆盖
  `ConversationPage` / `AgentProfilesPage` / `ScheduledTasksPage` / `OrchestrationPage` /
  `ProjectAiConfigDialog` / `InsightsPanel` 六个文件。这类写法的默认分支永远落在 Claude 上，
  新增工具会被静默标成 "Claude Code"。

**两处有意的行为变更**

1. **状态新增一档 `unsupported`**。原先把"这个 Runner 根本不提供该工具"与"提供了但没装"
   压成同一档 `unavailable`，而两者的下一步动作完全不同（换环境 vs 去安装）。现在状态码与
   一直存在的那句 reason（"此 Runner 不支持 Codex"）终于说的是同一件事。
   连带更新：`types.ts` 的 `ToolStatusKind`、`conversation.css` / `style.css` 的中性灰样式
   （新增工具不该看起来就是 Claude），以及 `app_test.go` 里钉住旧行为的用例（已改名并加注释）。
2. **Claude 的更新判据从字符串不等改为 semver**（`claude_runner.go`）。原写法
   `latest != local` 在本地是预发布版时会把**降级**报成"有更新可用"。

**新增防线（各配了能挡住它的变异）**

| 防线 | 变异检验 |
| --- | --- |
| `validProfileAgent` 真的读目录 | 写回 `id == "claude-code" \|\| id == "codex"` → 红 |
| Claude 走 semver 比较 | 写回 `latest != local` → 行为级 + 接线级**两条**同时红 |
| `probeAgents` 分档、就绪判据来自目录、维护状态优先 | 见 `agent_probe_test.go` |
| 泛化路由与旧路由逐字一致（成对用例） | 见 `agent_routes_test.go` |
| 前端危险形状清零（剥注释后判断） | 把 `agentDisplayName(x)` 写回三元式 → 红 |

**验证**：control-server `go build` / `go vet` 干净；前端 `tsc -b` 0、`vite build` 通过、
单测 **643/643**（原 637，新增 6 条）；新增的 Go 用例全绿。

### 15.2 已知例外：手机端读不到工具目录

`features/git/ConflictSolveView.tsx` 与 `pages/MobileRemotePage.tsx` 是**桌面与手机共用**的
组件，而手机端走的是云端 `/api/remote/*`，**拿不到控制服务的 `GET /api/agents`**。

因此这两处暂时保留按工具 ID 的小清单（已就地写明原因，并有测试断言"例外说明存在"）。
强行改成读目录会让手机端把工具显示成裸 id（`claude-code`），那是比现在更差的退化。

**正确修法**（需单独排期，属 REMOTE-CONTRACT 变更）：把工具目录放进手机快照
（`remote_control.go` 下发的快照里已有项目与会话元数据），或让云端转发该端点。
修完之后这两处可以一并收敛。

### 15.3 顺带查到的既有问题（未修，仅登记）

`apps/web/src/pages/ConversationPage.tsx` 等一批前端文件是 **CRLF 行尾**，而 `.gitattributes`
要求 `* text=auto eol=lf`。本项目此前记录过 CRLF 会**静默破坏 web 测试里的 `\n` 锚点**
（2026-09-12 为此排查过三轮）。本次补丁脚本按文件**现有**行尾写回，没有顺手归一 ——
归一会产生整文件重写的 diff，与本次改动无关，应单独一轮处理。

另：`internal/app/{state_events.go, task.go, insights_test.go, insights_fix_test.go}` 的
`gofmt -l` 不干净，是**改动前就存在**的（git 里未修改），同样留给单独一轮。

### 15.4 剩余：阶段 2 / 3 / 4

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 2 | 路径解析器（resolver）+ `agent_installations` 表；托管 Node 运行时（版本目录、arch/libc 判据、下载/SHA256/解压、镜像、SFTP 兜底） | 未开始 |
| 3 | 安装能力（npm 全局 + 自检 + 回滚）、`/runtime/install`、并发闸门扩展、SSH 逐主机授权与审计 | 未开始 |
| 4 | `CliToolsPage` 管理页（三态列表、运行时卡、阶段进度、三档不可安装文案） | 未开始 |

阶段 2 的**前置**是 §14.B 那条：`Config` 按值复制、路径被直接读 14 处，不先把路径换成
注入的 resolver，装完之后 `Ready()` / `Version()` / `Run()` 仍会用旧路径 —— 等于装了个没用的。

## 16. 实施状态（2026-09-21，阶段 2/3/4）

### 16.1 阶段 2：路径解析器 + 托管 Node 运行时（已完成，本机）

**路径解析器**（`agent_paths.go`）

- `Config.ClaudePath` / `CodexPath` 从"被直接读的不可变字段"改成**注入的解析器**，
  18 处读取全部经它。解析顺序：环境覆盖 > 登记表（实测可用）> PATH 查找 > 平台兜底候选。
- `ConfigFromEnv` 里那段 Windows 兜底**已退役**：它把"探测到的路径"写进 `Config`，
  从值上看起来与用户显式指定无法区分，会盖掉登记表里的实测路径。平台兜底现在由解析器
  统一处理，**两个工具一视同仁**（原先只给 Codex 写了）。
- 新增 `agent_installations` 表（`binary_path` 与 `install_kind`/`prefix` **成对记录**）。

**托管运行时**（`runtime_distribution.go` / `runtime_manager.go` / `runtime_install.go`）

⚠️ **抓官方数据时推翻了初版的三条判断**，三条都已就地更正：

| 初版判断 | 实际情况（2026-09-21 实际抓取） | 影响 |
| --- | --- | --- |
| "官方 Node Linux 包是 glibc-only，Alpine 只能如实拒绝" | **有 `linux-x64-musl` 官方构建** | 不必拒绝；只有"该版本没有 musl 包"（较老版本）才拒绝 |
| "远端需要能解压 `.tar.xz`，缺 tar/xz 要报错" | **同时提供 `.tar.gz`** | 选 gzip：纯 Go 解压，目标环境**不需要任何额外工具**，这类失败消失了 |
| 需要引入第三方 xz 库或调系统 tar | 不用 | 全链路只用 Go 标准库 |

- 文件名**以 `SHASUMS256.txt` 为准**，不自己拼：逻辑键与文件名并不一致
  （`linux-x64` → `…-linux-x64.tar.gz`，但 `win-x64-zip` → `…-win-x64.zip`）。
- 下载边写边算 SHA256，不匹配即**删除**已下载文件；没有"跳过校验"的开关。
- 解压拒绝一切逃逸条目（`..`、绝对路径、盘符、反斜杠变体），逃逸符号链接跳过。
  ⚠️ 这里修了一个**平台相关的判据**：`filepath.IsAbs("/etc/passwd")` 在 Windows 上返回
  false，于是同一条链接会在 Linux 被拒、在 Windows 被放行 —— 判据跟平台走，安全结论就
  跟平台走。现在显式挡绝对形态。
- ⚠️ **Windows 与 Linux 的包内布局不同**：Windows 的 zip 把 `node.exe`/`npm.cmd` 放在
  **顶层**，Linux 的 tar.gz 放在 `bin/` 下（且 `bin/npm` 是符号链接）。这一点是测试抓出来的
  —— 猜成同一种布局的表现是"解压成功但找不到 node"。
- 落点可用 `AUTO_TOOLCHAIN_ROOT` 覆盖（两个理由都不是预留：用户换盘、以及让整条链路能
  在临时目录里端到端跑测试）。

### 16.2 阶段 3：安装能力 + 泛化 API（已完成，本机）

- `GET /api/runners/{id}/agents`：运行时 + 每个工具的状态汇总，**三档分开表达**
  （`probeOk=false` / 真的未安装 / 已安装）。探测失败时 **`items` 为空**，
  而不是给出一堆 `installed=false`（那会被渲染成"所有工具都没装"）。
- `POST /api/runners/{id}/agents/{agentID}/install`：npm 全局安装。
- **升级按 `install_kind` 分流**（§6.3 的落地）：平台/系统 npm 装的走
  `npm install -g @latest`（与安装同一条代码路径），只有用户自己用官方安装器装的才调
  CLI 自带的 `update`。
- 并发闸门抽成 `beginAgentMaintenance`，**安装与升级共用**（原先只有 upgrade 有，
  安装若各写一份就一定会有一条路忘记检查活跃会话）。
- 逐主机授权（`runner_install_grants`）与审计（`agent_install_audit`）已实现；
  未授权时服务端显式下发 `needsGrant`，页面据此给出授权入口 —— **不靠判理由文案里有没有
  "授权"两个字**（靠文案判断的判据改一次文案就静默失效）。

**两处"装完真的能用"的修正**（都是测试抓出来的）：

1. 自检原先会回落到 PATH 查找产物 —— 那会把**用户自己那份 CLI** 认成我们的安装，
   于是把别人的路径登记成我们的安装位置，之后升级会去升级那一份。
   现在：装到托管 prefix 时**只在 prefix 里找**。
2. 登记版本没有剥产品名后缀（`2.1.217 (Claude Code)`），而 runner 的 `Version()` 会剥。
   同一个版本在两处呈现不同 → "当前 X → 最新 Y"的比较会在两处得出不同结论。
   现在统一走目录里的 `VersionTrim`。

### 16.3 阶段 4：管理页（已完成）

`apps/web/src/pages/CliToolsPage.tsx` + `cli-tools.css`，路由 `/cli-tools`，
首页入口在 `DashboardPage` 的 `dashboard-actions` 区。

页面数据**全部来自服务端**（`GET /api/runners`、`GET /api/runners/{id}/agents`、
`GET /api/runtimes/catalog`、`GET /api/agents`）。三档状态分开渲染；"不可安装"三档
文案各不相同；`autoUpdatable=false` 时给手动命令、不给点了必失败的按钮。

### 16.4 跨端（WSL / SSH）的实际执行（第一轮未做，第二轮补齐，见 §17）

已拍板的范围是"本机 + WSL + SSH 全放开"，**但这一块没有实现**：

- `POST …/runtime/install` 与 `POST …/agents/{id}/install` 在非本机 runner 上
  **如实返回 501**（而不是假装成功）。
- 授权与审计的机制**已经就位**（逐主机授权、审计落库、界面授权入口），
  缺的是"在目标环境里下载/解压/执行 npm"这一层执行通道。
- 也就是说：**SSH 上的安装目前走不通**。要接着做的话，入口是：
  - WSL：复用 `wslAgentRunner` 的 `wsl.exe --` 通道；
  - SSH：复用 `sshRunner` 的 exec，并且按 §7.5 做"本机下载 → SFTP 上传"的兜底
    （注意 `SFTPFilesystem.WriteFile` 的签名是 `content []byte`，~30MB 包会整块进内存，
    这一处要先验证再选）。
- 原方案里"远端缺 tar/xz"那一类失败**已经消失**：现在用的是 `.tar.gz`，且解压是纯 Go 的。

### 16.5 验证

| 项 | 结果 |
| --- | --- |
| control-server `go build` / `go vet` | 干净 |
| 覆盖本次改动的定向 Go 用例（50 个前缀，66s） | 全绿 |
| 前端 `tsc -b` | 0 |
| 前端单测 | **650/650**（阶段 1 后是 643，本轮 +7） |
| `vite build` | 通过 |
| 变异检验 | 抽出路径解析器后改回读 `Config` → 红；去掉解压的 `..` 拒绝 → 红（"escaped.txt 被写到了托管目录之外"）。两次都按 sha1 逐字节还原 |

**夹具用的是真实官方数据**（`SHASUMS256.txt` 与 `index.json` 的实际片段，2026-09-21 抓取），
不是按印象编的 —— 这一层的判据全部是关于"官方长什么样"，凭印象写等于把猜测固化成测试。

### 16.6 顺带记下的两个实施期教训

- **别在双引号包起来的 shell 命令里写反引号**：本轮又踩两次（`x === \`claude-code\``、
  `${encodeURIComponent(...)}` 被 shell 当命令替换执行），两次都表现为"脚本语法错误"或
  "写入的内容被吃掉一段"。写脚本文件再跑。
- **CRLF 会让多行 `old_string` 一律匹配失败**（表现为"命中 0 次"）。补丁脚本必须先探测
  文件行尾再替换。



## 17. 跨端执行通道（第二轮，2026-09-21）

§16.4 记的那块缺口已补齐。这一节记的是**怎么补的**与**取舍在哪**——凡是当初写
"未实现"的地方，都因为一个具体的技术判断而变成了可实现。

### 17.1 分工：下载在校验点这一侧，解压与落点在目标环境

| 步骤 | 在哪做 | 为什么 |
| --- | --- | --- |
| 抓 SHASUMS / index.json | 本机 | 完整性判据不该交给被安装的那台机器 |
| 下载 + SHA256 校验 | 本机 | 目标环境可能连 `sha256sum` 都没有 |
| 解压 | **目标环境** | 包里的 node 是给**那边**的平台编译的：在 Windows 上给 WSL 装，本机解压出来的东西一行都跑不了 |
| 落点 / npm 全局安装 | **目标环境** | 二进制必须落在它会执行的地方 |

这条分工把初版担心的"远端缺 tar/xz"缩小成了一个可操作的判断：只需要目标环境有
`tar -xzf`。缺了就**如实报错并说清缺什么**，不退化成"本机解压后逐个文件上传"——
那是几千个文件，慢到用户会以为卡死。

### 17.2 WSL 的零传输：`C:\a\b` 就是 `/mnt/c/a/b`

WSL 让 30 MB 的压缩包**不需要传输**：本机下载完，WSL 直接从 `/mnt/<盘符>` 读。

两个必须做的检查，合起来才是"零传输"成立的前提：

- **该盘符确实挂载可读**。WSL 可以配 `automount=false`，那时 `/mnt/c` 根本不存在。
  照直给出路径的症状是 tar 报"找不到文件"——一个**指不到真正原因**的报错。
  所以探测脚本会把可读的 `/mnt/*` 列出来，未探测前 `sharedPath` 一律返回不可用。
- **收尾时不能删这个压缩包**。它在**本机磁盘**上，目标环境里的 `rm` 删的是本机文件，
  而用户完全看不出是谁删的。脚本生成时按"共享/上传"分流，共享那条**不生成清理语句**。
  `TestCrossRuntimeInstallScriptNeverDeletesSharedArchive` 钉住它，变异检验确认有效。

SSH 没有这条路，只能上传。**不用 `SFTPFilesystem.WriteFile`**：它绑着项目沙箱
rootPath（工具链要落在 `$HOME`，不在任何项目里），签名还要求编辑语义的
`expectedVersion`。直接用 `sftp.Client` 流式拷——顺带把 §7.5 记的那个顾虑
（"`WriteFile` 收 `[]byte`，30 MB 会整块进内存"）一起消掉了：现在根本不进内存。

### 17.3 三处"目标环境与执行本进程的平台不同"的坑

这一层每一条都是实测出来的，不是推的：

1. **布局按目标平台，不按本机**。`nodeHomeBinary` 是按**本机** GOOS 判断布局的
   （Windows 的 zip 把 `node.exe` 放顶层，Linux 的 tar.gz 放 `bin/`）。跨端直接复用
   会指错形状，所以跨端走自己的 `cross*` 函数，固定 `bin/`。
2. **不用 `tar --strip-components`**。busybox 的 tar 没有这个选项，而 musl 发行版
   正是 busybox。改成解压后用 POSIX 循环数顶层目录，**数量不等于 1 就如实报错**——
   包结构变了这件事必须被说出来，不能猜。
3. **执行 npm 前要把它的 bin 目录前置到 PATH**。`npm` 与各家 CLI 都是
   `#!/usr/bin/env node` 的脚本，PATH 里没有 node 时它们报的是
   `env: node: No such file or directory`——**与"没装这个命令"在文案上几乎一样**，
   会把用户引到错的方向。

### 17.4 修掉一个真的漏洞：授权闸门只在界面上，不在服务端

第一轮的 `remoteInstallAllowed` **只有列表端点在用**：未授权时界面不给按钮，
但 `POST …/install` 与 `POST …/runtime/install` 直调是能过的。

也就是说"逐主机授权"当时只是一句界面文案——任何能调到本地 API 的东西都能往别人的
机器上装东西。现在两个入口都在**服务端**拒绝（403），并补了测试
（`TestCrossInstallEndpointsRequireGrant`）。

升级那一档的边界单独定：**平台装的（登记为 npm 类）**才走 npm 重装，因此要过同一道
闸门；用户用官方安装器装的仍走 CLI 自带的 `update`，属于本次改动之前就有的行为，
不顺手改掉（`TestCrossUpdateNpmInstallIsGrantScoped` 钉住三档）。

### 17.5 界面跟着一起收手

后端拒绝之后，界面就不能再亮出那些按钮了——否则正是项目记忆里"服务端判了不能改、
界面照旧亮出按钮"那一族错。三处改动：

- 未授权时，运行时区**只给一个动作：授权**（安装/升级按钮都不渲染）；
- 工具卡的升级按钮多一个 `upgradeNeedsGrant` 条件；
- `upgradeNeedsGrant` 由**服务端下发**，不是前端自己拼 `installKindUsed` 字符串——
  靠常量/文案判断的判据改一次就静默失效。

顺带把 `listAgentInstallations` 的读取从"只读本机"放开到两端：跨端也有登记项，而
"这份工具是怎么装上的"决定升级走哪条路、要不要过闸门，界面必须知道。

### 17.6 验证

| 项 | 结果 |
| --- | --- |
| `go build` / `go vet` | 干净 |
| 跨端定向 Go 用例（60 个前缀，73s） | 全绿 |
| 前端 `tsc -b` | 0 |
| 前端单测 | **651/651**（上一轮 650，+1） |
| `vite build` | 通过 |
| 变异检验 ①（共享压缩包不清理的抑制去掉） | **红**：`共享（/mnt）路径的压缩包在本机磁盘上，绝不能在目标环境里 rm 它` |
| 变异检验 ②（删掉"盘符未挂载"判断） | **红**：`D 盘没挂载时不该给出共享路径` |

两次都按 sha1 逐字节还原。变异检验 ② 第一次做的是一条**等价变换**（删 `nil` 判断，
而 nil map 读本就返回零值），测试没红 —— 那次不是漏网，是变异本身无效；换成真正
改变行为的那一条之后立刻变红。**记在这里：变异检验报"没红"时，先确认变异真的
改变了行为，再怀疑断言。**

夹具仍然全部照着真实形状造：探测脚本的输出用一台 Ubuntu 与一台 Alpine 的实际字段，
分发包用真名（含"逻辑键与文件名不一致"这件事）。

### 17.7 仍未做

- **远端直下**：目标环境有 `curl`/`wget` 时从那边直接下载，可以省一次上传。
  目前一律"本机下载 + 就位"，校验点因此永远在本机（这是有意的，不是遗漏）；
  远端直下要远端自己算 sha256，判据会移到不能信任的那一侧。
- **托管 Node 的自动升级**：只提示"有新版"，不自动动（§13 已定）。
- **WSL→Windows 这条方向**（控制服务跑在 WSL 里、管理 Windows 侧的 CLI）：托管工具链那套
  落点与执行通道都是围绕"Linux 目标环境"做的，而这条方向的目标恰好是 Windows。它如实报
  "不支持应用内升级"（界面因此给"需手动更新"），报错文案也改成"平台不为这条方向提供安装或
  升级"——不是笼统的"尚未就绪"，后者会被读成"以后会有"，让用户白白等。

### 17.8 收尾：跨端升级，以及"能不能自动升级"这一个判据

§17.4–§17.6 做完之后重新对了一遍源码，抓到一处**我自己制造的**不一致。

跨端**安装**做通了，但 WSL 侧仍然：

- `AutoUpdateSupported()` 返回 `false`，`Update()` 直接报"尚未就绪"；
- 而管理页的 `AutoUpdatable` 是**硬编码 `true`**。

于是同一个事实（这台机器上的这个工具能不能应用内升级）有两处判据、结论相反：
管理页给"升级"按钮，对话页说"需手动更新"。前者点下去必然失败（`Update` 报错），
后者在一个已经能升级的环境上说不能 —— 两处都错。

值得注意的是这一处**不是我漏写，而是我漏改**：`false` 是"跨端升级尚未就绪"时代的
正确值，我把那个时代结束了，却没回头改它的读数。**记在这里：把某个能力从"没有"改成
"有"时，要顺着"谁在读这个能力"往回找一遍。**

四处改动：

1. **编排抽成两端共用**（`cross_npm_update.go` 的 `runCrossCLIUpdate`）：确认来源 →
   执行 `<cli> update` → 健康检查 → 必要时回滚。SSH 与 WSL 的差别只有"怎么在那边跑命令"，
   所以只留一个 `run` 参数。SSH 侧原有的 `prepareRemoteNpmCLIRecovery` /
   `finishRemoteNpmUpdate` 两个方法被它取代（`remoteNpmCLIRecovery` 类型与
   `remoteNpmRollbackCommand` 留在原地，后者被 `codex_runner_test.go` 直接测着）。
2. **WSL 侧实现 `Update` / `CodexUpdate`**（`wsl_agent_update.go`）：在 WSL 内跑
   `<cli> update`。这里也解释了"两条升级路都要留"：平台装的走 npm 重装（布局是我们定的，
   确定性最高），用户自己装的只能让 CLI 自己去更新（我们不知道它当初怎么装的）。
3. **能力标记改正**：WSL 报 `true`。这不是"提高一个开关"，而是把读数改回事实。
4. **判据收成一处**（`agentAutoUpdatable`）：管理页不再硬编码 `true`，与
   `checkAgentUpdate` 用同一个函数。断言是行为级的：同一个 runner 上，
   `POST …/check-update` 与管理页的 `items[].autoUpdatable` **必须一致**
   （`TestAutoUpdatableIsOneJudgementForBothEndpoints`）。

`Update` 的三条纪律也一并钉住了（`cross_update_test.go`）：

- **确认来源在动它之前**：先跑 `npm prefix -g` + `readlink` 比对，认不出就不回滚；
- **命令报错 ≠ 工具坏了**：健康检查还能读出版本就不回滚 —— 否则一次网络抖动的 update
  失败会把一个可用的版本换掉，用户看到的是"升级失败，而且版本还变了"；
- **没装就不做无谓探测**：版本为空时一条脚本都不发。

变异检验：把"工具还能用就不回滚"那条判断去掉 ⇒ `TestRunCrossCLIUpdateDoesNotRollbackWhileToolStillWorks`
**红**（"工具还能用，不该发出回滚脚本"），按 sha1 逐字节还原。

## 18. 复查（2026-09-21）

对全部新增/修改代码做了一次复查。方法是**两路独立审查**（一路看 Go 侧的安装与跨端链路、
一路看前端与前后端契约）加自己逐条复核源码 —— 独立视角的价值在这一轮很明显：报上来的
问题里，多数是**我自己写完还看过一遍也没看出来**的。

### 18.1 后端

| # | 问题 | 后果 |
| --- | --- | --- |
| G-1 | 运行时安装脚本开头 `rm -rf "$staging"`，而 SSH 上传的压缩包**就落在 staging 里** | SSH 上"安装 Node 运行时"必然失败（tar 报找不到文件），而界面上是个可点的按钮 |
| G-2 | Claude 的版本比较在 SSH 与 WSL 两侧仍是 `latest != local` 字符串不等 | 装了预发布版的用户会被报"有更新"，点下去是**降级**；同一台机器上 Claude 与 Codex 各用一套判据 |
| G-3 | `installRuntime` 自占一个 `runnerUpdating[node]` 槽，与 `beginAgentMaintenance` 那把锁互不相见 | "装 Node"与"装 CLI"能同时开跑：两个 npm 写同一个 prefix，且到位动作（`mv node`）会把 CLI 正在用的 node 换掉 |
| G-4 | 登记表读取一律 `if err == nil`，把 SQL 出错与 `sql.ErrNoRows` 混成一个分支 | 一份装在系统 npm 全局里的工具会被当成"没装过"，升级时**另装一份** —— 正是 `resolveAgentInstallPlan` 里明令禁止的事 |
| G-5 | 跨端"用哪个 npm"与"装到哪个 prefix"各判一次 | 登记为系统 npm 的会被拿托管 npm 装到托管 prefix；闸门也量错了那套 node（系统 Node 14 会被放行） |
| G-6 | `agentUnavailableReason` 把 WSL 说成"远程服务器" | 对着一台本机 WSL 说"远程服务器上未安装"，用户会跑去检查网络 |

另清理了 6 处死代码（`stageInto`、`crossAgentCommandPath`、`runnerAgentBinaryPath`、
`recordedPaths`、`forgetAgentInstallation`、`probeRuntimeCross` 的未用参数、以及探测出来
却没有任何消费者的 `Downloader` / `Checksum`），并把探测脚本里的"mkdir 探可写性"去掉 ——
探测由列表端点触发，发生在**任何授权之前**，不该在那台机器上留下任何东西。

### 18.2 前端

| # | 问题 | 后果 |
| --- | --- | --- |
| F-1 | 目录读取失败被渲染成"正在读取工具目录…" | 永转圈。`agent-registry.ts` 明明备好了 `loaded` / `error`，页面只取了 `entries` —— **我自己写的红线，自己犯了** |
| F-2 | 用 `runtime.installed` 重判"能不能装"，忽略 `meetsMinimumFor` / `npmVersion` | Node 16（装了但太低）与 npm 缺失两种情况都亮出"安装"，服务端闸门会拒 |
| F-3 | 结构断言把 F-1 钉死了（`\{catalog.length === 0` 是正向断言） | 缺陷被测试保护着 |
| F-4 | 切执行环境时旧响应可能盖掉新状态；`checking` / `updates` 只按 agent 索引 | A 的状态挂在 B 的标签下；A 的"发现新版本"带到 B 的同名工具卡片上 |
| F-5 | `operation === "running"` 只用于文案，按钮照旧亮出 | 点了返回 409 |
| F-6 | 运行时卡片与每个工具卡各一个"允许在此主机安装" | 未授权的机器上出现 N+1 个同义按钮 |
| F-7 | "需在目标环境手动执行 update"与"升级需要先授权"可同时出现 | 两条互斥的指引同时摆在用户面前 |
| F-8 | `OrchestrationPage` 的"执行 Agent"下拉残留写死的 `<option value="codex">` | 目录已含 codex 时**出现两个 Codex** |
| F-9 | `SettingsPage` 的"默认 Agent"下拉写死两项 | 目录里新增的工具根本选不到（第一轮收敛漏掉的第 8 个位置） |

修法是"把判据还回服务的提供方"：目录三态用 `useAgentCatalogState()`；能不能装由
`canInstallAgent` 一处判定（全部消费服务端字段）；授权入口只留一处；提示按互斥条件分流；
切环境加请求代际。

### 18.3 复核为真但**不改**的两处

- **`ConflictSolveView.tsx` 里写死的两个选项**：这是桌面与手机**共用**的组件，而手机端读不到
  `GET /api/agents`（它走云端 `/api/remote/*`）。改成读目录会让手机端显示成空 ——
  正确修法是把工具目录放进手机快照，那是 REMOTE-CONTRACT 变更，得单独排期（§16 已登记）。
- **`ConversationPage` 里 6 处 `conversation?.agentId || "claude-code"`**：这是"早于多工具支持的
  老会话没有 agentId"的兼容默认，不是"工具清单写死" —— 那些会话确实是 Claude 建的，
  默认落到 claude-code 是**正确**的历史事实。

### 18.4 验证

| 项 | 结果 |
| --- | --- |
| `go build` / `go vet` | 干净 |
| Go 定向用例（63 个前缀） | 全绿 |
| 新增回归测试 | 5 条（运行时压缩包落点、WSL Claude 用 semver、登记读失败与缺失分开、闸门同源、压缩包不落 staging） |
| 前端 `tsc -b` / 单测 / `vite build` | 0 / **653/653**（+2）/ 通过 |
| 变异检验 | 把"登记读失败"重新当成"没有登记" ⇒ `TestRecordedInstallationSeparatesFailureFromAbsent` **红**，按 sha1 逐字节还原 |

新增的测试都落在**行为**上而不是字面量上（例如 WSL 那条：注入假探测让本地版本是
`2.1.218-beta.1`、假 npm 报 `2.1.217`，断言"不报有更新" —— 字符串比较会让它红）。

## 19. 第二轮复查（2026-09-21）

§18 修完之后又过了一遍，这次换的视角是：**审修复本身**，以及审上一轮没覆盖的维度
（错误路径、超时、审计、状态机、资源）。报上来一条**严重**的 —— 它意味着一个功能
**从来没有成功过**。

### 19.1 严重：生成的脚本用了还没赋值的变量

`crossAgentInstallScript` 生成的脚本里，`prefix=` 的赋值**排在 install 之后**，而 install
那一行就要用 `--prefix "$prefix"`。脚本开头是 `set -eu` —— 引用未赋值变量会直接退出。

⇒ **WSL / SSH 上安装或升级任何 CLI，100% 失败**；用户看到的只是
"安装失败：exit status 1"，完全指不到原因。

它是怎么躲过前面几轮的：`fakeCrossEnvironment` 按关键字返回预设输出、**不真跑脚本**；
结构断言又只检查"某几行在不在"，**不看顺序**。这正是项目纪律里"探针要有真实行为断言
（挡'只有接线、行为是错的'）"缺位的形态 —— 已记进 `TOOLING.md`。

修法与断言：把 `prefix=` 提到 install 之前；断言落在**行序**上（`prefix=` 的行号必须小于
install 的行号）。变异检验：把它移回去 ⇒ 红。

### 19.2 其余（已修）

| # | 问题 | 后果 |
| --- | --- | --- |
| A | **装运行时与"升级"都没有审计**（只有 install 有） | 往别人的机器上落一整套运行时却不留痕 —— 违反 §9.1 要求的三件事之一 |
| B | `installRuntime` 用 `r.Context()` 且**无超时** | 关页面就取消远端安装（在那边留下半装文件）；一直挂着则占住闸门十几分钟 |
| C | `runtimeStatusFor` 在没有跨端通道时返回 `nil` | JSON 成 `null`，界面按 `runtime?.installed` 渲染成"未安装" —— **把读不到写成没有**（红线） |
| D | `beginAgentMaintenance` 的活跃检查按工具键判 | 用它跑运行时那一档（agentID=`node`）永远命中不到 ⇒ 对话正跑着也能装运行时，而 `mv node` 会在会话底下把它换掉 |
| E | 界面的"够不够装"与服务端闸门**各判一次** | 登记为系统 npm 装的工具、机器上后来又装了托管 Node 时：界面按托管那套算 → 亮出升级按钮 → 点下去被拒 |
| F | 运行时装好了但 npm 不可用 → 没有重装入口 | 界面写着"请重装"，却无处可点（而这一档是**预期会发生**的） |
| G | 审计里 install 的 from/to 填了同一个值；审计写入失败把**已成功**的安装报成"失败" | 看不出旧版本；用户以为失败而重装一遍 |
| H | `needsGrant` 服务端下发但前端已无消费点 | 一个"假装存在的边界"（纪律：服务端算了就要有渲染点，否则删掉） |

E 的修法值得单独说：把"这次该用哪套运行时"收成**一个纯函数**
（`cross_install_target.go` 的 `resolveCrossInstallTarget`），安装路径与管理页都调它。
原本是两份 switch：一份决定"用哪个 npm、装到哪"，另一份决定"够不够" —— 而一台机器上
"当前生效的那套"与"登记的那套"可以不同，于是界面与服务端的结论必然分叉。

### 19.3 验证

| 项 | 结果 |
| --- | --- |
| `go build` / `go vet` | 干净 |
| Go 定向用例 | 全绿 |
| 新增回归测试 | 3 条（脚本行序、按安装方式选套、装运行时也写审计） |
| 前端 `tsc` / 单测 / `vite build` | 0 / **653/653** / 通过 |
| 变异检验 | 把 `prefix=` 移回 install 之后 ⇒ 行序断言**红**，按 sha1 逐字节还原 |

### 19.4 两轮复查的规律

**两轮里最严重的两条（§18 的 G-1、§19 的 19.1）都是"生成的脚本"上的错**，而且都逃过了
当时的测试：它们都不测脚本的**执行结果**。这一层（往别的机器上拼 shell）需要一个不同
于"结构断言"的验证方式 —— 至少要把求值顺序、变量作用域当成断言对象。

另外：§18 的 9 条里有 2 条（审计、`needsGrant`）在 §18 当时只被"记为轻微"，到 §19 才处理
—— **复查报告里的"轻微"不等于可以不做**，它们往往只是"当时没想清楚后果"。

## 20. 真实可用性验证（2026-09-21）

前 19 节都是设计与自复查，这一节回答一个更硬的问题：**装得上吗、装完真的能用吗。**
答案来自真实执行 —— 真服务端、真网络、真 npm、真二进制、真浏览器，全程没有桩。

### 20.1 新增的验证层：真实端到端

`apps/control-server/internal/app/real_e2e_test.go`，**默认跳过**，`MILEVIA_E2E=1` 打开。

为什么必须有这一层：其余用例用的是假 npm、假环境、假输出，而它们**答不了**"真实产物长什么样"。
本文件诞生第一天就抓到一个真 bug（§20.2），它逃过了此前全部单测 —— 因为那些用例喂的都是编造的输出。

它做的事（全部是真的）：

| 步骤 | 实测 |
|---|---|
| 检测系统既有 CLI | claude 2.1.266 / codex 0.154.0 |
| 拉可安装运行时清单 | 真访问 nodejs.org，6 个版本，最新 LTS 24.21.0 |
| 安装托管 Node | 24.21.0，56.2s，SHA256 校验 + 解压后自检（node 与 npm 都真跑一次，npm 11.19.0） |
| 安装 Claude Code（钉 2.1.277） | 5.4s，托管前缀，自检真执行 `claude.cmd --version` |
| 升级 Claude Code | 2.1.277 → 2.1.278，5.8s；升级后**直接执行二进制**确认版本 |
| 装完是否真的生效 | 探测 / `runner.Version()` / 路径解析器三处都指向托管那份（§14.B 那条风险的判据） |
| 安装 Codex | 0.155.1，8.4s，托管前缀 |
| 审计 | install-runtime / install / update 三条齐全，升级 from/to 正确 |
| 用户系统里那份 | 全程未被改动（收尾逐字核对路径与版本） |

边界（真实 HTTP）：四种非法版本号全部被拒且登记项未被破坏；未知工具/主机 404；
对本机授权 400；未授权远端安装 403；**升级进行中并发「装运行时」与「装另一个 CLI」都是 409**
（证明两个入口共用同一把闸门），随后闸门正常释放；旧的薄委托路由仍可用。

### 20.2 抓到并修掉的真 bug：版本号提取曾经有七份实现

- Claude 的输出是 `2.1.266 (Claude Code)`（产品名在**后**），Codex 是 `codex-cli 0.155.1`（在**前**）。
- 这件事被写了 **7 遍**：本机 Claude/Codex、SSH 的 Claude/Codex、WSL 的 Claude/Codex，
  以及 `agent_install.go` 里那份。前六份各按自己那侧的真实输出写对了；最后一份用目录里的
  单个字段 + **TrimSuffix**，于是对 Codex **静默失效**。
- 症状：界面（探测）显示 `0.155.1`，而登记表、审计、安装接口返回 `codex-cli 0.155.1`。
  用户看不出异常，但同一个版本在两处不一致 —— 这正是 `trimAgentVersion` 自己的注释所禁止的。
- 修法：收成一个 `agentVersionFromOutput`（按**版本号本身**提取，与产品名在前在后无关），
  四个 runner 全部改用它，目录里那个字段删除。
- 顺带修掉同类缺陷的漏网者：`windowsAgentRunner.CheckUpdate` 仍是 `latest != local`
  字符串比较（装了预发布版会把降级报成"有更新可用"）。
- 新增结构断言：产品名字面量不允许出现在任何生产代码里（注释不算），各 runner 必须调用
  那个唯一实现。变异检验双向确认它会红，且按 sha1 逐字节还原。

教训：**"给每个工具配一个要剥掉的字符串"是个错误的抽象** —— 它假定产品名总在同一侧。
判据应当落在"版本号长什么样"上。另外，"同一件事在 7 处各写一遍"本身就是缺陷，
只是前六处的错法恰好没暴露。

### 20.3 真实浏览器里的页面

用 Playwright 驱动真 Chromium 打开真服务端的 `/cli-tools`（走 CDP、等选择器），
断言全落在**页面可见文本**上（用户看到的是文本，断言也看文本）：

1. 装之前：运行时卡片显示「已安装 24.21.0 / 来源 平台托管 / npm 11.19.0 / 安装位置 / 下载源 nodejs.org / 已是最新」；
   两个工具显示系统里那份的版本。
2. 通过接口装 Codex 后刷新：卡片变成 0.155.1，并出现托管安装位置；Claude Code 未受影响。
3. 把 Codex 钉到 0.155.0，在页面上点「检查更新」→ 出现「升级到 0.155.1」→ 点它 → 确认对话框 → 确认 →
   卡片显示「已安装 0.155.1 / 已是最新」，接口核对一致。

产物（含 5 张截图与原始日志）：`outputs/cli-tools-e2e/`。

### 20.4 明确未能验证的部分

1. **WSL**：本机安全策略把 `wsl.exe` 列入程序黑名单，进程起不来（Access is denied）。
   跨端 WSL 那条路目前只有假环境单测覆盖。
2. **SSH**：本机没有可连接的 SSH 目标（无 docker、无 sshd，也不应拿别人的机器试）。
   已验证「未授权 → 403」；**SFTP 上传分发包那条分支未实测**。
3. **系统 npm 就地升级**：只做了只读验证（计划判定 + 自检命中系统那份的真实路径），
   没有真的重装用户系统里那份 Claude Code —— 那是用户环境，不该在验证里动它。
4. 托管 Node 自动升级、卸载、装到指定版本：本期按设计不做（§13）。

### 20.5 跑法

```bash
cd apps/control-server
MILEVIA_E2E=1 go test ./internal/app/ -run 'TestRealE2E' -v -timeout 40m
```

它只往 `.tmp/e2e-run/` 下写（托管工具链落点由 `AUTO_TOOLCHAIN_ROOT` 指定），跑完可整个删掉。
用例里有一道显式断言守着"安装计划必须落在托管前缀"——万一不成立就直接失败，
**而不是继续往下装**（否则会覆盖用户机器上既有的安装）。
