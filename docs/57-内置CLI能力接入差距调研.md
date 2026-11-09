# 内置 CLI 能力接入差距调研

调研日期：2026-11-05。

后续实施见 [58-内置CLI能力补齐实施记录.md](58-内置CLI能力补齐实施记录.md)。本文保留实施前的调研结论，当前能力与验证边界以实施记录为准。

## 1. 结论与范围

当前目录登记了 Claude Code、Codex、CodeBuddy Code、Qoder CLI、OpenCode、Cursor CLI 六种工具。六者都登记为可参与项目对话，但这不代表原生能力、运行环境和验证程度一致。

主要差距分为四类：

1. 缺少原生参数映射或专用入口：typed 图片/附件输入、会话启动时主 Agent 选择、插件管理、原生会话导入、完整协议交互等。部分能力可能通过斜杠命令或 CLI 自有配置使用，不能统称完全不可用。
2. 实现与入口未贯通：OpenCode 的 variant 参数、CodeBuddy 的斜杠命令目录等。
3. 适配过程丢失信息：CodeBuddy/Qoder 的工具事件、用量、子代理关联；OpenCode 普通对话的用量字段等。
4. 旧版本假设需要复核：CodeBuddy 权限映射、Claude 推理强度选项等。

本次检查以当前工作区为准，包含尚未提交的任务托管实现。普通对话与任务托管分别判断，不能把托管专用的适配计入普通对话。本文不宣称六种 CLI 的全部功能均已枚举，也不把“官方有接口”当作“当前账号已跑通”。

证据来源：生产代码、相关测试、现有设计/实验记录、本机 CLI 帮助及官方文档。实际运行了帮助、版本查询、Codex schema 生成及限定的自动化测试，没有发起真实模型任务，没有进行安装、升级或发布。

本机 PATH 可找到：Claude Code 2.1.308、Codex 0.166.0、CodeBuddy 2.177.0。未在当前 PATH 找到 Qoder、OpenCode、Cursor；这不等于其他 Runner 或平台托管目录没有安装。

## 1.1 逐项复核结论

上一版结论已用临时测试桩重新走了一遍实际代码路径。结果不是“每一条官方能力都没接”，而是下面这些断点可以被当前工作区直接证明：

| 结论 | 复核结果 | 直接证据 |
|---|---|---|
| CodeBuddy/Qoder 普通会话工具事件未进入平台事件流 | **确认存在** | `streamJSONSession.readOutput` 只处理 `system/init`、assistant 文本、result；不会调用 `sink.Event`。同一份含 tool_use/tool_result/usage 的 fixture 两个工具均得到 0 条转发事件 |
| OpenCode 普通会话用量未进入统计层 | **确认存在** | `usage.go` 能解析 `opencode.step_finish`，但 `readOpenCodeJSONL` 只结束回合，不发送 `opencode.step_finish`；同一 fixture 的直接解析为 17 tokens，普通 parser 路径为 0 |
| OpenCode/CodeBuddy/Qoder 的推理强度没有普通会话设置入口 | **确认存在** | `/models` 返回三者 `reasoningOptions=[]`；POST `reasoning-effort=high` 返回 400。OpenCode runner 仍能接受 `--variant high`，CodeBuddy/Qoder runner 参数中没有 effort |
| CodeBuddy 斜杠目录没有接通 | **确认存在** | 目录声明 `slashCommands=true`，但 `observeCommandCatalog` 只接受 Claude/Qoder；CodeBuddy 的命令接口固定返回空目录和“尚未接入” |
| CodeBuddy 两个可写权限档位的参数映射未区分 | **确认存在，但实际安全边界仍需真机验证** | `codebuddySessionArgs(workspace_write)` 与 `(...full_control)` 完全相同，均为 `--dangerously-skip-permissions`。这证明平台没有表达差异，不能单凭静态代码断言 CLI 实际越界 |
| Skill 面板当前只发现 Claude/Codex | **确认存在** | `discoverLocalSkillRoots` 只扫描 `.claude`、`.codex`，远端同样只拼这两类路径；CodeBuddy/Qoder/OpenCode/Cursor 的平台筛选结果为空 |
| 普通运行请求没有 typed 图片/附件 | **确认存在，但已有文件路径引用** | `AgentRunRequest` 没有附件/图片/文件字段，Claude 输入帧只编码一个 `type=text` 内容块；文件页已有“加入对话”，实际向输入框插入 `@路径`，不等同于原生附件参数映射 |
| CodeBuddy/Qoder/Cursor 普通对话仅支持控制服务所在本机环境 | **确认存在** | 创建会话及发送消息均检查 `isLocalRunnerID`；管理页可安装/更新或任务托管有跨环境实现，不代表普通对话已支持 WSL/SSH |

以下表述需要收窄：

- “没有 Skill/插件”不准确。准确说法是“平台没有发现、展示或管理这些 CLI 的 Skill/插件”；CLI 仍可能按自己的配置自动加载。
- “没有审批/提问能力”不准确。平台已有 Claude Hook 审批和 MCP `ask_user` 提问；缺口是没有把每个 CLI 的原生协议全面接入。
- “没有文件输入能力”不准确。已有文件树/文件页到对话框的 `@路径` 引用，CLI 可按自身能力读取；缺少的是 typed 图片/附件消息及原生附件参数映射，不能据此断言 CLI 无法处理项目内图片。
- “没有专用设置就完全不能用”不准确。Claude/Qoder 有通用命令入口，CodeBuddy 也允许手输斜杠命令；`agents`、`autocompact`、`effort` 等部分能力可能经此使用。具体命令能否在当前 headless 版本执行、执行后平台是否同步状态，仍需逐项验证。
- “CodeBuddy 权限不安全”没有充分证据。当前能确认的是两档映射相同，实际边界要用隔离项目和真实 CLI 版本验证。
- “Codex 没有 App Server 能力”不准确。平台已经用 `thread/revert`，本机 schema 也确认更多方法；缺口是尚未把它作为普通对话主协议全面使用。

## 2. 当前平台能力矩阵

以下是普通项目对话的实现情况。受环境、版本、账号约束的能力不能直接理解为所有环境可用。

| 能力 | Claude Code | Codex | CodeBuddy | Qoder | OpenCode | Cursor |
|---|---|---|---|---|---|---|
| 对话执行方式 | 常驻 stream-json | 每轮 exec | 常驻 stream-json | 常驻 stream-json | 每轮 run | 每轮 print |
| 普通对话运行环境 | 有本机/WSL/SSH 路径 | 有本机/WSL/SSH 路径 | 仅服务所在本机 | 仅服务所在本机 | 有本机/WSL/SSH 路径 | 仅服务所在本机 |
| 会话恢复 | 已接 | 已接 | 已接 | 已接 | 已接 | 已接 |
| 手动指定模型 | 已接 | 已接 | 已接 | 已接 | 已接 | 已接 |
| 模型候选目录 | 内置建议 | CLI 探测及回退 | 空，需手填 | 空，需手填 | 空，需手填 | 空，需手填 |
| 推理强度设置 | low/medium/high | low/medium/high/xhigh | 未开放 | 未开放 | runner 有参数，设置未开放 | 未发现可直接映射的参数 |
| 平台逐命令审批 | 已接 Hook | exec 路径未接 | 未接 | 未接 | run 路径未接 | print 路径未接 |
| 当轮插话 | 已接，SSH 除外 | 未接 | 未开放 | 本机会话已接 | 未接 | 未接 |
| 斜杠命令目录 | 已接 | exec 不提供 | 声明支持，目录未接 | 已接 | 未接 | 未接 |
| 平台 Skill 发现 | 已接 | 已接，扫描路径仍可扩展 | 未接 | 未接 | 未接 | 未接 |
| 平台托管档案/凭据路由 | 已接 | 已接 | 未接 | 未接 | 未接 | 未接 |
| 项目 MCP 注入 | 已接，远端需能力探测 | 本地/WSL 已接，SSH 未接 | 已接参数链路 | 已接参数链路 | 已接参数链路 | 未注入，读取 CLI 自有配置 |
| 优化建议功能 | 已接 | 已接 | 未开放 | 未开放 | 已接 | 未开放 |
| 单文件冲突建议 | 已接 | 已接 | 已接 | 已接 | 已接 | 已接 |

清单来源：`apps/control-server/internal/app/agent_catalog.go`。矩阵同时核对了 `conversation_models.go`、`project_commands.go`、`skills.go`、`mcp_config.go` 和各 runner，未仅按目录标志判断。

运行环境行表示存在代码路径，不代表所有组合均已真机验收；`isLocalRunnerID` 在 Windows 仅接受空 ID/`windows-local`，非 Windows 仅接受空 ID/`wsl-local`。因此 Windows 控制服务中的 WSL 项目不属于这里的“本机”。限制位置：`app.go:3670`、`:5974`、`:8894`。

## 3. 优先处理的确定问题

### 3.1 CodeBuddy 的两档权限实际使用相同参数

位置：`codebuddy_runner.go:161`、`agent_catalog.go:228`。

`workspace_write` 和 `full_control` 都追加 `--dangerously-skip-permissions`，只有 `read_only` 使用 plan。普通对话这条参数映射没有区分两个可写档位，不能据此保证“项目内执行”具备独立的工作区边界。

代码注释依据 2.156.0 的实验，认为 `acceptEdits` 不是合法值；本机 2.177.0 帮助已经把它列入合法权限模式。新版帮助还说明跳过权限后 HIGH/CRITICAL 仍可能询问。因此，当前映射和“完全控制直接执行”的说明都需要重新校准。

建议优先用隔离目录实测：项目内写入、项目外写入、shell 执行、高风险审批、MCP 调用及 plan 退出。帮助确认参数存在，不能证明 headless 下的完整行为。

### 3.2 CodeBuddy/Qoder 的事件没有贯通到平台

位置：`stream_json_session.go:293`。

共享解析器只处理 init 的会话标识、assistant 的 text、result 的回合终态；没有调用 `sink.Event` 转发原始或规范化事件。

这意味着普通会话即使成功执行了工具，也没有通过这条实现把工具调用、工具结果、result 用量、子代理父子关联及 partial 增量完整送到平台。影响范围比增加一个按钮大：时间线、使用统计和子代理过程都会缺少数据。

建议先贯通事件及脱敏处理，再补 UI。使用包含工具调用、工具失败、嵌套子代理、用量与恢复会话的真实事件 fixture 验证。不能仅以“最终回复正确”作为接入完成标准。

### 3.3 推理强度存在三种断点

位置：`conversation_models.go:249`、`:305`；`opencode_runner.go:383`；`codebuddy_runner.go:161`；`qoder_runner.go:365`。

- OpenCode：runner 已支持 `--variant`，设置层只允许 Claude/Codex，普通会话无法正常设置非空 variant。
- CodeBuddy：本机帮助已有 `--effort`，runner 未传递，设置层也未开放。
- Qoder：官方当前参考已有 `--reasoning-effort`，runner 未传递，设置层未开放；尚需安装版本核实。
- Claude：本机帮助支持 low/medium/high/xhigh/max，平台只开放前三档。具体模型、账号是否允许某档仍需探测。

建议能力按 CLI 版本、provider/model 返回，避免把一组静态档位套给所有模型。

### 3.4 CodeBuddy 的命令能力声明与接口不一致

位置：`agent_catalog.go:241`、`project_commands.go:766`。

目录声明 `SlashCommands: true`，但命令接口直接返回空目录和“尚未接入”。现有代码注释指出 CodeBuddy init 帧本来带有 `slash_commands`。

这不等于斜杠命令完全不能发送。`ConversationPage.tsx:3896` 按能力声明放行，因此 CodeBuddy 仍可手输斜杠命令；缺的是发现、选择及可执行性说明，具体命令是否被 CLI 执行还需实测。

建议接通 CodeBuddy 自己的目录缓存与观察入口，按非交互可执行性区分命令；不要使用 Claude 的静态命令表替代。

### 3.5 OpenCode 普通对话的用量链路断开

位置：`opencode_runner.go:570`、`:596`；`usage.go:177`。

普通 runner 的事件结构没有保留 step_finish 的 tokens；处理 step_finish 时也没有发送 `opencode.step_finish`。用量累加器虽然已有该事件的解析分支，但普通 runner 没有把数据送进来。

任务托管有独立的 `supervision_opencode_stream.go` 和用量处理，不能据此认为普通对话已经贯通。

建议补普通会话的 token、cache、模型及费用可用性标记；不提供的数据应明确为未知。检查多 step 情况，避免将某一步用量误当整个回合总量。

## 4. 各 CLI 尚未充分利用的能力

### Claude Code

已接入较深：流式回复、Hook 审批、插话、MCP、命令目录、模型、用量、后台任务生命周期及子代理展示均有实现。不能将这些笼统列为缺失。

以下主要是缺少专用入口或启动参数映射，不能全部判为功能不可用。现有命令目录已为 `effort`、`agents`、`list-agents`、`autocompact`、`reload-skills` 等提供中文标注；真实目录及 headless 可执行性仍取决于 CLI。`reload-plugins` 标注仅终端可用。现有通用命令入口与专用设置、管理和状态同步应分别判断。

仍可完善：

- 会话级 `--agent` / `--agents`：选择和配置主 Agent、自定义子代理。
- `--max-budget-usd`、`--max-turns`、`--autocompact`：CLI 原生运行预算和上下文策略的启动参数/专用设置。`autocompact` 已有通用命令标注，是否可经命令使用需实测；平台自己的超时、配额、定时任务与这些原生参数不同。
- `--fallback-model`：模型不可用时的回退策略。
- 插件安装/启停、输出风格、额外目录授权、系统提示词管理。
- 原生 `--fork-session`：当前分支实现主要复制平台消息并重建会话，未直接利用该原生能力。
- `--forward-subagent-text`、Hook 生命周期事件等更细的输出。现有子代理展示可用，但 runner 未显式启用这些增强参数，能否补齐缺失细节需实验。

### Codex

当前普通执行主路径为 `codex exec --json`。App Server 并非完全未使用：本地回退已调用 `thread/revert`，位置为 `checkpoint.go:613`。

本次用本机 0.166.0 生成了 App Server experimental schema，确认存在：

- `turn/steer`、`turn/interrupt`。
- `item/commandExecution/requestApproval`、`item/fileChange/requestApproval`、`item/permissions/requestApproval`。
- `item/tool/requestUserInput`、`turn/plan/updated`。
- `thread/fork`、`thread/compact/start`、`thread/revert`。
- `model/list`、`skills/list`、插件查询和管理、`review/start`。

这些协议能力尚未作为普通对话的完整交互适配接入。因此，“Codex exec 没有审批通道”应理解为当前选用入口的限制，不能推广为 Codex 整体没有审批能力。

本机还确认 `codex exec fork`、`codex queue`、`codex review` 和 `--image` 存在。可分别评估原生分支、会话消息投递、代码审查与图片输入。

建议保留 exec 兼容路径，先对当前自定义 provider 做 App Server POC。协议 schema 存在不代表当前账号、模型和传输均兼容；项目旧调研已记录 provider 兼容问题。

### CodeBuddy Code

本机 2.177.0 相比当前普通 runner 使用的能力明显更丰富：

- `--effort`、模型候选信息、工具白名单/黑名单。
- `--agent`、`--agents`、子代理权限设置、`--swarm`。
- `--acp`、`--serve`：可供客户端集成的协议/API 入口，值得先做交互审批和过程事件 POC。
- partial 输出、JSON Schema、原生会话分支、插件管理。
- `--sandbox` 及后台会话管理。
- `--max-turns`、`--autocompact`、fallback model。

其中事件贯通、权限映射和推理强度优先于 swarm、后台守护和远程 sandbox。后几项涉及资源生命周期，不能仅透传参数。

### Qoder CLI

当前已接常驻会话、恢复、MCP 参数、模型覆盖、命令目录和当轮插话。

上述普通会话仅支持控制服务所在本机环境；WSL/SSH 的安装管理能力不等于已支持普通对话。

官方当前参考还提供模型列表、推理强度、附件、主 Agent/自定义 Agent、原生 fork、worktree、max turns、输出 token 限制、上下文窗口、输出风格、额外目录、插件目录及 ACP。

平台尚未把上述能力形成完整入口。模型接口已知 `--list-models` 存在，但仍返回空目录，原因是登录后的输出尚未校准。优先补模型目录、事件与用量、推理强度及技能发现；ACP/审批能力需另做版本和协议实验。

### OpenCode

当前已接普通执行、恢复、MCP、模型覆盖、权限配置、优化建议和冲突建议。

官方 CLI 已提供 `models`，但平台模型接口仍返回空目录；可优先补 provider/model 的真实候选列表。

其他值得接入的能力包括 variant、`--agent`、`--command`、`--file`、原生 `--fork`、thinking、serve/attach。

现有 runner 把原生 `question` 显式 deny，避免无头运行无人回应。OpenCode 的 server API 已有会话、权限和问题相关接口，可评估更完整的协议适配；不能直接删除 deny 就视为支持。

### Cursor CLI

当前主要接入 print、恢复、模型、ask 模式和 force/sandbox 参数映射。官方概览还提供 plan 模式，平台没有独立的计划工作流。

平台 MCP 选择不会注入 Cursor，会明确记录“本次未注入”；Cursor 仍可能使用其用户级和项目级 `.cursor/mcp.json`。因此应描述为“平台与 CLI 配置未打通”，不能说 Cursor 没有 MCP。

下一步适合先校准当前版本的权限与沙箱行为、配置读取、计划模式、模型目录来源及 MCP 生效展示。没有本机 CLI 及真实账号证据的部分保留待验证，不猜测其 headless 支持的交互能力。

## 5. 跨工具缺口与已有替代实现

### 图片与附件输入

普通 `AgentRunRequest` 只有字符串 Prompt，没有 typed content/attachments；Claude 用户帧也只编码 text。对话输入粘贴读取 text/plain，未形成图片上传到运行参数的链路。

但已有文件引用入口：`FilesPage.tsx:28` 和 `ProjectFileTree.tsx:1115` 将路径交给对话，`ConversationPage.tsx:2770` 至 `:2782` 将其插为 `@路径` 文本。CLI 可能依此读取项目文件或图片，因此这里只确认“缺少结构化附件消息与原生参数映射”，不宣称文件或图片完全无法使用。

本机 Codex 已确认 `--image`；Qoder 官方提供 `--attachment`；OpenCode 官方提供 `--file`。文件预览、文件树上传与“作为模型消息附件发送”是不同链路。

实现需要同时覆盖消息模型、文件生命周期、运行环境路径映射、恢复重放、手机访问和 CLI 适配，不能只增加上传按钮。

### Skill 与插件

`skills.go` 当前只识别 Claude/Codex，扫描 `.claude/skills`、`.codex/skills` 和 Claude 插件树。其他四种工具的技能没有平台发现入口；CLI 自己可能仍会加载本地配置，这不代表平台可浏览和管理。

Codex 的共享 `.agents/skills` 等来源及插件提供的技能也需核对覆盖范围。优先评估原生 `skills/list`，减少平台自己猜测目录规则。

### 原生分支与外部会话

平台已有历史搜索、恢复、checkpoint、回退和分支；不能统称“没有会话管理”。但 `forkConversationAtCheckpoint` 创建新 ID 并复制平台消息，并未调用各 CLI 原生 fork。

导入用户在终端创建的原生会话，以及保留原生工具历史的分支，仍有独立价值。应区分平台消息复制、上下文重建和原生会话分支三种保真程度。

### 提问与审批

平台已有结构化提问卡片，通过内置 MCP `ask_user` 实现；不是完全没有提问能力。当前 `mcp_config.go` 只在 Windows 目标下注入，Cursor 又明确跳过平台注入；WSL/SSH 仍有地址/隧道缺口。

原生 AskUserQuestion 是既有调研后的明确取舍，`docs/51` 记录了模型/provider 可用性不稳定。优先补现有 MCP 通道覆盖；原生协议适配应按工具验证，避免同一会话出现两套行为不同的提问机制。

### 任务托管与原生多 Agent

工作区已存在大量任务托管实现，含 CLI 专用受控执行、版本要求、权限验证和跨环境代码。不能将其忽略后得出“平台没有多 Agent/托管”。

另一方面，平台任务托管、CLI 主 Agent 选择、CLI 原生子代理/swarm 是不同能力。Qoder/Cursor 托管适配仍带 `ValidationPending`；其他组合也应按真实验收矩阵判断，不能只按接口是否存在判断。

## 6. 建议实施顺序

| 顺序 | 工作项 | 完成判据 |
|---|---|---|
| 第一批 | CodeBuddy 权限校准；CodeBuddy/Qoder 事件贯通；普通 OpenCode 用量；能力声明纠偏 | 页面声明与实际权限一致，工具过程及可用用量可见，缺失数据明确为未知 |
| 第二批 | 推理强度贯通；OpenCode/Qoder 模型目录；CodeBuddy 命令目录 | 能从真实目录选择，参数实际进入目标进程，非法组合有明确反馈 |
| 第三批 | 图片/附件；各 CLI Skill 来源；主 Agent 选择；原生会话分支 | 输入、历史、恢复、远端路径与生命周期端到端正确 |
| 第四批 | Codex App Server、OpenCode Server、CodeBuddy/Qoder ACP POC | 审批、提问、中断、插话、进度、provider 兼容性均有真实协议证据 |
| 后续 | 插件管理、预算与上下文策略、后台会话、远程 sandbox、云任务 | 每项有明确收益、资源回收和跨环境边界 |

架构建议：沿用现有 AgentRunner 边界及工具目录，补充“CLI + 版本 + Runner + provider/model”的能力结果及验证来源。将消息内容、事件、控制操作分开建模；不要在 UI 中为所有工具硬套同一组按钮，也不要继续让新字段存在但没有上层入口。

## 7. 验证结果与限制

复核阶段另跑了 7 组临时测试（含子测试），覆盖共享流解析、OpenCode 用量、三个工具的推理强度 HTTP 接口、CodeBuddy 命令目录、权限参数、Skill 发现与档案支持、纯文本请求结构。全部通过，耗时 3.492s；这里的通过表示成功复现当前缺口。

其中 CodeBuddy/Qoder 事件转发数均为 0；OpenCode 相同样本直接交给统计器得到 17 input tokens，经普通 parser 后为 0；三个工具设置 `high` 均返回 HTTP 400。测试使用合成事件和 `httptest`，没有向真实模型发请求。临时测试文件已从项目移除，未加入业务实现。

14 项现有后端测试通过，涵盖目录一致性、权限选项、OpenCode 参数/权限、CodeBuddy 模型空目录、三个工具命令目录行为、模型传参及 Skill 过滤/扫描。

```text
go test ./internal/app -run '^(TestCatalogEntriesAreSelfConsistent|TestPermissionModesCoversEveryCatalogAgent|TestValidProfileAgentReadsCatalogNotHardcodedPair|TestOpenCodeExecArgs|TestOpenCodePermissionJSON|TestConversationModelsCodeBuddyReturnsEmptyArray|TestProjectCommandsForCodexHasNoSlashCommands|TestProjectCommandsForOpenCodeHasNoSlashCommands|TestProjectCommandsForCodeBuddyDoesNotUseClaudeCatalog|TestParseCodexModelCatalogFiltersHiddenModels|TestClaudeRunnerArgsUseRequestModel|TestCodexExecArgsPassModel|TestScanSkillTreeFindsCodexSystemSkills|TestFilterSkillsByAgent)$' -count=1 -timeout 120s

ok github.com/kegehe/milevia/apps/control-server/internal/app 2.793s
```

这些测试确认当前行为，没有证明缺失能力已实现，也没有覆盖全部环境或真实模型任务。部分测试明确固定了空目录等现状，所以“测试通过”与“接入完整”不能等同。

Codex 官方 App Server 与 CLI 参考页面本次均返回 HTTP 403；相关协议事实取自本机 0.166.0 生成的 467 个 schema 文件及帮助，不将未读到的网页当作依据。其他官方页面为本次实际读取内容；文档可能先于或晚于用户安装的 CLI。

本轮只新增本文，没有修改业务代码，没有执行暂存、提交、推送或发布。

## 8. 官方参考

- Claude Code CLI：https://code.claude.com/docs/en/cli-reference
- CodeBuddy CLI：https://www.codebuddy.ai/docs/cli/cli-reference
- Qoder CLI：https://docs.qoder.com/cli/cli-reference
- OpenCode CLI：https://opencode.ai/docs/cli/
- OpenCode Server：https://opencode.ai/docs/server/
- OpenCode Agents：https://opencode.ai/docs/agents/
- Cursor CLI：https://cursor.com/docs/cli/overview
- Codex App Server（本次 403）：https://developers.openai.com/codex/app-server/
- Codex CLI 参考（本次 403）：https://developers.openai.com/codex/cli/reference/
