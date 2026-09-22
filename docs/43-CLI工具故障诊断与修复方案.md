# CLI 工具故障诊断与修复方案

> 日期：2026-09-21
> 前置：`docs/42-CLI工具管理页调研与实施方案.md`（管理页的目录 / 探测 / 安装 / 升级 / 授权 / 审计骨架）
> 目标：在 CLI 工具管理页上加一个**问题检测诊断**能力 —— 能查出"这个 CLI 到底坏在哪"，
> 并给出**能真的把它修回可用**的动作。
> 触发场景（用户原话）：*一些 cli 在升级或者安装的过程中出现问题，导致 cli 工具无法使用，也无法更新。*

## 1. 结论

**问题不是"缺一个检测按钮"，而是三件事：**

1. **一个布尔把三种真相压成了一档**（§2.1）—— `installed=false` 同时表示"真的没装"与"装了但跑不起来"，
   于是界面既说不清、也给不出对的下一步；
2. **托管重装这条路没有回滚与残留清理**（§2.2）—— `npm install -g` 中断后留下的备份包与半成品
   从来没有人收拾，已有的回滚实现只挂在 CLI 自带 `update` 那条路上；
3. **"点了必失败"的结论只在点了之后才知道**（§2.3）—— `resolveAgentInstallPlan` / `resolveCrossInstallTarget`
   的判断是纯逻辑（只读文件系统与 PATH），完全可以提前跑出来告诉用户，现在却被藏在失败响应里。

所以本方案的核心是**只读诊断**（把"坏在哪、为什么、能怎么修"说清）+ **白名单修复动作**
（复用已有的回滚 / shim / 安装实现，不新造机制）。

**关键取舍：诊断与修复分成两个端点。** 诊断**不写任何东西、不在目标环境留任何痕迹** ——
这是 `docs/42 §18.1` 结尾为"探测不该留痕"去掉 mkdir 探可写性时定下的纪律，必须继续守。

## 2. 根因盘点（逐条对着源码）

### 2.1 `installed` 把三档压成了一档

`runner_agents.go:126`：

```go
Installed: status.Status != agentStatusUnavailable && status.Status != agentStatusUnsupported,
```

而 `agentStatusUnavailable` 由 `probeAgent`（`agent_probe.go:88-97`）在**两种完全不同的情形**下给出：

| 情形 | 判据 | 用户该做的事 |
| --- | --- | --- |
| 真的没装 | resolver 四路（override / 登记表 / PATH / 平台兜底）全未命中 | 去装 |
| **装了但跑不起来** | 二进制找到了，但 `--version` 返回空（半装 / shim 断 / node 没了 / 超时） | **去修** |

后果链（这就是"无法使用也无法更新"的完整机制）：

1. 界面徽标渲染 `item.reason`（`CliToolsPage.tsx:387`）→ "本机 Claude Code 未安装或不可执行"，
   但 `installed` 传的是 `false`，前端归入"未安装"分支；
2. `item.installed === false` ⇒ **不渲染「检查更新」**（`:415` 要求 `item?.installed`），
   只渲染「安装」（`:434`）；
3. 「安装」能不能点由 `canInstallAgent`（`:262`）决定，它要求
   `installSupported && runtime.npmVersion && runtime.meetsMinimumFor.includes(id)`。
   当**坏的正是托管运行时**时，这三条都不成立 ⇒ **连「安装」都不给**，
   只剩运行时卡片上的「安装 Node.js 运行时」。用户完全看不出"把 Node 装回来就能修好这个 CLI"。

> 这是本项目反复出现的那一族错：**把"读不到 / 用不了"写成"没有"。**
> `docs/42 §12.1` 已经为"通道坏了不能说成未安装"付出过一次，这里是同一个错误的第二处。

### 2.2 托管重装这条路没有回滚，也没有残留清理

`rollbackInterruptedNpmInstall`（`npm_cli_install.go:114`）与 `ensureNpmCLICommand`（`:163`）
**都已经实现好了**，但调用点只有两处（`claude_runner.go:419`、`codex_runner.go:170`）——
都挂在**CLI 自带 `update`** 的失败路径上。

而管理页的「升级」对平台装的工具走的是**另一条路**：`performAgentUpdate`（`app.go:7810`）
判定登记为 npm 类后直接调 `installAgentCLIFor(…, "latest")` → `installAgentCLI`
（`agent_install.go:170`）。这条路**没有回滚、没有残留清理**：

- `npm install -g <pkg>@latest` 中途被杀 / 断网 ⇒ npm 已把旧包 rename 成
  `.{pkg}-<oldVer>`，新包半装；
- 装后自检失败 ⇒ `installAgentCLI` 直接返回错误，**登记表保持旧值**（还是旧 binary_path / 旧 version），
  但**磁盘已经变了**；
- 再点一次 ⇒ 探测读不出版本 ⇒ 界面说"未安装"、只给「安装」⇒ 再跑一次 npm ⇒
  失败就再留一个半成品目录。**每次重试都在累积垃圾，且从来没人清。**

顺带一个推论：`ensureNpmCLICommand` 唯一的调用点是回滚路径。也就是说
**shim 只在回滚时会被重建**；正常安装的 shim 由 npm 自己写。
一旦 shim 被删/损坏而 prefix 里又没有可回滚的备份包，**当前没有任何代码能把它建回来**。

### 2.3 "点了必失败"可以在点之前就知道

`resolveAgentInstallPlan`（`agent_install.go:57`）与 `resolveCrossInstallTarget`
（`cross_install_target.go:29`）都**只读**：`fileExists` + `exec.LookPath` + 登记表，
一条命令都不执行。它们已经能准确回答"这台机器上现在能不能装 / 能不能升级、不能是为什么"，
而且 `docs/42 §19.2 E` 已经把这两处收成同一份判据（`meetsMinimumByInstallKind` 复用前者）。

但这些结论**只出现在失败响应里** —— 用户必须先点一次、失败一次，才能看到那句
"请先重新安装 Node.js 运行时"。把同一份纯逻辑提前跑一遍，成本接近零。

### 2.4 已经躺在那里的、免费的证据没人读

`agent_install_audit` 表（`runner_install_grants.go:31`）记录了每次 install / update /
install-runtime 的 `result` 与 `detail`（**失败原因原文**，`app.go:7868/7077/322`），
并且已经有 `GET /api/runners/{runnerID}/install-audit`（`app.go:1148`）。

**诊断报告不读它，等于把用户唯一的现场证据浪费掉。** 一句
「上次升级失败：安装 Claude Code 后自检失败：…（原始 npm 输出）」比十行推测都值钱，
而且是零成本的。

### 2.5 路径事实没有视图，于是"升级了没生效"无法解释

resolver 的解析顺序是 `override > 登记表（实测存在）> PATH > 平台兜底`
（`agent_paths.go:58`），而且**登记表的路径只要文件还在就会被优先采用**（`:71`）。

于是出现这个形状：

| 时刻 | 磁盘状态 | 生效路径 |
| --- | --- | --- |
| T1 | 平台装到托管 prefix，登记 path = P，P 可用 | P |
| T2 | 一次失败的升级把 P 位置的文件毁掉（文件在、跑不起来） | **仍然是 P**（`fileExists` 为真） |
| T3 | 用户自己又装了一份到系统 npm 全局（全局可用） | **仍然是 P** —— 新的那份永远不生效 |

用户看到的是"我明明装了新版，界面还是说不能用"。**当前没有任何界面或接口能解释这件事** ——
因为"这台机器上这个工具一共有几份、分别在哪、各自能不能跑"这个事实从来没被枚举过。

### 2.6 其余已知但本方案只报告、不修的

| 形状 | 说明 |
| --- | --- |
| `native` 登记 | 用户用官方安装器装的，平台不接管升级（`agent_install.go:105`）。诊断应报告，并给手动命令（界面已有） |
| 探测 8s 超时（`runtime_install.go:132`） | 启动慢的 CLI 会被读成"未安装"。诊断要**把"超时"与"输出为空"分开** |
| 登记 kind 不认识 | `resolveCrossInstallTarget:57` 已经如实报错，诊断只需把它提前 |
| 通道坏了（WSL / SSH） | `probeOk=false`，已有三档语义，不重复 |

## 3. 领域模型

新增 `apps/control-server/internal/app/agent_diagnose.go`。

```go
// 症状码：稳定的标识符，界面按它选图标/分组，断言按它钉行为。
// 每一档都必须是"下一步动作各不相同"的 —— 否则就是又一次把两种真相压成一句灰字。
const (
    issueBinaryMissing       = "binary-missing"        // 登记了但文件不在
    issueBinaryBroken        = "binary-broken"         // 文件在，执行失败/无输出
    issueProbeTimeout        = "probe-timeout"         // 能执行但超时（与上一条分开！）
    issueShimMissing         = "command-shim-missing"  // 命令入口不存在
    issueShimDangling        = "command-shim-dangling" // 入口存在但指向不存在的目标
    issuePackageInterrupted  = "package-interrupted"   // 留下 half-install 残骸
    issuePackageBackup       = "package-backup-available" // 有可回滚的完整备份包
    issueActivePackageBroken = "active-package-broken" // 生效包目录结构不完整
    issueRuntimeMissing      = "runtime-missing"       // 前置运行时没了
    issueRuntimeBroken       = "runtime-broken"        // 运行时文件在但跑不起来
    issueRuntimeTooOld       = "runtime-too-old"
    issueNpmUnavailable      = "npm-unavailable"
    issueShadowedInstall     = "shadowed-install"      // 机器上有多份，生效的不是预期那份
    issueRecordStale         = "record-stale"          // 登记表与磁盘实测不符
    issueRecordKindUnknown   = "record-kind-unknown"
    issueNativeUnmanaged     = "native-unmanaged"      // 平台不接管（如实说）
    issueEnvUnsupported      = "env-unsupported"
    issueChannelFailed       = "probe-channel-failed"  // 通道坏了（WSL/SSH）
)

// diagnoseIssue 是一条诊断发现。
type diagnoseIssue struct {
    Code     string   `json:"code"`
    Severity string   `json:"severity"` // blocker | warning | info
    Summary  string   `json:"summary"`  // 一句话，可直接给用户看
    Evidence []string `json:"evidence"` // 证据行（路径/版本/输出尾部），界面原样展示
    Remedies []string `json:"remedies"` // 该症状可用的修复动作 id（**服务端下发**）
}

// diagnosePathFact 是"这个工具在这台机器上的一份安装"。
// 四条来源逐一枚举 —— 这是"两份 CLI / 升级没生效"的唯一解释视图。
type diagnosePathFact struct {
    Source  string `json:"source"` // effective | path | system-npm | managed-prefix | recorded
    Label   string `json:"label"`  // 给用户看的来源说明
    Path    string `json:"path"`
    Exists  bool   `json:"exists"`
    Works   bool   `json:"works"`   // 真的执行一次成功
    Version string `json:"version"` // 实测版本
    Note    string `json:"note,omitempty"` // "当前生效的就是这一份" 等
}

// diagnosePreflight 把"点了会失败"提前说出来。
type diagnosePreflight struct {
    InstallOK     bool   `json:"installOk"`
    InstallReason string `json:"installReason,omitempty"` // resolveAgentInstallPlan 的原话
    UpgradeOK     bool   `json:"upgradeOk"`
    UpgradeReason string `json:"upgradeReason,omitempty"`
}

// agentDiagnosis 是单个工具的完整诊断报告。
type agentDiagnosis struct {
    AgentID string `json:"agentId"`
    Status  string `json:"status"` // ok | broken | not-installed | unmanaged | unsupported | channel-failed
    Version string `json:"version"` // 实测版本，可能为空
    Issues  []diagnoseIssue  `json:"issues"`
    Paths   []diagnosePathFact `json:"paths"`
    // LastFailure 取自 agent_install_audit —— 零成本、最有用的现场证据。
    LastFailure *installAuditEntry `json:"lastFailure,omitempty"`
    Preflight   *diagnosePreflight `json:"preflight,omitempty"`
    // DiagnosedAt 让界面能说"3 分钟前检测"。**不要**缓存到进程里：
    // 磁盘状态随时会被一次失败的安装改掉。
    DiagnosedAt time.Time `json:"diagnosedAt"`
}
```

**`Status` 与 `Issues` 是两个维度，不要合并**：`status=broken` 是一条结论，
`issues` 是得出这条结论的一条条证据 —— 界面要把证据逐条显示出来，否则用户还是要猜。

## 4. API

```
GET  /api/runners/{runnerID}/agents/{agentID}/diagnose      # 只读，单工具
GET  /api/runners/{runnerID}/diagnostics                    # 只读，批量（只出"有 blocker"的）
POST /api/runners/{runnerID}/agents/{agentID}/repair         # 写入，白名单动作
```

### 4.1 `GET .../diagnose`

响应体即 §3 的 `agentDiagnosis`。要点：

- **只读**：不写文件、不改登记表、不在目标环境留任何东西。
  可写性**不探测** —— 那一档改成"修复失败后的错误分类"，或者在只读侧看权限位。
  （`docs/42 §18.1` 已经为这件事去掉过一次 mkdir 探可写性，不能加回来。）
- 与 `GET .../agents` 的关系：列表端点是"概览"（每个工具一行状态），
  诊断端点是"详查"（一个工具）。**诊断不做进列表端点** —— 它要跑多次子进程
  （每个候选路径各一次 `--version`）、要扫目录、要读审计。
- `preflight` 直接复用 `resolveAgentInstallPlan` + `checkRuntimeGate`（本机），
  **不新造第三套判据**。这是本方案最关键的一条纪律（`docs/42 §19.2 E` 的教训）。
  ⚠️ 它**在本机是两步**：解析出计划 ≠ 装得上（Node 太旧时闸门会拒）。只做第一步的话，
  预检会说"可以装"、点下去才报"运行时版本过低"—— 而把这句话提前正是预检存在的理由。
  ⚠️ **跨端还没有预检**：`resolveCrossInstallTarget` 是纯函数，但它的入参（目标环境的
  运行时状态）要先在目标环境跑一次探测，尚未接通。报告里已如实写进 `limitations`
  （见 §13.8），不是"悄悄地没有"。

### 4.2 `GET .../diagnostics`

批量版本，供管理页一次拿到"哪些工具有问题"。两条约束：

- **只对有问题的出报告**（`ok` 的工具只回 `{agentId, status:"ok", version}`），
  否则一次请求会跑 `工具数 × 候选路径数` 次子进程；
- 判定"要不要详查"的判据本身要便宜：`installed=false` 或 `probeOk=false` 或
  **登记表有记录但探测失败** —— 最后一档正是本方案要抓的形状。

### 4.3 `POST .../repair`

```jsonc
// request
{ "remedies": ["restore-backup", "rebuild-shim"] }   // 只接受白名单 id
// response
{
  "success": true,
  "applied": [
    { "id": "restore-backup", "ok": true,  "detail": "已恢复到 Claude Code 2.1.216" },
    { "id": "rebuild-shim",   "ok": true,  "detail": "已重建 claude.cmd" }
  ],
  "diagnosis": { /* 修复后重跑的完整报告 —— 让界面能当场断言"症状真的没了" */ }
}
```

三条硬约束：

1. **`remedies` 是服务端下发 id 的复述，不是命令。** 服务端拿到 id 后在白名单里查表执行，
   **绝不把请求体拼进任何命令**（对标 `mcpRuntimeCommandPattern`，`docs/42 §9.4`）。
2. **与安装/升级共用同一道闸门**：`beginAgentMaintenance`（并发 + 活跃会话）
   + `remoteInstallAllowed`（跨端授权）+ `recordInstallAudit`（审计）。少任何一条都是在开新的后门。
3. **响应里回填修复后的诊断**。让"修好了"变成一个可断言的事实，而不是一句 toast。

## 5. 修复动作白名单

| ID | 做什么 | 复用 | 风险 | 一轮/二轮 |
| --- | --- | --- | --- | --- |
| `rebuild-shim` | `ensureNpmCLICommand(prefix, install)` | **已有函数，零新代码** | 幂等；只写我们自己 prefix 下的入口 | 一轮 |
| `cleanup-interrupted` | 删 prefix 下匹配 `.{包名}-interrupted-*` 的目录 | 新（~30 行） | 只删自己 prefix 内、名字严格匹配的目录 | 一轮 |
| `restore-backup` | `rollbackInterruptedNpmInstall(prefix, 登记版本, install)` | **已有函数，零新代码** | 只在 prefix 内 rename；版本必须与登记一致 | 一轮 |
| `reinstall` | `installAgentCLI(原版本, 失败则 latest)` —— 与原「安装/升级」**同一条代码路径** | `installAgentCLI` | 与既有安装同等（已有闸门与自检） | 一轮 |
| `install-runtime` | `installRuntimeFor` | 已有 | 已有闸门 | 一轮 |
| `reset-record` | 按**实测**重写 `agent_installations` 一行 | 新（~40 行） | ⚠️ 改的是"升级走哪条路"的判据 ⇒ 必须用户确认 + 审计 | 二轮（先只报告） |

**明确不提供**（沿用 `docs/42 §11` 已拍板的操作面）：

- **不做卸载**。"修复"的目标是回到可用，不是删东西；
- **不删用户自己的安装**（系统 npm 全局那份、官方安装器那份）。`shadowed-install` 只报告；
- **不用 sudo / 不提权**；
- **不允许指定任意版本**（`reinstall` 只回到"登记的那个版本"，失败再退到 `latest`）；
- **不改登记表**（`reset-record` 是唯一的例外，且要用户确认）。

### 5.1 `reinstall` 的执行顺序（要写死在代码里）

```
① 若有 package-backup-available  → 先 restore-backup（成本最低、最容易成功）
② 否则                           → 清理 package-interrupted 残骸
③ 然后                           → installAgentCLI(登记版本 || latest)  ← 与原安装同一条路
④ 自检（真的执行 --version）+ 重跑诊断
```

顺序不许换：**先回滚再重装**，因为回滚是本地 rename（秒级、不依赖网络），
重装要联网下载。能一步回到可用，就不该先冒险联网。

## 6. 前端

`CliToolsPage.tsx` + `cli-tools.css` 增量改造，**不新建页面**。

### 6.1 入口与触发

- 页头加一个「检测问题」按钮（**不是进页面自动跑** —— 诊断要起多次子进程，
  与 `docs/42 §14.G`"运行时探测不进热路径"同一条理由）。
- 例外：对**已经有 blocker 迹象**的工具（`!installed && 登记表有记录`）在页面加载后
  自动诊断一次 —— 这一档本来就少，而且用户正是为它进来的。
- 卡片徽标旁加一个"有问题"角标（`data-issue` 变体，不复用 `.pending`/`.missing` 的语义）。

### 6.2 诊断面板

复用 `.cli-tools-card` 的骨架，四个区块：

```
┌ Claude Code ─────────────────────── [已安装但不可用] ─┐
│ ⚠ 安装文件存在，但执行失败（半装）                    │
│    证据：claude.cmd --version → 退出码 1              │
│          ~/.local/share/milevia/toolchain/npm-global/…│
│    [恢复到 2.1.216]  [重新安装]                       │
│ ⚠ 发现可回滚的完整备份 2.1.216                        │
│    [恢复它]                                           │
├───────────────────────────────────────────────────────┤
│ 这台机器上的所有位置                                  │
│  当前生效   …/npm-global/bin/claude   ✓存在 ✗可执行  —│
│  PATH       C:\Users\…\npm\claude.cmd  ✓存在 ✓可执行 2.1.217│
│  系统 npm   …（当前生效的就是托管那份）—              │
│  登记表     …/npm-global/bin/claude   ✓存在 ✗可执行   │
├───────────────────────────────────────────────────────┤
│ 上次失败（3 小时前 · 升级 2.1.216 → 2.1.216）          │
│ 安装 Claude Code 后自检失败：…（npm 输出尾部）        │
├───────────────────────────────────────────────────────┤
│ 预检：现在点「安装」会失败 —— 托管运行时缺失，请先安装 Node.js 运行时│
└───────────────────────────────────────────────────────┘
```

四条渲染纪律：

1. **诊断自身也是三态**：读不到 / 没有发现问题 / 发现了问题。
   合并任何两档都会让用户去做没用的事 —— **这是本项目第五次踩它**，必须写成断言。
2. **修复动作列表来自服务端**（`issue.remedies`），前端不拼命令、不硬编码 remedy id。
3. **徽标与诊断不许打架**：有 blocker 时不出「已是最新」这类乐观文案。
4. 修复确认弹窗复用既有 `.backdrop` / `.modal` 与 `PendingAction` 模式，
   逐条列出"将做什么 / 影响什么 / 可能需要多久"。

### 6.3 修复后的对比

修复完成时**逐条对比**：`binary-broken 已解决` / `package-backup-available 仍在`。
不要只说"修复完成" —— 那是把"我只做了一半"包装成成功。

## 7. 一处必须拍板的取舍

**徽标对"登记有记录但探测失败"这一档该说什么？**

| 选项 | 说法 | 代价 |
| --- | --- | --- |
| A（保守） | 维持现状：`item.reason` = "未安装或不可执行" | 与诊断结论并存时会读起来像两件事 |
| B（推荐） | `reason` 由**同一处判据**产出一句"已安装但不可用，可检测修复" | 要动 `agentUnavailableReason`（`agent_probe.go:142`），并同步既有断言 |
| C（激进） | 改 `installed` 的语义，拆成 `installed` / `usable` 两个字段 | ❌ 会波及对话页、手机端快照、`legacyAgentFields` —— **不做** |

**建议 B**：仍然只动一处判据（`agentUnavailableReason`），
让它对"登记表里有记录"的情形给出"已安装但不可用"而不是"未安装"。
`installed` 的语义保持不变 —— 那是给对话页与手机端用的，不该为了管理页改线格式。

## 8. 安全边界（复用既有，不新开后门）

| 项 | 做法 |
| --- | --- |
| 诊断的只读性 | 不写文件、不改登记、不留痕迹；不探测可写性（§4.1） |
| 修复的闸门 | `beginAgentMaintenance`（并发 + 活跃会话）+ `remoteInstallAllowed`（跨端）+ 审计 |
| 输入 | `remedies` 只认白名单 id；工具/主机查目录与注册表；**禁止拼接请求体** |
| 跨端 | 一轮只开放只读诊断；`repair` 只给 `reinstall`（那条通道已端到端验证过，`docs/42 §20.1`） |
| 手机端 | **不暴露到 `/api/remote/*`** —— "修复"是写操作，与 `docs/42 §9.3` 同一条边界 |
| 输出 | 复用 `redactAgentText` / `stripAnsi` / `tailUpdateOutput` 清洗后再入库与展示 |

## 9. 测试矩阵

### 9.1 Go 侧

| # | 用例 | 挡的是什么 |
| --- | --- | --- |
| 1 | 跑一次 `diagnose`，托管目录的文件清单与 mtime **逐字节不变** | "诊断偷偷写东西" |
| 2 | 夹具造 8 种磁盘形状（半装包 / 断 shim / 死登记 / 两份 CLI / 备份包 / 残骸 / 运行时丢失 / native），断言**症状码 + 证据 + 可用修复动作集** | 症状分类本身 |
| 3 | `rebuild-shim` 跑两次，第二次 mtime 不变 | 幂等 |
| 4 | 造 `.claude-code-2.1.216` 备份 + 半装 active，跑 `restore-backup`，断言**执行 `--version` 得到 2.1.216** | 行为级（不看文件在不在） |
| 5 | 夹具里放 `.claude-code-other`（同前缀但非版本）、`claude-code-interrupted-x`（无前导点）、别的包的 `.{other}-interrupted-*`，断言**都没被删** | 清理越界 |
| 6 | `{"remedies":["rm -rf /"]}` → 400，且**一条命令都没发出** | 白名单 |
| 7 | 未授权 runner 上 `repair` → 403，且**没有发出任何脚本**（成对断言，照 `docs/42 §17.4`） | 授权闸门 |
| 8 | `repair` 撞上进行中的操作 → 409（判据是 `runnerUpdateExecuting`，**不是**按 (runner,agent) 的那个槽位） | 闸门与安装/升级同源。⚠️ 诊断是**只读**的，不进闸门，所以它**不参与**互斥 —— 初版这条写错了预期，见 §13.6 |
| 9 | `repair` 落一条 `action=repair` 且 detail 带被应用的 remedy id | 审计 |
| 10 | **`preflight.installReason` 与真的 POST install 失败的返回文案逐字相同** | ⭐ "诊断另写一套判据" —— 本项目付过代价的那个错 |
| 11 | `probe-timeout` 与 `binary-broken` 在同一夹具上必须给出**不同的码** | 把两种真相分开 |
| 12 | 通道失败（`probeOk=false`）时不出任何"工具坏了"的结论 | 三档不许合并 |

### 9.2 前端

| # | 用例 |
| --- | --- |
| 1 | 诊断三档分开渲染（读不到 / 没问题 / 有问题），文案互不相同，且**剥注释后**判断 |
| 2 | 源码里不出现硬编码 remedy id（修复动作全部来自服务端） |
| 3 | 有 blocker 时不渲染「已是最新」/「升级到 x」 |
| 4 | 探针：真页面 + 真服务端，造一个坏工具 → 出现症状文案 → 点修复 → 症状消失 |
| 5 | 变异检验：把 `cleanup-interrupted` 的"名字必须匹配我们包名且带前导点"去掉 ⇒ 9.1 的 #5 必须红 |

> 断言纪律（自证断言 / 成对用例 / 夹具覆盖面 / `SKIPPED` 与 `ESCAPED` 必须为 0）见 `TOOLING.md`。
> 夹具必须照**真实形状**造（半装包目录的真实命名是 `.{pkg}-{version}` / `.{pkg}-interrupted-{nano}`，
> 来自 `npm_cli_install.go:122-124`），不要照印象编。

## 10. 实施顺序

| 步 | 内容 | 风险 |
| --- | --- | --- |
| 1 | **只读诊断**：`agent_diagnose.go` + `GET .../diagnose` + Go 用例 1/2/10/11/12 | 最低 —— 不写任何东西 |
| 2 | **前端展示**：诊断面板 + `GET .../diagnostics` | 低 |
| 3 | **三个安全修复动作**：`rebuild-shim` / `cleanup-interrupted` / `restore-backup`（都只动我们自己 prefix 内的东西、幂等）+ `POST .../repair` | 低 |
| 4 | **两条重动作**：`reinstall` / `install-runtime`（复用既有闸门与自检） | 中（与既有安装同量级） |
| 5 | **顺手改一处最值钱的**：`installAgentCLI` 失败时，把"prefix 里存在可回滚的完整备份 `2.1.216`"直接写进错误文案，并指向诊断入口 | 低 |
| 6 | 跨端：只读诊断；`repair` 只开放 `reinstall` | 中（跨端通道） |
| 7 | `reset-record`（按实测修登记），先只报告不执行 | 中高（改升级判据） |

## 11. 本期不做

- 不做卸载（沿用 `docs/42 §11`）；
- 不删用户自己的安装、不 sudo；
- 不做"自动定时巡检"（诊断是用户触发 + 有问题时一次）；
- 不做跨 Runner 批量修复；
- 不在 `/api/remote/*` 暴露任何诊断/修复端点；
- 不改 `installed` 的语义（§7 选项 C）。

## 12. 待拍板

| # | 决策 | 建议 |
| --- | --- | --- |
| 1 | 徽标对"登记有记录但探测失败"的说法（§7） | 选 B：同一处判据产出"已安装但不可用" |
| 2 | `repair` 是否允许"一键全部修复"（按依赖顺序自动排 `restore-backup → cleanup → rebuild-shim → reinstall`） | 建议要 —— 但每一步仍要在确认框里逐条列出 |
| 3 | 诊断是否需要保留历史（"上次诊断结论"） | 建议不要。磁盘状态随时会变，缓存结论比没有结论更坏 |
| 4 | 是否把审计记录的 `detail` 原文直接展示 | 建议要（先经 `redactAgentText`），但只展示尾部 8 行（复用 `tailUpdateOutput`） |

## 13. 实现记录（2026-09-21 第 1–4 步已落地）

> ⚠️ 本节的 §3–§6 是**设计期草图**。权威形状以 §13 的实现记录为准，
> 已知差异集中在 §13.3（三处更保守）与 §13.7（复查轮第二批）：

### 13.1 拍板结果

四项全部按建议执行：**B（只改 `agentUnavailableReason` 的说法，不动 `installed` 语义）** /
**要一键修复（服务端定序）** / **不缓存诊断历史** / **展示审计原文尾部**。

### 13.2 落地的文件

| 文件 | 内容 |
| --- | --- |
| `apps/control-server/internal/app/agent_diagnose.go` | 19 个症状码 + 报告模型 + 本机完整诊断 + 跨端受限诊断 |
| `apps/control-server/internal/app/agent_diagnose_http.go` | `resolveDiagnosisTarget` + 两个只读端点 + `needsDeepDiagnosis` |
| `apps/control-server/internal/app/agent_repair.go` | 5 个动作的白名单表 + `planRemedies` + `POST .../repair` |
| `apps/control-server/internal/app/agent_probe.go` | `agentUnavailableReason` 改为方法形态 + 纯函数 `agentUnavailableReasonText` |
| `apps/web/src/lib/cli-diagnosis.ts` | 前端模型（纯函数：结论怎么念 / 动作怎么归并 / 结果怎么说） |
| `apps/web/src/pages/CliToolsPage.tsx` | 诊断行 + 诊断面板 + 修复确认框 |
| `apps/web/src/pages/cli-tools.css` | 四档 tone 观感（变体走 `data-tone`） |

路由三条（`app.go`）：`GET .../agents/{agentID}/diagnose`、`GET .../diagnostics`、
`POST .../agents/{agentID}/repair`。

### 13.3 与方案的差异（三处，都更保守）

1. **`reset-record` 本期不做，已从白名单里去掉**。原先两条"认不出的安装方式"的症状
   把它列为可用动作 —— 那会亮出一个点了没反应的按钮。改为给 `reinstall`
   （重装会把登记改写成一种确定的安装方式，是同一个问题的另一条更安全的出路）。
2. **`remedies` 从 id 数组改成对象数组**（`{id,label,detail}`）。原方案只给 id，
   界面就得自己维护一份 id→文案的映射 —— 那必然与服务端漂移。现在
   `agentDiagnosis.add()` 会**过滤掉白名单里不存在的 id**，于是"界面照着诊断亮出一个
   点了没反应的动作"在结构上不可能发生（有专门用例钉它）。
3. **跨端诊断只做能确定的事**：探测说就绪 ⇒ `ok`；探测说不可用 ⇒ `unknown` + 列出
   没跑的检查。**不猜** broken 或 not-installed。

### 13.4 实测推翻/修正过的东西（这节最值钱）

1. **`cmd.WaitDelay` 是必需的，不是保险。** 进程被 `Kill` 之后，它的后代（npm 的子
   shell、CLI 拉起的后台进程）仍然握着 stdout 的写端，于是 `cmd.Wait()` 会一直等到
   那些进程退出。实测：一次 300ms 预算的探测真的耗了 **29.7 秒**（夹具里 `ping -n 30`）。
   ⚠️ **`runVersionCommand`（`runtime_install.go:131`）有同一个问题**，而它在
   `/api/runners` 的热路径上（`probeAgents` → `probeRuntime`）。本轮没动它（保持改动
   范围），但这是**下一条该修的**：一个卡住的 CLI 会让整个仪表盘挂住。
2. **Windows 的目录 mtime 是延迟写的**，"诊断不改磁盘"的断言不能比目录 mtime ——
   实测同一个没动过的目录，前后两次 `stat` 也可能给出不同的值（假红）。
   快照改成：目录只记名字与模式，**文件**才记 size+mtime。
3. **开发机上真的装着全局 claude**（`%APPDATA%\npm\claude.cmd`），而
   `platformFallbackCandidates` 会如实把它找出来 —— "什么都没装"这类断言必须同时隔离
   `PATH` 与 `APPDATA`，否则 not-installed 会被判成 ok（实测踩到）。
4. **夹具限制改变了用例设计**：Windows 上写不出真正的 PE，而 claude 的 npm bin 是
   `.exe`（平台生成的入口直接执行它）⇒ 涉及"重建出来的入口真的能跑"的用例改用
   **codex**（bin 是 `.js`，入口走 `node "%~dp0…"`，可以用一个假 node 测到底）。
5. **"症状没了就不许再执行该动作"会让幂等分支在端点层不可达**：`rebuild-shim` 成功后
   症状消失，第二次请求会被 `applicableRemedies` 挡成 400。所以幂等改为**直接调动作本身**
   去测，另加一条用例测那道过滤（两条防线分别钉）。
6. **HTTP 层会给失败文案加一句通用前缀**（`localizedHTTPErrorText` 按状态码选，
   为的是不把内部细节泄露给界面），所以"预检与真实失败逐字相同"这条断言要比较
   **内核那一串**，而不是整串 —— 两处的通用前缀本来就不同，而且是有意的。

### 13.5 验证结果

- Go：`go build ./...` / `go vet ./internal/app/` 干净；新增 **32** 条用例全绿
  （15 条诊断 + 16 条修复 + 1 条探测回归），连同相邻既有用例一并回归通过。
- 前端：`tsc -b` 0；`vite build` 0；单测 **668/668**（新增 12 条：6 条纯函数 + 6 条源码断言）。
- `internal/app` 全量在跑 —— 本轮动了 `agent_probe.go` 的对外文案，
  要看有没有别的断言钉着旧句子（全量已抓到过一次真实回归，见 §13.4 第 2 条）。
- 未做：真实浏览器探针（真页面 + 真服务端点一次修复）、变异检验。
  这两项按 `TOOLING.md` 的纪律属于"每条新防线配两套"里的第二套，
  **是本轮的欠账**，见 §14。

## 14. 本轮欠账（如实记）

| # | 欠账 | 为什么不能省 |
| --- | --- | --- |
| 1 | 真实浏览器探针：真页面 + 真服务端，造一个坏工具 → 出现症状文案 → 点修复 → 症状消失 | Go 侧 49 条覆盖的是判据与安全边界，**覆盖不到"界面真的接通了"** |
| 2 | 变异检验：目前只做了 1 处（运行时只探一次，见 §13.7）。其余防线仍未做 —— 例如把 `cleanup-interrupted` 的"名字必须匹配我们包名且带前导点"去掉 ⇒ §9.1 #5 必须红；把 `applicableRemedies` 的过滤去掉同理；把 `hasEffective` 改回取 `probes` ⇒ §13.7 #1 必须红 | 结构断言挡不住"逻辑写反了" |
| 3 | 跨端深度诊断（去目标环境跑只读脚本） | 现在跨端只会说 `unknown` + 列出没跑的检查（路径事实也只给"未核对"）；`repair` 在跨端因此基本用不上 |
| 4 | `runVersionCommand` 的 `WaitDelay` | 它和 §13.4 第 1 条是同一个问题，而它在 `/api/runners` 热路径上 |
| 5 | `listRunnerAgents` 重构后，**"注册好的 runner"那条既有用例在本机跑不了** | `newTestServer` 会走 `New()` → 拉起 `wsl.exe` → 沙箱拦截并**中止整条命令**（`TOOLING.md` 记过）。这次只能按"逐行等价"人工核对（只换头部、函数体一字未动），**没有机器验证**。要真验证得在没有该黑名单的环境上跑 `TestListRunnerAgentsCoversCatalogWhenRegistered` |
| 6 | `runtimeManager.fetchNodeVersionIndex` 仍然没有 single-flight | §13.7 把"同一个批量请求里的 N 次探测"收成 1 次，但同一次页面加载里 `listRunnerAgents` 与 `listRunnerDiagnostics` 仍各探一次（各带一次索引下载）。真正该修的是缓存层加 single-flight，属 `runtimeManager` 的事，不在本轮范围 |
| 7 | 跨端**预检**（"点了会不会失败"） | 入参是目标环境的运行时状态（要先在目标环境跑一次探测）。现在只在报告的 `limitations` 里如实写"跨端没有预检"，让界面不至于给人"什么都查过了"的印象 —— 但用户仍然要先失败一次才知道结果（§13.8 已把文档里那条不实的承诺改掉） |

## 13.6 复查轮（换视角复审）

两路独立子代理审查这次因网络不可用没能起，改为自己**换视角**审（"自己复查容易只沿原思路
走一遍" —— 所以按维度分组重读，而不是重读一遍）。挑出 **9 处问题，全部修掉**。

### 真问题（会给出假信息或假的边界）

| # | 问题 | 为什么是问题 | 修法 |
| --- | --- | --- | --- |
| 1 | **诊断不检查"安装进行中"** | 那一刻产物正被替换（npm 先改名再解压），探测拿到空版本 ⇒ 诊断会给出一条**假的 `binary-broken`**，用户照着去点修复、而修复被闸门挡成 409。`probeAgent` 早就有这条判断（`agent_probe.go:70`），诊断绕过了它 | 诊断开头查 `agentMaintenanceActive` → `status=unknown` + 新症状码 `maintenance-active`，**一条探测都不跑**；跨端也补了显式的 `updating` 档 |
| 2 | **`runnerDiagnosticsView.ProbeOK` 恒为 `true`** | 这是本项目红线里的"假装存在的边界"：服务端算了个字段、前端没法消费它。更坏的是列表页说"无法检测"、诊断页却照旧列出一排结论 —— 用户不知道该信哪个 | 把"通道级失败"的判断抽成 `resolveProbeTarget`，`listRunnerAgents` 与 `listRunnerDiagnostics` **同源**；通道坏了就不详查、不给任何工具结论（全部进 `skipped`） |
| 3 | **`applicableRemedies` 的跨端例外放得太宽** | `diagnosisUnknown` 不止"跨端受限"：本机**登记表读失败**也是 unknown，而那时 prefix 给不出来 ⇒ 界面上多出两个**点了必失败**的按钮 | 例外加上 `!local` 条件（它本来就是为跨端存在的） |
| 4 | **"已解决 N 项"永远显示不出来**（前端，我自己实现的缺陷） | 修复成功后 `refresh()` 会重跑批量诊断，而修好的工具已 `ready` ⇒ 被判为"不必详查"（`skipped`，`diagnoses[key]` 被移除）⇒ 那条提示被关在展开面板的渲染条件里，**永远不出现** —— 而那一刻正是最该看见它的时候 | 把提示渲染到**面板之外**；用例里加了位置断言（它必须排在面板之前） |

### 一般（会误导，但不给假结论）

| # | 问题 | 修法 |
| --- | --- | --- |
| 5 | **审计把"没执行"记成 `failed`** | `repairStep` 加 `skipped`，`repairAuditDetail` 三态分开记（`ok`/`failed`/`skipped`）。把从未执行的动作记成一次"发生过的失败"，是让审计说假话 —— 而审计唯一的职责就是"发生过什么" |
| 6 | **`success` 把"被跳过"算成失败** | 只由**真的执行过**的动作决定；被跳过的仍在 `applied` 里（`skipped=true`），前端据此说"另有 N 个动作没执行"。否则用户会收到一句"修复失败"而症状其实已经修好 |
| 7 | **跨端得到的解释是错的** | `planRemedies` 的 `LocalOnly && !local` 判断原先排在 `!allowed` 之后 ⇒ 跨端请求"重建入口"被告知"诊断里没有给出它"，而真相是"这个动作要在目标机器上直接操作文件，跨端还没接通"。两句话对用户的含义完全不同 |
| 8 | **`applyCleanupInterrupted` 的收尾断言对着"所有残骸"** | 改成**逐个目标**确认（"我删掉的那一个真的没了"）；并把"不符合判据、已保留"如实说出来，而不是算进 `removed`（那会把"我没做"写成"没有这东西"） |

### 文档与实现的偏差

| # | 问题 | 修法 |
| --- | --- | --- |
| 9 | **§6.3「修复后的逐条对比」没实现** | 前端新增纯函数 `resolvedIssues(before, after)`（用 `code` 当身份算差集）与 `resolvedSummary`；修复后把差集说给用户听。这正是"不要只说修复完成"那条纪律的直接落实 |

顺带修了 §9.1 #8 那条**写错的预期**：诊断是只读的、不进闸门，所以"诊断进行中发起 install"
不会有 409 —— 参与互斥的只有 `repair`。用例改成"`repair` 撞上进行中的操作 → 409，
且**被挡住时不动磁盘**"。

### 复查后验证

- Go：`vet` 干净；定点 **41** 条用例全绿（新增 5 条：maintenance 假症状、跨端例外不放宽到本机、
  skipped 不算失败且审计分开记、两处端点 probeOk 同源、repair 闸门互斥且不动磁盘）。
- 前端：`tsc -b` 0；单测 **672/672**（新增 4 条）。
- `listRunnerAgents` 的重构按"逐行等价"核对过（只换了头部、函数体一字未动）；
  那条会构造完整 `Server` 的既有用例在本机跑不了（沙箱拦 `wsl.exe`），已在下方欠账里注明。

## 13.7 复查轮（第二批：换维度 + 一路独立审查）

这一轮**独立子代理审查起来了**（上一轮 502 / 拿不到 `copilot.tencent.com`）。它报了 11 条，
我逐条对着源码核实：**8 条成立**（另有 1 条的"证据"部分不成立，但引出的问题成立），
另外我自己在核实过程中又抓到 **4 条**（其中 2 条审查完全没提）。合计 **12 处，全部修掉**。

### 严重（会让用户看到"没有问题"而工具其实用不了）

| # | 问题 | 为什么是问题 | 修法 |
| --- | --- | --- | --- |
| 1 | **`override-path-dead` 是死代码，真事故被静默吞掉** | `assessResolvedPath` 的 `hasEffective` 取自 `probes` 这个 map，而 map 只在 `fact.Exists` 为真时才写入 ⇒ `probe.Exists` 恒为真、`!probe.Exists` 那一支**永远进不去**。而 resolver 对 override 是**无条件返回**的（`agent_paths.go:66`，不检查存在性也不往下找）：`AUTO_CLAUDE_PATH` 指向一个死文件时命令行完全用不了，诊断却一条症状都不报，最后判成 **`ok`="没有问题"** —— 正是这一整轮要消灭的结论 | `hasEffective` 改从**路径事实表**取（新增 `diagnosePathFactByPath`，用同一个 `sameCleanPath` 规则比对），于是"存在吗"与"实测过吗"是两个独立的事实 |

### 一般

| # | 问题 | 为什么是问题 | 修法 |
| --- | --- | --- | --- |
| 2 | **`preflight` 只覆盖了安装判据的一半** | 真安装是两步：`resolveAgentInstallPlan` + `checkRuntimeGate`。预检只做第一步 ⇒ Node 太旧时说"可以装"、点下去才报"运行时版本过低"，而把这句提前正是预检存在的唯一理由（违反 §4 声称的"同一份判据"） | 预检里接着调**同一个** `checkRuntimeGate`；用例加了正对照（Node 24 ⇒ 说可以装）与"与真实失败逐字相同"两条 |
| 3 | **跨端把本机的读数写成目标机的事实** | `buildCrossAgentDiagnosis` 用本机的 `fileExists` 去判一条**目标环境**上的路径（`/usr/local/bin/claude`）⇒ 表格里写"不存在"，而同一份报告的 Limitations 刚说过"没有在目标环境核对文件" | 路径事实加三态 `checked`/`probed`；跨端只给登记内容 + `checked=false`，界面念"未核对" |
| 4 | **"存在但没实测"被念成"存在但跑不起来"** | 实测预算只有 2 条（`diagnoseProbeBudget`），超出预算的候选 `works` 是零值 ⇒ 表格对它下了一个从没做过的结论（把"没查"写成"坏了"） | 同上：`probed=false` 时界面的说法是"存在，未实测"。Go 侧另加一条 `!fact.Exists`/`!probed` 的显式分支，绝不再顺着 `!probe.Works` 报"执行失败" |
| 5 | **`ok` 徽标与症状数自相矛盾** | warning 级症状（`probe-timeout` / `command-shim-missing`）不产生 blocker，`finalizeDiagnosis` 于是判 `ok` ⇒ 界面同时显示「**没有问题** · 1 项症状」 | 新纯函数 `diagnosisBadge` **同时**看结论与症状：`ok` 且有非 info 症状 ⇒ "能用，有 N 项要留意"（琥珀档），不再念"没有问题" |
| 6 | **中止之后剩下的动作凭空消失** | 某一步失败就 `break`，`plan.Order` 里剩下的动作既不执行、也不进 `applied`、审计里也没有 —— 而计划是**服务端**定的，客户端根本不知道还有哪几步 | 失败时把剩余动作按 `skipped` 追加进结果（"前一步没有成功，这个动作没有执行"）；`describeRepair` 同步说清"另有 N 个没执行（为什么）" |
| 7 | **一次失败的修复写了两条审计**（独立审查漏掉，我核实时抓到） | 循环里那条带错误原文的 + 循环后那条带步骤一览的 ⇒ 同一事件在审计里出现两次，等于说这台机器失败过两回。而审计唯一的职责就是"这台机器上发生过什么" | 合并成**一条**：错误原文由 `repairAuditDetail` 带上（经 `tailUpdateOutput` 脱敏 + 去 ANSI + 截断，与安装失败同待遇） |

### 轻微（会误导，或让记录不干净）

| # | 问题 | 修法 |
| --- | --- | --- |
| 8 | **请求体里的字符串成了记录里的标识** | 表外 id 原先直接当 `repairStep.ID` ⇒ 随响回给界面、也进审计落库（"只当表键用"这句对**执行**成立，对**记录**不成立） | 标识改为常量 `unknown-action`；客户端给的那个字符串经 `clampDiagnoseText`（去 ANSI + 脱敏 + 按字符截断）只出现在**说明**里 |
| 9 | **确认框宣称了一个做不到的执行顺序** | 它写"将按下面的顺序执行"，而列表是"症状里出现动作的次序"，真正执行次序由服务端 `remedyOrder` 定 —— 实测这两个次序**正好相反**（`[reinstall, rebuild-shim, cleanup, restore-backup]` vs `[restore-backup, cleanup, rebuild-shim, reinstall]`） | 改文案（"实际先后由服务端按依赖关系安排"），按钮去掉"按顺序"三个字 |
| 10 | **`describeRepair` 把"跳过"算进"做成了什么"** | 被跳过的动作也带 detail（那是"为什么没执行"），混进结果栏会读成"入口已重建；不是平台支持的修复动作" | `done` 过滤掉 `skipped`；同时把跳过的**理由**念出来 —— 确认框承诺过"并在结果里说明为什么"，而原先前端根本不渲染它 |
| 11 | **`resolvedNotes` 永不清除** | 那条"上次修复已解决 N 项"挂上去就不再下来，后续变更把它推翻后它还在说 | 一发起新的动作就先清掉该 key（它只对**那一次**修复有效） |
| 12 | **刷新会跑两遍完整批量诊断**（独立审查漏掉） | `refresh()` 显式调一次 + `viewState` 回到 ready 时 effect 又一次 ⇒ 每遍都要拉起子进程探测，而两遍结论必然一样 | 触发点收成**一处**（effect），`refresh` 只负责重载列表 |

### 顺带：一处重复的探测（同族，一起修）

批量诊断会为**每个**工具跑一次完整诊断（goroutine 并发），而运行时状态与工具无关 ——
每个工具各探一遍就是 N 份同样的 `node --version` / `npm --version` 子进程，外加 N 次并发的
Node 版本索引下载（`runtimeManager` 只有 TTL 缓存、**没有 single-flight**，冷缓存时那几个
goroutine 会各下一份）。改为算一次共用（`diagnoseShared` + `sync.Once` 惰性求值：
一个工具都不需要详查时一次都不探）。

### 一处**刻意不改**，只补注释

`before/state` 这份诊断是在 `beginAgentMaintenance` **之前**做的，闸门之后用的还是它 ——
看着像 TOCTOU，其实**顺序不能换**：`beginAgentMaintenance` 会置位 `runnerUpdating[runnerID,agentID]`，
而 `agentMaintenanceActive` 读的正是那一位 ⇒ 进了闸门再诊断，诊断只会得到"正在安装或升级"，
任何修复都跑不起来。真正的兜底在各动作自己：每个 `Apply` 都在执行时重新核对它依赖的事实
（没有备份就报"没有找到可回滚的完整备份"），而不是信这份快照。已把理由写进代码注释。

### 独立审查里**没采纳**的一条

"`issuesWithRemedies` 用与生产同一张表过滤再断言只剩白名单，属自证" —— 它当作**夹具**
是合理的（形状与真实报告一致），但它引出的缺口是真的：**没有任何用例**钉住
`agentDiagnosis.add()` 会过滤掉白名单外的 id。已补 `TestDiagnosisAddDropsUnknownRemedyIDs`
（其中特意喂了 `reset-record` 与一个带 `=` 的伪造串）。

### 复查后验证

- Go：`vet` 干净；定点 **49** 条用例全绿（新增 9 条：死覆盖回归、未实测不算坏、跨端未核对、
  `add` 过滤白名单、预检含运行时闸门、中止后剩余动作可见、失败只写一条审计、
  请求体不进审计、运行时只探一次）。
- **变异检验（本轮的欠账补上一次）**：把批量端点里的"共用运行时读数"改回每工具各探一次
  ⇒ `TestListRunnerDiagnosticsProbesRuntimeOnce` **变红**（已还原并复跑全绿）。
- 前端：`tsc -b` 0；`vite build` 0（3.27s）；单测 **675/675**（新增 4 条，含
  `diagnosisBadge` 的"ok 但有 warning 不许念成没有问题"）。
- 未做：真实浏览器探针、**其余**防线的变异检验（见 §14）。
- `internal/app` 全量：1598s，**11 条红，判为环境性**（**推断，不是证明**）。与上一次全量的
  10 条相比是**双向差异**：`TestConversationProfile…` 这次过了；`TestInsightPublishFailsWhenWorkspaceBusy`
  两次都红（预先存在）；另两条**超时型**（`TestVerifyInsightFindingsAllowedWhileScanRunning` 14.8s、
  `TestInsightVerifyAdvancesProgressAcrossBatches` 23.2s，"did not finish in time"）。判据：
  报错都是它们**自己设的截止时间**到点，且 `grep` 我改动过的全部符号在 `insights*.go` 里 **0 命中**。
  单跑这两条会被沙箱硬拦（`wsl.exe`），所以**只能推断**——完整清单与判据已补进 `TOOLING.md`。

## 13.8 第三次复查（两路独立审查 + 我自己核实）

这一轮派了**两路独立审查**（一路后端、一路前端，互不知情）。两路**各自独立**指出了同一条 P0
——交叉验证，不是同一份意见被念了两遍。它们再各报若干条；我逐条对着源码核实，成立的全部修掉，
**一处论据不成立**的（"夹具自证"这类）只采纳它引出的缺口。

### 严重：一份假报告被当成"修好了"，而且让断言**静默变绿**

| 问题 | 为什么两轮复查都没抓到 |
| --- | --- |
| **响应里那份"修复后重跑的诊断"永远是一份"维护中"的空报告** | 它在第二次复查里被我**自己**引入：那次修了"安装进行中不许探测"（§13.6 #1），而 `repair` 拿闸门的顺序是 `beginAgentMaintenance` → … → 重跑诊断 → `defer release()`。诊断一见维护位就早退成 `unknown` + `maintenance-active`、**一条探测都不跑** ⇒ 报告零症状。后果链：① 前端拿它算差集，把**所有**旧症状判成"已解决"（**修复失败也照说不误**）；② `requireNoIssue(result.Diagnosis, …)` 这类断言变成**恒真** —— 它让断言变**绿**而不是变红，而"绿"从来不引人注意 |
| **跨端的"没查成"被渲染成"没有发现任何症状"** | `buildCrossAgentDiagnosis` 的 default 分支会产出 `status=unknown` + **零症状**，而面板的空态只看 `issues.length === 0` ⇒ 徽标写"检测未完成"、点开却写"没有发现任何症状" |

修法（四处，缺一不可）：

1. **重跑诊断之前先放掉闸门**（`sync.Once` 做幂等释放，`defer` 仍然兜住 panic）。
   代价是一扇很小的窗：极端情况下另一个操作刚好插进来，报告又会是"维护中" ——
   所以必须配合第 2 条。
2. **前端 `resolvedIssues` 加一道闸**：`after` 不是一次**有结论**的检测（`unknown` 档）时
   返回空数组 —— 差集什么都不声称。这是"把'没查'写成'已解决'"的直接修补。
3. **空态看结论**：`diagnosisEmptyText` 在没结论时说"这次没有得出结论"，不说"没有发现任何症状"。
4. **把那条假绿的断言钉死**：新增 `requireConclusiveDiagnosis`，凡读 `result.Diagnosis` 的用例
   都必须先通过它。**先让它在旧代码上红过一次**（我确实先跑红再修的），否则它还是装饰。

### 一般

| # | 问题 | 修法 |
| --- | --- | --- |
| 1 | **npm-system 的工具被托管运行时的读数判死** | `assessRuntime` 用 `probeRuntime`（**托管优先**）判"版本过低/找不到 Node"，而这类工具的安装闸门用的是与系统 npm 同处一地的 node（`plan.RuntimeVersion`）。一台"托管运行时坏掉/太老、系统 node 正常"的机器上，**能正常跑**的工具会被判成"用不了"（假 blocker），而给出的动作（装托管运行时）根本换不掉它实际用的那个 node。改为按登记方式分流：系统那档走 `assessSystemNpmRuntime`（复用 installAgentCLI 用的同一个 `nodeVersionNearNpm`），并且**刻意不给修复动作**（平台修不了系统那一套） |
| 2 | **探测被取消被说成"执行失败"** | 父上下文取消（请求断了 / 操作结束）时 `probeCtx.Err()` 是 `Canceled`，原先只区分 `DeadlineExceeded` ⇒ 落到 `!probe.Works` ⇒ 假 `binary-broken` blocker。加 `pathProbe.Canceled` 与一条显式分支（如实说"实测被中断"） |
| 3 | **`fileExists` 把"读不到"并进"不存在"** | 生效路径 Stat 权限失败时会报成"配置里指定的可执行文件路径不存在"（blocker）。路径事实改用 `diagnosePathExistence` 返回 `(checked, exists)`；`!checked` 时如实说"读不到"，`Checked=false` 让界面念"未核对" |
| 4 | **相对路径的 override 仍被静默吞掉** | `diagnosePathCandidates` 原先只收 `filepath.IsAbs(effective)` 的生效路径 ⇒ `AUTO_*_PATH=.\bin\x.exe` 这种既不在表里、也永远不会被核对。判据改成"只要不是 resolver 兜底返回的裸命令名就收" |
| 5 | **`RepairResult.success` 没人读、成败判据两处各写一份** | 前端原本自己用 `describeRepair(...).ok` 重算一遍。改为 `describeRepair` 只出文案、成败直接消费服务端的 `success`；用例加断言"页面必须读 result.success" |

### 轻微（都属"算了没人读"与约定）

- **渲染路径补全**：`diagnosis.version`（实测版本）、`diagnosis.diagnosedAt`（**报告是快照，
  不写时间等于把旧读数当成此刻的事实**）、`lastFailure.createdAt`（否则"上周那次"与"刚才那次"
  分不出）、`preflight.upgradeOk/UpgradeReason`（`preflightNotes` 在它与 install 分歧时才补一行）、
  `RunnerDiagnosticsView.probeOk/probeError`（**通道坏掉时全部工具都是 `skipped`，
  而 `skipped` 有两种成因** —— 原先一律说"这个工具当前没有可疑迹象"，那是谎报）、
  `RunnerDiagnosticsView.runnerId`（用来校验响应归属）。
- **变体类名走 `data-*`**：删掉两个动态拼的类名（`cli-tools-diagnosis-${tone}` 是**死类名** ——
  CSS 只认 `[data-tone]`；`cli-tools-severity-${severity}` 换成 `data-severity`），CSS 里那两条
  旧选择器一并删掉（留着就是同一件事两份定义）。
- **两处注释说错**（比"没注释"更危险，因为我会信它）：① 预检"只读文件系统与 PATH、**一条命令
  都不执行**" —— 实际它为了拿运行时版本会跑 `node --version`；② `diagnosisWithRepairContext`
  自称"同源"，其实登记项被读了两次（相隔微秒）。都改成准确的措辞。
- **一处按设计的"不修"**：用户显式点过「检测这个工具」的报告，会被随后的批量诊断按
  `skipped` 覆盖掉（批量是权威）。不修 —— 那一档界面照实说"本轮没有详查"，
  而保留一份旧报告会变成"把旧读数当此刻的事实"。

### 验证

- Go：`vet` 干净；定点 **64 条用例全绿**（新增 4 条：取消分类、取消读数、读不到 ≠ 不存在、
  npm-system 分流；另加一个断言助手 `requireConclusiveDiagnosis` 并接入读回填诊断的用例）。
- **变异检验 3 处**（都先红再还原）：
  ① 批量端点改回"每工具各探一次运行时" ⇒ `TestListRunnerDiagnosticsProbesRuntimeOnce` 红；
  ② `probe.Canceled` 写死 false ⇒ **读数用例**红、**分类用例仍绿** ——
  正好实证了"分类对"与"读数对"是两件事，缺一条防线就漏一半（这两条因此成对存在）；
  ③ 关掉 npm-system 分流 ⇒ `TestDiagnoseJudgesSystemNpmRuntimeFromSystemNode` 红。
- 前端：`tsc -b` 0；`vite build` 0（2.52s）；单测 **681/681**（净增 6 条：差集挡"没查成"、
  空态看结论、skipped 两种成因、预检分歧才补第二句、元信息行/时间格式化，以及 3 处源码级断言）。



