# 移动端 Git 管理 — 调研与实施方案

> 日期：2026-09-21
>
> 状态：**待评审**（未动代码）。**已经过一轮自复查，见 §13** —— 那里改掉了 2 处过度设计、
> 1 处过度自信，补上 2 处漏项；实施时请以 §13 之后的版本为准。
>
> 关联文档：[08-Git仓库管理与协作](./08-Git仓库管理与协作.md)（桌面端 Git 工作台，本方案复用它的全部 handler）、
> [40-移动端文件查看与编辑调研与实施方案](./40-移动端文件查看与编辑调研与实施方案.md)（**本方案照抄它的通道形态**）、
> [32-移动端App与云端远程控制终极方案](./32-移动端App与云端远程控制终极方案.md)、
> [38-一手机多电脑绑定方案](./38-一手机多电脑绑定方案.md)
>
> 本文把第 32 篇「首期不做：移动端完整 Git 工作台」这条边界收回。**不重新设计 Git 领域能力** ——
> 第 08 篇的 27 个 REST 端点、`stateToken` 乐观锁、工作区租约、`git_operations` 审计全部沿用；
> 本文只回答「这 28 个端点怎么过中继，以及手机端那屏长什么样」。

## 0. 一句话结论

**技术可行，而且比文件视图更容易。** 原因只有一条，但很硬：

> `GitWorkbench` 的取数和 `FilesPanel` 是同一个形状 —— 只依赖一个
> `request: <T>(path, init) => Promise<T>` 注入口（`GitWorkbench.tsx:46`）。
> 第 40 篇为文件写的那套「REST → op 适配器」可以在 Git 上**原样重演**，
> 于是变更列表、diff 渲染、提交历史、提交详情、分支、操作记录、冲突解决
> 可以**整块复用**，不必为手机再写一套 Git 界面。

真正的难点不在功能，在四个数字和一个语义：

| 项 | 桌面端 | 手机端 | 后果 |
| --- | --- | --- | --- |
| 单条 git 命令超时 | 30s（`git.go:19`） | RPC 通道 20s（`fsrpc.go:47`） | **push/fetch 必然超时**，而电脑还在跑 |
| 前端 HTTP 超时 | 15s（`api.ts:5`） | 同左（沿用） | 桌面端其实也已在超时边缘 —— 见 §3.2 |
| 中继帧上限 | — | 384 KiB | 大 diff / lockfile diff 打不过去 |
| 请求并发闸门 | — | 4（`fs_relay.go:34`） | **进入 Git 视图的第一次加载是 5 个并发请求** ⇒ 必有一个吃 503，而被拒的那条恰好是会被 `.catch()` 静默吞掉的 `conflicts`（§3.3） |
| `stateToken` 有效期 | 2 分钟（`git_operations.go:1164`） | 同左 | 手机上停在变更列表超过 2 分钟再点「暂存」→ 409 |

后三条都是**在手机上才会暴露、且必须专门处理**的，本方案逐条给了做法。

## 1. 已核实的既有资产（实现时直接引用，不用再查）

### 1.1 桌面端 Git 工作台是可注入的

```ts
// apps/web/src/features/git/GitWorkbench.tsx:46
export function GitWorkbench({ projectID, conversationId, request, fail, active }): JSX.Element
// apps/web/src/features/git/GitWorkbench.tsx:220  —— GitBar 也是同一个形状
// apps/web/src/features/git/ConflictSolveView.tsx:21 —— 冲突解决视图同
```

三者都只通过 `request(path, init)` 取数，所有 `/api/projects/{id}/git/*` 都过它。
`GitWorkbenchPage.tsx:16` 注入的是 `api`（`lib/api.ts` 的 fetch 封装）。
**手机端换掉这个注入口，其余不动。**

### 1.2 电脑端的 Git 能力是完整的 28 个端点

`app.go:1132-1159` 注册（**读 11 + 写 17 = 28**；调研时数成 9+18=27，实现时逐条点过一遍改正）：

| 类别 | 端点 |
| --- | --- |
| 只读 | `summary`、`changes`、`diff`、`log`、`commits/{oid}`、`commits/{oid}/diff`、`branches`、`operations`、`conflicts`、`conflicts/content` |
| 变更 | `stage`、`unstage`、`stage-all`、`unstage-all`、`commit`、`commits/amend`、`discard` |
| 远端 | `fetch`、`pull`、`push` |
| 分支 | `POST branches`（新建）、`switch` |
| 冲突 | `resolve`、`abort`、`continue`、`suggest`、`suggestions/{id}`、`suggestions/{id}/cancel` |

### 1.3 写操作是**同步**的，但**先落库再执行**

`executeGitOperationForWorkspace`（`git_operations.go:1038-1048`）的顺序是：

```
insert git_operations (queued)  →  update (running)  →  execute(runner)  →  update (终态)
```

并在持租约的请求里同步返回 `202 + {operationId, status, errorMessage}`。
**这条顺序是本方案的救命条款**：手机端就算等到超时，电脑端那条操作也已经留下了
可查的审计记录 —— 所以「超时」不等于「失败了」，也不等于「没发生」。

### 1.4 其余关键事实

| 事实 | 位置 | 对本方案的意义 |
| --- | --- | --- |
| `stateToken` 绑 `projectID + workspaceID + 路径`，**TTL 2 分钟**，存在内存 | `git_operations.go:1162-1190` | 不绑会话/用户 ⇒ 可以安全地过手机；但过期与电脑重启都会 409 |
| 工作区由 `projectID` + **可选** `conversationId` query 决定 | `git_operations.go:1400` 走 `resolveRequestWorkspaceFromRequest` | 与文件视图同构，**必须绑定工作区** |
| `pull` 实际是 `pull --prune --ff-only` | `git.go:332` | **不会产生 merge 冲突** ⇒ 手机端放它是低风险的 |
| 冲突解决建议是**异步**的（202 + 轮询 status） | `git_conflict_suggest.go:216/224/265` | 不会撞 20s 超时，天然适配 |
| 写操作全程持项目工作区租约 | `gitPathsMutation` 的 `defer release()` | AI 在跑时写操作会 409（与文件写入同语义） |
| `git.css` 已有 `@media (max-width: 680px)` 单列化 | `git.css:393-425` | 手机 390px **落在这个断点内**，窄屏布局已有一半 |
| `GitBar` 的操作记录筛选用了两个原生 `<select>` | `GitWorkbench.tsx:300` | 违反手机端「不用原生 `<select>`」的既有规矩，**必须改** |
| 手机端项目快照只有 `gitBranch` 一个 Git 字段 | `remote_control.go:74` | 想做「N 个改动 · ↑2」角标就得额外一次请求，**本期不做**（理由见 §7.4） |
| `GitRepositoryState` 目前只有 `ready` 一个值 | `git.go:41-45` | 非 Git 项目走的是**报错路径**而不是状态字段 ⇒ 手机端空态必须按错误分支写（§7.5） |
| SSH runner 离线时回 `{"error":"runner_offline"}` | `git_operations.go:1417` | 手机端会看到英文枚举（**既有问题**，桌面端同样，第 40 篇 §15 已登记） |

### 1.5 一件必须现在决定的事：文件通道**还没发布**

| 对象 | 线上版本 | 发布日期 |
| --- | --- | --- |
| 桌面端 | 0.1.7 | 2026-09-19 |
| 手机端 | 0.1.8 | 2026-09-19 |

而第 40 篇那套通道（`remote_fs.go` / `fsrpc.go` / `fs_relay.go` /
`MobileRemotePage.tsx` 的改动）**全部还在工作区里未提交**（`git status` 逐条核对过：
`app.go`、`remote_control.go`、`agent.go`、`cloud-control/server.go`、`MobileRemotePage.tsx`
等均为 ` M`，三个新文件为未跟踪）。

⇒ **现在给通道改名不需要任何兼容负担**。这个窗口一旦发布就关上了，所以下面的
决策 1 必须在本期一次做完。

## 2. 硬约束（实现时必须当成前提）

| 约束 | 数值 | 来源 | 影响 |
| --- | --- | --- | --- |
| 单条 git 命令上限 | 30s | `git.go:19` `gitCommandTimeout` | 一次写操作最长 30s，且全程持租约 |
| RPC 等待上限 | 20s | `cloud-control/fsrpc.go:47` | **写操作会超时**，必须按 op 分档 |
| 中继帧上限 | 384 KiB（两端同值） | `fsrpc.go:43` / `fs_relay.go:29` | 大 diff 打不过去 |
| Agent WS 读限 | 512 KiB | `cloud-control/server.go` `agentConnect` | 超限**掐断整条中继连接**（不只是这次请求失败） |
| 请求并发 | 4 | `fs_relay.go:34` `fsRequestConcurrency` | 进 Git 视图首次加载 5 个并发 ⇒ 必有一个 503 |
| `stateToken` TTL | 2 分钟 | `git_operations.go:1164` | 手机上很容易越过 |
| 快照 | 32 MiB，事件驱动整份重传 | 第 40 篇 §3 | **Git 状态不进快照** |
| 对象存储 | 无 | — | 大 diff 没有旁路 |

## 3. 必须先回答的四个问题

### 3.1 决策 1：中继通道要不要改名？（**建议：要**）

现状：通道的四层名字全是 `fs` 专用词，而它即将同时承载 Git。

```
云端路由        POST /v1/instances/{id}/fs        server.go:513
帧类型          kind: "fs.request" / "fs.response"
Agent 端点      POST /api/remote/fs               fs_relay.go:102
本机端点        POST /api/remote/fs               remote_fs.go
op 名单         remoteFSOperations()              remote_fs.go:39
```

**问题不是审美，是名字变成谎话。** 第 40 篇已经把这条写进项目纪律：

> 一句已经失实的「这里没有文件访问」比没有注释更危险 —— 后来的人会据此放松审查。
> 以后动这个命名空间必须同时更新那段说明。（`REMOTE-CONTRACT.md`）

一个叫 `/api/remote/fs` 的端点同时提供 push / discard / switch branch，比
「这里没有文件访问」更危险：审查的人会按「fs = 项目沙箱内的文件读写」去评估它。

**建议改名（纯机械重命名，一次做完）**：

| 现在 | 改成 |
| --- | --- |
| `POST /v1/instances/{id}/fs` | `POST /v1/instances/{id}/rpc` |
| `kind: "fs.request" / "fs.response"` | `kind: "rpc.request" / "rpc.response"` |
| `POST /api/remote/fs`（Agent → 本机，两层） | `POST /api/remote/rpc` |
| `remoteFSOperations()` | `remoteFSOperations() ∪ remoteGitOperations()`，由 `remoteOperations()` 合成 |
| `fsRPCFrameLimit` / `fsResponseFrameLimit` | `rpcFrameLimit` / 同名 |
| `fsRequestHub` / `fsSlots` / `fsRequestConcurrency` | `rpcRequestHub` / `rpcSlots` / `rpcRequestConcurrency` |
| `requestId` 前缀 `fsr-` | `rpc-`（与命令 id 的 `cmd-` 分开，日志里一眼可辨） |
| `lib/`… `mobileFsReply` / `MobileFsTransport` | 抽成 `features/remote/mobile-rpc.ts`，fs 与 git 适配器共用 |

**必须同时保住的三条不变式**（改名不许动摇）：

1. **Agent 仍然不认识 op** —— 它只把帧转给**一个**端点。所以不能改成
   「按 op 前缀选 `/api/remote/fs` 还是 `/api/remote/git`」，那等于把 op 映射
   搬进 Agent，就是第 40 篇刻意避开的那类结构风险。
2. **云端仍然不认识 op 名单** —— 只鉴权、限长、判在线。
3. **op 名单仍然只有一份，且在执行点**（control-server）。

⇒ 形态是「**一条通道、一个端点、一张由多个领域合成的 op 表**」，
而不是「两条通道」或「两个端点」。合成函数是本项目里**唯一**知道
「哪些 op 归哪个领域」的地方：

```go
// remote_relay.go（新）
type remoteOperation struct {
    Method   string
    Path     string   // /api/projects/{projectID} 之后的片段，可含 {oid} 占位
    Query    bool     // params 是查询参数还是整个请求体
    Params   []string // 需要从 params 里替换进 Path 的路径参数（白名单，见 §5.2）
    MaxBytes int      // 该 op 允许的最大响应体（见 §4.4）；0 表示不限
    Handle   http.HandlerFunc
}

func (s *Server) remoteOperations() map[string]remoteOperation {
    merged := map[string]remoteOperation{}
    for name, op := range s.remoteFSOperations()   { merged[name] = op }
    for name, op := range s.remoteGitOperations()  { merged[name] = op }
    return merged
}
```

**这一条是本方案里最弱的一条建议，所以把两个方案摆平了说。**

| 方案 | 改什么 | 代价 | 收益 |
| --- | --- | --- | --- |
| **A. 全改**（含线上名字） | 路由、帧名、端点、Go 标识符 | 动到刚验证过的 fs 链路（探针 40/40、前端 615 条）；**且这批改动还没提交**，一次大范围重命名混在未提交的工作区里，会让 review 变难 | 名字与事实一致 |
| **B. 只改内部名**（备选，同样成立） | 只改 Go 标识符（`fsRequestHub` → `relayRequestHub` 等）+ 合成 op 表 | 近乎零 | 内部读者不被误导；线上名字保持稳定 |
| C. 什么都不改 | — | 零 | 无 |

**真正的风险不是名字，是「审查的人据此放松标准」** —— 而项目既有的对策是
`app.go` 里那段边界说明（第 40 篇已经写过一次「一句失实的注释比没有注释更危险」）。
只要那段说明如实写清「这里现在也提供 Git 操作」，**B 就足以覆盖这个风险**。

⇒ 我的建议：**如果要改，先把第 40 篇那批改动提交掉，让重命名成为一次可单独 review 的
纯机械改动**（AGENTS.md 规定我不主动提交，这一步你需要自己做）。若你希望这次改动的
diff 尽可能小、或不想在交付前再动已验证过的代码，**选 B 即可，方案其余部分不受影响**。

### 3.2 决策 2：写操作的超时怎么办？（**建议：按 op 分档 + 「超时≠失败」**）

三个数字叠在一起必然出事：

```
git 命令上限 30s   >   RPC 等待 20s   >   桌面端 HTTP 超时 15s
```

也就是说**同一个 fetch/push，在桌面端其实也已经在超时边缘**（`api.ts:5` 的 15s，
且只有 GET 有重试资格，POST 一次就放弃）。这不是本方案引入的问题，
但手机端必须正面处理，因为它没有「点第二次」那么廉价。

**做法（两条，都要）：**

1. **客户端提议超时 + 云端夹取。** 信封加一个可选字段 `timeoutMs`；云端
   `clamp(timeoutMs, 1s, 120s)` 后作为本次等待上限。
   这样**云端仍然不需要认识 op**（它只夹一个数），而每类 op 的超时由
   **唯独知道 op 语义的那一端**（适配器）决定：

   | op 类别 | 超时 | 依据 |
   | --- | --- | --- |
   | 只读（summary/changes/diff/log/branches/operations/conflicts） | 20s | 一条 `git status`/`git diff`，本地毫秒级，SSH 项目 SFTP 往返几十到几百 ms |
   | 本地写（stage/commit/discard/switch/createBranch/conflict.*） | 45s | `gitCommandTimeout` 30s + 通道与库余量 |
   | 网络写（fetch/pull/push） | 60s | 同上 + 远端握手；仍是 30s 命令上限在兜底 |
   | 冲突建议（suggest，异步） | 20s | 立刻返回 202 |

2. **超时是一档独立的界面状态，不是错误。** 文案必须说清「电脑还在跑」，
   而不是「失败」：

   > 电脑还在执行这条操作（可能是在与远端通信）。**它不会因为这里断开而停下** ——
   > 稍后在「操作记录」里能看到结果。

   这句是可以这么说的，依据就是 §1.3 那个「先落库再执行」的顺序：
   `git_operations` 里那条记录在超时之前就已经写进去了，手机端重进视图
   `git.operations` 一定能查到它。

> ⚠️ 不要用「超时后自动重发」来掩盖：`push` 重发会拿到非 fast-forward；
> `commit` 重发会拿到 `stateToken` 失效。重发是错的方向，**如实告知 + 可查**才是对的。

### 3.3 决策 3：并发闸门 4 撞上「首次加载 5 个请求」（**建议：抬闸门 + 给冲突补「读不到」档；不要串行**）

`GitWorkbench` 的 `reload()`（`:94-98`）在一个 `Promise.all` 里发 **5 个**请求：

```
/api/projects/{id}/git/summary
/api/projects/{id}/git/changes
/api/projects/{id}/git/branches
/api/projects/{id}/git/operations
/api/projects/{id}/git/conflicts      ← 已经 .catch(() => null)
```

而 Agent 侧的并发闸门是 **4**，满了立刻回 503：

> 电脑端同时处理的文件请求太多，请稍后重试

**这里有一个比「某个请求失败」严重得多的问题**：那第 5 条被拒的请求，
恰好落在**唯一一条带 `.catch(() => null)` 的调用**上（`GitWorkbench.tsx:98`）。
异常被吞掉之后 `conflictOverview` 是 `null`，界面**一个字都不说** ——
于是：**仓库真的处在冲突中时，手机端会显示成一个正常的、没有冲突的仓库。**

这正是本项目明令禁止的那件事（把「读不到」写成「没有」），而且触发是**竞态的**
（5 条里哪条被拒不确定）⇒ 症状是「这次能看见、下次看不见」的抖动。

**做法（两步，缺一不可）：**

1. **闸门抬到 8。** 4 这个数是按「文件视图会并发发树 + 打开文件 + 搜索」定的，
   Git 视图的天然批次就是 5。抬到 8 仍是有界的小数，不会压住本机服务
   （真正串行的是本机那条唯一 SQLite 连接，而 git 读是只读的、互不加锁）。
   **但这只是让概率变小，不是修复** —— 所以第 2 步才是重点。
2. **给 `conflicts` 加「读不到」这一档，去掉那个静默的 `.catch`。**
   冲突概览必须有三态：读到（有/无冲突）、读取中、**读不到**。
   「读不到」要显式渲染成一行说明（并在有未解析冲突文件时进一步降级为警示），
   绝不能落回「没有冲突」。

> ⚠️ **不要用「适配器串行」来解决**（本方案第一版是这么写的，复查后否掉了）。
> 串行的代价是**确定要付**的：5 条请求各是一次 RPC 往返（几百毫秒到一秒），
> 串起来把首屏从约 1 次往返拖到 5 次，在大仓库上（`summary` 还要对每个变更文件做
> `lstat` 指纹）体感明显。而它换来的「本机不被打扰」是个**假设**，不是测量结果。
> 更要紧的是：串行**根本修不掉第 2 条** —— 只要哪天卡住、超时或被拒，
> 那个静默的 `.catch` 照样把冲突藏起来。
>
> 顺带纠正我自己用错的一处类比：第 40 篇把 `/fs/stat` + `/fs/read` 合成一次 `fs.open`
> 是对的，因为那是**有先后依赖的两跳**（先看 `isText` 才能决定读不读）。
> Git 这 5 条是**互相独立的并行请求**，没有依赖关系，合并省不掉延迟 ——
> 所以**也不要为此新增一个 `git.overview` 聚合 op**，那是为省往返数量
> 引入一份必须与 5 个单端点保持同步的新契约。

### 3.4 决策 4：`stateToken` 只有 2 分钟（**建议：写前无条件刷新 + 409 自动重取**）

手机上的典型节奏是「看一眼 diff（30s）→ 想想（60s）→ 回去改两个文件（40s）→ 点提交」——
**2 分钟太容易越过了**。越过之后服务端回：

```
409  Git state changed; refresh the repository
```

这句是英文，而且完全指不到"过期了"这个原因。三种做法里选中间那条：

| 做法 | 评价 |
| --- | --- |
| 延长 TTL | **不做**。2 分钟是安全设计（token 绑定的是"当时那份状态"），延长等于放宽乐观锁 |
| 让用户自己关掉重开 | 差：症状是"点了提交报一句英文" |
| **✅ 写前无条件刷新 + 409 自动重取** | 手机端能自己修好，且不改变服务端语义 |

具体：

1. **每一次写操作之前，先取一次 `git.summary` 拿新鲜 `stateToken`，再发写请求。**
   无条件，不看时钟。
   第一版方案在这里写的是「距 `observedAt` > 90s 才重取」，复查后否掉：
   那个 90s 是拍的，而它省下的只是**写操作前的一次读** —— 写操作本就是用户主动、
   低频的动作，多付一次往返（几百毫秒，同时给界面一个「正在确认仓库状态…」的进度感）
   换掉一个魔法数加一类漏判，是划算的。
   > 万一实测证明大仓库上这次额外读太慢（比如 >2s），**再**引入时间阈值 ——
   > 那时它是有测量支撑的优化，而不是猜的。
2. 仍然收到 409 且 `error` 匹配「state changed」时：**自动 `reload()` 一次**，
   并给一句人话 + 可继续操作：

   > 仓库状态已经变化（可能是电脑上有别的改动），已为你刷新，请确认后再提交。

   注意**不要**把这句话塞进 `fail()` 的通用错误条 —— 它是可恢复的提示，不是错误。
   这条要新增一个 `notice` 通道（面板内一行提示条），与 `fail` 分开
   （否则一次过期会把整屏盖掉：第 40 篇 §「未同步不能变成功」同族的教训）。

> ℹ️ 服务端那句英文也建议顺手改成人话。它是**共享错误契约**（桌面端同样会显示），
> 改它要连桌面端一起评估 —— 与第 40 篇 §15 留的那条 `runner_offline` 是同一类，
> 本方案不单方面改，只登记。

## 4. 通道方案

### 4.1 四跳链路（与第 40 篇同构，只是名字不再叫 fs）

```text
手机 ──POST /v1/instances/{id}/rpc──▶ 云端（只在内存里组装一帧，不写任何表）
        {op, projectId, conversationId, params, timeoutMs?}
                                        │ Agent WSS：kind rpc.request
                                        ▼
                                    milevia-agent（转发，不映射 op，不留存）
                                        │ loopback HTTP
                                        ▼
                  control-server POST /api/remote/rpc
                    └ 查 remoteOperations()[op] → invokeLocalHandler（保留状态码）
```

### 4.2 响应语义（逐字沿用第 40 篇，**不要改**）

| 层 | 含义 | 取值 |
| --- | --- | --- |
| HTTP 码 | **通道**是否走通 | 200 问到了电脑 / 409 电脑离线 / 504 电脑没回话 / 413 请求帧太大 |
| 响应体 `ok` | **那次操作**是否成功 | `true` / `false` |
| `status` | 该次操作的**业务**结果码 | 200 / 400 / 409（版本冲突、lease 占用）/ 500 |
| `error` | 电脑端原本那句面向用户的中文 | 客户端**原样显示**，不要再翻译一遍 |

三条 409 在手机端必须**分开显示**（这是这条设计存在的全部理由）：

| 触发 | 服务端文案 | 手机端该说什么 |
| --- | --- | --- |
| 版本/状态过期 | `Git state changed; refresh the repository` | 仓库状态已变化，已刷新，请重试 |
| 工作区被 AI 占着 | `project workspace is occupied by another run or Git operation` | 电脑上有 AI 正在运行，Git 操作已锁定 |
| 改动已不存在 | `selected Git paths are no longer available` | 这些改动已经不在变更集里了，已刷新 |

### 4.3 op 名单（28 条，逐条固定）

`query` = params 是查询参数；`body` = params 是整个 JSON 请求体。
`{oid}` / `{suggestionID}` 是**路径参数**，从 `params` 里按白名单替换（§5.2）。

| op | 方法 | 路径 | 入参 | 超时 | MaxBytes |
| --- | --- | --- | --- | --- | --- |
| `git.summary` | GET | `/git/summary` | query | 20s | — |
| `git.changes` | GET | `/git/changes` | query | 20s | 320 KiB |
| `git.diff` | GET | `/git/diff` | query `path`,`stage` | 20s | 320 KiB |
| `git.log` | GET | `/git/log` | query | 20s | 256 KiB |
| `git.commit` | GET | `/git/commits/{oid}` | query（`oid` 走 path） | 20s | 256 KiB |
| `git.commitDiff` | GET | `/git/commits/{oid}/diff` | query（`oid` 走 path） | 20s | 320 KiB |
| `git.branches` | GET | `/git/branches` | query | 20s | 128 KiB |
| `git.operations` | GET | `/git/operations` | query | 20s | 256 KiB |
| `git.conflicts` | GET | `/git/conflicts` | query | 20s | 128 KiB |
| `git.conflictContent` | GET | `/git/conflicts/content` | query `path` | 20s | 320 KiB |
| `git.suggestion` | GET | `/git/conflicts/suggestions/{suggestionID}` | query | 20s | 256 KiB |
| `git.suggestionCancel` | POST | `/git/conflicts/suggestions/{suggestionID}/cancel` | body | 45s | — |
| `git.stage` | POST | `/git/stage` | body | 45s | — |
| `git.unstage` | POST | `/git/unstage` | body | 45s | — |
| `git.stageAll` | POST | `/git/stage-all` | body | 45s | — |
| `git.unstageAll` | POST | `/git/unstage-all` | body | 45s | — |
| `git.commit`（写） | POST | `/git/commits` | body | 45s | — |
| `git.commitAmend` | POST | `/git/commits/amend` | body | 45s | — |
| `git.discard` | POST | `/git/discard` | body | 45s | — |
| `git.switch` | POST | `/git/switch` | body | 45s | — |
| `git.createBranch` | POST | `/git/branches` | body | 45s | — |
| `git.conflictResolve` | POST | `/git/conflicts/resolve` | body | 45s | — |
| `git.conflictAbort` | POST | `/git/conflicts/abort` | body | 45s | — |
| `git.conflictContinue` | POST | `/git/conflicts/continue` | body | 45s | — |
| `git.conflictSuggest` | POST | `/git/conflicts/suggest` | body | 20s | — |
| `git.fetch` | POST | `/git/fetch` | body | 60s | — |
| `git.pull` | POST | `/git/pull` | body | 60s | — |
| `git.push` | POST | `/git/push` | body | 60s | — |

⚠️ op 名与 REST 路径**不要求一一对应**（`git.commit` 读单条提交、写走 `git.commitAmend`，
且写入那条我建议叫 `git.commitCreate` 以免与只读同名 —— 适配器里 REST→op 的映射表
才是唯一真相，与 `mobile-fs-request.ts` 的 `ROUTES` 同构）。

### 4.4 MaxBytes：为什么需要，以及两道闸门怎么分工

大 diff 是**常态**而不是例外：一个 `package-lock.json` 的工作区 diff 轻松 500 KiB+，
一次依赖升级的提交 diff 更大；`conflictContent` 要同时给 base/ours/theirs 三份。

- 上限取 **320 KiB**，与第 40 篇的文件查看上限同值 —— 依据同一个：
  `帧限 384 KiB − 信封与 JSON 转义余量`。
- 判据必须量**发出去的那一帧**，不是 handler 自己的响应体长度。
  第 40 篇为此栽过**两次**（图片 base64 膨胀、文本 JSON 转义），
  这里直接照抄结论：**闸门在 relay 侧、量 JSON 序列化之后**。
- 超限的文案必须是**这一条 op 专属**的：

  > 这个改动差异太大（1.2 MiB），手机上打不开，请在电脑上查看

  而不是笼统的通道错误 —— 用户对「差异太大」能立刻理解，对「通道容量」不能。

**两道闸门必须给不同文案（第 40 篇的硬纪律）**：
relay 侧（320 KiB，`ok:false` + 上面那句）负责**业务上说不清**的情况；
Agent 侧（384 KiB 帧限）是**兜底**，负责 relay 没拦住的东西。
测试上各有一条只有自己能挡的用例（350 KiB 的 diff 只有 relay 会挡；
一条 relay 不设限的 op 回 400 KiB 只有 Agent 会挡），否则「两条都能挡住同一个用例
＝那条测试什么都没证明」。

## 5. 后端改动

### 5.1 control-server

| 文件 | 变更 |
| --- | --- |
| `internal/app/remote_relay.go`（新） | `remoteOperation` 结构、`remoteOperations()` 合成表、relay handler（从 `remote_fs.go` 移过来并改名）、路径参数替换、MaxBytes 闸门 |
| `internal/app/remote_git.go`（新） | `remoteGitOperations()`（28 条，逐条写明方法/路径/入参/MaxBytes） |
| `internal/app/remote_fs.go` | 保留 `remoteFSOperations()`；relay handler 移出（或整个文件并入 `remote_relay.go`） |
| `internal/app/app.go` | 路由 `/api/remote/rpc`；**再次改写 relay 命名空间那段边界说明**（现在它同时提供项目沙箱内文件读写与 Git 操作，且不提供 shell / 任意命令 / 任意 URL 转发） |
| `internal/app/remote_control.go` | `invokeLocalHandler` 不变（它已经是通用形状） |

### 5.2 路径参数替换（`{oid}` / `{suggestionID}`）

这是本方案相对第 40 篇**新增**的一处机制，必须写死：

- 只有 `remoteOperation.Params` 里列出的名字可以从 `params` 替换进 `Path`，
  其余一律**原样留在 query 或 body**（不许偷偷进 URL 路径）。
- 替换值先过**形状校验**：`oid` 必须匹配 `^[0-9a-f]{40}$`（服务端本来还会
  `cat-file -e <oid>^{commit}` 复核，但 relay 不该把任意字符串拼进 URL）；
  `suggestionID` 必须是 UUID 形状。
- **`conversationId` 永远不许从 `params` 进 URL** —— 这条护栏已经在
  `relayFSRequest` 里（`remote_fs.go:119`），改名时**必须原样保留**，
  它决定落在哪个工作区（多会话有 worktree 隔离）。这次要为它补一条针对 git op 的用例。
- `projectId` 的 `/ \ ? # %` 拒绝同样保留。

### 5.3 云端与 Agent

| 文件 | 变更 |
| --- | --- |
| `cloud-control/internal/cloud/rpc.go`（由 `fsrpc.go` 改名） | 帧名、路由 `/rpc`、`timeoutMs` 的 `clamp(1s,120s)`、hub 改名 |
| `agent/internal/agent/rpc_relay.go`（由 `fs_relay.go` 改名） | 帧名、端点 `/api/remote/rpc`；**并发闸门 4 → 8**；413 文案把「这个文件的内容」改成「这次请求的内容」（它也承载 Git 了） |
| `agent/internal/agent/agent.go` | 读循环里那个 `kind == "rpc.request"` 分支（`agent.go:634`）改名。**它必须排在 event 回执分支之前**，否则帧被静默丢弃 —— 注释里已经写了，别在重命名时把顺序搞乱 |

## 6. 前端改动

### 6.1 核心：`mobile-git-request.ts`（`mobile-fs-request.ts` 的同构物）

```ts
// apps/web/src/features/git/mobile-git-request.ts
export function createMobileGitRequest(options: {
  transport: MobileRpcTransport;      // 与 fs 共用（features/remote/mobile-rpc.ts）
  onNotice?: (notice: string) => void; // §3.4 的 409 自动重取提示
}): { request: <T>(path: string, init?: RequestInit) => Promise<T>; invalidateAll: () => void };
```

职责：

1. **REST → op 的映射表是唯一一份**（与 `ROUTES` 同构），方法也写死在表里，
   `init.method` 只用来**校验调用方没写错**，写错当场抛 `wiring` 且不发请求。
2. **每个 op 的超时表**（§3.2），通过信封的 `timeoutMs` 送出去。
3. **剥离 `conversationId`**：`GitWorkbench` 的 `withWorkspace()`（`:64`）会把
   `conversationId` 追加到每一条 URL 上，适配器必须把它**从 query 里删掉**
   （由页面的信封统一携带），与 `mobile-fs-request.ts:453` 的处理逐字一致 ——
   不删的话它会作为 `params` 出现在 `git.commit` 的请求体里，而服务端只认
   `message` / `stateToken`。
4. **写操作前无条件取一次 `git.summary`** 换新鲜 `stateToken`，并处理 409（§3.4）。
5. **不支持端点抛结构化错误**：任何未列入 op 名单的路径一律 `wiring` 报错且不发请求。

### 6.2 `GitWorkbench` 的 `mobile` 变体（**不要另写一套界面**）

给 `GitWorkbench` 加 `mobile?: boolean`（与 `FilesPanel` 的 `mobile` 同构）。

> **工作量比想象的小 —— 这是去读源码核实的，不是估计。** 看 `FilesPanel` 的 `mobile`：
> 它在整个文件里只被消费 4 处，且**几乎全是往下传**（`FileViewer` 的 `wrap`、
> `FileEditor` 的软换行与键盘避让）。而「树 ⇄ 查看器」那个两级推进
> （`mobileView: "tree" | "editor"`、`files-mobile-back`、`hidden-mobile`）
> **根本没有拿 `mobile` 当条件** —— 它是无条件实现的，靠 CSS 在窄屏生效。
> 也就是说第 40 篇那次「移动变体」，实际体量是 **1 个 prop + 1 个二层状态 + 一批 CSS**。
> Git 照这个体量做即可，本节这五处减法基本就是全部。

在这一支里做五处减法：

| # | 桌面端 | 手机端 |
| --- | --- | --- |
| 1 | `GitBar` 右侧一排「拉取/获取/推送/刷新/操作记录」 | 收成「当前引用 + 同步胶囊 + ⋯」；四个动作进抽屉 |
| 2 | 变更列表与 diff **并排**（`git-workbench-body.changes-active`） | 两级推进：`列表 ⇄ diff`（与 `FilesPanel` 的 `树 ⇄ 查看器` 同构），返回键先回列表 |
| 3 | 操作记录用**原生 `<select>` 筛选** + 弹层（`GitWorkbench.tsx:300`） | **必须改**：原生 `<select>` 在手机端是本项目明令不用的（既有的 `taskFilters` 就是胶囊行）。改成横向胶囊 + 全高抽屉 |
| 4 | 提交信息面板固定在底部 | 键盘避让（沿用 `FileEditor` 的 `visualViewport` 做法），保存按钮在键盘上方 |
| 5 | `DiffViewer` 横向滚动 | **软换行**。第 40 篇已经为此栽过一次（只读查看器没开换行，是**看截图**才发现的），diff 同理且更严重 —— diff 行的前缀列会让行更长 |

**diff 渲染本身一行不改**：`parseDiffContent`（`:375`）→ `DiffViewer`（`:400`）
是纯展示组件，吃 `GitDiff`，这正是用户要的「看到 git 的文件差异」。

**必须保留原样 / 必须补上的三样东西：**

- **`GitConfirmation`**（`:419`）—— 它承载 `discard` / `push` / `switch` /
  `amend` 这些**不可逆**动作的确认层。手机上误触代价更高，**确认层只能更强，不能简化**。
  特别地：`discard-all` 的"包含未跟踪文件"勾选框（`:440`）必须保留。
- **增删颜色**：`.git-diff` 的加绿减红要与文件编辑器保存前的 diff 一致（项目既有约定）。
- **⚠️ `fail` 必须有落点 —— 这是第一版方案漏掉的一处。**
  `GitWorkbench` 把**所有**错误都往 `fail: (message) => void` 里塞（桌面端接的是页面的
  `setError`）。手机端**不能顺手写 `fail={setError}`**：`.mobile-error`
  （`MobileRemotePage.tsx:4836`）是一条**页面级**的 `role=alert`，而它在
  `MobileRemotePage` 里已经有**三处**记录了同一个问题 ——「它在弹层遮罩之下，用户看不到」
  （`:986-988` 的任务创建失败、`:1007` 的复制反馈、`:3700` 的同一个坑）。
  Git 视图是子态不是弹层，但页面级那条的位置、层级、生命周期都不由 Git 视图掌控：
  一次 `discard` 失败的提示可能出现在视野之外，或在切走后残留。
  ⇒ **Git 视图要有自己的就地错误落点**（面板内一行 `role="alert"`），
  与 §3.4 那条「可恢复提示」共用同一个区域但分级不同（错误=红、提示=中性）。
  照项目既有做法（`.mobile-task-modal-error`），**错误要长在它所属的那块界面上**。

### 6.3 视图与入口

**建议把「会话子态」从两个布尔改成一个槽位。** 现状是
`filesOpen`（`MobileRemotePage.tsx:933`）+ 两套互斥的 ⋯ 菜单；再加一个 Git
就是第三个布尔 + 第三套菜单，而返回键链（`backHandlerRef` 的 if 链 + `leaveConversationView`）
是**两处都要改**的结构（第 40 篇 §7.2 的教训：漏一处就会出现"点错也能执行"）。

```ts
// 一个槽位，而不是三个布尔
const [sessionSub, setSessionSub] = useState<null | { kind: "files"; path?: string } | { kind: "git" }>(null);
```

| 入口 | 位置 | 说明 |
| --- | --- | --- |
| **会话 ⋯ 菜单** | 「项目文件」旁边加一项「Git 工作台」 | 最小改动，符合「顶栏一行标题 + ⋯ 菜单」的既有约规 |
| **会话消息里的文件路径** | 已有的路径胶囊（`lib/project-path.ts`） | 点开文件后，从文件查看器里再进「这个文件的改动」—— **这是手机端最常用的 Git 入口**：AI 改完文件，用户想看"它到底改了什么" |
| **项目卡** | ❌ 不做 | 同上：手机端项目卡的长按已被拖动排序占用（`lib/use-card-drag.ts`），加菜单要另加显式按钮；而「N 个改动」还得额外一次请求（快照里没有） |

**明确不做「从项目列表直接进 Git」**：理由与第 40 篇拒绝「长按项目卡弹菜单」相同 ——
手势冲突 + 需要一个手机快照里不存在的读数。

### 6.4 头部与工作区标识

继承第 40 篇 §4.4 的硬要求：**Git 视图必须绑定工作区**。

- 从会话进入 → 带 `conversationId`；显示**该工作区**的实时 `head.branch`（来自 `git.summary`）。
- 头部同时显示：分支名 / 游离指针 / 未跟踪上游 / `↑ahead ↓behind`（`GitBar` 已有这套
  文案，`GitWorkbench.tsx:259-261`，直接复用）。
- 已知缺口（第 40 篇 §14 遗留 1）：手机快照里**没有每个会话的工作区信息**，
  所以从会话进来时我们只知道项目分支，不知道它是不是某个 worktree。
  本期沿用同样的处理：**如实显示项目分支**，并保留那句
  `title`（"手机端快照里还没有每个会话的工作区信息"）。
  ⚠️ 但 Git 视图比文件视图更需要它：文件视图编辑错工作区只是改错文件，
  Git 视图切换错工作区是**切错分支**。所以本方案把「把它加进快照」从遗留升级为
  **本期建议做的小项**（`remoteSnapshotConversation` 加 `workspacePath`/`branch` 两个字段，
  手机端头部渲染），成本很小，收益是消除一类不可逆误操作。

### 6.5 空态与错误态（本项目红线：不许把"读不到"写成"没有"）

| 真相 | 渲染 |
| --- | --- |
| 这个项目不是 Git 仓库 | 「这不是一个 Git 仓库。」+ 一句"在电脑上 `git init` 后重进本页" —— **不是**一个空的变更列表 |
| 还没回来 | 「正在读取仓库状态…」（`GitWorkbench` 已有，`:211`） |
| 读失败 | 「读不到仓库状态」+ **服务端那句原文** + 一颗「重试」。**不许**退化成「无改动」 |
| 真的没有改动 | 「工作区干净，没有未提交的改动」（`.git-empty`，与上面三档文案不同） |
| 电脑端 runner 离线 | 服务端回 `runner_offline` ⇒ 手机端现在会显示英文枚举。**本期不改共享契约**，但要在适配器里把它翻成一句人话（只影响手机端），并登记 |

⚠️ 第 40 篇复查抓到过同一个族的缺陷两次（`editable` 算了没人用、`unreadable` 声明了没渲染）。
Git 侧同类风险点是：**`detached`、`conflicted`、`repositoryState`** 三个字段
前端类型里都有、`GitBar` 只判了 `detached`。交付前要 `grep` 每个服务端字段，
**除定义处外至少要有第二个命中点，且在渲染路径上**。

## 7. 分阶段（建议按这个顺序做，一次发版交付）

项目既有纪律是「作为一个完整版本交付，不拆成多期上线」（第 40 篇 §9），
但**实现顺序**仍应分三段，因为第一段就已经交付了用户最需要的能力。

| 阶段 | 内容 | 交付价值 | 风险 |
| --- | --- | --- | --- |
| **A. 只读仓库洞察** | 通道泛化 + `git.summary/changes/diff/log/commit/commitDiff/branches/operations/conflicts/conflictContent` + 手机端只读视图（变更列表 ⇄ diff、提交历史、提交详情、分支列表） | **「能看到 git 的文件差异」= 用户这次的核心诉求，本阶段就齐了** | 低。完全不改仓库状态 |
| **B. 受控写入** | `stage/unstage/stage-*`、`commit/amend`、`discard`、`fetch/pull/push`、`switch/createBranch` + `GitConfirmation` 移动化 | 手机上完成"提交这次改动"的闭环 | 中。**不可逆动作**，确认层必须逐字保留 |
| **C. 冲突解决** | `conflicts/resolve|abort|continue|suggest|suggestions/{id}` + `ConflictSolveView` 移动化 | 手机上处理 merge/rebase 冲突 | 中。逐块选 ours/theirs 在手机上操作密度高，建议先只给「整文件取一方」+「用 working 版本」，逐块选择留后 |

## 8. 验收与测试

### 8.1 Go 侧（契约只能留在 Go 测试里）

- **op 名单显式化**：`remoteGitOperations()` 的**条数、每条的 method / path / 入参形态 /
  MaxBytes 逐条固定**。改名单必须同时改测试（第 40 篇的 `TestFSRemoteOperationWhitelistIsExplicit` 同构）。
- **合成表的性质**：fs 与 git 的 op 名**无交集**（有交集必须红 —— 否则后注册的静默覆盖先注册的）；
  两条路都能被同一个 relay handler 走到。
- **路径参数替换**：`oid` 非 40 位十六进制 → 400 且**没有发出本机请求**；
  `params` 里塞一个不在白名单里的名字 → 它进了 query/body 而**没进 URL 路径**；
  `conversationId` 从 `params` 传 → **仍然被忽略**（对 git op 也补一条）。
- **MaxBytes**：一条 350 KiB 的 diff → `ok:false` + 那句专属文案（不是通用通道错误）；
  同样大小的只读内容在**未设上限**的 op 上 → 由 Agent 侧帧判据挡下，两边文案不同。
- **超时字段**：`timeoutMs` 缺省 / 超上限 / 为负数 / 非数字 → 都落到合法值，且
  **云端不解析 op**（断言云端代码里没有任何 git op 字面量）。
- **鉴权不变量**（已有一条，需扩展）：`/api/remote/rpc` 只认 Agent 令牌；
  桌面页会话令牌**不得**使用（否则等于给一个被 XSS 拿到的页面加一条写 Git 的出口）。
- **`detached` 分支下写操作被拒**（`GitBar` 已经按它禁用，服务端也要有对应用例）。

### 8.2 前端单测

- 适配器 REST → op 映射**全覆盖**；未列入的端点抛 `wiring` 且**不发请求**；
  方法写错当场抛。（变异：改掉一条映射 → 必须有测试红）
- **超时分档表**：每类 op 送出的 `timeoutMs` 正确（变异：写操作退回 20s → 红）。
- **写前无条件刷新**：任何一条写 op 之前都先发了一次 `git.summary`，且带的是**新的**
  `stateToken`（数桩上的请求序列）。
- **冲突概览的「读不到」档**（§3.3 那条约出来的）：stub 让 `conflicts` 返回 503，
  断言界面渲染出**显式的"读不到冲突状态"**，**不是**"没有冲突"。
  这条同时也是「`.catch(() => null)` 已去掉」的结构断言 —— 两者都要，缺一个就还能回退。
- 409 三档分支：三种服务端文案分别落到三种界面状态，且**过期那一档不清空现有内容**。
- **`fail` 有落点**：写失败时错误出现在 Git 视图**内部**，而不是页面级 `.mobile-error`。

### 8.3 端到端探针（必须真浏览器，沿用 `.tmp/probe-mobile-files.mjs` 的脚手架）

新脚本 `.tmp/probe-mobile-git.mjs`（stub 云端的 `/v1/instances/{id}/rpc`，
按 op 分派且**按 params 真实变化**响应 —— 第 40 篇的教训：桩不按参数响应就等于没验）：

- 项目卡 → 会话 → ⋯ 菜单 → Git 视图 → 变更列表 → 点文件 → 看到 diff（**断言 diff 行内容与加/减行数**）；
- 提交历史 → 提交详情 → 单文件 diff；
- **软换行真的生效**（量 `scrollWidth <= clientWidth`，查看态与编辑态**都要量**）；
- 安卓返回键退栈：`diff → 列表 → 视图 → 会话`，四档逐级断言；
- 写操作（stage → commit）：确认框内容逐项断言，`stateToken` 过期时自动重取后成功；
- **冲突概览的"读不到"档**：让 `conflicts` 回 503，
  断言界面**不是**「没有冲突」（这条挡的是本项目最贵的一类缺陷）。
- 大 diff（>320 KiB）：渲染的是**"差异太大"卡**而不是空白，也不是一句通道错误；
- 写失败：错误落在 Git 视图内部，不是页面级 `.mobile-error`；
- 非 Git 项目：空态文案是「这不是一个 Git 仓库」，**不是**「没有改动」。

出图放 `outputs/mobile-git/`。

### 8.4 变异检验（判据是「失败条数变多」，`SKIPPED`/`ESCAPED` 必须都是 0）

至少覆盖：把 `conflicts` 的 `.catch(() => null)` 还原 → 红（**这条最重要**）；
超时分档表拍平 → 红；`conversationId` 护栏去掉 → 红；
MaxBytes 判据改成量 handler 响应体（而不是序列化后的帧）→ 红；
写前刷新去掉（直接用界面里那份 token）→ 红；409 自动重取去掉 → 红。
全部按 sha1 逐字节还原，还原校验写在 `finally` 里。

## 9. 安全

### 9.1 授权范围变了，必须写清楚

`/api/remote/*` 的定位已经变过一次（第 40 篇加了文件读写）。这次再变一次：

> `/api/remote/rpc` 提供**项目沙箱内**的文件读写**与**该仓库的 Git 操作
> （27 个领域动作，全部经既有的 handler；路径一律经 `resolvePath` /
> `resolveMutationPath` / `remotePathWithinRoot` 与 `validateGitPath` 校验）；
> 不提供 shell、任意命令执行、任意 `git` 子命令、任意 URL 转发，
> 也不接受任何 revision 表达式（`ref` / `branch` / `startPoint` 只能来自服务端列出的引用或严格校验的 SHA）。

**鉴权边界不变**：只认 Agent 令牌。这条要进 `TestRemoteRelayAcceptsDesktopSessionForPairingOnly` 的用例表。

### 9.2 风险与处理

| 风险 | 处理 |
| --- | --- |
| **手机丢失后被用来 push / 切分支 / 丢改动** | op 名单是闭集（28 条）；`discard`/`push`/`amend`/`switch` 保留桌面端同款确认层；**不建议**为手机端放宽任何一条 |
| 误推送到受保护分支 | ⚠️ **服务端目前没有这条策略**：第 08 篇设计的 `git_repository_settings` 表（`protected_branch_patterns`）**从未实现**。桌面端也没有。这是本方案暴露出的**既有缺口**，不是手机端引入的 —— 但它让"手机能 push"这件事的风险从"需要确认"变成"没有任何闸门"。**建议本期不做这件事**（另立一项），但要在文档里明说，不能默认它存在 |
| 跨工作区误操作 | 见 §6.4。工作区由 `conversationId` 唯一决定，且必须来自信封 |
| 与 AI 抢写 | 沿用工作区租约；AI 在跑时返回 409，手机端翻成「电脑上有 AI 正在运行，Git 操作已锁定」 |
| 超大响应掐断中继连接 | 双端帧上限（384 KiB）+ relay 侧 op 级上限（320 KiB）。这是**最容易被忽略的一条**：响应超限不会只让这次请求失败，而是让这台电脑与手机之间的命令、事件、快照一起断掉 |
| 状态令牌被重放 | `stateToken` 一次性、2 分钟、绑项目 + 工作区 + 路径；服务端每次写前重读真实状态比对 |
| 客户端伪造 Git 操作 | 名单只有一份（控制服务的合成表）。云端不认识 op、Agent 不映射 op —— 不存在"两份名单不同步"或"半路 400" |

## 10. 改动文件清单（预计）

### 后端

| 文件 | 变更 |
| --- | --- |
| `apps/control-server/internal/app/remote_relay.go` | **新增**：op 表类型 + 合成 + relay handler + 路径参数替换 + MaxBytes 闸门（由 `remote_fs.go` 演化） |
| `apps/control-server/internal/app/remote_git.go` | **新增**：28 条 git op 名单 |
| `apps/control-server/internal/app/remote_fs.go` | 保留 fs 名单，relay handler 迁出 |
| `apps/control-server/internal/app/app.go` | 路由改名 + **第三次改写 relay 边界说明** |
| `apps/control-server/internal/app/remote_relay_test.go` | **新增**：合成表、路径参数、MaxBytes、超时字段、conversationId 护栏 |
| `apps/control-server/internal/app/remote_pairing_auth_test.go` | 扩展：桌面会话不得用 `/api/remote/rpc` |
| `apps/cloud-control/internal/cloud/rpc.go` | 由 `fsrpc.go` 改名 + `timeoutMs` 夹取 |
| `apps/cloud-control/internal/cloud/server.go` | 路由 `/rpc`、帧分派改名、`failInstance` 改名 |
| `apps/agent/internal/agent/rpc_relay.go` | 由 `fs_relay.go` 改名、端点改名、并发 4 → 8、413 文案泛化 |
| `apps/agent/internal/agent/agent.go` | 读循环分支改名（**顺序不能动**） |

### 前端

| 文件 | 变更 |
| --- | --- |
| `apps/web/src/features/remote/mobile-rpc.ts` | **新增**：`MobileRpcReply` / `MobileRpcTransport`（fs 与 git 共用，从 `mobile-fs-request.ts` 抽出） |
| `apps/web/src/features/git/mobile-git-request.ts` | **新增**：REST → op 映射 + 超时分档 + 写前无条件刷新 + 409 分档 |
| `apps/web/src/features/git/mobile-rpc-errors.ts` | **新增**：把三种 409 与 `runner_offline` 翻成人话的唯一判据来源 |
| `apps/web/src/features/git/GitWorkbench.tsx` | `mobile` 变体（GitBar 收抽屉、列表⇄diff 两级、`<select>` 换胶囊、键盘避让、diff 软换行）；导出 `GitWorkbenchHandle`（`showTopLevel()`，供返回键问"还有没有上一层"）；**去掉 `conflicts` 的静默 `.catch(() => null)`，补「读不到冲突状态」这一档** |
| `apps/web/src/features/git/GitBar.tsx`（如拆分） | 原生 `<select>` → 胶囊；动作进抽屉 |
| `apps/web/src/features/git/ConflictSolveView.tsx` | `mobile` 变体（阶段 C） |
| `apps/web/src/pages/MobileRemotePage.tsx` | 会话子态槽位化（`sessionSub`）+ ⋯ 菜单两项 + 返回键两处 + 中继适配器 + 头部工作区标识 |
| `apps/web/src/git.css` | `.git-*` 的移动变体（**两段选择器或 `data-mobile` 属性**，避免与桌面规则互相串味；注意 `git.css` 已在 680px 断点内，手机 390px 会命中隐藏的那套规则，要逐条确认不是"碰巧能用"） |
| `apps/web/src/pages/mobile-remote.css` | Git 视图容器、抽屉、提示条 |

## 11. 待决策点（请拍板）

| # | 问题 | 我的建议 | 备选 |
| --- | --- | --- | --- |
| 1 | 通道改名（`fs` → `rpc`） | **先提交第 40 篇那批改动，再把改名做成一次独立的纯机械提交**（§3.1）。它能让名字与事实一致，且现在动没有兼容负担 | **只改内部 Go 标识符**，线上名字保持 `/api/remote/fs`。零风险、零 churn，代价只是名字长期不精确；`app.go` 那段边界说明如实写清就足以覆盖真正的风险 |
| 2 | 写操作范围 | **全给**（含 push/discard/switch），确认层逐字沿用桌面端 | 只给 `stage`/`commit`，`push`/`discard`/`switch` 留桌面端（更保守，但"手机上提完就推"是真实场景）。⚠️ **注意"只给一部分"在架构上不是靠手机端做**：任何能力开关的正确位置是**电脑端**的实例级配置（第 40 篇已论证过"让待验证的一方自己决定自己的权限没有意义"），而那个配置表（`git_repository_settings`）现在不存在 |
| 3 | 受保护分支策略 | **本期不做，但立一项**（服务端本来就没有 `git_repository_settings`） | 本期顺手补 `protected_branch_patterns`（会扩大范围到桌面端共用策略） |
| 4 | 本期是否顺带补「每个会话的工作区」进快照 | **做**（§6.4）。成本小，消除"切错分支"这类不可逆误操作 | 沿用第 40 篇的遗留处理（只显项目分支） |
| 5 | 入口结构：**独立 Git 子态** 还是 **一个「工作区」子态内含 文件/改动 两个 tab** | 独立子态（本方案主线）。理由：Git 工作台自身就有 变更/分支/历史/操作记录 四个屏，塞进文件的 tab 里会变成一个套娃 | 「工作区」双 tab。可取之处是**少一套菜单、返回键栈只多一层，且"从 diff 跳到该文件"变成同层切 tab 而不是子态套子态** —— 这恰好是手机端最常用的那条链路（AI 改完文件 → 看它改了什么 → 打开文件） |

## 12. 已知遗留 / 明确不做

1. **不做手机端任意附件下载**（继承第 40 篇）：Git 侧同理不提供 `git archive` 之类能力。
2. **不做 `reset --hard` / `rebase` / `cherry-pick` / tag / 删除分支**（第 08 篇的边界，服务端本来就没有）。
3. **不做 `stage` 的逐 hunk 暂存**（第 08 篇 §7.1 的边界）。
4. **冲突解决的逐块选择**在手机上建议后置（阶段 C 先给整文件级操作）。
5. **共享错误契约的两处英文**（`Git state changed; refresh the repository`、
   `runner_offline`）不在本期单方面改 —— 要连桌面端一起评估。
6. **不做 Git 状态的轮询**：沿用「进视图加载 + 手动刷新 + 写成功后重载」。
   二期若要做实时，复用既有事件通道发轻量事件（只带 `head.oid` 与 revision），
   与第 40 篇 §7.6 的 `fs.changed` 是同一件事，**应该一起做一次**。

## 13. 对本方案自身的复查（2026-09-21，用户问「这是不是最佳方案」）

把第一版方案逐条拿去对源码/实测重新验了一遍，**改掉 2 处过度设计、1 处过度自信、
补上 2 处漏掉的**。记在这里，免得实施时又按第一版的印象走。

| # | 第一版怎么写的 | 复查结论 | 依据 |
| --- | --- | --- | --- |
| 1 | 决策 3：**适配器串行队列** + 闸门抬到 8 | **串行撤销**，只抬闸门。串行的代价确定要付（首屏从约 1 次往返变 5 次），收益是个假设；而且它**修不掉真正的问题** | 5 条请求是**互相独立**的，不像 `/fs/stat`+`/fs/read` 那样有先后依赖 —— 合并/串行都省不掉延迟 |
| 2 | 决策 4：距 `observedAt` **> 90s 才**重取 token | 改成**无条件写前刷新**。90s 是拍的数字，省下的只是写前的一次读 | 写操作本就低频、用户主动；用一次往返换掉一个魔法数加一类漏判 |
| 3 | 决策 1：改名「必须现在决定」 | 降级为**最弱的一条建议**，并把两个方案摆平（§3.1）。真正的风险是"审查据此放松"，而那由 `app.go` 的边界说明覆盖 | 项目既有的对策一直是**说明**而不是名字；且这批改动未提交，大规模重命名会让 review 变难 |
| 4 | **漏了**：没写 `fail` 该接到哪里 | 补 §6.2 第三条。**不能顺手 `fail={setError}`**：页面级 `.mobile-error` 在该文件里已有**三处**"被遮罩盖住、用户看不到"的记录 | `MobileRemotePage.tsx:986-988`、`:1007`、`:3700` |
| 5 | **漏了**：把"5 个请求里有一个吃 503"当成性能问题 | 它其实是个**正确性**问题，而且比性能严重得多：被拒的那条恰好是唯一带 `.catch(() => null)` 的 `conflicts` ⇒ **有冲突时手机端会显示成"没有冲突"**，且症状是竞态抖动 | `GitWorkbench.tsx:98`；这条正撞在本项目"把读不到写成没有"的红线上 |

**复查后认为是稳的、不要再动的部分**（免得实施时又去怀疑）：

- 通道形态（复用第 40 篇的 RPC 四跳 + 唯一一份合成 op 表 + 三条不变式）；
- **整块复用 `GitWorkbench`**（而且工作量比我原先估的更小：`FilesPanel` 的 `mobile`
  实证了「移动变体 ≈ 1 个 prop + 1 个二层状态 + 一批 CSS」）；
- 按 op 分档超时 + 「超时≠失败」（依据是"先落库再执行"那个顺序）；
- op 级 MaxBytes + 与 Agent 侧**文案必须不同**；
- 工作区绑定（`conversationId` 只来自信封）；
- 实现顺序 A（只读）→ B（受控写入）→ C（冲突），一次发版。

### 还有一条更根本的备选（值得知道，不推荐）

`conversation.shortcut` 里已经有一条「检查状态」提示词
（"请检查当前项目的 Git 状态、未完成工作和明显风险"，`app.go:4262`）—— 也就是说
**"问 AI 项目改了什么"这条路今天就能走**。但它不是本需求的替代品：它不产生 diff、
不能暂存/提交、每次要烧一次 AI 回合。列在这里只是为了说明——
本方案和既有能力不冲突，也不该被它取代。

## 14. 实施进度与实现期的两处发现（2026-09-21）

### 已落地（四端）

| 端 | 内容 |
| --- | --- |
| control-server | 通道改名 `fs` → `rpc`（路由 `/api/remote/rpc`、帧 `rpc.request`/`rpc.response`）；`remote_relay.go`（op 类型 + 合成表 + relay handler + 路径参数 + MaxBytes）、`remote_fs.go`（fs 名单）、`remote_git.go`（**28 条** git op）；`app.go` 的命名空间边界说明改写到第三版 |
| cloud-control | `rpc.go`（由 `fsrpc.go` 改名）+ `timeoutMs` 的 `clamp(1s, 120s)`；504 文案里的秒数改取**实际用的**超时 |
| agent | `rpc_relay.go`（由 `fs_relay.go` 改名）；并发闸门 **4 → 8**（Git 视图首屏就是 5 个并发请求） |
| web | `features/remote/mobile-rpc.ts`（共用信封类型）、`features/git/mobile-git-request.ts`（适配器）、`MobileGitPanel.tsx`（错误落点 + 句柄）、`GitWorkbench` 的 `mobile` 变体、`MobileRemotePage` 的会话子态槽位化；`git.css` 与 `mobile-remote.css` 的移动规则 |

### 与原方案的三处有意偏差

1. **`git.changes` 的上限取 320 KiB，不是 128 KiB。** 它是手机端**最主要的那一屏**，
   而一个有几千个改动文件的仓库按 128 KiB 算会整屏打不开 —— 那时用户连"改了什么"都看不到。
   320 KiB 与 `git.diff` 同档，仍是帧预算之内。
2. **不做 `GitBar` 动作抽屉。** 方案说"四个动作进抽屉"，实测 390px 下
   `拉取 获取 推送 ⟳ 🕘` 加间距约 264px，一行放得下（680px 断点已经会换行兜底）。
   按项目"布局是量出来的"那条纪律，不加一层多余的开合。
3. **`GitWorkbench` 的手机端详情层是 `forwardRef` + `showTopLevel()`，而不是另包一层状态。**
   它在实现时收敛成了**派生值**（`selectedDiff`/`conflictPath` 有值就是有详情层）——
   独立记一个布尔必然有一天与它们不同步，症状是"返回键吃掉一次、界面上什么都不发生"。

### 实现期抓到的两个真问题（都不在原方案的预判里）

| # | 问题 | 为什么测试没红 |
| --- | --- | --- |
| 1 | **请求体里的 `conversationId` 没有被剔除** | 查询分支从第 40 篇起就剔了它，而请求体分支会把它**原样转发**。当时没有漏洞（handler 都 decode 到自己的结构体、忽略多余字段，工作区一律从 query 读），但那意味着"工作区只认信封"这条**靠的是碰巧没人读它** —— 后来一次无心的改动就能悄悄破坏它。是写"信封唯一来源"那条用例时**先红**才发现的；改成结构上剔掉（`stripRelayParam`）。 |
| 2 | **diff 的 `meta`/`hunk` 行被按字符折断（`a/sr` / `c/lo`）** | 移动端的 `grid-template-columns` 覆盖特异性（0,3,0）高于基准里的 `.git-diff-line.meta`（0,2,0），把它们的单列模板也盖掉了；而这两行的行号是 `display:none`，剩下的 code 被自动放进**第 1 列（34px）**。**软换行那条断言照样通过** —— 挤窄了当然不溢出。**是看截图发现的**，补了一条量"它到底占多宽"的断言（>200px）。 |

第 2 条与第 40 篇"只读查看器没开软换行"是同一类：**量"没溢出"证明不了"看得清"**。

### 环境限制（本沙箱特有，不影响交付）

**control-server 里凡是构造完整 `Server` 的测试在本环境跑不了**：`app.New` 那条路径会拉起
`wsl.exe`，而它被沙箱黑名单拦下（提示明确要求不得绕过）。因此：

- 能跑的：`go build` / `go vet` / **测试二进制编译** / 所有**不构造 Server** 的用例；
- 跑不了的：`remote_relay_test.go` 里那 8 条走 `newTestServer` 的用例、以及既有的绝大多数
  集成用例（它们在本轮改动之前就是同样的情况，不是回归）。

对策是把新逻辑**抽成纯函数**（`relayTarget` / `relayPathValues` / `validateRelayPathParam` /
`relayOversizeError`），让最关键的断言不依赖 Server —— 这本身也是更好的设计。
新增的 `remote_git_test.go` 因此全部可跑，覆盖：28 条名单逐条固定、合成表无交集、
上限都在帧预算内、占位符与声明严格互为对方、路径参数形状（含 **40/64 位**两种对象 ID）、
信封 conversationId 唯一来源（查询与请求体两条路）、超限文案与通道判据文案不同。

### 本轮验证

| 项 | 结果 |
| --- | --- |
| control-server | `go build` ✅ / `go vet` ✅ / `gofmt -l` 干净 / 纯逻辑用例 **全部 PASS** |
| cloud-control | `go build` ✅ / **全量 `go test ./...` ✅**（含新增的超时夹取用例） |
| agent | `go build` ✅ / **全量 `go test ./...` ✅** |
| 前端 | `tsc --noEmit` ✅ / **634/634 ✅** / `vite build` ✅ |
| 端到端探针（Git） | **27/27 ✅**，出图 `outputs/mobile-git/` 7 张 |
| 端到端探针（文件，回归） | **40/40 ✅**（子态槽位化没有弄坏它） |
| 变异检验 | 把「冲突读不到」改回静默降级 → 探针 **exit=1 被挡住**；按 bytes 还原、sha1 逐字节一致（`4d987a6c…`） |

Git 探针实测覆盖：入口菜单 → 变更列表两组 → 点文件看 diff（含**软换行**与
**meta 行占满整行**两条量宽断言）→ 返回键两级退栈 → 暂存前**先换 stateToken 且发出去的是新的**
→ 大差异降级为「差异太大」且错误落在 Git 视图内部 → 冲突读不到显式说明
→ 操作记录不用原生 `<select>`。

---

## 15. 复查修正（2026-09-21，实施完成后逐文件自查）

实施完成后把整个改动逐文件复查了一遍，抓到**一类只在真实链路上才暴露的问题**与若干残留。
以下每条都属于"测试不会红、界面看着也对"的那一类，所以逐条记下来。

### 15.1 失败分类的判据量错了东西（真问题）

手机端适配器原来按**英文原文**给失败分类（`classifyGitFailure` 的 needle 表）。
但电脑端的 `writeError` 会把文案**本地化**（`app.go` 的 `localizedErrorText`）：

| 服务端原文 | 到达手机端的形态 | 按原文匹配 |
| --- | --- | --- |
| `project workspace is occupied by another run or Git operation` | `项目工作区正被其他 AI 任务或 Git 操作占用…`（有条目 ⇒ **整句替换**） | ❌ **永不命中** |
| `Git state changed; refresh the repository` | `当前操作与进行中的操作冲突，请稍后重试。：Git state changed; …`（无条目 ⇒ 原文作后缀保留） | ✅ |
| `selected Git paths are no longer available` / `there are no eligible Git changes` / `runner_offline` | 同上，原文作后缀保留 | ✅ |

也就是**只有"工作区被占用"这一条是坏的**，而它的单测喂的是本地化**之前**的英文 ——
那条用例单独存在时是假绿，症状是 `workspace_busy` 这个种类在真实链路上不可达。

修法的选择值得记下来：这一条恰好是**服务端本来就给了稳定码**的那一条（`httpErrorCode`
产出 `workspace_occupied`，它的注释原文正是"stable client behavior without coupling it
to a localized error message"）。所以不再匹配句子，而是把码一路带出来：

```
relayErrorResponse → rpcResponse.code → agent → 云端 → MobileRpcReply.code
  → classifyGitFailure(message, code)：码优先、文案兜底
```

没有码的那几条仍匹配文案，注释里写明它们**为什么能活下来**（`fallback：原文` 的形态）
以及"服务端哪天给某条加了翻译，匹配就会静默失效"。

另外记一条纪律：**抽了取值函数不等于接线接上了**。漏掉 `relayErrorResponse` 里
`Code:` 那一行不会有任何编译错误，所以断言必须打在"真正发出去的那份答复"上。

### 15.2 `stateToken` 过期时会同时出现两条提示（真问题）

适配器为了让调用方仍能判失败，必须把 `stale_state` 抛出去，而宿主把抛出的错误直接塞进
红色错误条 —— 于是屏幕上同时出现绿色的"已为你刷新，请确认后再提交"和一条红色的
"`…：Git state changed; refresh the repository`"。现在：把提示抽成常量
`STALE_STATE_NOTICE`，适配器用它作为抛出的 message，宿主认出它就**不进**错误条；
并新增 `onRecovered`，在**写操作成功**时撤掉那条提示（只在写成功时撤 ——
失败之后宿主会立刻重载仓库，只读成功也撤的话它只会闪 300 毫秒）。

### 15.3 其余残留

- **通道改名后的旧名**：云端 `rpc.go` 的注释与一处 `log.Printf`、Agent 测试的失败消息、
  Agent 注释里的 `relayFSRequest`（该函数已不存在）、云端 `server.go` 一处"文件请求"。
  按项目纪律，失实的注释比没有注释更危险。
- **Git 适配器的 `invalidateAll` 是死代码**：它唯一的作用是清 `lastKnownStateToken`，
  而那个变量**只写不读**，页面也从没调过它。这里既然没有内容缓存，就不该有"清缓存"的出口
  —— 留着它等于暗示这里存了东西。已删除，文件头"唯一的缓存是那个令牌"也一并改掉。
- Agent 并发闸门的注释仍引用已删除的 `.catch(() => null)`。

### 15.4 复查后的验证

| 项 | 结果 |
| --- | --- |
| control-server | `go build` ✅ / `go vet` ✅ / `gofmt -l` 干净 / 纯逻辑用例 **16/16 PASS** |
| cloud-control | **全量 `go test ./...` ✅**（新增"`code` 穿过 hub"用例） |
| agent | **全量 `go test ./...` ✅**（失败回话用例补上 `code` 断言） |
| 前端 | `tsc --noEmit` ✅ / **637/637 ✅**（+3 条）/ `vite build` ✅ |
| 端到端探针（Git / 文件） | **27/27 ✅** / **40/40 ✅** |
| 变异检验（新增的两条防线） | ① 漏掉中继的 `Code:` 接线 → 纯函数用例**红**（`code = ""`）；② 让"码优先"永不生效 → 适配器用例**红**（`a stable error code wins over the message text`）。两次都按 bytes 还原、sha1 与基线一致 |

---

## 16. 手机端排版尺度优化（2026-09-21）

写完功能后按"实测 → 改 → 复测 → 看截图"做了一轮排版优化。做法是先加一个**只测量不判定**
的脚本（`.tmp/measure-mobile-git.mjs`，390×844）：把每个可交互元素与关键文字的字号、行高、
盒高、内边距全打出来 —— 探针验得了行为，验不了"字够不够大"。

改前实测出来的三处硬伤：diff 代码 **11px**、操作记录弹层的「审计记录」标签 **9px**、
分组图标按钮 **28×28**。其余是"桌面上没问题、手机上偏小"的一档（路径 12px、状态标签 10px）。

改后（全部用 `[data-mobile="true"]` 两段选择器限定，桌面端一个字不变）：

| 元素 | 改前 → 改后 |
| --- | --- |
| diff 代码 | 11 → **13px**（与文件查看器同一档） |
| 变更行路径 / 状态标签 | 12 → 13 / 10 → 11 |
| 分组图标按钮 | 28×28 → **34×34** |
| GitBar 刷新 / 操作记录 | 32×32 → 36×36 |
| 行内操作按钮 | 36 → 40 宽 |
| 页签 / 胶囊 | 34 → 38 高 / 28 → 34 高 |
| 确认弹层底部按钮 | 42 高、14px |

顺手修掉三处**只有真机上才看得见**的问题：

1. **行号跟着代码继承字号**：`.git-diff-number` 是固定列宽 + border-box，代码涨到 13px 时
   行号也涨，**三位数就会顶出盒子**溢到左边那一列。现在显式压回 11px，并把列宽 34→36px、
   内边距收紧到 `7px/3px` —— 四位数也放得下（改之前四位数本来就在溢）。
2. **提交面板的视觉顺序**：DOM 里它夹在「已暂存」与「工作区」之间（桌面两栏并置时才对），
   单列下 260px 高的提交框会把「工作区」顶出屏幕。手机端用 `order: 1` 挪到最后
   （只改视觉顺序，不动 DOM，桌面端不受影响）。
3. **空态占位文案写了方位词**：文件内容区写着"从**左侧**…"，而单列没有左侧 ——
   它自己还排在提交面板之后，外加 680px 断点的 `min-height: 300px` 带边框空盒子。
   手机端整块用 `:has()` 收掉，文案也改成不带方位的说法。

### 本轮验证

| 项 | 结果 |
| --- | --- |
| `tsc --noEmit` | ✅ 0 |
| 前端全量单测 | **637/637 ✅**（新增的排版断言挂在既有的 Git 用例里，故条数不变） |
| `vite build` | ✅（`dist` 先按 `.gitignore` 精确清过，避开批量删除守卫） |
| Git 端到端探针 | **31/31 ✅**（+4：提交面板落在最后、空态占位不显示、代码字号 ≥13、行号 ≤11） |
| 文件探针（回归） | **40/40 ✅** |
| 变异检验 | 把"收起空内容区"那条规则改回 `display: flex` → 探针 **31 通过 / 1 失败**（正是那条）、`exit=1`；还原后 sha1 逐字节一致（`1d861c5a…`） |

出图：`outputs/mobile-git/01-changes-list.png`（列表，提交面板已在最后）、
`02-diff.png`（13px 代码 + 11px 行号）、`measure-conflict.png`（冲突解决视图）、
`measure-confirm.png`（确认弹层）、`measure-ops.png`（操作记录胶囊）。

**补一处（同日第二轮复查）**：冲突解决视图里「AI 模型」那一项原本是**原生 `<select>`** ——
"手机端不用原生 `<select>`"是本项目的硬规则（操作记录的筛选栏当初就是为它换成胶囊的），
这一处是漏网的。它只有两个选项（Claude / Codex），现在手机端换成胶囊
（`aria-pressed` 当选中态钩子、尺寸复用同一排按钮），桌面端保持下拉。
测试里钉住两条结构：**只有一个 `<select`**、且它排在胶囊那一支**之后**
（防的是"三元条件写反"——那样手机端仍旧渲染原生下拉，而源码里两样都在、看不出来）。

**同日又补一处（第三轮）**：Git **提交历史**列表只有「正在读取 / 没有记录」两态 ——
`git.log` 失败时它写的是「该分支没有可显示的提交记录」（一句关于仓库事实的断言），
而真相是**没读到**；提交**详情**那侧一直有 error 档，历史列表漏了。现在补成三态
（`.git-history-error` 卡片：读不到 + 原因 + 下一步），失败**就地**落、不再抛页面级错误条；
「加载更多」失败另在列表下方补一行说明。这一处**桌面端与手机端共用**，两端一起修好了。

**同日第二轮（用户："布局和样式……感觉好乱"）**：把"乱"拆成可测的东西 —— 用
`.tmp/measure-mobile-git.mjs` 新增的"对齐基线"普查量出**一屏有三条左基线**
（外壳 12 / 顶栏内容 24 / 卡片 28），统一成**外 24、内 39** 两条，并在探针里加了一条
数值断言（"卡片 / 顶栏内容 / 页签共享同一条左基线"）—— 正是它抓到页签落在 20（
680px 断点把 `.git-tab-list` 的内边距收成 8px）。

同轮去掉的重复与噪音：header 的分支胶囊（只留给文件子态）、"当前引用"标签与 OID 芯片、
提交区的第二个通栏按钮（降为文字按钮）；顶端三行收成一块（去掉顶栏与页签之间那条线、
页签底改白）；详情层的 `.git-changes-content` 外层框收掉（避免盒子套盒子）——
**注意这里踩了一次**：基准样式里 `> .git-diff` / `.git-conflict-solve` 是 `border: 0`
（故意让外层提供），只删外层会让详情卡片整片没有边，必须把边框补回里层。

## 17. 第三轮复查：样式规则之间的相互作用

这一轮换了个角度：不看"功能对不对"，看"**我改过的规则之间会不会互相打架**"。
把 `.tmp/measure-mobile-git.mjs` 扩成一次尺寸与形状审计（按钮盒子 / 内边距 / box-sizing /
内部 svg 的宽高 / 顶栏那一行有没有换行 / 全量非等比图标扫描），抓到 3 处真问题 ——
**全部由前两轮的改动叠加造成，都不是逻辑缺陷，且断言原本都抓不到**。

### 17.1 图标被压扁（`.git-bar-actions` 的宽选择器 × 固定宽高）

① 段那条 `.git-bar-actions button { padding: 10px 14px }` 是为三颗**文字**按钮
（拉取/获取/推送）撑热区写的，但它的选择器（特异性 (0,3,0)）也命中了同排的两颗
**图标**按钮；而那两颗又定了 `width/height: 40px`，加上全局 `* { box-sizing: border-box }`
⇒ 内容区只剩 `40 − 14×2 − 1×2 = 10px`，16px 的 svg 被压成 **10×16**（横向压扁、非等比）。

- **判据错在哪**：先写的是"图标有没有溢出容器"，实测 `svgOverflowX: 0` —— 浏览器是把
  它**压缩**进内容区，不是让它溢出。换成**宽高比**判据（`|w − h| ≤ 0.6 且 w ≥ 14`）
  才现形。这条已进探针（三颗图标各一条）。
- **修法**：在 ④ 段给这两颗**显式** `padding: 0; border-radius: 8px`。

### 17.2 一条死规则，以及它偷偷承载的东西

`.git-refresh/.git-ops-trigger { width: 36px; height: 36px; border-radius: 8px }` 与本轮
后来加的 40px 规则**选择器完全相同**，被后者整体覆盖 ⇒ 死规则（留着会让读代码的人
以为刷新按钮是 36px）。删它的时候才发现：**`border-radius: 8px` 只在这里声明过**，
删掉会让圆角退回基准的 6px，与同屏的 `.git-icon-btn`（8px）不一致。
⇒ **删规则前先问一句：它除了这件事，还承载了什么。**

### 17.3 `.git-mobile-back` 有三条定义

一条无 `[data-mobile]` 限定的基础样式、④ 段里的"按钮形态"、布局段里的"链接形态" ——
后两条互相覆盖，留下"padding 被最后一条盖掉、min-height 仍来自中间那条"的**半生效**状态。
现在只允许**一条**（限定 `[data-mobile]`，元素本来就只在手机端渲染），
并在测试里数 `\.git-mobile-back \{` 的出现次数。

### 17.4 核对通过、没有问题的部分

`data-mobile` 挂在 `.git-workbench` / `.git-bar` / `.git-changes-view` **三处**
（所以 `.git-changes-view[data-mobile="true"]` 那两条能命中、不是死规则）；
所有写进 CSS 的类名在组件里都存在（`git-icon-btn` / `git-group-actions` /
`git-inline-actions` / `git-ops-load-more` / `git-branch-switch-btn` …）；
`mobileDetailOpen` 与 `git-mobile-back` 的渲染条件语义等价（不会分裂）；
680px 断点与手机端规则的**覆盖方向全部正确**（两处都设 padding/max-height 的地方，
赢的都是预期的那条）；两处原生 `<select>` 都是 `mobile ? 胶囊 : select`
（手机端根本不渲染它，符合"手机端不用原生 `<select>`"）；对齐基线仍是外 24 / 内 39。

**验证**：`tsc` 0 / 单测 **637/637** / Git 探针 **35/35**（+3）/ 文件探针 **40/40** /
`vite build` ✅。**变异检验**：删掉图标按钮的 `padding: 0` ⇒ 探针 **2 条 FAIL、exit=1**，
还原后 sha1 与基线逐字节一致。
