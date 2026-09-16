# MCP 连接与配置调研与实施方案

> 日期：2026-09-10（v3，第三轮复核修订）
> 状态：调研 + 方案，待评审后分期实施
> 关联：docs/18（AI CLI 配置档案）、docs/20（按项目环境分发的 Agent Runner）、docs/21（项目级 AI 配置）、docs/22（对话页 Skill 区域）、docs/26（首页设置页）、docs/29（交互式终端）、docs/30（项目级 AI 凭据隔离）
> 版本沿革：v1 有三处结论性错误（审批行为方向、改动点数量、只读路径可用性）；v2 逐条更正并补出审批链路；**v3 再次复核，发现两项影响架构的问题（`auto_approve_tools` 机制不成立、`--strict-mcp-config` 的版本门槛会导致挂起）以及 9 项实现级缺口。第 0 节为两轮复核记录，实施时以 v3 为准。**

---

## 0. 复核记录

### 0.1 第一轮复核（相对 v1 的更正，v2 已固化）

| # | v1 的说法 | 复核后的事实 | 依据 |
| --- | --- | --- | --- |
| 1 | MCP 工具不命中 hook，「会直接执行，等于绕过审批的后门」 | **方向相反**：`-p` 非交互模式下未被预授权的工具调用会被**拒绝**，不会静默执行。真实后果是 MCP 工具在默认权限下**根本不可用**（功能阻断），不是安全后门 | Claude Code 权限文档：「What happens when `claude -p` needs permission for a tool call? The call is denied.」 |
| 2 | 需改 2 处 `PreToolUse` matcher | 是 **3 处**。`ssh_runner.go:1279` 另有硬编码 matcher，且被 SSH 的 `Run` 与 `StartSession` 共用 | `grep '"matcher"'` |
| 3 | 只改 matcher 即可让 MCP 工具进入审批 | **不止**。服务端 `app.go:5666` 有硬校验 `input.ToolName != "Bash"` → 直接返回 400。matcher 放开后 hook 仍会被服务端拒绝，MCP 工具依旧不可用 | `app.go:5666-5669` |
| 4 | 审批链路只需改后端 | 前端也有耦合：`timeline.ts:125` 靠 `toolInput.command` 字符串相等把审批锚定到卡片（MCP 无该字段 → 审批卡永远不显示）；`ConversationPage.tsx` 的文案与渲染写死了「命令」语义 | `timeline.ts:124-127`、`ConversationPage.tsx:689,760-775` |
| 5 | MCP 工具集是动态的，无法静态写进只读 deny 清单 | **错**。deny 规则支持工具名 glob，`"mcp__*"` 能匹配所有 server 的全部 MCP 工具。只读路径加一条即可 | Claude Code 权限文档：「`"mcp__*"` matches every MCP tool across all servers」 |
| 6 | WSL 落盘写 `\\wsl$\<distro>\tmp\` | 更可靠的是代码既有范式：写 Windows 临时文件 + `windowsToWSLMntPath` → `/mnt/<drive>/...`。`\\wsl$` 要求发行版处于运行态，而写文件发生在拉起 CLI **之前** | `wsl_agent_run.go:289` 对 Codex schema 就是这个做法 |
| 7 | 注入点 = `args()` + `sessionArgs()` | **SSH 完全不走这两个函数**。`ssh_runner.go` 自行拼 shell 字符串（`Run` 两分支 + `StartSession`），且 Claude 路径目前没有落盘机制，需新增 | `ssh_runner.go:1013-1030,1214-1219` |
| 8 | Codex 走「隔离 CODEX_HOME 写 config.toml」 | 隔离 home 只在 `AuthMode == "api_key"` 时创建；其它情况下造隔离 home 会丢认证、写真实 config.toml 会污染用户配置。**更优解：用 `-c` 点号路径注入，不碰文件** | `codex_runner.go:419-421`；Codex 文档确认 `-c` 支持 `mcp_servers.x.enabled=false` 这类嵌套键 |
| 9 | 只读任务「默认不注入 MCP」即可 | 仍建议不注入，但即使注入也可用 `mcp__*` deny 兜住；两条路都可，详见 §8.3 | 同 #5 |

### 0.2 第三轮复核（v3 新增更正与增补）

| # | v2 的说法 | 复核后的事实 | 依据 |
| --- | --- | --- | --- |
| 10 | `auto_approve_tools` 写入 `settings.permissions.allow` | **不可行（架构级）**。权限评估是「Hooks → deny → ask → 权限模式 → allow → 回调」，**hook 是第 1 步且总会触发**（文档：PreToolUse hooks run before the execution of the tool **regardless of permission state**）。Milevia 的 hook 实现是「阻塞等人点」，于是 allow 规则永远轮不到 → 白名单形同虚设。**必须在 hook handler 内部判定白名单并直接返回 `allow`**（详见 §8.4） | Claude Code SDK permissions 文档「1 Hooks：Run hooks first」；hooks-guide |
| 11 | 未展开 hook-allow 与 deny 的优先级 | hook 返回 `allow` **不覆盖** deny / ask 规则；deny 来自任何设置作用域时优先级最高。**这反而让 §8.3 的双保险真正成立**：即使只读路径漏控注入，`mcp__*` deny 仍会拦下 | hooks-guide：「返回 allow 跳过交互式提示但**不覆盖权限规则**。如果拒绝规则与工具调用匹配，即使 hook 返回 allow，调用也会被阻止」 |
| 12 | `--strict-mcp-config` 的作用只是「屏蔽项目 `.mcp.json`」 | 还有**硬版本门槛**：低于 **v2.1.246** 时，strict 会话仍会为「它不加载的项目级 server」等待审批，在 `-p` 下表现为**启动挂起**（比报错更糟，会静默卡住任务）。本机 2.1.266 满足；**SSH 远端必须探测 ≥ 2.1.246** | MCP 文档：「Skipping the approval prompt for the project-scoped servers Claude Code isn't loading requires Claude Code v2.1.246 or later」 |
| 13 | 只提 `--strict-mcp-config` 一条闸门 | 官方共**三条**可选闸门：`--strict-mcp-config`（只认 `--mcp-config` 传入的）、`--setting-sources`（按来源整体排除 project）、`disabledMcpjsonServers`（逐 server 屏蔽项目级，**所有权限模式下都生效**）。注意 strict 会**连用户自己的 `~/.claude.json` MCP 一起屏蔽**，需在 UI 明示 | 同 #12 文档段 |
| 14 | 「解密密钥 → 写进临时文件」 | 可能属于过度设计：`.mcp.json` 支持 `${VAR}` / `${VAR:-default}` 环境变量展开，位置覆盖 `command` / `args` / `env` / `url` / `headers`。**若 `--mcp-config` 加载的文件同样支持**，则文件里只留 `${MCP_TOKEN_x}`，真值走 CLI 进程环境（`managedCLIEnvironment` 已具备）→ 文件不含明文密钥、同一份配置可跨环境复用，同时解掉 §7.2 的落盘密钥问题。**列为第二个实施前必测假设** | MCP 文档「Environment variable expansion in .mcp.json」 |
| 15 | 落盘名 `<data>/mcp-runtime/<hash>.json`（按项目+Agent+环境） | **并发缺陷**：两个并发 run 同名 → 先结束者的 `defer rm` 会删掉另一个仍在使用的文件，导致运行中途 MCP 失效。文件名必须带 **run / session 唯一后缀** | — |
| 16 | 「0600 + 用后清理」 | `chmod 600` **在 Windows 基本是空操作**（Go 的 perm 参数在 Windows 不生效，走 ACL）；WSL 经 `/mnt/c`(drvfs) 读时 Unix 模式通常被忽略。真实保护 = 目录位于用户私有数据目录 + `.gitignore` 的 `data/*`。**且目录解析必须沿用 `profile-master.key` 的逻辑**（`app.go:684-686`：`DataDir` 为空时回退 `filepath.Dir(DatabasePath)`），否则可能落到意外位置甚至被提交 | `app.go:684-686`；`.gitignore` 的 `data/*`、`.tmp/` |
| 17 | §12.1「无启用 MCP 时启动命令与改造前**逐字节一致**」 | **自相矛盾、无法通过**：方案同时要求把 matcher 从 `Bash` 改成 `Bash\|mcp__.*`，这必然改变**每一次普通 run** 的 `--settings` 字符串。应改为「不出现任何 MCP 参数」 | `claude_runner.go:642,690`、`ssh_runner.go:1279` |
| 18 | 前端「2 处」 | 数量对，但两个组件内部约 **6 处**分支要改，且 MCP 工具名会原样显示成 `mcp__github__create_issue`，需可读化 | `ConversationPage.tsx:674-689`（ToolCard）、`:760-775`（ApprovalBanner，标题在 `:769`） |
| 19 | 未提同一 run 的并发审批 | `waitForApproval` 对同一 run **只允许 1 个待审批**（并发请求 409）。MCP 工具在一个回合内**并行调用**的概率远高于 Bash；第二个调用的 hook 会因 `curl -f` 收到 409 而失败 → 非阻塞错误 → `-p` 下最终被拒。建议服务端对同 run 审批**排队**，或返回结构化 `deny` + 可读原因，而不是让 hook 抛错 | `app.go:5696-5702`；`scripts/claude-approval-hook.sh`（`curl --fail`） |
| 20 | Codex `-c` 注入（只说用点号路径） | 两个实现细节：① `-c` 的值按 **TOML** 解析，Windows 路径反斜杠与引号必须做 **TOML 转义**（不能只做 shell quote），应使用 TOML 编码器构造；② `env_vars` 在 **SSH 远端**上没有安全通道 —— 变量须存在于远端 Codex 进程环境，而 `export VAR=…` 会出现在远端 shell 的 argv/`ps` 里，**破坏「密钥不进 argv」原则**。Claude 有落盘通道，Codex-SSH 没有等价物，需单独设计 | — |
| 21 | 未提上游 `--bare` | Claude 官方将把 **`--bare` 设为 `-p` 的默认模式**；bare **不读取** 项目 `.mcp.json`、项目 CLAUDE.md、队友 hooks、插件与 skills 的自动发现。Milevia 依赖 skills 自动发现（`skills.go:148-162`，**未传** `--plugin-dir`）→ 该变更会在未来某个版本**静默破坏 skills**。列为前瞻风险，需纳入版本探测（曾按 §19.8 实现，因无可执行动作已于 §25 撤回；风险本身仍在） | Claude Code headless 文档「`--bare` … will become the default for `-p` in a future release」 |

### 0.3 复核中**被证伪、无需改动**的两条

- 「SSH 只读路径拿不到 deny 清单」——**查证为误判**。`ssh_runner.go:984-993` 在调用 `sshClaudePermissionArgs` 之后，会因 `len(request.ReadOnlyTools) > 0` **整体覆盖** `permissionArgs` 为只读 settings。SSH 只读与本地一致。
- 「hook-allow 在 `-p` 下可能无效」——**已被现有功能证伪**。Milevia 的默认权限模式（`approval_required`）今天就是用同一套 hook-allow 让 Bash 落地的；该机制在生产中已验证有效。

### 0.4 第四轮：三项前置验证实测结果（2026-09-10，本机）

用真实 CLI + 可控 mock 端点实测。环境：Claude Code **2.1.266**（本机）、**2.1.245**（`@anthropic-ai/claude-code@2.1.245` 隔离安装）、Codex **0.153.4**。探针是一个「启动时把自己的 argv/env 落盘」的最小 MCP stdio server —— 据此可判定 **server 是否真被拉起**、**变量是否被展开**。

| 项 | 结论 | 关键证据 |
| --- | --- | --- |
| **1. `--mcp-config` 是否支持 `${VAR}` 展开** | ✅ **支持**（`command` / `args` / `env` 全覆盖） | 变量组 config 的 server 被成功拉起（`STARTED pid=…`），且落盘值 `env.MCP_PROBE_MARKER=expanded-marker-B`（源自 `${MCP_PROBE_VAR}`）；字面量对照组为 `literal-marker-A` |
| **2. Codex `-c` 是逐 server 合并还是整表替换** | ✅ **逐 server 合并**（不污染用户配置） | 预置 `existingA` 的 `config.toml` 下，`-c mcp_servers.injectedB.…` 后 `existingA` **仍在**；单独 `-c mcp_servers.existingA.command="python"` 时其 `args` **保留**；整对象形式 `-c 'mcp_servers.injectedC={command=…,args=…,env=…}'` 同样合并且 `env` 生效 |
| **3. strict 的版本门槛（< 2.1.246 是否挂起）** | ⚠️ **未能复现「挂起」；2.1.245 与 2.1.266 行为一致** | 四组对照（2.1.245 / 2.1.266 × strict / 非 strict）在「能正常结束对话的 mock API」下**全部 `exit=0`**；strict 组两版本都**未拉起项目 `.mcp.json` 的 server**（`outX` 未生成），非 strict 组两版本都拉起了（`outX` 生成） |

**对方案的影响与处置：**

1. **验证 1 → 密钥从「内联」改为「占位符」（按环境分叉）。** 配置文件中只写 `${MCP_SEC_<id>}`，真值经 CLI 进程环境注入（本地 `exec.Command.Env`、WSL `WSLENV` 透传）。这样 Windows/WSL 的 MCP 配置文件**不含任何明文密钥**。SSH 远端无安全 env 通道，仍保持内联（见 §16 与 §0.4 局限）。**已落地，见 §16。**
2. **验证 2 → P1 的 Codex 注入可按原设计实施。** 仍需注意 `-c` 的值按 **TOML** 解析，路径/引号须做 TOML 转义（§0.2 第 20 条）；**Codex-SSH 的密钥通道仍未解决**（同上）。
3. **验证 3 → `≥2.1.246` 门槛属过度保守，已放宽。** 实测 2.1.245 的 strict **既不挂起、也正确屏蔽项目 server**，一刀切会让大量可用的远端白白失去 strict 保护。**处置**：`mcpStrictOK` 门槛已下调至与 `mcpInjectable` 同档（支持 `--mcp-config` 即开 strict），**仅在探测失败 / 版本无法解析时降级**；版本 < 2.1.246 时在任务日志打一条**可见警告**而非关闭 strict。判断依据是**两种出错方式的代价不对称**——保留门槛是「静默的安全缺口」，放宽最多是「可见的可用性风险」，不该用前者换后者。**已落地，见 §16。**

**实验局限（必须声明）：**

- 验证 3 使用 **mock Anthropic 端点**（返回合法 SSE，但不经真实服务），与真实 API 环境存在差异；官方所述场景可能依赖真实环境中的其他交互。
- **真实 SSH 远端未能验证**：本机 `wsl.exe` 被沙箱程序黑名单拦截，且无可用远端主机。故「远端旧版 strict 是否挂起」只有**本地跨版本**证据，**没有真实远端证据**。
- 因此 §7.3 的降级通道**仍然保留**（作为防御性措施），只是**触发条件建议放宽**。

---

## 1. 结论先行

1. **可行，且与现有架构高度契合。** Milevia 已把「把外部资产注入 AI CLI」沉淀为成熟范式：`skills.go` 的三环境资产发现、`agent_profiles.go` 的受管环境变量、`claude_runner.go` 的 `--settings` 内联 JSON。MCP 复用这三条通路即可，不需要新的执行模型。全仓确认**没有任何 MCP 代码**（`--mcp-config` / `--strict-mcp` / `--setting-sources` / `--bare` 均零命中），是纯新增。

2. **功能能否跑通，取决于审批接入 —— 这是 P0 的硬前提，不是可选的加固。** Milevia 以 `-p`（非交互）方式启动 Claude；在这种模式下，任何未被预授权的工具调用都会被**拒绝**。MCP 工具既不匹配现有 hook 的 `Bash` matcher，也不在 `--allowedTools` 里，所以**默认状态下 MCP 工具 100% 调用失败**。必须在 `PreToolUse` hook 里显式返回 `allow`，MCP 功能才成立。

3. **审批接入是一条跨 4 层的链路，不是改一个 matcher。** 后端 3 处 matcher + 1 处服务端硬校验（`app.go:5666`）+ 前端 2 个组件（内部约 6 处分支），缺任何一环 MCP 工具都不可用。这是本方案工程量最容易被低估的部分。

4. **`--strict-mcp-config` 是安全要害，但它防的是「进程启动」而非「工具调用」，且有版本门槛。** Milevia 在 `-p` 模式下，Claude Code 会**加载项目根目录 `.mcp.json` 的 server 且不弹审批**（官方原文：`In claude -p runs … it loads project-scoped servers without asking`）—— MCP server 是 CLI 启动时就连接的子进程。所以「克隆一个带恶意 `.mcp.json` 的仓库 → 在 Milevia 里打开」= 任意进程被执行。默认严格模式。**原以为 strict 有「≥ v2.1.246 否则挂起」的版本门槛，但 2026-09-10 实测未能复现（2.1.245 与 2.1.266 行为一致），该门槛属过度保守，见 §0.4。**

5. **三环境差异是最大实现复杂度。** stdio 型 MCP 的 `command`/`args`/`env` 在 Windows、WSL、SSH 上各不相同（可执行路径、`npx` vs `npx.cmd`、路径形态、目标机是否有 Node/Python）。方案用「一份逻辑定义 + `${PROJECT_DIR}`/`${HOME}` 占位符按目标环境解析」解决，不让用户配三份。HTTP 型 MCP 天然环境无关。

6. **凭据不能进 argv，也不该进落盘文件。** Claude 走 `--mcp-config`；最理想的载体不是「解密后写进临时文件」，而是**文件里只留 `${VAR}` 占位符 + 真值走进程环境**（若 `--mcp-config` 支持环境变量展开，见 §0.2 #14）。Codex 走 `env_vars` 白名单转发（argv 里只出现变量名）。

7. **Claude 与 Codex 的能力不对称，需如实呈现。** Claude 有 `--strict-mcp-config` 等三条闸门可屏蔽外部 MCP 来源；**Codex 没有等价开关**，用户自己 `~/.codex/config.toml` 里的 MCP 无法被 Milevia 屏蔽。Codex 只实现 MCP 的 tools 原语（不支持 resources/prompts）。

8. **分期。** P0：静态配置 + 三环境注入 + 完整审批链路 + 首页入口；P1：连接测试与工具发现 + 导入 + 模板；P2：远程 MCP 的 OAuth 2.1、工具级白名单可视化、审计。

---

## 2. 现状调研

### 2.1 Milevia 已有的可复用能力

| 能力 | 现有实现 | 对 MCP 的复用价值 |
| --- | --- | --- |
| 三环境资产发现 | `skills.go:335` `discoverSkillsForProject`，按 `resolveAgentTargetEnv` 分派本地扫描 / UNC 扫描 / 远端 `find+cat` | 「生效范围判定」与「远端配置读取（导入用）」可直接照搬 |
| 目标环境判定 | `agent_target_env.go:16-18` 三常量（`windows` / `wsl` / `remote-linux`）+ `resolveAgentTargetEnv`（`:37-58`） | 注入必须按同一套判定分叉，避免两套环境语义 |
| 受管进程环境 | `agent_profiles.go:117` `managedCLIEnvironment`，先屏蔽受控变量再叠加 profile.Env | MCP 凭据可复用同一「屏蔽 + 注入」语义 |
| 密钥加密存储 | `profile_secrets.go:115-154`：AES-GCM + `sec_<uuid>` 引用 + 主密钥落 `profile-master.key` | MCP 的 env / header 密钥直接复用，不新建密钥体系 |
| WSL 路径转换 | `windowsToWSLMntPath`（`wsl_discovery.go:191`）把 `C:\...` → `/mnt/c/...`；`uncToWslPath`（`:134`）把 `\\wsl$\...` → Linux 路径 | MCP 配置文件从 Windows 侧供给 WSL 时用前者，最可靠 |
| WSL 参数编码 | `wsl_agent_run.go:117` `wslEncodeArg`；`wslForwardEnvKeys`（`:27-34`）经 `WSLENV` 透传 | WSL 下含特殊字符的配置参数与凭据变量透传照此办理 |
| 远端文件落盘 | `ssh_runner.go:1116-1119`：Base64 写 `/tmp/milevia-codex-schema-*.json` + `trap rm` | SSH 下 Claude 的 MCP 配置文件落盘完全同构（需新增，Claude 路径目前没有） |
| Codex 隔离配置 | `codex_runner.go:419-458` 仅在 `AuthMode=="api_key"` 时建隔离 `CODEX_HOME` | 说明 Codex 不能用「一律隔离」的思路，见 §7.4 |
| 数据目录解析 | `app.go:684-686`：`profile-master.key` 优先落 `DataDir`，为空时回退 `filepath.Dir(DatabasePath)`；`.gitignore` 已忽略 `data/*`、`.tmp/` | MCP 运行时目录必须沿用同一解析，保证被忽略、不误提交 |
| DB 迁移范式 | `app.go:1413` `migrate` + 各特性 `migrateX` + `ensureColumn`（`:1267`） | 新增 `migrateMCPServers` |
| 路由分组 | `app.go:953` `routes()`；SSH 用 `registerSSHRoutes`（`ssh_connection.go:126`） | 新增 `registerMCPRoutes` |
| 前端设置页 | `SettingsPage.tsx` 分组锚点导航；`api.ts` 统一请求封装 | MCP 管理页复用页面骨架与请求层 |

### 2.2 Milevia 的 MCP 现状

**完全没有。** 无 MCP 相关的表、路由、类型或配置项。现有 Claude 启动参数中不含 `--mcp-config` / `--strict-mcp-config` / `--setting-sources` / `--bare`。

### 2.3 外部生态现状（2026-09 实测与官方文档）

**Claude Code 2.1.266（本机实测 `claude --help`）：**

| 参数 | 作用 |
| --- | --- |
| `--mcp-config <configs...>` | 从 JSON **文件或内联字符串**加载 MCP server；可多次指定 |
| `--strict-mcp-config` | 只用 `--mcp-config` 传入的 server，忽略其它所有来源 |
| `--setting-sources <sources>` | 选择 settings 来源（可按来源整体排除 project） |
| `mcp` 子命令 | `mcp add/list/get/remove`，管理三级作用域 |

**屏蔽项目 `.mcp.json` 的三条闸门（官方并列给出）：**

| 闸门 | 粒度 | 代价 |
| --- | --- | --- |
| `--strict-mcp-config` | 只认 `--mcp-config`；屏蔽**一切**其它来源 | 会连用户自己的 `~/.claude.json` MCP 一起屏蔽（需 UI 明示） |
| `--setting-sources` | 按来源排除（如去掉 project） | 连带丢弃项目 CLAUDE.md / settings / hooks |
| `disabledMcpjsonServers` | 逐 server，**所有权限模式下生效** | 需事先知道 server 名 |

**配置作用域与优先级（高 → 低）：** `--mcp-config` ＞ 项目根 `.mcp.json` ＞ `~/.claude.json` 的 `projects.<path>.mcpServers` ＞ `~/.claude.json` 顶层 `mcpServers` ＞ 插件。同名按最高优先级**整体**覆盖，**字段不跨作用域合并**。

**环境变量展开：** `.mcp.json` 支持 `${VAR}` 与 `${VAR:-default}`，可出现在 `command` / `args` / `env` / `url` / `headers`。变量缺失时配置仍加载，但会在 `claude mcp list` 报缺失警告并原样保留 `${VAR}` 文本。**（是否同样适用于 `--mcp-config` 文件：待实测，见 §0.2 #14）**

> `--scope user` 的常见误传是写进 `~/.claude/settings.json`，实际在 `~/.claude.json`。Milevia 不应直接改这两个文件（会与用户在终端里的操作互相覆盖）。

**传输类型：**

| 类型 | 形态 | 环境相关性 |
| --- | --- | --- |
| `stdio` | 客户端拉起本地子进程，stdin/stdout 走 JSON-RPC | **强环境相关** |
| `http` | Streamable HTTP 单端点，Bearer / OAuth | 环境无关 |
| `sse` | 旧版双端点，已被 Streamable HTTP 取代 | 环境无关，仅兼容 |

**权限模型（本方案的关键依据）：**

- 评估顺序：**Hooks → deny 规则 → ask 规则 → 权限模式 → allow 规则 → 回调**。
- **Hooks 是第 1 步，且 PreToolUse 在任何权限状态下都会触发** —— 这条决定了两件事：① MCP 工具要能用，只能靠 hook 返回 `allow`；② `auto_approve_tools` 不能靠 allow 规则实现（§8.4）。
- **hook 返回 `allow` 不覆盖 deny / ask 规则**；deny 来自任何作用域时优先级最高。
- `-p` 非交互模式下，走到「需要用户确认」这一步的工具调用**直接拒绝**。
- 工具名 glob：**deny 规则**支持 `"mcp__*"`（匹配所有 server 的全部 MCP 工具）、`"*"`；**allow 规则**支持 `mcp__<server>__*`（server 段必须是字面量），`"mcp__*"` 这种无锚点写法会被忽略并告警。
- 例外：若 MCP server 在工具元数据里标了 `_meta["anthropic/requiresUserInteraction"]`，该工具即使命中 allow 规则也会回落要求用户交互，`-p` 下被拒。Milevia 的 hook-allow 覆盖不了这一类。
- 上游变更：`--bare` 将成为 `-p` 的默认；bare 跳过 hooks / skills / plugins / MCP servers / CLAUDE.md 的**自动发现**（显式 `--settings`、`--mcp-config` 仍生效）。**Milevia 依赖 skills 自动发现，该变更会静默破坏 skills —— 见 §13。**

**Codex CLI：**

- `config.toml` 的 `[mcp_servers.<name>]`；`command` 表示 stdio、`url` 表示 Streamable HTTP，**二选一**。
- 支持 `env` / `env_vars`（白名单转发）/ `cwd` / `enabled` / `enabled_tools` / `disabled_tools` / `startup_timeout_sec` / `tool_timeout_sec`。
- **`-c/--config` 支持点号路径设置嵌套值**，值按 TOML 解析（官方以 `mcp_servers.context7.enabled=false` 举例）。
- **没有** `--strict-mcp-config` 等价物。
- 只实现 MCP 的 tools 原语；OAuth 需 `experimental_use_rmcp_client = true`。

---

## 3. 关键设计决策

| # | 决策 | 选择 | 理由 | 未选方案 |
| --- | --- | --- | --- | --- |
| 1 | 数据归属 | **Milevia 数据库为权威源**，运行时生成配置注入 | 需跨环境、跨 Agent 统一管理与脱敏、审计；直接改 CLI 配置文件会与用户终端操作互相覆盖 | 以文件系统为唯一真实来源（skills 的做法）——MCP 不是既有资产 |
| 2 | 与既有配置的关系 | **默认 `--strict-mcp-config`**，可为单项目放行 `.mcp.json` | 见 §1.4，`-p` 下 `.mcp.json` 的 server 会被无审批启动 | 永远附加（供应链风险）；永远严格且不可配（切断团队既有工作流） |
| 3 | 审批接入 | **接入既有允许/拒绝时间线**，这是功能前提 | 见 §1.2，不接则 MCP 工具全废 | 不接入（MCP 不可用） |
| 4 | 三环境参数 | **一份逻辑定义 + 占位符按环境解析** | 用户只配一遍；路径形态由 Milevia 转换 | 每环境各配一份（负担 ×3，易错） |
| 5 | Claude 注入载体 | **落盘临时文件，内容优先用 `${VAR}` 占位符**，真值走进程环境 | 兼顾「密钥不进 argv」与「密钥不落盘」；同一份配置可跨环境复用 | 内联 JSON（密钥进进程列表）；直接写明文密钥入文件（落盘泄露面） |
| 6 | WSL 落盘位置 | **Windows 临时文件 + `windowsToWSLMntPath`** | 代码既有范式（`wsl_agent_run.go:289`）；不依赖发行版运行态 | `\\wsl$\` UNC（写文件时发行版可能未启动） |
| 7 | Codex 注入方式 | **`-c 'mcp_servers.<name>={...}'`**，值用 TOML 编码器构造 | 不碰文件、不碰 `CODEX_HOME`，规避认证丢失与配置污染 | 隔离 `CODEX_HOME`（非 api_key 档案会丢认证） |
| 8 | 密钥存储 | **复用 `profileSecretStore`**（AES-GCM + `sec_<uuid>`） | 不新建密钥体系；前端天然可脱敏 | 明文入库 |
| 9 | 作用域模型 | **global / project 两级** | 覆盖绝大多数场景，避免复刻五级优先级的认知负担 | 完整五级 |
| 10 | 连接测试 | **在目标环境执行** | stdio server 是目标机上的进程，本机测不出远端可用性 | 只在控制服务本机测 |
| 11 | 只读分析任务 | **不注入 MCP**；并在 `insightReadOnlyDenyTools` 补 `"mcp__*"` 兜底 | 双保险；且 hook-allow 不覆盖 deny，这条兜底真正有效 | 只靠 hook 运行时拦截（更复杂且被证明不成立） |
| 12 | 自动放行 | **在 hook handler 内判定白名单并直接返回 `allow`** | hook 是权限评估第 1 步，写 `permissions.allow` 轮不到 | `settings.permissions.allow`（无效，见 §0.2 #10） |
| 13 | OAuth | **P2** | 需本地回调服务器 + token 加密落盘 + 刷新，工程量大 | P0 硬上 |
| 14 | 临时文件命名 | **带 run / session 唯一后缀** | 避免并发 run 互删 | 按「项目+Agent+环境」哈希（并发不安全） |

---

## 4. 架构设计

### 4.1 注入通路总览

```text
Milevia SQLite (mcp_servers)
        │  用户配置 / 模板 / 导入
        ▼
   ┌─────────────────────────────────────────────────┐
   │  MCP 配置生成器（每 run / 每会话 + 每 Agent + 每环境）│
   │  · 合并 global + project 作用域                  │
   │  · 按目标环境过滤 environments                   │
   │  · 占位符解析为环境本地路径                       │
   │  · 密钥：优先留 ${VAR}（真值走进程环境）           │
   │  · 输出 mcpServers JSON / Codex -c 参数          │
   └─────────────────────────────────────────────────┘
        │
        ├── Windows  ──► <data>/mcp-runtime/<runID>.json
        │                claude … --mcp-config <win 路径> --strict-mcp-config
        │
        ├── WSL      ──► <data>/mcp-runtime/<runID>.json（Windows 侧）
        │                wsl.exe -d <distro> -- claude … --mcp-config /mnt/c/…/<runID>.json
        │                （windowsToWSLMntPath 转换，同 wsl_agent_run.go:289）
        │
        └── SSH      ──► printf %s <base64> | base64 -d > /tmp/milevia-mcp-<runID>.json
                         trap 'rm -f $f' EXIT
                         cd <proj> && claude … --mcp-config $f --strict-mcp-config …
                         （与 ssh_runner.go:1118 的 schema 落盘同构，需为 Claude 路径新增）
```

### 4.2 CLI 注入点清单（**5 处**，不是 2 处）

| 环境 | 函数 | 说明 |
| --- | --- | --- |
| Windows | `claude_runner.go:621` `args()` | 一次性 Run |
| Windows | `claude_runner.go:679` `sessionArgs()` | 持久会话 |
| WSL | 复用上面两个（`wsl_agent_run.go:196,353` 调用） | 无需单独改 |
| SSH | `ssh_runner.go:1013-1030` `Run`（`PromptViaStdin` 两分支） | 自行拼 shell 字符串，**不走 `args()`** |
| SSH | `ssh_runner.go:1214-1219` `StartSession` | 同上 |

**请求构造点（5 处，需在此处解析出 MCP 配置并填入）**

| 位置 | 用途 | 是否注入 MCP |
| --- | --- | --- |
| `app.go:5106` | 会话内一次性 Run | 是 |
| `app.go:5164` | `StartSession` | 是 |
| `app.go:5252` `runAgent` | 主执行路径 | 是 |
| `insights.go:504` | 只读分析扫描 | **否** |
| `orchestration.go:2625` | 独立审查（只读） | **否** |

> 注意 `AgentSessionRequest`（`claude_runner.go:71-79`）**没有 `AgentID` 字段**，会话路径要在构造处自行取 `conversation.AgentID` 再算 MCP 配置。

### 4.3 审批链路改动点清单（**6 处 + 1 处白名单闸门**，跨 4 层）

| 层 | 位置 | 现状 | 需改为 |
| --- | --- | --- | --- |
| 后端·hook | `claude_runner.go:642` | `"matcher": "Bash"` | `"Bash\|mcp__.*"` |
| 后端·hook | `claude_runner.go:690` | `"matcher": "Bash"` | 同上 |
| 后端·hook | `ssh_runner.go:1279` | `{"matcher":"Bash"}`（SSH Run + StartSession 共用） | 同上 |
| 后端·handler | `app.go:5666` | `if input.ToolName != "Bash"` → 400 | 放开 Bash 校验，接受 `mcp__` 前缀；`permissionDecisionReason`（`:5724`）按工具类型生成 |
| 后端·handler | `app.go:5666` 之前 | 无白名单判定 | **新增自动放行判定**（§8.4）：命中 `auto_approve_tools` 直接返回 `allow`，不产生 pending |
| 前端·锚定 | `apps/web/src/lib/timeline.ts:125` | 靠 `toolInput.command === command` 配对 | 改为按 `tool_use_id` 锚定（服务端从 hook 输入带上，最稳），兼容无 `command` 的工具 |
| 前端·渲染 | `ConversationPage.tsx:674-689`（ToolCard）、`:760-775`（ApprovalBanner，标题 `:769`） | 文案写死「终端命令」/「此命令将会在当前项目目录执行。」/「等待确认命令执行」；`<code>{command}</code>` 在 MCP 下渲染成空块 | 展示 `server / tool / 参数 JSON`，文案改为工具调用语义；工具名做可读化（不直接显示 `mcp__x__y`） |

---

## 5. 数据模型

### 5.1 主表

```sql
create table if not exists mcp_servers (
  id            text primary key,                 -- mcp_<uuid>
  name          text not null,                    -- 注入后的 server key，须符合 [A-Za-z0-9_-]
  display_name  text not null default '',
  description   text not null default '',

  transport     text not null,                    -- stdio | http | sse

  -- stdio 专属
  command       text not null default '',
  args_json     text not null default '[]',
  env_json      text not null default '{}',       -- 明文 / ${VAR} 占位符 / sec_<uuid> 引用
  cwd           text not null default '',         -- 支持 ${PROJECT_DIR} / ${HOME}

  -- http / sse 专属
  url           text not null default '',
  headers_json  text not null default '{}',       -- 同上

  -- 作用域与适用范围
  scope         text not null default 'global',   -- global | project
  project_id    text,                             -- scope=project 时非空
  environments  text not null default '["windows","wsl","remote-linux"]',
  agents        text not null default '["claude-code"]',

  -- 策略
  enabled             integer not null default 1,
  auto_approve_tools  text not null default '[]', -- 形如 ["mcp__github__*"]（在 hook handler 内生效）
  startup_timeout_sec integer not null default 20,
  tool_timeout_sec    integer not null default 60,

  -- 元数据
  source        text not null default 'manual',   -- manual | import | preset
  created_at    datetime not null,
  updated_at    datetime not null
);
create unique index if not exists ux_mcp_servers_scope_name
  on mcp_servers(scope, coalesce(project_id,''), name);
```

> `environments` 的取值必须与 `agent_target_env.go:16-18` 的常量字面量严格一致：`windows` / `wsl` / `remote-linux`。

**迁移范式**（照 `preferences.go:45` `migrateAppPreferences`）：新增 `migrateMCPServers(ctx)`，建表后紧接 `ensureColumn` 补列，并在 `app.go:1451` 的迁移调用链末尾追加。**不建单例行**（MCP 是列表资源）。

### 5.2 项目级绑定（P1）

```sql
create table if not exists mcp_project_bindings (
  project_id  text not null,
  server_id   text not null,
  enabled     integer not null default 1,
  created_at  datetime not null,
  primary key (project_id, server_id)
);
```

P0 先只支持「scope=project 的专属定义」；全局 server 一律对所有项目生效。P1 再引入绑定表做细粒度开关。

### 5.3 密钥引用

| 形态 | 示例 | 处理 |
| --- | --- | --- |
| 明文 | `"LOG_LEVEL": "info"` | 原样写入 |
| 占位符 | `"ROOT": "${PROJECT_DIR}/docs"` | 按目标环境解析路径 |
| 环境变量引用 | `"GITHUB_TOKEN": "${MILEVIA_MCP_GITHUB_TOKEN}"` | **首选**：文件里留占位符，真值由 Milevia 注入 CLI 进程环境（若 §0.2 #14 假设成立） |
| 密钥引用 | `"GITHUB_TOKEN": "sec_9f3a…"` | 回退方案：运行时经 `profileSecretStore` 解密填入 |

**写入接口一律拒绝明文密钥**：标记为 secret 的字段在 `POST/PATCH` 时先换成 `sec_<uuid>` 再落库；`GET` 只回引用与「已设置」状态，**绝不回显明文**（同 docs/26 §7.3）。

---

## 6. HTTP API

```text
# 资源管理
GET    /api/mcp/servers                      列出（?projectId= &agentId= &scope=）
POST   /api/mcp/servers                      新增
GET    /api/mcp/servers/{id}                 详情
PATCH  /api/mcp/servers/{id}                 更新
DELETE /api/mcp/servers/{id}                 删除

# 能力
GET    /api/mcp/presets                      内置模板清单
POST   /api/mcp/servers/{id}/test            连接测试（目标环境执行，返回工具列表）
POST   /api/mcp/import                       从现有 Claude / Codex 配置导入（预览 + 确认两步）

# 项目视图
GET    /api/projects/{projectID}/mcp          该项目实际生效的 MCP（含来源与可用性）
PATCH  /api/projects/{projectID}/mcp          项目级开关（P1）
GET    /api/projects/{projectID}/mcp/status   当前会话最近一次注入的 server 与连接状态
```

**约束：**

- 经 `registerMCPRoutes` 分组注册，由 `app.go:953` 的 `routes()` 挂载，自动继承 `requireSession`（`app.go:1197`）。
- 后端校验：`transport` 枚举合法；`stdio` 必有 `command`、`http`/`sse` 必有合法 `url`；`name` 满足 `^[A-Za-z0-9_-]+$`（否则注入 JSON 的 key 非法）；`scope=project` 必有 `project_id`。**不信任前端枚举。**
- `POST /api/mcp/import` 第一遍只返回发现结果与重名情况，不落库；`confirm: true` 才写入。
- 所有接口不得回显 `sec_` 引用的明文。

---

## 7. 后端实现要点

### 7.1 配置生成器

新增 `mcp_config.go`：

```go
// buildMCPConfig 依据项目 + Agent + 目标环境 + 本次 run/会话，生成该次启动要注入的 MCP 配置。
// 返回落盘路径（或 Codex 的 -c 参数列表）、清理函数与是否启用严格模式。
func (s *Server) buildMCPConfig(
    ctx context.Context, project Project, agentID string, runKey string,
) (path string, codexArgs []string, cleanup func(), strict bool, err error)
```

步骤：① 按 `resolveAgentTargetEnv` 得到目标环境；② 取 `enabled=1` 且 `environments` 含该环境、`agents` 含 `agentID` 的条目，合并 global + project（project 覆盖同名）；③ 占位符解析（路径按环境、密钥优先留 `${VAR}`）；④ 按 Agent 序列化；⑤ 按环境落盘，**文件名含 `runKey` 唯一后缀**。

### 7.2 各环境注入实现

| 环境 | 落盘 | 参数 | 清理 |
| --- | --- | --- | --- |
| Windows | `<data>/mcp-runtime/<runKey>.json` | `--mcp-config <win路径> [--strict-mcp-config]` | 进程结束后 `defer` 删除；启动时扫掉超过 1 小时的残留 |
| WSL | **写 Windows 侧同一路径** | 用 `windowsToWSLMntPath(path)` 转 `/mnt/…` 后传 `--mcp-config` | 同 Windows（文件本就在 Windows 侧） |
| SSH | 与 CLI 同一条 shell 命令内 `base64 -d > /tmp/milevia-mcp-<runKey>.json` + `trap 'rm -f' EXIT` | `--mcp-config <远端路径>` | shell 退出即删 |

> **WSL 不要写 `\\wsl$\<distro>\…`**：写文件发生在拉起 CLI 之前，此时发行版可能未运行，UNC 不可写。走 `/mnt/<drive>` 是代码已验证的路径（`wsl_agent_run.go:289`）。

**目录与权限的现实说明（不要写「0600 保护」）**

- 运行时目录必须按 `profile-master.key` 的解析方式定位（`app.go:684-686`）：优先 `config.DataDir`，为空时回退 `filepath.Dir(config.DatabasePath)`。这样才落在 `.gitignore` 覆盖的 `data/` 下。
- Go 的 `os.WriteFile(path, data, 0o600)` **在 Windows 上不产生 Unix 语义的 0600**（走 ACL）；WSL 经 `/mnt/c`(drvfs) 读时 Unix 模式通常也被忽略。因此**真实保护来自「目录属于用户私有 profile」**，而不是文件模式位。文案与注释不要承诺 0600。
- 这也是一条论据：如果 §0.2 #14 的环境变量展开假设成立，文件里根本没有密钥，权限问题就从「必须保证」降级为「最好保证」。

### 7.3 Claude 注入（5 处，见 §4.2）

`AgentRunRequest` / `AgentSessionRequest` 增加 `MCPConfigPath string` 与 `StrictMCP bool`，由上层构造请求时填好 —— **保持 runner 只负责拼参数、不查库**，与现有 `Profile` / `ReadOnlyTools` 的传参风格一致。

本地/WSL：在 `args()`（`:621`）与 `sessionArgs()`（`:679`）中，于 `--settings` 之后、`--session-id` 之前插入：

```go
if request.MCPConfigPath != "" {
    args = append(args, "--mcp-config", request.MCPConfigPath)
    if request.StrictMCP { args = append(args, "--strict-mcp-config") }
}
```

SSH：`Run` 的两条 `fmt.Sprintf`（`:1013-1030`）与 `StartSession`（`:1214-1219`）各加一段 `mcpSetup` + `mcpArg`，形态照 `schemaSetup`（`:1118`）：

```go
mcpPath := "/tmp/milevia-mcp-" + runKey + ".json"
mcpSetup = fmt.Sprintf("printf '%%s' %s | base64 -d > %s && trap 'rm -f %s' EXIT && ",
    shellQuote(encoded), shellQuote(mcpPath), shellQuote(mcpPath))
mcpArg = " --mcp-config " + shellQuote(mcpPath) + " --strict-mcp-config"
```

**远端版本门槛（必须处理，且不是「报错」而是「挂起」）**

- SSH 用的是裸 `claude`（`ssh_runner.go:1016,1024,1215`），不走 `config.ClaudePath`，远端版本不可控。
- 需要三层探测与降级：
  1. **`--mcp-config` / `--strict-mcp-config` 是否存在** —— 不支持则跳过注入并给出可见提示，而不是让任务失败。
  2. **版本 ≥ v2.1.246（存疑 —— 实测未能复现）** —— 官方文档称低于该版本时 strict 会话仍会为「不加载的项目级 server」等待审批、在 `-p` 下**启动挂起**（比报错更危险，会静默卡住直到超时）。但 **2026-09-10 实测 2.1.245 与 2.1.266 行为一致、均 `exit=0`**，未能复现挂起（见 §0.4）。当前实现仍保留该门槛作为防御性措施，建议放宽为「仅探测失败时降级」。
  3. **降级要可见**：在项目视图与任务日志里明确写出「远端 Claude 版本不支持 MCP 注入，已跳过」。
- 探测结果按 runner 缓存（与 `sshRunner.Ready` / `Version` 同级），避免每次 run 都跑一次 `claude --version`。

### 7.4 Codex 注入（`-c` 方案）

不使用隔离 `CODEX_HOME`（`codex_runner.go:419-421` 只在 `api_key` 档案下才创建，其它情况强行隔离会丢 `auth.json` 认证）。改用 `-c` 点号路径注入，每条 server 一组参数：

```text
-c 'mcp_servers.<name>.command="npx"' \
-c 'mcp_servers.<name>.args=["-y","@modelcontextprotocol/server-filesystem","/path"]' \
-c 'mcp_servers.<name>.env_vars=["GITHUB_TOKEN"]' \
-c 'mcp_servers.<name>.startup_timeout_sec=20'
```

- **值必须是合法 TOML，不是「shell 引号包住就行」**。Windows 路径 `C:\Program Files\node\npx.cmd` 在 TOML 里要写成 `"C:\\Program Files\\node\\npx.cmd"`。**实现上应先用 TOML 编码器生成值文本**，再做 shell quote，禁止手工拼字符串（引号/反斜杠/Unicode 都会出错）。
- **密钥走 `env_vars` 而非 `env`**：argv 里只出现变量名，值由 Milevia 注入进程环境。
- **WSL**：`env_vars` 依赖变量存在于 Codex 进程环境，而 WSL 下只有 `wslForwardEnvKeys`（`wsl_agent_run.go:27-34`）列出的**静态前缀**会被 `WSLENV` 透传。MCP 密钥变量名是任意的，需把该机制扩展为「静态前缀 + 本次动态变量名集合」。
- **SSH 远端：目前无安全通道（v3 新增缺口）**。远端 Codex 的进程环境里必须有这些变量，而 `export GITHUB_TOKEN=…` 会出现在远端 shell 的 argv / `ps` 输出里，**破坏「密钥不进 argv」原则**。Claude 有 `base64 落盘 + trap rm` 通道，Codex-SSH 没有等价物。可选出路：① 远端临时 env 文件（`0600` + `trap rm`）+ `set -a; . file`；② 远端临时 `CODEX_HOME`（需一并复制 `auth.json`）；③ **P0 明确不支持 Codex-SSH 携带密钥型 MCP**，UI 说明。**需在实施前定夺。**
- **待实测验证**：逐条 `-c mcp_servers.<name>=…` 是与用户 `config.toml` 的 `mcp_servers` **逐 server 合并**，还是**整体替换整个表**。若为后者，需改为读取用户配置后整体重写 —— 那就会回到配置污染问题。**这一条必须在实现前用真实 Codex 验证。**
- **能力不对称**：Codex 无 `--strict-mcp-config` 等价物，用户自己 `~/.codex/config.toml` 中的 MCP 无法被屏蔽。需在 UI 上说明。

### 7.5 清理时机

Windows/WSL 的临时文件在进程结束后 `defer` 删除；异常退出时由下次启动的清理任务扫掉 `mcp-runtime/` 里超过 1 小时的残留。SSH 由 `trap … EXIT` 保证。**文件名带 `runKey`，因此并发 run 之间不会互删；清理只按「年龄」而不是「同名」判定。**

---

## 8. 权限与审批接入（P0 硬前提）

### 8.1 为什么必须做

`-p` 非交互模式下，未被预授权的工具调用会被**拒绝**（官方文档明确）。MCP 工具：
- 不匹配现有 hook 的 `Bash` matcher → 不会走审批；
- 不在 `--allowedTools` 里 → 不会被预授权。

两者叠加的结果是 **MCP 工具在默认权限模式下 100% 调用失败**。只有让 PreToolUse hook 命中介入并返回 `allow`，MCP 才算真正可用。因此这不是「防止后门」，而是「功能能否成立」。

> 该机制在生产中已验证：Milevia 的 `approval_required` 模式今天正是靠同一个 hook 返回 `allow` 让 Bash 落地的。

### 8.2 完整改动链路（6 处 + 1 处白名单闸门，见 §4.3）

**服务端 handler 要点**（`app.go:5666`）：

- 放开 `ToolName != "Bash"` 的硬校验：接受 `Bash` 与 `mcp__` 前缀的工具名，其余仍然拒绝。
- `permissionDecisionReason`（`:5724`）由写死的 `"Command " + decision + " by the project operator."` 改为按工具类型生成（Bash → 命令语义；MCP → 「工具调用」语义）。
- 超时维持 5 分钟（`:5713`）。

**并发审批要改造（v3 新增）**

`app.go:5696-5702` 目前对同一 run 的第二个待审批直接返回 **409**。MCP 工具在一个回合内被**并行调用**的概率远高于 Bash；此时第二个调用的 hook（`curl --fail` / `curl -fsS`）会因 409 失败，触发 Claude Code 的「非阻塞错误」路径，最终在 `-p` 下被拒 —— 表现为**部分 MCP 调用莫名其妙失败**。两种处理：

- **推荐**：服务端对同 run 的审批请求**排队**（FIFO），逐个交付用户裁决；
- 或至少返回结构化 `deny` + 可读原因，让模型看到明确拒绝，而不是让 hook 抛错。
- 顺带：`toolInput` 可能很大（MCP 参数常含文件内容），需设上限或截断后再入 `approval.pending` 事件与前端展示。

**前端要点**（`timeline.ts:125`、`ConversationPage.tsx:674-689,760-775`）：

- 锚定逻辑不能再依赖 `toolInput.command` 字符串相等。**改为让服务端在事件 payload 里带上 `tool_use_id` 直接锚定**（PreToolUse hook 输入原生含 `tool_use_id`，最稳）。
- `ToolCard`：`<b>{action.name === "Bash" ? "终端命令" : action.name}</b>` 在 MCP 下会显示 `mcp__github__create_issue`，需可读化（显示 `github · create_issue` 之类）；`{command && <pre className="command">}` 不渲染；审批文案「此命令将会在当前项目目录执行。」改为工具调用语义；展示 `server / tool / 参数 JSON`。
- `ApprovalBanner`（`:769`）：标题「等待确认命令执行」改为工具调用语义；`{description || command}` 在 MCP 下两者皆空 → 需回退到 `server · tool`；`<code className="approval-banner-command">{command}</code>` 会渲染成空块，需按类型切换。

### 8.3 只读分析路径

只读路径用 `--settings permissions.deny` 硬拒（`insightReadOnlySettingsJSON`，`insights.go:781`；清单 `insightReadOnlyDenyTools`，`insights.go:762`），本地 `claude_runner.go:623-632` 与 SSH `ssh_runner.go:984-993` **两条路径都生效**。

**P0 做法（双保险）**：
1. 只读任务**不注入** MCP（最省事）；
2. 同时在 `insightReadOnlyDenyTools` 补一条 `"mcp__*"` —— deny 规则支持工具名 glob，可一次性覆盖所有 server 的全部 MCP 工具。

> 这条兜底之所以可靠，正因为它不受 hook-allow 影响：**hook 返回 `allow` 不覆盖 deny 规则**。也就是说，即使未来某条只读路径漏掉了注入控制，`mcp__*` 仍会拦下。

> 适用范围说明：deny 清单在 `ReadOnlyTools > 0` 分支生效。`--permission-mode plan` 分支（`claude_runner.go:635-636`）既不装 hook 也不带 deny 清单，但 plan 语义 + `-p` 下需要确认的调用会被拒，**安全性同样成立**，只是机制不同。

### 8.4 自动放行白名单（`auto_approve_tools`）—— 机制已更正

**不要用 `settings.permissions.allow` 实现。** PreToolUse hook 是权限评估的**第 1 步**，且**在任何权限状态下都会触发**；Milevia 的 hook 又实现为「阻塞等人点」。allow 规则在第 5 步，永远轮不到 —— 白名单会完全失效。

**正确做法：在 hook handler 内部判定。** 请求到达 `waitForApproval` 后、创建 pending 之前：

1. 由 run / 会话上下文取出该 run **生效的自动放行清单**（与 `Profile` / `ReadOnlyTools` 同样的传参风格：入队时解析、随 run 上下文保存）；
2. 若 `tool_name` 命中清单（`mcp__<server>__*` 或 `Bash(<pattern>)` 形态），**立即返回 `permissionDecision: "allow"`**，不创建 pending、不发 `approval.pending` 事件；
3. 未命中才进入原有等待流程。

**兜底**：同时把规则写进 `settings.permissions.allow`，为将来可能出现的「无 hook 路径」留一层保险。注意 **allow 语法要求 `mcp__<server>__*`（server 段必须是字面量）**，`mcp__*` 这种无锚点写法会被忽略并告警。

**已知限制**：hook 返回 `allow` 不覆盖 deny / ask 规则；若某工具同时被 deny 命中（如只读路径的 `mcp__*`），仍会被拦 —— 这是期望行为。

**UI 需说明**：Milevia 目前**没有任何「始终允许」机制**（全仓 grep 零命中），本项是净新增能力；单次审批不会记忆，需用户显式加入白名单。

### 8.5 已知限制

若某 MCP server 在工具元数据里标记 `_meta["anthropic/requiresUserInteraction"]`（需 Claude Code ≥ v2.1.199），该工具即使命中 allow 规则也会回落要求用户交互，`-p` 下被拒 —— Milevia 的 hook-allow 无法覆盖。UI 需能显示「该工具要求交互确认，当前模式下不可用」这类可操作的原因，而不是泛化报错。

---

## 9. 安全设计

| 风险 | 说明 | 对策 |
| --- | --- | --- |
| **克隆即执行（供应链）** | `-p` 模式下项目 `.mcp.json` 的 server 会被**加载并启动子进程**，不弹审批（官方原文：`it loads project-scoped servers without asking`） | 默认 `--strict-mcp-config`；放行 `.mcp.json` 需用户在项目设置里显式开启，并列出具体 server 供确认。备选更细粒度闸门见 §2.3 |
| **strict 反而挂起**（实测未能复现，见 §0.4） | 官方称 Claude < v2.1.246 时，strict 会为「不加载的项目级 server」等待审批、`-p` 下挂起；**实测 2.1.245 与 2.1.266 一致、均 `exit=0`** | 仍保留远端版本探测与降级（§7.3）作防御性措施；建议放宽门槛，仅探测失败时降级 |
| **工具投毒 / 描述注入** | server 在 tool description 里埋隐藏指令（如 HTML 注释）诱导外泄 | ① 连接测试时展示完整 description 供审阅；② 对 description 做可疑模式检测（隐藏注释、`ignore previous instructions`、外链）并告警；③ 默认只连用户显式选择的 server |
| **跨 server 工具遮蔽** | 恶意 server 用同名工具劫持可信 server 的调用 | 注入时对 `name` 做全局唯一性校验；协议层 `mcp__<server>__` 前缀已限制混淆面 |
| **密钥泄露到命令行** | 内联 JSON 或 `-c env={…}` 会让凭据出现在进程列表 / 远端 `ps` | 首选：文件里只留 `${VAR}`，真值走进程环境；Claude 落盘 + 用后清理；Codex 用 `env_vars`。**Codex-SSH 无安全通道，需专门处理（§7.4）** |
| **密钥明文落盘** | 临时文件含明文密钥，读文件即泄露 | 优先避免落盘（`${VAR}`）；回退方案下依赖目录私有 + 用后清理；**不要声称 chmod 600 保护（Windows/WSL 不成立）** |
| **密钥明文入库 / 回显** | 数据库被读即泄露 | AES-GCM（`profile_secrets.go`）+ `sec_` 引用；API 永不回显明文 |
| **用户配置被污染** | 直接改 `~/.claude.json`、`~/.codex/config.toml` 会与用户终端操作互相覆盖 | Milevia 只写自己的临时文件、只传 `-c`/`--mcp-config` 参数 |
| **远端 `/tmp` 可读** | 同机其他用户可能读到配置 | 写后 `chmod 600`（Linux 侧有效）+ `trap rm`；文件名带 runKey 避免并发互删 |
| **审批参数入时间线** | MCP 工具参数可能很大或含敏感值，会写进会话事件 | `toolInput` 设上限 / 截断后再落事件与前端展示 |
| **过度授权** | 一个 MCP 拿到过宽凭据（如 GitHub admin token） | UI 与文档明确建议只读/最小权限凭据；对 `admin`/`*:*` 等特征给出提示 |
| **工具返回值的间接注入** | 工具返回值里嵌指令，间接操控模型 | 高风险写操作走审批（§8）；P2 可接注入分类器 |
| **上游默认行为变更** | `--bare` 将成为 `-p` 默认，不再自动发现 skills / 项目资产 | 风险仍在（§13）；探测与告警的实现在 §25 已撤回，届时需改为显式传参 |

---

## 10. 前端设计

### 10.1 首页入口

`DashboardPage.tsx:157-161` 的 `dashboard-actions` 已并列「设置 / 远程控制 / SSH连接」。新增同构按钮：

```tsx
<button className="dashboard-action dashboard-action-mcp secondary" title="MCP连接"
        onClick={() => navigate("/mcp-manager")}>
  <McpIcon /><span>MCP连接</span>
</button>
```

放在「SSH连接」之后、「设置」之前（资源管理类入口聚在一起，同 docs/26 §3.1 逻辑）。`App.tsx:80` 附近注册 `/mcp-manager` 路由。

> 与 SSH 同理：MCP 是全局资源且含凭据编辑，**独立页面**比塞进设置页更安全、信息承载力更强（docs/26 §5 已确立该原则）。

### 10.2 管理页结构

```text
MCP 连接
├── 服务器列表
│   ├── 名称 / 传输类型 / 作用域 / 适用环境 / 启用 / 连接状态
│   └── 操作：测试连接 · 查看工具 · 编辑 · 删除
├── 添加服务器
│   ├── 从模板（filesystem / fetch / github / playwright / context7 / memory / notion…）
│   ├── 从现有配置导入（Claude / Codex）
│   └── 手动配置
└── 项目视图（进入某项目时）
    ├── 生效清单（标注来源：全局 / 项目专属 / 项目 .mcp.json 被放行）
    ├── 降级提示（远端 Claude 版本不满足时的可见说明）
    └── 单项目开关与「是否放行项目 .mcp.json」
```

### 10.3 表单要点

- 按 `transport` 动态切换字段（stdio ↔ http/sse）。
- 密钥字段独立输入 + 掩码，旁标「保存后不可查看」；提交时换 `sec_` 引用，绝不放进普通 JSON 文本框。
- 环境选择器默认全选；stdio + 仅部分环境时提示「远端可能未安装该命令」。
- 占位符提示（`${PROJECT_DIR}`、`${HOME}`） + 「按环境预览解析结果」。
- **严格模式说明**：明确告知用户「默认不加载项目 `.mcp.json` 与你自己在终端里配置的 MCP」，避免误以为功能失效。
- 危险操作（删除、放行 `.mcp.json`、加白名单）走 `ConfirmDialog`。
- **保存即生效，不做「保存全部」**；变更只影响**新建**会话与任务，运行中的会话不热更新（避免工具集中途变化导致行为不可复现）。

### 10.4 工具列表与安全审阅

`POST /api/mcp/servers/{id}/test` 返回 tool 列表，渲染为可展开卡片：工具名、description 全文、参数 schema。命中可疑模式的高亮告警。若工具带 `requiresUserInteraction` 标记，标注「当前模式下不可用」。这是抵御工具投毒最有效的一环 —— 让用户在授权前看清 server 声称自己能做什么。

---

## 11. 分期实施

### P0：可用闭环

1. `migrateMCPServers` + `mcp_servers` 表（§5.1）。
2. `mcp_config.go` 配置生成器（合并、环境过滤、占位符、密钥处理、runKey 唯一命名）。
3. Claude 注入 5 处（§4.2）：本地/WSL 的 `args()`+`sessionArgs()`，SSH 的 `Run` 两分支 + `StartSession`。
4. 三环境落盘：Windows 临时文件、WSL 走 `windowsToWSLMntPath`、SSH base64 + `trap`；运行时目录按 `profile-master.key` 方式定位。
5. **完整审批链路 6 处 + 白名单闸门**（§4.3）：3 处 matcher + `app.go:5666` handler（含自动放行判定）+ 前端锚定改造 + 前端渲染改造；**并处理同 run 并发审批的 409（§8.2）**。
6. 只读路径：不注入 + `insightReadOnlyDenyTools` 补 `"mcp__*"`（§8.3）。
7. CRUD API + `registerMCPRoutes`。
8. 首页「MCP连接」入口 + `/mcp-manager` 列表/新增/编辑/删除 + 项目视图（只读）。
9. SSH 远端能力探测与降级：`--mcp-config` 支持性 + 版本门槛（当前实现为 ≥ v2.1.246，**经 2026-09-10 实测该门槛过度保守、建议放宽**，见 §0.4）。
10. **三项实施前必测假设**（见 §13）：Codex `-c` 合并语义、`--mcp-config` 是否支持 `${VAR}` 展开、远端 Claude 版本分布。

### P1：可观测与可验证

1. `POST /api/mcp/servers/{id}/test`：目标环境连接测试与 `tools/list` 发现。
2. 内置轻量 MCP client（`mcp_client.go`）：stdio 走目标环境 `execCommand` 拉起进程发 JSON-RPC；http/sse 走控制服务直连。
3. `POST /api/mcp/import`：解析 `~/.claude.json`、项目 `.mcp.json`、`~/.codex/config.toml`，两步确认导入。
4. 内置模板库 + tool description 可疑模式检测。
5. Codex `-c` 注入落地（若 P0 未完成验证则顺延）+ WSL 动态环境变量透传扩展 + **Codex-SSH 密钥通道定夺（§7.4）**。
6. `mcp_project_bindings` 绑定表与项目级细粒度开关。

### P2：生态与授权

> **状态：已全部完成（2026-09-10），实施记录见 §18。** 下列 4 项 + §17.9 挂账的 3 项收尾均已落地。

1. 远程 MCP 的 OAuth 2.1 + PKCE：本地回调服务器、浏览器唤起、token 加密落盘与刷新。
2. 工具级免审批白名单可视化（`mcp__<server>__*`）。
3. MCP 调用审计日志（server / tool / 参数摘要 / 结果状态）。
4. resources / prompts 展示（仅 Claude 支持；Codex 不支持）。

---

## 12. 验收与测试

### 12.1 注入

- 无启用 MCP 时，启动命令中**不出现任何 MCP 参数**（`--mcp-config` / `--strict-mcp-config`）。
  > 注意：`--settings` 的 hook matcher 会按设计从 `Bash` 变为 `Bash|mcp__.*`，因此**不能**用「与改造前逐字节一致」作为判据（v2 的该判据自相矛盾，已修正）。
- 全局与项目同名 server，注入后 project 定义胜出。
- `environments` 不含目标环境时不注入该条。
- `${PROJECT_DIR}` 在 Windows 解析为 `C:\…`、WSL 解析为 `/mnt/c/…`、SSH 解析为远端路径。
- 若采用 `${VAR}` 方案：配置文件中**不出现任何密钥明文**，且变量真值确实出现在 CLI 进程环境中。
- 若采用回退方案：`env_json` 中的 `sec_` 引用被解密填入；配置文件中**不出现 `sec_` 字面量**。
- **并发专项**：同一项目同时发起两个 run，两者的 `--mcp-config` 路径不同；先结束者清理后，后结束者仍能正常调用 MCP。
- 非法输入（stdio 缺 `command`、http 缺 `url`、`name` 含非法字符、`scope=project` 缺 `project_id`）→ 400。
- `GET /api/mcp/servers` 不返回任何密钥明文。
- **WSL 专项**：发行版处于停止状态时，注入仍然成功（验证走 `/mnt/` 而非 `\\wsl$`）。
- **SSH 专项**：远端 Claude 不支持 `--mcp-config`，或版本 < v2.1.246 时，任务**不失败也不挂起**，而是跳过/降级 strict 并给出可见提示。

### 12.2 审批（重点）

- 默认权限模式下，AI 调用 MCP 工具 → 时间线出现「MCP 工具调用」卡片（**含 server / tool / 参数**，不是空命令块）→ 允许后工具执行、拒绝后不执行。
- 审批横幅能正确定位到对应工具卡片（验证改为 `tool_use_id` 锚定后生效）。
- `auto_approve_tools` 中的工具**不产生 pending 事件、不弹审批**直接放行（验证是在 hook handler 内生效，而不是靠 allow 规则）。
- **并行专项**：同一回合内并行调用 2 个 MCP 工具，两者都能各自获得裁决（或第二个得到结构化的可读拒绝），不出现 hook 报错。
- 非 Bash 且非 MCP 的工具名仍被服务端拒绝（验证没有把校验放得过宽）。
- `full_control` 模式下 MCP 工具不弹审批（该分支不装 hook，与 Bash 行为一致）。
- 只读分析任务：即使库中有启用的 MCP，也**不注入**；且 `permissions.deny` 中含 `"mcp__*"`。
- **deny 优先级专项**：人为让某工具同时命中 hook-allow 与 deny，确认最终被拒（验证「hook-allow 不覆盖 deny」符合预期）。

### 12.3 前端

- 首页「MCP连接」可进入与返回；移动端不溢出。
- 传输类型切换时表单字段正确增删。
- 密钥字段提交后不可回显；再次编辑显示「已设置」而非原值。
- MCP 工具的卡片与横幅不出现空 `<code>` 块，工具名可读化。
- 删除、放行 `.mcp.json`、加白名单均有二次确认。
- 保存后新建会话立即生效；运行中的会话不被追溯修改。

### 12.4 安全回归

- 在项目根放置含恶意 server 的 `.mcp.json`，默认配置下**不启动**该 server 进程（用进程列表验证）。
- 显式放行 `.mcp.json` 后，其 server 出现在项目视图并被注入。
- `mcp-runtime/` 内容位于 `.gitignore` 覆盖范围内，`git status` 不出现运行时文件。
- 桌面端与 Web 端功能一致。

---

## 13. 风险与未决问题

| 项 | 说明 | 建议 |
| --- | --- | --- |
| **`--mcp-config` 是否支持 `${VAR}` 展开**（✅ 已实测支持，见 §0.4） | 官方只写明 `.mcp.json` 支持；实测**沿用成功** → 密钥可完全不落盘 | 切占位符方案（P1 首要改动）；「私有目录 + 用后清理」保留为兜底 |
| **Codex `-c` 合并语义**（✅ 已实测：逐 server 合并，见 §0.4） | 逐条 `-c mcp_servers.<name>=…` 是合并还是整体替换整个表 | **实测为逐 server 合并**、不污染用户配置 → §7.4 方案成立；仍需解决 Codex-SSH 的密钥通道 |
| **远端 Claude 版本** | SSH 裸 `claude`，版本不可控 | 能力 + 版本双探测 + 优雅降级（§7.3）。原假设「<2.1.246 会挂起」经实测**未能复现**（§0.4） |
| **Codex-SSH 密钥通道缺失** | `env_vars` 需要远端进程环境，`export` 会进远端 argv | 在「远端临时 env 文件 + trap rm」「远端临时 CODEX_HOME」「P0 不支持」三者中定夺（§7.4） |
| **requiresUserInteraction** | 带该标记的 MCP 工具在 `-p` 下无法被 hook-allow 拯救 | 记录为已知限制，UI 给出可操作提示 |
| **Codex 无 strict 等价物** | 用户自己的 `~/.codex/config.toml` MCP 无法屏蔽 | UI 明示；文档说明能力差异 |
| **WSL 动态环境变量** | `wslForwardEnvKeys` 是静态前缀表，MCP 密钥变量名任意 | 扩展为「静态前缀 + 本次动态集合」（§7.4） |
| **上游 `--bare` 成为 `-p` 默认** | bare 不自动发现 skills / 项目资产；Milevia 依赖 skills 自动发现（`skills.go:148-162`） | 探测与 UI 告警曾按 §19.8 实现，因无可执行动作已于 §25 撤回；风险仍在，届时需改为显式传参（如 `--plugin-dir` / `--settings`） |
| **远端依赖缺失** | stdio MCP 常在远端不可用（无 Node/Python） | 连接测试在目标环境执行（P1）；P0 至少在 UI 提示 |
| **会话中途工具集变化** | 用户改配置后长会话行为不可复现 | 已定：只影响新建会话（§10.3），UI 明说 |
| **OAuth token 存储位置** | 需与 `profile-master.key` 同级的加密存储 | P2 设计时统一，勿另起密钥体系 |

---

## 14. 关键源码依据

**注入点（5 处）**
- `apps/control-server/internal/app/claude_runner.go:621` `args()`、`:679` `sessionArgs()` —— 本地 + WSL 共用。
- `apps/control-server/internal/app/ssh_runner.go:1013-1030` `Run` 两分支、`:1214-1219` `StartSession` —— SSH 独立拼串，**不走 `args()`**。
- 请求构造点：`app.go:5106`、`:5164`、`:5252`（注入）；`insights.go:504`、`orchestration.go:2625`（只读，不注入）。

**审批链路（6 处 + 白名单）**
- `claude_runner.go:642`、`claude_runner.go:690`、`ssh_runner.go:1279` —— 三处 `PreToolUse` matcher（`grep '"matcher"'` 可复核）。
- `app.go:5666-5669` —— `ToolName != "Bash"` 硬校验；`:5696-5702` 同 run 并发审批 409；`:5713` 5 分钟超时；`:5724` `permissionDecisionReason`。
- `apps/web/src/lib/timeline.ts:124-127` —— 靠 `toolInput.command` 锚定审批。
- `apps/web/src/pages/ConversationPage.tsx:674-689`（ToolCard，`终端命令` 与「此命令将会在当前项目目录执行。」在 `:689`）、`:760-775`（ApprovalBanner，标题在 `:769`）。

**只读路径**
- `insights.go:762` `insightReadOnlyDenyTools`、`:781` `insightReadOnlySettingsJSON` —— 需补 `"mcp__*"`。
- 生效处：`claude_runner.go:623-632`、`ssh_runner.go:984-993`；`plan` 分支见 `claude_runner.go:635-636`。

**环境与路径**
- `agent_target_env.go:16-18`（三环境常量字面量）、`:37-58` `resolveAgentTargetEnv`。
- `wsl_discovery.go:191` `windowsToWSLMntPath`、`:134` `uncToWslPath`。
- `wsl_agent_run.go:196,353`（复用 `args()`/`sessionArgs()`）、`:289`（schema 走 `/mnt` 的范式）、`:27-34` `wslForwardEnvKeys`。
- `ssh_runner.go:1116-1119` —— base64 落盘 + `trap` 范式；`:974-983` SSH 审批 hook 命令拼装。
- `codex_runner.go:419-458` —— 隔离 `CODEX_HOME` 的触发条件（仅 `api_key`）。
- `app.go:684-686` —— `profile-master.key` 的目录解析（运行时目录须照此定位）。

**复用与参照**
- `skills.go:148-162` 技能发现（依赖自动发现，未传 `--plugin-dir`）、`:335` `discoverSkillsForProject`、`:384` `discoverSkillsRemote`。
- `agent_profiles.go:117-164` `managedCLIEnvironment`。
- `profile_secrets.go:115-154` AES-GCM 与 `sec_<uuid>`。
- `preferences.go:13-14`（`defaultClaudePermission = "approval_required"`）、`:45-63` `migrateAppPreferences`；`app.go:1267-1290` `ensureColumn`；`app.go:1413,1451-1462` 迁移链；`app.go:953-957` `routes()`；`app.go:1197-1232` `requireSession`；`app.go:1814-1824` `executionPolicy`。
- `ssh_connection.go:126-138` `registerSSHRoutes`。

**前端**
- `apps/web/src/pages/DashboardPage.tsx:157-161` `dashboard-actions`。
- `apps/web/src/App.tsx:80` 路由注册。
- `apps/web/src/stores/useUIPreferences.tsx:119-136`、`apps/web/src/lib/api.ts:18-89` 请求封装。
- `apps/web/src/lib/types.ts:34-37`（`Approval` / `ToolAction` 类型）、`components/NotificationProvider.tsx:122`（`approval.pending` 通知）。
- `apps/web/src/components/ConfirmDialog.tsx`。

**外部依据**
- Claude Code 2.1.266 `claude --help`（本机实测）：`--mcp-config <configs...>`、`--strict-mcp-config`、`--setting-sources`、`mcp` 子命令。
- Claude Code MCP 文档：三级作用域与优先级（同名整体覆盖、字段不合并）；`-p` 模式加载项目级 server **不再询问**；屏蔽项目级 server 的三条闸门（`disabledMcpjsonServers` / `--setting-sources` / `--strict-mcp-config`）；**strict 跳过审批需 ≥ v2.1.246**；`.mcp.json` 的 `${VAR}` / `${VAR:-default}` 展开。
- Claude Code 权限 / SDK permissions 文档：评估顺序 **Hooks → deny → ask → 权限模式 → allow → 回调**；PreToolUse **在任何权限状态下都会触发**；`-p` 下未预授权调用被拒；hook 返回 `allow` **不覆盖 deny/ask 规则**；`"mcp__*"` deny glob；allow 规则需 `mcp__<server>__` 字面量前缀；`requiresUserInteraction` 例外（≥ v2.1.199）。
- Claude Code hooks 文档：`matcher` 为工具名正则（`"Edit|Write"` 形式）；`hookSpecificOutput.permissionDecision` 四态与优先级 `deny > defer > ask > allow`。
- Claude Code headless 文档：`--bare` 跳过 hooks / skills / plugins / MCP / CLAUDE.md 自动发现，**将成为 `-p` 默认**。
- MCP 规范 2025-11-25：JSON-RPC 2.0 生命周期、tools/resources/prompts 原语、OAuth 2.1 + PKCE。
- Codex 配置文档：`-c/--config` 支持点号路径（`mcp_servers.context7.enabled=false`），值为 TOML 对象；`env_vars` 白名单转发；`[mcp_servers.*]` 的 `command`（stdio）或 `url`（streamable HTTP）二选一；无 strict 等价物；仅实现 tools 原语。

---

## 15. P0 实施记录（2026-09-10）

方案本身未变，以下记录**实际落地时的选择与该节的偏离**，后续维护以本节为准。

### 15.1 已实现清单

| 方案条目 | 落地位置 |
| --- | --- |
| `mcp_servers` 表 + 迁移 | `internal/app/mcp_servers.go` `migrateMCPServers`，在 `app.go` 迁移链 `migrateAppPreferences` 之后挂载 |
| 配置生成器 | `internal/app/mcp_config.go` `prepareMCPInjection` / `mcpServerEntry` / `resolveMCPPlaceholders` |
| Claude 注入（本地/WSL） | `claude_runner.go` `args()` / `sessionArgs()` 末尾 `appendMCPConfigArgs` |
| Claude 注入（SSH） | `ssh_runner.go` `Run` 两分支 + `StartSession`，`buildRemoteMCPSetup`（base64 落盘 + `trap rm`） |
| 三环境落盘 | Windows/WSL：`<data>/mcp-runtime/<runKey>.json`（WSL 经 `windowsToWSLMntPath` 转 `/mnt`）；SSH：`/tmp/milevia-mcp-<runKey>.json` |
| 审批链路 matcher | 3 处改为 `"Bash\|mcp__.*"`（`claude_runner.go` ×2、`ssh_runner.go` ×1） |
| 审批 handler | `app.go` `waitForApproval`：放开为 `isApprovableToolName`、自动放行前置判定、事件带 `toolUseId`、`permissionDecisionReason` 分类型 |
| 只读兜底 | `insights.go` `insightReadOnlyDenyTools` 增加 `"mcp__*"` |
| CRUD API | `registerMCPRoutes`（`/api/mcp/servers`、`/api/mcp/presets`、`/api/projects/{id}/mcp`） |
| 前端 | 首页「MCP连接」按钮；`pages/McpManagerPage.tsx`；`App.tsx` 路由 `/mcp-manager`；`timeline.ts` 改 `toolUseId` 锚定；`ConversationPage.tsx` ToolCard/ApprovalBanner 可读化 |
| SSH 能力探测 | `ssh_runner.go` `mcpCapability`（`--mcp-config` 支持性；strict 与 injectable 同档，版本 < 2.1.246 时照常启用并打可见警告——**门槛已于 §16 放宽**），结果按 runner 缓存，探测失败时降级并**在任务日志给出可见提示** |

### 15.2 与方案不同的三处选择

1. **密钥 P0 采用「回退方案」内联，而非 `${VAR}` 占位符。**
   §13 的 `${VAR}` 展开假设在 `--mcp-config` 上尚未实测；若假设不成立，MCP server 会拿到字面量 `${VAR}` 导致认证失败，属功能性破坏而非外观问题。P0 选择**解密后内联写入运行时文件**（文件位于私有数据目录、用后删除、不写日志、不含 `sec_` 字面量）。
   **→ 已于 §16 收尾：验证 1 确认支持展开后，Windows/WSL 已切到占位符方案；SSH 因无安全 env 通道保持内联。**

2. **同 run 并发审批不做排队，改为允许多个待审批并存。**
   §8.2 建议 FIFO 排队或返回结构化 deny。实际实现更简单且更贴合 UI：**移除原先的 409**，每个审批有独立 `approvalId`，前端按卡片各自裁决。MCP 并行调用不再互相阻塞。

3. **Codex 注入未在 P0 落地。**
   与 §11 的分期一致（P1 第 5 项）。`prepareMCPInjection` 对 `agentID != "claude-code"` 直接返回空，Codex 会话不受影响。§7.4 的两个未验证假设（`-c` 合并语义、Codex-SSH 密钥通道）仍需在 P1 实施前定夺。

### 15.3 前置验证结果（2026-09-10 已实测，详见 §0.4）

- `--mcp-config` **支持** `${VAR}` 展开（command/args/env）→ **已落地占位符方案**（Windows/WSL），见 §16。
- Codex `-c` 为**逐 server 合并**、不污染用户配置 → P1 可按原设计实施。
- 旧版 **2.1.245** 的 strict 行为与 2.1.266 一致、**未复现挂起** → `≥2.1.246` 门槛已放宽为「与 injectable 同档 + 低版本打警告」，见 §16。
- **仍未验证**：**真实 SSH 远端**上的 strict 行为（无可用远端；`wsl.exe` 被沙箱程序黑名单拦截，无法用 WSL 替代）。

### 15.4 测试

新增 `internal/app/mcp_config_test.go`：纯函数（glob 匹配、版本比较、占位符解析、密钥拆分/合并、`mcpSecretEnvName`）与集成用例（Windows 注入落盘与清理、环境/Agent 过滤、project 覆盖 global、自动放行存取、密钥占位符/内联两种模式）。`go test ./internal/app/` 全绿；前端 `tsc -b` 与 `vite build` 通过。

## 16. P0 收尾：三项决策落地（2026-09-10）

在 §0.4 三项实测与「最佳方案」判断的基础上，完成三项收尾改动。**判断主线**：安全缺口的代价是静默且不可见的，可用性风险的代价是可见且可重试的——两者不对称，不能互相置换。

### 16.1 SSH strict 门槛放宽（`ssh_runner.go`）

| 项 | 旧 | 新 |
| --- | --- | --- |
| `mcpStrictOK` 门槛 | `versionAtLeast(version, 2, 1, 246)` | 与 `mcpInjectable` 同档（`≥2.1.0`，即「支持 `--mcp-config` 就开 strict」） |
| 版本 < 2.1.246 | 关闭 strict（静默失去保护） | **照常启用 strict**，只在任务日志打一条可见警告 |
| 探测失败 / 版本无法解析 | 降级关闭 | 不变（仍降级，避免未知行为） |
| 缓存 | 增 `mcpVersion` 字段 | 版本一并缓存，供警告文案使用 |

`notifyMCPDowngrade` 更名为 `notifyMCPMessage`（新语义既含降级也含软警告），3 处调用同步更新。

### 16.2 密钥占位符（Windows/WSL），SSH 保持内联

**按环境分叉，不是一刀切**——收益只在有 env 通道的环境存在：

| 环境 | 配置文件生命周期 | env 通道 | 结论 |
| --- | --- | --- | --- |
| Windows 本地 | `<data>/mcp-runtime/*.json`，run 期间常驻 | `exec.Command.Env` + `managedCLIEnvironment` | **占位符** |
| WSL | 同上（经 `/mnt/c` 共享） | `WSLENV` 透传（`wsl_agent_run.go` 现成机制，加 `"MCP_"` 前缀即通） | **占位符** |
| SSH 远端 | `/tmp` 下 base64 落盘、`trap rm` 即删 | 无安全通道（`export` 会进远端 `ps`） | **保持内联** |

实现要点：

- `mcpInjection` 增 `Env []string`（`KEY=VAL`）；`resolveMCPValues(ctx, values, useEnvRefs)` 增模式参数：`true` 返回 `${MCP_SEC_<id>}` + env 增项，`false` 返回内联明文。
- 新增 `mcpSecretEnvName(ref)`：由 `sec_<uuid>` 派生稳定、shell 安全的变量名（非字母数字 → `_`）。
- `AgentRunRequest` / `AgentSessionRequest` 增 `MCPEnv []string`；本地 Claude 与 WSL Claude 的 4 处 `profileLaunch` additions 追加 `request.MCPEnv...`；`wslForwardEnvKeys` 增 `"MCP_"`。
- **SSH 保持内联不是妥协**：其配置文件本就是短命临时文件，改占位符要多一份「远端 env 文件 + source + trap rm」，安全性等价而复杂度翻倍。

### 16.3 自动放行白名单二次确认（前端）

`McpManagerPage.tsx`：保存时若 `autoApproveTools` 非空，先弹 `ConfirmDialog` 列出命中模式、说明「命中即不再弹审批、直接执行」，确认后才落库。自动放行是高权限设置，误配置会让 MCP 工具静默执行，值得一道确认。原 `submit` 拆为 `submit`（守卫）+ `performSave`（实际写入）。


---

## 17. P1 实施记录（2026-09-10）

P1 的主题是**可观测与可验证**——授权之前先让用户看清 server 声称能做什么。本轮落地 6 项。

### 17.1 连接测试与工具发现（`mcp_client.go`）

`POST /api/mcp/servers/{id}/test`：在**目标环境**执行 `initialize` + `tools/list`，返回工具清单与可疑标注。

- **stdio + Windows**：直接 `exec`，**交互式**握手（先 `initialize` 等响应，再 `notifications/initialized` + `tools/list`）——比流水线更贴近规范，能兼容要求「先完成 initialize」的实现。
- **stdio + WSL**：经 `wslAgentRunner.wslNativeCommand` 包裹同一命令，同样是交互式握手。
- **stdio + SSH**：远端 session 没有交互式通道，改用**流水线**——一次性写入三条消息后关闭 stdin，再读回 stdout 按 id 取响应。对顺序处理的 server 等价可用。
- **http / sse**：控制服务直连（Streamable HTTP）。处理 `mcp-session-id` 回传与 SSE 响应体（`data:` 行取最后一条带 result/error 的消息）。SSE 传输已被规范标记为过时；若对端只支持旧式 SSE 端点，会返回可读错误而非静默失败。
- **失败可操作化**：`mcpTestHint` 把「超时 / 命令未找到 / 远端未连接 / 进程立即退出」翻译成具体建议，并在 stdio 失败时附带子进程 stderr 尾部。

### 17.2 可疑工具元数据检测（反工具投毒）

`flagMCPToolMetadata` 对工具名/标题/描述做静态扫描，命中即标注（`info` / `warn` / `danger`）：

| 类别 | 例子 |
| --- | --- |
| `instruction_override` | “ignore all previous instructions”、「忽略之前的指令」 |
| `concealment` | “do not tell the user”、“keep this a secret” |
| `forced_invocation` | “always call this tool”、“必须先调用” |
| `exfiltration` | “send the contents of .env to http…”、“上传到…” |
| `credential_access` | “read the environment variables / api keys” |
| `hidden_text` | 零宽字符与双向控制符 |
| `encoded_blob` | 超长 base64 片段 |
| `unusual_name` | 工具名含非 `[A-Za-z0-9_.-]` 字符 |

目的是把「这句话在教模型做事，而不只是在描述工具」的地方挑出来提醒审阅——命中不必然代表恶意。

### 17.3 配置导入（`mcp_import.go`）

`POST /api/mcp/import`：两步确认（`confirm:false` 只预览，`confirm:true` 才写入）。

- **来源**：`~/.claude.json` 顶层 `mcpServers`（全局）、同文件的 `projects.<路径>.mcpServers`（项目级）、项目根 `.mcp.json`、`codex mcp list --json`。
- **Codex 不自己解析 TOML**：直接用官方命令的结构化输出，避免手写 TOML 解析器（也避免为此引入新依赖）。
- **密钥判定**：键名命中 `(?i)(token|secret|password|api[-_]?key|credential|pat|auth)` 的值在写入时换成 `sec_` 引用，明文不落库。
- **候选值与前端分离**：预览接口只回键名与来源，导入时后端重新从源文件读取真值，值不经过浏览器。
- **重名冲突**：同作用域同名已存在时标记 `conflict` 并默认跳过。
- **名称规约**：外部名称可能含非法字符，经 `sanitizeImportedServerName` 规约为 `^[A-Za-z0-9_-]+$`。

### 17.4 Codex `-c` 注入

`prepareMCPInjection` 不再对非 Claude 直接返回空，改为按 `agentID` 分叉：

- **stdio**：`-c mcp_servers.<name>.command/args/cwd/startup_timeout_sec/tool_timeout_sec`；env 一律走 **`env_vars`（只写变量名）**，真值随进程环境注入。
- **http / sse**：`-c mcp_servers.<name>.url`；`Authorization: Bearer <token>` 走官方推荐的 **`bearer_token_env_var`**；其余密钥头走 **`env_http_headers`**（头名 → 变量名），非密钥头走 `http_headers`。
- **TOML 值编码**：`tomlString` / `tomlStringArray` / `tomlStringMap` 生成合法 TOML 字面量（转义反斜杠、引号、控制字符）。**禁止手工拼引号**——Windows 路径 `C:\Program Files\node\npx.cmd` 在 TOML 里必须是 `"C:\Program Files\node\npx.cmd"`。
- **明文不进 argv**：注入参数只含变量名与 URL/命令，密钥仅存在于 `MCPEnv`（→ 子进程环境）。
- **WSL 动态变量名转发**：`env_vars` 的变量名是任意的（如 `GITHUB_PERSONAL_ACCESS_TOKEN`），不匹配 `wslForwardEnvKeys` 的静态前缀。`wslBuildEnv` / `wslNativeCommand` 增可变参数 `extraForward ...string`，把本次的 MCP 变量名显式列入 `WSLENV` 转发名单。
- **Codex-SSH 不支持**：`codexRunnerFor(remote)` 返回 nil，Milevia 本来就没有远端 Codex 通道；`env_vars` 在远端也没有安全 env 通道（§7.4 的结论保持）。
- **能力不对称**：Codex 无 `--strict-mcp-config` 等价物，用户自己 `config.toml` 里的 MCP 无法被屏蔽——UI 需说明。

### 17.5 项目级绑定（`mcp_bindings.go`）

`mcp_project_bindings(project_id, server_id, enabled, created_at, updated_at)`：

- 表只记录**显式覆盖**，无行等价于「启用」。这样新增全局 server 无需为每个项目补行。
- 绑定只作用于**全局** server；项目级定义由自身 `enabled` 列控制。
- `selectMCPServers` 在选定时排除「被本项目显式关闭的全局 server」。
- `PATCH /api/projects/{projectID}/mcp` 写开关（`enabled:true` 即删除覆盖行）；`GET` 返回绑定视图（含 `overridden` 标记）。

### 17.6 前端（`McpManagerPage.tsx`）

- 每个 server 卡片增「测试」按钮 → 测试抽屉：环境选择 + 结果面板（工具卡片按 `danger`/`warn` 着色，展示描述与命中片段）。
- 工具栏增「从现有配置导入」→ 导入抽屉：项目选择、来源状态、候选勾选（冲突/跳过项禁用）、确认导入。
- 项目视图增「项目开关」：逐条切换该项目是否注入某全局 server。

### 17.7 一个必须记下的实现陷阱

**SQLite 以 `SetMaxOpenConns(1)` 运行**（`app.go:655`）。在 `rows` 未关闭时发起第二个查询会**死锁**——第二个查询等一个被当前 rows 占用的连接，而 rows 要等遍历结束才释放。本轮初次实现把它写在 `selectMCPServers` / `listProjectMCPBindings` 里，直接导致测试 10 分钟超时。修法：**把绑定查询提到打开 rows 之前**。`profile_secrets.go:30` 已有一条同样的提醒注释，新增查询时应先看它。

### 17.8 验证

新增 `mcp_client_test.go`：TOML 编码（Windows 路径/引号/换行/控制字符）、可疑模式检测、JSON-RPC 解析（纯 JSON 与 SSE）、`tools/list` 解析、导入名称规约、密钥键判定、Codex `-c` 参数生成（stdio 与 HTTP 两条路径，断言明文不进 argv）、项目绑定生效与视图。

`go build` / `go vet` / `go test ./internal/app/` 全绿；前端 `tsc -b` + `vite build` 通过。

### 17.9 P1 中未做的部分

> 下列三项**均已在 P2 阶段补齐（2026-09-10）**，详见 §18.1 / §18.5 / §18.6。

- **P2** 全部：远程 MCP 的 OAuth 2.1 + PKCE、工具级白名单可视化、调用审计、resources/prompts 展示。
- `GET /api/projects/{projectID}/mcp/status`（当前会话的注入状态）未实现——会话级状态需额外埋点。
- 导入未做「Codex 项目级 `.codex/config.toml`」与「WSL 内 `~/.claude.json`」的读取（只读控制服务所在 Windows 用户的配置）。

---

## 18. P2 实施记录（2026-09-10）

P2 在 §11 中定义为 4 项，另有 §17.9 挂账的 3 项收尾。本轮全部落地。

### 18.1 远程 MCP OAuth 2.1 + PKCE（`mcp_oauth.go`，新增）

只对 `http` / `sse` 传输生效（stdio 是本机子进程，没有「授权」概念）。

- **发现链路**（三段，全部可选降级）：
  1. `GET /.well-known/oauth-protected-resource`（RFC 9728）→ 拿 `authorization_servers[0]`；
  2. 对授权服务器取 `/.well-known/oauth-authorization-server`（RFC 8414）与 `/.well-known/openid-configuration`（OpenID Discovery）；
  3. 无 `registration_endpoint` 时报错并提示手工注册 client，不做静默猜测。
- **客户端**：优先动态客户端注册（RFC 7591）并**复用已注册 client**（避免每次授权都在服务商侧留一条记录）；`token_endpoint_auth_method` 按注册响应决定，`client_secret` 加密后落 `sec_` 引用。
- **PKCE**：`S256`，verifier 32 字节随机 → challenge = base64url(sha256)。
- **回调**：`mcpOAuthCallbackPath = /api/mcp/oauth/callback`（loopback）。该路径在 `requireSession` 中**显式放行**——回调是浏览器顶窗导航，无法携带 `X-Milevia-Session`。安全性由 32 字节随机 `state` 承担：拿不到 state 就无法把授权码换成令牌。
- **授权范围**：**默认不请求任何 scope**（空 `scope` 参数）。少要权限优先于便利。
- **令牌存储**：`mcp_oauth_tokens`（`server_id` 主键）——`access_token` / `refresh_token` / `client_secret` 全部经 `profileSecrets` 加密为 `sec_` 引用，表里不落明文。
- **刷新**：`mcpOAuthRefreshSkew = 60s`，注入前若剩余寿命不足则先用 `refresh_token` 静默续期。
- **流程状态**：`mcpOAuthFlows`（内存 map，`flowTTL = 10min`）→ 前端 `POST /oauth/start` 拿到 `flowId` 后轮询 `GET /api/mcp/oauth/flows/{flowID}`（`pending` / `done` / `error`）。
- **注入接线**：`mcpServerEntry`（Claude）与 `codexServerArgs`（Codex）在**未显式配置 Authorization 头**时自动附加 OAuth 令牌——显式配置优先，不会被覆盖。

### 18.2 MCP 调用审计（`mcp_audit.go`，新增）

- 表 `mcp_call_audit`：`id / tool_use_id / conversation_id / run_id / server_name / tool_name / args_preview / decision / status / error_text / duration_ms / created_at / updated_at`，`tool_use_id` 与 `conversation_id` 各一索引。
- **入口**：`waitForApproval` 的 PreToolUse 分支记 `recordMCPCallStart`（写 `decision`），PostToolUse 分支记 `recordMCPCallFinish`（写 `status` + `duration_ms`）。自动放行同样入账（`decision=auto_allow`）——它是一次真实调用，只是没弹审批。
- **裁决取值**：`allow` / `auto_allow` / `deny` / `aborted`（客户端断开）/ `timeout`（审批超时）。**「为什么是这个结果」本身就是审计最有价值的部分**，故 `aborted` 与 `timeout` 分开记。
- **脱敏**：只存参数摘要（`mcpAuditArgsLimit = 600` 字符），命中 `looksLikeSecretKey` 的键值替换为 `***`。明文密钥永不入表。
- **保留**：`mcpAuditRetention = 2000` 条，读接口顺手清理（避免写路径每次跑子查询）。
- **独立写入上下文**：`mcpAuditWriteContext` 在请求 ctx 已取消时切到 5s 超时上下文，保证「审批超时 / 客户端断开」这类事件仍被记下。
- **耗时**：用 `julianday()` 差值算毫秒，与 `created_at` 同源，不受进程时钟漂移影响。
- `GET /api/mcp/audit`（`projectId` / `serverName` / `decision` / `limit` / `offset`）与 `DELETE /api/mcp/audit`。

### 18.3 工具级免审批白名单可视化

- 新增 `PUT /api/mcp/servers/{serverID}/auto-approve`，只改 `auto_approve_tools` 一个字段——走整表 PATCH 会把其它字段的零值语义卷进来。
- 模式校验 `normalizeMCPAutoApprovePatterns`：`^(Bash(\(.+\))?|mcp__[A-Za-z0-9_-]+__[A-Za-z0-9_*.-]+)$`。
  - **明确拒绝 `mcp__*` 无锚点写法**：它会让任何 server 的任何工具静默执行，与「工具级」语义相悖。要全量放行某 server 用 `mcp__<server>__*`。
  - 写入时进一步校验模式必须属于**本 server 的 `mcp__<name>__` 前缀**，防止把别的 server 的白名单塞进来。
- 前端在**测试连接抽屉的工具列表**里逐条勾选「免审批执行」，命中即调该接口，并弹二次确认（复用了 P0 已建的确认语义）。

### 18.4 resources / prompts 展示（`mcp_client.go`）

- 握手拿到 `tools/list` 后追加 `resources/list`（id=3）与 `prompts/list`（id=4）两条可选请求，`mcpProbeOptionalTimeout = 4s`，**失败静默忽略**（很多 server 不实现它们）。
- 三条传输路径（interactive / pipelined / HTTP）均覆盖；单次最多解析 `mcpMaxProbeResources = 200` 项。
- 前端测试结果面板增「资源与提示词」区块，仅在非空时渲染。

### 18.5 注入状态端点 + 项目 `.mcp.json` 放行（`mcp_project_settings.go`，新增）

- 表 `mcp_project_settings(project_id, allow_mcpjson, updated_at)`。
- `prepareMCPInjection` 的**全部 4 个返回分支**都调 `recordProjectMCPInjection` 记录快照（内存 `mcpLastInject`），`GET /api/projects/{projectID}/mcp/status` 回读。前端在项目视图展示「最近一次注入」。
- `Strict` 由写死的 `true` 改为 `!s.projectMCPAllowMcpJson(ctx, projectID)`；`projectMCPView` 增 `allowMcpJson` 与 `mcpJsonServers`（后者列出 `.mcp.json` 里发现但未生效的 server 名，供用户核对）。
- 放行是企业级风险开关（`.mcp.json` 中的 server 绕过 Milevia 的白名单与审计，且项目被 AI 改动后即刻生效），故前端**必须二次确认**，并给出红色告警文案。

### 18.6 导入扩展（`mcp_import.go`）

来源从 4 个扩到 6 个：

| # | 来源 | 说明 |
|---|---|---|
| 1 | Windows `~/.claude.json` | 已有 |
| 2 | 项目 `.mcp.json` | 已有 |
| 3 | Windows `~/.codex/config.toml` | 已有 |
| 4 | 项目 `.claude/settings.json` 等 | 已有 |
| 5 | **项目 `.codex/config.toml`** | 新增，经 `parseCodexProjectMCPConfig` |
| 6 | **WSL 内 `~/.claude.json`** | 新增，经 `readWSLUserFile`（`wslNativeCommand` 执行 `cat`） |

`mcpImportCandidate` 增 `Warnings []string`：解析到但**未静默导入**的键（`bearer_token_env_var` / `env_vars` / `env_http_headers`）在此提示「需手工补配」——否则会得到一个「看起来配好了但连不上」的 server。

### 18.7 Codex 项目级 TOML 解析（`codex_toml.go`，新增）

为 `.codex/config.toml` 手写**受控子集解析器**，不引入完整 TOML 依赖：`normalizeTOMLText` / `stripTOMLComment`（识别字符串内的 `#`）/ `tomlSectionName`（兼容 `[[...]]`）/ `splitTOMLPath`（含带引号段）/ `splitTOMLAssignment` / `tomlValueBalanced`（多行数组）/ `parseTOMLString`（转义）/ `parseTOMLStringArray` / `parseTOMLStringTable` / `parseTOMLBool`。行为由单测锁定。

### 18.8 审批 hook 的 settings 生成（`claude_runner.go` / `ssh_runner.go`）

- 新增 `mcpApprovalHooksSettingsJSON(command)`：`PreToolUse`（matcher `Bash|mcp__.*`，timeout 310）+ `PostToolUse`（matcher `mcp__.*`，timeout 60），整体 `json.Marshal` 编码。
- **PostToolUse 只挂 `mcp__.*`**：不给每次 Bash 调用多付一次网络往返。
- **不改动已部署的 `cmd/approval-helper`**：它已把 stdin 原样转发，服务端按 `hook_event_name` 分流即可。
- SSH 形态原先**手工拼 `{"hooks":...}` 字符串**，而 hook 命令是 `curl ...` 且内含引号，存在生成非法 JSON 的隐患。改为统一走 `mcpApprovalHooksSettingsJSON` + `shellQuote`，并有 round-trip 单测断言。

### 18.9 前端（`McpManagerPage.tsx` / `types.ts` / `style.css`）

- server 卡片对 `http` / `sse` 增「授权」按钮 → 授权抽屉：状态（已授权 / 已过期 / scope / 到期时间 / 是否支持自动续期）+ 开始 / 重新授权 + 清除授权；发起后开新窗并轮询流程状态，完成后自动刷新。
- 测试抽屉工具列表增「免审批执行」勾选（带二次确认）；增「资源与提示词」区块。
- 项目视图增：`.mcp.json` 放行开关（二次确认 + 红色风险文案）、`.mcp.json` 中发现的 server 名、「最近一次注入」快照。
- 工具栏增「调用审计」→ 审计抽屉：server 筛选、刷新、清空、记录列表（裁决 / 状态 / 耗时 / 参数摘要 / 错误）。
- 导入抽屉展示 `warnings`。

### 18.10 一个必须记下的实现陷阱

`probeStdioPipelined` 内原有局部类型 `type outcome struct`，本轮引入同名变量后会报 `cannot assign to outcome` / `outcome (type) is not an expression`。把局部类型改名为 `runResult` 即可。**同一函数内不要用与局部类型同名的变量名。**

### 18.11 验证

- 新增 `mcp_p2_test.go`（10 个用例）：Codex TOML 解析（含带引号表名 / 内联表 / 多行数组 / 未导入键的 warning）、TOML 值辅助函数、白名单模式规约、`mcp__<server>__<tool>` 拆分、参数摘要脱敏、结果状态判定、审计读写 round-trip、`.mcp.json` 放行对 `StrictMode` 的影响、`.mcp.json` server 名列举、hook settings JSON 含 PostToolUse。
- `go build ./...` / `go vet ./internal/app/` 通过；`go test ./internal/app/ -run 'TestParseCodex...|TestTOMLValueHelpers|...'` 全绿。
- 前端 `tsc -b --force` + `vite build` 通过，`npm test` 210/210 通过。
- **既有环境性失败（与本轮无关，勿误判为回归）**：本机 sandbox 拦截 `wsl.exe` / `reg.exe`，导致 `TestOrchestrationDispatchLinksIntentAndRunInOneTransaction`、`TestNewConversationUsesApplicationDefaultsWhenRequestOmitsThem` 等返回 503；Windows 非特权下符号链接测试（`TestSyncCodexSkillTreeRejectsTargetSymlink` 等）失败。这些用例单跑仍失败，属环境限制。

### 18.12 至此 P0 / P1 / P2 全部完成

§17.9 挂账的三项已全部销账。若后续要继续推进，方向是 §13 中列出的未决问题（而非分期实施项）。

---

## 19. 正文缺口补齐记录（2026-09-10，第二轮）

§18.12 之后做了一次「以正文为准」的反向核查：不只看分期清单（P0/P1/P2）是否打完勾，而是把 §8.4 / §8.5 / §9 / §10.3 / §10.4 / §12.4 / §13 逐条 `grep` 到代码里验证。结果发现**分期清单 100% 完成，但正文里另有 8 处「写了要求、代码没落实」**（记为 A–H）。本轮把这 8 处全部补上。下面逐项说明做法与取舍。

### 19.1 A. 检测 `requiresUserInteraction` 标记并给出可操作提示（`mcp_client.go`）

**背景（§8.4 提到但未落实）**：Claude Code ≥ v2.1.199 支持工具在 `_meta["anthropic/requiresUserInteraction"]` 声明「本工具需要交互确认」。带此标记的工具即便命中 allow 规则，在 `-p` 下仍会回落要求用户交互并被拒——**这是 hook-allow 覆盖不了的**。原实现只在工具卡上泛化地提示「可能需要审批」，用户拿到的是无信息量的「被拒绝」，无从判断原因。

**实现**：

- `mcpToolInfo` 增字段 `RequiresInteraction bool`（`json:"requiresInteraction,omitempty"`）。
- `buildProbeOutcome` 的响应形态补收原始 `_meta`，构造工具时调 `mcpToolRequiresInteraction(tool.Meta)`。
- 新增 `mcpToolRequiresInteraction(meta json.RawMessage) bool`：兼容 `anthropic/requiresUserInteraction` 与 `anthropic/requires_user_interaction` 两种键名，且接受 bool 与字符串 `"true"`（不同 server 实现不一致，宽松解析避免漏判）。
- `flagMCPToolMetadata` 追加 `requires_interaction` 标注，`Severity: "warn"`，`Note` 写明「加入免审批白名单也无法覆盖」。

**顺带修的一个 bug**：`flagMCPToolMetadata` 原在「haystack 为空」时**提前返回 nil**——但一个工具可能只有标记、没有描述，此时 haystack 为空会**直接丢掉 requires_interaction 标注**。改为去掉提前返回，末尾统一 `if len(flags) == 0 { return nil }`。

> `mcpToolFlag` 新增的 `Note` 字段与既有的 `Detail` 语义区分：`Detail` 是**命中原文的片段**（证据），`Note` 是**给用户看的解释/建议**（结论）。前端只把 `Note` 渲染为独立提示行，`Detail` 仍走「命中片段」样式。

### 19.2 B. 前端展示工具参数 schema（`McpManagerPage.tsx`）

**背景（§10.3 要求「工具列表应能看到参数 schema」）**：后端 `tools/list` 早已回传 `inputSchema`（`mcpToolInfo.InputSchema`），但前端从未渲染。

**实现**：工具卡片内增 `<details className="mcp-tool-schema"><summary>参数 schema</summary><pre>{mcpSchemaText(tool.inputSchema)}</pre></details>`。新增顶层辅助函数 `mcpSchemaText(schema)`：`JSON.stringify(schema, null, 2)`，序列化失败时退化为 `String(schema)`（避免脏数据把整页渲染打挂）。

### 19.3 C. 审批参数设上限并截断后落事件（`mcp_audit.go` + `app.go`）

**背景（§9 要求「审批事件里的 toolInput 要设上限」）**：原实现把 `input.ToolInput` **原样**塞进 `approval.pending` / `approval.<decision>` 事件。一个 MCP 工具（尤其带文件内容的写工具）可以携带任意大的 `toolInput`，直接落进事件表会造成不必要的膨胀。

**实现**：新增 `truncateApprovalToolInput`，三级策略：

| 级别 | 阈值 | 处理 |
| --- | --- | --- |
| 整体 | 32 KB | 超限退化为 `{"_truncated":true,"_originalBytes":N,"_note":"…"}`，**不再尝试逐键保留** |
| 单键（普通） | 1024 字符 | 按 rune 截断并附「（已截断，原始 N 字符）」 |
| 单键（`command`） | 16 KB | 单独放宽，见下 |

**`command` 键必须逐字保留**：前端 `lib/timeline.ts` 靠 `toolInput.command` 字符串相等把审批横幅锚定到对应工具卡（§0.1 第 4 条就是这个坑）。若对 `command` 施加 1KB 上限，长命令被截断后**锚定会失败，审批横幅不显示**。故引入 `approvalToolInputKeepKeys = {"command": true}`，对该键单独放宽到 16KB，并写了专门的回归测试 `TestTruncateApprovalToolInputPreservesCommand` 锁住这一行为。

`app.go` 的 `waitForApproval` 两处 `json.RawMessage(input.ToolInput)` 改为 `truncateApprovalToolInput(input.ToolInput)`。

### 19.4 D. `settings.permissions.allow` 兜底规则（`claude_runner.go` / `ssh_runner.go`）

**背景（§8.4 要求「除 hook 判定外，也写一份 allow 规则作纵深防御」）**：原 `mcpApprovalHooksSettingsJSON` 只生成 hooks 段，不写 `permissions.allow`。

**必须先说清语义**（否则后来者会误判）：按 §0.2 第 10 条，PreToolUse 是权限评估**第 1 步且总会触发**，而 `permissions.allow` 在第 5 步——**Milevia 的 hook 是阻塞等人的，所以 allow 规则永远轮不到**。写 allow **不是**为了实现放行（放行由 hook handler 内的白名单判定完成），而是为「**未来若出现不挂 hook 的路径**」留一层兜底。代码注释与本节都写清这一点。

**实现**：

- 签名变更 `mcpApprovalHooksSettingsJSON(command string, allowPatterns []string) string`：有合法 allow 时写入 `settings["permissions"] = {"allow": [...]}`。
- 新增 `mcpPermissionsAllow(patterns []string) []string` 做**规约收紧**：保留 `mcp__<server>__*`（要求 `mcp__` 后**必须再含 `__`**，因为 server 段必须是字面量）与 `Bash` / `Bash(...)`，其余一律丢弃，末尾 `dedupeStrings`。

  > 初版只透传 `mcp__` 前缀、丢弃 `Bash`。重新权衡后改为同时透传 `Bash` / `Bash(...)`——allow 语法本身支持该形态，让兜底语义与白名单尽量对齐。测试相应更新为期望 4 项，并新增断言「**全是非法模式时不写出 permissions 段**」。
  >
  > 反面：`mcp__*` 这种**无锚点**写法会被 CLI **忽略并告警**（§0.2 相关条目），故规约函数把它过滤掉。

- `args()` 与 `sessionArgs()`（本机 CLI）两处、`sshClaudePermissionArgs`（SSH，两个调用点）均串上 `request.MCPAutoApproveTools`。
- `AgentRunRequest` / `AgentSessionRequest` 各增 `MCPAutoApproveTools []string`；`app.go` 的 `StartSession` 与 `runAgent` 从注入快照取值填入。

### 19.5 E. `.gitignore` 显式排除 `mcp-runtime/`（`.gitignore`）

**背景（§12.4 要求）**：MCP 运行时配置默认落在 `<DataDir>/mcp-runtime`，虽然通常已被 `data/*` 覆盖，但 `DataDir` **是可配置的**——一旦被配到别处，临时配置（可能含 server url / header 名等）就可能被误提交。

**实现**：在 `# Runtime data` 段追加 `mcp-runtime/`，并附注释说明「按目录名显式再排除一次」。

### 19.6 F. 按环境预览占位符解析结果（`mcp_preview.go`，新建）

**背景（§10.4 要求「保存前能预览 `${PROJECT_DIR}` 在不同环境下解析成什么」）**：原实现只在保存后由注入链路解析，用户无从预知 `C:\…` / `/mnt/c/…` / 远端路径的差异。

**实现**：新增 `POST /api/mcp/preview`（`mcp_preview.go`，约 130 行），接收**表单字段**而非 serverID（预览发生在**保存之前**）。返回解析后的命令/参数/URL 逐项结果，并按目标环境给出 note。关键点：

- **不涉及密钥**：预览**不做任何 `sec_` 解密**——表单里已保存的凭据本就只以键名存在，天然不涉及明文（写进了类型注释）。
- `containsUnresolvedPlaceholder` 检测剩余的 `${...}`（如 `${HOME}`），提示这些将交给 CLI 按进程环境展开、Milevia 不代解。
- 路由注册在 `mcp_servers.go` 的 `registerMCPRoutes`。

**过程中发现并修掉一个真实 bug**：`resolveMCPPlaceholders` 原来只判断 `strings.Contains(value, "${PROJECT_DIR}")`，**没判断 `projectPath` 是否为空**。未选项目时 `${PROJECT_DIR}` 会被替换成**空串**，静默产出一个缺路径的命令。测试 `TestPreviewMCPServerNotesUnresolvedProjectDir` 抓到该问题（got `[]string{"", "${HOME}"}`）。修复：

```go
// projectPath 为空时**原样返回**：否则 `${PROJECT_DIR}` 会被替换成空串，静默产出一个
// 缺路径的命令（预览与诊断场景尤其容易踩到，那时项目可能还没选）。
func resolveMCPPlaceholders(value string, target agentTargetEnv, projectPath string) string {
    if projectPath == "" || !strings.Contains(value, "${PROJECT_DIR}") {
        return value
    }
    ...
}
```

这个修复同时改善了**所有调用方**——注入路径本就不该在 `projectPath` 为空时做替换。`mcp_preview.go` 的 `resolve` 闭包改为**先置 `mentionsProjectDir = true` 再调用**，保证未选项目时仍能给出「未选择项目」的 note。

### 19.7 G. 过度授权特征提示（`mcp_client.go`）

**背景（§8.5 要求「识别形如 `*:*` / 管理员权限等过度授权描述」）**：原 `mcpFlagRules` 只有投毒类规则，没有「权限过宽」类。

**实现**：`mcpFlagRules` 追加 6 条 `broad_scope` 规则：

| 规则片段 | 命中示例 |
| --- | --- |
| `\*:\*` | `*:*` |
| `admin(istrator)?\s+(access\|privileges?\|permissions?\|scopes?\|rights?)` | `administrator privileges` |
| `full\s+(admin\|access\|control\|privileges?)` | `full access` |
| `unrestricted\|all[\s_-]?permissions?` | `all permissions` / `all-permissions` |
| `(admin\|manage\|write\|delete):[\w*]+` | `admin:read` / `write:all` |
| `管理员权限\|超级用户\|完全控制\|全部权限\|所有权限\|不受限制` | 中文描述 |

命中的工具在工具卡上出现 `broad_scope` 提示，`Severity: "warn"`。

### 19.8 H. `--bare` 纳入版本探测与告警（4 类 runner + 前端）

> **⚠️ 本节实现已于 2026-09-15 撤回**（理由与范围见 §25）。保留原文仅作历史记录与实现参考，
> 描述的不是当前代码。

**背景（§13 前瞻风险）**：上游计划把 `--bare` 设为 `-p` 的**默认行为**，届时 CLI 将**不再自动发现 skills / 项目资产**——而 Milevia 依赖该自动发现（§22 对话页 Skill 区、项目级 AI 配置）。

**取舍：探测而非版本猜测**。是否为 `-p` 默认取决于**上游未来版本**，Milevia 无法预判，按版本号猜会产生误报。改为**探测 CLI `--help` 里是否已出现 `--bare`**；结果按 runner **缓存一次**（`sync.Once`），避免每次 `listRunners` 都付一次进程启动代价。

**实现（覆盖 4 类 runner）**：

- 新增接口 `bareFlagReporter { BareFlagAvailable(ctx) bool }`。
- `claudeCLIRunner`：`bareProbeOnce` / `bareProbeAvailable`，跑 `claude --help`，`strings.Contains(out, "--bare")`。
- `sshRunner`：`bareMu` / `bareChecked` / `bareAvailable`（手动缓存，因需持锁）。
- `windowsAgentRunner`：`bareOnce`（`(claude --help 2>$null) | Out-String`）。
- `wslAgentRunner`：`bareOnce`（经 `wslBridgeProbe` 跑 `claude --help`）。
- `app.go` 的 `listRunners`：claude 条目由 `map[string]string` 改为 `map[string]any`，命中时写入 `bare: true` + `reason`。
- 前端 `ToolStatus` 增 `bare?: boolean`；`ConversationPage.tsx` 在 `runner-inline` 之后增独立徽章 `.runner-inline-warn`（**注意**：claude 分支原本只在**不可用**时渲染 `reason`，可用时只显示版本号，故 `bare` 告警必须用独立可见元素，不能挂到原 `reason` 上）。

### 19.9 验证

- `go build ./...` + `go vet ./internal/app/` 通过。
- MCP 相关**定向测试 33 个全 PASS**（`grep -cE "^--- PASS"` = 33）。
- 前端 `tsc -b --force` 通过、`npm test` **210/210**、`vite build` 通过。
- **全量后端测试**（`go test ./internal/app/ -count=1 -timeout 900s`，耗时 8m11s）：仅 7 个失败，**全部为既有环境性失败，与本轮改动无关，勿误判为回归**：

  | 失败用例 | 根因 |
  | --- | --- |
  | `TestTaskAwaitingReviewReleasedJobCanBeRedispatched` | sandbox 拦截 `wsl.exe` → 注册失败 |
  | `TestOrchestrationDispatchLinksIntentAndRunInOneTransaction` | 同上，返回 503 |
  | `TestNewConversationUsesApplicationDefaultsWhenRequestOmitsThem` | 同上，返回 503 |
  | `TestProvisionCodexProfileSkillsPreservesNestedSystemSkills` | 隔离 `CODEX_HOME` 内找不到内置 `.system` skill |
  | `TestSyncCodexSkillTreeRejectsTargetSymlink` | Windows 非特权下无法建符号链接 |
  | `TestLocalFilesystemRenameMovesSymlinkNotTarget` | 同上 |
  | `TestLocalFilesystemWriteFileRejectsFinalSymlink` | 同上 |

  判定依据：失败信息本身即环境性（`wsl.exe: Access is denied.`、符号链接「The system cannot find the file specified / write through symlink succeeded」）；且这 7 个用例所属子系统（WSL 注册、codex profile 供给、本地文件系统符号链接、任务派发）**均不在本轮改动范围内**（本轮只碰 MCP 配置/审计/预览/hook 与前端）。

### 19.10 一个必须记下的陷阱

**`data /*` 兜不住可配置的 `DataDir`**：`mcp-runtime/` 常态下被 `data/*` 覆盖，但 `DataDir` 可被配到任意位置，所以「有个通配规则就够了」是错觉——凡是**路径可配置**的运行时产物，都应在 `.gitignore` 里**按目录名再显式排除一次**。

### 19.11 结论

至此，分期清单（P0/P1/P2）与正文表内要求**都已落实**。若还要继续推进，方向只剩 §13 列出的**未决问题**（需要产品决策或上游变更，而非「写代码补进度」）。

---

## 20. 全量代码复查（2026-09-10，第三轮）

§19 之后对**所有已实施/已修改代码**做了一轮逐文件复查（后端 MCP 全部新增文件 + 改动点 + 前端 + 跨端 runner）。结论：**3 处已修复，5 处待决策/低危已记录**。

### 20.1 已修复

#### (1) `app.go` 未通过 gofmt

新增 `mcpLastInject` / `stateEventSubs` 等字段打断了 `Server` 结构体与 `AgentRunRequest` 字面量的字段对齐。`gofmt -l` 能报出来，属机械问题。

**修复**：`gofmt -w`；`go build ./...` + `go vet ./internal/app/` 通过。
（附带发现 `preferences.go` 也有同类违例，但它在 **HEAD 中已提交**、非本轮改动，未动。）

#### (2) `truncateApprovalValue` 的字节/rune 口径不一致（`mcp_audit.go`）

原实现：

```go
if len(text) <= limit { return text }                              // 按字节判定
return truncateAuditText(text, limit) + "（已截断，原始 N 字符）"   // 内部按 rune 截断
```

`truncateAuditText` 是**按 rune** 截断的（注释明说「避免把多字节字符切成半个」）。于是中文内容会落进「字节超限、rune 未超限」的区间：**实测没有截断，却仍被追加「（已截断）」**；`command` 键的实际体积上限也被放大到 rune 上限（最坏约 3×）。

**修复**：判定与说明统一按 rune。原测试只用 ASCII（`len==rune`）覆盖不到，故补两个 CJK 回归用例（`...CJKValueNotFalselyAnnotated` / `...CJKValueTruncatedByRune`）。

#### (3) **Codex 密钥环境变量名不含 server 作用域**（`mcp_config.go`，本轮最严重）

`codexHeaderArgs` 用 **header 名**派生变量名：

```go
name := mcpSecretEnvName(mcpSecretRefPrefix + sanitizeMCPRunKey(key))  // key = "Authorization"
// → "MCP_SEC_Authorization"，与 server 无关
```

`buildCodexInjection` 会**按变量名去重**（`seenEnv`）。因此两个 Codex MCP server 只要配了同名 header（`Authorization` 最常见，`X-Api-Key` 等同理）：

- 两个 server 的 `bearer_token_env_var` 都指向 `MCP_SEC_Authorization`；
- 后一个 server 的密钥被去重**丢弃**；
- 它实际拿到的是**前一个 server 的令牌** —— 凭据串到另一个 server 的端点上。

已用测试复现（两个 server 均得 `MCP_SEC_Authorization`）。**修复**：变量名并入 server 名（`prefix` 恒为 `mcp_servers.<name>`，取其 name），即 `MCP_SEC_<server>_<header>`；补回归测试 `TestCodexHeaderSecretEnvNamesAreServerScoped`。

> 对照：Claude 路径用 `mcpSecretEnvName(sec_<uuid>)`、Codex OAuth 路径用 `mcpSecretEnvName(sec_<serverID>)`，**都是唯一的**，不受影响。同类的还有 Codex stdio 的 `env_vars`（直接用用户填的键名）——两个 server 声明同一个键名时同样只有一个值生效；但该处键名必须与 server 期望的变量名一致，**无法重命名**，属设计约束而非缺陷。

### 20.2 待决策 / 低危（未改）

#### (4) SSH + Codex 的 MCP **完全未注入**，但状态显示「已注入」

`app.go` 的 `case conversation.AgentID == "codex" && isSSH` 会路由到 `sshRunner`，`sshRunner.Run` → `runCodex`（`ssh_runner.go:1235`）。而 **`sshRunner` 全文没有出现 `CodexMCPArgs`**（`grep` 仅命中 `codex_runner.go` 与 `wsl_agent_run.go`）。与此同时 `prepareMCPInjection` 对 `codex + remote` 仍会：

- 调 `buildCodexInjection` 生成 `-c` 参数（并**把密钥解密成明文**放进 `Env`）；
- 把 server 列表记为成功注入。

结果：**配置静默失效 + 注入状态误导 + 一次无用的密钥解密**。根因是 §7.4 的「Codex-SSH 密钥通道」一直未定夺。

**建议**：二选一——① 在 `prepareMCPInjection` 里对 `codex + remote` 显式跳过并写 `Note`（与现有降级风格一致，成本最低）；② 补齐远端临时 env 文件通道（`0600` + `trap rm` + `set -a; . file`）后真正注入。

#### (5) 白名单里的 `Bash(<pattern>)` 永不生效

§8.4 要求 hook 命中「`mcp__<server>__*` 或 `Bash(<pattern>)`」时直接放行。但 `waitForApproval` 的判定是 `mcpAutoApproveAllows(patterns, input.ToolName)`，内部 `path.Match(pattern, toolName)`；而 Bash 的 `tool_name` 恒为字面量 `"Bash"`，`Bash(npm run test)` **永远匹配不上**。校验（`mcpAutoApprovePattern`）允许、UI 可填、存量也存了，但静默无效。

失败方向是「仍走人工审批」，**不构成安全漏洞**，属功能与文档不一致。

**建议**：对 `Bash(...)` 模式改为匹配 `tool_input.command` 的前缀（与 CLI allow 语义对齐），或在白名单里只接受 `Bash`/`mcp__…` 并在 UI 注明不支持子命令模式。

#### (6) `mcpInjection.RemotePath` 是死字段

只在 `mcp_config.go:209` 赋值，全仓无读取——SSH 的真实落盘路径由 `buildRemoteMCPSetup` 用 `sanitizeMCPRunKey(mcpKey)` **重算一遍**。两处口径重复，将来易漂移。建议删除该字段（或让它成为唯一来源）。

#### (7) Codex 注入状态把「被选中的全部 server」记为已注入

`prepareMCPInjection` 的 codex 分支写的是 `Servers: describeMCPStatusServers(servers)`，未剔除 `codexServerArgs` 返回 `ok=false`（命令/URL 解析为空、密钥解密失败）的条目；Claude 分支写的是**实际注入**的 `injected`。建议对齐，避免「最近一次注入」快照虚高。

#### (8) `truncateProbeText` 用字节切片

```go
if len(text) > 400 { return text[:400] + "…" }
```

`text[:400]` 是字节切分，可能切断多字节字符（同文件的 `truncateAuditText` 已按 rune 处理）。影响面小：仅用于 flag 的 `Detail` 与探针错误文本，非法 UTF-8 会被 `json.Marshal` 替换为 U+FFFD，表现为显示乱码。低危。

### 20.3 复查通过（无问题）的部分

| 区域 | 核对点 |
| --- | --- |
| OAuth 回调 | `state` 单次有效（取出即删）；结果页对 `message` 做 `htmlEscape`（`error_description` 来自 query，已转义）；完成后以短 TTL 回插供前端轮询 |
| `storeMCPOAuthToken` | 先读旧行再开事务（规避单连接下 `rows` 未关导致的死锁）；新 `sec_` 引用落库 → commit → 再回收旧引用；`refresh_token` 缺省时沿用旧值 |
| `state_events.go`（另一条工作线） | 锁序一致（`s.mu` → `stateEventSubMu`），无反向获取；慢客户端满队列断连有 `closeOnce` 保护；writer goroutine 由 `<-writerDone` 收敛，无泄漏 |
| 并发审批 | 移除「同 run 只能 1 个待审批（409）」是 §15.2 的**明确决策**，改为多个并存、前端按卡片各自裁决 —— 非缺陷 |
| WSL 环境传递 | `MCP_` 前缀进 `wslForwardEnvKeys`（覆盖 Claude 路径）；Codex 路径另经 `envNames(request.MCPEnv)` 显式转发。两条路径都通 |
| 跨端 runner | `windowsAgentRunner.Run/StartSession` 直接返回「跨端尚未就绪」错误，本就不执行 → 无需注入，非缺口 |
| 前端 | `timeline.ts` 改按 `tool_use_id` 锚定（更精确，且保留旧事件的命令文本回退）；`mcpToolMeta` / `mcpSchemaText` / `mcpPreviewRows` 无空值崩溃与 XSS；`tsc -b --force` 通过 |

### 20.4 验证

- `gofmt -l`：本轮所有改动文件已干净；`go build ./...`、`go vet ./internal/app/` 通过。
- 相关定向测试（MCP / Codex / TOML / Approval / Truncate / Preview / VersionAtLeast / Secret）全绿，含本轮新增的 3 个回归用例。
- 仍只有 2 个**既有环境性失败**（隔离 `CODEX_HOME` 缺内置 `.system` skill、Windows 非特权符号链接），与本轮无关，**勿误判为回归**。前端 `tsc` 通过。

---

## 21. 第四轮复查与修正（2026-09-10）

§20 之后又做了一轮复查，重点放在：① §20.2 挂账的 5 项；② 之前只粗读过的 `codex_toml.go` / `mcp_bindings.go` / `mcp_project_settings.go` / `mcp_import.go` / `mcp_preview.go`。结论：**新发现 1 处真实缺陷（已修），§20.2 的 5 项全部处置完毕**。

### 21.1 新发现并修复：env / header 值里的 `${PROJECT_DIR}` 未被解析

**证据（三处口径互相矛盾）**：

| 来源 | 说法 |
| --- | --- |
| 文档 §5.3 | `"ROOT": "${PROJECT_DIR}/docs"` → 「按目标环境解析路径」 |
| 前端表单（`McpManagerPage.tsx`） | 「环境变量（每行 KEY=VALUE，支持 `${PROJECT_DIR}`）」 |
| 预览接口（`mcp_preview.go:101-106`） | **确实解析**：`Env`/`Headers` 的每个值都过 `resolve` |
| `resolveMCPValues` 注释 | 「其余值解析 `${PROJECT_DIR}` 占位符」 |
| **`resolveMCPValues` 实现** | **非密钥分支 `out[key] = value`，原样赋值，不解析** |

**为什么是真 bug 而不是「交给 CLI 展开」**：`PROJECT_DIR` 是 Milevia 专属占位符，Milevia **从不把它写进 CLI 进程环境**（全仓 grep 无 `PROJECT_DIR=` 赋值）。CLI 按 `--mcp-config` 展开 `${VAR}` 时找不到该变量，按文档 §136 的行为会**原样保留 `${PROJECT_DIR}` 文本**（并在 `claude mcp list` 报缺失警告）。于是：

- 用户在预览里看到 `ROOT=D:\proj/docs`，
- 真实注入却是 `ROOT=${PROJECT_DIR}/docs` → env 值坏掉。

**修复**：`resolveMCPValues` 增加 `target`/`projectPath` 参数，非密钥值统一过 `resolveMCPPlaceholders`；Codex 两条路径（`codexEnvVarNames`、`codexHeaderArgs`）同样补上（Codex 的 `env_vars` 只转发变量名，真值由 Milevia 注入 CLI 进程环境，CLI 侧同样无法展开）。密钥（`sec_`）分支不动——它是解密出的明文，不含占位符。

`projectPath == ""`（未选项目）时仍**原样保留**，与 §19.6 修过的 bug 保持一致（不静默变空串）。

### 21.2 §20.2 五项挂账的处置

| # | §20.2 结论 | 本轮处置 |
| --- | --- | --- |
| 4 | SSH+Codex 的 MCP 完全未注入，状态却显示「已注入」 | **已修**：远端 + Codex 直接显式跳过，记录 `Note`「远端（SSH）Codex 暂不支持 MCP 注入」，状态不再虚报。见 §21.3 |
| 5 | `Bash(<pattern>)` 白名单永不生效 | **已修**：hook 判定改为「MCP 按工具名、Bash 按命令」。见 §21.4 |
| 6 | `mcpInjection.RemotePath` 死字段 | **已删**。远端路径由 `buildRemoteMCPSetup` 依同一 `runKey` 生成，两处口径本就靠 `sanitizeMCPRunKey` 对齐；留一个只赋值不读取的字段只会带来漂移风险 |
| 7 | Codex 注入状态虚高 | **已修**：`buildCodexInjection` 改为返回**实际注入成功**的 server 摘要（只含 `codexServerArgs` 判定 ok 的），`prepareMCPInjection` 用它记录状态；顺带在「全部解析失败」时给出说明 |
| 8 | `truncateProbeText` 按字节切分 | **已修**：改按 rune 截断（同文件 `truncateAuditText` 早已如此），避免切断多字节字符 |

### 21.3 SSH+Codex：为什么选「显式跳过」而不是「补齐远端 env 通道」

选**显式跳过**，理由是它严格优于现状、且不预设 §7.4 那个尚未定夺的密钥通道方案：

- 现状是「照常构建 `-c` 参数 → 解密密钥到内存 → 记为已注入」，但 `sshRunner.runCodex` 全程不读 `CodexArgs`，所以**实际什么都没发生**。三宗罪：静默失效、状态误导、无谓解密。
- 显式跳过把这三样一次性消除，且**不改变任何实际注入行为**（本来就注不进去），因此没有功能回归。
- 将来若补上远端 env 通道，删掉这个提前返回即可；代码里留了注释指向 §7.4。

### 21.4 `Bash(<pattern>)` 的匹配语义

文档 §8.4 要求 hook 内判定命中 `Bash(<pattern>)`。原实现只把 `tool_name` 拿去 `path.Match`，而 Bash 的 `tool_name` 恒为 `"Bash"`（命令在 `tool_input.command` 里，是另一个字段），因此 `Bash(...)` 形态**永远不可能命中**。

**新语义（只收紧、不放宽）**：

| 模式 | 命中条件 |
| --- | --- |
| `Bash` | 任意 Bash 调用 |
| `Bash(<前缀>*)` | 命令以 `<前缀>` 开头 |
| `Bash(<前缀>:*)` | 同上（兼容 Claude 的 `:` 习惯写法，冒号只作分隔） |
| `Bash(<命令>)` | 命令与 `<命令>` **完全相同** |
| `mcp__<server>__<tool\|*>` | 工具名 glob 匹配（原有行为不变） |

取不到 `command`（结构不符 / 缺字段）时**不命中**，回落人工审批。匹配失败的方向始终是「多弹一次确认」，不会出现「写了具体命令却放行了一批」。

前端表单提示同步补上 Bash 形态。

### 21.5 验证

- `go build ./...`、`go vet ./internal/app/` 通过；本轮改动文件 `gofmt -l` 干净。
- 新增 4 个回归用例：`TestPrepareMCPInjectionForCodexResolvesProjectDir`、`TestCodexInjectionStatusReflectsActuallyInjectedServers`、`TestPrepareMCPInjectionSkipsRemoteCodex`、`TestTruncateProbeTextIsRuneSafe`；并扩充 `TestMCPToolMatchesGlob`（Bash 形态 8 个断言）与 `TestResolveMCPValuesModes`（占位符 2 个断言）。
- 既有定向测试全绿。仍只有 2 个**既有环境性失败**（隔离 `CODEX_HOME` 缺内置 `.system` skill、Windows 非特权符号链接），**勿误判为回归**。

### 21.6 结论

四轮下来，分期清单（P0/P1/P2）、正文表内要求（A–H）、复查发现的缺陷（§20 的 3 项 + §21 的 6 项）都已落实。**代码层面已无可自行推进的项**；剩余方向只有 §13 的**未决问题**（需产品决策或上游变更），其中最实际的是 §7.4 的「Codex-SSH 密钥通道」——本轮按「显式跳过」保守处理，未擅自设计远端密钥通道。

---

## 22. 启动服务与端到端测试（2026-09-10）

把控制服务与前端真实拉起，对着运行中的服务做了一轮端到端验证（不再只是单测）。

### 22.1 启动方式（Windows / Git Bash）

`pnpm dev`（`dev.sh`）在本机**不可用**：脚本硬性要求 `lsof` 或 `fuser`，本机两者皆无。改为手动拉起两个进程：

```bash
# 控制服务（用隔离数据目录，避免污染开发库 data/auto.db）
cd apps/control-server
go build -o <tmp>/milevia-control.exe ./cmd/control-server
AUTO_HTTP_ADDR=127.0.0.1:8080 AUTO_CONTROL_URL=http://127.0.0.1:8080 \
  AUTO_DATA_DIR=<tmp> <tmp>/milevia-control.exe

# 前端
cd apps/web
VITE_CONTROL_URL=http://127.0.0.1:8080 node node_modules/vite/bin/vite.js \
  --host 127.0.0.1 --port 5173 --strictPort
```

两处与沙箱的交互值得记录（详见项目记忆）：vite 重建 `node_modules/.vite/deps` 与 pnpm 自身的 `_tmp_*` 临时目录都会触发 WorkBuddy 的批量删除保护（阈值为 50 项），前者靠先手动清缓存解决，后者靠**绕过 pnpm 直接跑 vite** 解决。

### 22.2 API 端到端（51 项断言，全部通过）

| 分组 | 覆盖 |
| --- | --- |
| 项目 | 创建返回 201；列表包含；清理后为空 |
| MCP server CRUD | 创建 stdio/http（201）；重名被唯一索引拒绝；按 id 取；PATCH 字段生效 |
| **按环境预览** | windows / wsl / remote-linux 三态解析；未选项目时**原样保留**并给 note；未知环境 4xx |
| **自动放行白名单** | 接受 `Bash` / `Bash(<前缀>*)` / `mcp__<server>__*`；拒绝无锚点 `mcp__*`、跨 server 模式、非 Bash/mcp 形态 |
| 项目级绑定 | `overridden` 语义、`enabled=true` 删除覆盖行回到默认；`allowMcpJson` 与 `strictMode` 联动 |
| 其他 | 注入状态空快照、调用审计、预设列表、导入预览（不落库） |

### 22.3 真实子进程验证（关键一条）

§21 修的「env/header 里的 `${PROJECT_DIR}` 不被解析」如果只在预览里验证，等于没验证。为此写了一个最小 MCP stdio server（回显收到的 `env`/`argv`/`cwd`，并实现 `initialize` / `tools/list` 最小握手），再经 `POST /api/mcp/servers/{id}/test` 由服务真实拉起：

```
{ "root": "D:\\tmp\\milevia-e2e\\proj/docs",     ← 占位符已在真实注入值里解析
  "argv": ["--marker", "hello"], "cwd": "D:\\tmp\\milevia-e2e\\proj" }
```

同时证明：探测器完成了一轮真实 MCP 交互（列出工具 `echo_root`）、`cwd` 占位符解析、参数原样透传。

### 22.4 UI 端到端（20 项断言，全部通过，连跑两次一致）

Playwright（headless Chromium，仓库 `apps/web` 已带 `playwright@1.62.1`）：

- 应用外壳、MCP 管理页、设置页、Agent 档案页、导入项目页均正常渲染；
- 打开新建表单 → 填名称/命令/参数/环境变量 → **真点「生成预览」**：
  - 作用域=全局：结果区显示占位符**原样保留** + 「未选择项目…」说明；
  - 作用域=仅指定项目：结果区显示解析后的真实路径（`env:ROOT = D:\…\proj/docs`）；
- 全程 **无未捕获异常、无控制台 error、无失败网络请求**。

### 22.5 其他

- 后端定向测试全绿；前端单测 210/210；仅 2 个**既有环境性失败**（隔离 `CODEX_HOME` 缺内置 `.system` skill、Windows 非特权符号链接），与本轮无关。
- 运行期卫生：`mcp-runtime` 无残留文件（用后即删生效）；服务日志除浏览器关闭导致的 WS 断开噪声与 WSL 注册警告外无异常。
- 测试脚本可重复执行，固定在临时目录：`e2e_mcp.py`（API）、`ui_test.cjs`（UI）、`echo-mcp.js`（回显用 MCP server）、`shots/*.png`（截图）。

---

## 23. 模板缺陷修复与连接体验审计（2026-09-15）

**起因**：用户提出「MCP 的连接与配置过程太麻烦，有没有更好的方式，参考一下 WorkBuddy 的连接器」。
先做了一轮只读审计（前端 `McpManagerPage.tsx` + 后端 9 个 `mcp_*.go` + 本文档），给出问题清单与改造建议；
**本轮只落地其中「已知缺陷」部分**，改造类目全部挂账（§23.6）。

### 23.1 审计结论：短板在「默认值从哪来」和「反馈什么时候到」

| | 现在的 Milevia（配置表模型） | WorkBuddy（连接器安装模型） |
| --- | --- | --- |
| 入口 | 首页按钮 → 一个大表单 | 连接器市场 / 自定义向导 |
| 用户要提供 | 命令、参数、环境变量、传输类型、作用域 × 3 环境 × 2 Agent、白名单 glob | 基础信息 → 认证方式 → MCP Server URL |
| 配置来自 | **用户自己**（模板也只给启动命令） | **连接器包**（`connector-meta.json` + `mcp.json` + `icon.svg` + `skills/`） |
| 认证 | 手填凭据；OAuth 需**先建好 server** | MCP OAuth 2.1：填完 URL 自动建凭据，无需手填 Client ID/Secret |
| 生效 | 保存后**才能测试**；失败要回改 | 点一次「信任」 |
| 工具权限 | 只有「免审批」开关（藏在测试结果里） | 工具级过滤 |

**结论**：**安全治理层是 Milevia 领先**（`--strict-mcp-config` 默认开、凭据 AES-GCM + 占位符不落盘、
hook-allow 审批链路、只读任务不注入 + `mcp__*` deny、投毒/过度授权检测、调用审计 —— WorkBuddy 的公开
文档里没有对应的审计层），**短板全在默认值来源与反馈时机**。因此改造方向是「把能力收进默认值与折叠区」，
**不是砍掉能力**。

### 23.2 已修缺陷

#### (1) GitHub 模板指向已归档的 npm 包（最硬的一条）

`@modelcontextprotocol/server-github` **已于 2025-04 归档弃用**，官方开发迁至
[`github/github-mcp-server`](https://github.com/github/github-mcp-server)。旧包还有一个更实质的问题：
它**硬 pin `@modelcontextprotocol/sdk@1.0.1`**，协议协商封顶在 `2024-11-05`，拿不到结构化工具输出 /
elicitation / 工具注解。用户点「GitHub」模板 → `npx -y @modelcontextprotocol/server-github` → 必然失败。

**修法**：换成官方两种形态，各带自己的依赖与凭据声明：

| 模板 | 形态 | 凭据 | 依赖 |
| --- | --- | --- | --- |
| `github`（GitHub（远程托管）） | http → `https://api.githubcopilot.com/mcp/` | `Authorization: Bearer <PAT>`（请求头） | 无（**环境无关**，故适用全部三种环境） |
| `github-local`（GitHub（本地容器）） | stdio → `docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server` | `GITHUB_PERSONAL_ACCESS_TOKEN`（环境变量） | `docker` |

> `-e GITHUB_PERSONAL_ACCESS_TOKEN` **不带值**：docker 从自身进程环境转发，而该变量正是 Milevia 在
> 「环境变量」里注入的那一个 —— 因此密钥仍然不进 argv，与 §16.2 的占位符方案一致。
>
> http 形态另有一条收益：**可以改用已有的 OAuth 2.1**（§18.1），卡片上的「授权」按钮即可走浏览器登录。

#### (2) 模板不声明依赖与凭据

`mcpPreset` 原只有 `ID/Name/DisplayName/Description/Transport/Command/Args/URL/Environments`。
`startCreate(preset)` 也只拷这几个字段 —— 用户点完 GitHub 模板，仍要自己知道有
`GITHUB_PERSONAL_ACCESS_TOKEN` 这回事。

**修法**：`mcpPreset` 增 `Requires` / `Credentials` / `DocsURL` / `Note`：

```go
type mcpPresetRequirement struct { Command, Label, Hint string }
type mcpPresetCredential struct { Key, Target, Label, Description, DocsURL string } // Target: env | header
```

前端配套（四条，都为了「把还差什么提到点击之前」）：

- 模板区由一排按钮改为**卡片**，带徽标：`需 Node.js` / `需 Docker` / `填 GitHub Personal Access Token`。
- 表单内新增「**模板要求**」区块：依赖（含 hint）+ 凭据（键名 / 落点 / 说明 / 「去申请」按钮）。
- 凭据提示**以 `#` 注释行预填进环境变量 / 请求头文本框**：

  ```text
  # GITHUB_PERSONAL_ACCESS_TOKEN=<GitHub Personal Access Token>      ← env
  # Authorization=Bearer <GitHub Personal Access Token>              ← header
  ```

  `parseKeyValueLines` 本就跳过 `#` 开头的行，所以既让用户看到该填哪个 key、填成什么形态，
  又不会因为留空而写进一条空值；http 形态自带 `Bearer ` 前缀，用户不用去查格式。
  **值位置只允许出现「待替换的占位符」，行尾不许追加说明文字** —— 这条是被测试逼出来的：
  初版写成 `# KEY=<必填>  ← 说明`，用户照提示取消注释后解析出
  `KEY=xx  ← 说明`，token 末尾多一段可见尾巴，**表现为 401 而不是「没填」**，排查成本极高。
  说明统一放在表单的「模板要求」区块里（那里同时给出申请链接）。
  **两处规则必须同步，已写成回归测试**。
- 解析、模板要求、运行时检查的命令选取抽到 `apps/web/src/features/mcp/mcp-model.ts`（纯模块）。
  原因见 §23.6 末条：这几条规则**取决于优先级 / 顺序**，靠扫源码验不出来。
- `transportLabel()` 把 `stdio/http/sse` 渲染成「本地进程 / 远程 HTTP / 远程 SSE」。

#### (3) 内置了一条只用于演示的模板

`{ID: "http-example", URL: "https://example.com/mcp"}` —— 它会以「远程 MCP（示例）」出现在正式模板列表里，
用户点进去必然连接失败。**已删除**（`example.com` 占位地址与「示例」字样都写进了回归测试）。

#### (4) 运行时依赖只在「连接测试」里才暴露，而测试在保存之后

见 §23.3。

#### (5) 过期文案 + 顺带修掉的一个真实缺口

- Agent 选项仍写着 `Codex（P1 起支持）`，而 P1 的 Codex `-c` 注入 2026-09-10 就完成了（§17.4）。
- **顺带发现并修掉**：`testMCPServer` 的 `ConnID` 取的是前端传入的 `connectionId`，但测试对话框
  （`runTest`）**从不传这个字段**，于是「目标环境 = SSH 远端」**永远**以
  「远端测试需要指定 SSH 连接」失败 —— 一个从未成功过的分支。新增
  `mcpConnectionIDFromRunner(runnerID, explicit)`：显式传入优先，否则从项目的 `runnerID` 反推
  （SSH runner 的 id 恒为 `ssh-<connectionID>`，见 `ssh_connection.go`）。
  **非 `ssh-` 前缀一律返回空** —— 项目的 runner 是本机 / WSL 时，把 runnerID 当连接 id 传下去只会得到
  「该 SSH 连接当前未建立」这种指向错误原因的提示。

### 23.3 新增 `POST /api/mcp/runtime-check`

**要解决的问题**：`/api/mcp/servers/{id}/test` 依赖 **已落库的 serverID**（内部 `fetchStoredMCPServer` 查库），
而 `/api/mcp/preview` 只解析占位符、不真的连。于是用户只能走
**「填完一屏 → 保存 → 测 试 → 发现 npx 不存在 → 回改 → 再保存」**，至少三个来回。

**实现**：新接口**接收表单 / 模板给出的命令名**，在目标环境实测其是否存在，不落库、不启动 server。

| 目标环境 | 做法 | 说明 |
| --- | --- | --- |
| windows | `exec.LookPath` | 按 `PATHEXT` 展开，能找到 `npx.cmd` |
| wsl | `runner.wslBridgeProbe(ctx, script)` | 复用既有通道，自动带 `wslPathPrefix`（`$HOME/.npm-global/bin` 等），与解析 claude/codex 二进制同一口径 |
| remote-linux | `client.execCommand(ctx, script)` | 连接 id 经 `mcpConnectionIDFromRunner` 反推 |

两个关键取舍：

- **脚本一律写成 `command -v X || echo <marker>`**，让整体退出码恒为 0。否则「命令确实没装」会与
  「通道本身坏了」（WSL 不可用、SSH 未连接）一样表现为非零退出，前端就无法区分该提示
  **「去装一下」** 还是 **「先把连接建起来」**。响应因此分成两层：`error`（通道级，items 里的「未找到」
  不成立）与 `items[].found`。
- **命令名进 shell 前必须白名单化**：`^[A-Za-z0-9][A-Za-z0-9._+-]*$`。非法项不拼进 shell，
  而是作为一条带 `error` 的独立结论回传（模板目录里的正常取值 `npx/uvx/docker/node/python3` 都在范围内）。

前端放在「按环境预览」区（与占位符解析同处：那条配置在这个环境里长什么样、跑不跑得起来），
命令来自模板的 `requires`，模板不含依赖时回落当前表单的启动命令（仅 stdio）。

### 23.4 验证

**测试**

- 新增 `mcp_presets_test.go`（9 例）：模板目录形状（id/name 唯一、name 合法、字段与传输类型自洽、
  依赖声明可被运行时检查支持、**凭据落点合法且真的被启动参数引用**）、不得指向已归档包、
  不得出现占位地址与「示例」字样、GitHub 两形态的固定结论、stdio 模板必须声明其启动命令的运行时；
  运行时检查的脚本形态 / 输出解析 / 命令名白名单 / 连接 id 反推。
- 新增 `apps/web/src/features/mcp/mcp-model.test.ts`（7 例）：解析器与注释规则的成对性
  （含「提示永远不会变成值」与「取消注释后值里无残留说明」两条往返断言）、凭据按落点分流、
  **运行时检查命令的优先级**、传输类型文案。
- 新增 `apps/web/src/mcp-manager.test.mjs`（7 例）：页面接线（模板元信息的写入与清理、
  预填与解析共用同一份实现、运行时检查接口与渲染分支）、模板卡片与「模板要求」区块、
  过期文案、类型声明、新增样式的类名前缀。

**结果**

- `go build ./...`、`go vet ./internal/app/` 通过；本轮改动的三个 Go 文件 `gofmt -l` 干净
  （`gofmt -l` 仍会列出 7 个**非本轮**文件，与 §20.1 记录的一致，未动）。
- 定向 Go 测试 **25 例全绿**（含本轮新增 9 例）。
- 前端 `tsc -b && vite build` 通过；`pnpm test` **352/352**（本轮新增 14 例）。

**变异检验**（`.tmp/mcp-mutation.py`，**13 处，13 挡住、0 漏网**，每条都在 `finally` 里逐字节还原并 sha1 自证）：

| # | 变异 | 被谁挡住 |
| --- | --- | --- |
| G1 | github 模板地址改回 `example.com` | `TestMCPGitHubPresetsPointAtOfficialServer` + `...NoPlaceholderEndpoints` |
| G2 | 运行时探测脚本去掉 `\|\| echo <marker>` 兜底 | `TestMCPRuntimeProbeScriptAlwaysSucceeds` |
| G3 | 连接 id 反推不再校验 `ssh-` 前缀 | `TestMCPConnectionIDFromRunner`（本机 runner 被当成连接） |
| G4 | `github-local` 凭据落点从 env 改成 header | `TestMCPPresetCatalogIsWellFormed`（stdio 不该有请求头凭据） |
| G5 | 清空 playwright 模板的依赖声明 | `TestMCPStdioPresetsDeclareRuntimeRequires` |
| M1 | 解析器不再跳过注释行 | mcp-model：提示行变成了值 |
| M2 | 凭据提示不再写成注释 | mcp-model：解析器读到了提示行 |
| M4 | 凭据提示不再按落点分流 | mcp-model：env / header 混在一起 |
| M5 | 凭据提示在**值位置**追加说明文字 | mcp-model：取消注释后值里带残留说明 |
| M3 | **运行时检查的命令优先级写反** | mcp-model：`["docker"]` 变成了 `["node"]` |
| P1 | Codex 文案退回过期版本 | mcp-manager：`P1 起支持` |
| P2 | 编辑既有 server 时不清模板要求 | mcp-manager：`startEdit` 切片里没有 `setPresetMeta(null)` |
| P3 | 运行时检查的命令选取绕过模型层 | mcp-manager：`runtimeCommands` 不再走 `runtimeCommandsFor` |

> **M3 是这轮唯一的真漏网**（首版），修法见 §23.6 末段。M4 首轮报「跳空」是因为它的锚点写在
> `presetGuidanceLines` 被重写之前 —— 脚本的「锚点命中次数 ≠ 1 就报跳空」这条自检把它暴露了出来，
> 修正锚点后正常挡住。**这也是「变异脚本的锚点必须随代码演进同步」的实证**。

**未验证的部分（如实声明）**

- **全量后端测试未执行**：本机 sandbox 对 `wsl.exe` 是**直接中止整条命令**（不是早期记录的
  「打印提示但退出码仍为 0」），凡构造 `Server` 的用例（`newTestServer` → runner 注册 → WSL 补注册探测）
  都跑不了。纯函数用例已逐一点名跑过，全绿；要跑全量需在沙箱外执行。
- **`runtime-check` 的三条分支未做端到端实跑**（Windows 分支在本机可跑，WSL / SSH 需对应环境）。
  已覆盖的部分是脚本生成、输出解析与命令名白名单的单测。

### 23.5 挂账：需要产品决策的改造（**不是缺陷**）

按杠杆排序，供后续排期：

| # | 项 | 要点 |
| --- | --- | --- |
| 1 | **粘贴 JSON 快路径** | 现实中拿到 MCP 配置的路径 90% 是从 README / 网页复制一段 `mcpServers`。现在**只能扫本地文件**（`mcp_import.go`），没有「粘贴文本」入口。解析层（`collectImportCandidates` / `looksLikeSecretKey` / `sanitizeImportedServerName`）全部现成，只差一个「从文本而不是从文件」的入口 |
| 2 | **草稿态试连** | 需要一个接受**表单字段**的测试接口（复用 `probeMCPServer`），才能把测试并入向导最后一步 |
| 3 | **三步向导 + 高级折叠** | 作用域 / 环境 / Agent / 白名单 / 超时全部折叠并给默认值（全局 + 全环境 + 需审批） |
| 4 | **OAuth 提到「建 server」之前** | 后端 `discoverMCPOAuthEndpoints` / `registerMCPOAuthClient`（动态注册）已具备，只是编排顺序倒置 |
| 5 | **工具策略与连接解耦 + 逐工具禁用** | 现在工具级免审批只能在「测试连接」的结果里勾，且只有 enabled / autoApprove，**没有 disable**（WorkBuddy / Codex 有工具级过滤） |
| 6 | **首屏降噪** | 0 个 server 时先问「怎么连」，项目视图 / `.mcp.json` 放行 / strict 说明折叠或后置 |

### 23.6 一条必须记下的教训

**模板的价值不在「省几次敲键盘」，而在「把用户本来要去别处查的信息带过来」。**
一条只写了启动命令的模板，用户仍然要自己知道「要装 Node 还是 Docker」「要填哪个 key、申请地址在哪」，
于是必然走成「保存 → 测试 → 失败 → 回改」——**模板省下的输入量，被返工次数吐了回去**。
配套的两条硬规则：

- 凡是「接入外部能力」的模板，必须同时声明 **运行时依赖**（且该依赖能被真实检查）与
  **所需凭据**（键名 + 落点 + 申请链接）。
- 凡是「配置正确性只能靠运行才知道」的东西，**检查口必须能在保存前调用**；依赖已落库 id 的接口
  做不到这一点（`/servers/{id}/test` 就是反例）。
- 凭据提示**可以教格式，但不能教在值的位置上**：行尾说明会被并进值里，故障表现为 401 而非「没填」。

**另有一条测试方法论上的教训（本轮被变异检验逼出来的）**：最初的回归测试里，
「模板声明的依赖优先于表单命令」这条**优先级**是用正则扫页面源码来验的，于是把优先级写反
（`fromPreset` 过滤成空数组）**照样绿** —— 文本里 `presetMeta?.requires` 与 `return fromPreset`
都还在。修法不是把正则写得更长，而是**把这段逻辑抽成纯函数**（`features/mcp/mcp-model.ts`）直接调用断言。
判据：**「结果取决于几行代码的顺序 / 优先级」的逻辑，一律抽出来做行为断言；扫源码只用来守
「接线是否还在」（谁 import 谁、调哪个接口、文案是什么）**。

---

## 24. 「一键连接」：让不懂 MCP 的用户也能连上（2026-09-15）

**起因**：§23 交付后用户把目标说明确了 —— **「MCP 连接的规则要简化到用户不懂也能用」**。
§23.5 挂账的 6 项里，「粘贴 JSON」「三步向导」「OAuth 提前」「首屏降噪」其实都是这一件事的侧面，
所以本轮不再逐项做，而是按目标整体重构主路径。

### 24.1 口径与可验收判据

**口径**：用户不需要懂 MCP 协议细节（stdio/http、npx、env 变量名、`${PROJECT_DIR}`、
作用域 × 环境 × Agent、白名单 glob），**但可以有 GitHub Token 这类凭据**。
协议细节是**实现细节**，该由目录条目和自动检测承担，不该由用户回答。

**可验收判据**（写下来是为了能被检查，而不是"感觉简单了"）：

1. **默认路径上用户输入 ≤ 1 项**：一个 Token，或一次浏览器授权点击；
2. **界面上不出现 MCP 概念词**（向导里连"目标环境"都不许出现 —— 已写成断言）；
3. **连不上时给的是「下一步动作」**（"这台电脑还缺 Node.js"），不是错误码。

### 24.2 三层收口：概念去哪了

| MCP 概念 | 以前 | 现在 |
| --- | --- | --- |
| 传输类型 stdio/http/sse | 表单必选 | 由目录条目决定，界面不出现 |
| 启动命令 / 参数 / 环境变量 | 表单必填 | 只在「高级设置 → 手动配置」 |
| `${PROJECT_DIR}` 等占位符 | 用户要自己写 | 条目自带 |
| 作用域 / 3 环境 / 2 Agent | 三组复选框 | 默认全局 + 全部；**能不能跑交给自动检测** |
| 自动放行 glob | 手写 `mcp__github__*` | 向导里一个勾选框：「以后不用再问」 |
| strict / `.mcp.json` 放行 / 注入快照 / 审计 | 主管理页内联 | 「高级设置」折叠区（默认收起） |

主界面因此变成三段：**已连接**（服务卡片）→ **可以连接的服务**（目录）→ **高级设置**（折叠）。
工具栏只剩一个「高级设置」开关 —— 断言里明确禁止「手动配置 / 导入 / 审计」再出现在工具栏上。

### 24.3 目录：筛选标准比字段更要紧

`mcpPreset` 增 `Summary`（给用户看的一句话）、`Category`、`Icon`、`OAuth`；
`mcpPresetCredential` 增 `ValuePrefix`（见 §24.5）。**但真正要紧的是筛选标准**：

> 进目录的服务必须满足 ①用户认得这个服务名 ②只需要「点一次浏览器授权」或「填一个凭据」就能用。

这条被写成测试 `TestMCPCommonPresetsAreOneClick`：`常用服务` 分组的条目**必须声明 OAuth、
必须没有本地依赖、必须是远程形态、必须给官方文档**；另加「条目数不得少于 5」。

目录从原来的 6 条（`filesystem / fetch / github / playwright / context7 / memory` —— 全是开发者视角）
换成 13 条，分两组：

**常用服务（远程托管 + OAuth，环境无关，点一次授权即可）**

| 服务 | 端点 | 认证 |
| --- | --- | --- |
| Notion | `https://mcp.notion.com/mcp` | OAuth |
| Linear | `https://mcp.linear.app/mcp` | OAuth |
| Sentry | `https://mcp.sentry.dev/mcp` | OAuth |
| Slack | `https://mcp.slack.com/mcp` | OAuth |
| Jira / Confluence | `https://mcp.atlassian.com/v1/sse` | OAuth |
| GitHub | `https://api.githubcopilot.com/mcp/` | OAuth 或 PAT |
| Stripe | `https://mcp.stripe.com` | OAuth 或 Restricted Key |

URL 全部取自各服务官方文档（不编造端点）；每条都附官方文档地址，用户能自己核对。

**本机运行（需要在目标环境装一个运行时）**：项目文件 `filesystem`(npx)、浏览器 `playwright`(npx)、
网页抓取 `fetch`(uvx)、长期记忆 `memory`(npx)、库文档 `context7`(npx)、
GitHub 本地容器 `github-local`(docker)。这一组仍进目录（用户可能就是要它），
但卡片上直接写「先装 Node.js / Docker」，不占一键路径的位置。

### 24.4 新增接口 `POST /api/mcp/test-draft`（草稿态试连）

**为什么必须新开一个**：`/api/mcp/servers/{id}/test` 依赖**已落库的 serverID**，于是一个
「先试试能不能连」的朴素需求必然变成「先保存 → 再测 → 失败回改」——**配错了还要先污染一次配置库**。

`test-draft` 直接接收表单字段与明文凭据（与创建接口同一信任模型：明文只出现在本地回环请求
与本次进程内存里，不落库、不回显），复用同一条探针通道 `probeMCPServer`。

顺带把两个测试入口共用的响应填充抽成 `fillMCPTestResponse`，并补上 `Tools` 的 nil 兜底
（原来只兜了 Resources / Prompts；`tools` 编成 `null` 时前端按 `T[]` 读会整块渲染失败）。

**同时修掉一个连带缺陷**：`buildRemoteStdioProbeCommand` 无条件拼 `cd <projectPath> && `，
未指定工作目录时变成 `cd ''` → 整个远端探测脚本以「没有那个文件或目录」失败，而错误指向 cwd。
草稿态试连常常没有项目，很容易踩到，故改为无工作目录时不拼 `cd`。

### 24.5 向导：两屏（加上目录共三屏）与三条路径

**屏 1 = 服务目录本身**（点卡片即进入），不套第二层模态。
**屏 2 = 只问缺的那一项**；**屏 3 = 自动检查 → 完成**。

| 路径 | 条件 | 流程 | 验证方式 |
| --- | --- | --- | --- |
| 纯授权 | 有 OAuth 且无凭据声明 | 屏 2 点「用浏览器登录 {服务名}」→ **先落库** → OAuth → 轮询 | **已落库**的 `/test` |
| 填凭据 | 有凭据声明 | 屏 2 填密钥 → 屏 3 | `/test-draft` |
| 无需准备 | 无凭据、无授权 | 直接进屏 3 | `/test-draft` |

三个关键取舍：

- **OAuth 必须先落库**（回调要按 serverID 存令牌），所以只有这一条路径会先创建 server。
  代价是「取消」不能假装什么都没发生 —— `closeWizard` 会提示「已保存，可在列表里完成授权或删除」。
- **OAuth 路径的验证必须走已落库的接口**：草稿态试连拿不到服务端保存的令牌。
  断言里明确禁止它出现 `test-draft`。
- **屏 3 先查依赖、再试连**，且**缺依赖时提前返回** —— 否则「要去装东西」和「凭据不对」
  会混成同一条错误（顺序也被断言锁住）。

**ValuePrefix：别让用户记格式。** http 形态的凭据落在 `Authorization` 头，值必须是
`Bearer <token>`。让用户在向导里填完整格式，等于要求他知道 Bearer 是什么；
所以条目声明 `ValuePrefix: "Bearer "`，用户只粘 token 本身，前缀由 `buildDraftServerPayload` 补。
漏了它的表现是 401 而不是「没填」，因此写成两条测试：Go 侧不变式
（`Authorization` 凭据必须有 `ValuePrefix`；env 落点不许有）+ 前端行为断言。

### 24.6 「以后不用再问」＝ 一次信任

默认「每次确认」是安全默认，但连上后每次调用都弹审批，不懂的用户会直接判定「坏了」——
门槛只是从"配置复杂"挪到了"审批烦"。所以屏 3 有一个勾选框：

> ☑ 以后调用 {服务名} 的能力不用再问我

勾上即写入 `autoApproveTools: ["mcp__<name>__*"]`（与工具级白名单共用同一套语法），
并注明"随时可以在「管理」里改回去"。**这条不做，前面两屏再简单也白搭。**

### 24.7 前端纯逻辑层扩大（`features/mcp/mcp-model.ts`）

「用户要不要动手、动哪一步」全部收进纯函数，页面只负责渲染：

| 函数 | 回答的问题 |
| --- | --- |
| `connectPlanFor` | 能不能授权 / 要不要填密钥 / 要不要先装东西 |
| `presetBadges` | 卡片上那行「要准备什么」（只用人话，断言里禁止出现协议名词） |
| `cardActionLabel` | 按钮文案（纯授权服务直说「用浏览器登录」） |
| `wizardStartsAt` | 向导从哪一屏开始（什么都不需要的条目别让用户白点一次「下一步」） |
| `credentialsSatisfied` | 凭据屏能不能往下走（能授权 ⇒ 一个字不填也放行） |
| `groupPresetsByCategory` | 按服务端顺序分组，不本地重排 |
| `buildDraftServerPayload` | 创建体：默认值全给上 + 补 `ValuePrefix` + 信任开关 |

### 24.8 验证

- Go 新增/扩充：目录不变式（目录字段齐全、Description 与 Summary 一致、分组已知、
  `Authorization` 必须有 `ValuePrefix`、env 落点不许有）、`TestMCPCommonPresetsAreOneClick`、
  `TestMCPLocalPresetsAreHonestAboutDependencies`、`TestResolveMCPDraftValues`、
  `TestBuildRemoteStdioProbeCommandSkipsEmptyCwd`。**定向 Go 测试 30 例全绿**；本轮改动的
  三个 Go 文件 `gofmt -l` 干净，`go vet` 通过。
- 前端新增 12 例行为断言（`mcp-model.test.ts`）+ 6 例接线断言（`mcp-manager.test.mjs`）；
  `tsc -b && vite build` 通过，`pnpm test` **370/370**。
- **变异检验**（`.tmp/mcp-mutation.py`，**39 处，39 挡住、0 漏网**，逐条独立进程执行、
  `finally` 里逐字节还原并 sha1 自证）：

| 组 | 处数 | 覆盖 |
| --- | --- | --- |
| G1–G11 | 11 | 后端目录（地址、依赖声明、凭据落点、`ValuePrefix`、分组筛选标准、面向用户的一句话）、运行时探测脚本兜底、连接 id 反推、空工作目录不拼 `cd`、草稿值占位符解析 |
| M1–M15 | 15 | 前端纯逻辑：注释行规则、凭据提示的前缀与落点、运行时检查命令优先级、徽标人话、向导起点、凭据屏放行、创建体默认值与凭据前缀、分组不重排、试连与创建共用同一份凭据整理 |
| P1–P13 | 13 | 页面接线：高级区默认收起且工具栏不再放回入口、向导三屏与不出现协议名词、先查依赖再试连、缺依赖提前返回、OAuth 先落库并用已落库接口验证、取消时如实告知、已连接卡片不暴露传输类型、目录卡片展示「要准备什么」、图标自带尺寸 |

> **首轮有一处真漏网（P12）**：断言写成 `assert.match(page, /const closeWizard = \(\) => \{[\s\S]*?if \(wizardServer\) \{/)`
> —— `[\s\S]*?` 一路跨到了 `finishWizard` 里那处同名判断，于是删掉 `closeWizard` 里的提示也照样绿。
> 修法是**先切片再断言**（`sliceBetween(page, "const closeWizard = ", "const wizardDraftPayload = ")`）。
> 这正是 §23.6 与项目 TOOLING 里记过的那条坑，本轮在自己的测试里又踩了一次 —— 说明「切片」这件事
> 必须当成写断言的默认动作，而不是"想起才做"。

**过程中发现并修掉的另一个真缺陷（草稿试连不带凭据）**：`test-draft` 读的是请求里的
`env` / `headers`，而创建体把凭据放在 `envSecrets` / `headerSecrets`（服务端加密通道）。
向导最初直接把创建体发给 `test-draft`，于是**试连根本没带凭据** —— 一个完全正确的 token 也会
返回「连不上」，而用户会去反复检查那个 token。修法是抽出 `draftProbeValues`，
让「创建」与「试连」共用同一份凭据整理（前缀只补一次），并配三条断言：
模型层两个消费者必须给出同一份值、试连按落点分流、页面调用必须带上它。

### 24.9 一条必须记下的教训

**默认值本身就是功能。** 本轮做减法时最有价值的动作不是"少显示几个字段"，而是**替用户把答不出来的
选择题答掉**：作用域、适用环境、Agent、启用状态全部给最宽/最常见的默认。用户答不出来的问题不该问 ——
问了只会让人怀疑"我是不是得先搞懂才能用"。

配套两条：

- **目录的筛选标准比条目数量重要**：一条只写启动命令的条目，等于把「查依赖、查凭据格式」原样丢回给用户。
  宁可少放几条，也不要放"点进去必然失败"的条目（这正是 §23.2 里删掉 `example.com` 的同一条理由）。
- **凡是有中间状态的路径，取消时必须如实告知**：OAuth 路径先落库，所以「取消」要说明它已存在，
  否则用户以为没连上、列表里却多了一条。

---

## 25. 撤回 `--bare` 告警（2026-09-15）

**改动**：移除对话页「--bare 风险」徽章与支撑它的探测链路。

**理由**：§19.8 的探测只回答「`--help` 里有没有 `--bare`」这个**开关是否存在**，看不出它是否
**已成为 `-p` 的默认**。而在它成为默认之前，这条提示对用户没有任何可执行动作 —— 既不是故障，
也无需处理，只会在每个可用环境上常驻（实测本机 2.1.266 即命中）。常驻的告警会被学会忽略，
真出事那天反而失效。

**撤回了什么**（全链路，避免留死代码）：

- 前端：`ConversationPage.tsx` 徽章、`types.ts` 的 `ToolStatus.bare`、`style.css` 的
  `.runner-inline-warn`（该类名仅此一处使用）。
- 后端：`app.go` `listRunners` 中的 `bare` 写入分支、`claude_runner.go` 的
  `bareFlagReporter` 接口与 `BareFlagAvailable`、以及四类 runner（`claudeCLIRunner` /
  `sshRunner` / `windowsAgentRunner` / `wslAgentRunner`）各自的探测字段与缓存。

**§13 表格中的上游风险条目仍然有效**，只是不再由 UI 呈现。真到上游改默认那天，症状是
**Skill 区失灵 + CLAUDE.md 不进上下文**（§19.8 所述的降级清单不变）。届时按下面从源码复现：

```bash
git show ab7d601:apps/control-server/internal/app/claude_runner.go | grep -n -A 15 BareFlagAvailable
```

本文档中另外三处提及「纳入探测与告警」的位置（§0.2 #21、§9 风险表、§13 风险表）已同步改写为
「已撤回、风险仍在」，避免文档把读者引向不存在的代码。

**教训（与 §19.10 同源）**：**没有可执行动作的告警不是告警，是噪音。** 探测能力本身是对的
（不猜版本号），但「探测到即常驻提示」这个 UI 决定错了 —— 前瞻风险的合适归宿是文档与
发布检查项，不是每个用户的进度条。

