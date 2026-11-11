package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTaskDraftCorrectsModelOutput(t *testing.T) {
	long := strings.Repeat("字", taskDescriptionRuneMax+5)
	draft, err := normalizeTaskDraft(taskDraft{
		Understood:  "  整理登录  ",
		Mode:        "split",
		Assumptions: []string{"  沿用现有会话接口  ", ""},
		Questions:   []string{" "},
		Tasks: []taskDraftItem{
			{Key: "t1", Description: "  " + long + "  ", Priority: "nope", DependsOn: []string{"t2", "missing", "t1"}},
			{Key: "t1", Description: "重复编号应丢掉", Priority: "high"},
			{Key: "", Description: "补上注册", Priority: "high", DependsOn: []string{"t1", "t1"}},
			{Key: "t4", Description: "", Priority: "low"},
			{Key: "t5", Description: "第三条", Priority: "urgent"},
			{Key: "t6", Description: "第四条", Priority: "normal"},
			{Key: "t7", Description: "第五条", Priority: "low"},
			{Key: "t8", Description: "第六条", Priority: "low"},
			{Key: "t9", Description: "超出上限的第七条", Priority: "low"},
		},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if draft.Mode != "split" || draft.Understood != "整理登录" {
		t.Fatalf("draft header: %+v", draft)
	}
	if len(draft.Assumptions) != 1 || draft.Assumptions[0] != "沿用现有会话接口" {
		t.Fatalf("assumptions: %#v", draft.Assumptions)
	}
	if len(draft.Tasks) != taskDraftMaxTasks {
		t.Fatalf("task count: got %d", len(draft.Tasks))
	}
	first := draft.Tasks[0]
	if first.Priority != "normal" || len([]rune(first.Description)) != taskDescriptionRuneMax || len(first.DependsOn) != 0 {
		t.Fatalf("first task: %+v len=%d", first, len([]rune(first.Description)))
	}
	second := draft.Tasks[1]
	if second.Key == "" || second.Priority != "high" || len(second.DependsOn) != 1 || second.DependsOn[0] != "t1" {
		t.Fatalf("second task: %+v", second)
	}
	if draft.Tasks[len(draft.Tasks)-1].Description == "超出上限的第七条" {
		t.Fatal("seventh task was kept")
	}
}

func TestNormalizeTaskDraftQuestionsClearTasks(t *testing.T) {
	draft, err := normalizeTaskDraft(taskDraft{
		Mode:      "split",
		Questions: []string{"要改哪一个登录入口？"},
		Tasks:     []taskDraftItem{{Key: "t1", Description: "不该留下", Priority: "normal"}},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if draft.Mode != "single" || len(draft.Tasks) != 0 || len(draft.Questions) != 1 {
		t.Fatalf("questions draft: %+v", draft)
	}
}

func TestNormalizeTaskDraftSingleDropsDependencies(t *testing.T) {
	draft, err := normalizeTaskDraft(taskDraft{
		Mode:  "split",
		Tasks: []taskDraftItem{{Key: "t1", Description: "只做这一件", Priority: "low", DependsOn: []string{"t0"}}},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if draft.Mode != "single" || draft.Tasks[0].Priority != "low" || len(draft.Tasks[0].DependsOn) != 0 {
		t.Fatalf("single draft: %+v", draft)
	}
}

func TestNormalizeTaskDraftRejectsEmptyOutput(t *testing.T) {
	if _, err := normalizeTaskDraft(taskDraft{Tasks: []taskDraftItem{{Description: "  "}}}); err == nil {
		t.Fatal("empty draft was accepted")
	}
}

func TestDraftTaskWithoutAgent(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", strings.NewReader(`{"text":"做一个登录"}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "当前项目没有可用的 AI 配置") {
		t.Fatalf("body=%s", response.Body.String())
	}
}

func TestCreateTaskBatchKeepsOrderDependenciesAndTodo(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	handler := server.routes()
	body := `{"tasks":[
		{"key":"t1","description":"先补会话校验","priority":"high","dependsOn":[]},
		{"key":"t2","description":"再补登录页","priority":"normal","dependsOn":["t1"]}
	]}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/batch", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(created.Tasks) != 2 {
		t.Fatalf("created: %+v", created.Tasks)
	}
	if created.Tasks[0].Status != taskTodo || created.Tasks[1].Status != taskTodo {
		t.Fatalf("status: %+v", created.Tasks)
	}
	if created.Tasks[0].Position >= created.Tasks[1].Position {
		t.Fatalf("positions: %v then %v", created.Tasks[0].Position, created.Tasks[1].Position)
	}
	detail := fetchTaskDetailForTest(t, handler, created.Tasks[1].ID)
	assertTaskDependencyOrder(t, detail, []string{created.Tasks[0].ID})
	if detail.Status != taskTodo {
		t.Fatalf("stored status: %s", detail.Status)
	}
}

func TestCreateTaskBatchRejectsBadDependencyWithoutWriting(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	handler := server.routes()
	body := `{"tasks":[
		{"key":"t1","description":"第一条","priority":"normal","dependsOn":["t2"]},
		{"key":"t2","description":"第二条","priority":"normal","dependsOn":[]}
	]}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/batch", strings.NewReader(body)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if tasks := fetchTasksForTest(t, handler, "/api/projects/"+projectID+"/tasks"); len(tasks) != 0 {
		t.Fatalf("invalid batch wrote tasks: %+v", tasks)
	}
}

func TestInsertTodoTaskRollsBackWhenALaterTaskFails(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	tx, err := server.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	now := time.Now().UTC()
	position := 0.0
	if _, err := server.insertTodoTaskTx(context.Background(), tx, projectID, taskInput{Description: "第一条", Priority: "normal", Position: &position}, now); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	missing := []string{"does-not-exist"}
	_, err = server.insertTodoTaskTx(context.Background(), tx, projectID, taskInput{Description: "第二条", Priority: "normal", Position: &position, PredecessorTaskIDs: &missing}, now)
	if err == nil {
		t.Fatal("missing predecessor was accepted")
	}
	var write *taskWriteError
	if !errors.As(err, &write) || write.status != http.StatusBadRequest {
		t.Fatalf("error: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if tasks := fetchTasksForTest(t, server.routes(), "/api/projects/"+projectID+"/tasks"); len(tasks) != 0 {
		t.Fatalf("rolled back task remained: %+v", tasks)
	}
}

func TestDraftTaskRejectsEmptyText(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", bytes.NewBufferString(`{"text":"  "}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestNormalizeTaskDraftOutputUsesTheLastJSONObject(t *testing.T) {
	raw := "先看代码。{\"tasks\":[{\"key\":\"t0\",\"description\":\"这是草稿\",\"priority\":\"low\",\"dependsOn\":[]}]}\n```json\n" +
		`{"understood":"补登录","mode":"single","assumptions":["沿用会话"],"questions":[],"tasks":[{"key":"t1","description":"在现有登录接口补过期校验。","priority":"high","dependsOn":["t9"]}]}` +
		"\n```"
	draft, err := normalizeTaskDraftOutput(raw)
	if err != nil {
		t.Fatalf("normalize output: %v", err)
	}
	if draft.Mode != "single" || len(draft.Tasks) != 1 || draft.Tasks[0].Description != "在现有登录接口补过期校验。" || len(draft.Tasks[0].DependsOn) != 0 {
		t.Fatalf("draft: %+v", draft)
	}
	if _, err := normalizeTaskDraftOutput("这里没有任务草稿"); err == nil {
		t.Fatal("prose was accepted as a draft")
	}
}

func TestNormalizeTaskDraftCapsNotesAndDropsCollidingKeys(t *testing.T) {
	long := strings.Repeat("注", taskDraftNoteLimit+9)
	notes := make([]string, taskDraftNoteMax+1)
	for index := range notes {
		notes[index] = long
	}
	draft, err := normalizeTaskDraft(taskDraft{
		Questions: notes,
		Tasks:     []taskDraftItem{{Key: "t1", Description: "不该留下", Priority: "normal"}},
	})
	if err != nil {
		t.Fatalf("questions: %v", err)
	}
	if len(draft.Questions) != taskDraftNoteMax || len([]rune(draft.Questions[0])) != taskDraftNoteLimit || len(draft.Tasks) != 0 {
		t.Fatalf("question cap: %+v", draft)
	}

	draft, err = normalizeTaskDraft(taskDraft{Tasks: []taskDraftItem{
		{Key: "", Description: "第一件", Priority: "normal"},
		{Key: "t1", Description: "和空编号撞车，应丢掉", Priority: "high"},
		{Key: "t2", Description: "第二件", Priority: "low", DependsOn: []string{" t1 ", "t1", "t2", "missing"}},
	}})
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(draft.Tasks) != 2 || draft.Tasks[0].Key != "t1" || draft.Tasks[0].Description != "第一件" {
		t.Fatalf("kept tasks: %+v", draft.Tasks)
	}
	if len(draft.Tasks[1].DependsOn) != 1 || draft.Tasks[1].DependsOn[0] != "t1" {
		t.Fatalf("dependencies: %+v", draft.Tasks[1])
	}
}

func TestTaskDraftRequestStaysOnTheDraftPost(t *testing.T) {
	draft := httptest.NewRequest(http.MethodPost, "/api/projects/p1/tasks/draft", nil)
	batch := httptest.NewRequest(http.MethodPost, "/api/projects/p1/tasks/batch", nil)
	read := httptest.NewRequest(http.MethodGet, "/api/projects/p1/tasks/draft", nil)
	if !taskDraftRequest(draft) || taskDraftRequest(batch) || taskDraftRequest(read) {
		t.Fatal("task draft detection matched the wrong request")
	}
}

func TestDraftTaskRejectsOverlongTextAndUnknownProject(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	handler := server.routes()
	limit := httptest.NewRecorder()
	handler.ServeHTTP(limit, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", strings.NewReader(`{"text":"`+strings.Repeat("字", taskDescriptionRuneMax)+`"}`)))
	if limit.Code != http.StatusConflict {
		t.Fatalf("text at the limit should reach agent selection, status=%d body=%s", limit.Code, limit.Body.String())
	}
	over := httptest.NewRecorder()
	handler.ServeHTTP(over, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", strings.NewReader(`{"text":"`+strings.Repeat("字", taskDescriptionRuneMax+1)+`"}`)))
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), "任务说明超过长度限制") {
		t.Fatalf("overlong: status=%d body=%s", over.Code, over.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/projects/missing/tasks/draft", strings.NewReader(`{"text":"做一个登录"}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing project: status=%d body=%s", missing.Code, missing.Body.String())
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", strings.NewReader(`{`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestTaskDraftAgentPrefersCodexThenClaude(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	load := func() Project {
		t.Helper()
		project, err := server.getProjectByID(context.Background(), projectID)
		if err != nil {
			t.Fatalf("load project: %v", err)
		}
		return project
	}
	if _, err := server.taskDraftAgent(context.Background(), load()); !errors.Is(err, errTaskDraftNoAgent) {
		t.Fatalf("project without a profile: %v", err)
	}
	pinDraftProfile(t, server, projectID, "claude-code")
	if agent, err := server.taskDraftAgent(context.Background(), load()); err != nil || agent != "claude-code" {
		t.Fatalf("claude profile: agent=%s err=%v", agent, err)
	}
	pinDraftProfile(t, server, projectID, "codex")
	if agent, err := server.taskDraftAgent(context.Background(), load()); err != nil || agent != "codex" {
		t.Fatalf("codex should win when both profiles exist: agent=%s err=%v", agent, err)
	}
}

func TestDraftTaskNormalizesModelOutputWithoutCreatingTasks(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	runner := pinDraftProfile(t, server, projectID, "codex")
	runner.outputs = []string{"我先看了仓库。\n" + `{"understood":"补登录","mode":"split","assumptions":["沿用现有会话"],"questions":[],"tasks":[{"key":"t1","description":"在现有登录接口补过期校验。","priority":"high","dependsOn":["t2"]}]}`}
	response := postDraftForTest(t, server, projectID, "登录过期没拦")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var draft taskDraft
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if draft.Mode != "single" || len(draft.Tasks) != 1 || draft.Tasks[0].Priority != "high" || len(draft.Tasks[0].DependsOn) != 0 {
		t.Fatalf("normalized draft: %+v", draft)
	}
	requests := runner.capturedRequests()
	if len(requests) != 1 {
		t.Fatalf("runner calls: %d", len(requests))
	}
	if !strings.Contains(requests[0].Prompt, "登录过期没拦") || requests[0].PermissionMode != "read_only" || string(requests[0].OutputSchema) != string(taskDraftOutputSchema) {
		t.Fatalf("runner request: agent=%s mode=%s prompt=%q", requests[0].AgentID, requests[0].PermissionMode, requests[0].Prompt)
	}
	if tasks := fetchTasksForTest(t, server.routes(), "/api/projects/"+projectID+"/tasks"); len(tasks) != 0 {
		t.Fatalf("draft wrote tasks: %+v", tasks)
	}
}

func TestDraftTaskQuestionsDoNotCreateTasks(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	runner := pinDraftProfile(t, server, projectID, "codex")
	runner.outputs = []string{`{"understood":"登录","mode":"split","assumptions":["可能有两个入口"],"questions":["要改哪一个入口？"],"tasks":[{"key":"t1","description":"不该留下","priority":"normal","dependsOn":[]}]}`}
	response := postDraftForTest(t, server, projectID, "登录有问题")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var draft taskDraft
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if draft.Mode != "single" || len(draft.Tasks) != 0 || len(draft.Questions) != 1 || len(draft.Assumptions) != 1 {
		t.Fatalf("questions draft: %+v", draft)
	}
}

func TestDraftTaskUsesClaudeWhenCodexHasNoProfile(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	runner := pinDraftProfile(t, server, projectID, "claude-code")
	runner.outputs = []string{`{"understood":"补一页","mode":"single","assumptions":[],"questions":[],"tasks":[{"key":"t1","description":"补上注册页的校验。","priority":"normal","dependsOn":[]}]}`}
	response := postDraftForTest(t, server, projectID, "注册页")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests := runner.capturedRequests()
	if len(requests) != 1 || requests[0].AgentID != "claude-code" || requests[0].PermissionMode != "plan" {
		t.Fatalf("claude request: %+v", requests)
	}
}

func TestDraftTaskDoesNotFallBackWhenTheFirstAgentFails(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	pinDraftProfile(t, server, projectID, "codex")
	claudeCalls := 0
	server.codexRunner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error {
		return errors.New("codex exploded")
	})
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		claudeCalls++
		sink.AssistantText(`{"understood":"","mode":"single","assumptions":[],"questions":[],"tasks":[{"key":"t1","description":"不该改由 Claude 完成。","priority":"normal","dependsOn":[]}]}`, "")
		return nil
	})
	response := postDraftForTest(t, server, projectID, "做一件事")
	if response.Code != http.StatusBadGateway || claudeCalls != 0 {
		t.Fatalf("status=%d claudeCalls=%d body=%s", response.Code, claudeCalls, response.Body.String())
	}
}

func TestDraftTaskMapsRunFailures(t *testing.T) {
	tests := []struct {
		name    string
		cause   error
		status  int
		snippet string
	}{
		{name: "workspace", cause: &projectWorkspaceOccupiedError{owner: "git"}, status: http.StatusConflict, snippet: "项目工作区正被其他"},
		{name: "quota", cause: &quotaAdmissionError{retryAfter: time.Second}, status: http.StatusConflict, snippet: "凭据额度正被占用"},
		{name: "deadline", cause: context.DeadlineExceeded, status: http.StatusGatewayTimeout, snippet: "操作超时"},
		{name: "other", cause: errors.New("codex exploded"), status: http.StatusBadGateway, snippet: "codex exploded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			projectID := insightTestProject(t, server)
			pinDraftProfile(t, server, projectID, "codex")
			server.codexRunner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error {
				return test.cause
			})
			response := postDraftForTest(t, server, projectID, "做一件事")
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.snippet) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tasks := fetchTasksForTest(t, server.routes(), "/api/projects/"+projectID+"/tasks"); len(tasks) != 0 {
				t.Fatalf("failed draft wrote tasks: %+v", tasks)
			}
		})
	}
}

func TestDraftTaskUnreadableOutputDoesNotCreateTasks(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	runner := pinDraftProfile(t, server, projectID, "codex")
	runner.outputs = []string{"我不会返回 JSON"}
	response := postDraftForTest(t, server, projectID, "做一件事")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "AI 没有返回可用的任务草稿") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDraftTaskCancelStopsTheAgent(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	pinDraftProfile(t, server, projectID, "codex")
	started := make(chan struct{})
	server.codexRunner = runnerFunc(func(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", strings.NewReader(`{"text":"做一件事"}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("draft never reached the agent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the draft")
	}
	if response.Body.Len() != 0 {
		t.Fatalf("canceled draft wrote a body: %s", response.Body.String())
	}
	if tasks := fetchTasksForTest(t, server.routes(), "/api/projects/"+projectID+"/tasks"); len(tasks) != 0 {
		t.Fatalf("canceled draft wrote tasks: %+v", tasks)
	}
}

func TestCreateTaskBatchValidationRejectsWithoutWriting(t *testing.T) {
	long := strings.Repeat("字", taskDescriptionRuneMax+1)
	tests := []struct {
		name    string
		body    string
		snippet string
	}{
		{name: "empty", body: `{"tasks":[]}`, snippet: "至少需要一条任务"},
		{name: "blank", body: `{"tasks":[{"key":"t1","description":"  ","priority":"normal","dependsOn":[]}]}`, snippet: "任务说明不能为空"},
		{name: "overlong", body: `{"tasks":[{"key":"t1","description":"` + long + `","priority":"normal","dependsOn":[]}]}`, snippet: "任务说明超过长度限制"},
		{name: "duplicate", body: `{"tasks":[{"key":"t1","description":"甲","priority":"normal","dependsOn":[]},{"key":"t1","description":"乙","priority":"normal","dependsOn":[]}]}`, snippet: "任务编号重复"},
		{name: "priority", body: `{"tasks":[{"key":"t1","description":"甲","priority":"now","dependsOn":[]}]}`, snippet: "不支持的优先级"},
		{name: "self", body: `{"tasks":[{"key":"t1","description":"甲","priority":"normal","dependsOn":["t1"]}]}`, snippet: "任务依赖只能指向"},
		{name: "unknown", body: `{"tasks":[{"key":"t1","description":"甲","priority":"normal","dependsOn":["existing"]}]}`, snippet: "任务依赖只能指向"},
		{name: "too many", body: batchBodyForTest(taskDraftMaxTasks + 1), snippet: "一次最多创建 6 条任务"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, projectID, _ := seedTaskConversation(t)
			createTaskForTest(t, server.routes(), projectID, "已有任务")
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/batch", strings.NewReader(test.body)))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.snippet) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tasks := fetchTasksForTest(t, server.routes(), "/api/projects/"+projectID+"/tasks"); len(tasks) != 1 {
				t.Fatalf("rejected batch changed the list: %+v", tasks)
			}
		})
	}
}

func TestCreateTaskBatchTrimsFillsAndDedupesWithoutDispatching(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	body := `{"tasks":[
		{"key":"","description":"  先补会话校验。  ","priority":"","dependsOn":[]},
		{"key":"t2","description":"再补登录页。","priority":"low","dependsOn":["","t1","t1"]}
	]}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/batch", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v body=%s", err, response.Body.String())
	}
	if len(created.Tasks) != 2 || created.Tasks[0].Description != "先补会话校验。" || created.Tasks[0].Priority != "normal" || created.Tasks[0].Title != "先补会话校验" {
		t.Fatalf("first task: %+v", created.Tasks)
	}
	detail := fetchTaskDetailForTest(t, server.routes(), created.Tasks[1].ID)
	assertTaskDependencyOrder(t, detail, []string{created.Tasks[0].ID})
	var jobs int
	if err := server.db.QueryRow(`select count(*) from task_orchestration_jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("batch dispatched %d orchestration jobs", jobs)
	}
}

func TestCreateTaskBatchKeepsAChainOfSix(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/batch", strings.NewReader(batchBodyForTest(taskDraftMaxTasks))))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(created.Tasks) != taskDraftMaxTasks {
		t.Fatalf("created %d", len(created.Tasks))
	}
	for index, task := range created.Tasks {
		if task.Status != taskTodo {
			t.Fatalf("task %d status: %s", index, task.Status)
		}
		if index > 0 && task.Position <= created.Tasks[index-1].Position {
			t.Fatalf("positions did not increase at %d", index)
		}
		detail := fetchTaskDetailForTest(t, server.routes(), task.ID)
		if index == 0 {
			assertTaskDependencyOrder(t, detail, nil)
			continue
		}
		assertTaskDependencyOrder(t, detail, []string{created.Tasks[index-1].ID})
	}
}

func TestCreateTaskBatchMissingProject(t *testing.T) {
	server, _, _ := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/missing/tasks/batch", strings.NewReader(`{"tasks":[{"key":"t1","description":"甲","priority":"normal","dependsOn":[]}]}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func pinDraftProfile(t *testing.T, server *Server, projectID, agentID string) *insightScriptRunner {
	t.Helper()
	if _, err := server.db.Exec(`update projects set runner=?,runner_id=? where id=?`, server.localRunnerID(), server.localRunnerID(), projectID); err != nil {
		t.Fatalf("set local runner: %v", err)
	}
	profile := createCLIManagedProfile(t, server, agentID, agentID+"-model")
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/agent-profile", strings.NewReader(`{"agentId":"`+agentID+`","profileId":"`+profile.ID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("pin %s profile: status=%d body=%s", agentID, set.Code, set.Body.String())
	}
	runner := &insightScriptRunner{}
	switch agentID {
	case "codex":
		server.codexRunner = runner
	case "claude-code":
		server.runner = runner
	default:
		t.Fatalf("unsupported draft agent %s", agentID)
	}
	return runner
}

func postDraftForTest(t *testing.T, server *Server, projectID, text string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/draft", bytes.NewReader(body)))
	return response
}

func batchBodyForTest(count int) string {
	tasks := make([]taskDraftItem, count)
	for index := range tasks {
		item := taskDraftItem{Key: fmt.Sprintf("t%d", index+1), Description: fmt.Sprintf("第%d件可以单独交付的工作。", index+1), Priority: "normal"}
		if index > 0 {
			item.DependsOn = []string{fmt.Sprintf("t%d", index)}
		}
		tasks[index] = item
	}
	body, err := json.Marshal(taskBatchRequest{Tasks: tasks})
	if err != nil {
		panic(err)
	}
	return string(body)
}
