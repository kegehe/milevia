# 严格串行 Dev 集成任务编排方案

> 日期：2026-07-23
>
> 决策：当前采用“**单项目单通道、任务分支隔离、自动集成到 dev、固定验收快照、用户手动合并 main**”的持久化任务队列方案。
>
> 非目标：不并行实施任务，不直接自动合并 main，不以模型自我声明作为质量结论，不在当前阶段引入 Temporal 或其他独立工作流服务。

## 1. 目标与边界

系统要把人工盯盘式的任务下发，变成可恢复、可审计的自动开发流水线，同时保留用户对稳定主线的最终控制权。

每个项目同一时刻最多只有一个任务处于准备、实施、修复、检查或集成状态。任务必须按队列顺序完成；队首任务未满足依赖时，调度器等待，不跳过执行后续任务。

分支职责固定：

| 分支 | 定义 | 写入方式 |
| --- | --- | --- |
| `main` | 用户已验收的稳定主线。 | 仅由用户将已验收快照合并进入。 |
| `dev` | 自动化任务的严格串行集成线，允许持续验收。 | 调度器在所有质量门禁通过后合并任务分支。 |
| `task/<task-id>-<slug>` | 单一任务的隔离实施分支。 | Agent 仅在对应 worktree 中修改与提交。 |
| `release/<dev-sha>` | 从某个固定 `dev` 提交创建的验收快照。 | 不继续写入；用户确认后合并到 `main`。 |

`main` 永远不接受 Agent 的直接写入。`dev` 不是自由开发分支，而是由调度器独占写入、具有完整审计记录的集成分支。

## 2. 为什么选择该方案

“每个任务等待用户合并 main 后再开始下一个”的方式最简单，但会让自动队列长期停在人工验收处。反过来，让多个任务并行实施会产生工作区、分支基线和合并冲突。

本方案在两者之间取平衡：任务仍然严格串行，因此没有并行改代码的冲突；已通过机器验证的任务自动进入 `dev`，因此队列不需要等待用户每项都合并 `main`；用户只在固定的 `release` 快照上进行批次验收，并决定何时把它合并入 `main`。

这比当前阶段引入 Temporal 更简单：不需要额外的 Temporal Server、Namespace、Task Queue 或跨系统状态同步。现有 Control Server、SQLite、Task、TaskRun、Run 和 Runner 即可承担单通道的持久化调度。

本方案不包含 Temporal 适配器或 Temporal 运行配置，确保只有一个内建调度器持有项目 lease。未来只有在文档第 10 节的演进条件满足后，才以独立变更重新评估该依赖。

## 3. 总体架构

```text
Web
  -> Control Server
       -> SQLite
            tasks / task_runs / task_events
            task_execution_intents
            task_orchestration_jobs
            git_task_records
            verification_runs
       -> 单项目调度循环（同一项目一个 lease）
            -> 创建 task 分支 + git worktree
            -> Agent Runner（实施 / 修复）
            -> 固定检查命令
            -> 独立审查 Agent
            -> 自动合并 task 分支到 dev
            -> dev 集成检查
       -> Git 仓库：main / dev / task/* / release/*
       -> WebSocket / API：状态、日志、检查报告、人工决策
```

Control Server 是唯一的调度者。它不依赖浏览器内存；所有状态转换、Git 基线、执行意图、检查输出摘要和用户决策必须写入数据库。服务重启后，它从数据库恢复未完成工作，而不是重新创建分支或重复启动 Agent。

## 4. 严格串行调度规则

### 4.1 任务选择

调度器只在当前项目没有活动任务时选择队首任务。队首任务必须同时满足：

- Task 领域状态为 `todo` 或 `action_required`，且对应编排 Job 状态为 `queued` 或 `changes_requested`；
- 前置任务对应的编排 Job 均已记录有效的 `integration_sha`；
- 未被取消、暂停或策略阻止；
- 项目 `dev` 工作树干净且不存在未完成 Git 操作；
- 当前调度者持有项目 lease 与 fencing token。

创建或更新任务依赖时，系统校验依赖图无环；若要求严格队列顺序，还必须校验前置任务的队列位置不晚于后置任务。

`Task` 使用 `todo`、`running`、`awaiting_review`、`action_required`、`done` 描述任务和 Agent Run 的领域事实；旧数据中的 `cancelled` 仅作为兼容状态读取。`task_orchestration_jobs` 单独保存 `queued`、`preparing`、`checking` 等编排阶段；不得用编排阶段替换 Task 状态，避免破坏现有 API 和状态转换规则。

### 4.2 任务状态机

```text
queued
  -> preparing
  -> implementing
  -> checking
  -> reviewing
  -> fixing -> checking
  -> ready_to_integrate
  -> integrated_to_dev
  -> released_to_main

checking / reviewing 失败
  -> fixing

超过修复上限、环境异常、策略拒绝
  -> needs_human

用户要求修改已集成任务
  -> changes_requested -> preparing
```

`integrated_to_dev` 表示任务已经自动进入 `dev`，不是已经发布到 `main`。只有固定验收快照被用户合并进入 `main` 后，快照内任务才标记为 `released_to_main`。worktree 和任务分支的清理是 `git_task_records` 的资源生命周期，不是任务状态；清理后仍必须保留任务的集成与发布审计记录。

### 4.3 每项任务的 Git 流程

1. 获取项目串行 lease，记录当前 `dev` 的 `base_dev_sha`。
2. 从 `base_dev_sha`（主工作区的 `main` 分支提交）创建独立 `git worktree`。任务在隔离 worktree 中实施，因此主工作区存在未提交/未跟踪改动不阻止首次派发；但主工作区必须在并入 `main` 前清理干净。
3. 从该 SHA 创建 `task/<task-id>-<slug>` 任务分支（工作区已在上一步创建）。实施开始前不在主工作区或 worktree 上运行基线验证，避免用尚不存在的改动阻塞任务派发。
4. Agent 只允许在该 worktree 中实施；TaskRun 保存分支、worktree、基线 SHA、Agent、Runner、Prompt 和策略快照。
5. 完成验证闭环后，自动创建任务提交。提交信息必须携带稳定标识，例如 `Task: <task-id>`。
6. 合并前再次确认 `dev` HEAD 仍等于 `base_dev_sha`。不相等时停止并转人工处理，禁止静默把任务合并到变化后的基线。
7. 严格串行时优先执行 `git merge --ff-only task/<task-id>-<slug>` 到 `dev`；不能 fast-forward 即视为异常，不自动解决冲突。
8. 在 `dev` 上执行集成检查。通过后记录 `integration_sha`，清理 worktree；任务分支可按保留策略归档或删除。

调度器不得 force push、改写 `dev` 历史、自动 push/merge `main`。发现已经集成的任务有问题时，创建修复任务或显式 revert 提交；不以重置共享分支修复问题。

## 5. 质量验证闭环

### 5.1 原则

模型说“已完成”或“没有问题”不是质量门禁。完成的定义是：预定义的自动检查成功、验收条件得到覆盖、独立审查没有阻塞问题，并且最终集成状态可复现。

实施和审查必须使用不同的 Agent 会话。审查 Agent 不继承实施过程的主观判断，只接收任务说明、验收条件、修改 diff、文件列表、检查输出和项目规范。

### 5.2 每轮验证流程

```text
实施 / 修复 Agent
  -> 格式化与静态检查
  -> 类型检查
  -> 受影响模块测试
  -> 全量测试或项目规定的回归测试
  -> 构建
  -> git diff --check 与策略检查
  -> 独立审查 Agent
       -> pass：允许集成
       -> fail：输出阻塞问题，回到修复
```

每个项目的命令由项目配置明确指定，例如 Go 项目可包含 `go test ./...`，前端项目可包含类型检查、Lint、测试和构建。调度器不得让模型临时猜测“应该运行什么”。

### 5.3 独立审查契约

审查输出必须是结构化结果，至少包含：

```json
{
  "verdict": "pass | fail",
  "blockingFindings": [],
  "nonBlockingFindings": [],
  "acceptanceCoverage": [],
  "testGaps": [],
  "reviewedCommit": "<sha>"
}
```

任何 `blockingFindings`、未解释的检查失败、未覆盖的关键验收条件、策略违规或审查提交 SHA 与当前 HEAD 不一致，都会阻止自动集成。

### 5.4 测试与验收条件

审查不能只看“测试全绿”，必须逐条追问：

- 每条验收条件是否有对应实现？
- 每条可自动验证的验收条件是否有测试或明确检查命令？
- 是否覆盖正常路径、边界输入、失败路径和回归风险？
- 是否修改了不在任务允许范围内的文件？
- 是否引入密钥、危险命令、权限变化、数据删除或迁移？

无法自动验证的验收项必须明确标记为人工验收项，不能被模型默认为通过。

### 5.5 收敛与升级

不能无限循环修复。默认最多执行三轮“修复 -> 检查 -> 独立审查”。以下情况转为 `needs_human`：

- 同类阻塞问题重复两轮；
- 测试不稳定或无法在干净环境复现；
- 需要改变任务范围、架构、权限、依赖版本策略或数据迁移策略；
- 涉及安全、密钥、删除、部署或不可逆外部副作用；
- Agent 达到时长、费用或工具调用预算。

自动化能提高发现问题的概率和一致性，但不能证明代码绝对无误。用户对 `release` 快照的验收仍然是最终质量关口。

## 6. dev 集成与 main 发布

### 6.1 自动集成 dev

任务分支通过验证后，调度器将其合并到 `dev`，再以 `dev` 的实际提交执行一次集成检查。原因是任务分支上的绿灯只能证明候选改动可用；`dev` 上的绿灯才证明当前累积集成状态可用。

集成检查失败时：保留 `dev` 和任务记录，立即冻结项目队列并标记 `needs_human`。不得继续提交后续任务，也不得把失败任务悄悄标记为通过。只有用户明确选择“修复当前集成”或“revert 当前集成”后，调度器才创建对应的最高优先级修复 Job；冻结期间，除该 Job 外的所有队列任务均不可领取。修复或 revert 在 `dev` 上通过完整集成检查后，用户才能解除冻结。

### 6.2 固定验收快照

用户不能对一个持续变化的 `dev` 分支做有效验收。发起验收时，系统从指定 `dev` SHA 创建不可变分支：

```text
release/<short-dev-sha>
```

验收页面、测试报告、变更清单和批准决定都绑定该 SHA。用户确认后自行将该固定快照合并到 `main`。调度器验证 `main` 已包含该快照的改动后，将快照中的任务标记为 `released_to_main`。

队列可以在 `dev` 上继续严格串行运行新任务；但这些新提交不属于已经发起验收的 release 快照，不能被用户意外带入 `main`。

### 6.2.1 main 与 dev 的祖先关系

发布优先使用 `git merge --ff-only release/<dev-sha>` 将固定快照推进 `main`，这样 `main` 保持为 `dev` 的已验收祖先。若用户以其他方式修改了 `main`，或 `main` 已不是 release 快照的祖先，调度器必须冻结 `dev` 自动集成，并创建显式的“main 回灌 dev”修复 Job：由用户确认合并策略后，将最新 `main` 合入新的 task/reconciliation 分支，执行完整验证闭环，再 fast-forward 集成至 `dev` 并创建新的 release 快照。调度器不得静默 rebase、自动解决冲突或重写 `dev` 历史。

### 6.3 用户发现问题

用户在 `dev` 或 release 快照验收中发现问题时，创建或重新打开带有原任务关联的修复任务：

- 修复任务从当前 `dev` 创建新 task 分支；
- 修复后走完整验证闭环；
- 自动集成回 `dev`；
- 原 release 快照标记为 `superseded`，重新创建新的固定快照。

对于明确需要撤出的问题提交，可由受权用户创建显式 revert 任务；调度器不自行重写或强推共享历史。

## 7. 持久化、幂等与恢复

需要新增或明确以下持久化事实：

| 记录 | 用途 |
| --- | --- |
| `task_orchestration_jobs` | 当前调度阶段、队列位置、重试回合、活动 lease、错误与恢复点。 |
| `task_execution_intents` | 一次逻辑下发至多创建一个 Agent Run，防止重启或重试重复执行。 |
| `git_task_records` | 基线 SHA、任务分支、worktree、任务提交、dev 集成提交、release 快照。 |
| `verification_runs` | 每个命令、退出码、日志工件、审查结论、被检查 SHA、开始和结束时间。 |
| `task_events` | 所有状态变化、用户决定、失败和恢复操作的审计事件。 |

数据库写入和外部副作用之间使用 outbox：在同一事务中写入状态、审计和待执行命令；Dispatcher 再创建 worktree、启动 Runner、执行检查或 Git 合并。命令携带稳定 idempotency key，重复投递只恢复已有操作，不能创建第二个 Agent、第二个 worktree 或第二次合并。

项目 lease 使用单调递增的 fencing token。只有持有当前 token 的调度器/Runner 可以写入任务状态、执行 Git 操作和提交结果；过期执行者必须停止，结果一律拒绝。

服务重启恢复规则：

1. 查找持有 lease 的未完成 job；验证持有者是否仍存活。
2. 检查 worktree、分支、任务提交和 `dev` SHA 的真实 Git 状态。
3. 若外部操作已成功但数据库尚未记录，按稳定操作 ID 补写状态；若未成功则恢复到可重试阶段。
4. 不根据内存状态重新执行 Agent；先查询执行意图和既有 TaskRun。
5. 任何无法确定的 Git 状态转 `needs_human`，禁止猜测性合并或删除。

## 8. 权限与操作限制

- `main`、`dev` 设为受保护分支；仅用户具备 `main` 合并权限。
- 调度器仅允许创建 task/release 分支、管理受控 worktree、向 `dev` 执行 fast-forward 集成。
- 禁止自动执行 `git push --force`、自动合并 `main`、删除远端分支、部署、数据迁移或访问新密钥。
- Task 的允许路径、Runner、Agent、检查命令、预算、超时和风险等级在开始前固化为不可变策略快照。
- Prompt、Issue、Git diff、测试日志和 Agent 输出均为不可信输入，不能改变权限、策略或命令白名单。

## 9. 实施阶段

### 阶段 1：单通道基础

- 明确任务队列顺序、前置依赖校验、单项目 lease 与 fencing token。
- 分离现有 Task 领域状态和 `task_orchestration_jobs` 编排状态；增加 `dev` 基线检查与冻结原因记录。
- 新增 job、执行意图、Git 记录、验证记录和 outbox。
- 实现从 `dev` 创建 task 分支/worktree、运行现有 `dispatchTaskByID`、保存 Run/TaskRun。
- 服务重启后可恢复 job，且不会重复下发 Agent。

验收：同一项目不会同时有两个活动任务；重启后不会产生重复 Run 或重复 worktree。

### 阶段 2：验证闭环

- 将项目检查命令配置化并保存原始输出为工件。
- 实现实施、检查、独立审查、修复回合和最大轮次限制。
- 建立验收条件覆盖报告与 `needs_human` 升级机制。

验收：模型一次自检不能绕过门禁；存在阻塞审查问题或检查失败时，任务不能进入 `ready_to_integrate`。

### 阶段 3：dev 自动集成

- 校验 `base_dev_sha`，以 `--ff-only` 集成 task 分支。
- 在 `dev` 执行集成检查；失败即冻结队列。
- 只允许当前集成的修复/revert Job 在冻结期间运行；补齐 `main -> dev` 回灌的人工确认流程。
- 清理 worktree，保留审计和任务提交映射。

验收：每个后续任务都从最新 `dev` 创建；任何异常基线或合并冲突都不会自动处理。

### 阶段 4：release 验收与 main 发布

- 从固定 `dev` SHA 创建 release 快照。
- 展示快照的任务、提交、diff、检查报告和人工验收项。
- 用户手动合并 release 到 `main`；系统验证后更新任务发布状态。

验收：`main` 永远不包含未验收的 `dev` 后续提交；用户验收针对固定 SHA，而不是移动中的分支。

## 10. 后续演进条件

只有在单通道方案稳定后，且实际出现以下需求时，才评估 Temporal 或其他工作流系统：

- 多台机器/多个 Runner 的长时间任务恢复；
- 多项目并行、定时执行、复杂补偿或跨服务人工等待；
- 工作流历史、可视化和运营告警需要独立扩展；
- 单体数据库队列的恢复与可观测性已成为维护负担。

即使未来引入 Temporal，Git 分支策略、质量门禁、验收快照、执行意图和用户对 `main` 的控制权仍保持不变。Temporal 只替换调度实现，不改变这些业务安全边界。
