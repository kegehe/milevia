# 项目加载与 Claude 对话闭环

> 日期：2026-07-17  
> 目标：完成第一个真实可用闭环，从选择项目目录到在网页中与 Claude Code 连续对话并实时查看开发过程。

## 1. 功能目标

用户可以在网页中选择一个项目目录，将它登记为平台项目；进入项目后输入提示词，由该项目所属 Runner 启动 Claude Code，并在网页中实时显示 Claude Code 的消息、执行过程和最终回复。

本功能只建立“项目加载 + Claude 对话”的最小闭环，不在本阶段实现任务管理、Git worktree、分支、测试门禁、Codex CLI、部署或多人协作。

```text
选择 Runner
  -> 浏览目录
  -> 选择项目根目录
  -> 校验目录与 Claude Code
  -> 登记项目
  -> 进入项目对话页
  -> 输入提示词
  -> Runner 启动 Claude Code
  -> 网页实时显示过程与回复
  -> 继续下一轮对话
```

## 2. 路径与 Runner 规则

浏览器不能安全、稳定地直接获得任意本地绝对路径，也不能自行启动 WSL 或本机 CLI。因此目录选择必须由 Runner 提供，网页只展示 Runner 返回的目录列表和选择结果。

| 项目位置 | 选择方式 | 执行方式 |
| --- | --- | --- |
| WSL 项目 | 选择 WSL Runner 后浏览 Linux 目录，例如 `/home/user/projects`。 | WSL Runner 在同一 WSL 环境启动 Claude Code。 |
| Windows 本机项目 | 选择 Windows Runner 后浏览 Windows 目录，例如 `C:\Projects`。 | Windows Runner 在 Windows 环境启动 Claude Code。 |

首期当前机器先交付 WSL Runner 路径。Windows Runner 与相同页面、相同项目模型和相同 Claude 对话流程兼容，作为本功能闭环的下一执行环境，不应由 WSL Runner 直接操作 `\\wsl$` 或 Windows 项目目录。

项目登记后必须保存：项目名称、项目根路径、所属 Runner、环境类型、是否为 Git 仓库、默认分支摘要和 Claude Code 可用状态。项目路径只对其所属 Runner 有意义，控制核心和浏览器不直接使用该路径访问文件。

## 3. 页面与交互

### 3.1 项目列表页

用于展示已加载项目，并提供“加载项目”入口。

- 显示项目名称、所属环境、项目路径摘要、Claude 状态和最近对话时间；
- 点击项目进入项目对话页；
- 空状态中提供加载项目操作。

### 3.2 加载项目页/弹窗

用户依次完成：

1. 选择 Runner；
2. 浏览该 Runner 可访问的目录；
3. 选择一个目录作为项目根目录；
4. 查看目录校验结果；
5. 确认加载项目。

目录校验至少包含：目录存在、目录可读、目录是否为 Git 仓库、Git 基本状态、Claude Code 是否可执行。Git 结果在当前阶段只作为能力提示，非 Git 目录也可加载和对话；后续使用分支、worktree、提交或 PR 功能时，再要求 Git 仓库。

### 3.3 项目 Claude 对话页

对话页是本功能的核心页面。

| 区域 | 内容 |
| --- | --- |
| 项目栏 | 项目名称、所属 Runner、路径摘要、Claude Code 可用状态。 |
| 对话时间线 | 用户提示词、Claude 消息、正在执行状态、错误与最终回复。 |
| 过程面板 | Claude 产生的命令、命令输出和文件修改摘要。 |
| 输入区 | 多行提示词输入、发送、停止当前运行、开始新会话。 |

首期优先保证过程可读和会话连续，不需要在此阶段制作完整代码 diff 浏览器；文件修改摘要和完整原始过程记录足够支持排查。

## 4. Claude 对话运行模型

```text
Project
  -> Claude Conversation
    -> Claude Turn
      -> Claude Run
        -> Process Events
```

| 对象 | 含义 |
| --- | --- |
| Claude Conversation | 同一项目中可连续追问的一段 Claude 会话。 |
| Claude Turn | 用户发送的一条提示词及其对应回复。 |
| Claude Run | Runner 为某一轮对话启动的 Claude Code 进程。 |
| Process Event | Claude 消息、工具行为、命令输出、文件修改、完成或失败等过程记录。 |

> **实现说明**：实际代码中 **Turn 不作为独立实体/表落地**——一次消息发送直接对应一个 Run（`runs` 表），Conversation → Message → Run → Event 四层。Turn 的概念隐含在 Message 与 Run 的一一对应中。同会话续问通过 Claude Code 的 `--resume` 恢复同一会话。

同一 Conversation 后续发送提示词时，Runner 必须恢复同一个 Claude 会话，而不是每轮创建全新上下文。用户选择"开始新会话"后，才建立新的 Conversation。

Runner 使用 Claude Code 的非交互、结构化流式输出能力运行一次对话，并把输出转换为平台过程事件。网页只展示统一事件，不依赖终端颜色文本或某个 Claude CLI 版本的非结构化输出。

## 5. 状态设计

### 项目加载状态

```text
未选择 -> 浏览目录 -> 校验中 -> 可加载 -> 已加载
                         -> 校验失败
```

### Claude 对话状态

```text
空闲 -> 发送中 -> Claude 启动中 -> 流式回复中 -> 已完成
                                  -> 已停止 / 失败
```

页面必须明确显示当前状态。执行期间禁止同一 Conversation 并发发送第二条提示词；用户可以停止当前 Run，停止后保留已经收到的过程记录。

## 6. 过程事件与网页展示

Runner 返回的过程信息至少应能区分：

| 事件类别 | 网页表现 |
| --- | --- |
| 用户消息 | 对话时间线中的用户提示词。 |
| Claude 消息 | 流式显示 Claude 回复与最终总结。 |
| 工具/命令开始 | 过程面板显示正在进行的操作。 |
| 工具/命令输出 | 可展开查看的输出内容。 |
| 文件修改 | 显示修改文件的路径和修改状态。 |
| 完成 | 标记本轮完成并允许继续发送。 |
| 失败/停止 | 显示可理解原因并允许重试或继续追问。 |

所有事件按 Conversation、Turn 和 Run 关联保存。用户刷新页面或稍后回到项目时，必须先看到历史记录，再接收当前运行的新事件。

## 7. 安全与运行边界

- Claude Code 仅能在所选项目根目录及平台明确允许的范围内运行；
- 项目目录选择、CLI 认证和命令执行都在所属 Runner 内完成；
- 网页不能传递任意 shell 命令，也不能请求 Runner 访问任意未选择路径；
- 首期不使用跳过 Claude Code 权限检查的危险参数；
- Claude 输出、命令输出和错误信息写入页面前需要按后续安全规则支持脱敏；
- Claude Code 未安装、未登录、版本不支持结构化输出时，项目不能进入可对话状态，页面需给出明确诊断。

## 8. 验收条件

完成以下场景，才视为本功能闭环完成：

1. 用户可在网页选择 WSL Runner，并浏览其允许范围内的目录。
2. 用户选择一个 Git 项目目录，平台显示校验结果并成功创建项目记录。
3. 用户进入该项目的 Claude 对话页，看到 Claude Code 的可用状态。
4. 用户输入一条提示词并发送，Runner 在该项目目录中启动 Claude Code。
5. 网页实时显示 Claude 消息和过程信息，且不会因刷新页面丢失已产生记录。
6. Claude 完成后，用户能看到最终回复，并在同一 Conversation 中继续发送下一条提示词。
7. 用户可停止运行；Claude 启动失败、认证失败、目录无权限等情况均有可理解的失败状态。
8. 在 Windows Runner 可用后，使用同一页面流程加载 Windows 本机 Git 项目并完成一次 Claude 对话，不新增第二套页面或业务模型。

## 9. 实施任务清单

### 基础能力

- [x] 定义 Project、Runner、Claude Conversation、Claude Turn、Claude Run 和 Process Event 的领域对象。
- [x] 定义项目加载、对话和运行的状态流转。
- [x] 定义浏览器 API 与 Runner 通信契约中的目录、项目和 Claude 对话对象。
- [x] 定义 Claude Code 结构化流式输出到 Process Event 的映射规则。

### Runner 能力

- [x] 实现 WSL Runner 的 Runner 注册、能力上报和 Claude Code 探测。
- [x] 实现 Runner 目录浏览与目录校验，限制可浏览根目录。
- [x] 实现项目路径、Git 信息和 Claude 可用状态探测。
- [x] 实现 Claude Conversation 的创建、恢复、新建与停止。
- [x] 实现 Claude 结构化事件采集、顺序上报和失败信息归类。

### Control Server 能力

- [x] 实现项目加载、项目查询和项目状态管理。
- [x] 实现 Claude Conversation、Turn、Run 与事件记录管理。
- [x] 实现向 Runner 发起 Claude 运行、停止和恢复的编排能力。
- [x] 实现历史事件读取和实时事件转发。

### Web 页面能力

- [x] 实现项目列表和空状态。
- [x] 实现 Runner 选择、目录浏览、校验结果和项目加载页面。
- [x] 实现项目 Claude 对话页、过程面板和状态提示。
- [x] 实现流式消息显示、运行停止、继续对话和开始新会话。
- [x] 实现刷新页面后恢复项目与对话历史。

### 验证任务

- [x] 用一个 WSL Git 项目完成加载、首次对话、继续对话和停止运行测试。
- [x] 验证目录不存在、无权限、非 Git 项目、Claude 未安装和 Claude 未登录的错误提示。
- [x] 验证浏览器刷新和短暂断连后，历史过程与当前运行状态一致。
- [x] Windows Runner 实现后，用 Windows 本机 Git 项目复用相同流程进行验证。

## 10. 本阶段不做的内容

> **更新**：以下为 2026-07 初版的"不做"清单。其中多项后续已实现，标注如下。

- 通用任务看板和任务分解； ✅ 已实现（`docs/07`、TaskBoardPage）
- Git worktree、分支、提交和合并请求； ⚠️ worktree 未实现；分支、提交、push、fetch **已实现**（`docs/08`、GitWorkbenchPage）
- Codex CLI 和其他 AI 工具； ✅ Codex CLI **已实现**（`docs/13`、`codex_runner.go`）
- 完整 diff、测试门禁、部署和通知； ⚠️ diff **已实现**；测试门禁/部署未实现；通知 **已实现**（`docs/15`）
- 多人协作、权限管理和远程 Runner 管理界面。 ⚠️ 多人协作/权限未实现；远程 Runner（SSH）**已实现**（`docs/11`、SSHManagerPage）

这些功能后续应建立在本次项目、Conversation、Run、事件和 Runner 边界之上。
