package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hydrationSnapshot 只保留列表接口批量补齐的派生字段，用来和单任务详情逐字段对照。
type hydrationSnapshot struct {
	DependsOn                 []TaskDependency
	BlockedBy                 []TaskBlocker
	Blocks                    []TaskDependency
	LastRun                   *TaskRun
	OrchestrationStatus       string
	OrchestrationTargetBranch string
	OrchestrationPending      bool
	OrchestrationUpdatedAt    string
}

func snapshotTaskHydration(task Task) hydrationSnapshot {
	snapshot := hydrationSnapshot{
		DependsOn:                 task.DependsOn,
		BlockedBy:                 task.BlockedBy,
		Blocks:                    task.Blocks,
		LastRun:                   task.LastRun,
		OrchestrationStatus:       task.OrchestrationStatus,
		OrchestrationTargetBranch: task.OrchestrationTargetBranch,
		OrchestrationPending:      task.OrchestrationPending,
	}
	if task.OrchestrationUpdatedAt != nil {
		snapshot.OrchestrationUpdatedAt = task.OrchestrationUpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return snapshot
}

func snapshotJSON(t *testing.T, snapshot hydrationSnapshot) string {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode hydration snapshot: %v", err)
	}
	return string(encoded)
}

func fetchTasksForTest(t *testing.T, handler http.Handler, path string) []Task {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
	}
	var tasks []Task
	if err := json.NewDecoder(response.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
	return tasks
}

// TestTaskListHydrationMatchesTaskDetail 保证任务列表（看板 / 会话侧栏每 10 秒轮询的那条路径）
// 批量补齐的依赖、最近一次执行、编排状态与单任务详情完全一致。批量查询最容易错在两处：
// 跨任务的 join 把顺序打乱、以及每条任务只取到"随便一次执行"。
func TestTaskListHydrationMatchesTaskDetail(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	handler := server.routes()
	now := time.Now().UTC()

	taskA := createTaskForTest(t, handler, projectID, "task A")
	taskB := createTaskForTest(t, handler, projectID, "task B")
	taskC := createTaskForTest(t, handler, projectID, "task C")

	// A ← B、A ← C、B ← C。三条依赖的 created_at 两两不同，因为下面要断言
	// "同一任务的依赖按 created_at 排序"——相等的时间戳下 SQLite 不保证顺序，断言会变成碰运气。
	for _, link := range []struct {
		taskID, predecessorID string
		createdAt             time.Time
	}{
		{taskB, taskA, now},
		{taskC, taskA, now.Add(time.Second)},
		{taskC, taskB, now.Add(2 * time.Second)},
	} {
		if _, err := server.db.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, link.taskID, link.predecessorID, link.createdAt); err != nil {
			t.Fatalf("insert dependency: %v", err)
		}
	}

	// A 有三次执行，最近一次是 sequence 3。created_at 故意与 sequence 反向，
	// 这样"取 sequence 最大"和"取 created_at 最新"会给出不同答案，断言才有区分度。
	for sequence := 1; sequence <= 3; sequence++ {
		if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values (?,?,?,NULL,?,?,?,?,?,?)`,
			fmt.Sprintf("task-run-%d", sequence), taskA, conversationID, sequence, taskDone, "prompt", "", "", now.Add(time.Duration(4-sequence)*time.Minute)); err != nil {
			t.Fatalf("insert task run: %v", err)
		}
	}

	// C 有进行中的编排（策略快照指定了 release 分支）；B 的编排已合并结束，不该出现在状态里。
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-c',?,?,1,'implementing',7,'{"mainBranch":"release"}',?,?)`, projectID, taskC, now, now); err != nil {
		t.Fatalf("insert orchestration job: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-b',?,?,1,'released_to_main',7,'{}',?,?)`, projectID, taskB, now, now); err != nil {
		t.Fatalf("insert orchestration job: %v", err)
	}

	listed := fetchTasksForTest(t, handler, "/api/projects/"+projectID+"/tasks")
	if len(listed) != 3 {
		t.Fatalf("listed tasks: got %d want 3", len(listed))
	}
	byID := map[string]Task{}
	for _, task := range listed {
		byID[task.ID] = task
	}

	// 显式断言关键形态：顺序、被挡关系、最近一次执行、编排状态。
	if got := byID[taskC].DependsOn; len(got) != 2 || got[0].TaskID != taskA || got[1].TaskID != taskB {
		t.Fatalf("task C dependsOn: %+v want [A B] in created_at order", got)
	}
	if got := byID[taskA].Blocks; len(got) != 2 || got[0].TaskID != taskB || got[1].TaskID != taskC {
		t.Fatalf("task A blocks: %+v want [B C] in created_at order", got)
	}
	if got := byID[taskC].BlockedBy; len(got) != 2 || got[0].TaskID != taskA || got[1].TaskID != taskB {
		t.Fatalf("task C blockedBy: %+v want [A B]", got)
	}
	if len(byID[taskB].Blocks) != 1 || byID[taskB].Blocks[0].TaskID != taskC {
		t.Fatalf("task B blocks: %+v want [C]", byID[taskB].Blocks)
	}
	if got := byID[taskB].DependsOn; len(got) != 1 || got[0].TaskID != taskA {
		t.Fatalf("task B dependsOn: %+v want [A]", got)
	}
	if lastRun := byID[taskA].LastRun; lastRun == nil || lastRun.Sequence != 3 || lastRun.ID != "task-run-3" {
		t.Fatalf("task A lastRun: %+v want task-run-3 (max sequence, not latest created_at)", lastRun)
	}
	if byID[taskB].LastRun != nil {
		t.Fatalf("task B lastRun: %+v want nil", byID[taskB].LastRun)
	}
	if byID[taskC].OrchestrationStatus != "implementing" || byID[taskC].OrchestrationTargetBranch != "release" {
		t.Fatalf("task C orchestration: status=%q branch=%q", byID[taskC].OrchestrationStatus, byID[taskC].OrchestrationTargetBranch)
	}
	if byID[taskB].OrchestrationStatus != "" {
		t.Fatalf("task B orchestration status: %q want empty for released_to_main", byID[taskB].OrchestrationStatus)
	}

	// 列表与详情不能分叉。注意两边如今共用 hydrateTasks，所以这条对照主要守的是
	// listTasks 的接线（以及将来有人给 git 路径单独加查询时的偏差）；真正的语义断言在上面。
	for _, id := range []string{taskA, taskB, taskC} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+id, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/tasks/%s: status=%d body=%s", id, response.Code, response.Body.String())
		}
		var detail TaskDetail
		if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
			t.Fatalf("decode task detail: %v", err)
		}
		want := snapshotJSON(t, snapshotTaskHydration(detail.Task))
		got := snapshotJSON(t, snapshotTaskHydration(byID[id]))
		if got != want {
			t.Fatalf("task %s hydration drift between list and detail:\n list=%s\n detail=%s", id, got, want)
		}
	}
}

// TestTaskListHydrationChunksLargeBoards 覆盖 id 分批的边界：任务数超过 sqlIDBatchSize 时，
// 每一批都必须带上自己的绑定参数，否则后面的任务会静默丢掉依赖/执行/编排状态。
func TestTaskListHydrationChunksLargeBoards(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	now := time.Now().UTC()
	total := sqlIDBatchSize + 1
	tx, err := server.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for index := 0; index < total; index++ {
		id := fmt.Sprintf("bulk-task-%03d", index)
		if _, err := tx.Exec(`insert into tasks (id,project_id,title,description,priority,position,status,created_at,updated_at) values (?,?,?,?,?,?,?,?,?)`,
			id, projectID, id, id, "normal", float64(index), taskTodo, now, now); err != nil {
			tx.Rollback()
			t.Fatalf("insert bulk task: %v", err)
		}
	}
	// 每条批量任务都依赖第一条：跨批次也要拿到同一个 predecessor。
	for index := 1; index < total; index++ {
		if _, err := tx.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, fmt.Sprintf("bulk-task-%03d", index), "bulk-task-000", now); err != nil {
			tx.Rollback()
			t.Fatalf("insert bulk dependency: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	listed := fetchTasksForTest(t, server.routes(), "/api/projects/"+projectID+"/tasks")
	if len(listed) != total {
		t.Fatalf("listed tasks: got %d want %d", len(listed), total)
	}
	for _, task := range listed {
		switch task.ID {
		case "bulk-task-000":
			if len(task.Blocks) != total-1 {
				t.Fatalf("predecessor blocks: got %d want %d", len(task.Blocks), total-1)
			}
		default:
			if len(task.DependsOn) != 1 || task.DependsOn[0].TaskID != "bulk-task-000" {
				t.Fatalf("task %s dependsOn: %+v want [bulk-task-000]", task.ID, task.DependsOn)
			}
			if len(task.BlockedBy) != 1 {
				t.Fatalf("task %s blockedBy: %+v want one todo blocker", task.ID, task.BlockedBy)
			}
		}
	}
}

func fetchTaskDetailForTest(t *testing.T, handler http.Handler, taskID string) Task {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks/%s: status=%d body=%s", taskID, response.Code, response.Body.String())
	}
	var detail TaskDetail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decode task detail: %v", err)
	}
	return detail.Task
}

func assertTaskDependencyOrder(t *testing.T, task Task, want []string) {
	t.Helper()
	got := make([]string, 0, len(task.DependsOn))
	for _, dependency := range task.DependsOn {
		got = append(got, dependency.TaskID)
	}
	if len(got) != len(want) {
		t.Fatalf("task %s dependsOn: got %v want %v", task.ID, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("task %s dependsOn: got %v want %v", task.ID, got, want)
		}
	}
}

// TestTaskDependencyOrderFollowsRequest 固定"依赖列表顺序 = 请求里给的顺序"。
// 同一批依赖是共用同一个 created_at 插入的，所以顺序完全由插入顺序决定；
// 写侧若再退回遍历 map，或读侧只按 created_at 排序，这个用例都会失败。
func TestTaskDependencyOrderFollowsRequest(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	handler := server.routes()
	predecessorA := createTaskForTest(t, handler, projectID, "predecessor A")
	predecessorB := createTaskForTest(t, handler, projectID, "predecessor B")
	predecessorC := createTaskForTest(t, handler, projectID, "predecessor C")

	body := func(ordered ...string) *bytes.Buffer {
		quoted := make([]string, 0, len(ordered))
		for _, id := range ordered {
			quoted = append(quoted, `"`+id+`"`)
		}
		payload := `{"description":"Depends on several tasks.","priority":"normal","predecessorTaskIds":[` + strings.Join(quoted, ",") + `]}`
		return bytes.NewBufferString(payload)
	}

	created := httptest.NewRecorder()
	handler.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks", body(predecessorB, predecessorC, predecessorA)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create task: status=%d body=%s", created.Code, created.Body.String())
	}
	var task struct{ ID string }
	if err := json.NewDecoder(created.Body).Decode(&task); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	assertTaskDependencyOrder(t, fetchTaskDetailForTest(t, handler, task.ID), []string{predecessorB, predecessorC, predecessorA})

	// 全量替换依赖后也要按新请求的顺序。
	patched := httptest.NewRecorder()
	handler.ServeHTTP(patched, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+task.ID, body(predecessorA, predecessorB)))
	if patched.Code != http.StatusOK {
		t.Fatalf("patch task: status=%d body=%s", patched.Code, patched.Body.String())
	}
	assertTaskDependencyOrder(t, fetchTaskDetailForTest(t, handler, task.ID), []string{predecessorA, predecessorB})

	// 老数据：同一 created_at 的依赖按插入顺序（rowid）兜底，而不是交给 SQL 任选。
	legacy := createTaskForTest(t, handler, projectID, "legacy task")
	tied := time.Now().UTC()
	for _, predecessorID := range []string{predecessorC, predecessorA} {
		if _, err := server.db.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, legacy, predecessorID, tied); err != nil {
			t.Fatalf("insert tied dependency: %v", err)
		}
	}
	assertTaskDependencyOrder(t, fetchTaskDetailForTest(t, handler, legacy), []string{predecessorC, predecessorA})
}
