package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// AI 辅助冲突解决：把单个冲突文件的 base/ours/theirs/working 交给只读 agent，
// 生成整文件合并建议，用户审阅后接受（复用 conflicts/resolve action=working）。
//
// agent 严格只读（claude plan / codex read_only），不写工作区；建议结果暂存于
// 内存注册表，由前端轮询 GET 获取。接受动作仍走既有 stateToken 写路径，天然审计。

const (
	gitConflictSuggestStatusRunning   = "running"
	gitConflictSuggestStatusCompleted = "completed"
	gitConflictSuggestStatusFailed    = "failed"
	gitConflictSuggestStatusCancelled = "cancelled"
	// 单文件三方+工作区文本总量预算，避免把超大文件塞进模型上下文。
	gitConflictSuggestBudget = 120 * 1024
)

type gitConflictSuggestion struct {
	ID           string
	ProjectID    string
	Path         string
	Agent        string
	Status       string
	ErrorMessage string
	Merged       string
	Explanation  string
	RequestedAt  time.Time
	StartedAt    time.Time
	FinishedAt   *time.Time
	cancel       context.CancelFunc
}

func (suggestion *gitConflictSuggestion) response() map[string]any {
	result := map[string]any{
		"id":          suggestion.ID,
		"projectId":   suggestion.ProjectID,
		"path":        suggestion.Path,
		"agent":       suggestion.Agent,
		"status":      suggestion.Status,
		"requestedAt": suggestion.RequestedAt,
	}
	if !suggestion.StartedAt.IsZero() {
		result["startedAt"] = suggestion.StartedAt
	}
	if suggestion.FinishedAt != nil {
		result["finishedAt"] = *suggestion.FinishedAt
	}
	switch suggestion.Status {
	case gitConflictSuggestStatusCompleted:
		result["merged"] = suggestion.Merged
		result["explanation"] = suggestion.Explanation
	case gitConflictSuggestStatusFailed, gitConflictSuggestStatusCancelled:
		result["error"] = suggestion.ErrorMessage
	}
	return result
}

func (s *Server) copyConflictSuggestion(id string) (*gitConflictSuggestion, bool) {
	s.conflictSuggestMu.Lock()
	defer s.conflictSuggestMu.Unlock()
	record, found := s.conflictSuggestions[id]
	if !found {
		return nil, false
	}
	copy := *record
	copy.cancel = nil
	return &copy, true
}

// pruneConflictSuggestionsLocked 删除早已终止的建议记录，避免内存无限增长。
func (s *Server) pruneConflictSuggestionsLocked(now time.Time) {
	for id, record := range s.conflictSuggestions {
		if record.Status == gitConflictSuggestStatusRunning {
			continue
		}
		if record.FinishedAt != nil && now.Sub(*record.FinishedAt) > 10*time.Minute {
			delete(s.conflictSuggestions, id)
		}
	}
}

// conflictSuggestionOutputSchema 约束模型返回 { merged, explanation }。
var conflictSuggestionOutputSchema = json.RawMessage(`{"type":"object","properties":{"merged":{"type":"string"},"explanation":{"type":"string"}},"required":["merged"],"additionalProperties":false}`)

// buildConflictSuggestionPrompt 把三方内容与当前工作区拼接成稳定、可解析的提示词。
func buildConflictSuggestionPrompt(path, oursLabel, theirsLabel string, detail GitConflictContent) string {
	var builder strings.Builder
	builder.WriteString("请帮助解决以下 Git 冲突。输出必须是合法 JSON，不要包含任何 Markdown 代码块围栏或多余文字。\n\n")
	fmt.Fprintf(&builder, "冲突文件：%s\n", path)
	fmt.Fprintf(&builder, "当前侧（ours）：%s\n传入侧（theirs）：%s\n\n", oursLabel, theirsLabel)
	builder.WriteString("下面给出共同祖先 base、当前侧 ours、传入侧 theirs，以及 git 自动合并后、仍带冲突标记的工作区文件 working。\n")
	builder.WriteString("请以 working 为模板：保留其中所有无冲突内容与行，逐个解决 `<<<<<<<`/`=======`/`>>>>>>>` 标记之间的冲突块，产出完整、可直接落盘的 resolved 文件内容。\n")
	builder.WriteString("若某处明显应取某一侧或两侧合并取语义正确者，请自行判断；不得在结果里残留任何冲突标记行。\n\n")

	fmt.Fprintf(&builder, "--- base（共同祖先）开始 ---\n%s\n--- base 结束 ---\n", detail.Base)
	fmt.Fprintf(&builder, "\n--- ours（%s）开始 ---\n%s\n--- ours 结束 ---\n", oursLabel, detail.Ours)
	fmt.Fprintf(&builder, "\n--- theirs（%s）开始 ---\n%s\n--- theirs 结束 ---\n", theirsLabel, detail.Theirs)
	fmt.Fprintf(&builder, "\n--- working（自动合并 + 冲突标记）开始 ---\n%s\n--- working 结束 ---\n", detail.Working)

	builder.WriteString("\n请返回 JSON：{\"merged\": \"<完整解决后的文件内容>\", \"explanation\": \"<简短说明如何取舍>\"}\n")
	return builder.String()
}

// parseConflictSuggestionJSON 从模型返回文本中抽取最外层 JSON 对象并解析。
func parseConflictSuggestionJSON(raw string) (merged, explanation string, err error) {
	text := strings.TrimSpace(raw)
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return "", "", errors.New("AI 没有返回 JSON 结果")
	}
	var payload struct {
		Merged      string `json:"merged"`
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &payload); err != nil {
		return "", "", fmt.Errorf("解析 AI 返回的 JSON: %w", err)
	}
	if strings.Contains(payload.Merged, "<<<<<<<") || strings.Contains(payload.Merged, ">>>>>>>") {
		return "", "", errors.New("AI 建议中仍包含冲突标记，已丢弃；请重试或手动解决")
	}
	return payload.Merged, payload.Explanation, nil
}

// conflictSuggestionContentBudget 判断是否超过可送模型的体积预算。
func conflictSuggestionContentBudget(detail GitConflictContent) int {
	return len(detail.Base) + len(detail.Ours) + len(detail.Theirs) + len(detail.Working)
}

// gitConflictSuggest 启动一次 AI 冲突建议（202 立返，后台 goroutine 跑只读 agent）。
func (s *Server) gitConflictSuggest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path  string `json:"path"`
		Agent string `json:"agent"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := validateGitPath(input.Path); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.Agent == "" {
		input.Agent = "claude-code"
	}
	if !validProfileAgent(input.Agent) {
		writeError(w, http.StatusBadRequest, errors.New("unsupported agent"))
		return
	}
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return
	}
	detail, err := runner.ConflictContent(r.Context(), repo, input.Path)
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("Git path is not currently in conflict"))
		return
	}
	if detail.Binary || detail.Oversized || detail.Kind != "content" && detail.Kind != "add-add" {
		writeError(w, http.StatusBadRequest, errors.New("该冲突类型暂不支持 AI 建议，请用整文件操作"))
		return
	}
	if detail.OursDeleted || detail.TheirsDeleted || detail.Ours == "" || detail.Theirs == "" {
		writeError(w, http.StatusBadRequest, errors.New("该冲突涉及删除，暂不支持 AI 建议"))
		return
	}
	if conflictSuggestionContentBudget(detail) > gitConflictSuggestBudget {
		writeError(w, http.StatusBadRequest, errors.New("文件过大，无法交给 AI 生成建议"))
		return
	}

	now := time.Now().UTC()
	record := &gitConflictSuggestion{ID: uuid.NewString(), ProjectID: projectID, Path: input.Path, Agent: input.Agent, Status: gitConflictSuggestStatusRunning, RequestedAt: now}

	// 在请求内用正确后端读取两侧标签（merge/rebase 语义已由后端换算）。
	oursLabel, theirsLabel := "当前侧", "传入侧"
	if overview, overviewErr := runner.ConflictOverview(r.Context(), repo); overviewErr == nil && overview.Context.OperationType != GitConflictOperationNone {
		if overview.Context.OursLabel != "" {
			oursLabel = overview.Context.OursLabel
		}
		if overview.Context.TheirsLabel != "" {
			theirsLabel = overview.Context.TheirsLabel
		}
	}

	s.conflictSuggestMu.Lock()
	s.pruneConflictSuggestionsLocked(now)
	if s.conflictSuggestActive[projectID] {
		s.conflictSuggestMu.Unlock()
		writeError(w, http.StatusConflict, errors.New("该项目已有 AI 冲突建议正在生成，请稍候"))
		return
	}
	s.conflictSuggestActive[projectID] = true
	s.conflictSuggestions[record.ID] = record
	s.conflictSuggestMu.Unlock()

	writeJSON(w, http.StatusAccepted, record.response())

	gctx, cancel := context.WithCancel(s.runtimeCtx)
	s.conflictSuggestMu.Lock()
	record.cancel = cancel
	record.StartedAt = time.Now().UTC()
	s.conflictSuggestMu.Unlock()

	go func() {
		defer cancel()
		s.runConflictSuggestion(gctx, project, record, detail, oursLabel, theirsLabel)
	}()
}

func (s *Server) runConflictSuggestion(ctx context.Context, project Project, record *gitConflictSuggestion, detail GitConflictContent, oursLabel, theirsLabel string) {
	prompt := buildConflictSuggestionPrompt(record.Path, oursLabel, theirsLabel, detail)
	// quotaWait=0：冲突解法是用户当场等结果的一次性建议，额度被占就该立刻回明确失败，
	// 而不是让编辑器里的人在等待条上多停两分钟（排队语义只给后台的优化建议扫描/复核）。
	text, runErr := s.runReadOnlyAgentWithSchema(ctx, project, record.Agent, prompt, conflictSuggestionOutputSchema, 0, nil)

	s.conflictSuggestMu.Lock()
	defer s.conflictSuggestMu.Unlock()
	finished := time.Now().UTC()
	record.FinishedAt = &finished
	if runErr != nil {
		if ctx.Err() == context.Canceled {
			record.Status = gitConflictSuggestStatusCancelled
			record.ErrorMessage = "AI 建议已取消"
		} else {
			record.Status = gitConflictSuggestStatusFailed
			record.ErrorMessage = runErr.Error()
		}
	} else {
		merged, explanation, parseErr := parseConflictSuggestionJSON(text)
		if parseErr != nil {
			record.Status = gitConflictSuggestStatusFailed
			record.ErrorMessage = parseErr.Error()
		} else {
			record.Status = gitConflictSuggestStatusCompleted
			record.Merged = merged
			record.Explanation = explanation
		}
	}
	if s.conflictSuggestActive[record.ProjectID] {
		delete(s.conflictSuggestActive, record.ProjectID)
	}
}

// gitConflictSuggestionStatus 轮询单个 AI 建议。
func (s *Server) gitConflictSuggestionStatus(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	suggestionID := chi.URLParam(r, "suggestionID")
	record, found := s.copyConflictSuggestion(suggestionID)
	if !found || record.ProjectID != projectID {
		writeError(w, http.StatusNotFound, errors.New("suggestion not found"))
		return
	}
	writeJSON(w, http.StatusOK, record.response())
}

// gitConflictSuggestCancel 取消正在生成的 AI 建议。
func (s *Server) gitConflictSuggestCancel(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	suggestionID := chi.URLParam(r, "suggestionID")
	s.conflictSuggestMu.Lock()
	record := s.conflictSuggestions[suggestionID]
	if record != nil && record.ProjectID == projectID && record.Status == gitConflictSuggestStatusRunning && record.cancel != nil {
		record.cancel()
	}
	s.conflictSuggestMu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]bool{"cancelled": true})
}
