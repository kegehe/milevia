# Agent 会话节点回退设计

## 目标

为 Claude Code、Codex 和 CodeBuddy Code 提供统一的会话节点回退能力，支持：

- 仅恢复对话历史；
- 仅恢复代码文件；
- 同时恢复对话和代码；
- 从历史节点创建新的会话分支。

第一阶段只实现控制服务后端闭环，前端通过 HTTP API 使用。回退节点以 Milevia 自己的节点记录为主，CLI 原生 ID 作为辅助映射。

## 架构

新增三层：

1. **Checkpoint Store**：保存 conversation、父节点、prompt、CLI 原生 session/turn ID、文件快照目录和状态。
2. **Workspace Snapshot**：在每个已完成用户 turn 后保存项目目录的文件快照；快照只覆盖项目目录内的普通文件，跳过 `.git`、符号链接和超出大小限制的文件，并记录跳过原因。
3. **Agent Rewind Adapter**：统一暴露 list、rewind、fork 操作。Codex 可调用 app-server 的 `thread/revert`/`thread/fork`；Claude 和 CodeBuddy 在 headless 模式下使用 Milevia 自己的对话截断和文件恢复。

## 数据模型

新增 `conversation_checkpoints` 表：

- `id`：Milevia 节点 ID；
- `conversation_id`；
- `parent_id`；
- `agent_id`；
- `native_session_id`；
- `native_turn_id`；
- `prompt`；
- `snapshot_path`；
- `status`：`active`、`rewound`、`branched`、`failed`；
- `created_at`、`rewound_at`；
- `metadata` JSON。

现有 `messages` 和 `events` 不删除。回退后通过 checkpoint 节点确定可见前缀，并保留原始记录以支持审计和分支。

## API

- `GET /api/conversations/{id}/checkpoints`
- `POST /api/conversations/{id}/rewind`
  - `checkpointId`
  - `mode`: `conversation`、`code`、`conversation_and_code`
  - `fork`: 是否创建新 conversation
- `POST /api/conversations/{id}/checkpoints/{checkpointId}/fork`

回退要求会话没有正在执行的 turn；执行期间加 conversation 级锁。成功后停止当前 native session，恢复文件，更新会话 native ID/可见节点，并广播统一事件。

## CLI 适配

- **Codex**：记录 `thread.started` 和 `turn.completed` 的 ID。后续接入 `codex app-server`，调用 `thread/revert` 截断 durable history；文件恢复仍由 Workspace Snapshot 执行。分支优先调用 `thread/fork`。
- **Claude Code**：记录 `system/init` 的 session ID 和每个 turn 边界。由于 `-p --input-format stream-json` 不暴露 `/rewind`，使用 Milevia snapshot 实现代码和对话恢复。
- **CodeBuddy Code**：记录 `system/init` 的 session ID 和 `result` turn 边界。当前使用 Milevia snapshot 实现恢复；不得假设 Claude 的 file-history 格式适用于 CodeBuddy。

## 文件恢复规则

- 快照目录必须位于应用数据目录，不放入项目工作区；
- 恢复前检查项目路径仍与 checkpoint 创建时一致；
- 采用临时目录写入后原子替换；
- 不删除 checkpoint 之后由用户新增但未被快照记录的文件，除非调用方显式选择“严格恢复”；第一阶段默认只恢复快照中记录的路径；
- 任何跳过、冲突和恢复失败都写入 checkpoint metadata，并作为 API 错误返回。

## 错误与一致性

- 回退过程失败时不截断现有对话，不标记节点为成功；
- 文件恢复成功但 CLI 原生历史失败时，返回部分成功状态并保留可重试信息；
- 服务重启后通过数据库 checkpoint 和快照目录恢复可见状态；
- 不执行 Git reset/revert，也不依赖项目必须是 Git 仓库。

## 测试

- checkpoint 创建、排序、父子关系和 API 参数校验；
- 文件快照对新增、修改、删除、符号链接和路径越界的处理；
- 三种 agent 的 turn 事件解析和 checkpoint 记录；
- 回退失败时的事务和并发锁；
- Codex app-server adapter 的 `thread/revert`/`thread/fork` 请求序列化。
