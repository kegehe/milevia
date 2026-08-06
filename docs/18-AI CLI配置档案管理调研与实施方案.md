# AI CLI 配置档案管理调研与实施方案

> 日期：2026-08-06
> 状态：已实施第一阶段。当前仅支持 CLI 登录档案与模型覆盖，不支持受管 API Key、`base_url` 或 Provider 配置。

## 1. 结论

可以提供类似 cc-switch 的“命名档案 + 会话显式选择”体验，但当前安全可行的边界比 cc-switch 更窄：

| 能力 | Claude Code | Codex | Windows 本机 / WSL 内 Control Server | Windows 调度 WSL / SSH |
| --- | --- | --- | --- | --- |
| 未选档案 | 使用原有 CLI 配置 | 使用原有 CLI 配置 | 支持 | 使用执行位置原有 CLI 配置 |
| CLI 登录档案 | 支持 | 支持 | 支持，名称和模型可固定 | 不由本机管理 |
| 受管 API Key / endpoint | 不支持 | 不支持 | 不支持 | 不支持 |
| 模型覆盖 | 支持 | 支持 | 支持 | 当前不由本机 Profile 注入 |

选择 Profile 只固定模型和版本；认证仍由目标机器上已经登录的 Claude Code 或 Codex CLI 处理。未选择 Profile，或把选择清空，都会回退到原有 CLI 配置。这是默认行为，不需要额外开关。

## 2. 为什么不开放 API Key Profile

此前的 Claude 方案曾将真实 Key 留在控制服务内存中，通过 loopback 凭据代理和临时 `apiKeyHelper` 文件向 Claude CLI 发放短期 capability。该方案不满足隔离要求：模型可执行的 Bash 与 CLI 同用户，能够通过进程列表或 `/proc` 找到 helper 路径、读取 capability，并调用本地代理。该 capability 等价于 API Key 的授权能力。

Codex 的独立 POC 也表明，试图用环境排除规则隐藏 `OPENAI_API_KEY` 不能阻止模型命令读取它。真实 Key、代理 capability、临时 helper 和可被命令发现的本地 socket 都不能作为安全边界。

因此当前实现 fail-closed：创建、编辑、会话选择和运行时均只接受 `auth_mode=cli_managed`、空 `base_url` 和空 `secret_ref`。遗留的 `keychain`、`env_ref` 或其他 credential revision 不能启动，也不能用于创建新会话。

## 3. 当前设计

### 3.1 数据与会话

`agent_profiles` 保存可编辑的档案身份；`agent_profile_revisions` 保存不可变的版本。Conversation 与 Run 保存 `agent_profile_revision_id`，因此模型覆盖和 Profile 状态在会话生命周期中可追溯。

Profile 没有默认值。请求没有 `profileId`、`profileId` 为空或用户清空选择时，revision ID 保持为空，执行原有 CLI 配置。显式选择时，服务端验证 Runner、Agent、启用状态、revision 状态与 `cli_managed` 约束。

修改名称或模型会创建 revision；停用会阻止新会话选择；撤销会拒绝后续准入并取消使用该 revision 的运行。旧 credential revision 可以通过编辑迁移为新的 CLI 登录 revision：服务端会清空 endpoint 和凭据引用，旧 revision 保留为不可执行的历史记录。

### 3.2 子进程环境

对显式 CLI 登录 Profile，控制服务从子进程环境移除以下继承变量：

- `ANTHROPIC_API_KEY`、`ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_BASE_URL`
- `OPENAI_API_KEY`、`OPENAI_BASE_URL`、`CODEX_API_KEY`

随后只追加 Milevia 自己的审批回调变量。不会传入 API Key、endpoint、Provider、loopback 地址、临时设置文件或认证 helper。模型非空时，Claude 使用 `--model`，Codex 使用受控 `-c model=...`。

这意味着“显式 CLI 登录档案”依赖 CLI 已持久化的登录态，而不是启动控制服务时继承的 API 环境变量。未选档案则保持历史行为，完全由原有 CLI 环境和配置决定。

### 3.3 平台边界

Control Server 与 CLI 位于同一执行位置时，本机可管理 CLI 登录档案。Windows 桌面端通过 `wsl.exe` 调度的 WSL，以及 SSH Runner，都是跨边界执行：控制服务不传输 Key、不修改远端 CLI 配置、不从远端读取密钥库。它们继续使用远端已配置的 CLI 登录状态。

要在 SSH 或 Windows 调度 WSL 上安全管理多档案，需要独立 Runner Agent。Agent 必须在执行地点保存和解析配置，控制服务只能下发非敏感 Profile/revision 标识；不得下发 API Key 或 capability。

## 4. API 与页面

现有接口：

```text
GET    /api/runners/{runnerID}/agent-profiles
POST   /api/runners/{runnerID}/agent-profiles
PATCH  /api/agent-profiles/{profileID}
POST   /api/agent-profiles/{profileID}/validate
POST   /api/agent-profiles/{profileID}/disable
POST   /api/agent-profile-revisions/{revisionID}/revoke
```

创建或编辑只接受名称、模型和 `authMode: "cli_managed"`。`baseUrl`、`apiKey`、`secretRef` 及任何其他认证方式均返回 400。页面仅展示并提交“CLI 登录”和模型；旧 credential Profile 标为“需要迁移”，不会出现在新会话选择器中。

`validate` 只校验 Profile 结构，不发起 endpoint 或密钥检测。CLI 可用性与登录状态继续由现有 Claude/Codex Runner 健康检查处理。

## 5. 安全不变量

1. 新建的可执行 Profile 不保存或传输 API Key、等价 capability、endpoint 或 Provider 配置；它们不进入 HTTP 响应、事件、日志、命令行、子进程环境或临时文件。
2. CLI 受管档案必须清理继承认证和 endpoint 环境变量；无档案会话保持原有配置，不声称隔离。
3. Profile 只能在所属 Runner 和 Agent 上使用；SSH/Windows WSL 跨边界时不接收本机 Profile 注入。
4. 运行时再次验证 revision，不能只依赖前端或创建会话时的校验。
5. 老的 credential revision fail-closed，且只能迁移到新的 `cli_managed` revision，不能继续执行。

## 6. 验收与回归

后端测试必须覆盖：

1. Claude 与 Codex 都拒绝 `keychain`、`env_ref`、API Key 和自定义 endpoint；
2. 遗留 credential revision 在新会话选择和运行时均被拒绝；
3. 编辑遗留 revision 会生成不含 endpoint/secret 的 CLI 登录 revision；
4. 受管 CLI Profile 会移除所有认证和 endpoint 环境变量，且 Claude 启动参数中没有 `apiKeyHelper`、`--bare` 或代理地址；
5. 未选择、清空选择、显式选择、停用、撤销与 revision 快照行为保持正确；
6. Linux/WSL、Windows 交叉构建、Web 测试和构建均通过。

## 7. 未来恢复受管 API Key 的门槛

该功能不是简单地重新接上密钥库或代理。任何恢复方案必须先在目标 CLI 版本和每个支持平台完成攻击性 POC，至少覆盖：模型调用 `env`、进程列表、`/proc`、临时文件/设置读取、打开文件描述符、loopback 端口与 socket 枚举、直接调用代理、插件、Hook、子代理和失败清理路径。

POC 必须证明模型可执行工具既得不到 Key，也得不到可实际调用 Provider 的等价 capability；仅证明环境变量未出现并不充分。在获得 CLI 提供的可验证强隔离边界，或把执行放入独立的受限账户/容器并建立可审计网络策略之前，受管 API Key Profile 必须继续保持关闭。

## 8. 后续建议

近期保持当前 CLI 登录档案方案，优先完善跨平台 Runner Agent 与可观测性。待有真实的隔离基础后，再单独设计 API Key 功能、威胁模型、平台 POC、迁移和回滚，不与当前 Profile 的模型覆盖能力混合发布。
