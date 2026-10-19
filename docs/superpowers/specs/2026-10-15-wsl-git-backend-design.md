# WSL Git Backend

## 目标

Windows 服务端上的 WSL 项目不再通过 `\\wsl$` UNC 路径让 Windows Git 访问 Linux 文件系统。所有 Git 命令和仓库文件操作都在目标 WSL 发行版内执行。

## 设计

- 为 Windows + `wsl-local` 项目增加专用 `wslGitBackend`，复用现有 `wslAgentRunner` 的路径转换、参数编码、超时和进程终止策略。
- Git runner 接口保持不变，Windows 本地、Linux 本地和 SSH 项目继续使用现有 backend。
- WSL backend 覆盖命令、stdin、路径列表、临时文件、文件元数据、读写、未跟踪文件校验和删除。
- `git status` 结果在一次扫描中同时生成 snapshot、changes 和状态令牌所需指纹。
- 前端只使用最新 summary 响应里的 changes；刷新时仅在存在冲突文件时读取冲突详情。

## 风险控制

- UNC 路径必须匹配已注册发行版后才能转换为 Linux 路径。
- 所有用户路径继续经过既有 Git 路径校验和 shell 参数编码。
- WSL 进程取消时终止整个进程组，避免残留 Git 进程和索引锁。

## 验证

- 单元测试覆盖路径转换、命令参数、状态合并和 WSL backend 文件操作。
- 运行 control-server Go 测试和 web Git 相关测试。
