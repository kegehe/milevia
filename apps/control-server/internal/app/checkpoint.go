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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	// checkpointRetentionPerConversation 每个会话保留的节点数上限（当前节点一定保留）。
	// 影子仓库按内容去重，超限删的是"行"；对象交给 git 自己的 gc 回收。
	checkpointRetentionPerConversation = 50

	// checkpointMaintenanceTimeout 是启动后台转换/清理的预算上限。转换一份遗留拷贝快照要
	// 重新哈希并压缩整个目录，慢是正常的；超时只是把剩下的留到下次启动，不会损坏数据。
	checkpointMaintenanceTimeout = 30 * time.Minute

	// checkpointGitTimeout 是快照/恢复类 git 命令的预算，比常规 Git 操作的 30 秒宽得多：
	// 一次 `add -A` 要 stat 整棵树并哈希改动的文件。真机实测 165 MB / 2 万个文件的树，
	// 30 秒必然 "context deadline exceeded" —— 而失败是静默的（只是"这次没快照"）。
	checkpointGitTimeout = 5 * time.Minute

	// checkpointImportGitTimeout 是**转换遗留拷贝快照**的预算，比快照档还宽：真机实测
	// 一份 186.7 MB / 2 万个文件的遗留快照，光是读+哈希就要 6 分钟（Windows 上小文件 I/O
	// 是瓶颈，与 git 本身无关）。这是后台活，慢可以接受，中途放弃不行（放弃了就得重来）。
	checkpointImportGitTimeout = 20 * time.Minute

	// checkpointImportPerStartup 是每次启动最多转换几个遗留节点。一次 6 分钟、六份就是
	// 半小时的磁盘/CPU 占用，不该在一个启动里做完 —— 剩下的留给下次启动，每次都是幂等的。
	checkpointImportPerStartup = 1

	// checkpointSweepStartDelay 是启动到开始整段快照维护（清理 + 遗留快照转换）之间的
	// 静默期。旧实现只有"此刻没有运行"这一道门，于是应用刚起来就开始整目录 git add ——
	// 用户两分钟后发的第一条消息只能跟它抢磁盘（真机上见过两者撞在一起、发送卡 61 秒）。
	checkpointSweepStartDelay = 2 * time.Minute

	// checkpointSweepWatchInterval 是转换期间检查"有人开始干活了吗"的间隔。
	checkpointSweepWatchInterval = 2 * time.Second

	// checkpointCleanupIdleWait 是清理阶段最多额外等空闲的时间。再等不到也要做：
	// 编排/定时任务持续运行的机器上，如果坚持"完全空闲才动手"，孤儿快照目录
	// （一份可能几百 MB）会在每次启动都被跳过、永远不回收。转换仍然只挑空闲窗口。
	checkpointCleanupIdleWait = 8 * time.Minute

	// checkpointShadowRepoName 是会话影子仓库在快照目录下的名字。
	checkpointShadowRepoName = "shadow.git"

	// checkpointRewindTimeout 是"回退"这条破坏性链路的预算。它跑在服务运行时上下文上
	// （不跟 HTTP 请求走）：客户端超时断开不该把 read-tree 杀在半路，留下一个改了一半的
	// 工作区 + 一个"没回退"的库。
	checkpointRewindTimeout = 10 * time.Minute

	// checkpointImportRepoPrefix 是遗留拷贝快照转换出来的、**按节点独立**的仓库前缀。
	// 刻意不并进会话影子仓库：转换的 work-tree 是遗留拷贝目录（数据目录里的 Windows 目录），
	// 与会话影子仓库的工作区不是同一个，共用一个 index 会互相踩。
	checkpointImportRepoPrefix = "imported-"
)

// 快照引擎。决定"恢复代码"怎么回写：
//   - git：影子 git 仓库 + read-tree 真回退（快照之后新建的文件会被删掉）；
//   - git-legacy：由遗留拷贝目录转换而来，快照树可能缺文件（旧机制跳过大文件与符号链接），
//     所以只覆盖回写、不删工作区里多出来的文件；
//   - legacy：仍是升级前的拷贝目录，按目录逐文件回写。
const (
	checkpointEngineGit       = "git"
	checkpointEngineGitLegacy = "git-legacy"
	checkpointEngineLegacy    = "legacy"
)

// checkpointLockSet 串行化同一个会话上的快照类 git 操作（快照 / 恢复 / 转换）。
//
// 为什么必须串：三者用的是同一批仓库与同一个 index，而"快照"发生在 run 收尾 —— 那一刻 run
// 已经是 completed，回退里"有没有活跃 run"的判据拦不住它；手机和桌面又可能同时点回退。
// 并发下最糟的不是卡一下，而是两条 git 同时写 index：某个节点的提交树与工作区对不上，
// 之后"恢复"出来的是别的内容。按会话分锁，避免一个会话的长操作卡住别的会话。
type checkpointLockSet struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *checkpointLockSet) lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	held := l.locks[key]
	if held == nil {
		held = &sync.Mutex{}
		l.locks[key] = held
	}
	l.mu.Unlock()
	held.Lock()
	return held.Unlock
}

type conversationCheckpoint struct {
	ID              string `json:"id"`
	ConversationID  string `json:"conversationId"`
	ParentID        string `json:"parentId,omitempty"`
	AgentID         string `json:"agentId"`
	NativeSessionID string `json:"nativeSessionId,omitempty"`
	NativeTurnID    string `json:"nativeTurnId,omitempty"`
	RunID           string `json:"runId,omitempty"`
	Prompt          string `json:"prompt"`
	SnapshotPath    string `json:"snapshotPath,omitempty"`
	SnapshotRef     string `json:"snapshotRef,omitempty"`
	// WorkspacePath 是**这一轮实际跑在哪个目录**（会话可以切到隔离工作区，那时它不是项目根）。
	// 快照与恢复都必须认它：拿项目根去恢复会把用户自己在项目根上的改动当成"快照之后新建的
	// 文件"删掉，而被切换到的那个工作区里的代码一动不动。
	WorkspacePath string `json:"-"`
	// HasSnapshot 是"这个节点现在能不能恢复代码"的如实读数：快照目录还在才算。项目被删、
	// 数据目录被清理、快照创建失败的节点都会是 false，界面据此只给"恢复对话"。
	HasSnapshot bool       `json:"hasSnapshot"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	RewoundAt   *time.Time `json:"rewoundAt,omitempty"`
	Metadata    string     `json:"metadata,omitempty"`
}

type rewindRequest struct {
	CheckpointID string `json:"checkpointId"`
	Mode         string `json:"mode"`
	Fork         bool   `json:"fork"`
}

// checkpointQueryer 是"能跑一次单行查询"的最小接口：*sql.DB 与 *sql.Tx 都满足它。
//
// 判据必须能跑在调用方那条连接上：服务端只有一条 SQLite 连接（SetMaxOpenConns(1)），
// 在事务里再去 s.db 查会**死锁** —— 手机快照就是在事务里组装的。
type checkpointQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkpointVisibility 是"回退之后哪些历史还看得见"的唯一判据。两个边界都**存在会话行上**：
//
//	hiddenAfter —— 最近一次回退的落点（目标节点的时间）。比它晚的内容属于被丢掉的那段。
//	rewoundAt   —— 最近一次回退的时刻。回退**之后**发生的内容不属于被丢掉的那段——
//	               回退后刚发的那条消息（哪怕失败/被停止）必须照常显示。
//
// 为什么不用"最新 active 节点的时间"当边界（早先的写法）：回退之后只要再成功跑一轮，
// 就会生成一个新节点，那个边界立刻越过被丢掉的区间 —— 被回退掉的内容会**重新出现**。
// 落点时间必须是存下来的值，不能由节点状态推导。
//
// 所有面向用户的历史读路径（会话详情、活动流、输入历史、手机快照、编排续跑、会话列表的
// 预览与搜索）都用它。
func checkpointVisibilityFor(ctx context.Context, queryer checkpointQueryer, conversationID string) (hiddenAfter *time.Time, rewoundAt *time.Time, err error) {
	var hiddenValue, rewoundValue sql.NullTime
	err = queryer.QueryRowContext(ctx, `select checkpoint_hidden_after,checkpoint_rewound_at from conversations where id=?`, conversationID).Scan(&hiddenValue, &rewoundValue)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if hiddenValue.Valid {
		hiddenAfter = &hiddenValue.Time
	}
	if rewoundValue.Valid {
		rewoundAt = &rewoundValue.Time
	}
	return hiddenAfter, rewoundAt, nil
}

// checkpointVisibilityClause 把判据变成 SQL 条件（不含前导 "and"）。column 是时间列名。
// 返回空串表示这条会话不需要过滤（从没回退过）。
func checkpointVisibilityClause(column string, hiddenAfter, rewoundAt *time.Time) (string, []any) {
	if hiddenAfter == nil {
		return "", nil
	}
	if rewoundAt != nil && rewoundAt.After(*hiddenAfter) {
		return "(" + column + " <= ? or " + column + " > ?)", []any{*hiddenAfter, *rewoundAt}
	}
	return column + " <= ?", []any{*hiddenAfter}
}

// truncateCheckpointPrompt 把节点列表里的提示词截到可读长度。
//
// 列表只用来让人认出"我要回到哪一轮"（界面自己还会截到 72 字）。prompt 是那一轮的第一条
// 用户消息原文，可能是用户粘进来的一大段日志；中继对整帧有上限（64 KiB），而手机端把
// 超限的失败当成"这条会话没有节点"—— 整个回退入口会**静默消失**。
func truncateCheckpointPrompt(prompt string) string {
	const limit = 200
	runes := []rune(strings.TrimSpace(prompt))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

// checkpointCutoffValue 把节点时间序列化成响应字段（RFC3339Nano，空串表示没有节点）。
//
// 为什么要把服务端的过滤条件也发给客户端：两端的消息列表都是**本地内容 ∪ 服务端内容**
// （桌面的 reload 合并、手机的历史分页缓存），只过滤服务端那一份挡不住已经在本地留过的
// 那些回合 —— 用户回退了却还看得见被丢掉的对话。客户端拿这个值把本地那份也丢掉。
func checkpointCutoffValue(cutoff *time.Time) string {
	if cutoff == nil {
		return ""
	}
	return cutoff.UTC().Format(time.RFC3339Nano)
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
 run_id text not null default '',
 prompt text not null default '',
 snapshot_path text not null default '',
 snapshot_ref text not null default '',
 workspace_path text not null default '',
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
	for _, column := range []struct{ name, definition string }{
		// run_id 让"消息 ↔ 节点"有一个不依赖 agent 的稳定键（Codex 的 native_turn_id 是原生
		// turn id，不能拿来对消息）。界面按 message.runId 在时间线上就地给出回退入口。
		{"run_id", "text not null default ''"},
		// snapshot_ref 是快照仓库里的提交对象 ID；空串表示这一行是遗留拷贝型快照。
		{"snapshot_ref", "text not null default ''"},
		// workspace_path 是这一轮实际跑的目录（隔离工作区时不是项目根）。空串 = 升级前的节点，
		// 那时快照的就是项目根，恢复也回到项目根。
		{"workspace_path", "text not null default ''"},
	} {
		if err := ensureColumn(ctx, s.db, "conversation_checkpoints", column.name, column.definition); err != nil {
			return fmt.Errorf("add checkpoint %s: %w", column.name, err)
		}
	}
	// 回填工作区路径：runs 表记着那一轮真的跑在哪个目录。先按 run_id 找，再按 native_turn_id
	// （Claude 系节点两者同值；升级前只写了 native_turn_id）。对不上就留空串 —— 那时按项目根处理，
	// 正是旧机制的行为。
	// 只回填**git 快照**（snapshot_ref<>''）的行：遗留拷贝型快照是旧机制从**项目根**拍的，
	// 给它写上 r.workspace_path 会让恢复把项目根的内容拷进隔离工作区（等于把另一份代码写进去）。
	// 遗留行保持空串 = 仍按项目根恢复，与旧机制一致。
	if _, err := s.db.ExecContext(ctx, `update conversation_checkpoints set workspace_path=coalesce(nullif((select r.workspace_path from runs r where r.id=conversation_checkpoints.run_id),''), nullif((select r.workspace_path from runs r where r.id=conversation_checkpoints.native_turn_id and r.conversation_id=conversation_checkpoints.conversation_id),''), '') where workspace_path='' and snapshot_ref<>'' and (run_id<>'' or native_turn_id<>'')`); err != nil {
		return fmt.Errorf("backfill checkpoint workspace paths: %w", err)
	}
	// 回填：Claude 系节点的 native_turn_id 就是 runID（见 createConversationCheckpoint），
	// 与 runs 表对得上才写。Codex 节点对不上，保持空串（界面退回按节点列表使用）。
	if _, err := s.db.ExecContext(ctx, `update conversation_checkpoints set run_id=native_turn_id where run_id='' and native_turn_id<>'' and exists(select 1 from runs r where r.id=conversation_checkpoints.native_turn_id and r.conversation_id=conversation_checkpoints.conversation_id)`); err != nil {
		return fmt.Errorf("backfill checkpoint run ids: %w", err)
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
	rows, err := s.db.QueryContext(r.Context(), `select id,conversation_id,parent_id,agent_id,native_session_id,native_turn_id,run_id,prompt,snapshot_path,snapshot_ref,status,created_at,rewound_at,metadata,workspace_path from conversation_checkpoints where conversation_id=? order by created_at asc,id asc`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	// 同一会话的快照目录只有那么几个，按路径缓存一次 stat 就够，不随节点数放大。
	snapshotPresence := map[string]bool{}
	items := make([]conversationCheckpoint, 0)
	for rows.Next() {
		var item conversationCheckpoint
		if err := rows.Scan(&item.ID, &item.ConversationID, &item.ParentID, &item.AgentID, &item.NativeSessionID, &item.NativeTurnID, &item.RunID, &item.Prompt, &item.SnapshotPath, &item.SnapshotRef, &item.Status, &item.CreatedAt, &item.RewoundAt, &item.Metadata, &item.WorkspacePath); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		item.HasSnapshot = checkpointSnapshotPresent(item.SnapshotPath, snapshotPresence)
		// 服务器路径不外泄：快照在不在由 hasSnapshot 表达。
		item.SnapshotPath = ""
		item.SnapshotRef = ""
		// 列表只用来让人认出"我要回到哪一轮"（界面自己还会截到 72 字），所以服务端先截一刀：
		// prompt 是那一轮的第一条用户消息原文，可能是用户粘进来的一大段日志；metadata 里还有
		// 遗留节点的 skipped 文件清单（可能很长）。整帧超了中继上限（64 KiB）时手机端拿到的是
		// 一个失败，而它把失败当成"没有节点"—— 回退入口会**静默消失**。
		item.Prompt = truncateCheckpointPrompt(item.Prompt)
		item.Metadata = ""
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": items})
}

// checkpointSnapshotPresent 判断快照是否还在磁盘上（git 快照仓库与遗留拷贝目录都是"目录"）。
func checkpointSnapshotPresent(path string, cache map[string]bool) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if present, seen := cache[path]; seen {
		return present
	}
	info, err := os.Stat(path)
	present := err == nil && info.IsDir()
	cache[path] = present
	return present
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
	// 自动编排接管的会话是只读的（与 clearConversation / 发送消息同一条）：桌面端只是把按钮
	// 禁掉了，手机与直接调用没有那层保护。
	if s.isOrchestrationConversation(r.Context(), conversationID) {
		writeError(w, http.StatusConflict, errors.New("automatic orchestration conversations are read-only"))
		return
	}
	// 从这里往下的破坏性部分（退休进程、改工作区、落库）跑在**服务运行时上下文**上，
	// 不跟 HTTP 请求走：客户端的超时（桌面 15s、手机受 agent 60s 上限）不该把 read-tree
	// 杀在半路，留下一个改了一半的工作区 + 一个仍然写着"没回退"的库。理由与 deleteConversation
	// 那条（"客户端放弃等待不能回滚破坏性工作"）同一条。
	ctx, cancel := context.WithTimeout(s.runtimeCtx, checkpointRewindTimeout)
	defer cancel()
	var projectID, projectPath, snapshotPath, snapshotRef string
	var projectRunner string
	var checkpoint conversationCheckpoint
	err := s.db.QueryRowContext(r.Context(), `select c.id,c.conversation_id,c.parent_id,c.agent_id,c.native_session_id,c.native_turn_id,c.run_id,c.prompt,c.snapshot_path,c.snapshot_ref,c.status,c.created_at,c.rewound_at,c.metadata,c.workspace_path,p.id,p.path,coalesce(nullif(p.runner_id,''),p.runner) from conversation_checkpoints c join conversations v on v.id=c.conversation_id join projects p on p.id=v.project_id where c.id=? and c.conversation_id=?`, req.CheckpointID, conversationID).Scan(&checkpoint.ID, &checkpoint.ConversationID, &checkpoint.ParentID, &checkpoint.AgentID, &checkpoint.NativeSessionID, &checkpoint.NativeTurnID, &checkpoint.RunID, &checkpoint.Prompt, &snapshotPath, &snapshotRef, &checkpoint.Status, &checkpoint.CreatedAt, &checkpoint.RewoundAt, &checkpoint.Metadata, &checkpoint.WorkspacePath, &projectID, &projectPath, &projectRunner)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("checkpoint not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	checkpoint.SnapshotPath = snapshotPath
	checkpoint.SnapshotRef = snapshotRef
	localWorkspace := projectRunner == "" || projectRunner == "windows-local" || projectRunner == "wsl-local"
	// fork 不碰工作区（只复制消息、起一条新会话），所以**先**处理它：把"没有代码快照"
	// 挡在它前面，会让远程 runner 项目的「分支」拿到一句"只能恢复对话"，而它根本不需要快照。
	if req.Fork {
		forked, err := s.forkConversationAtCheckpoint(ctx, conversationID, checkpoint)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"conversation": forked, "checkpoint": clientCheckpoint(checkpoint), "forked": true})
		return
	}
	if req.Mode != "conversation" && !localWorkspace {
		writeError(w, http.StatusConflict, errors.New("remote runner does not support local checkpoint code restore"))
		return
	}
	if req.Mode != "conversation" && strings.TrimSpace(snapshotPath) == "" {
		writeError(w, http.StatusConflict, errors.New("checkpoint has no file snapshot; conversation rewind is still available"))
		return
	}
	// 非 fork 的回退会**改工作区文件**（真回退还会删掉快照之后新建的文件），必须与
	// "接纳新消息 / 起新进程"互斥：sendMessage 走同一把锁。少了它，恢复大工作区的这几秒里
	// 新的一轮可以被接纳并开跑，两边同时动工作区 —— 而工作区锁正是为此存在的。
	// （fork 分支自己已经拿了这把锁，且 sync.Mutex 不可重入，所以放在它之后。）
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	if _, found, err := s.activeRunIDForConversation(ctx, conversationID); err != nil {
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
	nativeReverted := false
	if req.Mode != "code" && checkpoint.AgentID == "codex" && localWorkspace {
		beforeTurnID, err := s.nextCodexTurnID(ctx, conversationID, checkpoint.CreatedAt)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if beforeTurnID != "" && checkpoint.NativeSessionID != "" {
			if err := s.codexRevertThread(ctx, projectPath, checkpoint.NativeSessionID, beforeTurnID); err != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("Codex 原生对话回退失败：%w", err))
				return
			}
			// Codex 有原生回退：它的会话真的回到了那个回合，且保留下来的部分保真度更高，
			// 不需要重置会话、也不需要重建上下文。
			nativeReverted = true
		}
	}
	// 从恢复工作区到翻节点状态，整段都在会话的快照锁里：否则一次刚好在最坏时刻收尾的 run
	// 会在这两步之间插进一个新节点（它逃过这次回退，而快照可能拍在恢复过程中）。
	unlockCheckpoints := s.checkpointLocks.lock(conversationID)
	defer unlockCheckpoints()
	if req.Mode != "conversation" {
		if err := s.restoreCheckpointCode(ctx, checkpointRestoreRequest{
			ProjectID:     projectID,
			ProjectPath:   projectPath,
			WorkspacePath: checkpoint.WorkspacePath,
			Runner:        projectRunner,
			SnapshotDir:   snapshotPath,
			SnapshotRef:   snapshotRef,
			Engine:        checkpointEngineOf(checkpoint.Metadata, snapshotRef),
		}); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	// "恢复对话"的另一半：消息被时间戳挡在界面之外，但长驻 CLI 的原生会话还记得它们，
	// 下一轮 `--resume` 会把被丢掉的回合带回来。所以只要真的丢了回合（回退到最新节点是个
	// 空操作，不该动会话），就把原生会话重置，并让下一轮把保留的历史重新喂回去。
	// 顺序：重置会话 → 落库。任何一步失败，整次回退按失败返回；但注意**工作区可能已经被
	// 改过了**（恢复在前），所以"失败"不等于"什么都没发生"—— 客户端文案只说"回退失败"，
	// 不复述状态。这个窗口换来的是：绝不出现"界面回退了、模型没回退"的死角。
	if req.Mode != "code" && !nativeReverted {
		discarded, countErr := countDiscardedTurns(ctx, s.db, conversationID, checkpoint.CreatedAt)
		if countErr != nil {
			writeError(w, http.StatusInternalServerError, countErr)
			return
		}
		if discarded > 0 {
			if err := s.rebuildConversationSession(ctx, conversationID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	now := time.Now().UTC()
	// 目标节点自己的 metadata 也要合并（不是覆盖）：engine 决定"恢复代码"走哪条路，
	// 被覆盖掉之后，这个节点下次回退可能就会走错恢复路径。
	metadata := map[string]any{}
	if strings.TrimSpace(checkpoint.Metadata) != "" {
		_ = json.Unmarshal([]byte(checkpoint.Metadata), &metadata)
	}
	// 只恢复代码（mode=code）**不动对话可见性**：界面上的另一颗按钮写的是
	// 「只恢复代码 / 对话历史不变」。可见性边界与节点状态都保持原样。
	// 对话回退（含"全部"）才需要移动它们。
	if req.Mode != "code" {
		metadata["rewoundAt"] = now.Format(time.RFC3339)
	}
	metadataJSON, _ := json.Marshal(metadata)
	if req.Mode != "code" {
		if err := s.markCheckpointsRewound(ctx, conversationID, checkpoint, now, req.Mode); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		checkpoint.Status = "active"
	}
	checkpoint.Metadata = string(metadataJSON)
	writeJSON(w, http.StatusOK, map[string]any{"checkpoint": clientCheckpoint(checkpoint), "forked": false})
}

// markCheckpointsRewound 把目标节点标成落点、比它晚的全部标成已被回退，并**逐个**合并
// metadata（只加 mode / rewoundAt）。
//
// 不能拿目标节点那份 metadata 一把刷给所有节点：那里面的 engine 决定"恢复代码"走哪条路
// （git-legacy 只能用覆盖式回写，换成 git 就会把旧机制当初没能存进去的大文件/符号链接
// 当成"该删"删掉）。一次覆盖等于把之后每次回退都变成可能的数据丢失。
func (s *Server) markCheckpointsRewound(ctx context.Context, conversationID string, checkpoint conversationCheckpoint, now time.Time, mode string) error {
	rows, err := s.db.QueryContext(ctx, `select id,metadata from conversation_checkpoints where conversation_id=? and (created_at>? or (created_at=? and id>?))`, conversationID, checkpoint.CreatedAt, checkpoint.CreatedAt, checkpoint.ID)
	if err != nil {
		return err
	}
	type node struct {
		id       string
		metadata string
	}
	nodes := make([]node, 0)
	for rows.Next() {
		var item node
		if err := rows.Scan(&item.id, &item.metadata); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range nodes {
		metadata := map[string]any{}
		if strings.TrimSpace(item.metadata) != "" {
			_ = json.Unmarshal([]byte(item.metadata), &metadata)
		}
		metadata["rewoundAt"] = now.Format(time.RFC3339)
		metadata["rewoundBy"] = mode
		payload, _ := json.Marshal(metadata)
		if _, err := tx.ExecContext(ctx, `update conversation_checkpoints set status='rewound',rewound_at=?,metadata=? where id=?`, now, string(payload), item.id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `update conversation_checkpoints set status='active' where id=?`, checkpoint.ID); err != nil {
		return err
	}
	// 可见性边界与状态翻转放在同一个事务里：落点时间决定"藏到哪儿"，回退时刻决定
	// "从哪儿开始重新显示"（回退之后新发生的回合，哪怕失败/被停止，都要照常显示）。
	if _, err := tx.ExecContext(ctx, `update conversations set checkpoint_hidden_after=?,checkpoint_rewound_at=? where id=?`, checkpoint.CreatedAt, now, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

// clientCheckpoint 去掉不该外泄的服务器路径（与列表接口同一条规矩：快照在不在由
// hasSnapshot 表达，工作区路径与仓库位置都留在服务端）。
func clientCheckpoint(checkpoint conversationCheckpoint) conversationCheckpoint {
	checkpoint.HasSnapshot = checkpointSnapshotPresent(checkpoint.SnapshotPath, map[string]bool{})
	checkpoint.SnapshotPath = ""
	checkpoint.SnapshotRef = ""
	checkpoint.WorkspacePath = ""
	return checkpoint
}

// checkpointEngineOf 判断节点的快照引擎：新节点写 snapshot_ref；转换过的遗留节点靠 metadata 里的
// 引擎标记区分（它们的快照树可能缺文件，只能用覆盖式回写）；其余是升级前的拷贝目录。
func checkpointEngineOf(metadataJSON, snapshotRef string) string {
	if strings.TrimSpace(snapshotRef) == "" {
		return checkpointEngineLegacy
	}
	var metadata struct {
		Engine string `json:"engine"`
	}
	if json.Unmarshal([]byte(metadataJSON), &metadata) == nil && metadata.Engine == checkpointEngineGitLegacy {
		return checkpointEngineGitLegacy
	}
	return checkpointEngineGit
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
	if _, err := tx.ExecContext(ctx, `insert into conversation_checkpoints (id,conversation_id,parent_id,agent_id,native_session_id,native_turn_id,run_id,prompt,snapshot_path,snapshot_ref,workspace_path,status,metadata,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, childCheckpointID, child.ID, "", child.AgentID, child.AgentSessionID, checkpoint.NativeTurnID, checkpoint.RunID, checkpoint.Prompt, checkpoint.SnapshotPath, checkpoint.SnapshotRef, checkpoint.WorkspacePath, "active", metadata, checkpoint.CreatedAt); err != nil {
		return Conversation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return child, nil
}

func (s *Server) createConversationCheckpoint(ctx context.Context, conversationID, runID, prompt string) error {
	// 整个"拍快照 + 写节点行 + 裁旧节点"都在会话锁里：回退会在锁里改节点状态，若节点行能
	// 在两次状态更新之间插进来，它就是"回退之后的新节点"，会逃过这次回退（而且它的快照可能
	// 拍在恢复工作区的过程中）。恢复/转换也拿同一把锁（见 checkpointLockSet）。
	unlock := s.checkpointLocks.lock(conversationID)
	defer unlock()
	var projectID, projectPath, agentID, nativeSessionID, projectRunner string
	if err := s.db.QueryRowContext(ctx, `select p.id,p.path,c.agent_id,coalesce(c.agent_session_id,c.claude_session_id,''),coalesce(nullif(p.runner_id,''),p.runner) from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&projectID, &projectPath, &agentID, &nativeSessionID, &projectRunner); err != nil {
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
	// 这一轮**实际跑在哪个目录**：会话可以切到隔离工作区（<project> 的兄弟目录），那时快照
	// 必须拍那个目录。拿项目根去拍，快照里就完全是另一份代码；恢复时更糟 —— 会把项目根上
	// 用户自己的改动当成"快照之后新建的文件"删掉。runs.workspace_path 是权威来源。
	workspacePath := projectPath
	var runWorkspacePath string
	_ = s.db.QueryRowContext(ctx, `select coalesce(workspace_path,'') from runs where id=?`, runID).Scan(&runWorkspacePath)
	if strings.TrimSpace(runWorkspacePath) != "" {
		workspacePath = runWorkspacePath
	}
	var snapshotPath, snapshotRef string
	var metadata map[string]any
	if projectRunner == "" || projectRunner == "windows-local" || projectRunner == "wsl-local" {
		var err error
		snapshotPath, snapshotRef, metadata, err = s.createWorkspaceSnapshot(ctx, projectID, workspacePath, projectRunner, conversationID)
		if err != nil {
			// 快照失败不影响节点本身：会话回退仍然可用，"恢复代码"会给出明确原因。
			log.Printf("create workspace snapshot for conversation %s: %v", conversationID, err)
			snapshotPath, snapshotRef = "", ""
			metadata = map[string]any{"snapshotUnavailable": true, "snapshotError": err.Error()}
		}
	} else {
		metadata = map[string]any{"snapshotUnavailable": true, "runner": projectRunner}
	}
	id := uuid.NewString()
	metadataJSON, _ := json.Marshal(metadata)
	_, err := s.db.ExecContext(ctx, `insert into conversation_checkpoints (id,conversation_id,parent_id,agent_id,native_session_id,native_turn_id,run_id,prompt,snapshot_path,snapshot_ref,workspace_path,status,metadata,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, conversationID, parentID, agentID, nativeSessionID, nativeTurnID, runID, prompt, snapshotPath, snapshotRef, workspacePath, "active", string(metadataJSON), time.Now().UTC())
	if err != nil {
		return err
	}
	s.pruneConversationCheckpoints(ctx, conversationID)
	return nil
}

// pruneConversationCheckpoints 只保留最近 N 个节点与当前节点。删的是行；影子仓库按内容去重，
// 对象交给 git 自己的 gc，遗留拷贝目录则随行一起删掉（那种目录没有去重，一份就是一份磁盘）。
func (s *Server) pruneConversationCheckpoints(ctx context.Context, conversationID string) {
	type node struct {
		id     string
		status string
		path   string
		ref    string
	}
	rows, err := s.db.QueryContext(ctx, `select id,status,snapshot_path,snapshot_ref from conversation_checkpoints where conversation_id=? order by created_at desc,id desc`, conversationID)
	if err != nil {
		log.Printf("prune checkpoints for conversation %s: %v", conversationID, err)
		return
	}
	nodes := make([]node, 0)
	for rows.Next() {
		var item node
		if err := rows.Scan(&item.id, &item.status, &item.path, &item.ref); err != nil {
			rows.Close()
			log.Printf("prune checkpoints for conversation %s: %v", conversationID, err)
			return
		}
		nodes = append(nodes, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("prune checkpoints for conversation %s: %v", conversationID, err)
		return
	}
	// 当前落点 = 最新的 active 节点（回退过的会话里它可能很老，但它就是"我在哪"）。
	//
	// ⚠️ 不能用 `status == "active"` 当豁免：每个新节点插入时都是 active，没人回退过时
	// **所有**节点都是 active —— 那个豁免等于让保留策略永远删不动，节点行无上限地长。
	currentID := ""
	_ = s.db.QueryRowContext(ctx, `select id from conversation_checkpoints where conversation_id=? and status='active' order by created_at desc,id desc limit 1`, conversationID).Scan(&currentID)
	removed := 0
	for index, item := range nodes {
		// 最近 N 个与当前落点一定保留。
		if index < checkpointRetentionPerConversation || item.id == currentID {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `delete from conversation_checkpoints where id=?`, item.id); err != nil {
			log.Printf("prune checkpoint %s: %v", item.id, err)
			continue
		}
		removed++
		// 只有"一节点一目录"的遗留拷贝快照能随行删除；影子仓库是整会话共用的，删目录会把
		// 其它节点的快照一起带走。
		if item.ref == "" && checkpointIsLegacyCopyDir(item.path) {
			if err := os.RemoveAll(item.path); err != nil {
				log.Printf("remove legacy checkpoint snapshot %s: %v", item.path, err)
			}
		}
	}
	if removed > 0 {
		log.Printf("pruned %d checkpoint nodes for conversation %s", removed, conversationID)
	}
}

// checkpointIsLegacyCopyDir 判断路径是不是"一个节点一份"的遗留拷贝目录（而不是共用仓库）。
func checkpointIsLegacyCopyDir(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	base := filepath.Base(path)
	return base != checkpointShadowRepoName && !strings.HasPrefix(base, checkpointImportRepoPrefix)
}

// ── 影子 git 仓库 ────────────────────────────────────────────────────────────
//
// 每轮成功回合结束后，把工作区当前状态记进一个**只属于这个会话**的 git 仓库：
//   - .gitignore 命中的文件不进快照（node_modules / target / dist…），这是磁盘开销的大头；
//   - 对象按内容去重，未改动的文件不重复占空间，每轮只多存"这一轮改了什么"；
//   - 恢复走 read-tree：快照之后新建的文件会被删掉，是**真回退**而不是"把旧文件写回去"；
//   - 仓库放在数据目录下，不进用户仓库、不改 .git、不加忽略规则、不随项目推送。
//
// 前提是项目所在环境有 git —— 与 Git 工作台同源（见 gitRunnerForProject）：WSL 项目由发行版内
// 的 git 执行，Windows 项目由本机 git 执行。远程 runner 项目仍不建快照。

func (s *Server) checkpointStorageRoot() string {
	if s.config.DataDir != "" {
		return s.config.DataDir
	}
	return filepath.Dir(s.config.DatabasePath)
}

// checkpointShadowDir 返回会话影子仓库的目录（在数据目录下）。
func checkpointShadowDir(storageRoot, conversationID string) string {
	return filepath.Join(storageRoot, "checkpoints", conversationID, checkpointShadowRepoName)
}

// checkpointGitPaths 把影子仓库与工作区两侧都换算成目标 runner 能直接用的路径。
// WSL 项目的 git 跑在发行版内部：工作区要给 Linux 路径（由 UNC 转换），影子仓库在数据目录
// （Windows 盘）上，同样要换成 /mnt/<盘> 形式，否则发行版里的 git 认不出来。
func (s *Server) checkpointGitPaths(projectPath, projectRunner, gitDir string) (string, string, error) {
	switch projectRunner {
	case "", "windows-local":
		return projectPath, gitDir, nil
	case "wsl-local":
		runner, ok := s.wslAgentRunner().(*wslAgentRunner)
		if !ok || runner == nil {
			return "", "", errors.New("WSL runner is unavailable")
		}
		workTree, ok := uncToWslPath(projectPath, runner.distro)
		if !ok || workTree == "" {
			// 与 gitRunnerForProject 一致：项目路径不是 \\wsl$ 形式时退回本机 git
			// （Git 工作台对同一情形就是这么处理的）。
			return projectPath, gitDir, nil
		}
		return workTree, windowsToWSLMntPath(gitDir), nil
	default:
		return "", "", fmt.Errorf("runner %s does not support local checkpoints", projectRunner)
	}
}

// clearStaleCheckpointRepoLock 清掉快照仓库里遗留的 index.lock。
//
// 这些仓库只有我们写，而且同一仓库上的操作在构造上就是串行的（快照在 run 收尾、恢复拿
// projectLifecycleMu、转换在启动维护里），所以**命令开始前还存在的锁一定是死的**：
// 上一次被超时/退出打断的 git 进程（Windows 上被 kill 不会走清理路径）留下的。
// 不清掉的话，那个节点会永远转换失败 —— 而失败是静默的（只写日志、下次再来），
// 真机上就出现过"某份快照转了十次都失败"。
func clearStaleCheckpointRepoLock(gitDir string) {
	lock := filepath.Join(gitDir, "index.lock")
	if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[checkpoint] remove stale index lock %s: %v", lock, err)
	}
}

// checkpointGitError 把 git 自己的 stderr 带进快照/转换的错误里。gitCommandError.Error() 只说
// "Git <args[0]> failed"，而我们的 args[0] 是 --git-dir=…，于是日志里既看不出是哪条子命令、
// 也看不出 git 说了什么 —— 上面那个 index.lock 就是被这层信息缺失藏住的。
func checkpointGitError(action string, err error) error {
	var gitErr *gitCommandError
	if errors.As(err, &gitErr) {
		if detail := strings.TrimSpace(gitErr.stderr); detail != "" {
			return fmt.Errorf("%s: %w (%s)", action, err, detail)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

// initCheckpointRepo 建好（或复用）影子仓库并写入快照必需的配置。
// gitDir 是本机路径（用于存在性判断），gitDirArg 是目标 runner 能直接用的形式（WSL 下是 /mnt/…）。
//
// 判据是"配置写全了没有"而不是"仓库在不在"：建库与写配置之间崩溃过一次的话，只看 HEAD
// 会让这个仓库永远缺 user.name，之后每次提交都失败（而且是静默降级成"没有代码快照"）。
func initCheckpointRepo(ctx context.Context, runner GitRunner, repo, gitDir, gitDirArg string, base []string) error {
	if checkpointRepoConfigured(gitDir) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err := runner.runGit(ctx, repo, "init", "--quiet", "--bare", gitDirArg); err != nil {
			return fmt.Errorf("init checkpoint repository: %w", err)
		}
	}
	// 配置直接追加进仓库的 config 文件，不走 `git config`：每次调用都要 spawn 一个进程
	// （Windows 上单次 150–200ms，WSL 更贵），而这四条配置每个会话只写一次 —— 快照是在
	// 每轮 run 收尾时同步跑的，进程数直接变成用户等待。
	//
	// core.autocrlf=false 关掉的是"按平台换行转换"。注意项目自己的 .gitattributes 若写了
	// `text=auto`，git 仍会按它自己的规则在检出时成形 —— 那是用户仓库自己的约定，恢复出来的
	// 文件与"用户自己的 git checkout 会写出的形态"一致，通常不会产生额外 diff。
	if err := appendCheckpointRepoConfig(gitDir); err != nil {
		return fmt.Errorf("configure checkpoint repository: %w", err)
	}
	return nil
}

// checkpointRepoConfig 是影子仓库需要的那几行配置（git config 的 INI 格式，重复的 [core]
// 段是合法的，后面的取值生效）。
const checkpointRepoConfig = "\n[core]\n\tautocrlf = false\n\tsafecrlf = false\n[user]\n\tname = Milevia\n\temail = milevia@localhost\n"

func appendCheckpointRepoConfig(gitDir string) error {
	file, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(checkpointRepoConfig)
	return err
}

// checkpointRepoConfigured 用仓库自己的 config 作为"配置已写入"的标记。
func checkpointRepoConfigured(gitDir string) bool {
	payload, err := os.ReadFile(filepath.Join(gitDir, "config"))
	return err == nil && strings.Contains(string(payload), "Milevia")
}

// readCheckpointRepoRef 读 HEAD 指向的提交。刚提交完的 ref 一定是散文件，所以常规路径是两次
// 文件读而不是再 spawn 一次 `git rev-parse`；ref 被 gc 打进 packed-refs 时退回 rev-parse。
func readCheckpointRepoRef(ctx context.Context, runner GitRunner, repo, gitDir, gitDirArg string, base []string) (string, error) {
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err == nil {
		line := strings.TrimSpace(string(head))
		if strings.HasPrefix(line, "ref: ") {
			refPath := filepath.Join(gitDir, filepath.FromSlash(strings.TrimSpace(strings.TrimPrefix(line, "ref: "))))
			if payload, err := os.ReadFile(refPath); err == nil {
				if value := strings.TrimSpace(string(payload)); isFullGitObjectID(value) {
					return value, nil
				}
			}
		}
	}
	output, err := runner.runGit(ctx, repo, append(append([]string{}, base...), "rev-parse", "HEAD")...)
	if err != nil {
		return "", fmt.Errorf("resolve workspace snapshot: %w", err)
	}
	value := strings.TrimSpace(string(output))
	if !isFullGitObjectID(value) {
		return "", errors.New("checkpoint repository returned an invalid revision")
	}
	return value, nil
}

// checkpointGitRunner 返回目标项目的 git runner，但把命令预算放宽到快照档（见 checkpointGitTimeout）。
// 只换 timeout、复用同一个 backend：本地 / WSL 两条路径的分支逻辑仍只有 gitRunnerForProject 一处。
func (s *Server) checkpointGitRunner(ctx context.Context, projectID string) (GitRunner, string, error) {
	runner, repo, err := s.gitRunnerForProject(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	return withCheckpointGitTimeout(runner, checkpointGitTimeout), repo, nil
}

// withCheckpointGitTimeout 把 git runner 的命令预算换成 timeout。
//
// 注意预算挂在 **backend** 上（localGitBackend / wslGitBackend 各自的 timeout 字段才是
// runGit 真正用的那个），所以必须重建 backend —— 只换 gitCLIRunner.timeout 是无效的，
// 症状就是"改了超时，真机上仍然 30 秒就 deadline exceeded"。
func withCheckpointGitTimeout(runner GitRunner, timeout time.Duration) GitRunner {
	cli, ok := runner.(*gitCLIRunner)
	if !ok {
		return runner
	}
	switch backend := cli.backend.(type) {
	case *localGitBackend:
		return &gitCLIRunner{timeout: timeout, backend: &localGitBackend{timeout: timeout}}
	case *wslGitBackend:
		return &gitCLIRunner{timeout: timeout, backend: &wslGitBackend{distro: backend.distro, timeout: timeout}}
	default:
		// SSH runner 不建快照也不恢复代码（工作区在远端），没有需要放宽预算的路径。
		return runner
	}
}

func (s *Server) createWorkspaceSnapshot(ctx context.Context, projectID, projectPath, projectRunner, conversationID string) (string, string, map[string]any, error) {
	runner, repo, err := s.checkpointGitRunner(ctx, projectID)
	if err != nil {
		return "", "", nil, err
	}
	gitDir := checkpointShadowDir(s.checkpointStorageRoot(), conversationID)
	workTree, gitDirArg, err := s.checkpointGitPaths(projectPath, projectRunner, gitDir)
	if err != nil {
		return "", "", nil, err
	}
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		return "", "", nil, err
	}
	base := []string{"--git-dir=" + gitDirArg, "--work-tree=" + workTree}
	git := func(args ...string) ([]byte, error) {
		return runner.runGit(ctx, repo, append(append([]string{}, base...), args...)...)
	}
	if err := initCheckpointRepo(ctx, runner, repo, gitDir, gitDirArg, base); err != nil {
		return "", "", nil, err
	}
	clearStaleCheckpointRepoLock(gitDir)
	if _, err := git("add", "-A", "--", ":/"); err != nil {
		return "", "", nil, checkpointGitError("stage workspace snapshot", err)
	}
	// --allow-empty：这一轮没改任何文件时也要留下节点（它仍然是一个可以回到的时间点）。
	if _, err := git("commit", "--quiet", "--no-verify", "--allow-empty", "-m", "checkpoint "+time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", "", nil, checkpointGitError("commit workspace snapshot", err)
	}
	ref, err := readCheckpointRepoRef(ctx, runner, repo, gitDir, gitDirArg, base)
	if err != nil {
		return "", "", nil, err
	}
	return gitDir, ref, map[string]any{"engine": checkpointEngineGit}, nil
}

type checkpointRestoreRequest struct {
	ProjectID string
	// ProjectPath 是项目根（解析 git runner 用）；WorkspacePath 是**这个节点当时跑在哪个目录**
	// （会话切到隔离工作区时不是项目根）。恢复必须回到 WorkspacePath —— 否则会把项目根上
	// 用户自己的改动当成"快照之后新建的文件"删掉，而被切换到的那个工作区一动不动。
	ProjectPath   string
	WorkspacePath string
	Runner        string
	SnapshotDir   string
	SnapshotRef   string
	Engine        string
}

func (s *Server) restoreCheckpointCode(ctx context.Context, req checkpointRestoreRequest) error {
	if strings.TrimSpace(req.SnapshotDir) == "" {
		return errors.New("checkpoint has no file snapshot")
	}
	if _, err := os.Stat(req.SnapshotDir); err != nil {
		return errors.New("checkpoint snapshot unavailable")
	}
	workTree := strings.TrimSpace(req.WorkspacePath)
	if workTree == "" {
		// 升级前的节点（或工作区从未记录）：那时拍的就是项目根。
		workTree = req.ProjectPath
	}
	if _, err := os.Stat(workTree); err != nil {
		return errors.New("checkpoint workspace unavailable")
	}
	req.WorkspacePath = workTree
	switch req.Engine {
	case checkpointEngineGit, checkpointEngineGitLegacy:
		return s.restoreGitCheckpointSnapshot(ctx, req)
	default:
		return restoreLegacyCheckpointSnapshot(req.SnapshotDir, workTree)
	}
}

func (s *Server) restoreGitCheckpointSnapshot(ctx context.Context, req checkpointRestoreRequest) error {
	runner, repo, err := s.checkpointGitRunner(ctx, req.ProjectID)
	if err != nil {
		return err
	}
	workTree, gitDirArg, err := s.checkpointGitPaths(req.WorkspacePath, req.Runner, req.SnapshotDir)
	if err != nil {
		return err
	}
	base := []string{"--git-dir=" + gitDirArg, "--work-tree=" + workTree}
	git := func(args ...string) ([]byte, error) {
		return runner.runGit(ctx, repo, append(append([]string{}, base...), args...)...)
	}
	// 会话级串行由调用方（rewindConversation）持有 —— 它要覆盖"恢复 + 翻状态"整段。
	clearStaleCheckpointRepoLock(req.SnapshotDir)
	if req.Engine == checkpointEngineGitLegacy {
		// 转换自遗留拷贝的快照树可能缺文件（旧机制跳过大文件与符号链接），只覆盖回写：
		// 宁可留下多出来的文件，也不能把当时没能存进去的东西当"该删"删掉。
		if _, err := git("checkout", req.SnapshotRef, "--", "."); err != nil {
			return fmt.Errorf("checkpoint snapshot unavailable: %w", err)
		}
		return nil
	}
	// 真回退：先把工作区现状记进索引，再让工作区对齐目标树。这样"快照之后新建的文件"
	// 也会被删掉，而 .gitignore 命中的文件从不在索引里，永远不会被动到。
	if _, err := git("add", "-A", "--", ":/"); err != nil {
		return fmt.Errorf("stage workspace before restore: %w", err)
	}
	if _, err := git("read-tree", "--reset", "-u", req.SnapshotRef); err != nil {
		return fmt.Errorf("checkpoint snapshot unavailable: %w", err)
	}
	return nil
}

// ── 启动期维护 ──────────────────────────────────────────────────────────────

// sweepCheckpointSnapshots 在启动后清理与转换快照，三件事各自幂等：
//  1. 会话已不存在（删除 / 历史裁剪）→ 整个目录删掉；
//  2. 遗留拷贝目录被转换成功 → 删掉目录本身，腾出真金白银的磁盘；
//  3. 还有遗留拷贝节点 → 转成 git 快照（去重 + 压缩，实测 1.15 GB → 百 MB 量级）。
//
// 任何一步失败都只是"留到下次启动"，不会丢数据：DB 行的指针在 git 提交成功之后才改。
func (s *Server) sweepCheckpointSnapshots(ctx context.Context) {
	// 整段快照维护都避开"刚打开应用"那一小段：转换是分钟级磁盘活，清理也可能是一次几万
	// 个文件的 RemoveAll（转换中断 / 崩溃留下的拷贝目录），两者都不该和用户的第一条消息
	// 抢同一块磁盘。清理最多等 checkpointCleanupIdleWait 的空闲窗口，再等不到也做 ——
	// 否则持续有运行的机器上孤儿目录永远无人回收；转换仍只挑空闲窗口（见下）。
	cleanupWaitCtx, cancelCleanupWait := context.WithTimeout(ctx, s.checkpointSweepDelay+s.checkpointCleanupWait)
	waitErr := s.waitForIdleWindow(cleanupWaitCtx, s.checkpointSweepDelay)
	cancelCleanupWait()
	if waitErr != nil && !errors.Is(waitErr, context.DeadlineExceeded) {
		// 正常退出/超时会取消 ctx；那不算"维护被跳过"，别往日志里灌假故障。
		if !errors.Is(waitErr, context.Canceled) {
			log.Printf("[checkpoint] startup sweep deferred: %v", waitErr)
		}
		return
	}
	if ctx.Err() != nil {
		// 父 ctx（维护总预算）真的用完了：上面的 DeadlineExceeded 不代表可以继续。
		return
	}
	root := filepath.Join(s.checkpointStorageRoot(), "checkpoints")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("[checkpoint] read snapshot root: %v", err)
		}
		return
	}
	rows, err := s.db.QueryContext(ctx, `select c.id,coalesce(p.path,'') from conversations c left join projects p on p.id=c.project_id`)
	if err != nil {
		log.Printf("[checkpoint] list conversations: %v", err)
		return
	}
	projects := map[string]string{}
	for rows.Next() {
		var conversationID, projectPath string
		if err := rows.Scan(&conversationID, &projectPath); err != nil {
			rows.Close()
			log.Printf("[checkpoint] list conversations: %v", err)
			return
		}
		projects[conversationID] = projectPath
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[checkpoint] list conversations: %v", err)
		return
	}
	// 两趟：先做便宜的清理（删孤儿、删没人引用的目录），再做贵的转换。反过来写的话，
	// 转换把这次启动的预算用光就会 return，清理被饿死（清理是每个启动都该做完的活）。
	type legacyCandidate struct {
		conversationID string
		nodeIDs        []string
		path           string
	}
	candidates := make([]legacyCandidate, 0)
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		conversationID := entry.Name()
		conversationDir := filepath.Join(root, conversationID)
		projectPath, known := projects[conversationID]
		if !known {
			// 会话没了，但**别的会话**可能还引用着这里的仓库：从节点分出去的分支会话会原样
			// 复制 snapshot_path（它的节点就是从这条会话的那份快照长出来的）。删了它，
			// 分支会话的节点会永远失去代码快照 —— 而且是静默的。
			if inUse, err := s.checkpointPathsUnder(ctx, conversationDir); err != nil {
				log.Printf("[checkpoint] inspect orphan snapshot dir %s: %v", conversationDir, err)
				continue
			} else if inUse {
				log.Printf("[checkpoint] keep snapshot dir %s: still referenced by another conversation's node", conversationDir)
				continue
			}
			if err := os.RemoveAll(conversationDir); err == nil {
				removed++
			} else {
				log.Printf("[checkpoint] remove orphan snapshot dir %s: %v", conversationDir, err)
			}
			continue
		}
		if projectPath == "" {
			continue
		}
		children, err := os.ReadDir(conversationDir)
		if err != nil {
			log.Printf("[checkpoint] read snapshot dir %s: %v", conversationDir, err)
			continue
		}
		for _, child := range children {
			if !child.IsDir() || child.Name() == checkpointShadowRepoName {
				continue
			}
			path := filepath.Join(conversationDir, child.Name())
			if strings.HasPrefix(child.Name(), checkpointImportRepoPrefix) {
				// 转换产物：还有行指着它就留着，否则是转换后被裁掉的节点留下的壳。
				if referenced, err := s.checkpointPathReferenced(ctx, path); err != nil {
					log.Printf("[checkpoint] inspect snapshot repo %s: %v", path, err)
				} else if !referenced {
					if err := os.RemoveAll(path); err == nil {
						removed++
					}
				}
				continue
			}
			nodeIDs, err := s.legacyCheckpointNodes(ctx, path)
			if err != nil {
				log.Printf("[checkpoint] inspect snapshot dir %s: %v", path, err)
				continue
			}
			if len(nodeIDs) == 0 {
				// 转换成功后留下的空壳，或升级前就没人引用的孤儿。
				if err := os.RemoveAll(path); err == nil {
					removed++
				}
				continue
			}
			candidates = append(candidates, legacyCandidate{conversationID: conversationID, nodeIDs: nodeIDs, path: path})
		}
	}
	converted := 0
	for _, candidate := range candidates {
		if converted >= checkpointImportPerStartup {
			log.Printf("[checkpoint] conversion budget for this startup is used up; %d legacy snapshots (starting with %s) stay until the next start", len(candidates)-converted, candidate.path)
			break
		}
		// 转换要整目录读一遍再压缩（真机实测单份 186 MB 要 6 分钟），是有代价的活：
		// 开工前再确认一次"现在没人用"（上面的静默期可能已经过去很久）。
		// 等不到就留到下次启动。
		if err := s.waitForIdleWindow(ctx, 0); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[checkpoint] conversion deferred: %v", err)
			}
			break
		}
		started := time.Now()
		// 开始之后用户随时可能发消息：这一步是分钟级磁盘活，必须能被"有人开始干活"打断，
		// 否则它就会和首条消息的 CLI 冷启动抢同一块磁盘/页缓存。
		// 打断不损失什么：下一轮启动会拿同一份目录重来（转换本身是幂等的）。
		convertCtx, stopWatching := s.cancelWhenRunsStart(ctx, checkpointSweepWatchInterval)
		err := s.convertLegacyCheckpointSnapshot(convertCtx, candidate.conversationID, candidate.nodeIDs, candidate.path)
		// stopWatching 会无条件取消 convertCtx，所以"是不是被运行打断"必须由它
		// 自己回报；事后看 convertCtx.Err() 永远非 nil，会把任何真实失败都记成暂停。
		interrupted := stopWatching()
		if err != nil {
			// 用"是否被打断"而不是 errors.Is：中途撤手时 git 报的是"signal: killed"
			// 之类的包装错误，认错对象就会把"有人开始干活了"误记成转换失败。
			if interrupted {
				log.Printf("[checkpoint] conversion of %s paused: a run started; it resumes on a later start", candidate.path)
				break
			}
			// 停机/维护预算耗尽同样是取消，不是转换失败：错误可能是被包装过的
			// "signal: killed"，用父 ctx 判断，避免把正常退出记成故障。
			if ctx.Err() != nil {
				break
			}
			log.Printf("[checkpoint] convert legacy snapshot %s: %v", candidate.path, err)
			continue
		}
		converted++
		log.Printf("[checkpoint] converted legacy snapshot %s in %s", candidate.path, time.Since(started).Round(time.Second))
	}
	if converted > 0 || removed > 0 {
		log.Printf("[checkpoint] startup sweep converted=%d removed=%d", converted, removed)
	}
}

// cancelWhenRunsStart 返回一个"一发现有运行开始就取消"的派生 ctx。
//
// 用来给遗留快照转换这类分钟级磁盘活兜底：它只在真正空闲时开工，但开工之后用户随时
// 可能发消息。旧实现只检查开工前那一刻，于是转换会和首条消息的 CLI 冷启动抢同一块
// 磁盘/页缓存。返回的 stop 必须在转换结束后调用，并回报"是不是因为新运行而取消"——
// stop 自身也会无条件取消 ctx，调用方不能事后用 ctx.Err() 去判断原因。
func (s *Server) cancelWhenRunsStart(ctx context.Context, interval time.Duration) (context.Context, func() bool) {
	if interval <= 0 {
		interval = time.Second
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	// interrupted 由 watcher goroutine 写、调用方在 <-done 之后读；close(done)
	// 建立 happens-before，不需要原子量。
	interrupted := false
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if count, err := s.activeRunCount(runCtx); err == nil && count > 0 {
					interrupted = true
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	stop := func() bool {
		once.Do(func() {
			cancel()
			<-done
		})
		return interrupted
	}
	return runCtx, stop
}

// legacyCheckpointNodes 找出**所有**还在引用这个遗留拷贝目录的节点（快照创建早于引擎升级的节点）。
//
// 必须返回全部：从节点分出来的会话会原样复制 snapshot_path/ref，于是两条行可以指向同一份
// 拷贝目录。只重指一条、然后删掉目录，另一条就永远失去代码快照了（而且是静默的）。
func (s *Server) legacyCheckpointNodes(ctx context.Context, snapshotPath string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select id from conversation_checkpoints where snapshot_path=? and snapshot_ref='' order by created_at asc,id asc`, snapshotPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// checkpointPathsUnder 判断是否还有任何节点引用 dir 之下的路径。行数很少（每会话至多几十个），
// 直接读出来做前缀比较，比在 SQL 里给路径做 LIKE 转义稳。
func (s *Server) checkpointPathsUnder(ctx context.Context, dir string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `select snapshot_path from conversation_checkpoints where snapshot_path<>''`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	prefix := strings.TrimRight(dir, string(os.PathSeparator)) + string(os.PathSeparator)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return false, err
		}
		if path == dir || strings.HasPrefix(path, prefix) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Server) checkpointPathReferenced(ctx context.Context, snapshotPath string) (bool, error) {
	var referenced bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from conversation_checkpoints where snapshot_path=?)`, snapshotPath).Scan(&referenced)
	return referenced, err
}

// convertLegacyCheckpointSnapshot 把一份遗留拷贝快照转成 git 提交，然后删掉拷贝目录。
// 顺序刻意如此：git 提交先成功、DB 指针后更新、目录最后删 —— 任何一步中断都只是"下次再来"。
//
// 用**本机 git**：源是数据目录里的 Windows 目录（遗留快照是 Go 进程按 UNC 读工作区拷出来的），
// 所以哪怕项目在 WSL 里也不必进发行版；产物是普通 git 对象，之后用哪个 git 读都行。
// 仓库按节点独立（imported-<id>.git），与整会话共用的影子仓库互不干扰。
func (s *Server) convertLegacyCheckpointSnapshot(ctx context.Context, conversationID string, nodeIDs []string, legacyDir string) error {
	if len(nodeIDs) == 0 {
		return errors.New("no checkpoint rows reference this snapshot directory")
	}
	// 仓库名取第一条（稳定）：同一份拷贝目录被多条行引用时，它们共用这一个导入仓库。
	nodeID := nodeIDs[0]
	gitDir := filepath.Join(filepath.Dir(legacyDir), checkpointImportRepoPrefix+nodeID+".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		return err
	}
	// 本机 git + 转换档预算：源是数据目录里的 Windows 目录，转换要整目录读一遍并压缩。
	runner := withCheckpointGitTimeout(newGitRunner(), checkpointImportGitTimeout)
	base := []string{"--git-dir=" + gitDir, "--work-tree=" + legacyDir}
	if err := initCheckpointRepo(ctx, runner, legacyDir, gitDir, gitDir, base); err != nil {
		return err
	}
	git := func(args ...string) ([]byte, error) {
		return runner.runGit(ctx, legacyDir, append(append([]string{}, base...), args...)...)
	}
	unlock := s.checkpointLocks.lock(conversationID)
	defer unlock()
	clearStaleCheckpointRepoLock(gitDir)
	// -f：遗留拷贝快照里有 node_modules 这类被 .gitignore 命中的文件（旧机制不看忽略规则），
	// 转换要照单全收，否则转换完的快照会比原快照少文件。
	if _, err := git("add", "-A", "-f", "--", ":/"); err != nil {
		return checkpointGitError("stage legacy snapshot", err)
	}
	if _, err := git("commit", "--quiet", "--no-verify", "--allow-empty", "-m", "legacy checkpoint import"); err != nil {
		return checkpointGitError("commit legacy snapshot", err)
	}
	ref, err := readCheckpointRepoRef(ctx, runner, legacyDir, gitDir, gitDir, base)
	if err != nil {
		return err
	}
	// 保留创建时的读数（fileCount / skipped）：引擎换了不代表当初存了什么这件事没意义。
	metadata := map[string]any{}
	var previous string
	if err := s.db.QueryRowContext(ctx, `select metadata from conversation_checkpoints where id=?`, nodeID).Scan(&previous); err == nil && strings.TrimSpace(previous) != "" {
		_ = json.Unmarshal([]byte(previous), &metadata)
	}
	metadata["engine"] = checkpointEngineGitLegacy
	metadata["convertedFrom"] = checkpointEngineLegacy
	metadata["convertedAt"] = time.Now().UTC().Format(time.RFC3339)
	metadataJSON, _ := json.Marshal(metadata)
	for _, id := range nodeIDs {
		if _, err := s.db.ExecContext(ctx, `update conversation_checkpoints set snapshot_path=?,snapshot_ref=?,metadata=? where id=? and snapshot_ref=''`, gitDir, ref, string(metadataJSON), id); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(legacyDir); err != nil {
		log.Printf("[checkpoint] remove converted snapshot dir %s: %v", legacyDir, err)
	}
	return nil
}

// ── 上下文重建（"恢复对话"的另一半）─────────────────────────────────────────
//
// 截断消息只是把界面上的内容藏起来：长驻 CLI 的原生会话里那些回合还在，下一轮照样
// `--resume`。于是用户看到的是"回到了第 N 轮"，模型却仍然记得并引用被丢掉的第 N+1 轮
// 做过的事 —— 而工作区已经按那一刻恢复了，两边对不上（模型会说"那个文件我已经创建了"，
// 而它并不存在）。
//
// 所以回退到某个节点时，只要**真的丢了回合**：把原生会话重置（新的 session id +
// 未初始化），并在下一轮把保留下来的对话历史重新喂回去。模型因此真的只从那一刻继续，
// 而工作区也正好是那一刻。
//
// Codex 走另一条路：它有原生的 thread/revert（真回退，保真度更高），那条路成功时
// 不再重置会话、也不再重建。

// contextRebuildBudgetChars 是喂回历史时的字符预算（约 1.2 万字）。工作区已经恢复成那一刻的
// 样子，模型不缺事实，缺的是"我们之前干了什么"这条线索；给它一个够长的尾巴即可，
// 不能把整条会话（可能几十万字）原样塞回去。
const contextRebuildBudgetChars = 48000

// countDiscardedTurns 数这个节点之后还剩几轮（顶层用户消息）。它是"这次回退到底丢了东西没有"
// 的判据：回退到最新节点是个空操作，那时不该重置会话、也不该重建上下文。
func countDiscardedTurns(ctx context.Context, q checkpointQueryer, conversationID string, createdAt time.Time) (int, error) {
	var count int
	err := q.QueryRowContext(ctx, `select count(*) from messages where conversation_id=? and parent_tool_use_id='' and role='user' and created_at > ?`, conversationID, createdAt).Scan(&count)
	return count, err
}

// rebuildConversationSession 重置原生会话身份并置位"下一轮重建上下文"。
// 只改身份与标志：消息、节点、工作区都不动。
func (s *Server) rebuildConversationSession(ctx context.Context, conversationID string) error {
	sessionID := uuid.NewString()
	_, err := s.db.ExecContext(ctx, `update conversations set agent_session_id=?, claude_session_id=case when agent_id='claude-code' then ? else claude_session_id end, agent_initialized=0, claude_initialized=0, pending_context_rebuild=1 where id=?`, sessionID, sessionID, conversationID)
	return err
}

// clearContextRebuildFlag 在**这一轮真的跑完**之后清位（finishRun，status=completed）。
//
// 刻意不在"消息已受理"时清：受理之后 CLI 仍可能拉不起来（进程崩、配置坏），那时原生会话
// 已经是全新的而历史没喂进去 —— 下一轮重试就会在一段空会话里继续。留在完成时清，
// 失败的那一轮会原样重喂一遍（多喂一次无害，少喂一次就是模型失忆）。
func (s *Server) clearContextRebuildFlag(conversationID string) {
	if _, err := s.db.ExecContext(context.Background(), `update conversations set pending_context_rebuild=0 where id=? and pending_context_rebuild=1`, conversationID); err != nil {
		log.Printf("clear context rebuild flag for conversation %s: %v", conversationID, err)
	}
}

// contextRebuildPreamble 用"节点之前的对话历史"拼出一段前置提示。它在调用方的事务里读库：
// 发送路径只有一条 SQLite 连接。
func (s *Server) contextRebuildPreamble(ctx context.Context, tx *sql.Tx, conversationID string) (string, error) {
	cutoff, rewoundAt, err := checkpointVisibilityFor(ctx, tx, conversationID)
	if err != nil || cutoff == nil {
		return "", err
	}
	query := `select role,content from messages where conversation_id=? and parent_tool_use_id='' and role in ('user','assistant')`
	args := []any{conversationID}
	if clause, clauseArgs := checkpointVisibilityClause("created_at", cutoff, rewoundAt); clause != "" {
		query += " and " + clause
		args = append(args, clauseArgs...)
	}
	query += ` order by created_at asc,id asc`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type turn struct {
		role    string
		content string
	}
	turns := make([]turn, 0)
	for rows.Next() {
		var item turn
		if err := rows.Scan(&item.role, &item.content); err != nil {
			return "", err
		}
		if strings.TrimSpace(item.content) != "" {
			turns = append(turns, item)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(turns) == 0 {
		return "", nil
	}
	// 预算从最新的往回吃：更近的历史更有用，砍掉的是最前面那段。
	used, omitted := 0, 0
	lines := make([]string, 0, len(turns))
	for index := len(turns) - 1; index >= 0; index-- {
		item := turns[index]
		label := "用户"
		if item.role == "assistant" {
			label = "助手"
		}
		line := label + "：" + strings.TrimSpace(item.content)
		if used+len(line) > contextRebuildBudgetChars && len(lines) > 0 {
			omitted = index + 1
			break
		}
		used += len(line)
		lines = append(lines, line)
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	var builder strings.Builder
	builder.WriteString("【会话回退】这条会话已回退到此前某个节点：项目工作区里的代码已恢复到那一刻的状态，\n")
	builder.WriteString("那之后做过的改动全部作废。你是刚启动的新进程，只保留下面这段历史；\n")
	builder.WriteString("不要把回退之后做过的事当成现状，也不要引用它。\n")
	if omitted > 0 {
		fmt.Fprintf(&builder, "（更早的 %d 条消息已省略）\n", omitted)
	}
	builder.WriteString("\n以下是保留的对话历史：\n")
	builder.WriteString(strings.Join(lines, "\n"))
	builder.WriteString("\n\n请把它当作你的工作记忆，然后继续回答用户在下面这条消息里的要求。")
	return builder.String(), nil
}

// ── 遗留拷贝型快照（升级前的机制，只为读兼容保留）────────────────────────────

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

// restoreLegacyCheckpointSnapshot 把遗留拷贝快照逐文件写回工作区。它**不删除**工作区里多出来的
// 文件（旧语义），并且必须用快照文件自己的权限位创建目标文件 —— 早先这里传的是 entry.Type()，
// 普通文件拿到的 perm 是 0，Windows 上等于把每个恢复出来的文件打成只读，之后 agent 再也改不动。
func restoreLegacyCheckpointSnapshot(snapshotPath, projectPath string) error {
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
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(path, dst, info.Mode())
	})
}
