# 修复对话页面上下文计算错误

> **状态：已废弃。** 后续真实 Claude CLI 事件验证表明，`result.usage` 是 Run 累计量，不能作为最后一次调用的上下文快照；以 `docs/05-Claude对话上下文与使用状态.md` 和当前实现为准。

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 `contextInputTokens` 在流式模式下始终为 0、且回退逻辑使用错误的累计总输入 token 数导致上下文百分比显示荒谬的问题。

**Architecture:** 问题根因有三层：(1) Claude CLI 流式模式下 assistant 事件的 `usage.input_tokens` 始终为 0；(2) 后端 `normalizeContext` 用累计总 `input_tokens` 回退填补 `contextInputTokens`；(3) 前端 `contextLabel` 同样回退到累计 `inputTokens`。修复方案：移除有误导性的回退逻辑，在前端和后端当 `contextInputTokens` 不可用时明确告知用户，而不是显示基于错误数据的百分比。

**Tech Stack:** Go (control-server), TypeScript/React (web frontend), SQLite

## Global Constraints

- 不能改变 Claude CLI 的行为（这是外部依赖）
- 必须保持 API 向后兼容（不改变 JSON 字段结构）
- 前端显示在中文环境下运行

---

### Task 1: 后端 — 修正 `result` 事件中 `contextInput` 的赋值语义

**Files:**
- Modify: `apps/control-server/internal/app/usage.go:226-231`

**Interfaces:**
- Consumes: `event.Usage.InputTokens` (result 事件中的单次调用输入 token 数)
- Produces: `accumulator.contextInput` 设置为 result 事件中本次调用的输入 token

- [ ] **Step 1: 修改 `collect` 方法中 result 事件处理，用正确的语义设置 contextInput**

在 `usage.go` 第 226-231 行，当前代码是：
```go
// When streaming, the per-assistant usage.input_tokens is always 0.
// Use the result event's total input_tokens as the context snapshot
// so the UI can show a meaningful context utilisation percentage.
if accumulator.contextInput == 0 && event.Usage.InputTokens > 0 {
    accumulator.contextInput = event.Usage.InputTokens
}
```

数据库验证确认了：流式模式下所有 assistant 事件的 `usage.input_tokens` 为 0，且 `result.usage.input_tokens` 是**本次调用的输入 token 数**（不是累计总输入，那在 `modelUsage` 中）。这个回退逻辑实际上**语义是对的**——`event.Usage.InputTokens` 就是本次调用的输入 token 数。但是：

1. 注释说有误导性（说 "total input_tokens" 其实是本次调用而非累计）
2. 条件 `accumulator.contextInput == 0` 不够精确——在非流式模式如果有非零 assistant usage，这个回退不会触发（正确），但在 agent 多轮调用中，如果前面有 assistant usage 传了值，最后一轮的 result 不会更新 contextInput（实际应该用最后一轮的值）

修改为：result 事件中**始终**用 `event.Usage.InputTokens` 更新 `contextInput`（作为最后一轮 API 调用的输入 token 数）：

```go
// The result event carries the last API call's input token count in
// usage.input_tokens.  This is the best available approximation of the
// current context window utilisation.  Per-assistant usage.input_tokens
// is always zero during streaming, so we refresh the snapshot here on
// every result event.
if event.Usage.InputTokens > 0 {
    accumulator.contextInput = event.Usage.InputTokens
}
```

注意：删除 `accumulator.contextInput == 0 &&` 条件，让 result 始终更新 contextInput。

- [ ] **Step 2: 运行现有测试验证修改没有破坏行为**

```bash
cd apps/control-server && go test ./internal/app/ -run TestRunUsage -v -count=1
```

预期：两个 usage 测试通过。第一个测试 `TestRunUsageDeduplicatesEventsAndPersistsConversationSummary` 中 assistant 事件传了 `input_tokens:48000`，result 事件传了 `input_tokens:50000`，修改后 contextInput 会变成 50000（之前是 48000）。需要更新测试断言。

第二个测试 `TestSendMessagePersistsUsageFromRunnerLifecycle` 两者都是 1200，不变。

- [ ] **Step 3: 更新测试断言以匹配新行为**

在 `app_test.go:269`，将：
```go
if usage.Context.ContextInputTokens != 48000 || ...
```
改为：
```go
if usage.Context.ContextInputTokens != 50000 || ...
```

并且验证第 307 行的测试仍然通过（1200 = 1200）。

- [ ] **Step 4: 再次运行测试确认**

```bash
cd apps/control-server && go test ./internal/app/ -run TestRunUsage -v -count=1
```

预期：全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add apps/control-server/internal/app/usage.go apps/control-server/internal/app/app_test.go
git commit -m "fix: 修正 result 事件中 contextInput 的赋值语义

result 事件中 usage.input_tokens 是最后一次 API 调用的输入 token 数，
应该始终用它更新 contextInput，而不是仅在没有 assistant usage 时才回退。
多轮 agent 调用中，最后一轮的 result 才是当前上下文的最新快照。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: 后端 — 移除 `normalizeContext` 中错误的累计输入回退

**Files:**
- Modify: `apps/control-server/internal/app/usage.go:394-398`

**Interfaces:**
- Consumes: `RunUsage.ContextInputTokens`, `RunUsage.InputTokens`
- Produces: `normalizeContext` 不再用累计 `InputTokens` 回退填补 `ContextInputTokens`

- [ ] **Step 1: 修改 `normalizeContext` 函数**

当前代码（第 394-398 行）：
```go
// normalizeContext fills in context_input_tokens from total input_tokens when
// streaming assistant events don't carry per-message usage data (always 0).
func normalizeContext(usage *RunUsage) {
	if usage.ContextInputTokens == 0 && usage.InputTokens > 0 {
		usage.ContextInputTokens = usage.InputTokens
	}
}
```

`InputTokens` 是整个 run 所有轮次的累计总输入。用它除以 context window 完全没意义。

修改为：移除回退逻辑，当 `ContextInputTokens` 为 0 时保持为 0，让前端显示"上下文计算中"：

```go
// normalizeContext ensures the context snapshot is consistent.
// When context_input_tokens is still zero (no result event has arrived yet),
// leave it as-is so the UI can show an appropriate pending state instead of
// fabricating a number from the cumulative total input tokens.
func normalizeContext(usage *RunUsage) {
	// Intentionally empty: keep contextInputTokens as the raw value from the
	// most recent assistant or result event.  Do NOT fall back to the
	// cumulative InputTokens, which is the sum across all turns and does not
	// represent the current context window utilisation.
	_ = usage
}
```

- [ ] **Step 2: 运行测试确认**

```bash
cd apps/control-server && go test ./internal/app/ -run TestRunUsage -v -count=1
```

预期：全部 PASS（因为之前的修改已经确保 contextInput 从 result 事件中获取正确值）。

- [ ] **Step 3: 提交**

```bash
git add apps/control-server/internal/app/usage.go
git commit -m "fix: 移除 normalizeContext 中错误的累计输入回退

InputTokens 是全部轮次的累计总输入，用它作为 contextInputTokens
的回退值毫无意义。当没有可用的上下文快照时，应保持为 0 让前端显示
合适的等待状态。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: 前端 — 修正 `contextLabel` 和 `contextLevel` 的回退逻辑

**Files:**
- Modify: `apps/web/src/App.tsx:125-137`

**Interfaces:**
- Consumes: `RunUsage.contextInputTokens`, `RunUsage.contextWindow`
- Produces: `contextLabel()` 和 `contextLevel()` 不再回退到累计 `inputTokens`

- [ ] **Step 1: 修改 `contextLabel` 和 `contextLevel` 函数**

当前代码（第 125-137 行）：
```typescript
function contextLabel(context: RunUsage | undefined): string {
  const tokens = context?.contextInputTokens || context?.inputTokens;
  if (!tokens) return "上下文计算中";
  if (!context?.contextWindow) return "上下文暂不可用";
  return `上下文约 ${Math.min(100, Math.round(tokens / context.contextWindow * 100))}%`;
}

function contextLevel(context: RunUsage | undefined): string {
  const tokens = context?.contextInputTokens || context?.inputTokens;
  if (!tokens || !context?.contextWindow) return "pending";
  const percent = tokens / context.contextWindow * 100;
  return percent >= 85 ? "danger" : percent >= 70 ? "warning" : "normal";
}
```

修改为：只用 `contextInputTokens`，不 fallback 到 `inputTokens`，并增加 token 数量显示：

```typescript
function contextLabel(context: RunUsage | undefined): string {
  const tokens = context?.contextInputTokens;
  if (!tokens) return "上下文计算中";
  if (!context?.contextWindow) return "上下文暂不可用";
  const percent = Math.round(tokens / context.contextWindow * 100);
  const tokensDisplay = formatTokens(tokens);
  const windowDisplay = formatTokens(context.contextWindow);
  return `上下文 ${tokensDisplay} / ${windowDisplay} (${percent}%)`;
}

function contextLevel(context: RunUsage | undefined): string {
  const tokens = context?.contextInputTokens;
  if (!tokens || !context?.contextWindow) return "pending";
  const percent = tokens / context.contextWindow * 100;
  return percent >= 85 ? "danger" : percent >= 70 ? "warning" : "normal";
}
```

- [ ] **Step 2: 运行前端测试确认**

```bash
cd apps/web && npx vitest run
```

- [ ] **Step 3: 提交**

```bash
git add apps/web/src/App.tsx
git commit -m "fix: 前端上下文显示不再回退到累计输入，增加 token 数量

移除 contextLabel/contextLevel 中 contextInputTokens || inputTokens
的回退逻辑。inputTokens 是累计总输入，用它计算上下文百分比无意义。
同时显示具体 token 数量（如 45K / 200K (23%)）。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: 前端 — 更新 UsageDialog 中的上下文显示

**Files:**
- Modify: `apps/web/src/App.tsx:1182-1183`

**Interfaces:**
- Consumes: `usage?.context.contextInputTokens`, `usage?.context.contextWindow`
- Produces: UsageDialog 中的上下文详情行

- [ ] **Step 1: 修改 UsageDialog 中的上下文详情行**

当前代码（UsageDialog 组件内，约第 1182-1183 行）：
```typescript
{usage?.context.contextInputTokens && usage.context.contextWindow > 0 && <p className="context-detail">{formatTokens(usage.context.contextInputTokens)} / {formatTokens(usage.context.contextWindow)}</p>}
```

这个条件 `usage?.context.contextInputTokens &&` 在值为 0 时会短路（falsy），导致不显示。但实际上我们已经有了 token 详情，应该始终显示（只要 contextWindow > 0）。

修改为：
```typescript
{usage?.context.contextWindow > 0 && <p className="context-detail">{usage.context.contextInputTokens ? `${formatTokens(usage.context.contextInputTokens)} / ${formatTokens(usage.context.contextWindow)}` : "等待上下文快照"}</p>}
```

- [ ] **Step 2: 运行前端测试确认**

```bash
cd apps/web && npx vitest run
```

- [ ] **Step 3: 提交**

```bash
git add apps/web/src/App.tsx
git commit -m "fix: UsageDialog 上下文详情在没有快照时显示等待状态

当 contextInputTokens 为 0 时（尚未收到 result 事件），显示
"等待上下文快照" 而不是完全不显示该行。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Self-Review

### 1. Spec coverage

| 问题 | 对应任务 |
|------|----------|
| 问题 1: result 事件中 contextInput 赋值条件过严格 | Task 1 |
| 问题 2: normalizeContext 用累计总输入回退 | Task 2 |
| 问题 3: 前端 contextLabel/contextLevel fallback 到 inputTokens | Task 3 |
| 问题 5: 上下文显示没有具体 token 数量 | Task 3, Task 4 |

### 2. Placeholder scan

无 TBD/TODO/占位符。所有步骤包含具体代码和命令。

### 3. Type consistency

- `RunUsage.contextInputTokens` (number) — 前端和后端类型名称一致
- `formatTokens()` 已在前端定义（第 99-103 行），Task 3/4 使用它
- `contextLabel()` 返回值从 `string` 变为 `string`，不影响调用方
