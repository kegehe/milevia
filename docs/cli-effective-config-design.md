# CLI 有效 AI 配置读取设计

## 目标

项目 AI 配置弹窗需要显示当前项目实际生效的 AI 配置。项目没有 Milevia 项目级配置时，应读取对应 Runner 上 CLI 的当前配置；该能力不能依赖 CC Switch。

## 设计

控制服务定义统一的 CLI 配置探针接口，由不同 Runner 在配置所在机器读取数据：

- 本机 Runner：读取本机 `CODEX_HOME/config.toml` 或默认 `~/.codex/config.toml`。
- WSL Runner：在目标 WSL 环境内读取相同配置。
- SSH Runner：通过 SSH 在远端读取相同配置。

探针只返回脱敏快照，不返回 API Key、`auth.json` 内容或完整环境变量。Codex 快照包含 `model`、`model_provider`、Provider 的 `base_url`、`wire_api` 和凭据状态。CC Switch 只是可能的来源标签：如果配置指向其本地代理，显示代理来源，不读取 CC Switch 数据库作为必要依赖。

项目配置接口按以下优先级合并结果：

1. 项目级 Milevia 路由或 Profile；
2. Runner 上 CLI 的当前配置；
3. 明确的读取失败或不支持状态。

没有配置不能与读取失败混淆。前端显示来源、模型、Provider、Base URL、协议和凭据状态；凭据只显示是否已配置。

## 数据流

`GET /api/projects/{id}/agent-config` 获取项目配置。控制服务先解析项目路由；没有项目路由时调用项目对应 Runner 的 CLI 探针。WSL 与 SSH 探针必须使用目标环境的超时、输出大小限制和安全命令执行方式。解析失败返回结构化状态，不阻塞或伪装为空配置。

## 安全边界

- API Key 和 Token 不进入响应、日志、错误消息或数据库。
- 只允许读取固定 CLI 配置文件或受控的 `CODEX_HOME` 路径。
- 远端命令使用固定脚本和参数，不拼接用户输入。
- 探针必须有超时和输出上限。
- 项目级配置优先，避免 CLI 全局配置误覆盖项目设置。

## 测试

覆盖本机默认路径、自定义 `CODEX_HOME`、WSL、SSH、多 Provider、CC Switch 本地代理、文件不存在、格式错误、权限错误、超时、输出注入、脱敏和项目配置优先级。
