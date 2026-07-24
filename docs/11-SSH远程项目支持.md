# SSH 远程项目支持 — 设计与实现方案

> 日期: 2026-07-22
> 目标: 分析当前仅支持 WSL 本地项目加载的机制，设计将远端 Linux 服务器的 SSH 项目目录纳入平台管理的完整方案。

## 1. 现状分析

### 1.1 当前项目加载链路

```
前端 Importer 组件
  → GET /api/runners                     (获取可用 Runner 和根路径)
  → GET /api/directories?path=...        (浏览 Runner 所在环境的目录)
  → POST /api/projects/validate           (校验目录：Git + Claude Code)
  → POST /api/projects                    (创建项目，写入 projects 表)
  → 项目路径写入 projects.path 字段
  → Claude Code CLI 以 project.Path 为 cmd.Dir 执行
```

### 1.2 当前 Runner 模型

项目中只有一个 Runner 实现 —— `claudeCLIRunner`，它直接通过 `os/exec` 在 WSL 本地启动 Claude Code CLI：

```
控制平面 (control-server)
  └── claudeCLIRunner (claude_runner.go)
        ├── Ready()      → exec.LookPath("claude") + claude auth status
        ├── Run()        → exec.Command("claude", args...), cmd.Dir = project.Path
        ├── Version()    → exec.Command("claude", "--version")
        ├── CheckUpdate()→ npm view @anthropic-ai/claude-code version
        ├── Update()     → exec.Command("claude", "update")
        └── StartSession()→ 持久 stream-json 进程
```

`AgentRunner` 接口定义（`claude_runner.go:19-34`）：

```go
type AgentRunner interface {
    Ready(context.Context) bool
    Run(context.Context, AgentRunRequest, AgentRunSink) error
    Version(context.Context) string
    CheckUpdate(context.Context) (updateAvailable bool, latestVersion string, err error)
    Update(context.Context) (previousVersion, currentVersion string, err error)
}

type StreamingAgentRunner interface {
    AgentRunner
    StartSession(context.Context, AgentSessionRequest) (AgentSession, error)
}
```

### 1.3 现有 Runner 注册机制

`listRunners()` 函数（`app.go:2367-2410`）当前硬编码返回一个 Runner：

```go
func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
    // ...版本状态检查...
    roots := []map[string]string{
        {"name": "WSL Home", "path": s.config.AllowedRoot},
    }
    // 自动检测 Windows 盘符
    for _, drive := range []string{"c", "d", "e", "f"} { ... }
    
    writeJSON(w, http.StatusOK, []map[string]any{{
        "id":          "wsl-local",
        "name":        "WSL Local Runner",
        "environment": "wsl",
        "root":        s.config.AllowedRoot,
        "roots":       roots,
        "claude":      map[string]string{"status": status, "version": version},
    }})
}
```

**问题：没有 Runner 注册/发现机制**。Runner 不是独立实体，而是内嵌在控制平面中。要支持远程 SSH Runner，必须先建立 Runner 注册体系。

### 1.4 架构文档中的远程 Runner 规划

文档 `02-架构设计.md` 第 139 行明确提出：

> "首期项目在 WSL 中，因此使用 WSL Runner。后续项目位于 Windows、本机 Linux、容器或远程服务器时，只增加对应环境的 Runner，不改变项目、任务和页面结构。"

第 141 行规划了通信方式：

> "Control Server 与 Runner 使用 **gRPC + Protocol Buffers** 进行双向通信。"

但文档 10（Windows 项目目录支持）的实际实现选择了更务实的路径——**通过 WSL 的 `/mnt/` 挂载复用 WSL Runner**，而非新建独立 Runner。这是因为 WSL 天然提供了跨文件系统访问能力。

### 1.5 与 Windows 项目的本质区别

| 维度 | Windows 项目（文档 10） | SSH 远程项目（本文档） |
|------|------------------------|----------------------|
| 文件系统访问 | WSL 的 `/mnt/` 自动挂载，`os.ReadDir()` 直接可用 | 无本地挂载，需要通过 SSH 协议通信 |
| 进程执行 | `os/exec` 在 WSL 中执行 Claude Code | 必须在远程服务器上执行 Claude Code |
| Git 操作 | 本地 Git 可直接操作 `/mnt/` 路径 | 远程服务器的 Git 仓库 |
| 安全边界 | `allowedPath()` 白名单即可 | 需要 SSH 认证、密钥管理、权限控制 |
| 网络依赖 | 无 | 需要稳定 SSH 连接 |
| Runner 模型 | 复用 WSL Runner | **必须新建独立 Runner** |

**核心结论：SSH 远程项目无法像 Windows 项目那样复用现有 Runner，必须实现独立的远程 Runner 和执行平面分离。**

## 2. 需求分析

### 2.1 典型场景

用户在远程 Linux 开发服务器上有一批项目，希望：
- 通过 SSH 连接到远程服务器
- 浏览服务器上的项目目录
- 将远程项目加载到平台
- 在远程服务器上运行 Claude Code 进行 AI 开发
- 在 Web 页面中实时查看远程 Claude Code 的执行过程

例如：
- `user@dev-server-01:/home/user/projects/api-service`
- `user@192.168.1.100:/opt/repos/frontend-app`
- `user@jumphost:/data/projects/backend-worker`

### 2.2 功能需求

| 编号 | 需求 | 优先级 |
|------|------|--------|
| R1 | 在 Web 页面中添加和管理 SSH 连接配置 | P0 |
| R2 | 通过 SSH 浏览远程服务器上的目录结构 | P0 |
| R3 | 校验远程目录（Git 仓库、Claude Code 可用性） | P0 |
| R4 | 将远程项目加载到平台，与本地项目并列管理 | P0 |
| R5 | 在远程服务器上启动 Claude Code 并实时查看执行过程 | P0 |
| R6 | 支持多个 SSH 连接配置（多台远程服务器） | P1 |
| R7 | SSH 密钥认证（免密登录） | P0 |
| R8 | 连接状态监控与自动重连 | P1 |
| R9 | 远程项目的 Git 操作 | P2 |
| R10 | 远程项目的任务管理 | P2 |

### 2.3 非目标（明确不做）

- **不在平台中管理 SSH 密钥对生成**——用户自行通过 `ssh-keygen` 生成，平台只使用已有密钥
- **不实现 SSH 密码认证**——只支持密钥认证，避免在平台中存储密码
- **不实现跳板机/堡垒机**——首期只支持直连 SSH
- **不在远程服务器上自动安装 Claude Code**——要求远程服务器已安装好依赖
- **不实现多跳 SSH**——不支持 `user@A → user@B → target` 的链路
- **不跨 Runner 调度任务**——任务在哪个 Runner 的项目中就在哪个 Runner 执行
- **不实现远程 Runner 自动注册**——首期手动添加 SSH 连接

## 3. 设计方案

### 3.1 整体架构

新增 **Remote Runner Agent** 作为远程服务器上的执行代理。它是一种极薄的代理程序，通过 SSH 通道与控制平面通信。

```
┌─────────────────────────────────────────────────────────┐
│                     Web 控制台 (浏览器)                    │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ 项目列表      │  │ Importer     │  │ Chat 对话     │  │
│  │ (本地+远程)   │  │ (本地+SSH)   │  │ (实时过程)    │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────┬───────────────────────────────┘
                          │ HTTP + WebSocket
┌─────────────────────────▼───────────────────────────────┐
│               控制平面 (control-server)                   │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │ 项目管理  │  │ Runner 注册   │  │ SSH 连接管理      │  │
│  │ (projects)│  │ (多 Runner)  │  │ (ssh_configs)    │  │
│  └──────────┘  └──────────────┘  └──────────────────┘  │
│                         │                                │
│         ┌───────────────┼───────────────┐               │
│         ▼               ▼               ▼               │
│  ┌──────────┐   ┌──────────────┐  ┌──────────────┐    │
│  │WSL Local │   │SSH Remote    │  │SSH Remote    │    │
│  │ Runner   │   │Runner A      │  │Runner B      │    │
│  │(内嵌)    │   │(SSH Adapter) │  │(SSH Adapter) │    │
│  └──────────┘   └──────┬───────┘  └──────┬───────┘    │
└─────────────────────────┼─────────────────┼─────────────┘
                          │ SSH              │ SSH
                          ▼                  ▼
┌─────────────────┐  ┌──────────────┐  ┌──────────────┐
│ WSL 本地文件系统  │  │ 远程服务器 A   │  │ 远程服务器 B   │
│ /home/user/...   │  │ user@hostA    │  │ user@hostB    │
│ /mnt/c/...       │  │ Claude Code   │  │ Claude Code   │
│ Claude Code CLI  │  │ Git           │  │ Git           │
└─────────────────┘  └──────────────┘  └──────────────┘
```

### 3.2 设计原则

- **最小侵入**: 复用现有 `AgentRunner` 接口，新增 `sshRunner` 实现
- **渐进增强**: 先实现核心的 SSH 连接 + 目录浏览 + Claude 执行，再逐步完善
- **安全第一**: 密钥不经过浏览器，只存储在控制平面本地；SSH 连接可随时断开
- **统一体验**: 远程项目与本地项目在项目管理、对话页面中使用完全相同的 UI

### 3.3 SSH Runner 的核心思路

与 Windows 项目复用 WSL Runner 不同，SSH 远程项目必须有一个**在远程服务器上执行的 Runner**。但实现方式有两种路径：

#### 路径 A：SSH 直连模式（推荐首期）

控制平面通过 Go 的 SSH 客户端库（`golang.org/x/crypto/ssh`）直接连接到远程服务器，在远程执行命令：

```
控制平面 ──SSH(sftp)──> 远程服务器
  ├── os.ReadDir()     → sftp.ReadDir()
  ├── exec.Command()   → ssh session.Run()
  └── git 命令          → ssh session.Run("git ...")
```

**优点**：
- 不需要在远程服务器上部署任何 Agent 服务
- 完全复用现有的 `AgentRunner` 接口
- 开发周期短（约 3-5 天）

**缺点**：
- 每次操作建立 SSH 会话（命令模式），非持久连接
- Claude Code 的 stream-json 持久会话需要保持一个长时间 SSH 连接
- 网络不稳定时体验差
- 扩展性受限（不适合未来 gRPC 演进）

#### 路径 B：远程 Agent 模式（目标架构）

在远程服务器上运行一个轻量的 Runner Agent（Go 二进制），控制平面通过 gRPC 与其通信：

```
控制平面 ──gRPC──> 远程 Runner Agent
                      ├── 本地文件系统操作
                      ├── exec Claude Code
                      └── Git 操作
```

**优点**：
- 符合架构文档规划的 gRPC + Runner 方向
- 连接管理更健壮（gRPC 自带重连、负载均衡）
- 支持流式双向通信

**缺点**：
- 需要在远程服务器上部署 Agent（增加运维复杂度）
- 开发周期长（需要定义 proto、实现 Agent 服务）
- 首期用户可能只有一两台服务器，投入产出比不高

#### 推荐实施路径

**首期（本文档范围）采用路径 A**，原因：
1. 用户想快速把远程项目用起来，不需要复杂的 Agent 部署
2. 与文档 10 的策略一致——"最小改动、快速可用"
3. SSH 直连模式可以后向兼容——未来替换为 Agent 模式时，`AgentRunner` 接口保持不变

后续演进到路径 B 时，只需将 `sshRunner` 替换为 `grpcRunner`，前端、项目管理、对话页面完全不需要改动。

### 3.4 各层详细设计

---

#### 3.4.1 数据模型：新增 SSH 连接配置

##### A. 数据库表：`ssh_connections`

```sql
CREATE TABLE IF NOT EXISTS ssh_connections (
    id              text primary key,
    name            text not null,             -- 连接名称（用户自定义），如 "开发服务器"
    host            text not null,             -- 主机地址，如 "192.168.1.100"
    port            integer not null default 22,
    user            text not null,             -- SSH 用户名
    private_key_path text not null,            -- 私钥路径（控制平面本地的绝对路径）
    known_hosts     text not null default '',  -- 主机公钥（OpenSSH known_hosts 格式）
    root_path       text not null default '/', -- 远程服务器上可浏览的起始目录
    status          text not null default 'unknown',  -- unknown/connected/disconnected/error
    last_seen       datetime,                  -- 最近一次连接成功时间
    error_msg       text not null default '',  -- 最近一次连接错误信息
    created_at      datetime not null,
    updated_at      datetime not null
);
```

**设计决策**：
- `private_key_path` 存储控制平面本地的私钥文件路径，不存储私钥内容。私钥文件由用户自行放置在控制平面所在机器上
- `known_hosts` 存储远程主机的公钥（OpenSSH known_hosts 格式的单行条目）。首次连接时使用 `ssh.InsecureIgnoreHostKey()` 临时信任，连接成功后通过 `session.HostKey()` 获取主机公钥，序列化后存入数据库。后续连接使用 `ssh.FixedHostKey()` 进行严格验证
- `status` 字段支持连接状态展示，前端可以显示绿色/红色指示灯
- 不存储密码，只支持密钥认证

##### B. Runner 标识约定

当前 `projects.runner` 字段存储 `"wsl-local"`。对于 SSH 远程项目，约定：

```
projects.runner = "ssh-{connection_id}"
```

例如：`"ssh-abc123-def456"`。这样可以直接通过 runner 字段找到对应的 SSH 连接配置。

##### C. 项目创建时的新增字段

SSH 远程项目创建时：
- `runner` = `"ssh-{connection_id}"`
- `environment` = `"remote-linux"`
- `path` = 远程服务器上的绝对路径，如 `/home/user/projects/my-app`
- `pathDisplay` = `"{connection_name}:{remote_path}"` 格式，如 `"开发服务器:/home/user/projects/my-app"`

---

#### 3.4.2 后端：SSH Runner 实现

##### A. SSH 客户端封装（新文件：`ssh_runner.go`）

```go
package app

import (
    "context"
    "fmt"
    "io"
    "net"
    "os"
    "strings"
    "time"

    "golang.org/x/crypto/ssh"
    "github.com/pkg/sftp"
)

// sshClient wraps an SSH connection to a remote server.
type sshClient struct {
    mu       sync.Mutex
    config   ssh.ClientConfig
    host     string
    port     int
    client   *ssh.Client
}

func newSSHClient(conn SSHConnection) (*sshClient, error) {
    keyBytes, err := os.ReadFile(conn.PrivateKeyPath)
    if err != nil {
        return nil, fmt.Errorf("read private key %s: %w", conn.PrivateKeyPath, err)
    }
    signer, err := ssh.ParsePrivateKey(keyBytes)
    if err != nil {
        return nil, fmt.Errorf("parse private key: %w", err)
    }
    hostKeyCallback := ssh.InsecureIgnoreHostKey() // 首次连接
    if conn.KnownHosts != "" {
        _, _, pubKey, _, _, err := ssh.ParseKnownHosts([]byte(conn.KnownHosts))
        if err == nil {
            hostKeyCallback = ssh.FixedHostKey(pubKey)
        }
    }
    return &sshClient{
        config: ssh.ClientConfig{
            User:            conn.User,
            Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
            HostKeyCallback: hostKeyCallback,
            Timeout:         10 * time.Second,
        },
        host: conn.Host,
        port: conn.Port,
    }, nil
}

func (c *sshClient) connect() error {
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.client != nil {
        return nil // already connected
    }
    addr := net.JoinHostPort(c.host, fmt.Sprintf("%d", c.port))
    client, err := ssh.Dial("tcp", addr, &c.config)
    if err != nil {
        return fmt.Errorf("ssh dial %s: %w", addr, err)
    }
    c.client = client
    return nil
}

func (c *sshClient) close() error {
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.client != nil {
        err := c.client.Close()
        c.client = nil
        return err
    }
    return nil
}

// execCommand runs a command on the remote server, respecting context cancellation.
func (c *sshClient) execCommand(ctx context.Context, cmd string) ([]byte, error) {
    if err := c.connect(); err != nil {
        return nil, err
    }
    session, err := c.client.NewSession()
    if err != nil {
        return nil, fmt.Errorf("new ssh session: %w", err)
    }
    defer session.Close()

    // Respect context cancellation via goroutine + session.Signal
    type result struct {
        out []byte
        err error
    }
    ch := make(chan result, 1)
    go func() {
        out, err := session.CombinedOutput(cmd)
        ch <- result{out, err}
    }()
    select {
    case <-ctx.Done():
        session.Signal(ssh.SIGKILL)
        return nil, ctx.Err()
    case r := <-ch:
        return r.out, r.err
    }
}

// readDir lists directory entries on the remote server via SFTP.
func (c *sshClient) readDir(path string) ([]os.FileInfo, error) {
    if err := c.connect(); err != nil {
        return nil, err
    }
    sftpClient, err := sftp.NewClient(c.client)
    if err != nil {
        return nil, fmt.Errorf("new sftp client: %w", err)
    }
    defer sftpClient.Close()
    return sftpClient.ReadDir(path)
}
```

##### B. SSH Runner 实现 `AgentRunner` 接口

```go
// sshRunner implements AgentRunner for a remote server accessed via SSH.
type sshRunner struct {
    client   *sshClient
    connID   string
    connName string
}

func (r *sshRunner) Ready(ctx context.Context) bool {
    // 检查 SSH 连接是否可用 + 远程 claude 是否安装
    out, err := r.client.execCommand(ctx, "which claude && claude --version")
    return err == nil && len(out) > 0
}

func (r *sshRunner) Version(ctx context.Context) string {
    out, err := r.client.execCommand(ctx, "claude --version")
    if err != nil {
        return ""
    }
    return strings.TrimSuffix(strings.TrimSpace(string(out)), " (Claude Code)")
}

func (r *sshRunner) CheckUpdate(ctx context.Context) (bool, string, error) {
    // 远程执行 npm view 检查版本
    local := r.Version(ctx)
    if local == "" {
        return false, "", errors.New("Claude Code is not installed on remote")
    }
    out, err := r.client.execCommand(ctx, "npm view @anthropic-ai/claude-code version 2>/dev/null")
    if err != nil {
        return false, "", err
    }
    latest := strings.TrimSpace(string(out))
    return latest != local, latest, nil
}

func (r *sshRunner) Update(ctx context.Context) (string, string, error) {
    previous := r.Version(ctx)
    if previous == "" {
        return "", "", errors.New("Claude Code is not installed on remote")
    }
    _, err := r.client.execCommand(ctx, "claude update")
    if err != nil {
        return previous, "", err
    }
    return previous, r.Version(ctx), nil
}

func (r *sshRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
    // 通过 SSH 启动远程 claude 进程
    // 使用 ssh session 的 stdout/stderr 管道获取实时输出
    // ...
}

// sshRunner 也实现 StreamingAgentRunner 以支持持久会话
func (r *sshRunner) StartSession(ctx context.Context, req AgentSessionRequest) (AgentSession, error) {
    // 在远程服务器上启动 claude --input-format stream-json --output-format stream-json
    // 通过 SSH session 的 stdin/stdout 管道进行双向通信
    // ...
}
```

**关键设计决策**：
- `sshRunner` 完整实现 `AgentRunner` + `StreamingAgentRunner` 接口
- 目录浏览通过 SFTP 协议（`github.com/pkg/sftp`）
- 命令执行通过 `ssh.Session.CombinedOutput()` 或 `ssh.Session.Run()`
- 流式 Claude Code 会话通过一个持久的 SSH session 维护，stdin/stdout 管道双向传输

##### C. Runner 注册表（新文件：`runner_registry.go`）

当前 `listRunners()` 硬编码返回一个 Runner。引入 SSH 后，需要改为动态注册机制：

```go
// runnerRegistry manages all available runners.
type runnerRegistry struct {
    mu      sync.RWMutex
    runners map[string]AgentRunner  // runnerID → runner instance
    metas   map[string]RunnerMeta   // runnerID → metadata
}

type RunnerMeta struct {
    ID          string `json:"id"`
    Name        string `json:"name"`
    Environment string `json:"environment"` // "wsl", "remote-linux"
    Host        string `json:"host,omitempty"`
    Roots       []RootEntry `json:"roots"`
}

type RootEntry struct {
    Name  string `json:"name"`
    Path  string `json:"path"`
    Label string `json:"label,omitempty"`
}

func (reg *runnerRegistry) register(id string, runner AgentRunner, meta RunnerMeta) { ... }
func (reg *runnerRegistry) unregister(id string) { ... }
func (reg *runnerRegistry) list() []RunnerMeta { ... }
func (reg *runnerRegistry) get(id string) (AgentRunner, bool) { ... }
```

启动时注册 `wsl-local`，用户添加 SSH 连接时动态注册 `ssh-{conn_id}`。

---

#### 3.4.3 后端 API

##### A. SSH 连接管理 API

| 方法 | 路由 | 说明 |
|------|------|------|
| `GET` | `/api/ssh-connections` | 列出所有 SSH 连接配置 |
| `POST` | `/api/ssh-connections` | 添加 SSH 连接配置 |
| `GET` | `/api/ssh-connections/{id}` | 获取单个连接详情 |
| `DELETE` | `/api/ssh-connections/{id}` | 删除连接配置。先检查 `SELECT count(*) FROM projects WHERE runner='ssh-{id}'`，如有项目依赖则返回 409 拒绝删除；确认无依赖后断开连接、注销 Runner、删除记录 |
| `POST` | `/api/ssh-connections/{id}/test` | 测试连接是否可用 |
| `POST` | `/api/ssh-connections/{id}/connect` | 建立连接并注册 Runner |
| `POST` | `/api/ssh-connections/{id}/disconnect` | 断开连接并注销 Runner |

##### B. 调整现有 API

| 路由 | 变更 |
|------|------|
| `GET /api/runners` | 从硬编码改为从 `runnerRegistry` 动态读取，返回所有已注册 Runner（含 SSH Runner） |
| `GET /api/directories?path=&runner=` | 新增 `runner` 参数，指定由哪个 Runner 浏览目录。省略时默认 `wsl-local`；SSH Runner 通过 SFTP 读取 |
| `POST /api/projects/validate` | 新增 `runner` 字段（必填），指定校验哪个 Runner 的目录；远程目录通过 SSH 校验 Git 和 Claude Code |
| `POST /api/projects` | `runner` 字段从固定值改为必填参数；SSH 项目存储 `ssh-{connection_id}` |
| `POST /api/conversations/{id}/messages` | 发送消息时根据 `project.runner` 选择对应 Runner 执行，远程项目走 SSH Runner |

##### C. SSH 连接添加的请求/响应

```json
// POST /api/ssh-connections
{
    "name": "开发服务器",
    "host": "192.168.1.100",
    "port": 22,
    "user": "tangmaoke",
    "privateKeyPath": "/home/tangmaoke/.ssh/id_rsa",
    "rootPath": "/home/tangmaoke/projects"
}

// 响应
{
    "id": "abc123",
    "name": "开发服务器",
    "host": "192.168.1.100",
    "port": 22,
    "user": "tangmaoke",
    "status": "connected",
    "roots": [
        {"name": "开发服务器 /home", "path": "/home/tangmaoke/projects", "label": "remote-linux"}
    ]
}
```

##### D. 目录浏览 API 调整

`GET /api/directories?path=&runner=ssh-abc123`

对于 SSH Runner，`listDirectories()` 不走 `os.ReadDir()`，而是调用 `sshClient.readDir()`（SFTP）。

`allowedPath()` 也需要调整：对于远程路径，不检查本地文件系统，改为检查是否在 SSH 连接配置的 `root_path` 范围内。

---

#### 3.4.4 前端设计

##### A. 新增 SSH 连接管理页面

在项目仪表盘中增加"管理 SSH 连接"入口，打开一个 SSH 连接管理弹窗/面板：

```
┌─ SSH 连接管理 ──────────────────────────────┐
│                                              │
│  已配置的连接                                 │
│  ┌──────────────────────────────────────┐   │
│  │ 🟢 开发服务器                         │   │
│  │    user@192.168.1.100:22              │   │
│  │    最近连接: 2026-07-22 14:30         │   │
│  │    [测试连接] [断开] [删除]            │   │
│  └──────────────────────────────────────┘   │
│  ┌──────────────────────────────────────┐   │
│  │ 🔴 备用服务器                         │   │
│  │    user@10.0.0.50:22                  │   │
│  │    连接失败: permission denied         │   │
│  │    [测试连接] [删除]                   │   │
│  └──────────────────────────────────────┘   │
│                                              │
│  [+ 添加 SSH 连接]                           │
└──────────────────────────────────────────────┘
```

##### B. 添加 SSH 连接表单

```
┌─ 添加 SSH 连接 ──────────────────────────────┐
│                                                │
│  连接名称  [开发服务器____________]             │
│  主机地址  [192.168.1.100_________]             │
│  端口      [22_____________________]             │
│  用户名    [tangmaoke______________]             │
│  私钥路径  [/home/tangmaoke/.ssh/id_]           │
│            （控制平面所在机器的私钥路径）         │
│  根路径    [/home/tangmaoke/projects]           │
│            （远程服务器上可浏览的起始目录）       │
│                                                │
│  [测试连接]  [取消]  [保存并连接]              │
└────────────────────────────────────────────────┘
```

##### C. Importer 增强

现有 Importer 已经支持多根路径选择（WSL Home / Windows 盘符）。增强后：

```
选择环境
├── 🐧 WSL Home  (/home/tangmaoke)
├── 🪟 Windows C: (/mnt/c)
├── 🪟 Windows D: (/mnt/d)
├── 🖥️ 开发服务器 (user@192.168.1.100)    ← 新增
└── 🖥️ 备用服务器 (user@10.0.0.50)        ← 新增
```

每个 SSH 连接的根路径从 `SSHConnection.rootPath` 字段获取。选择后进入目录浏览，后续流程与本地项目完全一致。

##### D. 项目卡片增强

```typescript
{project.environment === "remote-linux" && 
  <span className="env-tag remote" title="远程服务器">🖥️</span>}
```

`pathDisplay` 对于远程项目显示为 `"开发服务器:/home/user/projects/my-app"`。

##### E. 新增 TypeScript 类型

```typescript
type SSHConnection = {
  id: string;
  name: string;
  host: string;
  port: number;
  user: string;
  status: "unknown" | "connected" | "disconnected" | "error";
  lastSeen?: string;
  errorMsg: string;
  createdAt: string;
};

// RunnerInfo 扩展
type RunnerInfo = {
  id: string;
  name: string;
  environment: string;      // "wsl" | "windows" | "remote-linux"
  host?: string;            // 远程主机（SSH Runner 才有）
  root: string;
  roots: { name: string; path: string; label?: string }[];
  claude: {
    status: string;
    version: string;
  };
};
```

---

#### 3.4.5 对话执行流程（SSH 远程项目）

```
用户发送消息
  → POST /api/conversations/{id}/messages
  → Server 根据 project.runner 获取对应 Runner
    → 如果是 "wsl-local": claudeCLIRunner.StartSession() (本地执行)
    → 如果是 "ssh-abc123": sshRunner.StartSession()
        → SSH session 保持打开
        → 远程执行: claude -p --verbose --input-format stream-json --output-format stream-json --replay-user-messages
        → stdout 通过 SSH 管道流式返回
        → 控制平面将输出转换为 Event，通过 WebSocket 推送到前端
  → 前端实时渲染 Claude Code 的思考、工具调用、文件变更
```

**关键点**：
- Claude Code 在**远程服务器**上运行，其工作目录是远程项目路径
- 工具调用（Bash、文件读写）都在远程服务器上执行
- 审批流程不变——危险命令仍然需要用户在 Web 页面中确认
- 上下文和用量统计照常工作——从远程 Claude Code 的 stream-json 输出中提取

---

### 3.5 审批回调适配

#### 3.5.1 问题

当前 Claude Code CLI 通过 `AUTO_CONTROL_URL` 环境变量获知控制平面的地址，审批钩子回调该地址的 `/api/internal/approvals/wait` 等待用户决策。`StartSession()` 中设置：

```go
cmd.Env = append(os.Environ(),
    "AUTO_CONTROL_URL="+r.config.ControlURL,   // 默认 http://127.0.0.1:8080
    "AUTO_APPROVAL_CONVERSATION_ID="+request.ConversationID,
    "AUTO_APPROVAL_TOKEN="+request.ApprovalToken,
)
```

对于 SSH 远程 Runner，远程服务器上的 Claude Code 进程使用 `http://127.0.0.1:8080` 只会访问到自己，无法到达控制平面。

#### 3.5.2 解决方案

**方案**：在创建 SSH Runner 时将 `ControlURL` 替换为控制平面的外部可达地址。用户需通过 `AUTO_CONTROL_URL` 环境变量设置一个远程服务器可达的地址（如 `http://192.168.1.50:8080`）。

`sshRunner.StartSession()` 中覆盖该值：

```go
func (r *sshRunner) StartSession(ctx context.Context, req AgentSessionRequest) (AgentSession, error) {
    // 远程 Claude Code 需要能回调控制平面 —— 使用配置中的外部可达地址
    cmd := fmt.Sprintf(
        "cd %s && AUTO_CONTROL_URL=%s AUTO_APPROVAL_CONVERSATION_ID=%s AUTO_APPROVAL_TOKEN=%s claude --input-format stream-json --output-format stream-json --verbose",
        shellescape.Quote(req.ProjectPath),
        shellescape.Quote(r.controlURL),     // 外部可达地址
        shellescape.Quote(req.ConversationID),
        shellescape.Quote(req.ApprovalToken),
    )
    // ...通过 SSH session 执行
}
```

**约束**：
- 用户必须在启动控制平面时设置 `AUTO_CONTROL_URL` 为远程服务器可达的地址（如局域网 IP）
- 如果无法满足（如 NAT 后），审批模式改用 `full_control`（无需回调）
- 使用 `full_control` 模式时不存在此问题 —— 不需要审批回调

### 3.6 安全设计

| 安全点 | 设计 |
|--------|------|
| SSH 认证 | 只支持密钥认证，不存储密码 |
| 私钥安全 | 私钥路径存储在数据库，文件由用户自行管理权限（`chmod 600`） |
| 主机验证 | 首次连接时自动获取主机指纹，保存到 `known_hosts` 字段；指纹变更时拒绝连接 |
| 路径安全 | 远程浏览限制在 `SSHConnection.rootPath` 范围内；路径以 `rootPath` 为前缀始可访问 |
| 数据传输 | 所有 SSH 流量加密 |
| 凭据隔离 | 浏览器不接触 SSH 密钥；所有 SSH 操作在控制平面完成 |
| 审批回调 | 远程服务器的 Claude Code 需能通过网络访问控制平面的 `AUTO_CONTROL_URL`；否则使用 `full_control` 模式 |

### 3.7 错误处理与状态管理

| 场景 | 处理 |
|------|------|
| SSH 连接超时 | 前端显示"连接超时"，Runner 状态标记为 `disconnected` |
| 认证失败 | 前端显示"认证失败，请检查私钥"，状态标记为 `error` |
| 远程 Claude Code 未安装 | `validateProject` 返回 `claudeReady: false`，前端提示 |
| 网络中断 | Run 标记为 `interrupted`，与本地中断处理一致 |
| 主机指纹变更 | 拒绝连接，提示用户更新 `known_hosts` |
| 远程磁盘满/权限不足 | 映射为标准错误消息，前端展示 |

---

### 3.8 数据库迁移

新增一张表：

```sql
CREATE TABLE IF NOT EXISTS ssh_connections (
    id              text primary key,
    name            text not null,
    host            text not null,
    port            integer not null default 22,
    user            text not null,
    private_key_path text not null,
    known_hosts     text not null default '',
    root_path       text not null default '/',
    status          text not null default 'unknown',
    last_seen       datetime,
    error_msg       text not null default '',
    created_at      datetime not null,
    updated_at      datetime not null
);
```

`projects` 表**不需要修改**。`runner` 字段存储 `"ssh-{connection_id}"`，`path` 字段存储远程绝对路径。现有字段完全兼容。

---

## 4. 改动文件清单

### 4.1 新增文件

| 文件 | 说明 |
|------|------|
| `apps/control-server/internal/app/ssh_runner.go` | SSH Runner 实现（`sshRunner`），完整实现 `AgentRunner` + `StreamingAgentRunner` 接口 |
| `apps/control-server/internal/app/ssh_connection.go` | SSH 连接配置的 CRUD 和连接管理 |
| `apps/control-server/internal/app/runner_registry.go` | Runner 注册表，管理多个 Runner 实例的注册/注销/查询 |
| `apps/web/src/components/SSHConnectionManager.tsx` | SSH 连接管理前端组件 |

### 4.2 修改文件

| 文件 | 改动类型 | 说明 |
|------|---------|------|
| `apps/control-server/internal/app/app.go` | 修改 | `Server` 结构体：`runner` 单实例字段替换为 `runnerRegistry *runnerRegistry`；新增 SSH 连接和 Runner 注册相关 API 路由；`listRunners()` 改为从 registry 动态读取；`listDirectories()`/`validateProject()`/`createProject()` 支持多 Runner（新增 `runner` 参数）；`sendMessage()`/`runClaude()`/`submitStreamingRun()` 根据 `project.runner` 从 registry 获取对应 Runner 而非使用全局 `s.runner`；`checkClaudeUpdate()`/`updateClaude()` 新增 `runnerID` 参数；`migrate()` 新增 `ssh_connections` 表 |
| `apps/control-server/go.mod` | 修改 | 新增依赖：`golang.org/x/crypto/ssh`、`github.com/pkg/sftp` |
| `apps/web/src/App.tsx` | 修改 | `ProjectDashboard` 增加"SSH 连接管理"入口；`Importer` 支持 SSH 连接根路径；`ProjectCard` 增加远程项目标识；新增 `SSHConnection`/`RunnerInfo` 类型 |

### 4.3 不改动的部分

| 部分 | 原因 |
|------|------|
| `claude_runner.go` | WSL 本地 Runner 保持不变，SSH Runner 是新的独立实现 |
| `task.go` | 任务模型与 Runner 无关 |
| `git.go` / `git_operations.go` | 首期 SSH 远程项目的 Git 操作暂不支持（P2），后续单独设计 |
| `usage.go` | 用量统计逻辑与 Runner 无关 |
| WebSocket 实时推送 | 复用现有事件模型 |
| 对话页面 (`Chat`) | 完全复用，通过 `RunnerInfo.environment` 区分展示 |
| 前端 CSS | 新增少量远程环境标识样式即可 |

---

## 5. 实施步骤

### 第一阶段：基础设施（估计 3-5 天）

1. **依赖引入**：`go get golang.org/x/crypto/ssh github.com/pkg/sftp`
2. **SSH 连接管理**：
   - 新建 `ssh_connection.go`：`SSHConnection` 数据模型、CRUD API
   - 数据库迁移：新增 `ssh_connections` 表
   - API 路由注册：`/api/ssh-connections`
   - 首次连接时自动获取主机公钥（`ssh.Session.HostKey()`），序列化为 known_hosts 格式存入数据库
3. **SSH Runner 实现**：
   - 新建 `ssh_runner.go`：`sshClient` 封装（连接、SFTP、命令执行）
   - `sshRunner` 实现 `AgentRunner` 接口
4. **Runner 注册表**：
   - 新建 `runner_registry.go`：注册/注销/查询
   - 修改 `listRunners()` 从注册表动态读取
   - 启动时注册 `wsl-local`，遍历 `ssh_connections` 表中 `status='connected'` 的记录自动重连并注册对应 Runner
5. **前端 SSH 连接管理**：
   - `SSHConnectionManager` 组件：连接列表、添加/删除/测试连接
   - `ProjectDashboard` 增加入口

### 第二阶段：项目加载闭环（估计 2-3 天）

6. **目录浏览适配**：
   - `listDirectories()` 支持 `runner` 参数
   - SSH Runner 使用 SFTP 浏览远程目录
7. **项目校验适配**：
   - `validateProject()` 支持远程目录校验
8. **项目创建适配**：
   - `createProject()` 正确设置远程项目的 `runner`、`environment`、`pathDisplay`
9. **前端 Importer 增强**：
   - 支持 SSH 连接根路径选择
   - 远程目录浏览体验对齐本地

### 第三阶段：对话执行闭环（估计 3-5 天）

10. **SSH Runner 流式会话**：
    - 实现 `StartSession()` 和 `Run()` 方法
    - 通过 SSH session 管道维持 stream-json 双向通信
11. **审批流程适配**：
    - 确保远程 Claude Code 的审批回调能正确到达控制平面
12. **端到端测试**：
    - 添加 SSH 连接 → 浏览远程目录 → 加载项目 → 发送对话 → 查看实时过程
13. **错误处理完善**：
    - SSH 断开重连
    - 远程进程异常退出
    - 超时处理

### 第四阶段：体验优化（估计 2-3 天）

14. **连接状态监控**：定时检测 SSH 连接可用性，前端实时显示状态
15. **远程项目 Git 支持**（P2）：远程 Git 操作
16. **性能优化**：SFTP 缓存、连接池
17. **文档与验收**

---

## 6. 风险与限制

### 6.1 SSH 连接稳定性

- **风险**：网络不稳定导致 SSH 连接中断，正在执行的 Claude Code 会话丢失
- **缓解**：
  - Claude Code Run 中断后标记为 `interrupted`（复用现有机制）
  - 前端显示明确的连接状态指示器
  - 未来可考虑 `tmux`/`screen` 包装 Claude Code 进程，使连接断开不丢失会话

### 6.2 延迟问题

- **风险**：SSH 远程命令执行比本地多一层网络延迟，Claude Code 的流式输出可能有明显延迟
- **缓解**：
  - 前端已有 loading 状态，用户对延迟有预期
  - 选择地理位置较近的服务器

### 6.3 Claude Code 版本差异

- **风险**：远程服务器上的 Claude Code 版本与控制平面所在 WSL 的版本可能不同
- **缓解**：
  - `sshRunner.Version()` 返回远程版本
  - 前端 Runner 信息区显示远程版本号
  - 版本更新操作仅对当前 Runner 生效

### 6.4 远程环境依赖

- **风险**：远程服务器缺少 Claude Code 运行所需的依赖（Node.js、Git 等）
- **缓解**：
  - `validateProject()` 阶段检测 Claude Code 可用性
  - 明确文档说明远程服务器的前置要求

### 6.5 文件路径差异

- **风险**：远程 Linux 服务器的路径格式与 WSL 不同
- **缓解**：
  - `pathDisplay` 使用 `"{连接名}:{远程路径}"` 格式
  - 不尝试在控制平面访问远程路径（所有操作通过 SSH Runner）

---

## 7. 结论

**SSH 远程项目支持是项目从"单人本机工具"走向"多环境平台"的关键一步**。与 Windows 项目（文档 10）通过 WSL `/mnt/` 挂载复用的"轻量方案"不同，SSH 远程项目需要真正实现 Runner 分离——控制平面通过 SSH 协议与远程执行环境通信。

核心改动分为三个层面：

| 层面 | 改动 | 说明 |
|------|------|------|
| Runner 模型 | 新增 `sshRunner` + `runnerRegistry` | 从单一内嵌 Runner 演进为多 Runner 注册机制 |
| SSH 连接 | 新增 `ssh_connections` 表 + CRUD API | 管理远程服务器连接配置 |
| 前端体验 | Importer 增强 + SSH 连接管理面板 | 统一本地与远程项目的加载体验 |

总改动量约 800-1200 行代码（Go + TypeScript），不影响现有 WSL 本地项目的任何功能。实现后，用户可以在同一个 Web 控制台中管理本地和远程项目，使用完全相同的对话、任务和审批流程。

本方案与文档 10（Windows 项目目录支持）共同构成平台"多环境项目加载"的完整能力矩阵：

| 项目位置 | 实现方式 | Runner | 文档 |
|----------|---------|--------|------|
| WSL 本地 | `os/exec` 直接执行 | `wsl-local` | 04 |
| Windows 本机 | WSL `/mnt/` 挂载 | `wsl-local`（复用） | 10 |
| **SSH 远程 Linux** | **SSH + SFTP 远程执行** | **`ssh-{id}`（新增）** | **11（本文档）** |

后续演进方向：
1. **远程 Agent 模式**：将 SSH Runner 升级为独立 gRPC Agent，在远程服务器上常驻运行
2. **容器环境 Runner**：支持 Docker 容器内的项目
3. **Runner 自动发现**：通过 mDNS 或 Consul 自动发现局域网内的可用 Runner