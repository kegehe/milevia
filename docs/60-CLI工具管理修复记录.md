# CLI 工具管理修复记录

日期：2026-11-06。承接 [59-CLI工具管理能力复查.md](59-CLI工具管理能力复查.md)，修复现有六种 CLI 的认证、管理与升级判据。修改保留工作区，未提交或发布。

## 1. 认证查询与登录入口

- 目录增加 `supportsAuthStatus`，与 `supportsLogin` 分别声明；前端兼容旧服务端的登录能力字段。
- Claude 使用 `auth status` JSON，Codex 使用 `login status`，二者补齐管理页查询和目标终端授权指引。
- Qoder 优先 `status -o json`，旧版不认识参数时回退文本；跨端补查询和 `qoder login` 指引。
- Cursor 官方包确认支持 `status --format json`。只有令牌、账号详情无法读取时为 `configured`；完整账号信息为 `logged_in`，明确无认证为 `logged_out`，格式陌生或查询失败为 `unreadable`。
- OpenCode 按明确的凭据数量和环境变量数量解析。`0 credentials` 与 `1 environment variable` 同时出现时为 `configured`；本地凭据文件回退同样不冒充账号已验证，读取有大小上限。
- CodeBuddy 保留前轮 ACP `_codebuddy.ai/getUserInfo` 方案，本轮已对本机安装版只读验证成功，无模型提示词。
- 普通非空输出不能证明已登录。退出码 1 的明确未登录输出保留；其它异常、超时、超限为未知。HTTP 不转发原始账号信息、Token 或 CLI 错误输出。

本机查询使用中性临时目录、进程树回收、20 秒期限和每个输出流 64 KiB 上限；跨端在目标环境使用私有临时目录、GNU timeout、输出文件限额和退出码协议。WSL 拒绝从 Windows 挂载目录解析的程序，并继续尝试原生别名。目标没有 GNU timeout 时保守返回未知。

## 2. Cursor 授权、模型和管理边界

- 本机登录使用原生 `NO_OPEN_BROWSER=1`，返回官方域名的 HTTPS 授权链接；重复点击复用流程，最长 10 分钟，服务关闭时回收，临时目录随进程清理。
- 模型目录读取目标 CLI 的 `models`，按已核实的 `Available models` / `id - display name` 格式解析，失败时保留自填。
- CodeBuddy 从目标 CLI `--help` 读取建议模型 ID；明确标注其不是账号可用型号保证，不借用 Claude 型号。
- Cursor 在 WSL / SSH 的管理查询、诊断与原生更新可用；跨端普通会话仍拒绝。本机适配器处理远端旧会话时，在申请工作区保护和 RPC 前返回明确限制，并保留幂等回执重放。
- 修正跨端 Cursor 管理卡片与诊断使用不同后端的矛盾。只有别名 `agent` 的安装也使用同一别名更新。

## 3. 安装发现、运行时和升级

- Claude 原生安装位置加入兜底候选，保留覆盖配置、登记、PATH、兜底的优先级。
- Claude 当前 npm 默认安装要求更新为 Node >=22；不把这个要求当成原生可执行文件的运行依赖。
- 用户指定历史版本时，由实际包的 engines 判断；本机和跨端 npm 安装均强制 `--engine-strict=true --force=false`，覆盖宽松用户配置。目录最低版本仍描述默认安装要求，不是自动同步 registry 的元数据缓存。
- 本机已安装包的诊断优先读取该包 `package.json` 的 Node 要求，避免历史包被最新目录要求误报；复杂区间不自行猜测成最低版本，安装仍交给 npm 严格校验。
- SSH Codex 安装就绪仅看真实版本，认证由独立接口判断。
- WSL / SSH Claude、Codex 与目录驱动工具共用实时升级自检。查询失败不会退回展示缓存，也不会假称更新成功；健康未知时明确提示未自动回滚。已确认不可用的编排仍保留来源核对和回滚后版本验证。

## 4. 验证

- 前端全量 **1,253 项通过**，TypeScript / Vite 构建通过；构建保留现有的大包体积提示。
- 后端认证、HTTP、目录分派、升级、自检、模型、安装与本机会话边界的针对性回归 **71 项主测试通过**；另对认证缓存、浏览器登录生命周期及跨端管理运行 `-race`，全部通过。
- 安装、运行时、诊断与路径解析的扩展回归通过。没有把整套 `internal/app` 声称为已通过。
- 本机 Claude / Codex 状态查询与 CodeBuddy 帮助候选读取通过；CodeBuddy ACP 账号识别通过。日志仅记录状态枚举，不保存账号或密钥。
- Cursor 官方 Windows 分发包 `2026.11.05-86047df` 下载到系统临时目录，在隔离 HOME / USERPROFILE / XDG 目录中确认未登录 JSON；通过实际本机查询适配器验证为 `logged_out`。没有安装到用户 PATH，没有发起真实授权。
- 跨端认证脚本在 Git for Windows 的 sh 中实际执行，验证 stderr 未登录与退出码 1 能完整返回；WSL 原生边界、别名、自检与超时协议另有回归。
- `git diff --check` 通过。

主要日志在系统临时目录：`milevia-cli-management-go-final.jsonl`、`milevia-cli-management-race.log`、`milevia-cli-management-install-checks.log`、`milevia-cli-management-installed.log`、`milevia-cli-management-web-all.log`。

## 5. 验收边界

本轮没有修改真实账号、执行真实 CLI 升级或回滚、调用计费模型，未连接生产 SSH。已登录 Cursor / Qoder 的真实授权完成、六工具真实 WSL / SSH 组合和实际更新回滚仍需相应环境验收；结构化输出、受控进程和假跨端回归不能替代这些验收。

Gemini / Copilot 新适配器、完整凭据档案路由、原生安装器管理和插件生命周期仍是独立功能；本轮补齐的是现有 CLI 已确认的管理缺口。

## 6. 二次代码审查（2026-11-06）

确认并继续修复以下问题：

1. **登录异步响应覆盖**：仅用 agentID 拦截响应，不能区分同一工具关闭重开；自动读数也可能覆盖手动刷新的结果。改为环境/工具独立的请求序号及面板操作令牌，授权成功、失败和 finally 都校验所属面板；切换环境、卸载和关闭时作废。面板与卡片消费同一份最新读数，并阻止用旧环境列表向新环境发起查询。
2. **旧授权链接遮住检查结果**：发起登录后再检查状态，原界面仍优先显示 loginInfo。检查开始时清除旧授权内容，显示这次查询结果。
3. **POSIX 探针子进程残留**：共享探针超时仅杀主进程，正常退出也未清理进程组。Linux 受控测试在修复前实际复现两种残留；修复后统一清理进程组，并等待取消看护退出。Windows 保留 Job 回收方式，CodeBuddy 移除重复的外层 POSIX 清理。
4. **运行时诊断误报及漏报**：包元数据缺失、损坏、名称不符或超限时，不再套用最新版最低 Node 要求；不支持的复杂 engines 区间明确记为未完成检查。诊断汇总在没有已确认 blocker 时把检查缺项收敛为 unknown，避免记录了 limitations 仍显示正常。实际缺少 Node/npm 或确认版本过低的故障仍优先报告。

验证：前端全量 1,258 项及 TypeScript/Vite 构建通过；后端认证、模型、升级与诊断针对性回归 80 项主测试通过、2 项 opt-in 真机测试跳过。Linux 测试由 Windows 交叉编译后在本机 WSL 执行，确认正常退出和超时两种情形均无存活后裔。构建仍有现有大包体积提示。

另对认证、浏览器登录、探针回收和运行时回归运行 Go race detector，通过；安装、运行时、npm 与修复扩展回归通过。相关日志为系统临时目录中的 `milevia-cli-review-go-final.jsonl`、`milevia-cli-review-race.log`、`milevia-cli-review-install-repair.log`、`milevia-cli-review-web-final.log` 和 `milevia-cli-review-build.log`。

本次不包含真实账号授权、真实 CLI 升级/回滚或生产 SSH 验收。新增回归覆盖了元数据异常与历史版本、异步结果倒序返回、面板关闭重开及授权链接清除；未提交、发布。
