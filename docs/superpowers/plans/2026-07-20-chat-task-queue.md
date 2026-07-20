# Chat Task Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a compact, dependency-aware task queue to the project chat's left work rail without changing server task rules.

**Architecture:** Put task types, ordering, filtering and display eligibility in a pure TypeScript module shared by `TaskBoard` and the new `TaskQueue`. `TaskQueue` fetches the existing project task list, fetches task detail only before showing a dispatch confirmation dialog, and sends successful dispatch results back to `Chat`, which remains responsible for the conversation message and active-run state.

**Tech Stack:** React 19, TypeScript, Vite, existing task HTTP APIs, Node 22 built-in test runner with TypeScript type stripping.

## Global Constraints

- Do not create a Git commit.
- Keep manual creation, manual dispatch and manual acceptance; do not add task scheduling or automatic execution.
- `POST /api/tasks/{taskID}/dispatch` remains the server-side authority for dispatch eligibility.
- Use no new backend API or package dependency.
- Keep desktop task queue width stable and move it to a mobile drawer below the desktop breakpoint.

---

### Task 1: Create Shared Task Presentation Logic

**Files:**
- Create: `apps/web/src/features/tasks/task-model.ts`
- Create: `apps/web/src/features/tasks/task-model.test.ts`
- Modify: `apps/web/tsconfig.json`
- Modify: `apps/web/src/features/tasks/TaskBoard.tsx`

**Interfaces:**
- Produces `Task`, `TaskRun`, `TaskDetail`, `TaskStatus`, `Priority`, `TaskFilter` and `Request` types.
- Produces `statusLabels`, `priorityLabels`, `isTaskBlocked(task)`, `canOfferDispatch(task)`, `filterQueueTasks(tasks, filter)` and `sortQueueTasks(tasks)`.
- `TaskBoard` consumes the shared models and retains its existing API behavior.

- [ ] **Step 1: Write the failing task-queue ordering test**

Create `task-model.test.ts` importing `filterQueueTasks` and `sortQueueTasks` from the not-yet-created `./task-model.ts`. Build tasks for a blocked todo, a dispatchable todo, a running task, an awaiting-review task and an action-required task. Assert that default filtering excludes done/cancelled tasks and that sorting produces awaiting-review, action-required, running, dispatchable, blocked.

```ts
import assert from "node:assert/strict";
import test from "node:test";
import { filterQueueTasks, sortQueueTasks, type Task } from "./task-model.ts";

test("orders actionable queue before blocked work", () => {
  const tasks = [blockedTodo, readyTodo, running, review, actionRequired];
  assert.deepEqual(sortQueueTasks(filterQueueTasks(tasks, "active")).map((task) => task.id), ["review", "repair", "running", "ready", "blocked"]);
});
```

- [ ] **Step 2: Verify the test fails for the intended reason**

Run: `cd apps/web && node --experimental-strip-types --test src/features/tasks/task-model.test.ts`

Expected: FAIL with `ERR_MODULE_NOT_FOUND` for `task-model.ts`.

- [ ] **Step 3: Add the pure task model**

Create `task-model.ts` with API-aligned types and explicit display helpers. `isTaskBlocked` returns true only for todo/action-required tasks with a non-empty `blockedBy`; `canOfferDispatch` returns true only for todo/action-required tasks without blockers. `filterQueueTasks(..., "active")` excludes done/cancelled; named filters select `todo`, `running`, and `awaiting_review` while blocked work remains in `todo`. `sortQueueTasks` ranks awaiting-review, action-required, running, ready, blocked, then priority, position and title.

```ts
export const queueRank = (task: Task): number => {
  if (task.status === "awaiting_review") return 0;
  if (task.status === "action_required" && !isTaskBlocked(task)) return 1;
  if (task.status === "running") return 2;
  if (canOfferDispatch(task)) return 3;
  if (isTaskBlocked(task)) return 4;
  return 5;
};
```

Exclude `src/**/*.test.ts` from the application TypeScript build; Node 22 runs it directly with type stripping.

- [ ] **Step 4: Replace TaskBoard-local models with shared imports**

Import the shared types, label maps and blocked predicate into `TaskBoard.tsx`. Keep `TaskDetail` and `TaskRun` response fields compatible with the existing task endpoints; do not change the board's routes, state transitions or dispatch request.

- [ ] **Step 5: Verify the pure test and the existing web build**

Run:

```bash
cd apps/web && node --experimental-strip-types --test src/features/tasks/task-model.test.ts
pnpm --filter @auto/web build
```

Expected: the Node test passes and TypeScript/Vite production build exits 0.

### Task 2: Build the Queue and Dispatch Confirmation

**Files:**
- Create: `apps/web/src/features/tasks/TaskQueue.tsx`
- Modify: `apps/web/src/App.tsx`

**Interfaces:**
- `TaskQueue` consumes `{ projectID, permissionMode, request, fail, onDispatched, openBoard }`.
- `onDispatched(message, runID)` is the existing `Chat` callback that appends a user message and updates run/usage/history state.
- `openBoard(taskID?)` renders `TaskBoard`; an optional task ID opens that detail after list load.

- [ ] **Step 1: Extend the failing source contract test**

Add a test that reads `TaskQueue.tsx` and asserts it contains the detail request before the dispatch request, the `canDispatch` guard, and no direct dispatch request in the task-row button handler. This protects the chosen two-step interaction until a browser test harness is introduced.

```ts
const source = readFileSync(new URL("./TaskQueue.tsx", import.meta.url), "utf8");
assert.match(source, /\/api\/tasks\/\$\{taskID\}/);
assert.match(source, /detail\.canDispatch/);
assert.match(source, /\/dispatch/);
```

- [ ] **Step 2: Verify the contract test fails**

Run: `cd apps/web && node --experimental-strip-types --test src/features/tasks/task-model.test.ts`

Expected: FAIL because `TaskQueue.tsx` does not exist.

- [ ] **Step 3: Implement `TaskQueue`**

Fetch `GET /api/projects/{projectID}/tasks` on mount and every 10 seconds. Render filter tabs with counts, task rows using shared filter/sort helpers and a compact status/priority/dependency summary. Task title opens `openBoard(task.id)`. Only show a row-level `下发` button when `canOfferDispatch(task)` is true.

On that button, stop propagation and fetch `GET /api/tasks/{taskID}`. Render a modal confirmation layer with title, a two-line description summary, priority, permission label and either the server block reason or a ready message. Keep confirmation disabled unless `detail.canDispatch` is true. `确认下发` posts `{}` to `/api/tasks/{taskID}/dispatch`, calls `onDispatched`, closes the layer and reloads list. Handle stale eligibility errors through `fail` and reload the list.

- [ ] **Step 4: Integrate queue state into `Chat`**

Replace the two vertically stacked shortcut groups in the left rail with one `quick-actions-row` containing prompt and command shortcut chips. Render `TaskQueue` immediately below it. Replace boolean `showTasks` with `null | { taskID?: string }`; pass selected IDs to `TaskBoard` so the queue can open the existing full detail view. Keep header `任务` button opening the board without a selected ID.

Extract the existing `onDispatched` closure into a named `handleTaskDispatched` callback so both `TaskBoard` and `TaskQueue` use exactly the same conversation/run refresh behavior.

- [ ] **Step 5: Verify source contract and production build**

Run:

```bash
cd apps/web && node --experimental-strip-types --test src/features/tasks/task-model.test.ts
pnpm --filter @auto/web build
```

Expected: all Node tests pass and the production build exits 0.

### Task 3: Apply Responsive Work-Rail Styling

**Files:**
- Modify: `apps/web/src/style.css`
- Modify: `apps/web/src/tasks.css`

**Interfaces:**
- `conversation-canvas` provides grid layout for `.quick-tag-rail` and `.timeline`.
- `TaskQueue` provides `.task-queue`, `.task-queue-row`, `.task-dispatch-confirmation` and mobile drawer classes.

- [ ] **Step 1: Add a failing style contract assertion**

Extend `task-model.test.ts` to read `style.css` and `tasks.css`. Assert that the desktop work rail uses `grid-template-columns: 300px minmax(0, 1fr)`, the action row uses horizontal overflow, and the mobile breakpoint defines a task drawer control.

- [ ] **Step 2: Verify the style contract fails**

Run: `cd apps/web && node --experimental-strip-types --test src/features/tasks/task-model.test.ts`

Expected: FAIL because the selected work-rail declarations are absent.

- [ ] **Step 3: Implement desktop and mobile styles**

Set `conversation-canvas` to a constrained two-column grid; make the work rail sticky beneath the project header and independently scrollable. Style prompt/command chips as a single no-wrap row with visible focus styles and an overflow affordance. Give task rows fixed compact heights, explicit status colors, truncated text and stable dispatch-button width.

At the existing 820px breakpoint, preserve the shortcut row, hide the rail queue behind a `任务（数量）` toggle, and render it as a modal-height drawer that does not cover the composer. At 520px, move filter controls to a horizontal scroller and make confirmation actions full-width without changing text size by viewport.

- [ ] **Step 4: Run final verification**

Run:

```bash
cd apps/web && node --experimental-strip-types --test src/features/tasks/task-model.test.ts
pnpm --filter @auto/web build
git diff --check
```

Expected: tests pass, build exits 0, and no whitespace errors are reported. Inspect `http://127.0.0.1:5173/` at desktop and mobile widths to confirm the task drawer, confirmation layer and composer do not overlap.
