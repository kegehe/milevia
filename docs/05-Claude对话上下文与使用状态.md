# Claude 对话上下文与使用状态

> 日期：2026-07-19  
> 目标：在当前 Claude Code 对话中显示可理解的上下文占用、调用次数、Token、缓存、耗时和费用估算，帮助用户判断会话是否需要继续或新建。

## 1. 功能范围

本阶段只为当前项目的 Claude 对话增加会话状态面板，不制作跨项目报表、预算限制、账单结算或团队统计。

用户需要在对话过程中知道：

- 当前使用的模型和权限模式；
- 当前上下文距离模型上下文窗口还有多少空间；
- 当前会话已经发送多少次任务、Claude 经历多少 Agent 轮次；
- 本次任务与整个会话的输入、输出、缓存 Token；
- 本次任务和会话累计的费用估算、耗时和工具调用数量。

## 2. 数据来源

当前 Claude Code 使用：

```text
claude -p --verbose --output-format stream-json
```

不需要新增 Claude CLI 调用。控制服务已经保存原始 `stream-json` 事件，只需在接收事件时解析并持久化统计字段。

| Claude 事件 | 可读取的数据 | 用途 |
| --- | --- | --- |
| `system/init` | `model`、`session_id`、工具 | 初始化 Claude 会话状态。权限模式以平台 Conversation 记录为准。 |
| `assistant` | `message.id`、`message.model`、`message.usage`、工具调用 | 统计主 Agent 的步骤、当前上下文估算、工具调用。 |
| `result` | `num_turns`、`duration_ms`、`ttft_ms`、`total_cost_usd`、`usage`、`modelUsage`、`terminal_reason` | 记录一轮任务最终统计。 |
| `assistant` / `user` 工具块 | `tool_use`、`tool_result`、`parent_tool_use_id` | 统计工具调用，并将子代理消息归属到其父工具调用。 |

Claude Code 官方说明中，`result.total_cost_usd` 和 `modelUsage.*.costUSD` 是客户端价格表计算出的估算值，不是账单权威数据。因此页面必须标注“费用估算”，不可用于自动扣费、预算阻断或对用户结算。

## 3. 上下文指标的定义

“会话累计 Token”不能等同于“当前上下文占用”。同一个 Claude 会话会经历多个 Agent 步骤，且每轮 `result.usage` 是该次运行的累计量；直接相加会超过上下文窗口，得到错误百分比。

当前上下文采用以下定义：

```text
最后一个主 Agent 步骤的完整输入
(input_tokens + cache_read_input_tokens + cache_creation_input_tokens)
÷
该模型的 contextWindow
=
当前上下文占用估算
```

步骤所属模型取自该 `assistant` 消息；上下文窗口取同一模型在 `result.modelUsage` 中的 `contextWindow`。模型切换时不跨模型混算。`result.usage` 是当前 Run 的累计量，只用于 Run 用量统计，不能回填上下文快照。

例如：最后一个步骤输入为 `48,000` Token，模型上下文窗口为 `200,000` Token，显示为：

```text
上下文约 24% · 48k / 200k
```

规则：

- 只统计主 Agent，子代理 Token 不混入当前主会话上下文百分比。
- 同一 Claude 消息 ID 的并行工具事件必须去重，避免重复计算。
- Claude 尚未返回第一个可信 Agent 步骤时显示“上下文计算中”；若 Run 已结束仍没有该快照，则显示“上下文暂不可用”，不显示伪造的 `0%`。
- 模型未返回 `contextWindow` 或 Token 字段时显示“暂不可用”。
- 上下文管理与压缩事件继续保留在原始事件流中；本阶段不展示压缩历史，也不将压缩前后的百分比相加。

## 4. 调用次数的定义

页面不能只显示一个含义模糊的“调用次数”。需要区分：

| 指标 | 定义 | 数据来源 |
| --- | --- | --- |
| 任务次数 | 用户在当前 Conversation 发送并创建的 Run 数量。 | `runs` 表。 |
| Agent 轮次 | Claude 为一个 Run 执行的 Agentic 回合数。 | `result.num_turns`。 |
| 模型步骤 | 主 Agent 唯一 `assistant.message.id` 数量。 | `assistant` 事件，按消息 ID 去重。 |
| 工具调用 | `tool_use` 块数量，按工具调用 ID 去重。 | `assistant` 事件。 |
| 子代理数量 | 去重后的 `parent_tool_use_id` 数量。一个子代理可产生多条消息，只计一次。 | 流式事件。 |

顶部默认只显示“任务次数”和“上下文占用”。其它指标放入状态面板，避免对话页变成统计报表。

## 5. 页面展示

### 5.1 对话顶部状态

在当前项目标题旁增加紧凑状态入口：

```text
模型名称 · 上下文约 24% · 12 次任务 · 使用状态
```

- 上下文低于 70% 使用正常色。
- 70% 至 84% 使用提醒色。
- 85% 及以上使用警示色，并提示优先新建会话。
- 运行中持续显示本次任务已耗时；首个 Claude 响应后显示首响应时间。

### 5.2 使用状态面板

点击“使用状态”打开项目右侧面板或站内弹窗，不打断当前对话。内容分为两段：

| 区域 | 展示内容 |
| --- | --- |
| 当前任务 | 模型、状态、已耗时、首响应时间、Agent 轮次、工具调用、输入/输出 Token、缓存读取/创建 Token、费用估算。 |
| 当前会话 | 上下文占用、任务次数、累计 Agent 轮次、累计工具调用、累计输入/输出/缓存 Token、累计费用估算。 |

模型、上下文和费用均应附带数据状态：运行中、已完成、不可用或估算值。失败或停止的任务若 Claude 返回 `result`，仍需计入已消耗的 Token 和费用；没有 `result` 时显示“未获得统计数据”。

## 6. 持久化设计

保持现有 `events` 原始事件表作为审计来源，新增结构化的运行统计表，不在网页端从全部历史 JSON 重新计算。

```text
run_usage
  run_id                 主键，关联 runs
  model                  主模型名称
  context_window         当前模型上下文窗口
  context_input_tokens   最近主 Agent 步骤输入 Token
  input_tokens           本次 Run 顶层 Agent 输入 Token，不含子代理
  output_tokens          本次 Run 顶层 Agent 输出 Token，不含子代理
  cache_read_tokens      本次 Run 顶层 Agent 缓存读取 Token
  cache_creation_tokens  本次 Run 顶层 Agent 缓存创建 Token
  estimated_cost_usd     本次 Run 整棵 Agent 树费用估算，包含子代理
  agent_turns            result.num_turns
  model_steps            主 Agent 唯一步骤数
  tool_calls             工具调用数
  subagent_count         子代理数量
  duration_ms            总耗时
  ttft_ms                首响应耗时
  terminal_reason        Claude 结束原因
  completed_at           统计完成时间
```

`result.modelUsage` 是按模型、包含子代理的聚合数据，需要单独保存：

```text
run_model_usage
  run_id + model          联合主键
  input_tokens            该模型输入 Token
  output_tokens           该模型输出 Token
  cache_read_tokens       该模型缓存读取 Token
  cache_creation_tokens   该模型缓存创建 Token
  estimated_cost_usd      该模型费用估算
  context_window          该模型上下文窗口
```

会话累计值通过 `run_usage` 与 `run_model_usage` 聚合，不重复写入 `conversations`。已完成会话的上下文入口读取最近一条 `run_usage`；运行中的上下文快照只保存在当前 Run 聚合器中。

## 7. 服务实现

### 7.1 事件解析

控制服务在读取 Claude 输出时执行两类操作：

1. 原样保存事件并推送网页，保持现有对话时间线行为。
2. 解析 `assistant` 与 `result` 事件，更新当前 Run 的内存聚合器、`run_usage` 与 `run_model_usage`。

聚合器以 `run_id` 为边界，记录已统计的 Claude 消息 ID 与工具调用 ID。Run 完成时一次写入最终统计，避免每个流事件频繁更新 SQLite。

### 7.2 API

新增：

```text
GET /api/conversations/{conversationID}/usage
GET /api/runs/{runID}/usage
```

会话接口返回：

- 当前上下文快照；
- 当前 Run 的实时聚合数据；
- 当前会话的累计聚合数据；
- 各模型使用明细。

运行过程中通过现有 WebSocket 发送 `usage.updated` 事件。网页仅更新状态入口和打开的状态面板，不刷新整个对话时间线。

## 8. 验收条件

1. 新建或恢复会话后，顶部显示模型和上下文状态；没有可用数据时明确显示计算中或不可用。
2. 一次 Claude Run 完成后，能看到本次任务的 Token、缓存、Agent 轮次、工具调用、耗时和费用估算。
3. 连续发送多条消息后，会话累计任务次数、Token 和费用估算正确累加。
4. 同一 Claude 消息 ID 的并行工具调用不会使模型步骤或 Token 重复计算。
5. 子代理 Token 不影响主会话上下文百分比，但会计入整个 Run 的费用估算和模型使用明细。
6. 停止或失败的任务若收到 `result`，仍保留已产生的使用数据。
7. 刷新网页或恢复历史会话后，状态面板仍能读取历史统计。
