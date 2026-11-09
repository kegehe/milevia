# 内置 CLI 能力补齐实施记录

实施日期：2026-11-06。对应调研：[57-内置CLI能力接入差距调研.md](57-内置CLI能力接入差距调研.md)。

本轮按已确认范围实施：普通对话核心链路、附件消息、Skill / 插件发现。所有修改留在工作区，未提交、发布、升级 CLI，也未发起真实模型或付费调用。工作区同时存在任务托管与工作区保护的其他修改，本文只说明本轮接入工作。

## 1. 核心链路

| 能力 | 实施结果 | 边界 |
| --- | --- | --- |
| CodeBuddy / Qoder 流式事件 | 转发初始化、工具调用、工具结果、用量和父工具关联；复用 partial 解析；保留早于首次发送到达的 init | 使用脱敏后的事件；图片 base64 不进入持久化事件 |
| CodeBuddy 命令发现 | 从真实运行的 init 读取独立命令目录，前端初始化后刷新 | 首次运行之前可能为空，不借用 Claude 命令表 |
| CodeBuddy 权限映射 | `workspace_write` 使用 `acceptEdits`，`full_control` 使用跳过权限；前者要求 CLI 至少 2.177.0 | 无头模式下未接入平台审批的原生请求会被拒绝。审批模式不等同于操作系统沙箱，未证明 CLI 工作区隔离 |
| 推理强度 | Claude 增加 xhigh / max；CodeBuddy 接入 effort；Qoder 接入 reasoning-effort；OpenCode 接入 variant，支持保留大小写的自定义值 | 选项表示参数映射，具体模型、账号及 CLI 版本是否接受由 CLI 决定；没有扩大任务托管适配器的能力声明 |
| 模型目录 | OpenCode / Qoder 在会话目标环境运行原生命令，解析可识别的目录 | 超时或输出不识别时保留自填；不把登录报错或 URL 当成模型。CodeBuddy / Cursor 仍保留自填 |
| OpenCode 用量 | 普通 parser 转发 step_finish；按 step ID 去重累计 token、缓存及费用，最后一跳作为上下文快照 | 不把缺失 token 统计为已知的零；其他 CLI 的缺失 result / turn 用量也保留“不可用”状态 |
| 手机设置 | 模型页支持推理强度与自定义 variant，经现有 RPC 复用后端校验 | 无需新增云端命令类型 |

主要实现：`stream_json_session.go`、`conversation_models.go`、`native_model_catalog.go`、`usage.go`、各 CLI runner，以及桌面 / 手机会话页。

## 2. 附件消息

- 消息新增结构化 `attachments`，数据库兼容迁移；消息历史、幂等重放、远程命令与快照、回退分支、上下文重建均保留附件路径。
- 桌面和手机共用上传 / 工作区文件引用组件；支持仅附件发送、桌面粘贴文件、预约发送、失败恢复与会话草稿隔离。上传部分失败时保留成功文件；切换会话取消未完成上传。
- 文件请求带 `conversationId`，服务端在工作区准入之后校验实际工作区。切换工作区成功后清空附件引用；上传与预约发送期间禁止切换，避免同名路径误指向另一工作区。
- 服务端限定工作区相对路径、最多 8 个、单个 10 MiB、合计 20 MiB；拒绝目录、越界路径及指向工作区外的符号链接。MIME / 大小由目标文件系统提供，执行前再次读取并核对。
- 幂等键比较规范化附件路径，不信任客户端 MIME / 大小。已接受的消息即使文件后来删除，重试也返回原消息；新消息引用丢失文件会明确失败。
- Windows 批处理 CLI 入口拒绝附件名中的二次 shell 解析字符，SSH / WSL 使用各自的参数引用方式。

| CLI | 本轮输入映射 |
| --- | --- |
| Claude / CodeBuddy | 不超过 5 MiB 的 PNG / JPEG / GIF / WebP 转为原生图片内容块，其他文件使用工作区路径引用 |
| Codex | 图片通过 `--image`，其他文件通过路径引用；包含本机、WSL、SSH 参数链路 |
| OpenCode | 通过 `--file`；包含本机、WSL、SSH 参数链路 |
| Qoder / Cursor | 通过工作区 `@路径` 引用，并记录降级说明；未将其宣称为原生图片输入 |

手机上传沿用现有中继限制，单文件 256 KiB；可引用工作区内已有的大文件，仍受消息附件总量限制。上传文件保存在 `.milevia-attachments/<随机目录>/`，可在文件页管理，不自动删除。回退 / 上下文重建保留历史路径引用，不会重新发送每张历史图片的 base64；文件内容仍遵循已有工作区与快照规则，不承诺不可变的历史附件存档。

## 3. Skill / 插件发现

- 六种 CLI 的原生及兼容 Skill 目录集中到共享来源表，本机 / WSL / SSH 使用同一套目录定义；本机支持 `CODEX_HOME`、OpenCode 的 `XDG_CONFIG_HOME`。
- 同名来源按用户 < 插件 < 项目覆盖；原生目录和兼容目录均保留 CLI 归属。本机 Skill 文件最多读取 128 KiB，插件 manifest 扫描限制条目、文件大小和时间；SSH 沿用现有远程扫描机制。
- 新增项目插件目录接口；扫描支持的 manifest，读取 OpenCode JSONC 的 `plugin` 配置项并去重。不会返回 provider 配置或凭据；带 URL / 查询参数等可能暴露凭据的配置项不进入展示目录。
- 桌面 Skill 面板展示插件发现状态；Skill 与插件查询独立处理，插件接口失败不清空已有 Skill。

这是发现与展示功能。`discovered` / `configured` 不代表插件已启用，未实现插件安装、更新、卸载、启停。SSH 暂不扫描插件 manifest，接口会明确说明；SSH Skill 发现仍可用。手机沿用快照中的 Skill 列表，未增加独立插件管理页。目录规则的支持不等于已对所有 CLI / 平台组合做过真机验收。

## 4. 验证

以下验证不调用真实模型：

| 检查 | 结果 |
| --- | --- |
| Web 全量 `pnpm test` | 1,244 项通过 |
| Web `pnpm build` | TypeScript 与 Vite 构建通过；保留已有大包体积提示 |
| 后端针对性回归 | CLI 集成、流式事件、用量、模型、Skill、远程快照与回退分支通过 |
| 后端扩大回归 | 未通过：Cursor 远端 fixture 收到工作区保护 409，与旧 400 断言冲突；整套运行于 600 秒达到总超时，未完成所有用例 |
| 浏览器 `node scripts/verify-message-attachments.mjs` | 桌面 / 手机共享组件：部分上传失败、成功文件保留、发送 busy、路径引用、切换取消通过；完整 ConversationPage：上传 / 引用带会话 ID、切换工作区清空附件、仅附件 POST、历史展示通过 |
| `git diff --check` | 通过 |

后端针对性回归命令（在 `apps/control-server` 执行，使用下述内存限制）：

```powershell
go test -p 1 ./internal/app -run '^(TestCLI|TestStreamJSON|Test.*Usage|TestSupervisionOpenCode|TestRemoteSnapshot|TestDiscover|TestScan|TestParseRemote|TestForkConversation)' -count=1
go test -p 1 ./internal/app -run '^(TestCLI|TestConversationModel|TestReasoning|TestStreamJSON|TestForkConversation)' -count=1
```

后端扩大回归命令（在 `apps/control-server` 执行）：

```powershell
$env:GOMEMLIMIT='512MiB'
$env:GOMAXPROCS='2'
$env:GOGC='30'
go test -p 1 ./internal/app -skip 'Live|POC|InstalledCLI|TestRecreatedStreamingSessionResumesPersistedNativeSession|TestRemoteTaskDispatchHonorsRequestedConversation' -count=1 -timeout 10m
```

扩大回归排除真实 CLI / Live / POC 用例，以及此前已观察到的两条环境 / fixture 失败，不能称为后端全量无条件通过：

1. `TestRecreatedStreamingSessionResumesPersistedNativeSession` 使用假 runner `runner`，被工作区已有的严格容器核对保护拒绝。
2. `TestRemoteTaskDispatchHonorsRequestedConversation` 的 fixture 硬编码 `wsl-local`，当前机器真实 WSL 未就绪。

没有通过放宽工作区保护来掩盖上述失败。后端测试日志保存在系统临时目录 `milevia-cli-go-tests.log`、`milevia-cli-targeted-tests.log`；前端日志为 `milevia-cli-web-tests.log`、`milevia-cli-build.log`。浏览器截图同样位于系统临时目录。

本次扩大到整个 `internal/app` 后，还观察到 `TestCursorConversationOnlyOnLocalRunner`：远端 Cursor 消息仍被拒绝，但现有工作区身份核对先返回 409（`target execution identity conflicts with durable facts`），旧断言要求 400。未改动保护或放宽测试断言。整套最终在 600 秒触发总超时，当时正在运行 `TestAppliedToBranchTaskStaysReviewableAndRedispatched`；它仅执行了约 13 秒，不能据此判定该单个用例挂死。日志栈还显示 WSL 探测等待。这次扩大回归不作为通过证据。

单独运行 `go test -p 1 ./internal/app -run '^TestCursorConversationOnlyOnLocalRunner$' -count=1 -timeout 60s` 也重现相同 409 / 400 差异（约 1.8 秒），详见临时目录 `milevia-cli-cursor-recheck.log`。本轮没有修改该用例，亦未完成它与工作区保护规则之间的基线修复。

## 5. 后续独立范围

以下属于调研中的更大协议 / 产品能力，本轮没有声称补完：Codex App Server 全面切换、各 CLI 原生审批 / 提问协议、ACP、原生会话导入、会话主 Agent 选择、完整插件生命周期管理、CodeBuddy / Qoder / Cursor 普通会话新增 WSL / SSH 支持，以及六种 CLI 的统一托管凭据路由。这些需要独立设计与真实 CLI / 环境验收。

## 6. 二次代码审查与修正

在核心链路、附件消息、Skill / 插件发现的本轮改动中，进一步修正了以下实际问题：

| 问题 | 修正 |
| --- | --- |
| 手机乐观消息使用 Windows / 点路径，服务端返回规范路径，可能留下重复气泡 | 附件草稿、合并和消息匹配共用路径规范化；保留绝对路径前缀与 `..`，交由服务端拒绝 |
| 发送失败、取消预约或恢复旧草稿时覆盖新附件，合并超过 8 个时静默丢文件 | 合并去重并保留全部引用；超限由发送校验拒绝，用户可自行移除；损坏的本地草稿做防御解析 |
| 工作区切换的迟到回包可能清掉另一会话的附件 | 清理操作只更新归属会话的存储，当前视图另做会话归属检查 |
| 预约自动发送清空后来新写的正文和附件 | 自动发送使用预约快照；最终失败或取消时合并恢复；手机取消同时恢复正文、Skill、引用和附件 |
| 停止纯附件消息后没有恢复引用，纯附件消息复制为空 | 按 run 保存并恢复附件，与新草稿合并；复制与展示复用含路径的消息正文 |
| 附件路径输入可在忙碌状态按 Enter 重复提交 | 增加同步 busy 保护并禁用输入；异步读取使用该操作自己的取消信号 |
| 普通文件读取入口可能打开 FIFO / 设备等特殊文件，SVG 可能误传原生图片参数 | 本地 / SFTP 在 Stat 和 OpenRead 检查普通文件；原生图片限 PNG / JPEG / GIF / WebP；图片编码再次核对实际读取大小；Skill / manifest 同样跳过特殊文件 |
| 大图片回显超过共享流缓冲区；读取异常或长 stderr 可能让回合无法结束 | 协议行上限改为 64 MiB；读取失败终结活跃及排队回合并停止进程；stderr 持续排空 |
| 模型目录探测可能选择与执行不同的环境，且本机探测输出无内存上限 | 复用执行环境的 runner 选择；本机 stdout / stderr 各限制 1 MiB 并持续排空；保留带点的合法 provider ID |
| result 到达前的真实上下文用量被标为不可用 | 将已收到的上下文 token 纳入可用性判断，继续区分缺失统计和已知的零 |
| 自定义推理强度在下拉框中显示为默认值；桌面允许并发切换请求 | 桌面 / 手机保留当前自定义选项；桌面切换期间禁用重复操作，忽略过期目录视图的回包 |

二次验证命令（分别在 `apps/web`、`apps/control-server` 执行）：

```powershell
$env:NODE_OPTIONS='--max-old-space-size=4096'
$env:RAYON_NUM_THREADS='2'
pnpm exec tsx --test --test-concurrency=1 --test-reporter=tap "src/**/*.test.ts" "src/**/*.test.mjs"
pnpm build
node scripts/verify-message-attachments.mjs
```

```powershell
$env:GOMEMLIMIT='512MiB'
$env:GOMAXPROCS='2'
$env:GOGC='30'
go test -p 1 ./internal/app -run '^(TestCLI|TestStreamJSON|Test.*Usage|TestConversationModel|TestSetConversation|TestDiscover|TestScan|TestParseRemote|Test.*Filesystem|Test.*OpenRead|TestCappedWriter|TestRunProbe|TestRemoteSnapshot|TestForkConversation|TestReasoning)' -count=1 -timeout 180s
```

新增后端回归覆盖流读取异常的活跃 / 排队回合收尾、17 MiB 图片回显脱敏、长 stderr 排空、双管道超限探测、result 前上下文用量、模型目录执行环境、SVG 路径回退。新增前端回归覆盖附件路径匹配、损坏草稿、超限合并保留与仅附件复制。浏览器 fixture 不调用模型，覆盖桌面 / 手机共享附件组件以及完整 ConversationPage 的预约、停止恢复和自定义推理强度。

本轮不重复将整个 `internal/app` 运行作为通过证据，前述保护 / fixture 差异与整套超时限制仍保留。未做六种 CLI 的真实模型验收，也没有验证 Linux FIFO 或远端 SSH 真机行为。直接运行前端测试 / 构建曾出现 Node 进程异常和内存分配失败，验证使用上述串行测试、显式 Node 堆和 Rayon 线程限制；未为此修改项目构建配置。

二次日志位于系统临时目录：`milevia-cli-review-web-final.log`、`milevia-cli-review-build-final.log`、`milevia-cli-review-browser.log`、`milevia-cli-review-go-final.log`。

二次最终结果：前端 1,248 项测试全部通过；TypeScript / Vite 构建通过（保留大包体积提示）；上述后端针对性回归通过；浏览器四组场景通过，包括手机模型面板的自定义 variant 回显及桌面推理强度切换 busy 状态；`git diff --check` 通过。所有修正仍保留在工作区，未提交或发布。

## 7. 第三次代码审查与修正

本次继续沿附件接收、持久化、远程回执和消息操作检查，发现并修正了四处遗漏：

1. 手机端在处理明确失败的消息时，把该消息自身计入“其他待发消息”，使最后一条失败消息不能回填正文、Skill 和错误提示。现在按请求 ID 排除自身，再判断是否有其他请求。
2. 手机待发消息仅按正文与附件路径确认，旧历史可能误确认新的重复发送；快照截断正文时又可能无法确认。现在本地发送回包、消息历史、实时事件和远程快照统一提供 `clientRequestId`，手机优先按请求 ID 确认。新服务端对没有请求 ID 的消息也明确返回空串，避免把其他来源的相同消息当回执；无该字段的旧服务端继续使用原有内容匹配兼容逻辑，因此完整修复需要新的服务端协议。
3. 手机消息气泡虽显示附件，但复制、引用和填回输入框仍只取正文。复制和引用现在包含附件路径；填回输入框恢复结构化附件，并继续保留已有草稿，不自动发送。
4. 手机“清空草稿”未计入附件：仅附件时按钮不可用，清空其他内容时又遗留附件。现在仅附件也能清空，同时移除结构化引用；附件上传 / 引用忙碌期间禁用该操作，避免迟到回包重新添加文件。

新增三组消息确认与失败判定单测，覆盖重复正文 / 同一路径附件的请求区分、截断正文的确认、明确空请求 ID、旧协议兼容及失败消息自身的排除。后端 CLI 回归校验远程发送回包和快照保留请求 ID。

浏览器脚本扩展到实际 `MobileRemotePage`：使用隔离的测试缓存和内存云端 fixture，验证纯附件复制到系统剪贴板、引用、填回、清空；发送与旧历史相同的正文和附件后保留新的待发气泡；云端返回明确失败后恢复正文、附件和错误提示。原有桌面 / 手机共享附件组件、完整桌面对话页和手机模型面板用例也继续运行。该测试不连接真实云端，不调用真实模型。

本轮后端命令在上一轮范围上增加消息幂等与插话回归：

```powershell
go test -p 1 ./internal/app -run '^(TestCLI|TestStreamJSON|Test.*Usage|TestConversationModel|TestSetConversation|TestDiscover|TestScan|TestParseRemote|TestRemoteSnapshot|TestForkConversation|TestReasoning|Test.*Steer|Test.*ClientRequest)' -count=1 -timeout 180s
```

实际 CLI POC 测试需要显式环境开关，未启用。前述整套后端的保护断言冲突 / 超时及真机验收边界仍保留，不作为本次通过证据。本轮日志：系统临时目录 `milevia-cli-review3-go.log`、`milevia-cli-review3-web-final.log`、`milevia-cli-review3-build-final.log`、`milevia-cli-review3-browser-final.log`。

第三次最终结果：前端全量 1,251 项测试通过；TypeScript / Vite 构建通过（仍有大包提示）；上述后端针对性回归通过；五组浏览器场景全部通过；`git diff --check` 通过。所有修改仍在工作区，未提交、未发布。
