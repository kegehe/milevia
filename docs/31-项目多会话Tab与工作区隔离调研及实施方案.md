# 项目多会话 Tab 与工作区隔离调研及实施方案

> 日期：2026-08-21
>
> 目标：同一已加载项目可同时打开多个 AI 会话，在会话 Tab 间切换，且聊天上下文、草稿、事件、运行记录与 Agent 原生 Session 互不串联。
>
> 决策摘要：优先交付“多会话 Tab + 上下文隔离 + 同项目单写入工作区”；一期对被占用的工作区返回可识别的占用状态，不引入新的全局 Run 队列。只有确有多个 Agent 同时修改代码的需求时，才为会话分配独立 Git worktree 和分支。

## 1. 结论

可以实现，但不能只移除“只能有一个当前会话”的限制。

当前系统已经按 `conversation_id` 持久化消息、事件、运行记录、用量和 WebSocket 订阅；Claude Code 与 Codex 也分别按会话持有或恢复原生 Session。因此，聊天上下文隔离已有坚实基础。

真正需要区分的是两个层次：

1. **多会话 Tab**：多个会话可同时打开、后台继续运行、自由切换，互不混入历史和输入内容。
2. **并发操作工作区**：多个会话是否能同时读写同一个项目目录。这不是聊天上下文问题；若直接放开，会造成文件覆盖、Git 锁冲突、测试结果互相影响和不可归属的改动。

推荐先完成第一层，并继续保护默认项目工作区的一次可写执行。第二层应以独立 worktree 实现，不能靠取消锁实现。

## 2. 当前实现与限制来源

### 2.1 已具备的隔离能力

| 范围 | 现有机制 | 隔离结论 |
| --- | --- | --- |
| 聊天记录 | `messages.conversation_id` | 不同会话的用户和助手消息不混合。 |
| 事件与实时输出 | `events.conversation_id`、`/ws/conversations/{conversationID}` | WebSocket 订阅按会话分组。 |
| 运行与用量 | `runs.conversation_id`、`run_usage.conversation_id` | 停止、状态和统计可按会话处理。 |
| 前端草稿 | `projectId + conversationId` 的 LocalStorage key | 切换会话不会覆盖另一会话草稿。 |
| Claude Code | `sessions[conversationID]`，每会话一个持久流式 Session | 不同会话不会共用 Claude 进程上下文。 |
| Codex | `agent_session_id`，后续使用 `codex exec resume <sessionID>` | 不同会话恢复各自的 Codex thread。 |

### 2.2 当前“单会话”语义

`conversations.is_current` 同时承担了“最近选择的会话”和“唯一可工作的会话”两个职责：

- `conversations_one_current_per_project` 部分唯一索引要求每项目最多一个 `is_current=1`。
- 创建 `?new=true` 会话时，如果当前会话在运行，后端返回冲突；否则会将旧会话设为非当前。
- 激活历史会话时，如果原当前会话仍运行，后端返回冲突。
- 发送消息、修改权限、清空上下文要求目标会话是当前会话。
- 前端进入 `/projects/:projectId/conversations/:conversationId` 时默认调用 `activate`，使“查看会话”变成“切换唯一工作会话”。

这就是用户必须先停止或关闭先前会话才能使用新会话的直接原因。

### 2.3 工作区锁是另一项独立保护

`projectWorkspaceLeases` 以 `projectID` 为粒度保护 Agent Run、Git 操作和文件写入。即使解除 `is_current` 限制，第二个运行仍会收到“项目工作区被另一运行或 Git 操作占用”的冲突。

该锁不应在一期删除。多个 Agent 即使拥有独立聊天上下文，只要同时操作同一目录，仍会在文件系统和 Git 层面相互污染。

## 3. 推荐架构

### 3.1 一期：多会话 Tab，默认工作区串行执行

一期的目标是让用户不必关闭会话：可以新建、打开、关闭 UI Tab，并在任意时刻切回；运行中的会话允许留在后台，完成后保留结果。

```text
Project
  Conversation Tabs（仅前端打开状态）
    Tab A -> Conversation A -> Agent Session A -> Messages / Events / Runs A
    Tab B -> Conversation B -> Agent Session B -> Messages / Events / Runs B
    Tab C -> Conversation C -> Agent Session C -> Messages / Events / Runs C

  Default project workspace
    同时只授予一个可写 Run 的 lease
```

原则：

- 会话 Tab 是 UI 概念，关闭 Tab 不删除会话，也不停止后台 Run；但不等于无限保留 Agent 进程，空闲原生 Session 由后端生命周期策略回收。
- 路由继续使用 `/projects/:projectId/conversations/:conversationId`，以保证刷新、深链和浏览器历史正确；窗口级 Tab 状态使用 `sessionStorage`，不使用跨窗口共享的 LocalStorage。
- 打开或聚焦会话不再是发送、清空、改权限的授权前提。
- 某会话在后台运行时，切到另一个会话仍可查看和编辑草稿；若立即发起需要工作区的 Run，一期返回明确的占用状态。项目级等待队列是后续独立能力，不能与会话内队列混用。
- 当前工作区的文件、Git、终端页面仍指向项目默认目录，不假装是每个会话的独立代码副本。

### 3.2 二期：会话级独立 worktree，支持并发开发

只有需要“多个会话同时修改代码”时实施二期。每个可写会话绑定独立 branch 和 Git worktree，Agent 使用会话的工作目录而不是项目默认目录。

```text
Project repository
  default worktree                 -> 用户当前主工作目录
  .milevia/worktrees/<conversationA> -> branch milevia/conversationA
  .milevia/worktrees/<conversationB> -> branch milevia/conversationB

Conversation A Run -> worktree A
Conversation B Run -> worktree B
```

二期必须提供：创建/复用/清理 worktree、分支命名、基线 revision、变更预览、提交、合并或丢弃、冲突处理，以及工作树中端口和开发服务的资源隔离。现有自动编排模块已有任务 worktree 生命周期，可复用其底层 Git 机制，但普通会话不可直接复用自动编排任务记录。

## 4. 一期后端改造

### 4.1 重新定义 `is_current`

短期保留列与唯一索引，兼容现有迁移和项目概览；其语义仅限“服务端记录的最近一次选择”，不再表示唯一可工作的会话，也不能代表某个浏览器窗口的活跃 Tab。

客户端 Tab 选择只能存放在客户端状态中。所有新接口必须显式接收 `conversationId`；禁止新增任何以 `is_current` 决定授权、任务目标、通知目标或 Agent 选择的代码。

长期可移除 `is_current`，或仅保留 `last_selected_at` 作为无业务副作用的排序信息。该迁移可在一期稳定后单独发布。

### 4.2 解除会话操作授权与 current 的耦合

以下操作应仅验证会话归属、会话自身状态和来源限制，不再验证 `is_current`：

| 操作 | 一期规则 |
| --- | --- |
| 创建会话 | 不因其他会话在运行而拒绝；新会话成为最近选择会话。 |
| 打开/聚焦会话 | 只更新最近选择标记；不得停止、抢占或改变其他会话。 |
| 发送消息 | 允许向任一普通会话发送；实际执行由会话状态和工作区调度决定。 |
| 修改权限 | 会话自身 idle 时允许修改。 |
| 清空上下文 | 仅替换目标会话为新的空会话，不影响其他会话。 |
| 停止 Run | 仅停止指定 `runID` 所属会话。 |

自动编排会话和定时任务会话继续保持只读，不能被普通 Tab 操作转为交互会话。

### 4.3 项目级派生状态

以下依赖 `is_current` 的查询需改为明确的聚合或“最近选择”查询：

- 项目状态中的 `running`：检查项目任意会话是否有运行中的 Run。
- 项目概览标题：取最近选择会话或最近活动会话，字段命名不得暗示它是唯一活跃会话。
- 通知、任务派发、优化建议：调用方必须传入 `conversationId`，或由后端按确定且与用户选择无关的规则选择；不能将“最近选择”作为跨窗口共享的隐式业务目标。

### 4.4 工作区调度

一期不允许绕过 `projectWorkspaceLeases`，也不新增项目级 Run 队列：

- 可立即执行时获得 lease 并创建 Run。
- 工作区忙时返回带占用类型和会话/操作摘要的 409；前端保留输入草稿，并展示“等待当前任务结束后重试”。
- 现有 `queued` 仅表示同一个流式 Agent Session 已接收但尚未执行的 Turn，不能复用为“等待项目工作区”的状态。
- 只读并发是否放行必须单独评估；在文件系统、Git 与外部命令均可能变动的当前模型中，默认仍使用互斥策略。

若后续需要“任务结束后自动运行”，必须作为独立的持久化项目调度器设计：包含等待原因、优先级、取消、服务重启恢复、获取 lease 的时点、额度准入和与会话内队列的边界。

### 4.5 Agent Session 生命周期

Claude 的流式 Session 是常驻进程。多会话后，不能因为用户曾经打开很多 Tab 就无限保留进程。已实现 `ConversationSessionManager`，并按以下规则运行：

- 仅在第一次实际发送 Claude 消息时启动原生 Session；打开、关闭或恢复 UI Tab 不启动进程。
- Tab 关闭不停止运行中的 Run；Run 结束后的 Session 可进入空闲状态。
- 空闲 TTL 默认 30 分钟、每 Runner 默认最多 4 个常驻 Session；可分别由 `AUTO_CONVERSATION_SESSION_IDLE_TTL` 与 `AUTO_CONVERSATION_SESSIONS_PER_RUNNER` 配置。扫描间隔为 TTL 的四分之一，最短 1 秒、最长 1 分钟。
- 达到上限时，先驱逐无运行、无审批、无队列的最久未使用 Session。驱逐时先标记 `stopping`，异步停止进程，并仅由既有 watcher 在 `Done` 后移除内存记录。
- 正在停止的 Session 仍占用容量；在 watcher 移除前，新建 Session 返回可重试的 409，不能连接到即将退出的旧进程，也不会继续级联驱逐健康 Session。
- 修改空闲会话的权限模式时，先标记并停止旧 Session；watcher 确认退出前不写入新权限并返回可重试的 409，下一次发送将以新权限启动进程。
- 撤销 Agent profile revision 时，停止所有绑定该 revision 的原生 Session（包括没有活动 Run 的 Session），并取消关联中的 Run；Session 退出前不会再次接受该 revision 的新 Turn。
- 每个原生 Session 保存启动配置指纹（Runner、Agent、工作目录、权限和 profile revision）。新 Turn 发现指纹不一致时，只回收无运行、无审批、无队列的旧进程；运行中 Session 返回明确冲突，必须先按既有 Run 生命周期停止后重试。
- Session 的创建配置包括 Agent、权限模式、profile revision 和工作区。任一可变配置变更时，必须在会话没有运行、审批和队列时停止并回收 Claude Session，确认退出后再更新记录；下次发送以新配置创建或恢复。Codex 每 Run 新建进程，仍使用同一配置快照。
- 下次发送从已持久化的 `agent_session_id` 恢复。驱逐是进程回收，不是删除聊天上下文；已在 Windows 本机 Claude Code `2.1.233` 完成真实 POC：首轮进程以 `--session-id` 写入随机短语，退出后第二个 `claude -p --resume <session_id>` 进程准确返回短语。服务层亦有回归测试，确保重建流式 Session 时传入持久化 ID 与 `Resume: true`。
- 可选集成发布门禁为 `MILEVIA_RUN_CLAUDE_RESUME_POC=1 go test ./internal/app -run TestClaudeResumeAfterProcessExitPOC -count=1`。它使用操作者已登录的本机 Claude，可能产生模型费用；CLI 行为变化时必须重新执行。若未来 POC 不成立，回收后的降级行为是新建原生 Session、保留平台持久化历史，并在 UI 中明确标注原生上下文已重置；不得静默声称上下文连续。
- Session 不能跨工作区迁移；二期中 worktree 被清理、重建或切换基线前必须先停止关联 Session。

## 5. 一期前端改造

### 5.1 会话 Tab 状态

新增窗口级 `ConversationTabsStore`，状态以 `projectId` 分区并持久化到 `sessionStorage`：

```ts
type ConversationTabsState = {
  openConversationIds: string[];
  activeConversationId: string | null;
  readPositions: Record<string, { createdAt: string; id: string }>;
  latestPositions: Record<string, { createdAt: string; id: string }>;
  unreadConversationIds: string[];
};
```

`sessionStorage` 隔离同源浏览器窗口，同时能跨页面刷新恢复当前窗口的 Tab。会话草稿继续使用既有的 LocalStorage 分区键；不得把窗口活跃 Tab 写入 LocalStorage。

建议限制每项目最多 12 个打开 Tab；超过上限时要求用户先关闭一个 UI Tab。关闭 Tab 只从该状态移除 ID，绝不删除数据库会话或停止运行；Server 端的空闲 Session 是否回收由 4.5 节的策略决定。

### 5.2 交互规则

- `+` 新建会话，创建成功后立即打开并聚焦新 Tab；会话 Tab 支持方向键循环切换，`Home` / `End` 跳转首尾，键盘焦点和 URL 始终对应同一会话。
- 历史会话点击后打开或聚焦对应 Tab；不再区分“运行中只能只读查看”。
- Tab 显示标题、Agent、运行/失败状态和未读输出提示；关闭按钮仅关闭 Tab。工作区被占用不是会话状态，而是发送动作的独立反馈。
- 活跃 Tab 改变时更新 URL；地址栏直接访问某会话时，若尚未打开则加入 Tab 列表。
- 页面刷新后恢复已打开 Tab。活动批量接口将已删除 ID 作为 `missingConversationIds` 返回，前端从当前窗口 Tab 状态中剔除并选择相邻 Tab；跨项目 ID 仍返回 404，避免泄漏其他项目会话信息。
- 活跃内容仍可使用现有单个 `ConversationPage`；切换时按会话 ID 重新加载历史和草稿，不必将所有复杂对话 DOM 永久挂载。

### 5.3 后台会话状态同步

活跃 Tab 沿用会话 WebSocket。后台 Tab 不必长期挂载完整时间线；一期以定时请求会话摘要更新标题与运行状态，避免为每个打开 Tab 长期维护 WebSocket。需要更低延迟时再增加有上限的项目级订阅管理器。

未读状态不能只依赖内存或会话列表。新增可增量读取的会话活动 API，例如：

```text
POST /api/projects/{projectID}/conversations/activity

{
  "cursors": [
    { "conversationId": "...", "after": { "createdAt": "...", "id": "..." } }
  ]
}
```

响应按 `conversationId` 返回较新事件、`latestPosition` 与是否截断。活动流至少包含持久化的助手输出、Run 状态和审批相关事件，排序键固定为 `{ createdAt, id }`；批量接口只允许查询当前项目且打开的会话。这样最多 12 个后台 Tab 只需一次轮询，且刷新后可以从水位补齐未读。

未读状态以每个 Tab 的 `{ createdAt, id }` 为本地水位，切换到该 Tab、阅读到末尾后推进；刷新和重连后以该活动 API 校正。单独的 UUID 不具备时间排序语义，不得仅凭“切过 Tab”或内存增量判断未读。

切回后台 Tab 时必须以服务端持久化历史为准重新加载，不能依赖内存缓存。这可保证页面刷新、网络重连和服务重启后的正确性。

## 6. 二期数据模型与边界

worktree 不是会话上的可变字符串，而是可审计的资源版本。新增不可变的 `conversation_workspaces`，会话只引用当前工作区；每个 Run 再保存实际使用的工作区快照：

```sql
create table conversation_workspaces (
  id text primary key,
  conversation_id text not null references conversations(id) on delete cascade,
  generation integer not null,
  mode text not null,
  path text not null,
  branch text not null,
  base_revision text not null,
  state text not null,
  created_at datetime not null,
  archived_at datetime,
  unique(conversation_id, generation)
);

alter table conversations add column active_workspace_id text references conversation_workspaces(id);
alter table runs add column workspace_id text references conversation_workspaces(id);
alter table runs add column workspace_path text not null default '';
alter table runs add column workspace_branch text not null default '';
alter table runs add column workspace_base_revision text not null default '';
```

`mode` 至少包括 `project_shared`、`isolated_worktree` 和 `read_only_snapshot`。`state` 应至少包括 `provisioning`、`ready`、`active`、`merging`、`archived`、`cleaned`、`failed`。工作区一旦被 Run 使用，其路径、分支和基线只可归档不可覆写；重建或更新基线创建新 generation。需要增加索引与完整迁移回滚策略，且仅对 Git 项目启用 `isolated_worktree`。

二期的 workspace lease 应按实际 `workspace_path` 加锁，不再按 `projectID` 全局互斥；但共享主工作区的 Git、文件编辑和项目运行仍需保持项目级互斥。项目启动端口、环境变量和终端工作目录也必须成为会话工作区的一部分。

工作树的创建、清理、重建、基线更新和合并必须串行化。Claude 的持久进程启动时绑定工作目录，Session 存活期间不得迁移或删除其 worktree；必须先停止并回收该 Session。Codex 每轮也必须以该会话的 `workspace_path` 作为进程工作目录恢复。所有 worktree 路径均须在受控根目录下计算和验证，不能接受客户端路径。

独立 worktree 不代表必然获得并发执行资格。Run 在实际派发时仍要经过 Agent profile、credential pool 和 quota group 的并发额度检查；前端要区分“工作树可用但等待凭据额度”与“工作树/项目操作被占用”。

## 7. 关键风险与处理原则

| 风险 | 处理原则 |
| --- | --- |
| 后台 Run 的输出丢失或串到错误 Tab | 每个事件和 WebSocket 强制带 `conversationId`；切回时从 API 重载并按 ID 合并。 |
| 多会话草稿串写 | 继续使用 `projectId + conversationId` 键；任何异步失败恢复均校验路由版本和会话 ID。 |
| 旧逻辑仍假设唯一 current | 全局检索 `is_current`，逐处改为聚合、最近选择或显式 `conversationId`。 |
| 同目录并发写 | 一期保留工作区 lease；二期使用独立 worktree。 |
| Claude 会话停止影响其他会话 | Session 管理键必须始终是 `conversationID`，停止操作只作用于目标 Run/Session。 |
| 空闲 Session 进程累积 | 使用按 Runner 配置的 TTL、最大数量和 LRU 驱逐；驱逐后按保存的原生 Session ID 恢复。 |
| Claude Session 配置已过期 | 权限、profile revision 或工作区变更时先回收空闲 Session；新配置不可注入已启动进程。 |
| Claude resume 行为与预期不符 | 将“停止后 resume”作为真实 CLI POC 的发布门槛；失败时明确降级为新原生 Session。 |
| Codex thread 混用 | 保持 `(agent_runtime_id, agent_id, agent_session_id)` 唯一性，恢复时只使用目标会话 Session ID。 |
| UI Tab 恢复到已删除会话 | 启动时批量校验，404 后删除本地 Tab 记录并选择相邻 Tab。 |
| 多窗口覆盖最近选择会话 | Tab 选择只存客户端；服务端业务操作要求明确的 `conversationId`。 |
| 多窗口 Tab 状态互相覆盖 | 窗口级 Tab 状态使用 `sessionStorage`，草稿才使用 LocalStorage。 |
| 刷新后未读丢失或误报 | 以 `{ createdAt, id }` 作为水位，并使用会话活动增量 API 补齐。 |
| worktree 清理时仍有 Agent 进程 | 先停止关联 Session 并等待退出，再清理或重建工作树。 |
| worktree 已就绪但额度不足 | 在派发时执行 quota admission；单独展示额度等待原因，不把它伪装为工作区占用。 |
| 历史 Run 无法定位实际工作树 | Run 保存不可变 `workspace_id`、路径、分支与基线快照。 |

## 8. 实施顺序

1. 为 `is_current` 的所有读取建立清单，先编写多会话后端集成测试。
2. 调整创建、激活、发送、清空和权限接口，解除操作对 `is_current` 的授权依赖；禁止新业务依赖该字段。
3. 修正项目状态、任务、通知和优化建议中的派生查询，改为显式 `conversationId` 或确定性聚合。
4. 完成 Claude “停止后 resume”真实 CLI POC，并将结果和失败降级策略固化为可选集成测试；CLI 升级时重新执行。
5. 实现 `ConversationSessionManager` 的 TTL、容量、配置变更回收和 LRU 驱逐。
6. 建立使用 `sessionStorage` 的 `ConversationTabsStore`，实现 URL、恢复、关闭、新建、历史打开和未读水位行为。
7. 实现会话活动批量增量 API 与后台摘要轮询，验证运行中切换、刷新恢复和多窗口状态隔离。
8. 已完成：工作区占用以 `409 workspace_occupied` 返回，附带脱敏的占用类型和操作摘要；前端保留草稿并显示“等待当前任务结束后重试”，默认互斥保持不变。项目级自动排队另立设计。
9. 在一期稳定后，再单独设计并实现带工作区版本快照的 worktree、额度调度、合并和清理流程。

## 9. 验收标准

### 9.1 一期

1. 同一项目可创建并同时打开至少三个普通会话 Tab。
2. A 会话运行时可切换到 B；A 不被停止，B 的历史和草稿不出现 A 的内容。
3. 页面刷新、路由深链和服务重启后，每个会话恢复自己的平台历史；原生 Session 的恢复行为满足经 POC 验证的契约或显示明确降级状态。
4. 向 A 停止、清空或修改权限不会影响 B；权限、profile revision 或工作区变更后，旧 Claude Session 必须先退出。
5. A 与 B 的 WebSocket 输出、状态徽标和未读提示不会串联；从活动水位恢复的未读结果与持续在线时一致。
6. 同一默认工作区的第二个可写执行返回含占用原因的明确反馈，不要求用户关闭任何会话。
7. 超过空闲 TTL 或 Session 上限时，最久未使用的 Claude Session 被回收；再次发送遵守 POC 验证后的恢复或降级契约。
8. 两个浏览器窗口分别切换 Tab，不会改变对方的活跃 Tab、任务目标或通知目标。
9. 自动编排与定时任务会话仍然不可被普通交互操作修改。

### 9.2 二期

1. 两个独立 worktree 会话可并发执行可写 Agent Run，修改只出现于各自工作树。
2. 文件、Git、终端和项目运行页明确展示当前使用的工作区。
3. 分支提交、差异预览、合并、冲突和清理操作均可审计和恢复。
4. 默认项目工作区的既有 Git/文件互斥行为不回归。
5. 清理、重建或迁移 worktree 前，关联 Agent Session 已停止并确认退出。
6. 并发 worktree Run 分别遵守 profile、credential pool 和 quota group 的额度限制。
7. 每个历史 Run 能显示其不可变的 `workspace_id`、路径、分支和基线 revision，即使会话已切换到新 generation。

## 10. 涉及模块

| 模块 | 一期改动 |
| --- | --- |
| `apps/control-server/internal/app/app.go` | 会话生命周期、消息准入、项目状态、WebSocket 与运行状态。 |
| `apps/control-server/internal/app/conversation_workspaces.go` | 会话共享/隔离工作区的创建、列表与激活 API。 |
| `apps/control-server/internal/app/*runner*.go` | 原生 Agent Session 的创建、恢复、停止与工作目录绑定。 |
| `apps/control-server/internal/app/*` 会话活动接口 | 后台 Tab 未读水位的批量增量查询与稳定排序。 |
| `apps/control-server/internal/app/task.go` | 任务派发改为显式会话或确定性业务规则，不依赖最近选择会话。 |
| `apps/control-server/internal/app/notification.go` | 通知目标会话选择规则。 |
| `apps/control-server/internal/app/insights.go` | 默认 Agent/会话选择规则。 |
| `apps/web/src/pages/ConversationPage.tsx` | 路由激活语义、Tab 栏、后台状态与切换行为。 |
| `apps/web/src/lib/conversation-draft.ts` | 保持现有会话级草稿隔离，补充 Tab 恢复测试。 |
| `apps/web/src/lib/types.ts` | 会话摘要、Tab 状态、排队/工作区状态 DTO。 |

## 11. 不采用的方案

- **只删除唯一索引**：会留下发送、权限、清空和前端激活链路的 current 依赖，无法得到真正可用的多会话。
- **只做前端假 Tab**：后端仍会在切换或运行中拒绝，且后台状态不可正确恢复。
- **取消项目工作区 lease**：聊天虽隔离，文件和 Git 状态会互相污染。
- **把现有 `queued` Run 当作工作区队列**：它是流式会话内已提交的 Turn 队列，缺少等待原因、重启恢复、优先级和工作区 lease 调度语义。
- **无限保留关闭 Tab 的 Claude 进程**：会在多会话使用后耗尽本机或远程 Runner 资源。
- **将窗口活跃 Tab 写入 LocalStorage**：同源窗口共享该存储，会互相覆盖活跃 Tab。
- **只靠会话列表校正未读**：列表没有可恢复的活动水位，无法在刷新或断线后可靠补齐。
- **一期直接实现 worktree**：会把多 Tab 体验、Git 分支策略、合并产品流程、端口隔离和清理回收混为一个高风险发布。

## 12. 最终建议

一期是当前需求的最佳方案：它以较小风险实现“多个会话同时存在、Tab 切换、上下文不互相影响”，并保留对共享项目目录的安全保护。

二期不是一期的简化替代，而是独立的“并发开发工作区”能力。只有明确需要多个 Agent 同时写代码时，才应投入 worktree 隔离、合并和资源管理。

## 13. 当前实施状态（2026-08-21）

已完成并通过构建与回归测试的范围：

- 后端的创建、打开、发送、清空和权限修改不再以 `is_current` 作为会话可操作性的前置条件；该字段仅保留为旧接口兼容的最近选择标记。
- 项目运行状态改为聚合所有会话；任务、通知和优化建议的会话回退规则改为显式目标或确定性的最近活动会话。
- 前端新增项目级、窗口级的 `sessionStorage` Tab 状态；可创建、打开、关闭和切换多个会话，刷新后恢复当前窗口的 Tab。草稿继续按 `projectId + conversationId` 隔离。
- 路由进入目标会话不再调用会阻塞运行中会话的旧 `activate` 流程；后台 Tab 以受限的会话摘要轮询刷新标题和运行状态，切回后仍从持久化历史重新加载。
- 同一项目默认工作区的 `projectWorkspaceLeases` 保持不变。第二个需要写工作区的 Run 仍返回占用冲突，不会覆盖第一个 Run 的文件或 Git 状态。

已完成：可恢复未读水位的活动增量 API、Claude 常驻 Session 的 TTL/LRU 回收、权限模式变更触发的空闲 Session 回收、profile revision 撤销时的 Session 回收、启动配置指纹校验、真实 `--resume` POC，以及带占用类型和摘要的 `workspace_occupied` 反馈。二期已完成完整工作区联动：每个会话都有 `project_shared` 工作区资源，所有新 Run 保存实际工作区的 ID、路径、分支与基线快照；本地 Git 项目支持服务端生成受控目录和分支的 `isolated_worktree` 创建、列表、激活与安全归档。文件、Git、终端、项目启动及其日志订阅均按当前会话工作区解析；不同隔离 worktree 可并行执行，默认共享目录仍保持互斥。Git 操作审计、下载票据、终端归属和项目运行管理器均绑定工作区，项目删除会回收全部 worktree 的运行资源；项目概览状态聚合所有工作区进程。归档会校验会话空闲、运行、终端和路径安全，并保留历史 Run 快照。
