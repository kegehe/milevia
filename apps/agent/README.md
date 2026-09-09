# Milevia Agent

电脑端 Agent 通过出站 WSS 连接云端 Control Plane，不需要在电脑上开放公网入站端口。

## 配置

```text
MILEVIA_INSTANCE_ID=电脑实例稳定 ID
MILEVIA_CLOUD_URL=https://milevia.example.com
MILEVIA_CLOUD_AGENT_TOKEN=与此实例 ID 对应的云端 Agent Token（每台电脑独立）
MILEVIA_AGENT_ENROLLMENT_TOKEN=首次注册使用的管理员部署令牌（仅在首次启动时临时注入，绝不写入安装包）
MILEVIA_LOCAL_URL=http://127.0.0.1:8080
AUTO_REMOTE_AGENT_TOKEN=control-server 的 Agent Token
```

`MILEVIA_CLOUD_AGENT_TOKEN` 必须与云端 `MILEVIA_CLOUD_AGENT_TOKENS` 中当前实例 ID 的值一致；每台电脑使用独立随机高熵值，不能复用移动端用户 Token。Agent 只访问本机回环地址，并通过 HTTPS/WSS 与云端通信。

缺少实例 ID 和 Agent Token 时，Agent 可用 `MILEVIA_AGENT_ENROLLMENT_TOKEN` 调用云端注册接口。注册成功后凭据由 Windows DPAPI 保存到应用数据目录，后续启动不再需要该令牌；请立即从启动环境中移除它。
注册返回的独立凭据保存到 `MILEVIA_AGENT_CREDENTIAL_FILE`，后续启动优先使用该文件。

## 运行

```powershell
go run ./cmd/milevia-agent
```

Agent 会自动重连，事件只有在云端返回 `event.ack` 后才从本地 Outbox 删除；进程退出或网络中断时会保留待发送事件。

Windows 构建使用 DPAPI（当前 Windows 用户范围）加密保存注册后凭据；凭据文件由桌面端放在用户本地数据目录，安装包不会携带实例 ID 或 Agent Token。
