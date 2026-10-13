package app

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"
)

const (
	taskGeneratedTitleLimit = 60
	taskTitleRefineTimeout  = 20 * time.Second
)

var taskTitleOutputSchema = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`)

// generatedTaskTitle provides the title returned with the create response. It
// is deliberately local so task creation never waits for an Agent.
func generatedTaskTitle(description string) string {
	value := strings.Join(strings.Fields(description), " ")
	if value == "" {
		return "未命名任务"
	}
	for _, delimiter := range []string{"。", "！", "？", "!", "?", "；", ";", "."} {
		if index := strings.Index(value, delimiter); index >= 0 {
			value = strings.TrimSpace(value[:index])
			break
		}
	}
	return truncateTaskTitle(value)
}

func truncateTaskTitle(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) <= taskGeneratedTitleLimit {
		return value
	}
	return string(runes[:taskGeneratedTitleLimit]) + "..."
}

func parseGeneratedTaskTitle(text string) string {
	var result struct {
		Title string `json:"title"`
	}
	if json.Unmarshal([]byte(extractInsightJSON(text)), &result) != nil {
		return ""
	}
	title := truncateTaskTitle(result.Title)
	if title == "" {
		return ""
	}
	return title
}

func taskTitlePrompt(description string) string {
	return "为下面的任务说明生成一个简洁、准确的中文任务标题。只概括要完成的工作，不添加背景、引号或句号；不超过 60 个字符。仅返回 JSON 对象 {\"title\":\"...\"}。\n\n任务说明：\n" + description
}

func (s *Server) refineTaskTitleAsync(projectID, taskID, description string) {
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), taskTitleRefineTimeout)
		defer cancel()
		project, err := s.getProjectByID(ctx, projectID)
		if err != nil {
			return
		}
		for _, agentID := range []string{"codex", "claude-code"} {
			// CLI-managed projects have not opted into a model-backed title pass.
			// Keep creation deterministic unless the project has an active profile.
			profile, err := s.projectRuntimeProfile(ctx, project, agentID)
			if err != nil || profile == nil {
				continue
			}
			output, err := s.runReadOnlyAgentWithSchema(ctx, project, agentID, taskTitlePrompt(description), taskTitleOutputSchema, 0, nil)
			if err != nil {
				continue
			}
			title := parseGeneratedTaskTitle(output)
			if title == "" {
				continue
			}
			// A later edit owns the title. Never let an older background response
			// overwrite the title derived from newer task content.
			if _, err := s.db.ExecContext(ctx, `update tasks set title=?,updated_at=? where id=? and description=?`, title, time.Now().UTC(), taskID, description); err != nil && ctx.Err() == nil {
				log.Printf("refine task title %s: %v", taskID, err)
			}
			return
		}
	}()
}
