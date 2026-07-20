# Task Orchestration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add project-scoped task boards where users manually create, dispatch, review, and complete dependency-aware Claude development tasks.

**Architecture:** Introduce a backend Task domain that owns task planning, dependency validation, task-run snapshots, review decisions, and audit events. Extend the existing transactional Message/Run creation and centralized Run terminal handling so every dispatched task uses the current Claude conversation, events, permission mode, stop operation, and process lifecycle. Add a React task board and task detail panel as a feature module rendered from the existing project chat view.

**Tech Stack:** Go 1.26, Chi, SQLite, React 19, TypeScript, Vite, existing WebSocket Claude session transport.

## Global Constraints

- Do not create a Git commit; leave all implementation changes in the working tree.
- Keep `Task` (plan), `TaskRun` (one immutable execution) and existing `Run` (CLI process) distinct.
- Only the service may validate dependencies, construct dispatch prompts, create TaskRun records, or transition a task state.
- A Task is eligible for dispatch only when it is `todo` or `action_required`, has no active TaskRun, and every predecessor is `done`.
- A completed Claude Run transitions its Task to `awaiting_review`; only `accept` transitions the Task to `done`.
- Task dispatch must reuse the existing conversation permission, queue, approval, stop, event and usage behavior.
- No AI task decomposition, automatic task start, retry policy, cross-project dependency, direct shell execution, or scheduler implementation.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `apps/control-server/internal/app/task.go` | Task domain types, schema migration, CRUD, dependency graph validation, dispatch/review service methods, task routes, run-terminal and recovery helpers. |
| `apps/control-server/internal/app/app.go` | Register task routes; invoke task migration and recovery; pass TaskRun attachments through the existing message/run transaction; invoke task terminal updates from `finishRun`. |
| `apps/control-server/internal/app/app_test.go` | HTTP and service regression coverage for dependency, dispatch, review, duplicate dispatch and interrupted-run behavior. |
| `apps/web/src/features/tasks/TaskBoard.tsx` | Task list/board, detail panel, create/edit form, dependency controls, dispatch/review controls and typed API client contracts. |
| `apps/web/src/tasks.css` | Responsive task board and task-detail styles aligned with the existing project workspace. |
| `apps/web/src/App.tsx` | Import task styles, add the project-header task entry and render the TaskBoard without changing conversation behavior. |

## Task 1: Build the Task Domain and Dependency API

**Files:**
- Create: `apps/control-server/internal/app/task.go`
- Modify: `apps/control-server/internal/app/app.go:257-293,317-395`
- Test: `apps/control-server/internal/app/app_test.go`

**Interfaces:**
- Produces `Task`, `TaskRun`, `TaskEvent`, `TaskDetail`, and `taskInput` JSON models.
- Produces `func (s *Server) migrateTasks(ctx context.Context) error` and `func (s *Server) taskRoutes(r chi.Router)`.
- Produces `func (s *Server) taskForProject(ctx context.Context, taskID, projectID string) (Task, error)` for dispatch and review work.

- [ ] **Step 1: Write failing CRUD and dependency tests**

Add tests that create a project directly in SQLite, then exercise the router with JSON requests. Assert that creation requires non-empty title, description and acceptance criteria; a task belongs to exactly one project; a same-project predecessor can be added; self dependency, cross-project dependency and a cycle return `400`.

```go
func TestTaskDependenciesRejectCyclesAndCrossProjectLinks(t *testing.T) {
    server := newTestServer(t)
    projectID, otherProjectID := seedProjects(t, server)
    first := createTask(t, server, projectID, "Create API")
    second := createTask(t, server, projectID, "Create UI")
    other := createTask(t, server, otherProjectID, "Other project")

    requireStatus(t, server.routes(), http.MethodPost,
        "/api/tasks/"+second.ID+"/dependencies",
        `{"predecessorTaskId":"`+first.ID+`"}`, http.StatusCreated)
    requireStatus(t, server.routes(), http.MethodPost,
        "/api/tasks/"+first.ID+"/dependencies",
        `{"predecessorTaskId":"`+second.ID+`"}`, http.StatusBadRequest)
    requireStatus(t, server.routes(), http.MethodPost,
        "/api/tasks/"+first.ID+"/dependencies",
        `{"predecessorTaskId":"`+other.ID+`"}`, http.StatusBadRequest)
}
```

- [ ] **Step 2: Run the focused test to establish the missing endpoint**

Run: `cd apps/control-server && go test ./internal/app -run TestTaskDependenciesRejectCyclesAndCrossProjectLinks -count=1`

Expected: FAIL because `/api/tasks/{taskID}/dependencies` is not registered.

- [ ] **Step 3: Define models, migration and pure validation helpers in `task.go`**

Create typed models with explicit JSON names and closed state sets:

```go
const (
    taskTodo          = "todo"
    taskRunning       = "running"
    taskAwaitingReview = "awaiting_review"
    taskActionRequired = "action_required"
    taskDone          = "done"
    taskCancelled     = "cancelled"
)

type Task struct {
    ID, ProjectID, Title, Description, AcceptanceCriteria, Priority, Status string
    Position float64 `json:"position"`
    DependsOn []TaskDependency `json:"dependsOn"`
    BlockedBy []TaskBlocker `json:"blockedBy"`
    LastRun *TaskRun `json:"lastRun,omitempty"`
    CreatedAt, UpdatedAt time.Time
    CompletedAt, CancelledAt *time.Time `json:"completedAt,omitempty" json:"cancelledAt,omitempty"`
}

type TaskRun struct {
    ID, TaskID, ConversationID, RunID, Status, PromptSnapshot, AcceptanceSnapshot, FailureReason string
    Sequence int
    CreatedAt time.Time
    StartedAt, FinishedAt *time.Time `json:"startedAt,omitempty" json:"finishedAt,omitempty"`
}
```

`migrateTasks` must create `tasks`, `task_dependencies`, `task_runs`, `task_events` and the indexes specified in document 07. The migration must use foreign keys to `projects`, `conversations`, and `runs`; `run_id` is unique. Add a recursive CTE helper that rejects a dependency when the proposed predecessor can already reach the task.

- [ ] **Step 4: Implement CRUD, task detail and dependency handlers**

Register these routes from `Server.routes`:

```go
r.Get("/api/projects/{projectID}/tasks", s.listTasks)
r.Post("/api/projects/{projectID}/tasks", s.createTask)
r.Get("/api/tasks/{taskID}", s.getTask)
r.Patch("/api/tasks/{taskID}", s.updateTask)
r.Post("/api/tasks/{taskID}/dependencies", s.addTaskDependency)
r.Delete("/api/tasks/{taskID}/dependencies/{predecessorTaskID}", s.deleteTaskDependency)
r.Get("/api/tasks/{taskID}/runs", s.listTaskRuns)
r.Get("/api/tasks/{taskID}/events", s.listTaskEvents)
```

Validate project ownership in every mutation. Return `409` when a task in `running` or `awaiting_review` is edited in a way that would change its planned execution content; allow title/priority/position changes. Calculate `BlockedBy` by joining direct predecessors whose status is not `done`; never persist `blocked` as a status.

- [ ] **Step 5: Run focused backend tests**

Run: `cd apps/control-server && go test ./internal/app -run 'TestTask(Create|Dependencies)' -count=1`

Expected: PASS.

## Task 2: Attach TaskRun to Existing Message/Run Lifecycle

**Files:**
- Modify: `apps/control-server/internal/app/app.go:397-444,1258-1587`
- Modify: `apps/control-server/internal/app/task.go`
- Test: `apps/control-server/internal/app/app_test.go`

**Interfaces:**
- Consumes `Task`, `TaskRun`, `taskForProject` and task migration from Task 1.
- Produces `func (s *Server) dispatchTask(w http.ResponseWriter, r *http.Request)`.
- Produces `func (s *Server) finishTaskRunTx(ctx context.Context, tx *sql.Tx, runID, runStatus string, now time.Time) error`.
- Extends `startMessage` to accept a `*runStartRecord` that can carry a `ShortcutRun` and/or `TaskRun` within the existing transaction.

- [ ] **Step 1: Write failing dispatch and terminal-state tests**

Use a test project and current conversation. Create a predecessor and dependent task; mark the predecessor `done`; dispatch the dependent task. Assert exactly one user message, Run and TaskRun are created, the TaskRun prompt contains title, description and acceptance criteria, and the Task status is `running`. Finish the Run with `completed` and assert `awaiting_review`; finish a second dispatched task with `failed` and assert `action_required`.

```go
func TestDispatchTaskCreatesSnapshotAndRunCompletionWaitsForReview(t *testing.T) {
    server, projectID, conversationID := seedTaskConversation(t)
    task := createTask(t, server, projectID, "Implement board")

    response := dispatchTask(t, server, task.ID)
    if response.TaskRun.Status != "queued" && response.TaskRun.Status != "running" {
        t.Fatalf("unexpected TaskRun status: %q", response.TaskRun.Status)
    }
    assertTaskStatus(t, server, task.ID, taskRunning)
    server.finishRun(response.RunID, conversationID, "completed", nil)
    assertTaskStatus(t, server, task.ID, taskAwaitingReview)
}
```

- [ ] **Step 2: Run the focused test to establish the missing TaskRun integration**

Run: `cd apps/control-server && go test ./internal/app -run TestDispatchTaskCreatesSnapshotAndRunCompletionWaitsForReview -count=1`

Expected: FAIL because task dispatch is not implemented.

- [ ] **Step 3: Refactor `startMessage` around one transactional attachment record**

Replace the shortcut-only parameter with a record that has optional origins:

```go
type runStartRecord struct {
    Shortcut *ShortcutRun
    Task     *TaskRun
}

func (s *Server) startMessage(
    ctx context.Context,
    conversationID, content string,
    record *runStartRecord,
) (Message, string, *runStartRecord, int, error)
```

After inserting `runs`, insert `shortcut_runs` when `record.Shortcut != nil` and `task_runs` when `record.Task != nil`; assign the generated Run ID before insertion. Update `sendMessage` and `runShortcut` to use the new return value without changing their response bodies or permission behavior.

- [ ] **Step 4: Implement server-owned dispatch prompt and `dispatchTask`**

`dispatchTask` must: load the task with project and current conversation; require status `todo` or `action_required`; reject an active TaskRun and every unresolved predecessor; create a monotonically increasing sequence; create an immutable TaskRun with `queued` when the runner is streaming and `running` otherwise; and call `startMessage` with a constructed prompt.

```go
func taskPrompt(task Task) string {
    return fmt.Sprintf("任务：%s\n\n任务说明：\n%s\n\n验收条件：\n%s\n\n请在当前项目中完成该任务。完成后总结改动、运行的检查及未解决风险。",
        task.Title, task.Description, task.AcceptanceCriteria)
}
```

Expose it as `POST /api/tasks/{taskID}/dispatch`, returning `{ "message": Message, "runId": string, "taskRun": TaskRun }` with HTTP 202.

- [ ] **Step 5: Update terminal and recovery paths atomically**

Inside the existing `finishRun` transaction, call `finishTaskRunTx` after updating `runs`. It must only update TaskRuns that are still `queued` or `running`, preventing duplicated terminal events from overwriting a review decision. Map `completed` to `succeeded` plus `awaiting_review`; map `failed`, `stopped`, `interrupted` to the corresponding TaskRun status plus `action_required`; write exactly one TaskEvent for the transition.

Call an analogous transaction helper from `recoverInterruptedRuns` before the transaction commits, so a service restart cannot leave a Task in `running` after its Run is interrupted.

- [ ] **Step 6: Run lifecycle regression tests**

Run: `cd apps/control-server && go test ./internal/app -run 'Test(DispatchTask|FinishRun|RecoverInterrupted)' -count=1`

Expected: PASS.

## Task 3: Add Review, Reopen, Cancel and Stop Operations

**Files:**
- Modify: `apps/control-server/internal/app/task.go`
- Modify: `apps/control-server/internal/app/app.go:1588-1638`
- Test: `apps/control-server/internal/app/app_test.go`

**Interfaces:**
- Consumes the TaskRun lifecycle from Task 2.
- Produces `POST /api/tasks/{taskID}/review`, `/reopen`, `/cancel`, `/stop`.
- Produces `func (s *Server) stopRunByID(ctx context.Context, runID string) (string, error)` used by both the existing run endpoint and task endpoint.

- [ ] **Step 1: Write failing review and eligibility tests**

Cover accepting a task after a completed Run, rejecting review for any state other than `awaiting_review`, requesting changes, reopening a done predecessor, cancelling an active task rejection, and a dependent task becoming dispatchable only after acceptance.

```go
func TestTaskReviewAcceptUnlocksDependentTask(t *testing.T) {
    server, projectID, conversationID := seedTaskConversation(t)
    predecessor := createTask(t, server, projectID, "Add API")
    dependent := createTask(t, server, projectID, "Use API")
    addDependency(t, server, dependent.ID, predecessor.ID)

    requireStatus(t, server.routes(), http.MethodPost, "/api/tasks/"+dependent.ID+"/dispatch", `{}`, http.StatusConflict)
    run := dispatchTask(t, server, predecessor.ID)
    server.finishRun(run.RunID, conversationID, "completed", nil)
    requireStatus(t, server.routes(), http.MethodPost, "/api/tasks/"+predecessor.ID+"/review", `{"action":"accept"}`, http.StatusOK)
    requireStatus(t, server.routes(), http.MethodPost, "/api/tasks/"+dependent.ID+"/dispatch", `{}`, http.StatusAccepted)
}
```

- [ ] **Step 2: Run the focused review test**

Run: `cd apps/control-server && go test ./internal/app -run TestTaskReviewAcceptUnlocksDependentTask -count=1`

Expected: FAIL because review endpoints are absent.

- [ ] **Step 3: Implement strict state transitions and audit events**

Implement `reviewTask` with a body `{action: "accept" | "request_changes", note?: string}`. `accept` atomically transitions only `awaiting_review -> done`, sets `completed_at`, and records `task.accepted`; `request_changes` transitions only `awaiting_review -> action_required` and records the note. Implement reopen only from `done` or `cancelled`; implement cancel only from `todo` or `action_required`.

Implement `stopTask` by locating its one active TaskRun and calling `stopRunByID`. Extract the present cancellation/session-stop logic out of `stopRun`, then keep `/api/runs/{runID}/stop` as a thin HTTP wrapper. Do not allow task cancellation or reopening while an active TaskRun exists.

- [ ] **Step 4: Run review and stop tests**

Run: `cd apps/control-server && go test ./internal/app -run 'TestTask(Review|Cancel|Reopen|Stop)' -count=1`

Expected: PASS.

## Task 4: Implement the React Task Board and Task Detail Workflow

**Files:**
- Create: `apps/web/src/features/tasks/TaskBoard.tsx`
- Create: `apps/web/src/tasks.css`
- Modify: `apps/web/src/App.tsx:1-20,600-652`

**Interfaces:**
- Consumes the APIs from Tasks 1-3.
- Produces `TaskBoard({ projectID, request, fail, close })` and `TaskDetailDialog`.
- Keeps the existing Chat component responsible for current Conversation, WebSocket and generic error display.

- [ ] **Step 1: Add typed task contracts and a testable request boundary**

In `TaskBoard.tsx`, define Task, TaskRun, TaskEvent and TaskDetail contracts matching the API. Receive the current `api` helper through a generic prop instead of duplicating fetch behavior:

```tsx
export type Request = <T>(path: string, init?: RequestInit) => Promise<T>;

export function TaskBoard({ projectID, request, fail, close }: {
  projectID: string;
  request: Request;
  fail: (message: string) => void;
  close: () => void;
}) {
  // Load /api/projects/{projectID}/tasks, then render state columns from one list.
}
```

- [ ] **Step 2: Build board/list projection and create/edit form**

Provide a compact view toggle. The board columns are 待处理, 受阻, 执行中, 待验收, 需处理 and 已完成; `cancelled` is exposed through a filter. Categorize blocked `todo` and `action_required` tasks into 受阻 based on `blockedBy.length`, without changing their persisted status.

The form requires title, description and acceptance criteria, limits title to 120 characters and long text to 12000 characters, offers `urgent/high/normal/low`, and allows selecting same-project predecessor tasks except the current task. Save with `POST` or `PATCH`, refresh the list after success, and surface service errors through the existing error banner.

- [ ] **Step 3: Build task detail, dispatch and review controls**

The detail panel must show description, acceptance conditions, blocking tasks, blocks-next tasks, latest TaskRun state and run history. It fetches `GET /api/tasks/{taskID}`. Before dispatch, show the server-provided `promptPreview`, current permission label, and blockers. Disable dispatch when status or blockers make it ineligible; on click call `POST /api/tasks/{taskID}/dispatch`, refresh detail/list, and close to the existing conversation where the run process is visible.

For `awaiting_review`, provide only 确认完成 and 要求修改. Both use a confirmation dialog; request-changes requires a reason. Provide reopen and cancel only when their server-side state conditions allow them. For an active TaskRun, offer 停止任务 using `/api/tasks/{taskID}/stop`.

- [ ] **Step 4: Integrate from the project chat header**

Add a header button with a `ListTodo`-style textual affordance only if no icon library is installed, labelled “任务”. Keep the conversation canvas mounted when the board opens only if doing so does not duplicate WebSockets; otherwise use a task-view conditional that returns to the exact Conversation state. Pass `api` and `fail` directly to `TaskBoard`.

```tsx
const [showTasks, setShowTasks] = useState(false);

<button className="secondary" onClick={() => setShowTasks(true)}>任务</button>
{showTasks ? <TaskBoard projectID={project.id} request={api} fail={fail} close={() => setShowTasks(false)} /> : <ConversationCanvas />}
```

- [ ] **Step 5: Add responsive styles and run the frontend build**

Use full-width workspace sections rather than cards nested within cards. Maintain the existing 7-8px border radius, compact control sizes, no viewport-scaled typography, and make board columns horizontally scrollable on narrow screens. Add focus-visible states for controls and ensure all status labels wrap rather than overlap.

Run: `pnpm --filter @auto/web build`

Expected: TypeScript and Vite build exit 0.

## Task 5: Execute End-to-End Regression Verification

**Files:**
- Modify: `apps/control-server/internal/app/app_test.go` only when a missing integration assertion is exposed.

**Interfaces:**
- Consumes all previous API and UI behavior.
- Produces fresh evidence that existing conversation, shortcut and task paths remain compatible.

- [ ] **Step 1: Add a streaming queue regression test for task dispatch**

Use a controllable `StreamingAgentRunner` that records request order. Dispatch two independent tasks into the same current conversation, start and finish each recorded turn in order, and assert each TaskRun has its own Run ID and both tasks become `awaiting_review` only after their corresponding terminal events.

- [ ] **Step 2: Run all backend tests with race detection**

Run: `cd apps/control-server && go test -race ./...`

Expected: PASS with no race reports.

- [ ] **Step 3: Run backend static checks and frontend build**

Run: `cd apps/control-server && go vet ./...`

Expected: PASS.

Run: `cd apps/control-server && go build ./cmd/control-server`

Expected: PASS.

Run: `pnpm --filter @auto/web build`

Expected: PASS.

- [ ] **Step 4: Inspect the final diff and working tree without committing**

Run: `git diff --check`

Expected: no output.

Run: `git status --short`

Expected: implementation files are modified or added; no `git commit` command is run.

## Plan Self-Review

| Document 07 requirement | Planned task |
| --- | --- |
| Manual task creation, board and list views | Tasks 1 and 4 |
| Hard same-project predecessor dependencies and no cycles | Task 1 |
| Task/TaskRun/Run separation and snapshots | Task 2 |
| Existing permission, queue, stop, event and usage reuse | Task 2 and Task 5 |
| Human acceptance unlocks downstream work | Task 3 |
| Cancellation, reopen, restart recovery and audit history | Tasks 2 and 3 |
| Automatic scheduler excluded but future-compatible | Global constraints and Task 2 model |
| Verification without a code commit | Task 5 and global constraints |

No plan step requires a scheduler, a new Runner, a direct shell endpoint, AI task generation, cross-project dependencies, or a Git commit.
