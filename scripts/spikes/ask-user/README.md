# 会话内提问 · 三个最小 spike

这三个脚本只回答三个问题，不产出任何生产代码。每条结论都必须能在
`.tmp/spikes/ask-user/<name>-<时间戳>/` 里翻到原始事件流对照。

| # | 问题 | 通过意味着 | 失败意味着 |
|---|---|---|---|
| 1 | Claude 的 `AskUserQuestion` 能否用 PreToolUse hook 的 `updatedInput` 作答 | 原生提问通道可用（仍要按会话探测，见下） | 原生通道不存在，只能走 MCP 兜底 |
| 2 | CodeBuddy / Qoder 的 `--settings` hooks 是否与 Claude 同构 | 一次改动覆盖两个 CLI | 放弃原生，同走 MCP 兜底 |
| 3 | HTTP 形态的 MCP 各 CLI 通不通、一次调用能挂多久 | 兜底通道可行，且知道阻塞上限 | 需要换 stdio helper 或缩短等待 |
| 4 | MCP 工具在**生产权限模式**下能否放行 | 单通道（只做 MCP）成立 | 只能回到原生 hook，或给受限模式加放行规则 |

## 跑法

```bash
node scripts/spikes/ask-user/spike1-claude-ask.mjs
node scripts/spikes/ask-user/spike2-family-hooks.mjs
node scripts/spikes/ask-user/spike3-http-mcp.mjs --sleeps=5,90 --clis=claude,codex
node scripts/spikes/ask-user/spike4-permission.mjs --clis=codebuddy
node scripts/spikes/ask-user/spike4-permission.mjs --clis=qoder
```

退出码：`0` 通过、`1` 失败、`2` 无法判定（换环境重跑，别当成通过）。

## 三个判定口径里最容易读错的两处

**Spike 1 的 `INCONCLUSIVE` 不是"失败"。** 它表示这台机器的账号/模型没有把
`AskUserQuestion` 放进 init 帧的工具表 —— 2026-10-28 实测本机 Claude 的 init 工具表是
`Task, CronCreate, ..., PowerShell, Write`，没有提问工具；CodeBuddy 的 init 表里有，
但模型侧同样拿不到。这条通道的可用性取决于**模型/供应商**，不是取决于 CLI，
所以生产实现必须按会话探测，不能写死在目录里。

**Spike 2 的段 A 才是关键。** 段 A 用"把 `echo ORIGINAL_VALUE` 改写成
`echo SPIKE_INJECTED_OK`"来证明 hook 触发且 `updatedInput` 被采纳 —— 它不依赖提问
工具是否暴露，所以即使段 B 无法判定，只要有段 A 的 PASS，就说明这个 CLI 的 hook
通道可用，原生提问通道只差"工具在不在"。

## 参数

- Spike 3 的 `--sleeps`：默认 `5,90`。短档只验连通，长档逼出 CLI 自己的工具调用
  超时。长档失败时记下 `durationMs`，那就是默认超时的下界。
- Spike 3 的 `--clis`：本机没装 `opencode` / `cursor-agent` 时会被判成
  `INCONCLUSIVE` 并跳过，不算失败。

## 每个目录里有什么

| 文件 | 内容 |
|---|---|
| `*.jsonl` | CLI 的原始事件流，判结论的唯一依据 |
| `hooks-*/` 或 `hook-*.json` | hook 收到的 stdin 原文，用来对比各 CLI 的 schema 差异 |
| `verdict*.json` | 本次结论 + 关键片段，便于贴进方案文档 |
| `mcp-server.log` | 假 MCP server 收到的请求（含是否带 Authorization） |

## 这些结论怎么进方案

- Spike 1 PASS 且 Spike 3 PASS → 原生优先、MCP 兜底，两套都做。
- Spike 1 FAIL 或长期无法判定 → **只做 MCP**，原生留成"探测到就顺带用"的可选项。
- Spike 2 段 A FAIL → CodeBuddy/Qoder 与 Claude 不同构，不要按"同族"去做，统一走 MCP。
- Spike 3 长档失败 → 阻塞时长必须以该数值为硬上限，超过就要考虑"暂停本轮"而不是硬等。

## 实测记录（2026-10-28，本机 Windows 11 / Node 22）

| 用例 | 结果 | 证据要点 |
|---|---|---|
| Spike 1 · claude | INCONCLUSIVE | init 工具表 26 个，无 `AskUserQuestion`；模型明确回复"这个会话里没有该工具"。换 Anthropic 直连账号才能判 |
| Spike 2 · codebuddy 段 A | **PASS** | hook 收到 `hook_event_name=PreToolUse, tool_name=Bash`，命令被改写成 `echo SPIKE_INJECTED_OK` 并执行 |
| Spike 2 · codebuddy 段 B | INCONCLUSIVE | init 表里有 `AskUserQuestion`，模型没调用 |
| Spike 2 · qoder 段 A | **PASS** | 同上；payload schema 与 claude 逐字段一致 |
| Spike 2 · qoder 段 B | INCONCLUSIVE | init 表里没有 `AskUserQuestion` |
| Spike 3 · claude 5s / 90s | **PASS / PASS** | HTTP MCP 连通；**90 秒阻塞调用没被掐断**，说明默认工具超时 > 90s |
| Spike 3 · codebuddy 5s | **PASS** | 需 `--permission-mode bypassPermissions`；否则 `DeferExecuteTool` 那层要审批 → 表现为"通了但被拒" |
| Spike 3 · qoder 5s | **PASS** | 同样需要 `--permission-mode bypass_permissions`，否则返回 `Error: Allow MCP tool spike/sleep?` |
| Spike 3 · codex 5s | INCONCLUSIVE | codex 本机连普通回合都跑不完（卡在 `turn.started`，被 spike 上限终止），与 MCP 无关 |

### Spike 4 · 权限矩阵（同一天补跑）

| 模式（均为生产里真实会出现的） | codebuddy | qoder |
|---|---|---|
| 生产 workspace_write / full_control | **PASS**（`--dangerously-skip-permissions`） | **PASS**（`--permission-mode accept_edits`） |
| 只读模式 | **FAIL**（`--permission-mode plan`） | **FAIL**（`--permission-mode default`） |
| 受限模式 + 放行规则 | `permissions.allow:["mcp__spike__sleep"]` 无效；加上 `DeferExecuteTool` 才 PASS | `--allowed-tools mcp__spike__sleep` 有效 |

结论：**写入/完全控制两档本来就已经放行 MCP 调用**，单通道方案在这两个 CLI 上零改动。
只有只读档会拦，而且两个 CLI 的解法不同（qoder 可以精确到工具名，codebuddy 必须把
`DeferExecuteTool` 一起放开 —— 那等于放开所有延迟工具，不建议为"提问"在只读会话里这么做）。

### 这轮实测改变了什么

1. **CodeBuddy 与 Qoder 的 hook 与 Claude 逐字段同构**（段 A 双 PASS）。原方案里"待验证"的那一格已经落地：
   这条链路上三个 CLI 可以共用一份 hook 适配，不需要各写一套。
2. **原生提问通道的瓶颈不是 CLI，而是模型/供应商**：同一台机器上 claude 和 qoder 的工具表里
   没有 `AskUserQuestion`，codebuddy 有但模型不用。所以"按会话 init 帧探测"是必须的，
   而 MCP 兜底也不能省。
3. **HTTP MCP 可行，且三种 CLI 都能连**；真正的坑在**权限面**：codebuddy/qoder 在无头模式下
   会拦 MCP 工具调用。生产实现里"MCP 兜底"必须同时解决权限放行，否则提问工具永远调不通。
4. **90 秒阻塞已验证可用**。更长的等待仍需显式配 `MCP_TOOL_TIMEOUT`（claude）/ `tool_timeout_sec`（codex），
   这两项本机未验证。
5. **MCP 通道不需要为生产权限模式做任何改动**：codebuddy 的 workspace_write/full_control 本来就带
   `--dangerously-skip-permissions`，qoder 的 accept_edits 也放行 MCP。原方案里"必须连带解决权限放行"
   这条风险，在主路径上不成立；只有只读档需要单独决策。

### 还没结论的两件事

- claude 的 `updatedInput` 对 `AskUserQuestion` 到底生不生效（Spike 1 INCONCLUSIVE）。
- codex 的 MCP 通道（本机 codex 自身不可用）。
