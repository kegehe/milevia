package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
)

type ModelUsage struct {
	Model               string  `json:"model"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	EstimatedCostUSD    float64 `json:"estimatedCostUsd"`
	ContextWindow       int64   `json:"contextWindow"`
}

type RunUsage struct {
	RunID               string       `json:"runId"`
	ConversationID      string       `json:"conversationId"`
	Available           bool         `json:"available"`
	Reason              string       `json:"reason,omitempty"`
	Status              string       `json:"status"`
	Model               string       `json:"model"`
	ContextWindow       int64        `json:"contextWindow"`
	ContextInputTokens  int64        `json:"contextInputTokens"`
	InputTokens         int64        `json:"inputTokens"`
	OutputTokens        int64        `json:"outputTokens"`
	CacheReadTokens     int64        `json:"cacheReadTokens"`
	CacheCreationTokens int64        `json:"cacheCreationTokens"`
	EstimatedCostUSD    float64      `json:"estimatedCostUsd"`
	AgentTurns          int64        `json:"agentTurns"`
	ModelSteps          int64        `json:"modelSteps"`
	ToolCalls           int64        `json:"toolCalls"`
	SubagentCount       int64        `json:"subagentCount"`
	DurationMS          int64        `json:"durationMs"`
	TTFTMS              int64        `json:"ttftMs"`
	TerminalReason      string       `json:"terminalReason"`
	HasResult           bool         `json:"hasResult"`
	StartedAt           *time.Time   `json:"startedAt,omitempty"`
	CompletedAt         *time.Time   `json:"completedAt,omitempty"`
	Models              []ModelUsage `json:"models"`
}

type ConversationUsage struct {
	TaskCount           int64   `json:"taskCount"`
	AgentTurns          int64   `json:"agentTurns"`
	ModelSteps          int64   `json:"modelSteps"`
	ToolCalls           int64   `json:"toolCalls"`
	SubagentCount       int64   `json:"subagentCount"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	EstimatedCostUSD    float64 `json:"estimatedCostUsd"`
}

type ConversationUsageResponse struct {
	ConversationID string            `json:"conversationId"`
	Available      bool              `json:"available"`
	Reason         string            `json:"reason,omitempty"`
	Context        RunUsage          `json:"context"`
	CurrentRun     *RunUsage         `json:"currentRun,omitempty"`
	LatestRun      *RunUsage         `json:"latestRun,omitempty"`
	Session        ConversationUsage `json:"session"`
	Models         []ModelUsage      `json:"models"`
}

type runUsageAccumulator struct {
	runID          string
	conversationID string
	model          string
	contextModel   string
	contextWindow  int64
	contextInput   int64
	inputTokens    int64
	outputTokens   int64
	cacheRead      int64
	cacheCreation  int64
	cost           float64
	agentTurns     int64
	modelSteps     int64
	toolCalls      int64
	durationMS     int64
	ttftMS         int64
	terminalReason string
	hasResult      bool
	messageIDs     map[string]struct{}
	toolIDs        map[string]struct{}
	parentIDs      map[string]struct{}
	models         map[string]ModelUsage
	startedAt      time.Time
}

func newRunUsageAccumulator(runID, conversationID string) *runUsageAccumulator {
	return &runUsageAccumulator{runID: runID, conversationID: conversationID, startedAt: time.Now().UTC(), messageIDs: map[string]struct{}{}, toolIDs: map[string]struct{}{}, parentIDs: map[string]struct{}{}, models: map[string]ModelUsage{}}
}

func (s *Server) beginRunUsage(runID, conversationID string) {
	s.usageMu.Lock()
	if s.runUsage[runID] == nil {
		s.runUsage[runID] = newRunUsageAccumulator(runID, conversationID)
	}
	s.usageMu.Unlock()
}

// contextSnapshotTokens is the complete prompt size for one model call.
// Cached input still occupies the model context and must be included when
// measuring context-window utilisation.
func contextSnapshotTokens(inputTokens, cacheReadTokens, cacheCreationTokens int64) int64 {
	return inputTokens + cacheReadTokens + cacheCreationTokens
}

func (s *Server) discardRunUsage(runID string) {
	s.usageMu.Lock()
	delete(s.runUsage, runID)
	s.usageMu.Unlock()
}

func (s *Server) collectUsageEvent(runID, conversationID, eventType string, payload json.RawMessage) {
	s.usageMu.Lock()
	accumulator := s.runUsage[runID]
	if accumulator == nil {
		accumulator = newRunUsageAccumulator(runID, conversationID)
		s.runUsage[runID] = accumulator
	}
	changed := accumulator.collect(eventType, payload)
	snapshot := accumulator.snapshot("running", nil)
	s.usageMu.Unlock()
	if changed {
		s.appendEvent(runID, conversationID, "usage.updated", mustJSON(snapshot))
	}
}

func (accumulator *runUsageAccumulator) collect(eventType string, payload json.RawMessage) bool {
	switch eventType {
	case "system":
		var event struct {
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		}
		if json.Unmarshal(payload, &event) == nil && event.Subtype == "init" && event.Model != "" {
			accumulator.model = event.Model
			return true
		}
	case "assistant":
		var event struct {
			ParentToolUseID *string `json:"parent_tool_use_id"`
			Message         struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens         int64 `json:"input_tokens"`
					OutputTokens        int64 `json:"output_tokens"`
					CacheReadTokens     int64 `json:"cache_read_input_tokens"`
					CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
				Content []struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return false
		}
		changed := false
		if event.ParentToolUseID != nil && *event.ParentToolUseID != "" {
			if _, exists := accumulator.parentIDs[*event.ParentToolUseID]; !exists {
				accumulator.parentIDs[*event.ParentToolUseID] = struct{}{}
				changed = true
			}
		} else if event.Message.ID != "" {
			if _, exists := accumulator.messageIDs[event.Message.ID]; !exists {
				accumulator.messageIDs[event.Message.ID] = struct{}{}
				accumulator.modelSteps++
				if contextTokens := contextSnapshotTokens(event.Message.Usage.InputTokens, event.Message.Usage.CacheReadTokens, event.Message.Usage.CacheCreationTokens); contextTokens > 0 {
					accumulator.contextInput = contextTokens
				}
				if event.Message.Model != "" {
					accumulator.model = event.Message.Model
					accumulator.contextModel = event.Message.Model
				}
				changed = true
			}
		}
		for _, content := range event.Message.Content {
			if content.Type != "tool_use" || content.ID == "" {
				continue
			}
			if _, exists := accumulator.toolIDs[content.ID]; !exists {
				accumulator.toolIDs[content.ID] = struct{}{}
				accumulator.toolCalls++
				changed = true
			}
		}
		return changed
	case "result":
		var event struct {
			NumTurns       int64   `json:"num_turns"`
			DurationMS     int64   `json:"duration_ms"`
			TTFTMS         int64   `json:"ttft_ms"`
			TotalCostUSD   float64 `json:"total_cost_usd"`
			TerminalReason string  `json:"terminal_reason"`
			Usage          struct {
				InputTokens         int64 `json:"input_tokens"`
				OutputTokens        int64 `json:"output_tokens"`
				CacheReadTokens     int64 `json:"cache_read_input_tokens"`
				CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			ModelUsage map[string]struct {
				InputTokens         int64   `json:"inputTokens"`
				OutputTokens        int64   `json:"outputTokens"`
				CacheReadTokens     int64   `json:"cacheReadInputTokens"`
				CacheCreationTokens int64   `json:"cacheCreationInputTokens"`
				CostUSD             float64 `json:"costUSD"`
				ContextWindow       int64   `json:"contextWindow"`
			} `json:"modelUsage"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return false
		}
		accumulator.agentTurns = event.NumTurns
		accumulator.durationMS = event.DurationMS
		accumulator.ttftMS = event.TTFTMS
		accumulator.cost = event.TotalCostUSD
		accumulator.terminalReason = event.TerminalReason
		accumulator.inputTokens = event.Usage.InputTokens
		accumulator.outputTokens = event.Usage.OutputTokens
		accumulator.cacheRead = event.Usage.CacheReadTokens
		accumulator.cacheCreation = event.Usage.CacheCreationTokens
		accumulator.hasResult = true
		// result.usage is cumulative for this run. It is valid for run-level
		// usage metrics, but not for a context snapshot: a multi-turn run can
		// exceed the model window many times over. ContextInput therefore only
		// comes from a concrete main-agent assistant event above.
		for model, usage := range event.ModelUsage {
			accumulator.models[model] = ModelUsage{Model: model, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadTokens: usage.CacheReadTokens, CacheCreationTokens: usage.CacheCreationTokens, EstimatedCostUSD: usage.CostUSD, ContextWindow: usage.ContextWindow}
		}
		contextModel := accumulator.contextModel
		if contextModel == "" {
			contextModel = accumulator.model
		}
		if usage, ok := accumulator.models[contextModel]; ok {
			accumulator.contextWindow = usage.ContextWindow
		}
		return true
	case "item.started":
		// Codex emits item.started/item.completed for command_execution and
		// file_change. Count each tool invocation once, on the started event,
		// using the stable item id as the dedup key.
		var event struct {
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return false
		}
		if event.Item.Type != "command_execution" && event.Item.Type != "file_change" {
			return false
		}
		if event.Item.ID == "" {
			return false
		}
		if _, exists := accumulator.toolIDs[event.Item.ID]; exists {
			return false
		}
		accumulator.toolIDs[event.Item.ID] = struct{}{}
		accumulator.toolCalls++
		return true
	case "item.completed":
		// Codex agent_message items are model steps. Dedup by item id.
		var event struct {
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(payload, &event) != nil || event.Item.Type != "agent_message" {
			return false
		}
		if event.Item.ID == "" {
			return false
		}
		if _, exists := accumulator.messageIDs[event.Item.ID]; exists {
			return false
		}
		accumulator.messageIDs[event.Item.ID] = struct{}{}
		accumulator.modelSteps++
		return true
	case "turn.completed":
		// Codex emits a usage block at the end of each turn. The usage fields
		// are per-turn (not cumulative across the run), so each turn's tokens
		// must be accumulated. contextInput is a snapshot of the latest turn's
		// prompt size — it reflects the maximum context used and should NOT be
		// accumulated.
		var event struct {
			Usage struct {
				InputTokens           int64 `json:"input_tokens"`
				OutputTokens          int64 `json:"output_tokens"`
				CachedInputTokens     int64 `json:"cached_input_tokens"`
				CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return false
		}
		accumulator.inputTokens += event.Usage.InputTokens
		accumulator.outputTokens += event.Usage.OutputTokens
		accumulator.cacheRead += event.Usage.CachedInputTokens
		accumulator.cacheCreation += event.Usage.CacheWriteInputTokens
		// contextInput is the latest turn's prompt size (a snapshot), not a
		// cumulative total. Later turns have larger prompts, so keeping the
		// latest value approximates peak context-window utilisation.
		accumulator.contextInput = contextSnapshotTokens(event.Usage.InputTokens, event.Usage.CachedInputTokens, event.Usage.CacheWriteInputTokens)
		accumulator.hasResult = true
		accumulator.agentTurns++
		return true
	}
	return false
}

func (accumulator *runUsageAccumulator) snapshot(status string, completedAt *time.Time) RunUsage {
	models := make([]ModelUsage, 0, len(accumulator.models))
	for _, usage := range accumulator.models {
		models = append(models, usage)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	startedAt := accumulator.startedAt
	return RunUsage{RunID: accumulator.runID, ConversationID: accumulator.conversationID, Available: true, Status: status, Model: accumulator.model, ContextWindow: accumulator.contextWindow, ContextInputTokens: accumulator.contextInput, InputTokens: accumulator.inputTokens, OutputTokens: accumulator.outputTokens, CacheReadTokens: accumulator.cacheRead, CacheCreationTokens: accumulator.cacheCreation, EstimatedCostUSD: accumulator.cost, AgentTurns: accumulator.agentTurns, ModelSteps: accumulator.modelSteps, ToolCalls: accumulator.toolCalls, SubagentCount: int64(len(accumulator.parentIDs)), DurationMS: accumulator.durationMS, TTFTMS: accumulator.ttftMS, TerminalReason: accumulator.terminalReason, HasResult: accumulator.hasResult, StartedAt: &startedAt, CompletedAt: completedAt, Models: models}
}

func (s *Server) persistRunUsage(runID, status string) error {
	s.usageMu.Lock()
	accumulator := s.runUsage[runID]
	if accumulator == nil {
		s.usageMu.Unlock()
		return nil
	}
	completedAt := time.Now().UTC()
	snapshot := accumulator.snapshot(status, &completedAt)
	s.usageMu.Unlock()

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`insert into run_usage (run_id,conversation_id,model,context_window,context_input_tokens,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,estimated_cost_usd,agent_turns,model_steps,tool_calls,subagent_count,duration_ms,ttft_ms,terminal_reason,has_result,completed_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) on conflict(run_id) do update set model=excluded.model,context_window=excluded.context_window,context_input_tokens=excluded.context_input_tokens,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,cache_creation_tokens=excluded.cache_creation_tokens,estimated_cost_usd=excluded.estimated_cost_usd,agent_turns=excluded.agent_turns,model_steps=excluded.model_steps,tool_calls=excluded.tool_calls,subagent_count=excluded.subagent_count,duration_ms=excluded.duration_ms,ttft_ms=excluded.ttft_ms,terminal_reason=excluded.terminal_reason,has_result=excluded.has_result,completed_at=excluded.completed_at`, snapshot.RunID, snapshot.ConversationID, snapshot.Model, snapshot.ContextWindow, snapshot.ContextInputTokens, snapshot.InputTokens, snapshot.OutputTokens, snapshot.CacheReadTokens, snapshot.CacheCreationTokens, snapshot.EstimatedCostUSD, snapshot.AgentTurns, snapshot.ModelSteps, snapshot.ToolCalls, snapshot.SubagentCount, snapshot.DurationMS, snapshot.TTFTMS, snapshot.TerminalReason, snapshot.HasResult, completedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(`delete from run_model_usage where run_id=?`, snapshot.RunID); err != nil {
		return err
	}
	for _, usage := range snapshot.Models {
		if _, err = tx.Exec(`insert into run_model_usage (run_id,model,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,estimated_cost_usd,context_window) values (?,?,?,?,?,?,?,?)`, snapshot.RunID, usage.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreationTokens, usage.EstimatedCostUSD, usage.ContextWindow); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) liveRunUsage(runID string, status string) *RunUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	accumulator := s.runUsage[runID]
	if accumulator == nil {
		return nil
	}
	snapshot := accumulator.snapshot(status, nil)
	return &snapshot
}

func (s *Server) getConversationUsage(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	var exists string
	if err := s.db.QueryRowContext(r.Context(), `select id from conversations where id=?`, conversationID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response := ConversationUsageResponse{ConversationID: conversationID, Available: true}
	if err := s.db.QueryRowContext(r.Context(), `select count(*),coalesce(sum(agent_turns),0),coalesce(sum(model_steps),0),coalesce(sum(tool_calls),0),coalesce(sum(subagent_count),0),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(cache_read_tokens),0),coalesce(sum(cache_creation_tokens),0),coalesce(sum(estimated_cost_usd),0) from runs r left join run_usage u on u.run_id=r.id where r.conversation_id=?`, conversationID).Scan(&response.Session.TaskCount, &response.Session.AgentTurns, &response.Session.ModelSteps, &response.Session.ToolCalls, &response.Session.SubagentCount, &response.Session.InputTokens, &response.Session.OutputTokens, &response.Session.CacheReadTokens, &response.Session.CacheCreationTokens, &response.Session.EstimatedCostUSD); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	latest, err := s.loadLatestTerminalRunUsage(r.Context(), conversationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err == nil {
		response.LatestRun = latest
		response.Context = *latest
	}
	models, err := s.loadConversationModelUsage(r.Context(), conversationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response.Models = models
	var runID, status string
	if err := s.db.QueryRowContext(r.Context(), `select id,status from runs where conversation_id=? and status='running' order by created_at desc limit 1`, conversationID).Scan(&runID, &status); err == nil {
		response.CurrentRun = s.liveRunUsage(runID, status)
		if response.CurrentRun != nil {
			// Fill contextWindow from model history when the run hasn't
			// produced a result event yet.
			if response.CurrentRun.ContextWindow == 0 && response.CurrentRun.Model != "" {
				for _, model := range response.Models {
					if model.Model == response.CurrentRun.Model {
						response.CurrentRun.ContextWindow = model.ContextWindow
						break
					}
				}
			}
			response.Context = *response.CurrentRun

			// A completion persists usage immediately before changing the run
			// status. During that small window the SQL aggregate already
			// contains this run, so do not add its live snapshot a second time.
			persisted, err := s.hasPersistedRunUsage(r.Context(), runID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if !persisted {
				response.Session.AgentTurns += response.CurrentRun.AgentTurns
				response.Session.ModelSteps += response.CurrentRun.ModelSteps
				response.Session.ToolCalls += response.CurrentRun.ToolCalls
				response.Session.SubagentCount += response.CurrentRun.SubagentCount
				response.Session.InputTokens += response.CurrentRun.InputTokens
				response.Session.OutputTokens += response.CurrentRun.OutputTokens
				response.Session.CacheReadTokens += response.CurrentRun.CacheReadTokens
				response.Session.CacheCreationTokens += response.CurrentRun.CacheCreationTokens
				response.Session.EstimatedCostUSD += response.CurrentRun.EstimatedCostUSD

				// Merge live model usage so the per-model breakdown stays current.
				for _, m := range response.CurrentRun.Models {
					found := false
					for i, existing := range response.Models {
						if existing.Model == m.Model {
							response.Models[i].InputTokens += m.InputTokens
							response.Models[i].OutputTokens += m.OutputTokens
							response.Models[i].CacheReadTokens += m.CacheReadTokens
							response.Models[i].CacheCreationTokens += m.CacheCreationTokens
							response.Models[i].EstimatedCostUSD += m.EstimatedCostUSD
							found = true
							break
						}
					}
					if !found {
						response.Models = append(response.Models, m)
					}
				}
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getRunUsage(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	var status, conversationID string
	if err := s.db.QueryRowContext(r.Context(), `select r.status,r.conversation_id from runs r join conversations c on c.id=r.conversation_id where r.id=?`, runID).Scan(&status, &conversationID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if live := s.liveRunUsage(runID, status); live != nil {
		writeJSON(w, http.StatusOK, live)
		return
	}
	usage, err := s.loadRunUsage(r.Context(), runID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, RunUsage{RunID: runID, ConversationID: conversationID, Available: false, Reason: "该任务未获得可用的使用统计。", Status: status})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) loadLatestTerminalRunUsage(ctx context.Context, conversationID string) (*RunUsage, error) {
	var runID, status string
	var startedAt, completedAt time.Time
	if err := s.db.QueryRowContext(ctx, `select id,status,created_at,completed_at from runs where conversation_id=? and status in ('completed','failed','stopped') order by completed_at desc limit 1`, conversationID).Scan(&runID, &status, &startedAt, &completedAt); err != nil {
		return nil, err
	}
	usage, err := s.loadRunUsage(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return &RunUsage{RunID: runID, ConversationID: conversationID, Available: false, Reason: "该任务未获得可用的使用统计。", Status: status, StartedAt: &startedAt, CompletedAt: &completedAt}, nil
	}
	return usage, err
}

func (s *Server) hasPersistedRunUsage(ctx context.Context, runID string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from run_usage where run_id=?)`, runID).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Server) loadRunUsage(ctx context.Context, runID string) (*RunUsage, error) {
	usage := &RunUsage{}
	var startedAt, completedAt time.Time
	err := s.db.QueryRowContext(ctx, `select u.run_id,u.conversation_id,r.status,u.model,u.context_window,u.context_input_tokens,u.input_tokens,u.output_tokens,u.cache_read_tokens,u.cache_creation_tokens,u.estimated_cost_usd,u.agent_turns,u.model_steps,u.tool_calls,u.subagent_count,u.duration_ms,u.ttft_ms,u.terminal_reason,u.has_result,r.created_at,u.completed_at from run_usage u join runs r on r.id=u.run_id where u.run_id=?`, runID).Scan(&usage.RunID, &usage.ConversationID, &usage.Status, &usage.Model, &usage.ContextWindow, &usage.ContextInputTokens, &usage.InputTokens, &usage.OutputTokens, &usage.CacheReadTokens, &usage.CacheCreationTokens, &usage.EstimatedCostUSD, &usage.AgentTurns, &usage.ModelSteps, &usage.ToolCalls, &usage.SubagentCount, &usage.DurationMS, &usage.TTFTMS, &usage.TerminalReason, &usage.HasResult, &startedAt, &completedAt)
	if err != nil {
		return nil, err
	}
	usage.StartedAt = &startedAt
	usage.CompletedAt = &completedAt
	usage.Available = true
	models, err := s.loadRunModelUsage(ctx, runID)
	if err != nil {
		return nil, err
	}
	usage.Models = models
	return usage, nil
}

func (s *Server) loadRunModelUsage(ctx context.Context, runID string) ([]ModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `select model,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,estimated_cost_usd,context_window from run_model_usage where run_id=? order by model`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []ModelUsage{}
	for rows.Next() {
		var usage ModelUsage
		if err := rows.Scan(&usage.Model, &usage.InputTokens, &usage.OutputTokens, &usage.CacheReadTokens, &usage.CacheCreationTokens, &usage.EstimatedCostUSD, &usage.ContextWindow); err != nil {
			return nil, err
		}
		models = append(models, usage)
	}
	return models, rows.Err()
}

func (s *Server) loadConversationModelUsage(ctx context.Context, conversationID string) ([]ModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `select m.model,coalesce(sum(m.input_tokens),0),coalesce(sum(m.output_tokens),0),coalesce(sum(m.cache_read_tokens),0),coalesce(sum(m.cache_creation_tokens),0),coalesce(sum(m.estimated_cost_usd),0),max(m.context_window) from run_model_usage m join run_usage u on u.run_id=m.run_id where u.conversation_id=? group by m.model order by m.model`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []ModelUsage{}
	for rows.Next() {
		var usage ModelUsage
		if err := rows.Scan(&usage.Model, &usage.InputTokens, &usage.OutputTokens, &usage.CacheReadTokens, &usage.CacheCreationTokens, &usage.EstimatedCostUSD, &usage.ContextWindow); err != nil {
			return nil, err
		}
		models = append(models, usage)
	}
	return models, rows.Err()
}

func (s *Server) recordUsagePersistenceError(runID, conversationID string, err error) {
	if err != nil {
		s.appendEvent(runID, conversationID, "usage.error", mustJSON(map[string]string{"error": "无法保存用量统计，请稍后刷新重试。"}))
	}
}
