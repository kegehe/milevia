# AI CLI 配置档案管理调研与实施方案

> 日期：2026-08-06
> 状态：已实施第一阶段并扩展受管凭据。当前支持 CLI 登录档案（模型覆盖），并已放开受管 `api_key` / `base_url` 档案：API Key 加密存储于本地，仅在运行时注入对应 CLI 进程。

## 1. 结论

可以提供类似 cc-switch 的“命名档案 + 会话显式选择”体验，但当前安全可行的边界比 cc-switch 更窄：

| 能力 | Claude Code | Codex | Windows 本机 / WSL 内 Control Server | Windows 调度 WSL / SSH |
| --- | --- | --- | --- | --- |
| 未选档案 | 使用原有 CLI 配置 | 使用原有 CLI 配置 | 支持 | 使用执行位置原有 CLI 配置 |
| CLI 登录档案 | 支持 | 支持 | 支持，名称和模型可固定 | 不由本机管理 |
| 受管 API Key / base_url | 支持 | 支持 | 支持，密钥加密存储 | 不由本机管理 |
| 模型覆盖 | 支持 | 支持 | 支持 | 当前不由本机 Profile 注入 |

选择 Profile 决定模型、版本与（受管档案）端点/密钥；认证由目标机器上已经登录的 CLI 或受管密钥处理。未选择 Profile，或把选择清空，都会回退到原有 CLI 配置。这是默认行为，不需要额外开关。项目可设置默认 Profile，新会话自动套用。

## 2. 为什么不开放 API Key Profile

此前的 Claude 方案曾将真实 Key 留在控制服务内存中，通过 loopback 凭据代理和临时 `apiKeyHelper` 文件向 Claude CLI 发放短期 capability。该方案不满足隔离要求：模型可执行的 Bash 与 CLI 同用户，能够通过进程列表或 `/proc` 找到 helper 路径、读取 capability，并调用本地代理。该 capability 等价于 API Key 的授权能力。

Codex 的独立 POC 也表明，试图用环境排除规则隐藏 `OPENAI_API_KEY` 不能阻止模型命令读取它。真实 Key、代理 capability、临时 helper 和可被命令发现的本地 socket 都不能作为安全边界。

因此早期实现 fail-closed：创建、编辑、会话选择和运行时均只接受 `auth_mode=cli_managed`、空 `base_url` 和空 `secret_ref`。遗留的 `keychain`、`env_ref` 或其他 credential revision 不能启动，也不能用于创建新会话。

**当前扩展**：我们新增了 `api_key` 档案模式。受管 API Key 使用 AES-GCM 加密后存于独立 `profile_secrets` 表，主密钥来自数据目录旁随机生成的 `profile-master.key`；运行时把解密后的 Key 与 endpoint **直接注入 CLI 子进程环境**，不再使用 loopback 代理、临时 capability 文件或 helper。这样避免了旧方案的进程/socket 泄露面，但用户仍需了解并接受：模型可执行的命令与 CLI 同用户，能够读取子进程环境中的明文 Key —— 与 `cli_managed` 仅依赖 CLI 持久化登录相比，隔离性更弱，适合密钥可轮换、且运行该档案的机器风险可控的场景。若将执行放入受限账户/容器并配可审计网络策略，隔离性可进一步收紧。

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

**受管 `api_key` Profile**：在移除上述继承变量的同时，把解密后的 Key 和 endpoint 注入为权威值——Claude 注入 `ANTHROPIC_API_KEY` / `ANTHROPIC_BASE_URL`，Codex 注入 `OPENAI_API_KEY` / `OPENAI_BASE_URL`。模型通过 `--model`（Claude）或 `-c model=`（Codex）参数传入，不依赖环境变量。Key 不进入命令行或日志，仅作为子进程环境变量存在。Codex 的受管 `api_key` 档案只要求本机存在 Codex 二进制，不要求 CLI 已登录（密钥由档案注入）；`cli_managed` 或未选档案时，仍要求 Codex 已登录。

这意味着“显式 CLI 登录档案”依赖 CLI 已持久化的登录态；受管档案则完全由档案内密钥与 endpoint 决定。未选档案则保持历史行为，完全由原有 CLI 环境和配置决定。

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
PATCH  /api/projects/{projectID}/agent-profile
```

创建或编辑接受名称、模型、`authMode`（`cli_managed` 或 `api_key`）、`baseUrl`（api_key 时可选端点）与 `apiKey`（仅 api_key 时）。`keychain` / `env_ref` 模式与 `secretRef` 仍返回 400。页面提供“CLI 登录 / 受管 API Key”选择，以及“设为项目默认档案”一键应用。受管 Key 写入前端后不会在响应中回显。

`validate` 只校验 Profile 结构与受管密钥可解密，不发起外连；端到端连通性在首次运行校验。CLI 可用性与登录状态继续由现有 Claude/Codex Runner 健康检查处理。

## 5. 安全不变量

1. 新建的可执行 Profile 不把 API Key、等价 capability、endpoint 或 Provider 配置写入 HTTP 响应、事件或日志；受管 Key 仅以 AES-GCM 密文存于 `profile_secrets`，主密钥独立落在 `profile-master.key`。
2. 受管 `api_key` 档案在 runtime 再次校验并存取的密钥可解密；CLI 受管档案必须清理继承认证和 endpoint 环境变量；无档案会话保持原有配置，不声称隔离。
3. Profile 只能在所属 Runner 和 Agent 上使用；SSH/Windows WSL 跨边界时不接收本机 Profile 注入。
4. 运行时再次验证 revision，不能只依赖前端或创建会话时的校验；受管密钥只在运行时注入子进程环境。
5. 老的 `keychain`/`env_ref` credential revision 保持 fail-closed，且只能迁移到新的 `cli_managed` 或 `api_key` revision，不能继续执行。

## 6. 验收与回归

后端测试必须覆盖：

1. 受管 `api_key` 档案可按 Claude 与 Codex 创建；`keychain`、`env_ref` 模式仍被拒绝；
2. 创建响应与事件不泄漏明文 Key，受管档案可在运行时解密并注入 Key/endpoint；
3. 编辑遗留 revision 会生成不含 endpoint/secret 的 CLI 登录 revision；
4. 受管 CLI Profile 会移除所有认证和 endpoint 环境变量，且 Claude 启动参数中没有 `apiKeyHelper`、`--bare` 或代理地址；
5. 未选择、清空选择、显式选择、项目默认、停用、撤销与 revision 快照行为保持正确；
6. Linux/WSL、Windows 交叉构建、Web 测试和构建均通过。

## 7. 受管 API Key 的隔离边界

该功能不是简单地重新接上密钥库或代理。任何**强隔离**方案必须先在目标 CLI 版本和每个支持平台完成攻击性 POC，至少覆盖：模型调用 `env`、进程列表、`/proc`、临时文件/设置读取、打开文件描述符、loopback 端口与 socket 枚举、直接调用代理、插件、Hook、子代理和失败清理路径。

POC 必须证明模型可执行工具既得不到 Key，也得不到可实际调用 Provider 的等价 capability；仅证明环境变量未出现并不充分。

**当前实现明确选择的边界**：受管 Key 在运行时解密后直接注入子进程环境。它避免了代理 helper / socket / 临时文件的泄露面，但**不提供强隔离**——模型可执行的命令与 CLI 同用户，能读取环境变量中的 Key。因此：
- 受管 `api_key` 档案应使用**可轮换的密钥**，并只在本机信任的 runner / 项目上启用；
- 如需更强隔离，把执行放入独立受限账户/容器并配可审计网络策略后再放开。

## 8. 后续建议

优先完善跨平台 Runner Agent（让 SSH / Windows WSL 也能安全托管受管密钥）与可观测性。受管 Key 的强隔离（受限账户/容器 + 网络白名单）可在此基础上单独推进，不与当前直接注入方案混合发布。
