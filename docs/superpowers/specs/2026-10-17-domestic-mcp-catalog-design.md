# 国内 MCP 服务目录第一批设计

## 目标

在 MCP 管理页加入第一批国内服务候选，同时避免把未经确认的第三方 MCP 地址伪装成官方一键连接。

## 目录分层

- `connect`：已确认可用的远程 MCP，进入现有 OAuth/凭据/试连向导。
- `manual`：只有官方开放平台文档或 API 入口，目录卡片只打开文档，不创建 MCP 配置。

现有 Notion、GitHub、Stripe 保持 `connect`。第一批国内服务中，百度网盘、Gitee、语雀已确认有可用的官方实现，使用 `connect`；飞书、阿里云盘、WPS/金山文档暂使用 `manual`。

## 安全边界

`manual` 条目不携带 URL、启动命令、OAuth 或凭据定义，因此不会被误送入连接测试，也不会保存空的 MCP server。只有确认官方 MCP endpoint、认证方式和权限范围后，才可升级为 `connect`。语雀的官方 stdio server 需要目标环境具备 Node.js / npx；Gitee 使用 PAT；百度网盘使用官方远程 SSE 授权。

## UI 行为

`connect` 卡片显示“连接”，复用现有向导；`manual` 卡片显示“查看接入说明”，打开条目的官方文档地址，并显示当前尚未确认官方 MCP 地址的说明。

## 验证

- 后端目录测试固定 9 个条目及 6 个国内 `manual` 条目的约束。
- 前端测试覆盖 `mode` 分流和类型声明。
- 运行 MCP 后端模板测试、前端 MCP 测试和 TypeScript 构建。
