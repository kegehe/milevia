# 支持 Windows 项目目录 — 设计与实现方案

> 日期: 2026-07-22
> 目标: 分析当前 WSL-only 项目加载机制，设计将本地 Windows 项目目录纳入平台的完整方案。

## 1. 现状分析

### 1.1 当前项目加载链路

```
前端 Importer 组件
  → GET /api/directories?path=...        (浏览目录)
  → POST /api/projects/validate           (校验目录)
  → POST /api/projects                    (创建项目)
  → 项目路径写入 projects.path 字段
  → Claude Code CLI 以 project.Path 为 cmd.Dir 执行
```

### 1.2 关键约束点

| 组件 | 文件 | 当前行为 | 问题 |
|------|------|---------|------|
| 路径安全 | `app.go:2235-2248` `allowedPath()` | 路径必须在 `AllowedRoot`（默认 `$HOME`）内才放行 | `/mnt/c/...` 不在 `$HOME` 下（`filepath.Rel("/home/tangmaoke", "/mnt/c/...")` 以 `..` 开头），会被拒绝 |
| 目录浏览 | `app.go:591-617` `listDirectories()` | 默认从 `AllowedRoot`（`$HOME`）开始，`os.ReadDir()` 可用 | 首次加载时无法越过 `$HOME` 进入 `/mnt/c/` 浏览 Windows 目录 |
| 项目校验 | `app.go:620-639` `validateProject()` | 校验 Git 分支和 Claude Code 可用性 | 跨文件系统性能差异 |
| 项目创建 | `app.go:642-679` `createProject()` | Runner 固定为 `wsl-local` | Windows 目录下的项目仍然使用 WSL 中的 Claude Code |
| Runner 标识 | `app.go:2283` `listRunners()` | 仅返回一个 Runner: `wsl-local` | 缺乏 Windows Runner 概念 |

### 1.3 WSL 中访问 Windows 目录的机制

WSL 通过 `/mnt/<盘符>/` 自动挂载 Windows 文件系统：
- `C:\` → `/mnt/c/`
- `D:\` → `/mnt/d/`

在 WSL 中，`os.ReadDir("/mnt/c/Users/tangmaoke/projects")` 完全可用。Go 的 `os/exec` 也可以在 `/mnt/c/...` 路径下执行 `claude` 命令。

**从架构文档 `02-架构设计.md` 第 139 行可知，项目设计之初就考虑到多环境 Runner：**
> "首期项目在 WSL 中，因此使用 WSL Runner。后续项目位于 Windows、本机 Linux、容器或远程服务器时，只增加对应环境的 Runner，不改变项目、任务和页面结构。"

### 1.4 现有 Runner 模型

```go
// claude_runner.go 中的 StreamingAgentRunner 接口（实际使用的接口）
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

当前唯一的 Runner 实现是 `claudeCLIRunner`（实现了 `StreamingAgentRunner`），它直接在 WSL 中执行 `claude` CLI。

## 2. 需求分析

用户希望将 Windows 本地的项目目录纳入平台管理，例如：
- `C:\Users\tangmaoke\projects\my-web-app`
- `D:\repos\backend-service`

这些目录在 WSL 中对应的路径是：
- `/mnt/c/Users/tangmaoke/projects/my-web-app`
- `/mnt/d/repos/backend-service`

**核心发现：Windows 项目目录无需独立的 Windows Runner，可以复用 WSL Runner。** 原因：

1. WSL 已经通过 `/mnt/` 挂载了所有 Windows 盘符
2. Claude Code CLI 本身就在 WSL 中运行，它完全可以操作 `/mnt/c/...` 路径下的文件
3. Git 操作、文件读写、命令执行等所有能力在 `/mnt/` 路径下同样可用

因此实现复杂度大幅降低——**不需要新建 Runner，只需要调整路径浏览和项目管理的部分逻辑**。

## 3. 设计方案

### 3.1 设计原则

- **最小改动**: 复用现有架构，不引入新的 Runner 类型
- **渐进增强**: 先解决"能加载 Windows 目录"的核心需求，后续再考虑独立 Windows Runner
- **安全边界不变**: 仍然通过 `AllowedRoot` 控制可访问范围

### 3.2 方案概述

**方案核心思路**：让前端能感知到 `/mnt/c/`、`/mnt/d/` 等 Windows 盘符入口，用户可以从这些入口浏览 Windows 目录并加载项目。后端不需要大改——只要能访问 `/mnt/` 路径即可。

### 3.3 各层改动

#### 3.3.1 控制服务器 (control-server)

##### A. 新增 Runner 信息：暴露可访问的根路径

**文件**: `app.go` 中的 `listRunners()`

当前只返回一个 Runner：
```go
writeJSON(w, http.StatusOK, []map[string]any{{
    "id":          "wsl-local",
    "name":        "WSL Local Runner",
    "environment": "wsl",
    "root":        s.config.AllowedRoot,
    ...
}})
```

**改动**：增加 `roots` 字段，列出 WSL 中可访问的多个根路径（`$HOME` + `/mnt/` 下的可用盘符）。

```go
func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
    // ... 现有逻辑 ...
    roots := []map[string]string{
        {"name": "WSL Home", "path": s.config.AllowedRoot},
    }
    // 检测可用的 Windows 盘符
    for _, drive := range []string{"c", "d", "e", "f"} {
        mntPath := filepath.Join("/mnt", drive)
        if info, err := os.Stat(mntPath); err == nil && info.IsDir() {
            roots = append(roots, map[string]string{
                "name":  fmt.Sprintf("Windows (%s:)", strings.ToUpper(drive)),
                "path":  mntPath,
                "label": "windows",
            })
        }
    }
    writeJSON(w, http.StatusOK, []map[string]any{{
        "id":          "wsl-local",
        "name":        "WSL Local Runner",
        "environment": "wsl",
        "root":        s.config.AllowedRoot,
        "roots":       roots,
        "claude":      ...
    }})
}
```

##### B. 调整 `AllowedRoot` 默认值或允许跨根访问

当前 `allowedPath()` 只允许 `AllowedRoot`（默认 `$HOME`）下的路径。需要将 `/mnt/c/` 等路径也纳入白名单。

**方案选择**：

| 方案 | 改动 | 优点 | 缺点 |
|------|------|------|------|
| a) 放宽 `AllowedRoot` 为 `/` | 改环境变量默认值 | 简单直接 | 安全范围过大，WSL 中所有文件可被访问 |
| **b) `allowedPath()` 支持多根** | 修改 `allowedPath()` 函数 | 精确控制、不改环境变量、向后兼容 | 需要改验证逻辑 |
| c) `AllowedRoot` 多值（逗号分隔） | 改环境变量格式 + allowedPath | 用户可自由配置 | 复杂度略高，当前阶段不需要 |

**推荐方案 b**（多根路径验证）：修改 `allowedPath()` 支持多个允许的根，既保持现有安全策略，又能精确控制哪些路径可访问。不需要改环境变量默认值。

更精确的实现——修改 `allowedPath()` 函数，支持多个允许的根：

```go
func (s *Server) allowedPath(path string) (string, error) {
    absolute, err := filepath.EvalSymlinks(path)
    if err != nil {
        return "", err
    }
    // 允许的根路径列表
    allowedRoots := []string{s.config.AllowedRoot}
    // 添加 Windows 盘符路径
    for _, drive := range []string{"c", "d", "e", "f"} {
        mntPath := filepath.Join("/mnt", drive)
        if info, err := os.Stat(mntPath); err == nil && info.IsDir() {
            allowedRoots = append(allowedRoots, mntPath)
        }
    }
    for _, root := range allowedRoots {
        resolvedRoot, err := filepath.EvalSymlinks(root)
        if err != nil {
            continue
        }
        relative, err := filepath.Rel(resolvedRoot, absolute)
        if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
            return absolute, nil
        }
    }
    return "", errors.New("path is outside all allowed roots")
}
```

##### C. 项目创建时标注环境

在 `createProject()` 中，根据项目路径自动标注运行环境。当前 `runner` 字段固定为 `"wsl-local"`，但由于 Windows 目录仍通过 WSL 的 `/mnt/` 访问，实际执行者不变。建议新增 `environment` 字段区分：

```go
// 在 Project 结构体中增加
Environment string `json:"environment"` // "wsl" 或 "windows"

// createProject 中
env := "wsl"
if strings.HasPrefix(path, "/mnt/") {
    env = "windows"
}
p := Project{..., Environment: env, ...}
```

`runner` 字段保持 `"wsl-local"`（因为实际执行仍在 WSL 中），前端通过 `environment` 字段区分展示。这为后续真正分离 Windows Runner 时预留了语义空间。

##### D. 性能考量：跨文件系统访问

`/mnt/` 路径下的文件访问性能不如原生 Linux 文件系统（ext4）。主要影响：

| 操作 | 影响 | 缓解 |
|------|------|------|
| `git status` | 大仓库较慢 | 缓存 + 选择性刷新 |
| `os.ReadDir()` | 首次浏览略慢 | 前端已有 loading 状态 |
| Claude Code 读写文件 | 明显慢于原生 | 可提示用户，但不阻止 |

建议在 `validateProject()` 返回中增加一个 `performance` 字段提示用户：
```json
{
  "path": "/mnt/c/Users/...",
  "performance": "cross-filesystem"  // 或 "native"
}
```

#### 3.3.2 前端 (web)

##### A. Importer 组件增强：选择 Runner/盘符

**文件**: `App.tsx` 中的 `Importer` 组件

当前 Importer 直接打开 `/api/directories` 浏览目录。改为先展示可用的根路径入口：

```
加载项目
├── 🐧 WSL Home  (/home/tangmaoke)
├── 🪟 Windows C: (/mnt/c)
└── 🪟 Windows D: (/mnt/d)
```

修改 `Importer` 组件：先调用 `GET /api/runners` 获取 `roots` 列表，让用户选择一个入口，再从该根路径开始浏览目录。

**改动点（伪代码）**：

```typescript
function Importer({ close, created, fail }) {
  const [runners, setRunners] = useState<any[]>([]);
  const [activeRoot, setActiveRoot] = useState<string>("");

  // 首次加载时获取 runners 信息
  useEffect(() => {
    api("/api/runners").then(setRunners).catch(...);
  }, []);

  // 用户选择根路径后，从该路径开始浏览
  const selectRoot = (rootPath: string) => {
    setActiveRoot(rootPath);
    browse(rootPath);
  };

  if (!activeRoot) {
    // 显示根路径选择界面
    return <RootSelector runners={runners} onSelect={selectRoot} />;
  }
  // ... 现有的目录浏览界面
}
```

##### B. 项目卡片增强：显示来源标识

前端 `Project` 类型当前只有 `pathDisplay` 字段（目录名），无法区分 Windows 和 WSL 项目。需要后端在项目数据中增加标识。

**方案**：在 `Project` 结构体和 API 响应中增加 `environment` 字段：

```go
// app.go 中的 Project 结构体
type Project struct {
    // ... 现有字段 ...
    Environment string `json:"environment"` // "wsl" 或 "windows"
}
```

创建项目时赋值：
```go
env := "wsl"
if strings.HasPrefix(path, "/mnt/") {
    env = "windows"
}
p := Project{..., Environment: env, ...}
```

前端 `ProjectCard` 中根据 `environment` 字段显示标识：

```typescript
{project.environment === "windows" && <span className="env-tag windows">🪟 Windows</span>}
```

同时 `PathDisplay` 优化后（见 C 节），Windows 项目会直接显示 `C:\Users\...` 格式的路径，本身就具有辨识度。

##### C. PathDisplay 优化

当前 `PathDisplay` 使用 `filepath.Base(path)`，仅显示目录名（如 `my-app`）。对于 Windows 路径 `/mnt/c/Users/tangmaoke/projects/my-app`，仅显示 `my-app` 就丢失了盘符信息。

方案：在 `createProject()` 和 `listProjects()` 中增加判断——对 `/mnt/` 下的路径，`PathDisplay` 转为 Windows 原生格式显示完整路径：

```go
func projectPathDisplay(absPath string) string {
    if strings.HasPrefix(absPath, "/mnt/") {
        // /mnt/c/Users/tangmaoke/projects/my-app → C:\Users\tangmaoke\projects\my-app
        drive := strings.ToUpper(string(absPath[5]))  // "c" → "C"
        remainder := absPath[6:]                       // "/Users/tangmaoke/projects/my-app"
        if remainder != "" && remainder[0] == '/' {
            remainder = remainder[1:]
        }
        return drive + ":\\" + strings.ReplaceAll(remainder, "/", "\\")
    }
    // WSL 路径保持仅目录名
    return filepath.Base(absPath)
}
```

**注意**：`PathDisplay` 在项目卡片（`ProjectCard`）中作为"工作目录"直接显示（见 `App.tsx:527`），所以当值从目录名改为完整 Windows 路径后，前端卡片可能因路径过长而溢出行高。建议同时调整 CSS：对 `pathDisplay` 设置 `text-overflow: ellipsis` 和 `max-width`。区分 Windows/WSL 项目的标识使用 `environment` 字段（见 B 节），无需通过路径格式判断。

#### 3.3.3 数据库

**无需修改数据库 schema**。现有 `projects` 表结构完全兼容：

```sql
projects (id, name, path, runner, git_branch, claude_ready, created_at)
```

`path` 字段存储 WSL 中的绝对路径（如 `/mnt/c/Users/...`），`runner` 字段保持 `wsl-local`。前端仍然通过 `pathDisplay` 展示。

### 3.4 改动文件清单

| 文件 | 改动类型 | 说明 |
|------|---------|------|
| `apps/control-server/internal/app/app.go` | 修改 | `listRunners()` 增加 roots；`allowedPath()` 支持多根；`Project` 结构体增加 `Environment` 字段；`createProject()`/`listProjects()` 优化 pathDisplay |
| `apps/web/src/App.tsx` | 修改 | `Importer` 组件增加根路径选择；`ProjectCard` 增加环境标识；`Project` 类型增加 `environment` 字段 |
| `docs/10-Windows项目目录支持.md` | 新建 | 本文档 |

### 3.5 不改动的部分

- `claude_runner.go` — 不需要修改，Claude Code 在 WSL 中可以直接操作 `/mnt/` 路径
- `git.go` / `git_operations.go` — 不需要修改，Git 命令在 `/mnt/` 路径下同样可用
- `task.go` — 不涉及路径逻辑
- 数据库 schema — 完全兼容
- WebSocket 实时通信 — 不受影响

## 4. 风险与限制

### 4.1 跨文件系统性能

WSL2 通过 9P 网络文件系统协议（`drvfs`）访问 Windows 文件系统，性能显著低于原生 ext4：
- 大量小文件读写：明显慢于 WSL 原生文件系统
- Git 操作：大仓库（数百 MB 以上）`git status` 可能明显卡顿
- `npm install` / `pip install`：node_modules 等大量小文件的安装会很慢

当前系统实际挂载参数：`type 9p (rw,noatime,aname=drvfs;path=C:\;uid=1000;gid=1000;...)`

**建议**：在前端给 Windows 项目打上性能标签，提示用户。对于需要频繁编译或大量 IO 的项目，建议将代码复制到 WSL 原生文件系统（`~/projects/`）中。

### 4.2 路径权限

Windows 目录中的文件权限模型与 Linux 不同。WSL 默认将 `/mnt/` 下的文件映射为当前 WSL 用户所有（通过 `metadata` 挂载选项），但 `chmod` 等操作可能无效。这可能影响某些需要特定权限位的工具。

### 4.3 符号链接

Windows 和 Linux 的符号链接不互通。如果项目中有符号链接，在 `/mnt/` 下可能不可用。

## 5. 实施步骤

### 第一阶段：快速可用（估计 2-3 小时）

1. 修改 `allowedPath()` 支持多根路径，或放宽 `AllowedRoot` 默认值
2. 修改 `listRunners()` 返回可用 Windows 盘符
3. 前端 `Importer` 增加根路径选择 UI
4. 测试：浏览 `/mnt/c/` 目录，加载一个 Windows 项目，发送一次对话

### 第二阶段：体验优化（估计 2-3 小时）

1. `PathDisplay` 优化为 Windows 原生格式显示
2. 项目卡片增加来源图标
3. `validateProject()` 增加跨文件系统性能提示
4. 前端响应式适配（小屏幕上根选择器布局）

### 第三阶段：后续演进（可选）

1. 真正的 Windows Runner：在 Windows 上运行 `claude` CLI，通过 gRPC 与控制平面通信（架构文档已规划）
2. 跨文件系统性能警告阈值：大型仓库自动提示
3. 项目位置迁移：支持将 Windows 项目"克隆"到 WSL 原生文件系统以改善性能

## 6. 结论

**支持 Windows 项目目录不需要架构级别的改动**。WSL 天然挂载了所有 Windows 盘符到 `/mnt/`，Claude Code CLI 和所有 Git 操作都可以直接在 `/mnt/` 路径下工作。

核心改动只有两点：
1. **放宽 `allowedPath()` 的安全边界**——允许 `/mnt/` 路径
2. **增强前端 Importer**——让用户可以选择从哪个根路径开始浏览目录

总改动量小（约 100-150 行代码），不影响现有 WSL 项目的工作流程，且为后续真正的多环境 Runner 打下基础。
