package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	checkpointMaxFileBytes = 8 << 20
	checkpointMaxFiles     = 20000
)

type conversationCheckpoint struct {
	ID              string     `json:"id"`
	ConversationID  string     `json:"conversationId"`
	ParentID        string     `json:"parentId,omitempty"`
	AgentID         string     `json:"agentId"`
	NativeSessionID string     `json:"nativeSessionId,omitempty"`
	NativeTurnID    string     `json:"nativeTurnId,omitempty"`
	Prompt          string     `json:"prompt"`
	SnapshotPath    string     `json:"snapshotPath,omitempty"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	RewoundAt       *time.Time `json:"rewoundAt,omitempty"`
	Metadata        string     `json:"metadata,omitempty"`
}

type rewindRequest struct {
	CheckpointID string `json:"checkpointId"`
	Mode         string `json:"mode"`
	Fork         bool   `json:"fork"`
}

func (s *Server) conversationCheckpointCutoff(ctx context.Context, conversationID string) (*time.Time, error) {
	var cutoff time.Time
	err := s.db.QueryRowContext(ctx, `select created_at from conversation_checkpoints where conversation_id=? and status='active' order by created_at desc,id desc limit 1`, conversationID).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cutoff, nil
}

func (s *Server) migrateCheckpoints(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
create table if not exists conversation_checkpoints (
 id text primary key,
 conversation_id text not null references conversations(id) on delete cascade,
 parent_id text not null default '',
 agent_id text not null default '',
 native_session_id text not null default '',
 native_turn_id text not null default '',
 prompt text not null default '',
 snapshot_path text not null default '',
 status text not null default 'active',
 metadata text not null default '{}',
 created_at datetime not null,
 rewound_at datetime
);
create index if not exists conversation_checkpoints_conversation on conversation_checkpoints(conversation_id,created_at desc);
`)
	if err != nil {
		return fmt.Errorf("migrate checkpoints: %w", err)
	}
	return nil
}

func (s *Server) listConversationCheckpoints(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "conversationID")
	var exists bool
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from conversations where id=?)`, id).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select id,conversation_id,parent_id,agent_id,native_session_id,native_turn_id,prompt,snapshot_path,status,created_at,rewound_at,metadata from conversation_checkpoints where conversation_id=? order by created_at asc,id asc`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := make([]conversationCheckpoint, 0)
	for rows.Next() {
		var item conversationCheckpoint
		if err := rows.Scan(&item.ID, &item.ConversationID, &item.ParentID, &item.AgentID, &item.NativeSessionID, &item.NativeTurnID, &item.Prompt, &item.SnapshotPath, &item.Status, &item.CreatedAt, &item.RewoundAt, &item.Metadata); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		item.SnapshotPath = ""
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": items})
}

func (s *Server) rewindConversation(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	var req rewindRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid rewind request"))
		return
	}
	if req.Mode == "" {
		req.Mode = "conversation_and_code"
	}
	if req.Mode != "conversation" && req.Mode != "code" && req.Mode != "conversation_and_code" {
		writeError(w, http.StatusBadRequest, errors.New("mode must be conversation, code, or conversation_and_code"))
		return
	}
	if strings.TrimSpace(req.CheckpointID) == "" {
		writeError(w, http.StatusBadRequest, errors.New("checkpointId is required"))
		return
	}
	var projectPath, snapshotPath string
	var projectRunner string
	var checkpoint conversationCheckpoint
	err := s.db.QueryRowContext(r.Context(), `select c.id,c.conversation_id,c.parent_id,c.agent_id,c.native_session_id,c.native_turn_id,c.prompt,c.snapshot_path,c.status,c.created_at,c.rewound_at,c.metadata,p.path,coalesce(nullif(p.runner_id,''),p.runner) from conversation_checkpoints c join conversations v on v.id=c.conversation_id join projects p on p.id=v.project_id where c.id=? and c.conversation_id=?`, req.CheckpointID, conversationID).Scan(&checkpoint.ID, &checkpoint.ConversationID, &checkpoint.ParentID, &checkpoint.AgentID, &checkpoint.NativeSessionID, &checkpoint.NativeTurnID, &checkpoint.Prompt, &snapshotPath, &checkpoint.Status, &checkpoint.CreatedAt, &checkpoint.RewoundAt, &checkpoint.Metadata, &projectPath, &projectRunner)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("checkpoint not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	checkpoint.SnapshotPath = snapshotPath
	localWorkspace := projectRunner == "" || projectRunner == "windows-local" || projectRunner == "wsl-local"
	if req.Mode != "conversation" && !localWorkspace {
		writeError(w, http.StatusConflict, errors.New("remote runner does not support local checkpoint code restore"))
		return
	}
	if req.Mode != "conversation" && strings.TrimSpace(snapshotPath) == "" {
		writeError(w, http.StatusConflict, errors.New("checkpoint has no file snapshot; conversation rewind is still available"))
		return
	}
	if req.Fork {
		forked, err := s.forkConversationAtCheckpoint(r.Context(), conversationID, checkpoint)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"conversation": forked, "checkpoint": checkpoint, "forked": true})
		return
	}
	if _, found, err := s.activeRunIDForConversation(r.Context(), conversationID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if found {
		writeError(w, http.StatusConflict, errors.New("conversation has an active run"))
		return
	}
	// A persistent CLI process retains the old native context even after the
	// workspace is restored. Retire it before the next turn can be admitted.
	if !s.sessionManager.retireForConfiguration(conversationID) && !s.awaitSessionRetired(conversationID, modelSwitchRetireTimeout) {
		writeError(w, http.StatusConflict, errors.New("native agent session is still stopping"))
		return
	}
	if req.Mode != "code" && checkpoint.AgentID == "codex" && localWorkspace {
		beforeTurnID, err := s.nextCodexTurnID(r.Context(), conversationID, checkpoint.CreatedAt)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if beforeTurnID != "" && checkpoint.NativeSessionID != "" {
			if err := s.codexRevertThread(r.Context(), projectPath, checkpoint.NativeSessionID, beforeTurnID); err != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("Codex 原生对话回退失败：%w", err))
				return
			}
		}
	}
	if req.Mode != "conversation" {
		if err := restoreCheckpointSnapshot(snapshotPath, projectPath); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	now := time.Now().UTC()
	metadata := map[string]any{"mode": req.Mode}
	metadataJSON, _ := json.Marshal(metadata)
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `update conversation_checkpoints set status='rewound',rewound_at=?,metadata=? where conversation_id=? and (created_at>? or (created_at=? and id>?))`, now, string(metadataJSON), conversationID, checkpoint.CreatedAt, checkpoint.CreatedAt, checkpoint.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `update conversation_checkpoints set status='active' where id=?`, checkpoint.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	checkpoint.Status = "active"
	writeJSON(w, http.StatusOK, map[string]any{"checkpoint": checkpoint, "forked": false})
}

func (s *Server) nextCodexTurnID(ctx context.Context, conversationID string, createdAt time.Time) (string, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `select e.payload from events e join runs r on r.id=e.run_id where e.conversation_id=? and e.type='turn.started' and e.created_at>? order by e.created_at asc,e.id asc limit 1`, conversationID, createdAt).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var event struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		TurnID string `json:"turn_id"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return "", err
	}
	if event.Turn.ID != "" {
		return event.Turn.ID, nil
	}
	if event.TurnID != "" {
		return event.TurnID, nil
	}
	return event.ID, nil
}

func (s *Server) codexRevertThread(ctx context.Context, projectPath, threadID, beforeTurnID string) error {
	binary := s.config.CodexPath
	if binary == "" {
		binary = "codex"
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := codexCommandContext(callCtx, binary, "app-server", "--stdio")
	cmd.Dir = projectPath
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	write := func(value any) error {
		line, err := json.Marshal(value)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		_, err = stdin.Write(line)
		return err
	}
	if err := write(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "milevia", "version": "dev"}, "capabilities": map[string]any{}}}); err != nil {
		return err
	}
	if err := write(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	if err := write(map[string]any{"id": 2, "method": "thread/revert", "params": map[string]string{"threadId": threadID, "beforeTurnId": beforeTurnID}}); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var response struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil || string(response.ID) != "2" {
			continue
		}
		if response.Error != nil {
			return errors.New(response.Error.Message)
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if stderr.Len() > 0 {
		return errors.New(strings.TrimSpace(stderr.String()))
	}
	return errors.New("Codex app-server 未返回 thread/revert 响应")
}

func (s *Server) forkConversationAtCheckpoint(ctx context.Context, sourceID string, checkpoint conversationCheckpoint) (Conversation, error) {
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	if _, found, err := s.activeRunIDForConversation(ctx, sourceID); err != nil {
		return Conversation{}, err
	} else if found {
		return Conversation{}, errors.New("conversation has an active run")
	}
	var source Conversation
	var current int
	err := s.db.QueryRowContext(ctx, `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,model_override,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where id=?`, sourceID).Scan(&source.ID, &source.ProjectID, &source.ClaudeSessionID, &source.AgentID, &source.AgentSessionID, &source.AgentRuntimeID, &source.AgentProfileRevisionID, &source.ProjectAgentRouteRevisionID, &source.ExecutionPolicy, &source.Status, &source.PermissionMode, &source.ModelOverride, &source.Title, &source.LastActivityAt, &source.ClaudeInitialized, &source.AgentInitialized, &current, &source.CreatedAt)
	if err != nil {
		return Conversation{}, err
	}
	now := time.Now().UTC()
	child := source
	child.ID = uuid.NewString()
	child.ClaudeSessionID = uuid.NewString()
	child.AgentSessionID = child.ClaudeSessionID
	child.Status = "idle"
	child.IsCurrent = true
	child.CreatedAt = now
	child.LastActivityAt = now
	child.Title = source.Title + "（分支）"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `update conversations set is_current=0 where project_id=? and is_current=1`, source.ProjectID); err != nil {
		return Conversation{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,model_override,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, child.ID, child.ProjectID, child.ClaudeSessionID, child.AgentID, child.AgentSessionID, child.AgentRuntimeID, child.AgentProfileRevisionID, child.ProjectAgentRouteRevisionID, child.ExecutionPolicy, child.Status, child.PermissionMode, child.ModelOverride, child.Title, child.LastActivityAt, false, false, true, child.CreatedAt); err != nil {
		return Conversation{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into messages (id,conversation_id,run_id,role,content,client_request_id,parent_tool_use_id,created_at) select lower(hex(randomblob(16))),?, '',role,content,'',parent_tool_use_id,created_at from messages where conversation_id=? and created_at<=?`, child.ID, sourceID, checkpoint.CreatedAt); err != nil {
		return Conversation{}, err
	}
	metadata := checkpoint.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	childCheckpointID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `insert into conversation_checkpoints (id,conversation_id,parent_id,agent_id,native_session_id,native_turn_id,prompt,snapshot_path,status,metadata,created_at) values (?,?,?,?,?,?,?,?,?,?,?)`, childCheckpointID, child.ID, "", child.AgentID, child.AgentSessionID, checkpoint.NativeTurnID, checkpoint.Prompt, checkpoint.SnapshotPath, "active", metadata, checkpoint.CreatedAt); err != nil {
		return Conversation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return child, nil
}

func (s *Server) createConversationCheckpoint(ctx context.Context, conversationID, runID, prompt string) error {
	var projectPath, agentID, nativeSessionID, projectRunner string
	if err := s.db.QueryRowContext(ctx, `select p.path,c.agent_id,coalesce(c.agent_session_id,c.claude_session_id,''),coalesce(nullif(p.runner_id,''),p.runner) from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&projectPath, &agentID, &nativeSessionID, &projectRunner); err != nil {
		return err
	}
	parentID := ""
	_ = s.db.QueryRowContext(ctx, `select id from conversation_checkpoints where conversation_id=? and status='active' order by created_at desc,id desc limit 1`, conversationID).Scan(&parentID)
	nativeTurnID := runID
	if agentID == "codex" {
		var payload string
		if err := s.db.QueryRowContext(ctx, `select payload from events where conversation_id=? and run_id=? and type='turn.started' order by created_at asc,id asc limit 1`, conversationID, runID).Scan(&payload); err == nil {
			var event struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
				TurnID string `json:"turn_id"`
				ID     string `json:"id"`
			}
			if json.Unmarshal([]byte(payload), &event) == nil {
				switch {
				case event.Turn.ID != "":
					nativeTurnID = event.Turn.ID
				case event.TurnID != "":
					nativeTurnID = event.TurnID
				case event.ID != "":
					nativeTurnID = event.ID
				}
			}
		}
	}
	storageRoot := s.config.DataDir
	if storageRoot == "" {
		storageRoot = filepath.Dir(s.config.DatabasePath)
	}
	var snapshotPath string
	var metadata map[string]any
	var err error
	if projectRunner == "" || projectRunner == "windows-local" || projectRunner == "wsl-local" {
		snapshotPath, metadata, err = createWorkspaceCheckpoint(projectPath, conversationID, filepath.Join(storageRoot, "checkpoints"))
		if err != nil {
			// Keep the conversation checkpoint even when the workspace cannot be
			// snapshotted. Conversation rewind remains useful; code rewind will
			// return a clear missing-snapshot error instead of losing the node.
			snapshotPath = ""
			metadata = map[string]any{"snapshotUnavailable": true, "snapshotError": err.Error()}
		}
	} else {
		metadata = map[string]any{"snapshotUnavailable": true, "runner": projectRunner}
	}
	id := uuid.NewString()
	metadataJSON, _ := json.Marshal(metadata)
	_, err = s.db.ExecContext(ctx, `insert into conversation_checkpoints (id,conversation_id,parent_id,agent_id,native_session_id,native_turn_id,prompt,snapshot_path,status,metadata,created_at) values (?,?,?,?,?,?,?,?,?,?,?)`, id, conversationID, parentID, agentID, nativeSessionID, nativeTurnID, prompt, snapshotPath, "active", string(metadataJSON), time.Now().UTC())
	return err
}

func createWorkspaceCheckpoint(projectPath, conversationID, storageRoot string) (string, map[string]any, error) {
	root, err := filepath.Abs(projectPath)
	if err != nil {
		return "", nil, err
	}
	base := filepath.Join(storageRoot, conversationID, uuid.NewString())
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, err
	}
	count, skipped := 0, []string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if entry.IsDir() && (rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator))) {
			return fs.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			skipped = append(skipped, rel)
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if count >= checkpointMaxFiles {
			skipped = append(skipped, rel)
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() > checkpointMaxFileBytes {
			skipped = append(skipped, rel)
			return nil
		}
		dst := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := copyFile(path, dst, info.Mode()); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(base)
		return "", nil, err
	}
	return base, map[string]any{"fileCount": count, "skipped": skipped}, nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 128*1024)
	_, err = io.CopyBuffer(out, in, buf)
	return err
}

func restoreCheckpointSnapshot(snapshotPath, projectPath string) error {
	if snapshotPath == "" {
		return errors.New("checkpoint has no file snapshot")
	}
	root, err := filepath.Abs(projectPath)
	if err != nil {
		return err
	}
	base, err := filepath.Abs(snapshotPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(base); err != nil {
		return fmt.Errorf("checkpoint snapshot unavailable: %w", err)
	}
	return filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == base {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
			return errors.New("invalid checkpoint path")
		}
		dst := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(path, dst, entry.Type())
	})
}
