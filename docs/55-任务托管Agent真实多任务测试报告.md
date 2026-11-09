# 任务托管 Agent 真实多任务测试报告

测试日期：2026-11-03。支持范围：Windows 服务端、本机项目、Claude Code CLI 2.1.308。本文记录真实运行发现的问题、修复和最终结果；不以确定性夹具代替真实模型和执行器。

> 2026-11-04的通用适配实施重测见[§7](#7-通用适配实施后的重测2026-11-04)。下文§1—§6保留2026-11-03历史语义，不能替代后续修改的验证结果。

## 1. 测试方式

新增 `apps/control-server/internal/app/supervision_live_multitask_windows_test.go`，显式设置 `MILEVIA_RUN_SUPERVISION_MULTITASK_LIVE=1` 才运行，默认不会调用模型。

- 使用隔离临时SQLite和真实项目目录，避免修改用户现有任务和业务源码；项目登记使用测试初始化，创建任务、依赖、托管草稿和启动均调用正式HTTP路由，通过真实TCP服务访问。
- 使用正式后台Worker、领域状态机、进程登记、CLI适配、文件应用、命令/浏览器执行器和最终验收事务。调度、实施和独立审查全部启动已安装的真实Claude CLI，没有替换模型输出、执行器或验收证据。
- 每批一次启动后不再人工选择动作、不手动恢复、不注入通过结果。任务选择顺序故意与依赖顺序不同。Worker补查间隔缩短为500毫秒（生产30秒），仅加快检查，不替代任何动作或门禁。
- 前三轮每批预算120个Agent回合、25分钟有效时长、单轮90秒。第三轮最终验收连续三次达到单轮上限，最终轮将测试预算调整为120回合、30分钟总时长、单轮3分钟；保持重试上限不变。所有角色与格式纠正共同计数，不返还已用额度。
- 项目预置有缺陷的实现和受保护的行为断言。启动前由测试观察者运行各Node原检查并确认确实失败；终态后另起Node独立执行检查，并逐字核对检查文件、服务器和AGENTS规则均未被修改。观察者输出不注入Agent验收证据。
- 成功项必须同时具备：实际实施和独立审查的持久PID/进程出生标记、Run completed、进程静默、真实命令或浏览器检查、同一最终源码快照/要求/计划的通过检查与独立审查及acceptance、Task done。
- 浏览器使用真实独立Node服务、Playwright/Chromium、新浏览器上下文和实际断言，保存逐步骤报告与最终截图；不是生成截图冒充交互。
- 终态额外检查任务认领、目录准入和受管执行全部释放或静默，并分页读取各角色输入/输出过程记录。

HTTP测试覆盖后台真实闭环；另外的页面浏览器脚本覆盖真实渲染和操作，但其API为夹具。这两类证据分别报告，不能把后者描述为生产页面到真实CLI的整链点击测试。

## 2. 真实任务与最终结果

最终轮两组均符合预期，整条`go test -race`命令退出码0，无DATA RACE：

| 任务 | 实际验证 | 本轮真实检查次数 | 最终独立审查/验收 | 原任务结果 |
| --- | --- | --- | --- | --- |
| 加法（add，69e69aa8） | Node原始5项断言，覆盖正数、负数、零、小数 | 3次，全部通过 | 最终快照各1份通过记录 | done |
| 订单总价（total，a023e5a6） | 依赖加法；Node原始4项断言，覆盖空订单、普通订单、零数量、小数 | 1次，通过 | 最终快照各1份通过记录；另有1次计划修订审查，不代替成果审查 | done |
| 标题规范化（slug，aeaa405d） | Node原始4项断言，覆盖空白、大小写、已有连字符、空串 | 2次，全部通过 | 最终快照各1份通过记录 | done |
| 计数器界面（browser，00091f6e） | 真实Chromium共8步，验证初始0→点击1→再次点击2→重置0 | 2次，全部通过，均有报告和截图 | 最终快照各1份通过记录 | done |
| 外部签名数据（blocked，343ca4e3） | 实际执行Node检查，缺少供应方数据，exitCode=1 | 1次，预期失败 | 无通过审查、无验收；保留unresolved | todo，未误标完成 |
| 正数判断（positive，0fcd0de8） | Node原始5项断言，覆盖正数、零、负数、小数 | 1次，通过 | 最终快照各1份通过记录 | done |

四任务批次`completed`：51个真实CLI回合，实际托管22分53秒，保留110条过程消息。两任务失败隔离批次`completed_with_issues`：11个真实CLI回合，实际托管2分50秒，保留24条过程消息。总计62个真实CLI回合、10次真实检查；所有受管执行最终quiescent，两批均通过认领与目录释放断言，无待回答问题。

独立读取导出数据再次核对：

- 四任务最终源码快照均为`ea14bc9a9050d82500a5afe2458c30c9c6a22bc1d8785f1902182e27f2e4512c`；失败隔离批次为`4f0843ede884ea2d3aeb8c9a3cc98cd906def6c1a5e2380ab5a1da184754d860`。
- 五个成功任务均满足“最终真实检查时间 < 最终独立审查时间 < 验收时间”，且版本与最终快照相符。
- 订单实施派发的实际调度输入中，加法明确为accepted/fresh；不是仅根据历史任务标题或模型自述放行。
- 每个成功任务都只实施1次，代码修复计数均为0；加法检查3次是跨快照复验，没有反复重写代码。
- 加法首个审查输出不是合法JSON，被拒绝；调度自行重新派发并通过，没有人工续跑。过早缺证据的验收也被门禁拦截，随后补齐真实复验和审查。
- 终态后的独立Node观察者验证加法、订单、标题、正数判断全部通过；外部数据检查仍真实失败。所有原始断言、server.cjs和AGENTS.md均逐字未变，未生成伪造签名数据。
- 两次浏览器验证的四个报告/截图文件均与登记SHA-256一致，逐步断言全部通过、console为空；已实际打开最终截图核对。
- 各角色运行均有持久输入，分页过程记录可读；最终轮另外导出完整messages、run links及持久调度输入。独立复核摘要保存在最终证据根目录`audit-summary.json`。

实际运行证实可以自动恢复部分中途错误，也说明吞吐成本仍需关注：这个小型四任务批次用了51个CLI回合和约23分钟，不能由此宣称大型任务集的效率或长时间无人值守稳定性已经达标。

## 3. 前三轮失败与修复

没有把失败轮次覆盖或记成成功：

| 轮次 | 四任务批次 | 两任务失败隔离批次 | 整条命令 |
| --- | --- | --- | --- |
| 第一轮 | 322.93秒后needs_input / invalid_scheduler_output，未完整通过 | 117.86秒，通过预期的completed_with_issues | 退出码1，失败 |
| 第二轮 | 442.74秒后needs_input / scheduler_execution_failed，未完整通过 | 130.49秒，通过预期的completed_with_issues | 退出码1，失败 |
| 第三轮 | 1193.31秒后结束；四项均实施并通过候选，最终验收连续三次调度超时后needs_input，未完整通过 | 176.99秒，通过预期的completed_with_issues | 退出码1，失败 |

确认并修复：

1. **同批任务归属不足。** 第一轮加法首审把标题任务的合法变更算成加法越界。调度补充解释后重审通过，但仅靠调度自述不够可靠。现在由服务端按实际实施工作项输入/输出快照计算变更归属，附带任务、工作项、当时范围和文件差异，纳入审查指纹；其他任务合法变更不算本任务越界，整批回归仍需检查。截断或未核验结果明确标记缺口。新增两任务实际文件应用回归覆盖此规则。
2. **浏览器动作结构不可靠。** 第一轮浏览器派发出现漏闭括号及引用历史contextId，初次输出及两次格式纠正均未成功。提示补充完整有效JSON示例、明确当前projectContext.id及实际截图能力；没有修补模型JSON、放宽版本检查或增加无限重试。第二轮已真实完成浏览器检查，仍需承认模型可能再次产生无效输出。
3. **Go超时错误未分类。** 第二轮单轮90秒到期，`context deadline exceeded`未被瞬时超时分类识别。补齐该分类，沿用已有退避、累计重试和资源核对。新增回归修复前确定失败，修复后验证启动新回合、消耗次数保留、退避、重试耗尽和用户暂停/停止保护；认证、协议、取消和未知进程不走自动重试。
4. **最终阶段被退避覆盖。** 第三轮已进入finalizing，超时后自动退避却固定恢复running。现在按失败工作项budget_class恢复原阶段，final保持finalizing；重试事件记录resumeState。扩展回归修复前确定失败，修复后覆盖running/finalizing与无控制/暂停/停止的六种组合，并验证新回合仍使用原预算类别。

实时HTTP可能读到执行收口与自动退避之间短暂的needs_input。真实测试允许后台维护5秒后再判定稳定阻塞；测试不调用恢复接口，也不替后台安排重试。

本轮生产修改集中在`supervision_roles.go`（可信归属、审查输入与指纹）、`supervision_scheduler_cli.go`（派发协议提示）、`supervision_runtime.go`（超时分类和原阶段恢复）；新增归属、超时退避回归及真实多任务实验。没有修改现有业务任务、放宽正式验收或调整生产默认预算。

## 4. 其他验证

| 检查 | 最终结果 |
| --- | --- |
| 全部确定性托管/目录准入/任务归属领域测试 | 161项顶层测试通过，0失败、0跳过，无DATA RACE；`go test -race`，285.200秒 |
| 现有应用包的全部非上述领域测试 | 1540项顶层测试通过、44项按平台/环境条件跳过，0失败；1164.819秒，命令退出码0 |
| 缺陷专项回归 | 归属与超时两项最终专项竞态测试通过；归属等7项较早专项也通过，未把重叠测试重复相加 |
| 前端全部测试 | 1240项通过，0失败、0取消、0跳过 |
| 前端生产构建 | 通过，保留既有大chunk提示 |
| 独立托管页面真实Chromium交互 | 桌面、手机均通过；覆盖创建/启动、各角色过程及分页、控制、补充要求、预算、证据、产物、差异和跨项目切换；手机无横向溢出。API使用夹具，保留既有transformWithEsbuild弃用提示 |
| 静态与命令包 | 最后阶段恢复修复后重新执行`go vet ./...`和`go test ./cmd/...`，通过；approval-helper无测试如实显示 |
| 平台编译 | `GOOS=linux CGO_ENABLED=0 go build ./internal/app`通过，仅验证编译，不代表开放Linux托管 |
| 格式与文本 | 新增/修改Go文件gofmt检查、工作区diff空白检查及未跟踪文本空白检查通过 |

领域最终日志：`milevia-supervision-multitask-verified-deterministic.jsonl`；现有应用日志：`milevia-supervision-multitask-existing-app.jsonl`；最终专项：`milevia-supervision-multitask-phase-fixed-cases.jsonl`；前端日志：`milevia-supervision-multitask-web-tests.txt`、`milevia-supervision-multitask-web-build.txt`，均位于系统临时目录。较早160项领域测试在最后超时修复之前通过，最终以补充该缺陷回归、覆盖最终阶段恢复后的161项结果为准。

## 5. 原始证据与复现

最终轮导出每批的初始文件与任务、启动前失败输出、最终详情、全部决策/工作项/角色任务/证据/事件/执行记录、最终工作区、浏览器报告/截图和分页过程对话；permit hash不导出。

系统临时目录保留：

- 第一轮：`milevia-supervision-real-multitask-20261103-161151`，日志`milevia-supervision-real-multitask-run.jsonl`。
- 第二轮：`milevia-supervision-real-multitask-fixed-20261103-162436`，日志`milevia-supervision-real-multitask-fixed.jsonl`。
- 第三轮：`milevia-supervision-real-multitask-final-20261103-163637`，日志`milevia-supervision-real-multitask-final.jsonl`（名称含final，实际未完整通过，不能据文件名认定成功）。
- 最终轮：`milevia-supervision-real-multitask-verified-20261103-170145`，日志`milevia-supervision-real-multitask-verified.jsonl`。

上述目录均在`$env:TEMP`下；临时目录可能被系统清理，需长期保留时复制整目录。每批子目录为`independent_dependency_browser`和`failure_isolation`，`process-*.json`是实际模型输入/输出历史，`observer-*.txt`是终态后的独立复验，`browser-artifacts/<executionId>/`是实际报告和最终截图。

在仓库根目录使用PowerShell复现：

```powershell
$env:MILEVIA_RUN_SUPERVISION_MULTITASK_LIVE = '1'
$env:MILEVIA_SUPERVISION_LIVE_ARTIFACT_DIR = Join-Path $env:TEMP ('milevia-supervision-live-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
Set-Location apps/control-server
go test -race ./internal/app -run '^TestSupervisionWorkflowInstalledCLIMultiTaskEndToEnd$' -v -count=1 -timeout=65m
```

需要已认证的Claude CLI、Node及项目提供的Playwright/Chromium。该命令实际调用模型、消耗现有CLI账号额度，结果受模型输出和外部服务状态影响。

## 6. 结论的适用范围

当前实现已经在上述支持范围内真实跑通“选择多个任务→自主规划/派发→实际修改文件→真实测试→独立审查→过期证据复验→正式完成/未解决收口→资源释放”的闭环。五项可完成任务全部done，故意缺少外部条件的一项正确保留未解决，没有拖住独立任务。

本轮发现并修复四处问题，前三轮失败如实保留；最终成功不能推导为所有场景均无缺陷，也不能把故意失败项写成成功。模型格式错误和响应耗时仍有波动，产品继续依靠有限纠正、退避、预算和严格证据门禁处理。

这些是隔离小项目中的真实执行，覆盖四任务混合批次及两任务失败隔离。没有验证大型业务仓库、几十/几百任务批次、长时间无人值守、Codex/WSL/SSH或其他平台；也没有覆盖生产页面直接点击启动CLI的全链过程。真实模型格式错误、网络超时仍可能发生，超过有限纠正/重试后会如实要求处理。Windows Job仅管理进程生命周期，不是文件或网络沙箱。

所有修改保留工作区，未暂存、提交、推送或发布。设计对应T53—T55；相关修复见 [设计审查记录 §13](./53-任务托管Agent设计审查记录.md#13-真实多任务运行发现的问题2026-11-03)。

## 7. 通用适配实施后的重测（2026-11-04）

兼容改造后的首轮真实六任务实验中，四任务场景在最终验收阶段失败，两任务失败隔离通过。随后诊断重测捕获到监控续租和存储核对的超时；取得证据后通过真实停止接口收口四任务场景，两任务失败隔离再次通过。诊断轮不能算四任务完成，也不能将两任务通过外推为整组通过。

已补具体取消原因持久化，并将活动心跳从重复预算/存储统计中分离；不放宽工具隔离、输出静默或验收门禁。真实实验导出增加普通过程事件及执行规格，保留实际CLI、输入/固定指令、目录与认证归属的核对依据。修复细节、前后轮次和验证记录见[实施记录 §40](./54-任务托管Agent实施记录.md#40-真实长会话诊断与执行归属补齐2026-11-04)。

轻量心跳修复后的重测沿用同一四任务与两任务场景、120次调用/30分钟活动/3分钟单轮预算及既有重试上限，在同一测试进程中顺序执行，未由观察者选择动作、手动恢复或修改证据。四任务最终completed，49次Agent调用、约28.1分钟活动；两任务completed_with_issues，12次调用，正数判断done、外部签名数据缺失项unresolved。四任务的最终检查、独立审查和验收均fresh；57次执行和两任务的14次执行全部quiescent，终态目录与任务认领释放核对通过。

该轮四任务仍发生一次续租超时并自动恢复，另有预算阶段收口，不能称为运行期间没有问题。继续检查导出的普通过程事件发现18093条高频thinking_tokens被托管sink持久化，已按普通会话规则修正；详见[实施记录 §41](./54-任务托管Agent实施记录.md#41-高频遥测阻塞控制连接修复2026-11-04)。该轮材料为临时目录 `milevia-supervision-adapters-final-live-20261104-113341`；其199项整组通过不能替代后续遥测修复的最终验证。

遥测修复后的新一轮仍使用相同断言、预算和真实CLI，没有用测试替身或降低门禁，结果如下：

| 场景 | 最终结果 |
| --- | --- |
| 四任务混合批次 | completed，48次Agent调用、约14.7分钟活动，59次执行均quiescent；四任务最终检查、独立审查、验收、done、任务认领和目录释放断言全部通过。 |
| 两任务失败隔离 | completed_with_issues，10次调用、约1.8分钟活动，12次执行均quiescent；独立任务done，外部签名数据缺失项有真实失败检查且unresolved。 |
| 高频遥测与监控 | 四任务441条普通过程事件、0条thinking_tokens；未出现续租或存储监测超时。3次修复前基线失败均是实际缺陷，不算传输故障。 |
| 同进程完整回归 | 198项确定性领域测试加真实规格、真实六任务两项顶层实验，共200项全部通过，0失败、0跳过、无DATA RACE；按精确名称核对无缺失，1319.818秒。 |

最终材料为临时目录 `milevia-supervision-telemetry-final-live-20261104-121329`，原始日志与名称清单为 `milevia-supervision-telemetry-final-all.jsonl`、`milevia-supervision-telemetry-final-all-names.txt`。单轮CLI规格实验实际核对Claude 2.1.308、输入/固定指令绑定及进程静默。页面与静态验证分层记录在实施记录，未用它们替代真实执行。

其他CLI和环境仍未通过这组真实验收，全部环境/CLI兼容尚未完成。上述实验均在隔离小项目中运行，不能宣称大型业务仓库或长期无人值守已通过验证。

## 8. Windows Codex 与 CodeBuddy 真实六任务验收（2026-11-04）

在同一套生产调度、实施、真实命令测试、独立审查及最终同快照验收门禁下，分别使用已登录的 Codex 0.166.0 与 CodeBuddy 2.177.0 跑完四任务混合批次和两任务失败隔离批次。观察者未代选动作、伪造证据或恢复会话；每个 CLI 均保留版本和工具隔离校验。

| CLI | 四任务混合批次 | 两任务失败隔离 | 整项测试 |
| --- | --- | --- | --- |
| Codex 0.166.0 | completed；43次真实模型调用，20分43秒；四项均done，最终有效真实检查、独立审查和验收各1份 | completed_with_issues；10次调用，5分6秒；独立任务done，缺外部签名数据项unresolved/todo | PASS，1554.62秒 |
| CodeBuddy 2.177.0 | completed；46次真实模型调用，21分10秒；四项均done，最终有效真实检查、独立审查和验收各1份 | completed_with_issues；10次调用，4分1秒；独立任务done，缺外部签名数据项unresolved/todo | PASS，1523.05秒 |

四任务包含独立任务、依赖任务和真实浏览器验证；失败隔离项具有真实失败检查，不将未满足的外部条件写为完成。四任务最终源码指纹：Codex 为 `11d0be3f09d672ec85b3e899ce253fbd12cab5cf120933f986eef4661cb09712`，CodeBuddy 为 `1fa467e20a58ff135a45c3fc7629a8e4b0bb1894e5bf4250941f2aa48cccde88`。

原始材料在系统临时目录：

- `milevia-supervision-codex-final-live-20261104-134320`，日志 `milevia-supervision-codex-final-live.txt`。
- `milevia-supervision-codebuddy-live-20261104-140119`，日志 `milevia-supervision-codebuddy-live.txt`。

复现沿用 §5 的真实多任务命令，并设置 `MILEVIA_SUPERVISION_LIVE_AGENT=codex` 或 `codebuddy` 选择 CLI。此结果只证明 Windows 本机这两个固定版本的上述场景；不代表 WSL/SSH、其他版本或未实现 CLI 已验收。跨端目标协议实验另行记录，不与真实全局 Agent 多任务验收混算。

## 9. 原生 Linux 服务与混合角色验证（2026-11-04）

在用户提供的 SSH 宿主的独立 `/tmp/milevia-supervision-ssh-*/` 目录上传当前 Go 源码，以目标机原生 CGO=1 编译测试程序，运行产品 HTTP 接口、正式 worker、SQLite 和真实 CLI；不使用 CGO=0 的业务数据库替身。测试 CLI 安装在该独立目录，没有升级原有全局 CLI，认证与连接仍由服务器原用户提供。SSH 使用已固定的公钥，不保存密码或复制本机登录资料。

必须区分这项实验与 Windows→SSH 产品调度：测试程序和任务目录均在 Linux 服务端本机，因此证明的是原生 Linux 托管链；不能据此声称 Windows 页面已能启动 SSH 项目托管。

| 实验 | 已取得的实际结果 |
| --- | --- |
| Linux Claude Code 2.1.308 两任务失败隔离 | PASS，134.07秒，11次真实模型调用，24条过程消息。正数判断任务 done，真实命令检查、独立审查、同快照验收各1份；外部签名数据缺失任务实际运行失败检查后 unresolved，整批 completed_with_issues；终态任务认领、目录和进程释放断言通过。 |
| Linux Codex 0.166.0 两任务失败隔离首轮 | FAIL，启动后仅显示“失败或不支持的事件”，尚无模型候选，不能计为通过。 |
| Codex 诊断修复后原环境重测 | FAIL，实际连接原因是 `Connection refused (os error 111)`。正确进行两次有限重试，三轮后 needs_input，没有伪造任务完成；服务器模型连通性不满足本场景验收条件。 |
| Linux 原生进程回归 | 10项顶层测试及Codex npm路径子用例全部通过，包括挂起握手、拒绝握手、取消、setsid后代清理、实际退出码、stdin透传和监督进程SIGKILL。 |

上述两任务原始日志保留在控制机临时目录：`milevia-supervision-native-linux-claude-live.txt`、`milevia-supervision-native-linux-codex-live.txt`、`milevia-supervision-native-linux-codex-recheck.txt`；Linux原生进程日志为 `milevia-supervision-native-linux-process-latest.txt`。原生多任务导出在服务器测试目录 `native-product/live-claude/` 与 `live-codex-recheck/`，不包含认证配置。

真实测试入口增加 `MILEVIA_SUPERVISION_LIVE_IMPLEMENTATION_AGENT` 与 `MILEVIA_SUPERVISION_LIVE_REVIEW_AGENT`，可独立选择实施和审查CLI；未设置时沿用调度CLI。完成断言同时核对实际Run的agent_id和已启动进程的不可变CLI规格，不能仅凭保存的角色配置判定混合执行成功。

后续两项实验均已实际收口：

| 实验 | 实际结果 |
| --- | --- |
| Linux Claude Code 四任务混合批次 | PASS，750.25秒，44次真实模型调用，96条过程消息；四项均 done/accepted/fresh。每项最终有效检查、独立审查和同快照验收各1份，终态认领、目录及进程释放断言通过。最终源码指纹 `28cacc403c99b7b9c75b3aa93407067a8848a2c7bfb4da14529ce25a96f73177`。 |
| Windows 三种CLI混合双任务 | PASS，159.65秒，11次真实模型调用，24条过程消息；Claude Code调度、Codex实施、CodeBuddy独立审查。独立任务 done，最终检查/审查/验收各1；缺少外部签名数据的任务有真实失败检查，保持 unresolved/todo，整批 completed_with_issues。 |

Linux四任务的首轮浏览器检查发生在浏览器下载完成前，因 Chromium 不存在实际失败；随后完成官方浏览器归档的下载、CRC核对和安装，并另行真实启动浏览器、点击按钮和核对文本。观察者没有修改任务状态、证据或恢复会话。正式worker检测到环境变化后，Agent自动重新安排浏览器验证、审查及验收，最终通过。这轮包含人工补齐运行环境，不能描述成从缺失浏览器到安装依赖均由托管Agent自行完成。四任务浏览器有两次实际执行检查，最终报告和截图均通过既有断言。

新增原始日志为控制机临时目录 `milevia-supervision-native-linux-claude-four-live.txt` 与 `milevia-supervision-mixed-live-latest.txt`；前者导出位于服务器隔离目录 `native-product/live-claude-four/independent_dependency_browser/`，后者导出位于控制机 `milevia-supervision-mixed-live-20261104-190742/failure_isolation/`。以上均不证明 Windows→SSH/WSL 的正式产品调度接入已经完成。

后续最新代码的原生CGO回归55项通过、0失败（另1项需要控制机配置的SSH测试明确跳过）；控制机使用新构建包的真实SSH/WSL专项另行通过，整组竞争检测17.269秒。Windows相关整组竞争检测273.688秒、targetexec整组8.090秒及最后快照专项9.886秒通过。该原生扩展回归实际发现并修复普通快照的Linux/WSL路径判断缺陷，前两轮失败材料如实保留，见[实施记录 §47](./54-任务托管Agent实施记录.md#47-目标续期测试派发绑定与linux快照修复2026-11-04)。这些回归没有重新调用模型，不与前面的真实模型调用数相加。

## 10. 目标 worker 与最终交接增量验证（2026-11-05）

本批没有新增真实模型调用。Qoder、Cursor 未登录，按用户要求先完成代码；配置、协议和权限探针不等同于真实账号的模型验收。

| 验证层 | 已取得的结果 |
| --- | --- |
| worker 目标执行与原领域回调 | 调度输出、实际文件 mutation、命令证据、独立审查和浏览器产物传输/归档专项通过。模型、检查及截图正文是夹具，协议、持久化、磁盘与领域回调实际运行，不算真实浏览器或真实模型全流程。 |
| 交接前的相关整组回归 | 370项顶层通过、24项明确跳过、0失败，竞争检测320.403秒；目标核心按该过滤器另有8项通过1.907秒；随后三个包 vet、命令入口测试通过。这轮早于最新最终交接实现。 |
| 目标最终交接专项 | 23.406秒竞争检测通过；包含选中多任务的通过子集、普通 worker 接入、单独交还、封存回复丢失、业务回复重试、晚到暂停、源码/依赖变化、证据改变后的高版本授权与重新封存、预算耗尽及无执行时撤销授权后的只读恢复。业务证据为夹具，目标实际采集、协议和事务实际运行。 |
| 目标执行核心全量 | 最新全量竞争检测8.547秒通过。 |
| 页面 | 最终交接状态增加中文解释；类型检查通过，1240项前端测试通过、0失败/0跳过；桌面及手机页面交互专项通过。 |

原始控制机日志：`milevia-supervision-worker-final-app.jsonl`、`milevia-supervision-target-handoff-complete.txt`。最终交接实现后的整组竞争检测384项顶层通过、24项明确跳过、0失败，359.428秒；三平台辅助程序构建、新包真实SSH/WSL桥接15.059秒通过。原生Linux CGO=1回归73项顶层通过、1项明确跳过，目标核心按该过滤器另1项通过。后续源码修改分批验证，不复用旧结果。最终交接实现细节见[实施记录 §50](./54-任务托管Agent实施记录.md#50-目标最终封存与业务验收交接2026-11-05)。


## 11. 角色收尾恢复、API交还与反向桥接（2026-11-05）

本节没有新增模型调用。数据库故障注入及真实目标管道验证只补观察不重复执行、证据/事件/预算幂等、冲突回执拒绝、暂停/外部改动、预算暂停、目标断线及控制端租约到期恢复。归档和产物清理API验证交还前及缺证明返回409、实际交还后204，真实文件保留/删除符合预期。

冻结收尾和API修改后，完整竞争检测390项顶层通过、24项明确跳过、0失败，357.369秒；目标核心全量竞争检测9.751秒通过，vet和命令入口测试通过。新包Windows→WSL部署、升级和身份专项7.420秒通过。日志：`milevia-supervision-observation-delivery-final-app.jsonl`、`milevia-supervision-role-observation-recovery.txt`、`milevia-supervision-observation-wsl-final.txt`。

后续Windows目标反向桥接新增后，Windows实际专项竞争检测11.649秒通过；Ubuntu-24.04内运行Linux测试进程，真实调用Windows PowerShell与辅助程序，两项Node检查退出0/7、去重、中文管道字节、EOF关闭、身份重连及篡改拒绝均通过，10.24秒。该Linux测试只验证桥接，不使用SQLite，不宣称原生CGO或公共跨端产品多任务验收。首轮缓冲超时及修复日志分别为`milevia-supervision-windows-reverse-bridge.txt`与`milevia-supervision-windows-reverse-bridge-final.txt`，实际WSL→Windows日志为`milevia-supervision-actual-wsl-to-windows-final.txt`。反向桥接后整组验证另行追加。


## 12. 实际环境路由与活动升级回归（2026-11-05）

本批没有新增模型调用。新增验证包括：WSL同一管理身份分离实际Linux/Windows目标、持久路由防篡改、安装记录按实际环境选择、原目标源码采集与停止交还，以及真实长运行Node进程在辅助程序升级时收敛、重复请求不重跑、旧授权不能启动新尝试。

Windows路由及反向桥接竞争检测专项19.770秒通过；Ubuntu-24.04内实际调用Windows PowerShell/helper，包含两项检查退出0/7、大中文源码回读、活动执行升级及目标身份/篡改拒绝，15.69秒通过。Linux桥接测试二进制CGO=0，不使用SQLite；另在用户服务器以CGO=1原生运行最新源码，app 86项顶层通过、2项跳过、0失败（10.105秒），目标核心按筛选器另1项通过。历史空Runner记录兼容缺陷的首轮5项失败保留，不与最终通过混算。

原始日志：`milevia-supervision-routes-and-active-upgrade.txt`、`milevia-supervision-routes-final-wsl-to-windows.txt`、`milevia-supervision-native-linux-routes-regression.jsonl`、`milevia-supervision-native-linux-routes-final-regression.jsonl`。该结果不是全部环境/CLI的真实模型多任务验收；公共跨端入口仍待共同目录保护与准入完成。


新构建包复验：Windows→WSL部署/升级7.51秒、Windows桥接含活动升级17.20秒，专项竞争检测合计26.439秒通过；固定公钥的真实SSH成功/失败命令及部署/回读/交还专项9.235秒通过；Ubuntu WSL→Windows使用同一新包再次通过，14.30秒。日志分别为 `milevia-supervision-routes-final-windows-wsl.txt`、`milevia-supervision-routes-final-ssh.txt`、`milevia-supervision-routes-current-bundle-wsl-to-windows.txt`。辅助程序包包含 Windows amd64、Linux amd64/arm64，全部重新构建并校验摘要；不包含模型验收。


冻结本节全部源码后的完整回归：app 398项顶层通过、25项明确跳过、0失败，竞争检测382.669秒；目标核心全量竞争检测9.051秒通过，三个包 `go vet` 通过，命令入口测试0.682秒通过。日志 `milevia-supervision-routes-complete-final-app.jsonl`。跳过项未计入通过；本轮没有新增真实模型多任务实验。最新文档通过UTF-8损坏文本检查及 `git diff --check`，所有修改保留工作区，没有提交或发布。
