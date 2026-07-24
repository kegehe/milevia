# Codex CLI 集成调研与实施方案

> 日期：2026-07-23  
> 目标：在创建项目对话时选择 Claude Code 或 Codex CLI，并保持项目、任务、运行记录、停止、实时过程、历史会话和审计链路一致。  
> 结论：可以接入，但不能只增加一个前端下拉框；应将“执行环境 Runner”和“AI 工具 Adapter”拆开。本机 `codex-cli 0.144.6` 已通过阶段 0：`codex exec --json` 加 `exec resume` 可恢复同一会话，Adapter 必须用进程工作目录固定项目路径，并用 `-c sandbox_mode="..."` 在恢复轮重申策略；不承诺 Codex 的逐命令网页审批；App Server 留作二期增强。

## 1. 调研范围与证据

本次结论基于以下实际检查，而非仅依据设计文档：

- 控制服务代码：`apps/control-server/internal/app/app.go`、`claude_runner.go`、`ssh_runner.go`、`usage.go`；
- Web 页面：`apps/web/src/App.tsx`；
- 本机 CLI：`codex-cli 0.144.6`，`codex login status` 和 `codex doctor --json` 均通过；
- 本机 CLI 帮助：`codex exec --help`、`codex exec resume --help`、`codex app-server --help`；
- 本机生成的 App Server v2 JSON Schema：`codex app-server generate-json-schema --experimental`。
- 阶段 0 实测：首轮 `--json` 返回 `thread.started.thread_id`；恢复轮可复用该 ID。将子进程 `Dir` 设为项目目录，并传入 `-c sandbox_mode="read-only"` 后，`pwd` 返回项目目录且写文件被只读沙箱拒绝。

本机当前 Codex 使用的是已配置的自定义 provider。健康检查显示 provider HTTP 可达，但 Responses WebSocket 不可用。因此不能假定 App Server 的实时传输在当前 provider 上可直接作为生产依赖，必须先做兼容性 POC。

官方 Codex 手册抓取因 `developers.openai.com` 返回 HTTP 403 未成功；本文关于 CLI 参数和 App Server 协议的事实以本机已安装版本的帮助和生成 schema 为准。CLI 升级后必须重新生成 schema 并跑兼容性测试。

## 2. 当前项目实际情况

### 2.1 已有的正确基础

- 项目、会话、消息、Run 和事件均已持久化到 SQLite；页面刷新后可以恢复时间线。
- `AgentRunner`、`StreamingAgentRunner` 和 `AgentSession` 已经形成了一个初步的 CLI 边界，见 `claude_runner.go`。
- 本地 WSL 与 SSH Runner 都可执行 Claude，且项目已按 `projects.runner` 选择执行位置。
- 所有 AI Run 都通过项目工作区租约互斥，避免与 Git 写操作并发。
- Claude 的原始 JSON 事件、助理文本、停止、WebSocket 和任务编排已经接入同一条 Run 链路。

这意味着不需要重新设计项目、任务、Run、事件和 WebSocket 的主链路。

### 2.2 当前的耦合点

当前 `Runner` 实际同时代表“机器环境”和“Claude 工具”，不能表示同一台机器有多个 AI CLI：

| 位置 | 当前耦合 | 后果 |
| --- | --- | --- |
| `Config` | 只有 `ClaudePath`、`AUTO_CLAUDE_PERMISSION_MODE`、Claude 审批 Hook | 无法配置 Codex 路径、模型或沙箱策略。 |
| `projects` | `claude_ready` | 项目可用性被错误地等同于 Claude 是否可用。 |
| `conversations` | `claude_session_id`、`claude_initialized` | 会话无法记录所选工具及工具原生会话 ID。 |
| `AgentRunner` | `Version`、`CheckUpdate`、`Update` 的注释和实现都是 Claude | 接口看似通用，实际工具专用。 |
| `startMessage` | 只按 `project.runner` 找 Claude/SSH Claude 实现 | 无法按 Conversation 选择 CLI。 |
| `usage.go` | 只解析 Claude `system`、`assistant`、`result` 的字段 | Codex 事件不能复用，错误统计会误导用户。 |
| `App.tsx` | 对话、版本、空状态、错误、消息头像和权限文案均写死 Claude | 即使后端执行 Codex，页面仍会错误标注为 Claude。 |
| SSH | `sshRunner` 远端命令固定为 `claude` | 远端 Codex 不能仅靠本地 Adapter 补齐。 |

特别注意：`runnerRegistry` 的 ID 是 `wsl-local`、`ssh-...`，它描述执行地点，不应被用于表达 `claude-code` 或 `codex`。两者是正交维度。

## 3. Codex CLI 能力核实

### 3.1 可用于首期的稳定命令面

本机 `codex exec --help` 已确认：

```text
codex exec --json -C <project-dir> --sandbox <mode> <prompt>
codex exec resume --json <session-id> <prompt>
```

- `--json` 输出 JSONL，适合后端逐行读取、持久化和经 WebSocket 推送。
- `exec resume` 接受先前的 Conversation/session UUID，因此控制服务可在每次 Run 后退出进程，后续 Run 恢复同一个 Codex 会话。本机 0.144.6 的 `exec resume --help` 未列出 `-C`、`--sandbox` 或 `--color`；恢复轮不能重新传入这些设置。实测表明恢复进程的当前工作目录由子进程 `Dir` 决定，且可通过 `-c sandbox_mode="read-only"` 重申沙箱策略。因此 Adapter 必须每轮同时设置 `cmd.Dir` 与 `sandbox_mode`，不能依赖首轮配置继承。
- `--sandbox` 支持 `read-only`、`workspace-write`、`danger-full-access`。
- `-C` 可明确指定项目目录；实现必须使用 `exec.Command` 传参数组，不可拼接 shell 字符串。
- `--ephemeral` 会禁用本地会话持久化，不能用于需要恢复的平台会话。

这与 Claude 的持久 stdin/stdout `stream-json` 进程不同：Codex 首期应是“每个逻辑 Run 一个 CLI 进程 + 持久化原生 session ID”，而不是强行塞进 `StreamingAgentRunner`。

### 3.2 App Server 的能力与限制

本机 `codex app-server` 标记为 experimental，schema v2 已验证存在下列协议对象：

- `thread/start`、`thread/resume` 和 `turn/start`；
- `thread/started`、`item/agentMessage/delta`、`item/started`、`item/completed`、`turn/completed`；
- `commandExecution/requestApproval`、`applyPatch/requestApproval` 等审批请求与对应决策；
- 线程级 `approvalPolicy`、`sandbox`、`cwd`、`model`。

这条路径理论上最接近 Claude 当前的“常驻会话 + 网页审批”体验。但其风险也明确：协议是 experimental，schema 包含 v1/v2 和不稳定字段，且后端需要实现 JSON-RPC 双向请求、重连、版本协商和审批决策回传。本机诊断仅说明当前 provider 未启用 Responses WebSocket；App Server 默认可使用 stdio，这一结果不能单独证明 App Server 可用或不可用，仍必须完成端到端 POC。

因此不得在首期以 App Server 取代现有 Claude 流式协议，也不得在没有 POC 的情况下对用户承诺 Codex 的逐命令网页审批。

## 4. 方案比较与推荐

| 方案 | 实现 | 会话恢复 | 网页逐命令审批 | 风险 | 结论 |
| --- | --- | --- | --- | --- | --- |
| A. `codex exec --json` | 每个 Run 启动一个进程，保存 session ID，后续 `exec resume` | 本机 0.144.6 已验证 | 不作为首期能力 | 低 | **本地首期采用** |
| B. App Server | 常驻 JSON-RPC，线程和回合长期存在 | 有 | 协议支持 | 高，且 experimental/provider 依赖 | 二期 POC 后再决定 |
| C. 调用 OpenAI API 自建 Agent | 控制服务直接管理 Responses | 可自行设计 | 可自行设计 | 偏离“接入本机 Codex CLI”，丢失本地 Codex 配置、技能和规则 | 不采用 |
| D. 前端仅切换文案 | 仍由 Claude 执行 | 无 | 无 | 功能不真实 | 不采用 |

推荐 A：它使用已安装 CLI 的公开非交互入口，保留 Codex 的认证、项目 `AGENTS.md`、配置、技能和会话存储；同时能复用现有 Run、取消、工作区租约、WebSocket 和审计模型。

## 5. 推荐目标架构

```text
Project
  -> Runtime Runner（wsl-local / ssh-...，决定在哪里执行）
  -> Conversation.agent_id（claude-code / codex，创建后不可修改）
  -> Agent Adapter（决定如何启动 CLI、解析事件、恢复会话）
  -> Run（一次用户输入和一次实际 CLI 执行）
```

建议将现有边界调整为以下概念，而不是继续扩展 Claude 命名的接口：

```go
type AgentID string

type AgentAdapter interface {
    ID() AgentID
    Capabilities() AgentCapabilities
    Probe(context.Context, Runtime) AgentStatus
    StartRun(context.Context, AgentRunRequest, AgentRunSink) error
}

type Runtime interface {
    ID() string
    Exec(context.Context, CommandSpec) (Process, error)
    // 本地和 SSH 的参数传递、进程组终止在 Runtime 内处理。
}
```

`AgentAdapter` 负责 CLI 参数、会话恢复、事件标准化和工具专属错误；`Runtime` 负责本地或 SSH 进程。Claude Adapter 可以继续在本地 Runtime 内维护现有持久 session；Codex Adapter 首期每 Run 启动独立进程。二者均向统一 `AgentRunSink` 发出平台事件。

不要让 Codex 继承 `claudeCLIRunner`，也不要让 `sshRunner` 同时既是 SSH 客户端又是唯一的 AI 工具实现。否则接入第三个 CLI 时会再次复制全部本地和远端代码。

## 6. 数据模型和迁移

### 6.1 Conversation 是工具选择的归属

工具必须属于 Conversation，而不是 Project 或 Run：同一项目可保留 Claude 与 Codex 的历史会话；当前模型的 `is_current` 仍保证同一项目只有一个当前会话。一个会话中的上下文不能跨工具恢复；每个 Run 再快照工具信息以保证审计。

建议使用增量迁移，避免 SQLite 中直接破坏既有数据：

```sql
alter table conversations add column agent_id text not null default 'claude-code';
alter table conversations add column agent_session_id text not null default '';
alter table conversations add column agent_initialized integer not null default 0;
alter table conversations add column agent_runtime_id text not null default '';
alter table conversations add column execution_policy text not null default 'approval_required';
alter table runs add column agent_id text not null default 'claude-code';
alter table runs add column agent_runtime_id text not null default '';
alter table runs add column execution_policy text not null default 'approval_required';
alter table runs add column agent_run_id text not null default '';

update conversations
set agent_session_id = claude_session_id,
    agent_initialized = claude_initialized,
    agent_runtime_id = (select runner from projects where projects.id = conversations.project_id),
    execution_policy = permission_mode
where agent_id = 'claude-code';

update runs
set agent_runtime_id = (
        select p.runner
        from conversations c join projects p on p.id = c.project_id
        where c.id = runs.conversation_id
    ),
    execution_policy = (
        select c.permission_mode
        from conversations c
        where c.id = runs.conversation_id
    )
where agent_id = 'claude-code';

create unique index if not exists conversations_agent_session_unique
on conversations(agent_runtime_id, agent_id, agent_session_id)
where agent_session_id <> '';
```

`agent_runtime_id` 是原生 session 的作用域，避免同一 UUID 在不同 Runtime（尤其是不同 SSH 主机）中被错误恢复。每个 Run 写入 `agent_id`、`agent_runtime_id` 和 `execution_policy` 快照，保证之后即使项目配置变化也能审计真实执行条件。

当前 `migrate()` 会在服务每次启动时执行，不能把上述 `ALTER TABLE` 直接塞进已有的 `create table if not exists` 字符串。必须增加有版本号的迁移记录表，或使用 `PRAGMA table_info` 实现幂等的 `ensureColumn`；每个版本仅成功执行一次，并把新增列、回填和索引放在同一迁移版本内。否则第二次启动会因重复列名失败。

在一个完整版本内保留 `claude_session_id`、`claude_initialized` 和 `permission_mode` 作为兼容列；所有读取逐步切到通用列；完成数据校验和回滚窗口后再删除旧列。所有 SQL 查询、测试夹具和 JSON 类型必须同步替换，不能只改建表语句。

`projects.claude_ready` 也应保留为历史兼容字段，但不得再决定项目是否可加载。项目目录可被加载；页面由 Runtime 返回每种工具的动态状态，例如：

```json
{
  "runtime": "wsl-local",
  "agents": [
    {"id": "claude-code", "status": "ready", "version": "..."},
    {"id": "codex", "status": "ready", "version": "0.144.6"}
  ]
}
```

### 6.2 事件与用量

保留原始工具事件是正确的，但页面不能再直接把 `type=assistant` 解释为 Claude 结构。建议每条事件新增或构造以下平台信封：

```text
agent_id            claude-code | codex
adapter_version     解析器版本
kind                assistant_text | tool_started | tool_finished |
                    approval_requested | run_completed | diagnostic
raw_type            CLI 原始事件名
raw_payload         CLI 原始 JSON
```

前端按 `kind` 绘制时间线，详情面板再按 `agent_id + raw_type` 展示工具特有字段。历史 Claude 事件在读出时可由兼容映射转换，避免一次性重写全部记录。

`run_usage` 当前仅有 Claude 解析器，不能套用给 Codex。首期 Codex 只写可以确定的数据：执行时长、开始/结束、模型名（若事件提供）、工具调用数和会话 ID；Token、上下文窗口和费用没有可靠字段时必须显示“该工具未提供”，不得显示 0 或复用 Claude 估算公式。为此建议将 `usage_source`、`availability` 加入响应，或建立按 Agent 的 `UsageNormalizer`。

## 7. Codex 首期执行设计

### 7.1 运行命令

新会话第一条消息：

```text
codex exec --json --color never -C <projectPath> --sandbox workspace-write <prompt>
```

后续消息：

```text
codex exec resume --json <agentSessionID> <prompt>
```

实际实现采用参数数组，不经过 shell；本地命令用已有进程组终止机制，SSH 使用安全的参数转义或扩展 SSH 执行层的 argv 协议。进程取消后标记 Run 为 `stopped`/`interrupted`，绝不把“进程退出”自动当成成功。

Codex JSONL 解析器必须先完成 POC 固化 fixture，再写生产映射：

1. 捕获首轮和恢复轮的完整脱敏 JSONL，并验证恢复轮仍在首轮项目目录和沙箱策略下执行；
2. 从 POC 确认的 `exec --json` 初始化事件取得并持久化 Codex 原生 session/thread ID；不得把 App Server 的 `thread/started` 事件名直接用于 `exec --json` 解析器；
3. 映射助理文本、命令、文件变更、错误和完成状态为平台 `kind`；
4. 以 fixture 覆盖无工具调用、工具调用、停止、鉴权失败、模型失败和恢复失败；
5. 未认识的 JSONL 行仍作为 `diagnostic` 原始事件保存，不因解析失败丢失整轮记录。

这一步很重要：CLI 帮助确认 JSONL 能力，生成 schema 仅确认 App Server 的事件类型；`exec --json` 的实际事件包、session ID 字段、恢复后的工作目录和沙箱行为必须以当前安装版本的真实 fixture 为准。任何一项无法验证时，不能把 `exec resume` 作为生产会话恢复机制。

### 7.2 首期权限产品边界

Claude 当前的 `approval_required` 依赖 Bash Hook 回调控制服务。`codex exec` 的非交互命令面没有为网页提供可回复的审批通道，因此首期 Codex 会话只提供明确且真实的策略：

| 平台策略 | Codex 首期映射 | 页面文案 |
| --- | --- | --- |
| `read_only` | `--sandbox read-only` | 仅分析，不修改项目。 |
| `workspace_write` | `--sandbox workspace-write` | 可在当前项目范围内读写和执行。 |
| `approval_required` | 不可选 | 需要逐项网页审批时请使用 Claude Code。 |
| `full_control` | 不纳入首期 | 需完成安全 POC 和二次确认设计后才开放。 |

不得把 `workspace_write` 叫作“命令需确认”，也不得把 Claude 的 `full_control` 文案直接复用到 Codex。若业务必须支持逐条命令/补丁网页审批，应实施第 10 节的 App Server POC，而不是绕过沙箱或伪造确认。

Codex 的 `execution_policy` 可在空闲时修改：`exec resume` 没有 `--sandbox` 参数，但 Adapter 必须每轮以 `-c sandbox_mode="..."` 显式重申 Conversation 的当前策略，并将子进程 `Dir` 固定为项目目录。这个行为已在本机 0.144.6 的只读 POC 中验证。页面仍需在 Run 执行中禁用策略切换；Claude 保留现有空闲时切换权限的行为；二者由 Agent 能力声明分别控制。

### 7.3 会话、队列与停止

- 同一 Codex Conversation 同时只允许一个 Run。`exec resume` 是独立进程，不能复用 Claude 当前的长连接队列语义。
- 首期收到后续输入时返回“当前 Codex 任务执行中，请等待或停止后再发送”；后续可增加平台级队列，但每条队列任务必须在前一 Run 完成后才启动。
- 新建、切换和恢复会话时按 `agent_id` 隔离。Claude session ID 绝不可传给 Codex，反之亦然。
- 控制服务重启后运行中的 Codex 进程按现有恢复逻辑标记中断；已保存 session ID 的已完成 Conversation 可继续恢复。

## 8. API 与前端改造

### 8.1 API

`POST /api/projects/{projectID}/conversations` 扩展为：

```json
{
  "agentId": "codex",
  "executionPolicy": "workspace_write"
}
```

服务端必须验证：该 Agent 已注册、目标 Runtime 上已就绪、策略被该 Agent 支持、当前项目没有互斥 Run。不能信任前端传入的 CLI 路径、模型或任意 config 覆盖。

下列接口也改为通用语义：

- `/api/runners` 返回 `agents[]`，替换只含 `claude` 的状态；
- 工具版本接口改为 `/api/runners/{runnerID}/agents/{agentID}/...`；Codex 更新检查仅在 CLI 明确支持可靠检查时提供；
- Conversation、Run、Event、Usage 响应均包含 `agentId`；
- `POST /permission-mode` 改名或兼容为 `execution-policy`。旧 Claude 路由可在迁移期保留适配；
- WebSocket 外层事件包含 `agentId`、平台 `kind` 和原始事件信息。

### 8.2 页面

新建会话弹窗先选择工具，再显示该工具支持的策略。不可用工具保持可见但禁用，并显示“未安装”“未登录”“该 SSH Runtime 未安装”等具体原因。

其他必要修改：

- 项目加载校验展示 Claude Code 与 Codex 的独立状态；不再因 Claude 不可用阻止加载项目；
- 当前会话顶部、消息作者、空状态、历史会话和错误卡显示实际工具名称；
- 历史会话列表加工具徽标，可按工具筛选；
- 使用状态面板改为中立文案，按 Agent 的数据能力显示或隐藏 Token/费用；
- 版本管理从“Claude Code 版本”抽成 Runtime 的“已安装工具”；
- 快捷任务和任务编排仍只发送普通提示词，因此可复用，但其说明不得承诺 Claude 特有的 Bash Hook 审批；`TaskQueue`、`TaskBoard` 和相关 TypeScript props 当前将权限写死为 `approval_required | full_control`，必须改为从 Conversation 的 Agent capabilities 获取策略标签和是否可修改，不能只改说明文案。

工具一经创建不得在 Conversation 内切换。用户想换工具时创建新 Conversation；这样历史、权限、原生 session 和用量的归属始终明确。

## 9. 配置、安全与 SSH

建议新增以下配置，不读取或保存 API Key：

| 配置 | 默认 | 作用 |
| --- | --- | --- |
| `AUTO_ENABLED_AGENTS` | `claude-code` | 控制页面可选择的工具集合；完成阶段 0 后才显式加入 `codex`。 |
| `AUTO_CODEX_PATH` | `codex` | 本地 Codex 二进制。 |
| `AUTO_CODEX_DEFAULT_SANDBOX` | `workspace-write` | 新 Codex 会话默认策略。 |
| `AUTO_CODEX_MODEL` | 空 | 可选固定模型；为空时使用用户 Codex 配置。 |

认证状态只调用 `codex login status` 或 `codex doctor --json` 的脱敏结果；日志、事件、数据库和 HTTP 响应不得写入 API Key、`CODEX_HOME/auth.json` 内容或完整环境变量。

SSH 是第二阶段：远端必须自行安装、登录和配置 Codex；本地认证不会自动传到远端。先把 `Runtime` 的探测结果改为每工具独立，再实现 SSH Codex 命令。不得把 `~/.codex`、认证文件或环境变量通过 SSH 复制过去。远程 App Server 还需要控制服务可达地址、加密传输和 token 生命周期设计，首期不做。

## 10. 实施顺序

### 阶段 0：兼容性 POC（已通过，本机 0.144.6）

- 在临时 Git 项目运行首轮 `codex exec --json` 和一次 `codex exec resume`；
- 保存脱敏 fixture，确认 `exec --json` 的 `thread.started.thread_id`、文本、工具、完成和错误事件；不得以 App Server schema 代替该证据；
- 已验证：恢复轮以进程 `Dir` 使用项目目录；通过 `-c sandbox_mode="read-only"` 重申策略后，写入被拒绝；
- 实现必须把这两个设置视为每轮必传条件，并在 CLI 升级时重新执行 POC；
- 验证本机 custom provider 能完成以上流程；失败时不进入功能开发。

### 阶段 1：通用化但不改变 Claude 行为

- 添加 `agent_id`、通用 session 字段和 Run 快照，回填既有 Claude 数据；
- 引入 Runtime/AgentAdapter 解析入口，让 Claude Adapter 通过它运行；
- 用回归测试证明既有 Claude 本地、SSH、审批、历史、任务、Git 租约仍可用。

### 阶段 2：本地 Codex MVP

- 实现 `codex_exec_adapter.go`、JSONL fixture 解析器、停止和错误映射；
- 只启用 WSL 本地 `read_only`、`workspace_write`；
- 新建会话中选择 Codex，支持首轮、恢复、停止、历史和原始事件查看；
- Codex 用量字段不能确定时返回不可用。

### 阶段 3：产品打磨与远端

- 完成无 Claude 项目的加载、工具状态、版本管理和通用文案；
- 在远端单独部署 Codex 后增加 SSH 支持及端到端测试；
- 按需要增加平台级 Codex Run 队列。

### 阶段 4：App Server POC

- 固定一个 Codex CLI 版本和 App Server schema；
- 实现 JSON-RPC initialize、thread、turn、取消、审批请求和决策回传；
- 断线/重连、服务重启、重复审批、策略升级、provider WebSocket 的矩阵测试全部通过后，再考虑开放 Codex 网页逐项审批。

每一阶段都应以 feature flag 发布，并支持关闭 Codex 而不影响已有 Claude Conversation。

## 11. 验收与测试矩阵

后端至少覆盖：

1. 旧数据库迁移后所有 Claude Conversation 都为 `agent_id=claude-code`，且能继续发送消息。
2. 创建 Codex Conversation 时保存 `agent_id=codex`，不接受未知工具或不支持的策略。
3. Codex 首轮 JSONL 保存 session ID 和 Runtime 作用域；第二轮确实调用 resume，且仍处于首轮目录和策略；两种失败都记录 Run 和事件。
4. 停止 Codex 后进程组被终止，工作区租约释放，后续 Run 可以执行。
5. Claude 与 Codex 会话 ID 不会互用；不同工具的历史、用量和文案不串联。
6. Claude 既有 Hook 审批和 SSH 流式会话测试继续通过。
7. Codex 不提供的 Token/费用字段明确为不可用，绝不以 Claude 结构解析。
8. `codex login status`、stderr、原始事件和错误响应不泄漏认证信息。
9. Codex 首轮完成后，修改执行策略被拒绝；新建另一会话后可使用新策略，原会话不受影响。

前端至少覆盖：

1. 新会话可选择 Claude Code 或已就绪的 Codex，选择后只显示对应策略。
2. Codex `approval_required` 不可选，并给出准确说明。
3. 历史、消息、空状态、运行状态和使用面板显示实际 Agent。
4. 工具未安装、未认证、运行中、停止和恢复失败都有可理解的状态。
5. 窄屏下工具选择与策略选项不重叠、不截断。

## 12. 本期不做的事项

- 不把 Codex App Server experimental 协议直接作为生产主链路；
- 不在平台内安装、登录、复制或托管 Codex 认证凭据；
- 不承诺 Codex 首期支持 Claude 式 Bash Hook 审批；
- 不跨 Claude/Codex 合并上下文、Token、费用或原生 session；
- 不让单个 Conversation 中途切换工具；
- 不在没有远端显式安装和认证的情况下自动把 Codex 带到 SSH 机器。

## 13. 当前实施状态

当前代码已交付本地 WSL Codex MVP：创建会话时可选择 Claude Code 或 Codex，Codex 支持 `read_only` 与 `workspace_write`、首轮 `exec`、后续 `exec resume`、停止、历史会话恢复，以及命令和文件变更事件展示。

`conversations` 现保存 `agent_id`、`agent_session_id`、`agent_initialized`、`agent_runtime_id`、`execution_policy`；`runs` 保存 Agent、Runtime、策略和原生 run 标识快照。旧的 Claude 专用列仍为兼容字段，所有新运行使用通用字段作为实际恢复和审计来源。

SSH Codex、通用 AgentAdapter/Runtime 的完全解耦、Codex 细粒度网页审批和 App Server 仍属于后续阶段，不能作为本次交付能力承诺。

### 最终建议

先完成“环境与工具分离”的小范围重构，再以 `codex exec --json + exec resume` 交付本地 Codex MVP。阶段 0 已证明该路径可控，但 Adapter 必须为每个 Run 设置项目工作目录和 `sandbox_mode`；CLI 升级后重新执行 POC。这样用户创建会话时即可真实选择 Claude Code 或 Codex，同时不牺牲已有 Claude 稳定性，也不把 experimental App Server 和未验证的审批行为带入主流程。

当阶段 0 的 fixture 和阶段 2 的端到端测试稳定后，再决定是否投入 App Server，以换取 Codex 的常驻会话、细粒度审批和更丰富的实时事件。
