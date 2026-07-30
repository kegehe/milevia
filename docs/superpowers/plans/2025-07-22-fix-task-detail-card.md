# 任务详情卡片样式修复 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 TaskDetailDialog 和 InlineTaskDetail 的 6 个已确认样式/布局问题

**Architecture:** 纯前端 CSS + JSX 改动，涉及 3 个文件：tasks.css（样式修复）、TaskBoard.tsx（弹窗详情结构调整）、TaskQueue.tsx（内联详情结构调整）。不改变组件接口和行为逻辑。

**Tech Stack:** React 18 + TypeScript + CSS

## Global Constraints

- 不改变现有组件的 props 接口和行为逻辑
- 不改变全局样式文件 style.css
- 保持与现有设计系统（配色、圆角、阴影风格）一致
- 不引入新的依赖
- 内联 CSS 压缩风格（与现有 tasks.css 一致）

---

### Task 1: 弹窗详情 footer 吸底 + body 独立滚动 + review-sheet 改为流式布局

**问题:** 弹窗内容多时 footer 被滚出视野；task-review-sheet 绝对定位遮挡 footer。

**修复方案:**
- `.task-detail-dialog` 改为 flex column 布局，overflow: hidden
- body 区域加 `overflow-y: auto` 独立滚动
- header 和 footer 保持可见
- `.task-review-sheet` 从绝对定位改为 body 内的流式块元素，放在执行记录 section 下方

**Files:**
- Modify: `apps/web/src/tasks.css:15-16`
- Modify: `apps/web/src/features/tasks/TaskBoard.tsx:397`

**Interfaces:**
- Consumes: 现有 `.task-detail-dialog`、`.task-detail-body`、`.task-review-sheet` CSS 类
- Produces: 修改后的 CSS 规则 + review-sheet DOM 位置变更

- [ ] **Step 1: 修改 TaskDetailDialog JSX — 将 review-sheet 移入 body 内部**

在 `TaskBoard.tsx` 第 397 行，将 `task-review-sheet` 从 `</footer>` 之后移到 `</div>` (task-detail-body 闭合) 之前，紧跟执行记录 section 之后。

原代码（第397行，压缩在一行内）：
```jsx
return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-detail-title"><section className="modal task-dialog task-detail-dialog"><header>...</header><div className="task-detail-body">...执行记录section...</div><footer className="task-detail-actions">...</footer>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}>...</div>}</section></div>;
```

需要将 `{reviewAction && <div className="task-review-sheet" ...>...</div>}` 移到 `</div>` (task-detail-body 闭合标签) 之前，即在执行记录 `</section>` 之后、`</div>` 之前。

使用 Edit 工具做精确替换。找到 `</section></div><footer className="task-detail-actions">`，替换为 `</section>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}><h3>要求修改</h3><textarea autoFocus required disabled={reviewSubmitting} value={note} onChange={(event) => setNote(event.target.value)} placeholder="说明需要补充或修改的内容" /><div><button className="secondary" disabled={reviewSubmitting} onClick={() => setReviewAction(null)}>返回</button><button className="primary" disabled={reviewSubmitting} onClick={() => void submitReview("request_changes")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></div></div>}</div><footer className="task-detail-actions">`。

同时删除原来 footer 后面的 `{reviewAction && ...}` 片段。

- [ ] **Step 2: 修改 CSS — 弹窗改为 flex column + body 独立滚动**

修改 `tasks.css` 第 15-16 行的 `.task-dialog` 和 `.task-detail-dialog` 规则：

```css
/* 第15行：.task-dialog 保持 overflow: auto 用于 TaskEditor（表单弹窗），但给 task-detail-dialog 覆盖 */
.task-dialog { width: min(720px, calc(100vw - 32px)); max-height: min(800px, calc(100vh - 32px)); overflow: auto; }
/* 第16行 .task-detail-dialog 增加 flex column + overflow 控制 */
.task-detail-dialog { position: relative; display: flex; flex-direction: column; overflow: hidden; }
```

在 tasks.css 中追加 `.task-detail-body-scroll` 规则（或直接修改 `.task-detail-body` 在 task-detail-dialog 下的行为）：

在现有 `.task-detail-body` 规则后追加针对 detail dialog 的覆盖：
```css
.task-detail-dialog .task-detail-body { overflow-y: auto; flex: 1; margin-top: 16px; padding-right: 2px; }
```

- [ ] **Step 3: 修改 CSS — task-review-sheet 从绝对定位改为流式块元素**

修改 `tasks.css` 第 16 行中的 `.task-review-sheet` 规则，去掉绝对定位：
```css
.task-review-sheet { display: grid; gap: 12px; margin-top: 14px; border: 1px solid #bfcfc5; border-radius: 7px; padding: 15px; background: #fbfefb; box-shadow: 5px 5px 0 #d5e4dc; }
```

删除 `position: absolute; right: 16px; bottom: 16px; left: 16px;`，改为 `margin-top: 14px;`。

- [ ] **Step 4: 验证构建**

```bash
cd /home/tangmaoke/projects/milevia/apps/web && npx tsc --noEmit 2>&1 | head -20
```

预期：无类型错误。

- [ ] **Step 5: Commit**

```bash
git add apps/web/src/tasks.css apps/web/src/features/tasks/TaskBoard.tsx
git commit -m "fix: 弹窗详情footer吸底 + review-sheet流式布局，避免遮挡操作按钮"
```

---

### Task 2: 内联卡片操作按钮分组 + 验收 textarea 视觉优化

**问题:** 操作按钮平铺无分组；验收 textarea 在 flex 容器中 `width: 100%` 撑满导致视觉断裂。

**修复方案:**
- 在 actions 区域用分隔线将主操作与辅助操作（删除、查看详情）分开
- 验收区域给 textarea 加左侧边框强调色，增强与上方按钮的视觉关联

**Files:**
- Modify: `apps/web/src/features/tasks/TaskQueue.tsx:418-434`
- Modify: `apps/web/src/tasks.css:96-107`

**Interfaces:**
- Consumes: 现有 `.task-inline-detail-actions`、`.task-inline-review-reject` CSS 类
- Produces: 新增 `.task-inline-detail-actions .action-group` 和 `.task-inline-review-reject` 样式优化

- [ ] **Step 1: 修改 JSX — 在操作按钮中插入分组结构**

修改 `TaskQueue.tsx` 第 418-434 行的 `task-inline-detail-actions` div。在「删除任务」按钮前插入一个分隔元素，将主操作和辅助操作分开。

原代码结构（第418-434行）：
```jsx
<div className="task-inline-detail-actions">
  {(detail.status === "todo" || detail.status === "action_required") && <>
    {detail.canDispatch && <button className="primary" ...>下发任务</button>}
    <button className="danger-text" ...>取消任务</button>
  </>}
  {detail.status === "running" && <button className="danger-text" ...>停止任务</button>}
  {detail.status === "awaiting_review" && <>
    <button className="secondary" ...>确认完成</button>
    <div className="task-inline-review-reject">...</div>
  </>}
  {(detail.status === "done" || detail.status === "cancelled") && <button className="secondary" ...>重新打开</button>}
  <button className="danger-text" ...>删除任务</button>
  <button className="secondary" ...>查看完整详情</button>
</div>
```

修改为在「删除任务」和「查看完整详情」前插入分隔线 `<span className="task-inline-actions-sep" />`：

```jsx
<div className="task-inline-detail-actions">
  {(detail.status === "todo" || detail.status === "action_required") && <>
    {detail.canDispatch && <button className="primary" ...>下发任务</button>}
    <button className="danger-text" ...>取消任务</button>
  </>}
  {detail.status === "running" && <button className="danger-text" ...>停止任务</button>}
  {detail.status === "awaiting_review" && <>
    <button className="secondary" ...>确认完成</button>
    <div className="task-inline-review-reject">...</div>
  </>}
  {(detail.status === "done" || detail.status === "cancelled") && <button className="secondary" ...>重新打开</button>}
  <span className="task-inline-actions-sep" />
  <button className="danger-text" ...>删除任务</button>
  <button className="secondary" ...>查看完整详情</button>
</div>
```

- [ ] **Step 2: 添加 CSS — 分隔线样式**

在 `tasks.css` 第 96 行附近（`.task-inline-detail-actions` 规则之后）追加分隔线样式：

```css
.task-inline-actions-sep { width: 1px; height: 22px; flex: none; margin: 0 3px; background: #d2e3da; }
```

- [ ] **Step 3: 修改 CSS — 验收 textarea 视觉优化**

修改 `tasks.css` 第 105-107 行，给 `.task-inline-review-reject` 增加左侧边框强调和微妙的背景：

```css
.task-inline-review-reject { display: grid; width: 100%; gap: 6px; margin-top: 2px; border-left: 3px solid #778bc4; padding-left: 9px; }
.task-inline-review-reject textarea { width: 100%; min-height: 54px; box-sizing: border-box; resize: vertical; border: 1px solid #ccd5d9; border-radius: 5px; padding: 7px 9px; color: #19332c; background: #fff; font: inherit; font-size: 11px; line-height: 1.45; }
.task-inline-review-reject textarea:focus { border-color: #55a697; outline: 2px solid #cfede3; }
```

关键改动：添加 `border-left: 3px solid #778bc4; padding-left: 9px;`。

- [ ] **Step 4: 验证构建**

```bash
cd /home/tangmaoke/projects/milevia/apps/web && npx tsc --noEmit 2>&1 | head -20
```

预期：无类型错误。

- [ ] **Step 5: Commit**

```bash
git add apps/web/src/tasks.css apps/web/src/features/tasks/TaskQueue.tsx
git commit -m "fix: 内联卡片操作按钮分组 + 验收textarea视觉优化"
```

---

### Task 3: 内联卡片移动端响应式适配

**问题:** 内联详情卡片在小屏幕（≤520px）下没有响应式处理。

**修复方案:**
- 在 `@media (max-width: 520px)` 中追加 `.task-inline-detail` 相关规则
- 操作按钮在小屏下改为等宽排列
- 卡片内边距缩小

**Files:**
- Modify: `apps/web/src/tasks.css:75`（在 520px 断点内追加规则）

**Interfaces:**
- Consumes: 现有 `@media (max-width: 520px)` 断点
- Produces: 追加的内联卡片响应式规则

- [ ] **Step 1: 在 520px 断点内追加内联卡片响应式规则**

修改 `tasks.css` 第 75 行，在 `@media (max-width: 520px)` 块的末尾（`}` 之前）追加：

```css
.task-inline-detail { padding: 10px; }.task-inline-detail-head { gap: 6px; }.task-inline-detail-title b { font-size: 13px; }.task-inline-detail-actions { gap: 5px; }.task-inline-detail-actions .primary, .task-inline-detail-actions .secondary, .task-inline-detail-actions .danger-text { flex: 1; text-align: center; }.task-inline-review-reject { padding-left: 6px; }
```

第 75 行当前末尾是 `}.task-dispatch-body { padding: 15px; } }`，需要在这最后的 `}` 之前插入新规则。

- [ ] **Step 2: 验证构建**

```bash
cd /home/tangmaoke/projects/milevia/apps/web && npx tsc --noEmit 2>&1 | head -20
```

预期：无类型错误。

- [ ] **Step 3: Commit**

```bash
git add apps/web/src/tasks.css
git commit -m "fix: 内联任务详情卡片移动端响应式适配"
```

---

### Task 4: 执行记录添加列标题

**问题:** 弹窗详情中执行记录表格缺少列标题，用户需自行推断列含义。

**修复方案:**
- 在 `.task-run-list` 中添加表头行（条件渲染：有记录时显示）
- 表头样式与 `.task-list-head` 风格一致

**Files:**
- Modify: `apps/web/src/features/tasks/TaskBoard.tsx:397`
- Modify: `apps/web/src/tasks.css:16`

**Interfaces:**
- Consumes: 现有 `.task-run-list` 结构
- Produces: 新增 `.task-run-list-head` CSS 类 + JSX 表头行

- [ ] **Step 1: 修改 JSX — 在 run-list 中添加表头行**

在 `TaskBoard.tsx` 第 397 行中，找到执行记录 section 的代码片段：

```jsx
<section><h3>执行记录</h3>{detail.runs.length === 0 ? <p>尚未下发。</p> : <div className="task-run-list">{detail.runs.map(...)}</div>}</section>
```

修改为在 `.task-run-list` 内部先渲染表头：

```jsx
<section><h3>执行记录</h3>{detail.runs.length === 0 ? <p>尚未下发。</p> : <div className="task-run-list"><div className="task-run-list-head"><span>次数</span><span>状态</span><span>时间</span></div>{detail.runs.map(...)}</div>}</section>
```

即把 `{detail.runs.map(...)}` 改为 `{<div className="task-run-list-head"><span>次数</span><span>状态</span><span>时间</span></div>}{detail.runs.map(...)}`。

- [ ] **Step 2: 添加 CSS — 表头样式**

在 `tasks.css` 第 16 行中 `.task-run-list` 规则后追加表头样式：

```css
.task-run-list-head { display: grid; grid-template-columns: 76px minmax(0, 1fr) auto; gap: 8px; padding: 4px 0 6px; color: #7a9086; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 10px; border-bottom: 1px solid #dce9e1; }
```

- [ ] **Step 3: 验证构建**

```bash
cd /home/tangmaoke/projects/milevia/apps/web && npx tsc --noEmit 2>&1 | head -20
```

预期：无类型错误。

- [ ] **Step 4: Commit**

```bash
git add apps/web/src/tasks.css apps/web/src/features/tasks/TaskBoard.tsx
git commit -m "fix: 执行记录列表添加列标题"
```