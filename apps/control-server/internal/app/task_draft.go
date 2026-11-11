package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	taskDraftTimeout       = 90 * time.Second
	taskDraftMaxTasks      = 6
	taskDraftNoteLimit     = 500
	taskDraftNoteMax       = 8
	taskDescriptionRuneMax = 12000
)

// errTaskDraftNoAgent 是项目没有可用来读代码的运行档案。标题润色同样跳过这种项目。
var errTaskDraftNoAgent = errors.New("当前项目没有可用的 AI 配置，无法起草任务。")

var taskDraftOutputSchema = json.RawMessage(`{"type":"object","properties":{"understood":{"type":"string"},"mode":{"type":"string"},"assumptions":{"type":"array","items":{"type":"string"}},"questions":{"type":"array","items":{"type":"string"}},"tasks":{"type":"array","items":{"type":"object","properties":{"key":{"type":"string"},"description":{"type":"string"},"priority":{"type":"string"},"dependsOn":{"type":"array","items":{"type":"string"}}},"required":["key","description","priority","dependsOn"],"additionalProperties":false}}},"required":["understood","mode","assumptions","questions","tasks"],"additionalProperties":false}`)

// taskDraftRequest 是「起草任务」：只读分析，用户取消时必须停掉 Agent。
// 它不写任务，所以不能套用写操作的 detachWriteContext。
func taskDraftRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks/draft")
}

type taskDraftItem struct {
	Key         string   `json:"key"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	DependsOn   []string `json:"dependsOn"`
}

type taskDraft struct {
	Understood  string          `json:"understood"`
	Mode        string          `json:"mode"`
	Assumptions []string        `json:"assumptions"`
	Questions   []string        `json:"questions"`
	Tasks       []taskDraftItem `json:"tasks"`
}

func taskDraftPrompt(text string) string {
	return "你是 Milevia 的任务起草助手。先阅读当前项目代码，再理解用户输入，起草成可以直接下发给编码 Agent 的任务说明。只返回 JSON，不要写解释。\n\n规则：\n- 每条 description 必须自包含：目标、允许改动的范围、限制、怎样算做完。下发时只发送 description，不要把关键信息只写在标题里。\n- 用户输入只是一件可交付的工作时，mode 为 single，tasks 只放一条，dependsOn 为空。\n- 只有输入里确实包含可以分开交付的多件工作时才拆开，mode 为 split，最多 6 条。不要把一件工作拆成调研、实现、测试这种固定套话。\n- dependsOn 只填写必须先完成的任务 key，并且只能指向更早的 key。没有先后关系就留空。\n- 不要发明仓库里看不到的接口、文件或行为。拿不准的写成 assumptions；缺了就无法开工的写成 questions，此时 tasks 为空数组。\n- priority 只能是 urgent、high、normal、low。\n- key 使用 t1、t2 这种编号。\n\n用户输入：\n" + text
}

func (s *Server) taskDraftAgent(ctx context.Context, project Project) (string, error) {
	var failure error
	for _, agentID := range []string{"codex", "claude-code"} {
		profile, err := s.projectRuntimeProfile(ctx, project, agentID)
		if err != nil {
			failure = err
			continue
		}
		if profile == nil {
			continue
		}
		return agentID, nil
	}
	if failure != nil {
		return "", failure
	}
	return "", errTaskDraftNoAgent
}

func (s *Server) draftTask(w http.ResponseWriter, r *http.Request) {
	project, err := s.getProjectByID(r.Context(), chi.URLParam(r, "projectID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var input struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &input) {
		return
	}
	text := strings.TrimSpace(input.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, errors.New("请先输入要整理的内容。"))
		return
	}
	if len([]rune(text)) > taskDescriptionRuneMax {
		writeError(w, http.StatusBadRequest, errors.New("任务说明超过长度限制。"))
		return
	}
	agentID, err := s.taskDraftAgent(r.Context(), project)
	if errors.Is(err, errTaskDraftNoAgent) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), taskDraftTimeout)
	defer cancel()
	output, err := s.runReadOnlyAgentWithSchema(ctx, project, agentID, taskDraftPrompt(text), taskDraftOutputSchema, 0, nil)
	if err != nil {
		writeTaskDraftRunError(w, err)
		return
	}
	draft, err := normalizeTaskDraftOutput(output)
	if err != nil {
		writeError(w, http.StatusBadGateway, errors.New("AI 没有返回可用的任务草稿，请再试一次。"))
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func writeTaskDraftRunError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var occupied *projectWorkspaceOccupiedError
	if errors.As(err, &occupied) {
		writeError(w, http.StatusConflict, err)
		return
	}
	var quota *quotaAdmissionError
	if errors.As(err, &quota) {
		writeError(w, http.StatusConflict, errors.New("凭据额度正被占用，请稍后重试。"))
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, err)
		return
	}
	writeError(w, http.StatusBadGateway, err)
}

func normalizeTaskDraftOutput(raw string) (taskDraft, error) {
	var parsed taskDraft
	if json.Unmarshal([]byte(extractInsightJSON(raw)), &parsed) != nil {
		return taskDraft{}, errors.New("unreadable task draft")
	}
	return normalizeTaskDraft(parsed)
}

func normalizeTaskDraft(parsed taskDraft) (taskDraft, error) {
	questions := compactDraftNotes(parsed.Questions)
	assumptions := compactDraftNotes(parsed.Assumptions)
	understood := truncateRunes(strings.TrimSpace(parsed.Understood), taskDraftNoteLimit)
	if len(questions) > 0 {
		return taskDraft{Understood: understood, Mode: "single", Assumptions: assumptions, Questions: questions, Tasks: []taskDraftItem{}}, nil
	}
	tasks, err := correctDraftTasks(parsed.Tasks)
	if err != nil {
		return taskDraft{}, err
	}
	mode := "single"
	if len(tasks) >= 2 {
		mode = "split"
	} else if len(tasks) == 1 {
		tasks[0].DependsOn = []string{}
	}
	return taskDraft{Understood: understood, Mode: mode, Assumptions: assumptions, Questions: []string{}, Tasks: tasks}, nil
}

func correctDraftTasks(items []taskDraftItem) ([]taskDraftItem, error) {
	kept := make([]taskDraftItem, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		description := strings.TrimSpace(item.Description)
		if description == "" {
			continue
		}
		description = truncateRunes(description, taskDescriptionRuneMax)
		key := strings.TrimSpace(item.Key)
		if key == "" {
			key = fmt.Sprintf("t%d", index+1)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		priority := strings.TrimSpace(item.Priority)
		if !validTaskPriority(priority) {
			priority = "normal"
		}
		kept = append(kept, taskDraftItem{Key: key, Description: description, Priority: priority, DependsOn: append([]string{}, item.DependsOn...)})
		if len(kept) == taskDraftMaxTasks {
			break
		}
	}
	if len(kept) == 0 {
		return nil, errors.New("empty task draft")
	}
	indexOf := make(map[string]int, len(kept))
	for index, item := range kept {
		indexOf[item.Key] = index
	}
	for index := range kept {
		kept[index].DependsOn = earlierDraftDeps(kept[index].DependsOn, index, indexOf)
	}
	return kept, nil
}

func earlierDraftDeps(deps []string, index int, indexOf map[string]int) []string {
	if index == 0 {
		return []string{}
	}
	seen := map[string]struct{}{}
	ordered := make([]string, 0, len(deps))
	for _, dep := range deps {
		dep = strings.TrimSpace(dep)
		depIndex, ok := indexOf[dep]
		if !ok || depIndex >= index {
			continue
		}
		if _, exists := seen[dep]; exists {
			continue
		}
		seen[dep] = struct{}{}
		ordered = append(ordered, dep)
	}
	return ordered
}

func compactDraftNotes(values []string) []string {
	notes := make([]string, 0, len(values))
	for _, value := range values {
		value = truncateRunes(strings.TrimSpace(value), taskDraftNoteLimit)
		if value == "" {
			continue
		}
		notes = append(notes, value)
		if len(notes) == taskDraftNoteMax {
			break
		}
	}
	return notes
}

type taskBatchRequest struct {
	Tasks []taskDraftItem `json:"tasks"`
}

func (s *Server) createTaskBatch(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	var input taskBatchRequest
	if !decode(w, r, &input) {
		return
	}
	items, err := validateConfirmedTasks(input.Tasks)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	created := make([]Task, 0, len(items))
	idByKey := map[string]string{}
	for _, item := range items {
		var predecessors *[]string
		if len(item.DependsOn) > 0 {
			ids := make([]string, 0, len(item.DependsOn))
			for _, key := range item.DependsOn {
				id, ok := idByKey[key]
				if !ok {
					writeError(w, http.StatusBadRequest, errors.New("任务依赖只能指向本次创建中更早的任务。"))
					return
				}
				ids = append(ids, id)
			}
			predecessors = &ids
		}
		position := 0.0
		task, err := s.insertTodoTaskTx(r.Context(), tx, projectID, taskInput{Description: item.Description, Priority: item.Priority, Position: &position, PredecessorTaskIDs: predecessors}, now)
		if err != nil {
			writeTaskInsertError(w, err)
			return
		}
		idByKey[item.Key] = task.ID
		created = append(created, task)
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, task := range created {
		s.refineTaskTitleAsync(task.ProjectID, task.ID, task.Description)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tasks": created})
}

func validateConfirmedTasks(items []taskDraftItem) ([]taskDraftItem, error) {
	if len(items) == 0 {
		return nil, errors.New("至少需要一条任务。")
	}
	if len(items) > taskDraftMaxTasks {
		return nil, errors.New("一次最多创建 6 条任务。")
	}
	kept := make([]taskDraftItem, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		description := strings.TrimSpace(item.Description)
		if description == "" {
			return nil, errors.New("任务说明不能为空。")
		}
		if len([]rune(description)) > taskDescriptionRuneMax {
			return nil, errors.New("任务说明超过长度限制。")
		}
		key := strings.TrimSpace(item.Key)
		if key == "" {
			key = fmt.Sprintf("t%d", index+1)
		}
		if _, exists := seen[key]; exists {
			return nil, errors.New("任务编号重复。")
		}
		seen[key] = struct{}{}
		priority := strings.TrimSpace(item.Priority)
		if priority == "" {
			priority = "normal"
		}
		if !validTaskPriority(priority) {
			return nil, errors.New("不支持的优先级。")
		}
		kept = append(kept, taskDraftItem{Key: key, Description: description, Priority: priority, DependsOn: append([]string{}, item.DependsOn...)})
	}
	indexOf := make(map[string]int, len(kept))
	for index, item := range kept {
		indexOf[item.Key] = index
	}
	for index := range kept {
		deps, err := confirmedDraftDeps(kept[index].DependsOn, index, indexOf)
		if err != nil {
			return nil, err
		}
		kept[index].DependsOn = deps
	}
	return kept, nil
}

func confirmedDraftDeps(deps []string, index int, indexOf map[string]int) ([]string, error) {
	seen := map[string]struct{}{}
	ordered := make([]string, 0, len(deps))
	for _, dep := range deps {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		depIndex, ok := indexOf[dep]
		if !ok || depIndex >= index {
			return nil, errors.New("任务依赖只能指向本次创建中更早的任务。")
		}
		if _, exists := seen[dep]; exists {
			continue
		}
		seen[dep] = struct{}{}
		ordered = append(ordered, dep)
	}
	return ordered, nil
}
