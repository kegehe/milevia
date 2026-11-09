# CLI 工具管理能力复查

> 本文保留调研时的缺口快照。2026-11-06 的修复结果与验证边界见 [60-CLI工具管理修复记录.md](60-CLI工具管理修复记录.md)。

调研日期：2026-11-06。对应用户要求：检查其他 CLI 是否有可以接入但尚未接入的管理能力。

本次检查六个现有 CLI 的目录、后端分派、登录状态、安装升级、配置和模型入口，并补查 Gemini CLI / GitHub Copilot CLI 两个目录外候选。确实存在漏接和误判，不能笼统归为“CLI 不支持”。本次仅新增本记录，未修改产品代码、提交或发布。

## 1. 当前能力概况

下表描述工作区当前实现；“已接”不表示所有系统、账号和版本均完成真机验收。安装升级路径未实际执行。

| CLI | 安装 / 升级管理 | 登录入口与状态 | 模型与配置 |
| --- | --- | --- | --- |
| Claude Code | npm 安装升级已接；官方原生安装的路径发现不完整 | CLI 提供 `auth login` / JSON `auth status`；统一管理页未开放 | 模型为静态建议；CLI 配置、托管档案已接 |
| Codex | npm 安装升级已接 | CLI 提供 `login` / `login status`，含设备授权；统一管理页未开放；SSH 就绪判断混入登录检查 | 原生模型目录有降级；CLI 配置、托管档案已接 |
| CodeBuddy Code | npm 安装升级已接 | 登录提供终端指引；前轮已补 ACP 状态探测与 `configured` | 模型仍只自填；帮助已有候选型号；统一配置 / 档案未接 |
| Qoder CLI | npm 安装升级已接 | 本机可启动授权并查询；WSL / SSH 状态查询漏登记；登录指引缺少 `login` 子命令 | 已接模型命令，本轮未做真实账号验收；统一配置 / 档案未接 |
| OpenCode | npm 安装升级已接 | 登录提供指引；已接 `auth list`，但凭据状态存在两种实测误判 | 已接 `models`；统一配置 / 档案未接 |
| Cursor CLI | 已接版本 / 新版查询和本机自更新；平台不代装；WSL / SSH 管理被整体拒绝 | 本机 `status` 已接，登录只给指引；官方已有输出 URL 的登录方式 | 平台仍返回空模型表，但官方已有 `models` / `--list-models`；统一配置 / 档案未接 |

前轮已完成的附件、事件、用量、推理强度与 Skill 发现见 [58-内置CLI能力补齐实施记录.md](58-内置CLI能力补齐实施记录.md)，本次没有将它们重新列为漏接。CodeBuddy 的 ACP 登录状态修复见 [42-CLI工具管理页调研与实施方案.md](42-CLI工具管理页调研与实施方案.md) 第 37 节。

## 2. 应优先修复的管理问题

### 2.1 Claude / Codex 的登录查询被“能否发起登录”一起禁用

- `agent_catalog.go` 中两者未设置 `SupportsLogin`。
- `agent_login.go` 的登录状态接口先检查这个字段，不满足即 501；`agent_auth.go` 也提前返回。前端只对 `supportsLogin` 为真的条目发起查询。
- 两者本机 / 跨端管理适配器也尚未实现统一 `LoginStatus`，因此只改目录开关仍不够。
- 本机 Claude 2.1.318 的 `auth --help`、Codex 0.167.0 的 `login --help` 均确认有状态命令；Codex 同时列出 `--device-auth`。Claude 官方文档确认 `auth status` 返回 JSON，未登录退出码为 1。

**影响：** 管理页不能统一显示这两个工具的账号登录状态，不能解释为 CLI 没有能力。项目档案管理不等价于目标 CLI 自身的账号状态。

**建议：** 分开声明“可查询认证状态”和“可启动登录”，补齐本机 / WSL / SSH 适配；按结构化字段、认证来源和退出码区分账号、配置凭据、未登录与无法确认。未登录退出码 1 不能统统作为探测故障。

代码：`agent_catalog.go:155`、`agent_catalog.go:187`、`agent_login.go:110`、`agent_auth.go:175`、`CliToolsPage.tsx:587`。

### 2.2 Qoder 跨端查询、精确指引和 JSON 状态未贯通

- 本机 `qoder_runner.go:278` 执行 `qoder status` 并解析文本。
- 目录没有 `AuthStatusArgs` / 协议，`catalogAgentBackend.LoginStatus` 在 WSL / SSH 直接返回 `unreadable`，不执行查询。临时诊断确认调用记录为空。
- 目录没有 `LoginArgs`，跨端给出的命令是裸 `qoder`，而非明确的 `qoder login`。裸命令可能进入 TUI，但指引不够准确。
- 复用临时目录中的官方 1.1.82 包，在隔离用户目录运行 `status --help`，确认支持 `-o, --output <text|json>`；`status -o json` 返回：

```json
{"logged_in": false, "version": "1.1.82", "allow_byok": 0}
```

**建议：** 共用 `status -o json` 的专用解析和命令声明；旧版明确降级到现有文本解析。依据 JSON 字段是否存在区分“未登录”和“格式不认识”，不能把缺失字段默认成 false。PAT 来源按官方认证优先级另行处理。

代码：`agent_catalog.go:252`、`agent_catalog_backend.go:251`、`qoder_runner.go:278`。

### 2.3 OpenCode 把凭据记录当账号登录，还漏读环境变量

使用已有官方 OpenCode 1.19.5，分别隔离 HOME / USERPROFILE / XDG 目录，只运行 `auth list`，不发模型请求：

| 隔离输入 | CLI 实际输出 | 当前平台解析 | 应有口径 |
| --- | --- | --- | --- |
| 没有凭据 | `0 credentials` | `logged_out` | 该结果符合预期 |
| auth.json 放入本地无效示例 Key | `OpenAI api`、`1 credentials` | `logged_in` | `configured`，未验证凭据有效性 |
| 仅设置无效示例 `OPENAI_API_KEY` | `0 credentials`，另有 `Environment` / `OpenAI OPENAI_API_KEY` / `1 environment variable` | `logged_out` | `configured`，需要合并认证来源 |

第三种情况会误导用户：CLI 明确识别到了环境变量，平台却因先匹配 `0 credentials` 提示再登录。第二种情况会让页面显示“已读取账号登录信息”，实际仅有一条本地 Key 记录。

`opencodeAuthFileHasCredentials()` 的错误回退同样直接返回 `logged_in`，应一并校正。OAuth 记录仍需区分是否有身份 / 可确认会话，不能把记录存在等同于线上有效。

**建议：** 专用解析凭据和 Environment 两个区段，保留来源 / 类型。不自动发送模型请求来验证。登录流程可利用本机帮助确认的 `auth login --provider --method` 跳过两层选择；不同 provider 的后续交互仍需单独适配，不能宣称所有登录都能无头完成。

代码：`opencode_runner.go:199`、`opencode_runner.go:244`、`agent_auth.go:220`。

### 2.4 通用状态解析缺少正向证据

`agentAuthStateFromStatusOutput()` 的规则是：非空、未匹配空标记，就返回 `logged_in`。临时诊断输入成功命令的陌生文本 `Warning: credentials cannot be read`，当前仍返回 `logged_in`。

这证明解析过宽；不声称在当前 Cursor 真机见到了这条输出。Cursor 和 OpenCode 本机以及目录后端都依赖此规则。

**建议：** 每个工具用明确身份 / 认证字段或有界正向文本模式判定。陌生输出保持 `unreadable`；日志脱敏，接口只返回固定说明。

### 2.5 升级后真探失败仍可拿旧版本当健康证据

`catalogAgentBackend.freshVersion()` 真探失败后回落 `Version()`；WSL 的后者会立即返回旧缓存。升级后健康检查和回滚核对也调用该方法。

临时桩实验：先缓存 `2.162.0`，随后所有真实探测均失败，`freshVersion()` 仍返回 `2.162.0`。这只证明旧版曾经可用，不能证明当前可执行文件健康。现有测试 `TestCatalogBackendUpdateFallsBackToTheCachedReadWhenFreshProbeFails` 甚至显式要求这个回退行为。

**建议：** 页面展示可以保留“上次读数”；升级前探测、升级后自检、回滚核对必须返回真实结果与错误，不能共享展示缓存的容错语义。探测失败应报告“未能确认健康”，不能直接等价于已损坏，也不能因此直接触发缺乏依据的回滚。

代码：`agent_catalog_backend.go:213`、`agent_catalog_backend_test.go:592`、`wsl_agent_runner.go:525`。本次未执行真实升级 / 回滚。

### 2.6 SSH Codex 的安装就绪与登录状态仍混在一起

本机 Codex 检查可执行文件；WSL 通过原生 `--version` 检查。SSH 的 `CodexReady()` 仍执行 `command -v codex && codex login status`。

`probeAgent()` 对目录中 `readinessBinary` 的 Codex 调用这个 Ready；已安装但未登录时可能进入 unavailable，没有安装登记时给出“未安装”，且不再读取版本。

**建议：** SSH 也用目标环境二进制 / 版本判断安装就绪，登录另走认证读数。这属于代码路径确认，未连接真实 SSH 账号复现。

代码：`ssh_runner.go:1074`、`app.go:9136`、`agent_probe.go:91`；对比 `codex_runner.go:130`、`wsl_agent_runner.go:439`。

## 3. 已有官方能力、平台尚未接入的优化

| 能力 | 证据与现状 | 建议与边界 |
| --- | --- | --- |
| Cursor 模型目录 | 官方已有 `agent models`、`--list-models`；`conversation_models.go:228` 仍注明“没有非交互模型目录命令”并返回空表 | 在执行环境读取账号目录并缓存，失败保留自填；需安装版输出校准 |
| Cursor 登录链接 | 官方明确 `NO_OPEN_BROWSER=1 agent login` 可打印 URL；`cursor_runner.go:199` 仅给指引 | 可持有授权进程并返回链接；需处理超时、重复点击、退出、取消与 URL 安全展示 |
| Cursor 跨端管理 | 官方支持 Linux / WSL；通用后端已有别名、版本、自更新与状态命令，但 `agent_probe.go:181` 拒绝跨端 | 分开管理与普通会话能力；不可仅删除拒绝分支导致对话误报可用 |
| Cursor 平台安装 | 有官方安装器；当前 `SupportsInstall:false` | 属产品扩展；需原生下载 / 验证 / 安装 / 来源登记路径，不能安装同名第三方 npm 包 |
| Claude 原生安装发现 | 官方位置包括 `~/.local/bin/claude`、Windows `~/.local/bin/claude.exe`；当前按 npm 分类，兜底只查 npm 目录 | GUI 继承旧 PATH 且无覆盖 / 登记时可能漏掉原生版；分发方式与发现位置不应绑定成单选 |
| Claude npm 运行时要求 | registry 2.1.318 的 `engines.node` 为 `>=22.0.0`，平台填 18 | 按待安装版本读 engines，区分安装器与 CLI 运行时。官方说明旧 Node 通常只产生 EBADENGINE，二进制仍能运行，因此不是必然安装失败 |
| CodeBuddy 型号建议 | 本机 2.181.0 `--help` 的 `--model` 已列出支持 ID；平台仍只自填并举 `gpt-5` 旧示例 | 可提供注明版本 / 来源的候选；帮助表不等于账号授权目录，不能保证账号全部可用 |

CodeBuddy 包中还出现 `_codebuddy.ai/models/refresh` 等私有扩展字符串；本轮未调用或确认其参数 / 会话要求，不把字符串存在当成已验证的稳定模型查询接口。

## 4. 需要独立设计的能力

1. **四个新增工具的统一凭据 / 档案路由。** 数据库 CHECK 和 `SupportsProfiles` 仍限制 Claude / Codex，配置快照也只读这两种。接入另四种需要迁移、密钥保护、来源优先级和逐 runner 注入验证。Qoder 官方明确 PAT 环境变量优先于保存的账号登录。
2. **完整插件生命周期。** 当前发现与展示已接，不等于安装、更新、启停、卸载。多个 CLI 有原生命令 / 配置入口，但不存在统一可直接复用的生命周期；SSH manifest 扫描还明确返回未接。`discovered` 不等于已启用。
3. **原生 MCP 认证 / 管理状态。** 当前平台 MCP 定义、连接测试与注入不等于已读取各 CLI 自己的配置和 OAuth 状态。OpenCode 官方的 `mcp list` / `mcp auth list` 可作为后续只读诊断来源。Cursor 当前采用用户配置并明确记录未注入；本轮未证明有会话级配置替代参数，不随意取消边界。
4. **普通会话跨端接入。** 管理查询与会话能力可分别推进，但对话仍涉及事件、恢复、权限、附件、进程树与工作区保护；管理能查不代表对话已支持。

## 5. 目录之外的两个明确候选

当前正式目录只有六个工具；测试中出现 Gemini 不代表产品已经接入。

| 候选 | 官方已确认能力 | 本次核对的 npm 元数据 | 接入判断 |
| --- | --- | --- | --- |
| Gemini CLI | 官方 headless 文档提供 `-p/--prompt`、结构化 JSON 与流式 JSON；支持账号 / API Key 等认证 | `@google/gemini-cli` 0.67.0，`gemini → bundle/gemini.js`，Node >=20 | 管理和普通会话有明确适配基础，值得优先评估；未安装运行，不能宣称已接 |
| GitHub Copilot CLI | 官方支持 `copilot -p` 程序化调用和工具授权控制 | `@github/copilot` 1.0.106，`copilot → npm-loader.js` | 值得接入；认证、事件输出、恢复和系统依赖需验收；不要与旧 `gh copilot` 扩展混同 |

仅添加 npm 包和命令，不能自动获得登录、项目对话、模型或凭据路由能力。这是新增适配器范围。

## 6. 实测、诊断与限制

- PATH 中核对 Claude 2.1.318、Codex 0.167.0、CodeBuddy 2.181.0。未执行更新，不因版本与前轮不同就推断本轮升级。
- Qoder 使用此前下载的官方 1.1.82 临时包；OpenCode 使用已有临时官方 1.19.5 安装。它们不在当前 PATH，不能因此说平台所有环境都没有安装。
- Qoder / OpenCode 状态实验隔离用户目录，无真实 Key。未启动授权、注销、模型对话、计费请求、安装或升级；未连接生产 SSH。
- 临时 Go 诊断共 **9 个预期行为断言失败**：Claude / Codex 各一项、Qoder 查询 / 指引各一项、OpenCode 两项、陌生输出、Node 元数据闸门、真探回退缓存各一项。它们用于证明缺口，不是回归通过证据，也不等于 9 个独立根因。
- 临时诊断代码已复制到系统临时目录并从项目移除，不会把故意失败的测试留给项目。
- 移除临时诊断后，选取现有认证、目录分派、登录指引、升级读数和运行时闸门共 **8 项现有测试通过**。部分测试恰好固化了上述旧行为，因此“现有测试通过”不能反证这些缺口不存在。没有将本轮针对性检查表述为全量回归。
- Cursor 安装版、Qoder 登录账号、六工具 WSL / SSH、真实升级 / 回滚尚未验收；相应条目已区分官方文档、代码路径与隔离实测。
- OpenAI Docs 官方 Codex 页面多次读取均为 HTTP 403，因此 Codex 具体命令以本机 0.167.0 帮助核对，未将无法读取的页面当成已验证证据。

隔离输出、诊断代码与日志：

```text
%TEMP%\milevia-cli-management-audit-798yxrwc\
  qoder-status.json
  qoder-status-help.json
  qoder-status-json.json
  opencode-empty.json
  opencode-fixture.json
  opencode-env.json
  opencode-help.json
  cli_management_audit_temp_test.go
  go-desired-behavior-final.log
  existing-tests.log
```

## 7. 建议实施顺序

1. 修认证 / 安装误判与升级自检：OpenCode 来源解析、通用解析正向证据、SSH Codex 就绪判断、真探与展示缓存分离。
2. 补已有原生命令：Claude / Codex 登录查询、Qoder JSON 及跨端指引、Cursor 模型目录与授权链接。
3. 补安装来源发现和运行时元数据，再设计四工具档案路由 / 原生管理；Gemini、Copilot 分别实现与验收。

## 8. 官方资料

- Claude CLI 与认证：https://code.claude.com/docs/en/cli-reference
- Claude 原生安装与 npm 要求：https://code.claude.com/docs/en/setup
- Qoder 参数：https://docs.qoder.com/cli/cli-reference.md
- Qoder 认证 / PAT 优先级：https://docs.qoder.com/cli/authentication.md
- OpenCode auth、MCP 与命令：https://opencode.ai/docs/cli/
- Cursor 模型 / 参数：https://cursor.com/docs/cli/reference/parameters
- Cursor 认证 / NO_OPEN_BROWSER：https://cursor.com/docs/cli/reference/authentication
- Cursor 安装：https://cursor.com/docs/cli/installation
- Gemini 无头输出：https://geminicli.com/docs/cli/headless/
- Gemini 认证：https://geminicli.com/docs/get-started/authentication/
- Copilot CLI 程序化调用：https://docs.github.com/en/copilot/concepts/agents/about-copilot-cli
- npm 元数据：对应官方包的 `https://registry.npmjs.org/<package>/latest`；只查询元数据，未执行安装脚本。
