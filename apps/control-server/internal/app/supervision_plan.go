package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

var errSupervisionPlanInvalid = errors.New("托管计划领域校验失败")

func invalidSupervisionPlan(message string) error {
	return fmt.Errorf("%w: %s", errSupervisionPlanInvalid, message)
}

type supervisionAcceptancePoint struct {
	ID           string `json:"id"`
	Requirement  string `json:"requirement"`
	Kind         string `json:"kind"`
	Verification string `json:"verification"`
	Critical     bool   `json:"critical"`
}

type supervisionTaskPlan struct {
	RequiresBaseline bool                         `json:"requiresBaseline,omitempty"`
	Goal             string                       `json:"goal"`
	Scope            []string                     `json:"scope"`
	DependsOn        []string                     `json:"dependsOn"`
	Acceptance       []supervisionAcceptancePoint `json:"acceptance"`
	Environment      []string                     `json:"environment"`
	Review           []string                     `json:"review"`
	Limitations      []string                     `json:"limitations"`
}

type supervisionRecordPlanAction struct {
	Type                 string              `json:"type"`
	SessionID            string              `json:"sessionId"`
	TaskID               string              `json:"taskId"`
	RequirementsRevision int64               `json:"requirementsRevision"`
	ExpectedPlanRevision int64               `json:"expectedPlanRevision"`
	ContextID            string              `json:"contextId"`
	RevisionID           string              `json:"revisionId"`
	RulesHash            string              `json:"rulesHash"`
	Plan                 supervisionTaskPlan `json:"plan"`
}

type supervisionPlanVersion struct {
	TaskID               string              `json:"taskId"`
	Revision             int64               `json:"revision"`
	RequirementsRevision int64               `json:"requirementsRevision"`
	ContextID            string              `json:"contextId"`
	Status               string              `json:"status"`
	Plan                 supervisionTaskPlan `json:"plan"`
}

func (s *Server) migrateSupervisionPlans(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_plan_versions (
		session_id text not null, task_id text not null, revision integer not null,
		requirements_revision integer not null, context_id text not null,
		decision_id text not null references supervision_decisions(id) on delete cascade,
		status text not null check(status in ('ready','review_required','blocked')), plan_json text not null, semantic_hash text not null,
		reason text not null, created_at datetime not null,
		primary key(session_id,task_id,revision),
		foreign key(session_id,task_id) references supervision_tasks(session_id,task_id) on delete cascade,
		foreign key(session_id,context_id) references supervision_project_contexts(session_id,id)
	);
	create table if not exists supervision_action_results (
		decision_id text not null references supervision_decisions(id) on delete cascade, attempt integer not null,
		state text not null check(state in ('applied','rejected')), error text not null, created_at datetime not null,
		primary key(decision_id,attempt)
	);
	create table if not exists supervision_plan_progress (
		session_id text primary key references supervision_sessions(id) on delete cascade,
		revisions_without_progress integer not null default 0
	);`)
	return err
}

// 拒绝未知字段和大小写别名；重复键和深度已由外层提案解析器检查。
func decodeSupervisionAction(raw json.RawMessage, target any) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var check func(any, reflect.Type) error
	check = func(value any, typ reflect.Type) error {
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() == reflect.Struct {
			object, ok := value.(map[string]any)
			if !ok {
				return errors.New("调度动作对象不能为空")
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
			}
			for key, child := range object {
				field, ok := fields[key]
				if !ok {
					return errors.New("调度动作包含未知或非规范字段")
				}
				if err := check(child, field); err != nil {
					return err
				}
			}
		} else if typ.Kind() == reflect.Slice && value != nil {
			array, ok := value.([]any)
			if !ok {
				return errors.New("调度动作列表类型无效")
			}
			for _, child := range array {
				if err := check(child, typ.Elem()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := check(value, reflect.TypeOf(target)); err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func validateSupervisionPlan(plan *supervisionTaskPlan, manifest supervisionWorkspaceManifest) error {
	if manifest.TargetOS != "" && manifest.TargetOS != "windows" && manifest.TargetOS != "linux" && manifest.TargetOS != "darwin" {
		return errors.New("项目快照的目标路径语义未知")
	}
	if strings.TrimSpace(plan.Goal) == "" || len(plan.Goal) > 8000 || len(plan.Scope) < 1 || len(plan.Scope) > 100 || len(plan.Acceptance) < 1 || len(plan.Acceptance) > 100 || len(plan.Review) == 0 || len(plan.Environment) == 0 {
		return errors.New("计划需要目标、具体修改范围、验收点、环境及独立审查内容")
	}
	seen := map[string]bool{}
	for _, name := range plan.Scope {
		key := name
		if supervisionManifestWindows(manifest) {
			key = strings.ToLower(key)
		}
		if !supervisionManifestPathLocal(manifest, name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:*?[]\x00") || seen[key] {
			return errors.New("计划范围必须是项目内不重复的明确相对路径")
		}
		seen[key] = true
		for _, segment := range strings.Split(name, "/") {
			if supervisionSnapshotExclusion(segment, true) != "" || supervisionGeneratedPath(segment, true) || strings.TrimRight(segment, ". ") != segment {
				return errors.New("计划范围包含排除或敏感路径")
			}
		}
		// 新文件可以不存在，但所有已有祖先及范围内输入必须可覆盖。
		for _, entry := range manifest.Entries {
			entryPath := entry.Path
			if supervisionManifestWindows(manifest) {
				entryPath = strings.ToLower(entryPath)
			}
			ancestor := key == entryPath || strings.HasPrefix(key, entryPath+"/")
			descendant := strings.HasPrefix(entryPath, key+"/")
			if key != entryPath && ancestor && entry.Kind != "directory" && entry.Gap == "" {
				return errors.New("计划范围的祖先必须是目录")
			}
			if (ancestor || descendant) && entry.Gap != "" {
				return errors.New("计划范围包含未覆盖、排除或链接输入")
			}
		}
	}
	critical := false
	seen = map[string]bool{}
	for _, point := range plan.Acceptance {
		if strings.TrimSpace(point.ID) == "" || len(point.ID) > 100 || seen[point.ID] || strings.TrimSpace(point.Requirement) == "" || strings.TrimSpace(point.Verification) == "" || len(point.Requirement) > 8000 || len(point.Verification) > 16000 {
			return errors.New("验收点需要唯一 ID、预期行为和具体验证方式")
		}
		seen[point.ID], critical = true, critical || point.Critical
		switch point.Kind {
		case "command", "browser", "review", "manual":
		default:
			return errors.New("验收点验证类型无效")
		}
	}
	if !critical {
		return errors.New("计划至少需要一个关键验收点")
	}
	for _, list := range [][]string{plan.Environment, plan.Review, plan.Limitations} {
		if len(list) > 100 {
			return errors.New("计划条目数量超限")
		}
		for _, item := range list {
			if strings.TrimSpace(item) == "" || len(item) > 8000 {
				return errors.New("计划条目为空或超限")
			}
		}
	}
	if plan.DependsOn == nil {
		plan.DependsOn = []string{}
	}
	if plan.Limitations == nil {
		plan.Limitations = []string{}
	}
	sort.Strings(plan.Scope)
	sort.Strings(plan.DependsOn)
	sort.Slice(plan.Acceptance, func(i, j int) bool { return plan.Acceptance[i].ID < plan.Acceptance[j].ID })
	return nil
}

// Paths in snapshots are slash-separated target-relative names. The controller
// may run on another OS; filepath.IsLocal and runtime.GOOS cannot interpret a
// remote manifest. Legacy manifests were local-only and retain their semantics.
func supervisionManifestWindows(manifest supervisionWorkspaceManifest) bool {
	return manifest.TargetOS == "windows" || manifest.TargetOS == "" && runtime.GOOS == "windows"
}

func supervisionManifestPathLocal(manifest supervisionWorkspaceManifest, name string) bool {
	if manifest.TargetOS != "" && manifest.TargetOS != "windows" && manifest.TargetOS != "linux" && manifest.TargetOS != "darwin" {
		return false
	}
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	if !supervisionManifestWindows(manifest) {
		return true
	}
	for _, segment := range strings.Split(name, "/") {
		if strings.TrimRight(segment, ". ") != segment || strings.ContainsAny(segment, `<>"|`) {
			return false
		}
		for _, char := range segment {
			if char < 32 {
				return false
			}
		}
		base, _, _ := strings.Cut(strings.ToUpper(segment), ".")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return false
		}
		if (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && len([]rune(base)) == 4 {
			runes := []rune(base)
			if strings.ContainsRune("123456789¹²³", runes[3]) {
				return false
			}
		}
	}
	return true
}

func loadSupervisionPlansTx(ctx context.Context, q *sql.Tx, id string) ([]supervisionPlanVersion, error) {
	rows, err := q.QueryContext(ctx, `select p.task_id,p.revision,p.requirements_revision,p.context_id,p.status,p.plan_json,p.semantic_hash from supervision_plan_versions p join supervision_tasks t on t.session_id=p.session_id and t.task_id=p.task_id and t.plan_revision=p.revision where p.session_id=? order by t.position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []supervisionPlanVersion{}
	for rows.Next() {
		var p supervisionPlanVersion
		var raw, hash string
		if err := rows.Scan(&p.TaskID, &p.Revision, &p.RequirementsRevision, &p.ContextID, &p.Status, &raw, &hash); err != nil {
			return nil, err
		}
		if supervisionSHA256([]byte(raw)) != hash {
			return nil, errors.New("托管计划内容校验失败")
		}
		if err := json.Unmarshal([]byte(raw), &p.Plan); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// 目前只应用 record_plan。其他动作保留 proposed，不能用成功占位替代实际执行。
func (s *Server) applySupervisionPlanDecision(ctx context.Context, decisionID string, lease supervisionLease) (bool, error) {
	d, err := loadSupervisionDecision(ctx, s.db, decisionID)
	if err != nil {
		return false, err
	}
	var output string
	if err := s.db.QueryRowContext(ctx, `select output from supervision_decisions where id=?`, d.ID).Scan(&output); err != nil {
		return false, err
	}
	proposal, err := parseSupervisionProposal(output, d)
	if err != nil {
		return false, err
	}
	var typ struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(proposal.Action, &typ); err != nil {
		return false, err
	}
	if typ.Type != "record_plan" {
		return false, nil
	}
	var applied bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from supervision_action_results where decision_id=? and attempt=?)`, d.ID, d.Attempt).Scan(&applied); err != nil {
		return true, err
	}
	if applied {
		return true, nil
	}
	var action supervisionRecordPlanAction
	validationErr := decodeSupervisionAction(proposal.Action, &action)
	if validationErr == nil {
		// 即使没有已登记的执行器，外部编辑器也可能改动；应用前重新观察。
		_, err = s.captureSupervisionProjectContext(ctx, d.SessionID, d.ExpectedStateVersion)
		if err != nil {
			return true, err
		}
	}
	err = s.withSupervisionTx(ctx, d.SessionID, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionLease(ss, lease, time.Now().UTC()); err != nil {
			return err
		}
		if ss.Control != "" || ss.RecoveryState != "ready" || ss.StateVersion != d.ExpectedStateVersion || ss.LeaseEpoch != d.LeaseEpoch || (ss.State != "running" && ss.State != "finalizing") {
			return errSupervisionState
		}
		latest, err := loadSupervisionDecision(ctx, tx, d.ID)
		if err != nil {
			return err
		}
		if latest.State != "proposed" || latest.ExecutionID != d.ExecutionID {
			return errSupervisionState
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		if err := requireSupervisionRoleObservationsTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		a, ok := next[ss.AdmissionID]
		if !ok || a.State != "owned" || a.Epoch != ss.AdmissionEpoch || a.RecoveryState != "ready" || s.admissionHasOperationsLocked(a) {
			return errWorkspaceNotQuiescent
		}
		if _, err := s.supervisionWorkspaceSourceTx(ctx, tx, ss, a); err != nil {
			return err
		}
		projectContext, err := loadSupervisionProjectContext(ctx, tx, ss.ID, "current")
		if err != nil {
			return err
		}
		if validationErr == nil && (action.Type != "record_plan" || action.SessionID != ss.ID || action.ContextID != projectContext.ID || action.RevisionID != projectContext.RevisionID || action.RulesHash != projectContext.RulesHash) {
			validationErr = errors.New("计划动作的会话、代码或规则上下文不匹配")
		}
		if validationErr == nil {
			validationErr = validateSupervisionPlan(&action.Plan, projectContext.Manifest)
		}
		if validationErr == nil {
			recordErr := s.recordSupervisionPlanTx(ctx, tx, ss, d, proposal.Reason, action)
			if recordErr != nil && !errors.Is(recordErr, errSupervisionPlanInvalid) && !errors.Is(recordErr, errTaskExecutionClaimed) {
				return recordErr // 存储故障回滚整个动作，不能提交半份计划或当成模型错误。
			}
			validationErr = recordErr
		}
		state, message := "applied", ""
		if validationErr != nil {
			state, message = "rejected", validationErr.Error()
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='invalid',validation_error=? where id=?`, message, d.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_work_items set result_state='invalid',result_error=? where execution_id=?`, message, d.ExecutionID); err != nil {
				return err
			}
			if d.Attempt >= 3 {
				ss.State, ss.Reason, ss.ClockAt = "needs_input", "invalid_scheduler_output", nil
				if errors.Is(validationErr, errSupervisionNoProgress) {
					ss.Reason = "no_progress"
				}
				ss.StateVersion++
			}
		} else {
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='applied' where id=?`, d.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_action_results(decision_id,attempt,state,error,created_at) values (?,?,?,?,?)`, d.ID, d.Attempt, state, message, time.Now().UTC()); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, ss, "decision.action_"+state, map[string]any{"decisionId": d.ID, "attempt": d.Attempt, "error": message}, time.Now().UTC())
	})
	return true, err
}

func (s *Server) recordSupervisionPlanTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, d supervisionDecision, reason string, action supervisionRecordPlanAction) error {
	var requirements, currentRevision int64
	if err := tx.QueryRowContext(ctx, `select requirements_revision,plan_revision from supervision_tasks where session_id=? and task_id=?`, ss.ID, action.TaskID).Scan(&requirements, &currentRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return invalidSupervisionPlan("计划任务不在本次选中范围")
		}
		return err
	}
	if action.RequirementsRevision != requirements || action.ExpectedPlanRevision != currentRevision {
		return invalidSupervisionPlan("计划要求版本或前一计划版本不匹配")
	}
	claim, exists, err := taskExecutionClaim(ctx, tx, action.TaskID)
	if err != nil {
		return err
	}
	if !exists || claim.OwnerKind != "supervision" || claim.OwnerID != ss.ID {
		return errTaskExecutionClaimed
	}
	if err := validateSupervisionPlanDependenciesTx(ctx, tx, ss.ID, action.TaskID, action.Plan.DependsOn); err != nil {
		return err
	}
	encoded, _ := json.Marshal(action.Plan)
	hash, status := supervisionSHA256(encoded), "ready"
	if currentRevision > 0 {
		var previous, previousHash, previousStatus string
		var previousRequirements int64
		if err := tx.QueryRowContext(ctx, `select plan_json,semantic_hash,status,requirements_revision from supervision_plan_versions where session_id=? and task_id=? and revision=?`, ss.ID, action.TaskID, currentRevision).Scan(&previous, &previousHash, &previousStatus, &previousRequirements); err != nil {
			return err
		}
		if supervisionSHA256([]byte(previous)) != previousHash {
			return errors.New("托管计划内容校验失败")
		}
		if hash == previousHash && previousRequirements == requirements {
			return supervisionNoProgress("当前计划已保存，请按现有计划实施、验证或处理具体阻塞，不要重复记录同一计划")
		} // 不增加计划/业务版本；作为领域纠正，防止会话静默停在running。
		if previousStatus == "review_required" {
			status = "review_required" // 普通修订不能解除尚未完成的独立审查。
		}
		var old supervisionTaskPlan
		if err := json.Unmarshal([]byte(previous), &old); err != nil {
			return err
		}
		points := map[string]supervisionAcceptancePoint{}
		for _, point := range action.Plan.Acceptance {
			points[point.ID] = point
		}
		for _, point := range old.Acceptance {
			if replacement, exists := points[point.ID]; !exists || replacement != point {
				status = "review_required"
			}
		}
	}
	for _, dependency := range action.Plan.DependsOn {
		var state string
		if err := tx.QueryRowContext(ctx, `select status from tasks where id=?`, dependency).Scan(&state); err != nil {
			return err
		}
		if state != taskDone && status == "ready" {
			status = "blocked"
		}
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_plan_versions(session_id,task_id,revision,requirements_revision,context_id,decision_id,status,plan_json,semantic_hash,reason,created_at) values (?,?,?,?,?,?,?,?,?,?,?)`, ss.ID, action.TaskID, currentRevision+1, requirements, action.ContextID, d.ID, status, string(encoded), hash, reason, time.Now().UTC()); err != nil {
		return err
	}
	phase := "ready"
	if status == "review_required" {
		phase = "planning"
	} else if status == "blocked" {
		phase = "blocked"
	}
	if _, err := tx.ExecContext(ctx, `update supervision_tasks set plan_revision=?,phase=?,evidence_freshness='missing' where session_id=? and task_id=?`, currentRevision+1, phase, ss.ID, action.TaskID); err != nil {
		return err
	}
	noProgress := 0
	if currentRevision > 0 {
		if err := tx.QueryRowContext(ctx, `select coalesce((select revisions_without_progress from supervision_plan_progress where session_id=?),0)+1`, ss.ID).Scan(&noProgress); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_plan_progress(session_id,revisions_without_progress) values (?,?) on conflict(session_id) do update set revisions_without_progress=excluded.revisions_without_progress`, ss.ID, noProgress); err != nil {
		return err
	}
	ss.StateVersion++
	if noProgress >= 3 {
		ss.State, ss.Reason, ss.ClockAt = "needs_input", "no_progress", nil
	}
	return appendSupervisionEventTx(ctx, tx, ss, "plan.recorded", map[string]any{"taskId": action.TaskID, "revision": currentRevision + 1, "status": status, "noProgress": noProgress}, time.Now().UTC())
}

func validateSupervisionPlanDependenciesTx(ctx context.Context, tx *sql.Tx, sessionID, taskID string, dependencies []string) error {
	rows, err := tx.QueryContext(ctx, `select task_id,dependency_snapshot from supervision_tasks where session_id=?`, sessionID)
	if err != nil {
		return err
	}
	graph, selected := map[string][]string{}, map[string]bool{}
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var original []TaskDependency
		if err = json.Unmarshal([]byte(raw), &original); err != nil {
			break
		}
		selected[id] = true
		for _, dependency := range original {
			graph[id] = append(graph[id], dependency.TaskID)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	original := map[string]bool{}
	for _, id := range graph[taskID] {
		original[id] = true
	}
	seen := map[string]bool{}
	for _, id := range dependencies {
		if id == taskID || id == "" || seen[id] || (!selected[id] && !original[id]) {
			return invalidSupervisionPlan("计划依赖重复、自引用或超出选中范围")
		}
		seen[id] = true
	}
	for id := range original {
		if !seen[id] {
			return invalidSupervisionPlan("计划不能移除任务已有的显式依赖")
		}
	}
	plans, err := loadSupervisionPlansTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		graph[plan.TaskID] = plan.Plan.DependsOn
	}
	graph[taskID] = dependencies
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return false
		}
		if visited[id] || !selected[id] {
			return true
		}
		visiting[id] = true
		for _, dependency := range graph[id] {
			if !visit(dependency) {
				return false
			}
		}
		visiting[id], visited[id] = false, true
		return true
	}
	for id := range selected {
		if !visit(id) {
			return invalidSupervisionPlan("计划依赖形成环")
		}
	}
	return nil
}
