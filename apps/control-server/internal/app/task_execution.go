package app

// 任务执行归属是公共领域数据。旧编排持有长期归属，普通派发只持有运行期归属；
// 托管将在完成目录准入与恢复后使用 supervision 归属，本文件不开放托管启动入口。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type taskExecutionSource string

const (
	taskSourceManual        taskExecutionSource = "manual"
	taskSourceOrchestration taskExecutionSource = "orchestration"
	taskSourceSupervision   taskExecutionSource = "supervision"
	// 不能根据当前任务的旧编排记录推断所有历史 Run 都来自旧编排。
	taskSourceLegacy taskExecutionSource = "legacy"
)

const orchestrationOwnedStatuses = `'queued','preparing','implementing','checking','paused','needs_human','stopped','removing','awaiting_main','integrated_to_dev'`

var errTaskExecutionClaimed = errors.New("task execution is owned by another scheduler")
var errTaskSupervised = errors.New("任务正由托管 Agent 管理，请在任务托管页操作或先停止并接管")

type TaskExecutionClaim struct {
	TaskID    string    `json:"taskId"`
	OwnerKind string    `json:"ownerKind"`
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s *Server) migrateTaskExecution(ctx context.Context) error {
	// task_runs 的旧表重建发生在 migrateTasks 中，补列须在其后。
	for _, column := range []string{"execution_source", "execution_owner_id"} {
		definition := "text not null default ''"
		if column == "execution_source" {
			definition = "text not null default 'legacy'"
		}
		if err := ensureColumn(ctx, s.db, "task_runs", column, definition); err != nil {
			return fmt.Errorf("add task run %s: %w", column, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
create table if not exists task_execution_claims (
	task_id text primary key references tasks(id) on delete cascade,
	owner_kind text not null check(owner_kind in ('manual','orchestration','supervision')),
	owner_id text not null check(length(trim(owner_id))>0),
	created_at datetime not null
);
create index if not exists task_execution_claims_owner on task_execution_claims(owner_kind,owner_id);
create table if not exists task_execution_claim_events (
	id integer primary key autoincrement,
	task_id text not null,
	owner_kind text not null,
	owner_id text not null,
	type text not null check(type in ('claimed','released')),
	created_at datetime not null
);
create index if not exists task_execution_claim_events_task on task_execution_claim_events(task_id,id);
create trigger if not exists task_claim_insert_event after insert on task_execution_claims begin
	insert into task_execution_claim_events(task_id,owner_kind,owner_id,type,created_at)
	values(new.task_id,new.owner_kind,new.owner_id,'claimed',strftime('%Y-%m-%dT%H:%M:%fZ','now'));
end;
create trigger if not exists task_claim_delete_event after delete on task_execution_claims begin
	insert into task_execution_claim_events(task_id,owner_kind,owner_id,type,created_at)
	values(old.task_id,old.owner_kind,old.owner_id,'released',strftime('%Y-%m-%dT%H:%M:%fZ','now'));
end;`)
	if err != nil {
		return fmt.Errorf("migrate task execution claims: %w", err)
	}
	// 先检查冲突再回填，不能用 INSERT OR IGNORE 静默吞掉两个长期所有者。
	var conflict bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from task_orchestration_jobs j
		join task_execution_claims c on c.task_id=j.task_id
		where j.status in (`+orchestrationOwnedStatuses+`) and (c.owner_kind<>'orchestration' or c.owner_id<>j.id))`).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return fmt.Errorf("migrate task ownership: %w", errTaskExecutionClaimed)
	}
	if _, err := tx.ExecContext(ctx, `insert into task_execution_claims(task_id,owner_kind,owner_id,created_at)
		select task_id,'orchestration',id,created_at from task_orchestration_jobs j
		where status in (`+orchestrationOwnedStatuses+`) and not exists(select 1 from task_execution_claims c where c.task_id=j.task_id)`); err != nil {
		return fmt.Errorf("backfill orchestration claims: %w", err)
	}
	// 仅在能从唯一执行意图证明来源时补历史来源；其余历史保持 legacy。
	if _, err := tx.ExecContext(ctx, `update task_runs set execution_source='orchestration',
		execution_owner_id=(select i.job_id from task_execution_intents i where i.run_id=task_runs.run_id limit 1)
		where execution_source='legacy' and run_id<>''
		and (select count(*) from task_execution_intents i where i.run_id=task_runs.run_id)=1
		and exists(select 1 from task_execution_intents i join task_orchestration_jobs j on j.id=i.job_id
			where i.run_id=task_runs.run_id and j.task_id=task_runs.task_id)`); err != nil {
		return err
	}
	// 老版本允许普通执行和编排排队相遇。保留编排长期认领，活动 Run 仍由原恢复链处理；
	// 不在迁移中伪造取消或任务终态。没有长期所有者时回填普通运行期认领。
	if _, err := tx.ExecContext(ctx, `insert into task_execution_claims(task_id,owner_kind,owner_id,created_at)
		select r.task_id,'manual',r.id,r.created_at from task_runs r
		where r.status in ('queued','running')
		and r.id=(select r2.id from task_runs r2 where r2.task_id=r.task_id and r2.status in ('queued','running') order by sequence desc,id desc limit 1)
		and not exists(select 1 from task_execution_claims c where c.task_id=r.task_id)`); err != nil {
		return fmt.Errorf("backfill running task claims: %w", err)
	}
	// 旧编排入队/状态转换/移出都经过数据库约束，同一事务更新认领与审计。
	// 覆盖单项入队、批量入队、恢复和清理，而不依赖某一个 HTTP handler。
	// 触发器只依赖稳定的认领表，避免阻断 task_runs 旧表重建。
	// 活动普通 Run 已在启动事务或上面的迁移中认领，不能被编排覆盖。
	_, err = tx.ExecContext(ctx, `
create trigger if not exists orchestration_task_claim_insert after insert on task_orchestration_jobs
when new.status in (`+orchestrationOwnedStatuses+`) begin
	select case when exists(select 1 from task_execution_claims where task_id=new.task_id and (owner_kind<>'orchestration' or owner_id<>new.id))
		then raise(abort,'task execution is owned by another scheduler') end;
	insert into task_execution_claims(task_id,owner_kind,owner_id,created_at)
		select new.task_id,'orchestration',new.id,new.created_at where not exists(select 1 from task_execution_claims where task_id=new.task_id);
end;
create trigger if not exists orchestration_task_claim_update after update of status on task_orchestration_jobs begin
	select case when new.status in (`+orchestrationOwnedStatuses+`) and exists(select 1 from task_execution_claims where task_id=new.task_id and (owner_kind<>'orchestration' or owner_id<>new.id))
		then raise(abort,'task execution is owned by another scheduler') end;
	insert into task_execution_claims(task_id,owner_kind,owner_id,created_at)
		select new.task_id,'orchestration',new.id,new.created_at where new.status in (`+orchestrationOwnedStatuses+`)
		and not exists(select 1 from task_execution_claims where task_id=new.task_id);
	delete from task_execution_claims where task_id=old.task_id and owner_kind='orchestration' and owner_id=old.id
		and new.status not in (`+orchestrationOwnedStatuses+`);
end;
create trigger if not exists orchestration_task_claim_delete after delete on task_orchestration_jobs begin
	delete from task_execution_claims where task_id=old.task_id and owner_kind='orchestration' and owner_id=old.id;
end;`)
	if err != nil {
		return fmt.Errorf("protect orchestration task ownership: %w", err)
	}
	return tx.Commit()
}

func taskExecutionClaim(ctx context.Context, q sqlQueryer, taskID string) (TaskExecutionClaim, bool, error) {
	var claim TaskExecutionClaim
	err := q.QueryRowContext(ctx, `select task_id,owner_kind,owner_id,created_at from task_execution_claims where task_id=?`, taskID).
		Scan(&claim.TaskID, &claim.OwnerKind, &claim.OwnerID, &claim.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskExecutionClaim{}, false, nil
	}
	return claim, err == nil, err
}

// claimTaskExecutionTx 必须与派发/启动事务共用连接。重复认领同一 owner 幂等；
// 更换 owner 不做 upsert，防止新调用覆盖旧所有者。
func claimTaskExecutionTx(ctx context.Context, tx *sql.Tx, taskID, ownerKind, ownerID string, now time.Time) error {
	if ownerKind != string(taskSourceManual) && ownerKind != string(taskSourceOrchestration) && ownerKind != string(taskSourceSupervision) {
		return errors.New("invalid task execution owner kind")
	}
	if strings.TrimSpace(ownerID) == "" {
		return errors.New("task execution owner ID is required")
	}
	claim, exists, err := taskExecutionClaim(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if exists {
		if claim.OwnerKind == ownerKind && claim.OwnerID == ownerID {
			return nil
		}
		return errTaskExecutionClaimed
	}
	// 旧数据库迁移核对完成前的兜底，也防止绕过长期归属的直接调用。
	var jobID string
	err = tx.QueryRowContext(ctx, `select id from task_orchestration_jobs where task_id=? and status in (`+orchestrationOwnedStatuses+`)`, taskID).Scan(&jobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (ownerKind != string(taskSourceOrchestration) || ownerID != jobID) {
		return errTaskExecutionClaimed
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from task_runs where task_id=? and status in ('queued','running'))`, taskID).Scan(&active); err != nil {
		return err
	}
	if active {
		return errTaskExecutionClaimed
	}
	_, err = tx.ExecContext(ctx, `insert into task_execution_claims(task_id,owner_kind,owner_id,created_at) values (?,?,?,?)`, taskID, ownerKind, ownerID, now)
	return err
}

func releaseTaskExecutionTx(ctx context.Context, tx *sql.Tx, taskID, ownerKind, ownerID string) error {
	_, err := tx.ExecContext(ctx, `delete from task_execution_claims where task_id=? and owner_kind=? and owner_id=?`, taskID, ownerKind, ownerID)
	return err
}

func rejectSupervisedTaskMutation(ctx context.Context, q sqlQueryer, taskID string) error {
	claim, exists, err := taskExecutionClaim(ctx, q, taskID)
	if err != nil {
		return err
	}
	if exists && claim.OwnerKind == string(taskSourceSupervision) {
		return errTaskSupervised
	}
	return nil
}

// 替换/删除依赖必须同时检查现有和新关系的两端，不能从未托管任务绕过保护。
func rejectSupervisedDependencyMutation(ctx context.Context, tx *sql.Tx, taskID string, predecessorIDs ...string) error {
	if err := rejectSupervisedTaskMutation(ctx, tx, taskID); err != nil {
		return err
	}
	var supervised bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from task_dependencies d join task_execution_claims c on c.task_id=d.predecessor_task_id
		where d.task_id=? and c.owner_kind='supervision')`, taskID).Scan(&supervised); err != nil {
		return err
	}
	if supervised {
		return errTaskSupervised
	}
	for _, id := range predecessorIDs {
		if err := rejectSupervisedTaskMutation(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func rejectSupervisedTaskDeletionTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	if err := rejectSupervisedDependencyMutation(ctx, tx, taskID); err != nil {
		return err
	}
	var supervised bool
	err := tx.QueryRowContext(ctx, `select exists(select 1 from task_dependencies d join task_execution_claims c on c.task_id=d.task_id
		where d.predecessor_task_id=? and c.owner_kind='supervision')`, taskID).Scan(&supervised)
	if err != nil {
		return err
	}
	if supervised {
		return errTaskSupervised
	}
	return nil
}

func writeTaskExecutionError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if isTaskExecutionConflict(err) {
		status = http.StatusConflict
	}
	writeError(w, status, err)
}

func (s *Server) hydrateTaskExecutionClaims(ctx context.Context, byID map[string]*Task, ids string, args []any) error {
	rows, err := s.db.QueryContext(ctx, `select task_id,owner_kind,owner_id,created_at from task_execution_claims where task_id in (`+ids+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var claim TaskExecutionClaim
		if err := rows.Scan(&claim.TaskID, &claim.OwnerKind, &claim.OwnerID, &claim.CreatedAt); err != nil {
			return err
		}
		if task := byID[claim.TaskID]; task != nil {
			task.ExecutionClaim = &claim
		}
	}
	return rows.Err()
}

// claimSupervisionTasksTx 是后续托管 start 事务的任务部分；调用方还须在同一事务
// 建立会话与目录 pending 准入。任何一项失败必须回滚整个事务，不能返回部分选择。
func claimSupervisionTasksTx(ctx context.Context, tx *sql.Tx, projectID, sessionID string, taskIDs []string, now time.Time) error {
	if len(taskIDs) == 0 || strings.TrimSpace(sessionID) == "" {
		return errors.New("supervision requires selected tasks and an owner ID")
	}
	seen := make(map[string]bool, len(taskIDs))
	for _, taskID := range taskIDs {
		if taskID == "" || seen[taskID] {
			return errors.New("selected task IDs must be non-empty and unique")
		}
		seen[taskID] = true
		var actualProject, status string
		if err := tx.QueryRowContext(ctx, `select project_id,status from tasks where id=?`, taskID).Scan(&actualProject, &status); err != nil {
			return err
		}
		if actualProject != projectID || (status != taskTodo && status != taskActionRequired) {
			return errors.New("selected tasks must belong to this project and be todo or action_required")
		}
		if err := claimTaskExecutionTx(ctx, tx, taskID, string(taskSourceSupervision), sessionID, now); err != nil {
			return err
		}
	}
	return nil
}

func isTaskExecutionConflict(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errTaskExecutionClaimed) || errors.Is(err, errTaskSupervised) ||
		strings.Contains(err.Error(), errTaskExecutionClaimed.Error()) ||
		strings.Contains(err.Error(), "task already has an active execution")
}

func admitTaskExecutionTx(ctx context.Context, tx *sql.Tx, taskRun *TaskRun, source taskExecutionSource, now time.Time) error {
	if source == "" {
		source = taskSourceManual
	}
	ownerID := taskRun.ID
	switch source {
	case taskSourceManual:
		if taskRun.ExecutionIntentID != "" {
			return errors.New("manual task execution cannot use an orchestration intent")
		}
	case taskSourceOrchestration:
		if err := tx.QueryRowContext(ctx, `select id from task_orchestration_jobs where task_id=? and status in (`+orchestrationOwnedStatuses+`)`, taskRun.TaskID).Scan(&ownerID); err != nil {
			return fmt.Errorf("orchestration task owner is unavailable: %w", err)
		}
		if taskRun.ExecutionIntentID != "" {
			var intentOwner string
			if err := tx.QueryRowContext(ctx, `select job_id from task_execution_intents where id=?`, taskRun.ExecutionIntentID).Scan(&intentOwner); err != nil {
				return err
			}
			if intentOwner != ownerID {
				return errors.New("task execution intent does not belong to this task owner")
			}
		}
	default:
		// 托管的依赖门禁、目录子令牌和结果消费尚未实现，不能先用普通派发代跑。
		return errors.New("task execution source is not available")
	}
	if err := claimTaskExecutionTx(ctx, tx, taskRun.TaskID, string(source), ownerID, now); err != nil {
		return err
	}
	taskRun.ExecutionSource, taskRun.ExecutionOwnerID = source, ownerID
	return nil
}
