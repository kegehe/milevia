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

func newTaskTransferProject(t *testing.T, server *Server, projectID string) string {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,'main',1,?)`,
		projectID, projectID, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

func insertTransferTaskWithPinned(t *testing.T, server *Server, projectID, id, title, description, status string, position float64, pinned bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into tasks (id,project_id,title,description,priority,pinned,position,status,created_at,updated_at) values (?,?,?,?,'normal',?,?,?,?,?)`,
		id, projectID, title, description, pinned, position, status, now, now); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}
func insertTransferTask(t *testing.T, server *Server, projectID, id, title, description, status string, position float64) {
	t.Helper()
	insertTransferTaskWithPinned(t, server, projectID, id, title, description, status, position, false)
}

func insertTransferDependency(t *testing.T, server *Server, taskID, predecessorID string) {
	t.Helper()
	if _, err := server.db.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`,
		taskID, predecessorID, time.Now().UTC()); err != nil {
		t.Fatalf("insert dependency: %v", err)
	}
}

func exportTasksForTest(t *testing.T, handler http.Handler, projectID, scope string) taskTransferFile {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/tasks/export?scope="+scope, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("export %s: status=%d body=%s", scope, response.Code, response.Body.String())
	}
	var file taskTransferFile
	if err := json.NewDecoder(response.Body).Decode(&file); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	return file
}

func importTasksForTest(t *testing.T, handler http.Handler, projectID string, file taskTransferFile, dedupe bool) (int, taskTransferImportResult, string) {
	t.Helper()
	body, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("encode import: %v", err)
	}
	query := ""
	if !dedupe {
		query = "?dedupe=false"
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/import"+query, bytes.NewReader(body))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return response.Code, taskTransferImportResult{}, response.Body.String()
	}
	var result taskTransferImportResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode import: %v", err)
	}
	return response.Code, result, ""
}

func taskDetailForTest(t *testing.T, handler http.Handler, taskID string) TaskDetail {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get task %s: status=%d body=%s", taskID, response.Code, response.Body.String())
	}
	var detail TaskDetail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return detail
}

func countTasksForTest(t *testing.T, server *Server, projectID string) int {
	t.Helper()
	var count int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return count
}

func transferFileForTest(items ...taskTransferItem) taskTransferFile {
	return taskTransferFile{Format: taskTransferFormat, Version: taskTransferVersion, Tasks: items}
}

func transferItem(key, description string) taskTransferItem {
	return taskTransferItem{Key: key, Description: description, Priority: "normal", Status: taskTodo}
}

func TestTaskTransferExportActiveDropsDependenciesOnExcludedTasks(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "export-active")
	insertTransferTask(t, server, projectID, "task-done", "已完成", "已经做完了。", taskDone, 1)
	insertTransferTask(t, server, projectID, "task-todo", "待办", "还没做。", taskTodo, 2)
	insertTransferDependency(t, server, "task-todo", "task-done")

	active := exportTasksForTest(t, handler, projectID, "active")
	if len(active.Tasks) != 1 || active.Tasks[0].Key != "t1" {
		t.Fatalf("active export tasks = %+v", active.Tasks)
	}
	if active.Tasks[0].Description != "还没做。" {
		t.Fatalf("active export description = %q", active.Tasks[0].Description)
	}
	if len(active.Tasks[0].DependsOn) != 0 {
		t.Fatalf("active export kept dependency on excluded task: %+v", active.Tasks[0].DependsOn)
	}
	if active.DroppedDependencies != 1 {
		t.Fatalf("droppedDependencies = %d want 1", active.DroppedDependencies)
	}

	all := exportTasksForTest(t, handler, projectID, "all")
	if len(all.Tasks) != 2 {
		t.Fatalf("all export tasks = %d want 2", len(all.Tasks))
	}
	if all.DroppedDependencies != 0 {
		t.Fatalf("all droppedDependencies = %d want 0", all.DroppedDependencies)
	}
	// 导出顺序按 position：done 任务在前，key=t1；todo 任务 key=t2。
	if len(all.Tasks[1].DependsOn) != 1 || all.Tasks[1].DependsOn[0] != "t1" {
		t.Fatalf("all export dependencies = %+v", all.Tasks[1].DependsOn)
	}
	if all.Tasks[1].Status != taskTodo || all.Tasks[0].Status != taskDone {
		t.Fatalf("export statuses = %q,%q", all.Tasks[0].Status, all.Tasks[1].Status)
	}
}

func TestTaskTransferExportKeepsManualPositionOrderForPinnedTasks(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "export-pinned-order")
	// B 手工排在前面（position 1），A 置顶但 position 2。导出必须按 position 排：
	// 导入端按数组顺序重新编号，看板列内也只按 position 排。
	insertTransferTask(t, server, projectID, "task-b", "B", "B 的说明。", taskTodo, 1)
	insertTransferTaskWithPinned(t, server, projectID, "task-a", "A", "A 的说明。", taskTodo, 2, true)

	file := exportTasksForTest(t, handler, projectID, "all")
	if len(file.Tasks) != 2 {
		t.Fatalf("export tasks = %d", len(file.Tasks))
	}
	if file.Tasks[0].Description != "B 的说明。" || file.Tasks[1].Description != "A 的说明。" {
		t.Fatalf("export order = %+v", []string{file.Tasks[0].Description, file.Tasks[1].Description})
	}
	if !file.Tasks[1].Pinned || file.Tasks[0].Pinned {
		t.Fatalf("pinned flags = %v,%v", file.Tasks[0].Pinned, file.Tasks[1].Pinned)
	}
}
func TestTaskTransferExportRejectsUnsupportedScope(t *testing.T) {
	server := newTestServer(t)
	projectID := newTaskTransferProject(t, server, "export-scope")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/tasks/export?scope=weird", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", response.Code)
	}
}

func TestTaskTransferImportRebuildsIDsOrderAndDependencies(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	sourceID := newTaskTransferProject(t, server, "import-source")
	insertTransferTask(t, server, sourceID, "old-a", "旧 A", "任务 A 的说明。", taskTodo, 1)
	insertTransferTask(t, server, sourceID, "old-b", "旧 B", "任务 B 的说明。", taskTodo, 2)
	insertTransferDependency(t, server, "old-b", "old-a")
	file := exportTasksForTest(t, handler, sourceID, "all")

	targetID := newTaskTransferProject(t, server, "import-target")
	insertTransferTask(t, server, targetID, "target-existing", "已有", "目标项目已有任务。", taskTodo, 7)
	status, result, body := importTasksForTest(t, handler, targetID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 2 || result.Dependencies != 1 {
		t.Fatalf("import result = %+v", result)
	}
	for _, created := range result.Created {
		if created.ID == "old-a" || created.ID == "old-b" {
			t.Fatalf("import reused source id: %+v", created)
		}
	}

	var positions []float64
	rows, err := server.db.Query(`select position from tasks where project_id=? order by position`, targetID)
	if err != nil {
		t.Fatalf("query positions: %v", err)
	}
	for rows.Next() {
		var position float64
		if err := rows.Scan(&position); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		positions = append(positions, position)
	}
	rows.Close()
	if len(positions) != 3 || positions[0] != 7 || positions[1] != 8 || positions[2] != 9 {
		t.Fatalf("positions = %v want [7 8 9]", positions)
	}

	var importedBID string
	for _, created := range result.Created {
		if created.Key == "t2" {
			importedBID = created.ID
		}
	}
	detail := taskDetailForTest(t, handler, importedBID)
	if len(detail.DependsOn) != 1 || detail.DependsOn[0].Title != "旧 A" {
		t.Fatalf("imported dependency = %+v", detail.DependsOn)
	}
	if detail.CompletedAt != nil || detail.CancelledAt != nil {
		t.Fatalf("imported timestamps should be empty: completed=%v cancelled=%v", detail.CompletedAt, detail.CancelledAt)
	}
}

func TestTaskTransferImportNormalizesStatusAndKeepsMetadata(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-metadata")
	createdAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Title: "保留的标题", Description: "源状态是运行中。", Priority: "high", Pinned: true, Status: taskRunning, CreatedAt: &createdAt},
		taskTransferItem{Key: "t2", Description: "源状态是已完成。", Priority: "weird", Status: taskDone},
		taskTransferItem{Key: "t3", Description: "源状态是待验收。", Status: taskAwaitingReview},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 3 {
		t.Fatalf("imported = %d want 3", result.Imported)
	}
	var importedID string
	for _, created := range result.Created {
		if created.Key == "t1" {
			importedID = created.ID
		}
	}
	detail := taskDetailForTest(t, handler, importedID)
	if detail.Status != taskTodo {
		t.Fatalf("status = %q want todo", detail.Status)
	}
	if detail.Title != "保留的标题" || detail.Priority != "high" || !detail.Pinned {
		t.Fatalf("metadata not preserved: %+v", detail)
	}
	if !detail.CreatedAt.Equal(createdAt) {
		t.Fatalf("createdAt = %v want %v", detail.CreatedAt, createdAt)
	}
	for _, task := range fetchTasksForTest(t, handler, "/api/projects/"+projectID+"/tasks") {
		if task.Status != taskTodo {
			t.Fatalf("task %s status = %q want todo", task.ID, task.Status)
		}
		if task.CompletedAt != nil || task.CancelledAt != nil {
			t.Fatalf("task %s kept terminal timestamp", task.ID)
		}
		if task.Priority != "normal" && task.ID != importedID {
			t.Fatalf("invalid priority not normalized: %q", task.Priority)
		}
	}
}

func TestTaskTransferImportSatisfiesDonePredecessor(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-done-predecessor")
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "前置已完成。", Status: taskDone},
		taskTransferItem{Key: "t2", Description: "依赖已完成的前置。", Status: taskTodo, DependsOn: []string{"t1"}},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.SatisfiedDependencies != 1 || result.Dependencies != 0 {
		t.Fatalf("dependency accounting = %+v", result)
	}
	var taskID string
	for _, created := range result.Created {
		if created.Key == "t2" {
			taskID = created.ID
		}
	}
	detail := taskDetailForTest(t, handler, taskID)
	if len(detail.BlockedBy) != 0 {
		t.Fatalf("blockedBy = %+v want empty", detail.BlockedBy)
	}
	if !detail.CanDispatch {
		t.Fatalf("task should be dispatchable, blockReason=%q", detail.BlockReason)
	}
}

func TestTaskTransferImportKeepsCancelledPredecessorBlocking(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-cancelled-predecessor")
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "前置已取消。", Status: taskCancelled},
		taskTransferItem{Key: "t2", Description: "依赖已取消的前置。", Status: taskTodo, DependsOn: []string{"t1"}},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Dependencies != 1 || result.SatisfiedDependencies != 0 {
		t.Fatalf("dependency accounting = %+v", result)
	}
	var taskID string
	for _, created := range result.Created {
		if created.Key == "t2" {
			taskID = created.ID
		}
	}
	detail := taskDetailForTest(t, handler, taskID)
	if len(detail.BlockedBy) != 1 {
		t.Fatalf("blockedBy = %+v want one", detail.BlockedBy)
	}
	if detail.CanDispatch {
		t.Fatal("blocked task should not be dispatchable")
	}
}

func TestTaskTransferImportDedupesAndReusesExistingDependencyTarget(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-dedupe")
	insertTransferTask(t, server, projectID, "existing-a", "服务端精炼过的标题", "同一件事的说明。", taskTodo, 1)

	file := transferFileForTest(
		// 源机器的标题可能被 AI 精炼成另一个样子，去重键不能带 title。
		taskTransferItem{Key: "t1", Title: "源机器上的标题", Description: "同一件事的说明。", Status: taskTodo},
		taskTransferItem{Key: "t2", Description: "新任务。", Status: taskTodo, DependsOn: []string{"t1"}},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 1 || result.Duplicates != 1 || result.Dependencies != 1 {
		t.Fatalf("import result = %+v", result)
	}
	var taskID string
	for _, created := range result.Created {
		if created.Key == "t2" {
			taskID = created.ID
		}
	}
	detail := taskDetailForTest(t, handler, taskID)
	if len(detail.DependsOn) != 1 || detail.DependsOn[0].TaskID != "existing-a" {
		t.Fatalf("dependency should point at existing task: %+v", detail.DependsOn)
	}
}

func TestTaskTransferImportDoesNotTouchExistingTaskDependencies(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-dedupe-existing-deps")
	insertTransferTask(t, server, projectID, "existing-a", "已有任务", "同一件事的说明。", taskTodo, 1)
	// t1 与已有任务同说明（会去重），但它在文件里带着一条依赖。导入不能把这条依赖
	// 加到目标项目已有的任务上——那会悄悄改变它的阻塞关系。
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "同一件事的说明。", Status: taskTodo, DependsOn: []string{"t2"}},
		taskTransferItem{Key: "t2", Description: "新任务。", Status: taskTodo},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 1 || result.Duplicates != 1 || result.Dependencies != 0 {
		t.Fatalf("import result = %+v", result)
	}
	if detail := taskDetailForTest(t, handler, "existing-a"); len(detail.DependsOn) != 0 {
		t.Fatalf("existing task gained dependencies: %+v", detail.DependsOn)
	}
}

func TestTaskTransferImportCountsMergedDependencyOnce(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-merged-dependency")
	insertTransferTask(t, server, projectID, "existing-a", "已有任务", "同一件事的说明。", taskTodo, 1)
	// t1 与 t2 都命中同一条已有任务，t3 同时依赖两者：实际只落一条依赖。
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "同一件事的说明。", Status: taskTodo},
		taskTransferItem{Key: "t2", Description: "同一件事的说明。", Status: taskTodo},
		taskTransferItem{Key: "t3", Description: "新任务。", Status: taskTodo, DependsOn: []string{"t1", "t2"}},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 1 || result.Duplicates != 2 || result.Dependencies != 1 {
		t.Fatalf("import result = %+v", result)
	}
	var taskID string
	for _, created := range result.Created {
		if created.Key == "t3" {
			taskID = created.ID
		}
	}
	if detail := taskDetailForTest(t, handler, taskID); len(detail.DependsOn) != 1 || detail.DependsOn[0].TaskID != "existing-a" {
		t.Fatalf("dependencies = %+v", detail.DependsOn)
	}
}
func TestTaskTransferImportCanDisableDedupe(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-no-dedupe")
	insertTransferTask(t, server, projectID, "existing-a", "已有任务", "同一件事的说明。", taskTodo, 1)
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "同一件事的说明。", Status: taskTodo},
		taskTransferItem{Key: "t2", Description: "同一件事的说明。", Status: taskTodo},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, false)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 2 || result.Duplicates != 0 {
		t.Fatalf("import result = %+v", result)
	}
	if got := countTasksForTest(t, server, projectID); got != 3 {
		t.Fatalf("task count = %d want 3", got)
	}
}

func TestTaskTransferImportIsIdempotent(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-idempotent")
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "第一个任务。", Status: taskTodo},
		taskTransferItem{Key: "t2", Description: "第二个任务。", Status: taskTodo, DependsOn: []string{"t1"}},
	)
	if status, _, body := importTasksForTest(t, handler, projectID, file, true); status != http.StatusOK {
		t.Fatalf("first import: %d %s", status, body)
	}
	tasksAfterFirst := countTasksForTest(t, server, projectID)
	var dependencyCount int
	if err := server.db.QueryRow(`select count(*) from task_dependencies`).Scan(&dependencyCount); err != nil {
		t.Fatalf("count dependencies: %v", err)
	}

	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("second import: %d %s", status, body)
	}
	if result.Imported != 0 || result.Duplicates != 2 {
		t.Fatalf("second import result = %+v", result)
	}
	if got := countTasksForTest(t, server, projectID); got != tasksAfterFirst {
		t.Fatalf("task count changed: %d -> %d", tasksAfterFirst, got)
	}
	var after int
	if err := server.db.QueryRow(`select count(*) from task_dependencies`).Scan(&after); err != nil {
		t.Fatalf("count dependencies: %v", err)
	}
	if after != dependencyCount {
		t.Fatalf("dependency count changed: %d -> %d", dependencyCount, after)
	}
}

func TestTaskTransferImportSkipsInvalidItemsAndDropsTheirDependencies(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-invalid")
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "   ", Status: taskTodo},
		taskTransferItem{Key: "t2", Description: strings.Repeat("字", 12001), Status: taskTodo},
		taskTransferItem{Key: "t3", Description: "正常任务。", Status: taskTodo, DependsOn: []string{"t1", "t2"}},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 1 || len(result.Invalid) != 2 {
		t.Fatalf("import result = %+v", result)
	}
	if result.DroppedDependencies != 2 || result.Dependencies != 0 {
		t.Fatalf("dependency accounting = %+v", result)
	}
	var taskID string
	for _, created := range result.Created {
		if created.Key == "t3" {
			taskID = created.ID
		}
	}
	if detail := taskDetailForTest(t, handler, taskID); len(detail.DependsOn) != 0 {
		t.Fatalf("invalid dependencies were kept: %+v", detail.DependsOn)
	}
}

func TestTaskTransferImportRejectsMalformedFiles(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-malformed")

	tooMany := make([]taskTransferItem, 0, taskTransferMaxTasks+1)
	for index := 0; index <= taskTransferMaxTasks; index++ {
		tooMany = append(tooMany, transferItem(fmt.Sprintf("t%d", index+1), "任务说明。"))
	}
	cases := []struct {
		name string
		file taskTransferFile
	}{
		{name: "format", file: taskTransferFile{Format: "other", Version: 1, Tasks: []taskTransferItem{transferItem("t1", "说明。")}}},
		{name: "version", file: taskTransferFile{Format: taskTransferFormat, Version: 2, Tasks: []taskTransferItem{transferItem("t1", "说明。")}}},
		{name: "empty", file: transferFileForTest()},
		{name: "too-many", file: transferFileForTest(tooMany...)},
		{name: "duplicate-key", file: transferFileForTest(transferItem("t1", "说明一。"), transferItem("t1", "说明二。"))},
		{name: "bad-key", file: transferFileForTest(transferItem("t 1", "说明。"))},
		{name: "unknown-dependency", file: transferFileForTest(taskTransferItem{Key: "t1", Description: "说明。", DependsOn: []string{"t9"}})},
		{name: "self-dependency", file: transferFileForTest(taskTransferItem{Key: "t1", Description: "说明。", DependsOn: []string{"t1"}})},
		{name: "cycle", file: transferFileForTest(
			taskTransferItem{Key: "t1", Description: "说明一。", DependsOn: []string{"t2"}},
			taskTransferItem{Key: "t2", Description: "说明二。", DependsOn: []string{"t1"}},
		)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, _, body := importTasksForTest(t, handler, projectID, testCase.file, true)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d want 400, body=%s", status, body)
			}
			if got := countTasksForTest(t, server, projectID); got != 0 {
				t.Fatalf("malformed import created %d tasks", got)
			}
		})
	}
}

func TestTaskTransferImportCycleIgnoresSatisfiedEdges(t *testing.T) {
	// A(done) ← B 与 B → A 看起来成环，但 A 是已完成前置，这条依赖不会建立，不应误报环。
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-cycle-satisfied")
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "已完成的前置。", Status: taskDone, DependsOn: []string{"t2"}},
		taskTransferItem{Key: "t2", Description: "依赖已完成前置的任务。", Status: taskTodo, DependsOn: []string{"t1"}},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.Imported != 2 {
		t.Fatalf("imported = %d want 2", result.Imported)
	}
	if result.SatisfiedDependencies != 1 || result.Dependencies != 1 {
		t.Fatalf("dependency accounting = %+v", result)
	}
}

func TestTaskTransferImportNormalizesStatusAndPriorityCase(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-case")
	file := transferFileForTest(
		taskTransferItem{Key: "t1", Description: "前置已完成。", Status: " Done "},
		taskTransferItem{Key: "t2", Description: "依赖已完成前置。", Status: taskTodo, DependsOn: []string{"t1"}},
		taskTransferItem{Key: "t3", Description: "优先级大写。", Priority: "HIGH", Status: taskTodo},
	)
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	if result.SatisfiedDependencies != 1 || result.Dependencies != 0 {
		t.Fatalf("dependency accounting = %+v", result)
	}
	var highID string
	for _, created := range result.Created {
		if created.Key == "t3" {
			highID = created.ID
		}
	}
	if detail := taskDetailForTest(t, handler, highID); detail.Priority != "high" {
		t.Fatalf("priority = %q want high", detail.Priority)
	}
}
func TestTaskTransferImportFallsBackToGeneratedTitle(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	projectID := newTaskTransferProject(t, server, "import-title")
	file := transferFileForTest(taskTransferItem{Key: "t1", Description: "第一句是标题。后面还有内容。", Status: taskTodo})
	status, result, body := importTasksForTest(t, handler, projectID, file, true)
	if status != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", status, body)
	}
	detail := taskDetailForTest(t, handler, result.Created[0].ID)
	if detail.Title != "第一句是标题" {
		t.Fatalf("title = %q", detail.Title)
	}
}
