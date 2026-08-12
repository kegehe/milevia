# Git 仓库管理与协作

> 日期：2026-07-20  
> 阶段目标：让已加载的 Git 项目可在页面中安全地查看仓库状态与历史，并完成受控的暂存、提交、同步、推送和分支切换；为 AI 任务、隔离 worktree 与后续远程协作保留清晰边界。

## 1. 结论

这个需求不应理解为“把 `git` 命令做成按钮”。平台同时运行 AI 开发任务、保存任务验收记录，并管理多个本地项目；Git 操作会直接改变工作目录、暂存区、提交历史和远端分支。因此它需要一个独立的仓库工作台，以及由 Runner 执行、服务端裁决、全程可审计的 Git 领域能力。

建议分三期实施：

| 阶段 | 交付目标 | 包含 | 明确不包含 |
| --- | --- | --- | --- |
| 第一阶段：仓库可见与安全写入 | 单人本地闭环 | 状态、提交历史、差异、暂存/取消暂存、提交、fetch、推送、创建/切换本地分支 | pull/merge/rebase、删除分支、强推、改远端、凭据录入、worktree |
| 第二阶段：同步与隔离 | 减少主工作目录冲突 | 拉取预览和显式 fast-forward 更新、远端分支、分支发布、任务 worktree、变更审阅联动 | 自动合并、自动推送、自动清理工作目录 |
| 第三阶段：协作集成 | 对接托管平台与多人 | GitHub/GitLab PR、检查状态、受保护分支策略、身份和权限、组织审计 | 以网页令牌替代 Runner 身份、绕过组织规则 |

首期应调用 Runner 环境中的原生 Git CLI，而非在浏览器中实现 Git，也不建议用 Go Git 库替换 Git。原生 CLI 与现有仓库配置、SSH、credential helper、hooks、LFS、submodule、worktree 的兼容性最好；平台只解析稳定的机器可读输出，例如 `git status --porcelain=v2 -z` 和 `git log` 的自定义分隔格式。

## 2. 现状与缺口

现有架构已经为本需求提供了正确的安全边界：浏览器不直接访问本机路径，路径经过 `AUTO_ALLOWED_ROOT` 限制，控制服务在本机 SQLite 中保存项目和运行记录，Claude 在项目目录中执行。当前项目登记时只调用一次 `git rev-parse --abbrev-ref HEAD`，把结果存入 `projects.git_branch`；页面仅在项目卡片显示这个历史快照。

这意味着以下问题必须在本功能中补齐：

- 分支快照会过期，且 detached HEAD、未出生分支、已删除分支不能正确表达；
- 没有工作树、暂存区、上游分支、ahead/behind、远端或冲突状态；
- 没有提交历史、文件差异和提交前检查证据；
- 没有互斥机制。Git 操作若与 Claude 修改、任务 Run 或另一次 Git 操作同时发生，结果不可预测；
- 没有 Git 操作审计、失败分类和凭据边界。

因此 `Project.git_branch` 不再是仓库真相。它可在迁移期保留为项目卡片摘要，但必须由实时仓库快照刷新；详情页只使用 Git 服务读取到的当前状态。

## 3. 产品边界与术语

### 3.1 支持的仓库

首期只支持已加载目录自身是**非 bare Git 工作树**的项目。嵌套仓库、submodule、Git LFS、linked worktree 可以被检测并提示，但不作为首期可写对象。非 Git 项目仍可用于 Claude 与任务管理，只隐藏 Git 工作台，并提供“初始化 Git 仓库”作为以后可选能力，不在首期自动初始化。

| 对象 | 含义 | 首期策略 |
| --- | --- | --- |
| 工作树 | 磁盘上的实际项目文件。 | 读取、暂存和提交均针对项目根目录。 |
| 暂存区（index） | 下一次提交的精确内容。 | 显示 staged/unstaged 两份差异，用户逐文件暂存。 |
| HEAD | 当前检出的提交或分支。 | 支持分支 HEAD；detached HEAD 只读并提示先创建分支。 |
| upstream | 当前分支跟踪的远端分支。 | 显示并计算 ahead/behind；首期创建分支时可选设置。 |
| 远端 | `origin` 等远端配置。 | 读取；push 时选择已有远端和已存在/新建同名远端分支。 |
| 冲突/合并状态 | 未完成的 merge、rebase、cherry-pick 等。 | 全部写操作锁定，提供诊断，不在首期网页内解决。 |

### 3.2 首期用户能完成的工作

1. 进入项目后查看当前分支、上游、同步差距、工作区摘要、最近提交和远端。
2. 在“变更”页查看未提交文件，分别查看工作树相对暂存区、暂存区相对 HEAD 的差异；逐文件或全部暂存、取消暂存。
3. 填写提交信息、检查将要提交的文件、点击确认后创建本地提交；提交结果进入审计和任务历史。
4. 点击“获取更新”执行 `fetch --prune`，刷新远端信息和 ahead/behind；点击“推送”将当前分支按正常 fast-forward 规则推到指定远端。
5. 在分支抽屉创建本地分支、切换到干净工作树中的分支；可选择从当前 HEAD 或已存在的本地/远端引用创建。

下列操作必须在后续阶段另行设计：`pull`、merge、rebase、reset、cherry-pick、tag、删除或重命名分支、修改远端 URL、强制推送，以及任何会覆写本地或远端历史的命令。它们不是缺少按钮，而是需要冲突处理、预览和恢复机制。

> **更新**：初版将 `restore`、`clean` 列为"后续阶段"。实际 `git.go` 已实现 `RestoreWorktree`（恢复单文件）、`RestoreAll`（恢复全部）、`DiscardInitialChanges`（丢弃未暂存初始改动）、`RemoveUntracked`（清理未跟踪文件）四个操作，对应前端 stage-all / unstage-all / discard 路由。`pull`、merge、rebase、reset 等仍属后续阶段。

## 4. 方案比较与推荐

| 方案 | 优点 | 风险/缺点 | 结论 |
| --- | --- | --- | --- |
| 浏览器 Git（WASM/HTTP 直连托管平台） | UI 似乎直接，部分读取无需后端。 | 不能安全访问 WSL 文件、无法复用 SSH/credential helper、会泄露令牌边界、对 LFS/hooks/worktree 支持差。 | 不采用。 |
| Control Server 直接执行 Git | 代码少，单机演示快。 | 违反既有“项目文件仅由 Runner 接触”的目标架构；未来远程 Runner 时必须重写。 | 不采用。 |
| Runner 原生 Git CLI + Git 服务模块 | 复用真实 Git 行为和本机认证；与 Runner、审计、任务和远程环境兼容。 | 需设计命令白名单、解析器、锁和错误映射。 | 采用。 |
| 使用 `go-git` 读写仓库 | Go 内调用便捷。 | 与本机 Git 配置、认证、hooks、LFS、子模块和高级工作树特性兼容风险较高。 | 不作为主实现。 |

推荐的逻辑链路如下。第一阶段可把 Git Runner 作为 `control-server` 内与 Claude Runner 平行的本地适配器实现；接口必须独立，之后再迁移到独立 Runner，不改变浏览器 API。

```text
Web Git 工作台
  -> Control Server：身份、项目归属、操作策略、审计、WebSocket 事件
    -> Git Runner Adapter：校验项目路径、获取仓库锁、调用固定 Git argv
      -> 本机 Git / SSH agent / credential helper / 远端仓库
```

所有 Git argv 固定由服务端构造，浏览器只传递枚举、项目 ID、已校验的相对路径、引用名和提交信息。禁止传入完整 shell、`-c` 配置项、任意 `git` 子命令、绝对路径或环境变量。

## 5. 领域模型、快照与审计

Git 的磁盘状态是外部可变状态，不能只靠数据库保存。数据库记录“项目设置、操作意图和历史”，Git Runner 每次读取或写入前都必须以仓库实际状态为准。

```text
Project
  -> GitRepository（项目当前仓库能力与策略）
       -> GitSnapshot（短时缓存、可丢弃的真实状态投影）
       -> GitOperation（一次用户触发的执行和审计事实）
  -> Task -> TaskRun -> Run（既有 AI 执行链路）
```

### 5.1 `GitSnapshot`

`GET /api/projects/{projectID}/git/summary` 返回下列信息，且响应包含 `observedAt` 与 `stateToken`：

| 字段 | 说明 |
| --- | --- |
| `repositoryState` | `ready`、`not_repository`、`detached`、`conflicted`、`operation_in_progress`、`unavailable`。 |
| `head` | `headOid`、`branch`、`isDetached`、`upstream`、`ahead`、`behind`。 |
| `worktree` | staged、modified、untracked、deleted、renamed、conflicted 的文件计数。 |
| `remotes` | 名称、fetch URL、push URL 的脱敏展示；不返回嵌入 URL 的密码或 token。 |
| `capabilities` | 是否可提交、可切换、可推送及不可用原因。 |
| `stateToken` | 服务端签发的短时、一次性快照令牌；绑定项目、规范化仓库状态摘要与过期时间。读取时间只用于过期，不参与状态比较。 |

状态读取使用 `git status --porcelain=v2 --branch -z`，而不是解析面向人类的 `git status` 文本。零字符分隔可以正确处理换行、空格和非 ASCII 文件名。提交日志的字段和记录同样使用 NUL 分隔，并限制条数、输出字节数和单次执行时间。

`stateToken` 的服务端记录至少保存：项目 ID、HEAD OID、规范化的 porcelain v2 状态、index 校验值，以及快照中每个可暂存目标的文件指纹。写操作取得工作区租约后重新读取对应状态：提交比较 index；暂存/取消暂存比较目标文件指纹和 index；切换分支比较工作树与 index 均干净。任一比较不一致或令牌过期均返回 `409 state_changed`，要求页面刷新。令牌不能由客户端自行构造，也不能以时间戳散列代替状态比较。

### 5.2 `GitOperation`

每一次写入、fetch 和 push 都先创建 `queued` 操作记录，再在工作区租约内执行并写成 `running`、`succeeded`、`failed`、`cancelled` 或 `needs_attention`。记录应包括：

- 操作类型、项目 ID、发起者（首期为 `local_user`）、请求摘要、前置 `stateToken`；
- 开始/结束时间、执行前后 HEAD 与分支、远端、结果摘要、可恢复错误码；
- 关联的 Task、TaskRun、Run（有明确关联时）；
- 经脱敏和长度限制后的 stderr/stdout；绝不记录 HTTP Basic token、SSH 私钥、passphrase 或 credential helper 输出。

建议 SQLite 迁移如下；`git_repository_settings` 只记录策略，状态本身不永久缓存为真相。

> **实现说明**：`git_operations` 表已落地（字段与下方一致，含 `executor_id`、`lease_until`）。`git_repository_settings` 表**尚未实现**——`default_remote`、`protected_branch_patterns`、`require_clean_switch` 等策略字段当前无对应存储，受保护分支模式、默认远端配置等功能待后续补充。下方保留该表设计作参考。

```text
git_repository_settings   # ⚠️ 未实现，保留设计参考
  project_id                 text primary key references projects(id)
  enabled                    integer not null default 1
  default_remote             text
  protected_branch_patterns  text not null default '[]'
  require_clean_switch       integer not null default 1
  created_at, updated_at     datetime not null

git_operations            # ✅ 已实现
  id, project_id             text
  task_id, task_run_id       text null
  type, status               text
  request_summary            text not null
  before_state, after_state  text not null default '{}'
  error_code, error_message  text not null default ''
  output_excerpt             text not null default ''
  executor_id                text
  lease_until                datetime
  requested_at, started_at, finished_at datetime

create index git_operations_project_requested
  on git_operations(project_id, requested_at desc);
```

`GitOperation` 是审计事实，不能因为用户刷新页面而删除，也不能仅依赖 WebSocket 通知判断成功。执行器领取操作时以事务写入 `executor_id` 与有界租约；正常结束时在同一事务记录最终状态、前后快照和 `finished_at`。服务重启后，未终结操作不得假定失败或成功：本地提交可在 HEAD、index 和记录的前置快照可证明时补记结果；fetch 可重读本地远端引用；任何结果不唯一的 push 一律标为 `needs_attention`，要求用户执行一次新的 fetch 后再决定。恢复逻辑绝不自动重试 push、提交或分支切换。

## 6. 并发、任务与工作区规则

### 6.1 项目工作区租约

每个 `projectID` 有一个由 Run 调度和 Git 执行器共同使用的 `ProjectWorkspaceLease`，而不是只在 Git 模块内部维护一把锁。首期单机的实现可以是进程内互斥锁加操作记录租约；在多个 Control Server 或远程 Runner 场景，必须改为 Runner 可见的租约协议。所有 Claude Run 在入队前登记为工作区写入者，在真正启动前取得租约；所有会改变 Git 元数据或工作树的 Git 操作也必须取得同一租约。读型查询可并行，但命令结束后立即丢弃结果，不能跨操作复用。

```text
Claude Run / 提交 / 暂存 / 切换分支 / fetch / push
  -> 通过同一 ProjectWorkspaceLease 准入
  -> 取得项目工作区租约
  -> 再读取真实状态并校验 stateToken、无冲突、无不兼容运行
  -> 创建/更新 GitOperation
  -> 执行固定 argv 的 Git 命令
  -> 重新读取 GitSnapshot，提交事务，推送实时事件
  -> 释放锁
```

租约不能只存在于前端，也不能只在 Git HTTP handler 中检查 AI 是否“当前运行”。所有 Git 写接口与 AI Run 的准入都在租约内二次验证，避免多标签页、排队 Run、任务运行和终端手工操作造成的 TOCTOU 问题。Git 自身的 `.git/index.lock` 仍可能表示另一进程正在操作仓库，平台将其映射为“仓库正被外部操作占用”，不删除该锁文件。

### 6.2 与 Claude / Task 的关系

- Git 工作台不是新的 Claude Run，不进入 Claude 会话；它是由用户明确点击触发的独立 Runner 操作。
- 当项目有运行中或已准入等待的 Claude Run，首期拒绝所有会写 Git 元数据或工作树的 Git 操作，包括暂存、提交、切换分支、fetch 与 push。允许读取状态和日志；页面应显示正在占用工作区的 Run，并提供进入该 Run 的入口。
- 推送只改变远端引用，不改变工作树，但首期同样要求 AI Run 结束，以避免“AI 正在修改，用户误以为推送包含最终版本”的认知冲突，并保持单一工作区租约规则。
- Task 的“验收”不等于 Git 提交。一个提交可关联多个已验收任务，一个任务也可能经历多次提交。首期提交确认层允许勾选待验收/已完成任务作为**关联元数据**，不自动改变任务状态。
- 第二阶段若启用任务 worktree，则每个 TaskRun 使用自己的工作树与分支；工作台必须明确显示正在查看的是项目主工作树还是某个任务工作树。不同工作树仍共享同一 Git 对象库，分支引用操作需要仓库级协调。

### 6.3 分支切换门禁

切换前必须检查：无运行中的 AI、无进行中的 Git 操作、无 merge/rebase/cherry-pick、工作树和暂存区均干净、目标分支存在且不是当前分支。否则拒绝并返回具体文件/状态，不提供“强制切换”按钮。若用户需要保留未提交改动，首期应先提交或在终端自行 stash；stash 工作流在第二阶段作为独立设计项。

## 7. 首期页面与交互

项目标题栏将当前静态分支文本升级为可点击的 Git 状态入口：`分支名 · 3 个改动 · ↑2 ↓1`。非 Git 项目显示“非 Git 项目”，不展示空的 Git 页面。入口打开项目内的 Git 工作台，不离开对话与任务上下文。

```text
项目头：项目名 | Git: feature/login · 3 改动 · ↑2 | 任务 | 对话设置
-----------------------------------------------------------------
概览   变更   历史   分支   操作记录

概览：当前分支、上游、ahead/behind、远端、最近提交、待处理变更
变更：文件列表 -> 工作树 diff / 暂存 diff -> 暂存、取消暂存、提交
历史：提交图的线性首期列表 -> 提交详情、文件列表、提交 diff
分支：当前、本地、远端分支 -> 创建、切换（切换需满足门禁）
操作记录：fetch、push、提交、切换等结果、失败原因和关联任务
```

### 7.1 变更与提交

- 文件列表按 `已暂存`、`未暂存`、`未跟踪`、`冲突` 分组，显示文件状态和路径；路径只以项目根目录为基准。
- 点击文件默认显示未暂存 diff；已暂存文件显示暂存 diff；二者都有时通过页签区分，避免把“将提交的内容”与“仍在工作树的内容”混淆。
- 首期只支持逐文件和全部暂存。按 hunk/行暂存必须等到稳定 diff 解析、换行符和二进制文件策略明确后再做。
- 二进制、超过大小上限、子模块指针或不可读取文件显示元信息和提示，不在网页渲染全文。
- 提交面板只展示已暂存文件；提交信息必填，建议限制首行 72 字符、全文 4,000 字符。点击“提交”前显示分支、文件数、消息和关联任务，确认后提交。Git hooks 的输出按操作日志回传；hook 拒绝是正常失败而非平台异常。

### 7.2 历史、同步与分支

- 历史首期显示当前 HEAD 可达的最近 100 条提交：短 SHA、标题、作者、作者时间、父提交数；提交详情再按需加载 diff，禁止一次读取整个大仓库历史。
- “获取更新”只执行 `git fetch --prune <remote>`，不会改工作树；完成后重新计算 ahead/behind。它仍会更新 Git 元数据，因此与 Claude Run 和其他 Git 操作使用同一工作区租约。
- “推送”默认 `git push <remote> HEAD:<branch>`，仅允许普通推送，不添加 `--force`。无 upstream 时，确认层明确询问远端与分支名，并以 `-u` 建立 upstream；受保护分支按项目策略禁用。
- 分支列表以当前、本地、远端分组；创建和切换使用 `git switch`，不使用兼具恢复语义的 `checkout`。创建前服务端验证引用名，调用 `git check-ref-format --branch`。

### 7.3 实时更新与失败状态

新增 `GET /ws/projects/{projectID}` 项目级 WebSocket，推送带 `operationId`、`eventId`、`createdAt` 的 `git.operation.updated` 与 `git.snapshot.updated`。操作终态以 `git_operations` 的持久化记录为准；浏览器重连后先读取操作列表和最新摘要，再订阅新事件，不依赖 WebSocket 补齐历史。手工终端修改无法可靠被文件监听完全覆盖，因此首期额外提供刷新按钮和 10 秒低频摘要轮询；操作确认始终以服务端即时读取为准。

界面必须区分：身份认证失败、远端拒绝、非 fast-forward、网络失败、仓库锁、冲突/未完成操作、hooks 失败、没有用户身份、状态已经变化、用户取消。不得只显示原始 Git stderr，也不得把含敏感信息的错误原样输出。

## 8. API 与命令白名单

### 8.1 浏览器 API

```text
GET  /api/projects/{projectID}/git/summary
GET  /api/projects/{projectID}/git/changes
GET  /api/projects/{projectID}/git/diff?path=...&stage=worktree|index
POST /api/projects/{projectID}/git/stage            { paths, stateToken }
POST /api/projects/{projectID}/git/unstage          { paths, stateToken }
POST /api/projects/{projectID}/git/commits          { message, stateToken, taskIds? }
GET  /api/projects/{projectID}/git/log?ref=HEAD&cursor=&limit=50
GET  /api/projects/{projectID}/git/commits/{oid}
POST /api/projects/{projectID}/git/fetch            { remote, stateToken }
POST /api/projects/{projectID}/git/push             { remote, branch, setUpstream, stateToken }
GET  /api/projects/{projectID}/git/branches
POST /api/projects/{projectID}/git/branches         { name, startPoint, trackRemote? }
POST /api/projects/{projectID}/git/switch           { branch, stateToken }
GET  /api/projects/{projectID}/git/operations?cursor=&limit=50
GET  /api/git/operations/{operationID}
POST /api/projects/{projectID}/git/stage-all       # 后续新增
POST /api/projects/{projectID}/git/unstage-all     # 后续新增
POST /api/projects/{projectID}/git/discard         # 后续新增（restore/clean 语义）
GET  /ws/projects/{projectID}                      # ⚠️ 通用项目级 WS 未实现
```

> **实现说明**：通用的 `GET /ws/projects/{projectID}`（用于推送 `git.snapshot.updated` / `git.operation.updated`）**未实现**。当前实际 WebSocket 端点为：`/ws/conversations/{id}`、`/ws/projects/{id}/run`（项目启停日志）、`/ws/processes`、`/ws/notifications`。Git 操作的最终结果目前通过 `GET operation` 轮询或会话级 WS 获取，无独立的项目级 Git 实时推送。

所有写接口都返回 `202 Accepted` 与 `operationId`，由 `GET operation` 或 WebSocket 获得最终结果；短操作也遵循同一模型，便于 hooks、网络和未来远程 Runner。读取接口不可接受任意 revision 表达式：`ref`、`branch`、`startPoint` 必须来自服务端列出的引用，或通过严格的 SHA/分支名校验。

### 8.2 Runner 内可调用命令

| 用户意图 | 固定命令形态 | 备注 |
| --- | --- | --- |
| 状态 | `git -C <repo> status --porcelain=v2 --branch -z` | 稳定解析，包含 upstream 计数。 |
| 日志 | `git -C <repo> log --format=<fixed> -z --max-count=<bounded>` | 所有数字由服务端上限约束。 |
| 差异 | `git -C <repo> diff --no-ext-diff --no-textconv -- <validated paths>` | staged 使用 `--cached`；禁止 external diff 与 textconv。 |
| 暂存 | `git -C <repo> add -- <validated paths>` | 路径必须属于状态快照中的项目相对路径。 |
| 取消暂存 | `git -C <repo> restore --staged -- <validated paths>` | 不能传任意 revision。 |
| 提交 | `git -C <repo> commit -F <temporary message file>` | 消息经 stdin/临时受限文件提供，不能拼接进 shell。 |
| 获取/推送 | `git -C <repo> fetch --prune <remote>` / `git -C <repo> push [--set-upstream] <remote> HEAD:<branch>` | 禁止 force、force-with-lease 与任意 refspec。 |
| 分支 | `git -C <repo> switch -c <name> <start>` / `switch <name>` | 仅通过通过校验的引用。 |

所有命令采用 Go `exec.CommandContext` 直接传 argv，设置 `Cmd.Dir` 为已解析的项目根目录，使用超时、输出上限、受控环境变量和独立进程组；超时或取消时终止整个进程组，而不只终止 Git 父进程。Git Runner 设置 `GIT_TERMINAL_PROMPT=0`，并以受控 `GIT_ASKPASS` 拒绝交互式输入；凭据必须由 Runner 用户在页面外预先配置的 SSH agent 或 credential helper 提供。绝不通过 `sh -c`、字符串拼接或浏览器传来的命令文本执行。

## 9. 身份、凭据与安全策略

首期是本机单用户系统，Git 认证应复用 Runner 操作系统用户已配置的 SSH agent、SSH key、Git Credential Manager 或其他 credential helper。Web 页面既不读取私钥，也不接收、持久化、回显或写入 access token；身份缺失时操作快速失败并显示“请在 Runner 环境完成 Git 登录”，不能弹出或等待终端密码输入。

| 风险 | 策略 |
| --- | --- |
| 命令注入 | API 是领域动作，不提供通用 `exec`；Git 以 argv 调用，路径与引用白名单校验。 |
| 路径越界 | 仅从 `projectID` 解析服务端保存的路径，并再次验证它仍在 Runner allowed root 内。 |
| 凭据泄露 | 不返回进程环境、credential helper 输出、完整 URL 用户信息、私钥或 token；日志作模式脱敏和长度截断。 |
| 误推送 | 明确确认层、显示远端/目标分支/HEAD 摘要；默认禁用 protected branch；禁止 force。 |
| 工作树破坏 | 切换分支要求干净状态；不实现 reset/clean/强制 checkout；遇到冲突只读和诊断。 |
| AI 与人工互相覆盖 | 项目级互斥、操作前二次状态校验、所有写操作审计；后续用 worktree 隔离。 |
| Web 跨站请求 | 保持本机 Origin 校验；未来多用户部署再加 session/CSRF/角色授权与 API 速率限制。 |
| 恶意仓库配置 | 首期只允许本机用户信任的项目接入；读取差异禁用 external diff/textconv，网络协议限制为 `https`、`ssh`、`git`，拒绝 `ext`、`file` 与自定义 remote helper。commit hooks、`core.sshCommand` 等项目配置可执行代码，因此不得把首期当作不可信仓库沙箱。 |

在多用户或远程 Runner 阶段，凭据归属必须明确为 Runner 机器身份、用户身份或组织集成身份，三者不能混用。GitHub/GitLab 的 API 集成与 Git 网络认证也应分离：前者可使用最小权限 OAuth/GitHub App，后者仍由 Runner 的 Git/SSH/credential helper 完成。

## 10. 实施顺序

### 阶段 A：只读仓库洞察

1. 抽取现有 `gitBranch` 为 `GitRunner` 接口，完善仓库探测、Git 版本与 capability 诊断。
2. 实现 `GitSnapshot`、porcelain v2 解析器、日志/提交详情/diff 读取，以及受限输出和错误分类。
3. 增加 Git 摘要、变更、历史页面和刷新机制；让项目卡片分支信息由摘要更新。
4. 增加单元测试：含空格、中文、换行的路径，detached HEAD，无 upstream，未出生仓库，二进制文件，冲突状态与 Git 不可用；验证 diff 不执行 textconv，且不可信协议被拒绝。

### 阶段 B：受控写入

1. 引入每项目 Git 锁、`GitOperation` 迁移、操作状态机和 WebSocket 事件。
2. 实现逐文件/全部暂存、取消暂存、提交确认；在工作区租约内重新读取 `stateToken`，并覆盖文件在摘要读取后变更、AI Run 与 Git 操作竞争的测试。
3. 实现 fetch 与普通 push；对认证失败、hook 拒绝、非 fast-forward、远端保护规则、控制服务在 push 结果未知时重启写集成测试。
4. 实现创建/切换分支及严格的干净工作树门禁。

### 阶段 C：与任务闭环

1. 在 TaskDetail 和 GitOperation 间增加可选关联，展示“这次提交包含/关联哪些任务”。
2. 允许验收页跳到关联提交的 diff；不自动接受 Task。
3. 设计并实现任务 worktree：创建、运行绑定、状态显示、显式合并/清理策略。此阶段须重新评审并发和分支保护。

### 阶段 D：远程协作

实现 PR/MR、远端检查、评论、组织角色、受保护分支和集成身份。GitHub/GitLab 是可插拔 Integration，不应让其对象模型进入 Git Runner 的基础接口。

## 11. 验收与测试

首期完成需至少通过以下场景：

1. 加载非 Git、正常 Git、detached HEAD 与无远端项目，页面均给出准确状态且不误报可写。
2. 文件名包含空格、中文与换行时，状态、暂存和 diff 始终定位同一文件；路径不能逃逸项目根目录。
3. 用户修改两个文件，只暂存其中一个，提交面板与最终 commit 只包含暂存文件；提交 hooks 失败后工作树与操作记录可解释。
4. Claude Run 执行中，Git 写操作被拒绝且有明确原因；两个浏览器标签同时提交时，只有一个能通过状态令牌和项目工作区租约。
5. fetch 后正确刷新 ahead/behind；普通 push 成功、无认证、权限拒绝和 non-fast-forward 均被正确分类，且日志不泄露秘密。
6. 脏工作树、冲突状态、正在 rebase、外部 `.git/index.lock` 存在时，切换分支被拒绝且平台不删除任何 Git 锁或用户文件。
7. 刷新页面、控制服务重启后，已结束操作可从审计记录恢复；运行中操作根据真实仓库状态标记为成功、失败或 `needs_attention`，不伪造成功。
8. Go 后端执行单元、HTTP 和临时仓库集成测试；前端完成 TypeScript 构建，并以浏览器测试覆盖关键确认层、禁用状态和错误呈现。

## 12. 参考依据

- [Git `status` 文档](https://git-scm.com/docs/git-status)：`--porcelain` 输出版本化，适合脚本解析；v2 提供分支与 ahead/behind 信息。
- [Git `switch` 文档](https://git-scm.com/docs/git-switch)：分支切换应使用语义明确的 `switch`，不要将恢复文件的 `checkout` 语义暴露给工作台。
- [Git `worktree` 文档](https://git-scm.com/docs/git-worktree)：一个仓库可以有多个工作树，但引用和工作树管理需要显式协调。
- [Git `credential` 文档](https://git-scm.com/docs/gitcredentials)：凭据应通过 Git 的 credential helper 处理，应用层不保存秘密。
- [GitHub authentication 文档](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/about-authentication-to-github)：命令行 Git 的 HTTPS 与 SSH 使用不同认证方式；GitHub 不再接受账户密码作为 Git HTTPS 凭据。

## 13. 后续决策点

开始阶段 B 前，需要确认以下产品选择：哪些分支模式默认受保护（建议 `main`、`master`、`release/*`）；提交是否必须关联任务；项目是否只接入受信任仓库；以及任务 worktree 是默认开启还是仅对并行任务开启。它们会影响策略表、确认层文案和后续权限模型，但不影响本文件确定的 Runner、GitOperation、工作区租约和 API 边界。
