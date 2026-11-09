package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const supervisionImplementationPrompt = `你是任务托管的实施 Agent，只实施本次下发的选中任务。你没有工具，不运行命令，不访问网络。根据输入的任务、计划、源码和补充要求生成具体文件变更。不要提交、推送、部署、回滚或扩大任务范围。
输出且只输出 JSON：{"summary":"实际实现说明","edits":[{"path":"项目内相对文件路径","baseSha256":"输入源码中的原始哈希，新文件为空串","content":"变更后的完整文件内容","delete":false}]}。
每个文件最多出现一次。只修改计划 scope 覆盖的文件；不要修改 AGENTS.md、CLAUDE.md、敏感文件、生成物、依赖目录。保留用户已有改动，禁止猜测未提供文件的原始内容与哈希。需要更多源码时返回空 edits 并在 summary 说明缺口，由调度重新读取和派发。可以新增必要测试文件，但不能把自测或自己的判断写成验收通过。规格不足或已有实现满足要求时如实说明；不得伪造命令结果。代码必须完整，不使用占位符、伪实现或省略片段。文件修改由服务端核对版本及范围后应用。`

const supervisionReviewPrompt = `你是独立审查 Agent。你与实施会话独立，没有工具，不修改文件或运行命令。只审查本次任务，核对任务原文、补充要求、计划、当前源码、启动基线和真实验证证据。证据或文件缺失要明确失败或说明缺口；不能把 Agent 自述、命令退出0或构建通过视为所有需求已验证。
sources 与 baselineSources 是实际提供的正文。manifestTotal 给出项目清单总数；manifestTruncated 仅表示超过清单数量上限，workspaceChanges 是当前批次相对启动基线的文件变化。任务阶段与 evidenceFreshness 是流程状态，不能单独替代对同版本实际证据的判断；历史审查失败需对照本轮输入重新核对。
implementationChanges 是服务端按实际实施工作项输入/输出快照计算的变更归属，包含 taskId、executionId、当时修改范围与逐文件变化。只把当前 taskId 的实施增量用于判断本任务是否越界；其他选中任务的合法变更不是本任务越界，不得要求回退它们。workspaceChanges 仍用于检查整批改动对当前任务的回归和残留影响，不能仅因文件属于其他任务就忽略实际缺陷。归属资料截断或有未核验的实施结果时明确指出缺口，不能只凭调度 instruction 的自述认定变更来源。
输出且只输出 JSON：{"verdict":"pass或fail","summary":"审查结论与依据","points":[{"id":"计划验收点ID","passed":true,"reason":"与源码和实际证据的对应关系"}],"blockers":["阻塞问题"],"acceptanceChangeApproved":false}。
points 必须逐项覆盖计划全部验收点。关键条件缺少实际验证、行为错误、范围越界、任务要求未实现必须 fail。若本次计划为 review_required，只有对照提供的历史计划确认改变没有消除用户要求时才能 acceptanceChangeApproved=true；用户明确要求不能被你豁免。不得把自己的审查结论当成测试执行记录。`

type supervisionEdit struct {
	Path       string `json:"path"`
	BaseSHA256 string `json:"baseSha256"`
	Content    string `json:"content"`
	Delete     bool   `json:"delete"`
}
type supervisionImplementationResult struct {
	Summary string            `json:"summary"`
	Edits   []supervisionEdit `json:"edits"`
}
type supervisionReviewPoint struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason"`
}
type supervisionReviewResult struct {
	Verdict                  string                   `json:"verdict"`
	Summary                  string                   `json:"summary"`
	Points                   []supervisionReviewPoint `json:"points"`
	Blockers                 []string                 `json:"blockers"`
	AcceptanceChangeApproved bool                     `json:"acceptanceChangeApproved"`
}

type supervisionRoleInput struct {
	SchemaVersion                   int                               `json:"schemaVersion"`
	Decision                        supervisionDecision               `json:"decision"`
	Job                             supervisionRoleJob                `json:"job"`
	Task                            supervisionTask                   `json:"task"`
	Plan                            supervisionPlanVersion            `json:"plan"`
	PreviousPlans                   []supervisionTaskPlan             `json:"previousPlans"`
	ProjectContext                  supervisionProjectContext         `json:"projectContext"`
	Sources                         []supervisionSource               `json:"sources"`
	BaselineSources                 []supervisionSource               `json:"baselineSources"`
	Evidence                        []supervisionEvidence             `json:"evidence"`
	Amendments                      []supervisionAmendment            `json:"amendments"`
	ManifestTotal                   int                               `json:"manifestTotal"`
	WorkspaceChanges                []supervisionRoleChange           `json:"workspaceChanges"`
	ChangesTruncated                bool                              `json:"changesTruncated"`
	ManifestTruncated               bool                              `json:"manifestTruncated"`
	ImplementationChanges           []supervisionImplementationChange `json:"implementationChanges"`
	ImplementationChangesIncomplete bool                              `json:"implementationChangesIncomplete"`
}

type supervisionImplementationChange struct {
	ExecutionID    string                  `json:"executionId"`
	TaskID         string                  `json:"taskId"`
	State          string                  `json:"state"`
	InputRevision  string                  `json:"inputRevision"`
	OutputRevision string                  `json:"outputRevision"`
	Scope          []string                `json:"scope"`
	Changes        []supervisionRoleChange `json:"changes"`
	Incomplete     bool                    `json:"incomplete"`
}

// Attribute actual revision deltas, never the model's claimed edit list. Keep
// unverified attempts visible and bound the input independently of batch size.
func supervisionImplementationChangesTx(ctx context.Context, tx *sql.Tx, sessionID string) ([]supervisionImplementationChange, bool, error) {
	rows, err := tx.QueryContext(ctx, `select j.execution_id,j.task_id,j.state,j.input_revision,j.output_revision,p.plan_json from supervision_role_jobs j join supervision_plan_versions p on p.session_id=j.session_id and p.task_id=j.task_id and p.revision=j.plan_revision where j.session_id=? and j.kind='implementation' and j.state<>'queued' order by j.created_at desc,j.execution_id desc limit 51`, sessionID)
	if err != nil {
		return nil, false, err
	}
	items := []supervisionImplementationChange{}
	for rows.Next() {
		var item supervisionImplementationChange
		var raw string
		if err := rows.Scan(&item.ExecutionID, &item.TaskID, &item.State, &item.InputRevision, &item.OutputRevision, &raw); err != nil {
			rows.Close()
			return nil, false, err
		}
		var plan supervisionTaskPlan
		if err := json.Unmarshal([]byte(raw), &plan); err != nil {
			rows.Close()
			return nil, false, err
		}
		item.Scope = plan.Scope
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	incomplete := len(items) > 50
	if incomplete {
		items = items[:50]
	}
	remaining := 2000
	for i := range items {
		item := &items[i]
		item.Changes = []supervisionRoleChange{}
		if item.State != "completed" || item.OutputRevision == "" {
			item.Incomplete = true
			incomplete = true
			continue
		}
		manifests := make([]supervisionWorkspaceManifest, 2)
		for k, revision := range []string{item.InputRevision, item.OutputRevision} {
			var raw string
			if err := tx.QueryRowContext(ctx, `select r.manifest from supervision_revisions r where r.id=? and exists(select 1 from supervision_project_contexts c where c.session_id=? and c.revision_id=r.id)`, revision, sessionID).Scan(&raw); err != nil {
				return nil, false, err
			}
			if supervisionSHA256([]byte(raw)) != revision {
				return nil, false, errors.New("实施变更归属快照校验失败")
			}
			if err := json.Unmarshal([]byte(raw), &manifests[k]); err != nil {
				return nil, false, err
			}
		}
		changes, truncated := supervisionRoleChanges(manifests[0].Entries, manifests[1].Entries)
		if len(changes) > remaining {
			changes = changes[:remaining]
			truncated = true
		}
		remaining -= len(changes)
		item.Changes, item.Incomplete = changes, truncated
		incomplete = incomplete || truncated
	}
	return items, incomplete, nil
}

type supervisionRoleChange struct {
	Path   string                    `json:"path"`
	Before *supervisionSnapshotEntry `json:"before,omitempty"`
	After  *supervisionSnapshotEntry `json:"after,omitempty"`
}

// Readable context is wider than the write scope. Include known files mentioned
// by this task/plan and the real verification command, without granting edits.
func supervisionRoleContextPaths(entries []supervisionSnapshotEntry, initial []string, text string, evidence []supervisionEvidence) []string {
	paths := map[string]bool{}
	for _, name := range initial {
		paths[name] = true
	}
	for _, e := range evidence {
		if e.Kind != "command" && e.Kind != "browser" {
			continue
		}
		var result supervisionCheckResult
		if json.Unmarshal(e.Details, &result) != nil {
			continue
		}
		args := result.Check.Args
		if result.Check.Browser != nil {
			args = append(append([]string{}, args...), result.Check.Browser.ServerArgs...)
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			name := path.Clean(path.Join(result.Check.Directory, strings.ReplaceAll(arg, `\`, "/")))
			if name != ".." && !strings.HasPrefix(name, "../") && !strings.HasPrefix(name, "/") {
				paths[name] = true
			}
		}
	}
	for _, entry := range entries {
		if entry.Kind == "file" && strings.Contains(text, entry.Path) {
			paths[entry.Path] = true
		}
	}
	out := []string{}
	for name := range paths {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func supervisionRoleChanges(before, after []supervisionSnapshotEntry) ([]supervisionRoleChange, bool) {
	old := map[string]supervisionSnapshotEntry{}
	current := map[string]supervisionSnapshotEntry{}
	names := map[string]bool{}
	for _, entry := range before {
		if entry.Kind == "file" {
			old[entry.Path] = entry
			names[entry.Path] = true
		}
	}
	for _, entry := range after {
		if entry.Kind == "file" {
			current[entry.Path] = entry
			names[entry.Path] = true
		}
	}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	changes := []supervisionRoleChange{}
	for _, name := range ordered {
		a, aExists := old[name]
		b, bExists := current[name]
		if aExists && bExists && a == b {
			continue
		}
		if len(changes) >= 2000 {
			return changes, true
		}
		change := supervisionRoleChange{Path: name}
		if aExists {
			change.Before = &a
		}
		if bExists {
			change.After = &b
		}
		changes = append(changes, change)
	}
	return changes, false
}

func buildSupervisionRoleInputTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, d supervisionDecision, pc supervisionProjectContext, j supervisionRoleJob) (supervisionRoleInput, error) {
	var err error
	in := supervisionRoleInput{SchemaVersion: 1, Decision: d, Job: j, ProjectContext: pc, PreviousPlans: []supervisionTaskPlan{}, Amendments: []supervisionAmendment{}}
	var taskRaw, deps string
	if err := tx.QueryRowContext(ctx, `select task_id,position,phase,requirements_revision,task_snapshot,dependency_snapshot,plan_revision,evidence_freshness,implementation_started,fix_rounds from supervision_tasks where session_id=? and task_id=?`, ss.ID, j.TaskID).Scan(&in.Task.TaskID, &in.Task.Position, &in.Task.Phase, &in.Task.RequirementsRevision, &taskRaw, &deps, &in.Task.PlanRevision, &in.Task.EvidenceFreshness, &in.Task.ImplementationStarted, &in.Task.FixRounds); err != nil {
		return in, err
	}
	in.Task.TaskSnapshot = json.RawMessage(taskRaw)
	in.Task.DependencySnapshot = json.RawMessage(deps)
	in.Plan, err = supervisionTaskPlanTx(ctx, tx, ss.ID, j.TaskID)
	if err != nil {
		return in, err
	}
	paths := []string{}
	for _, e := range pc.Manifest.Entries {
		for _, scope := range in.Plan.Plan.Scope {
			if e.Path == scope || strings.HasPrefix(e.Path, scope+"/") {
				paths = append(paths, e.Path)
				break
			}
		}
	}
	var requested string
	if err := tx.QueryRowContext(ctx, `select paths from supervision_source_requests where session_id=?`, ss.ID).Scan(&requested); err == nil {
		var extra []string
		if err := json.Unmarshal([]byte(requested), &extra); err != nil {
			return in, err
		}
		paths = append(paths, extra...)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return in, err
	}
	allEvidence, err := supervisionEvidenceTx(ctx, tx, ss.ID, j.TaskID)
	if err != nil {
		return in, err
	}
	paths = supervisionRoleContextPaths(pc.Manifest.Entries, paths, taskRaw+string(mustJSON(in.Plan.Plan))+string(j.Spec), allEvidence)
	// 空 scope 文件集不意味着读取全项目。
	if len(paths) == 0 {
		paths = []string{"__no_existing_input__"}
	}
	in.Sources, err = supervisionSourcesTx(ctx, tx, pc, paths, supervisionSourceBudget)
	if err != nil {
		return in, err
	}
	baseline, err := loadSupervisionProjectContext(ctx, tx, ss.ID, "baseline")
	if err != nil {
		return in, err
	}
	in.BaselineSources, err = supervisionSourcesTx(ctx, tx, baseline, paths, 128<<10)
	if err != nil {
		return in, err
	}
	in.Evidence = []supervisionEvidence{}
	for _, e := range allEvidence {
		if e.TaskID == j.TaskID {
			e.Details = supervisionModelJSON(e.Details)
			in.Evidence = append(in.Evidence, e)
		}
	}
	rows, err := tx.QueryContext(ctx, `select plan_json from supervision_plan_versions where session_id=? and task_id=? and revision<? order by revision desc limit 4`, ss.ID, j.TaskID, j.PlanRevision)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var raw string
		var p supervisionTaskPlan
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return in, err
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			rows.Close()
			return in, err
		}
		in.PreviousPlans = append(in.PreviousPlans, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return in, err
	}
	rows, err = tx.QueryContext(ctx, `select id,task_id,content,state,created_at from supervision_amendments where session_id=? and task_id in ('',?) order by created_at,id`, ss.ID, j.TaskID)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var a supervisionAmendment
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Content, &a.State, &a.CreatedAt); err != nil {
			rows.Close()
			return in, err
		}
		in.Amendments = append(in.Amendments, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return in, err
	}
	// Preserve the complete small-project manifest, including files outside
	// the write scope. For large projects prioritize actual role inputs.
	in.ManifestTotal = len(pc.Manifest.Entries)
	in.WorkspaceChanges, in.ChangesTruncated = supervisionRoleChanges(baseline.Manifest.Entries, pc.Manifest.Entries)
	in.ImplementationChanges, in.ImplementationChangesIncomplete, err = supervisionImplementationChangesTx(ctx, tx, ss.ID)
	if err != nil {
		return in, err
	}
	filtered := []supervisionSnapshotEntry{}
	included := map[string]bool{}
	for _, entry := range pc.Manifest.Entries {
		relevant := supervisionScopeContains(in.Plan.Plan.Scope, entry.Path) || supervisionIsRule(entry.Path)
		for _, name := range paths {
			relevant = relevant || entry.Path == name
		}
		if relevant && len(filtered) < 2000 {
			filtered = append(filtered, entry)
			included[entry.Path] = true
		}
	}
	for _, entry := range pc.Manifest.Entries {
		if !included[entry.Path] && len(filtered) < 2000 {
			filtered = append(filtered, entry)
			included[entry.Path] = true
		}
	}
	in.ManifestTruncated = len(filtered) < len(pc.Manifest.Entries)
	in.ProjectContext.Manifest.Entries = filtered
	return in, nil
}

func saveSupervisionRoleInputTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, d supervisionDecision, pc supervisionProjectContext) error {
	j, err := loadSupervisionRoleJob(ctx, tx, d.ExecutionID)
	if err != nil {
		return err
	}
	in, err := buildSupervisionRoleInputTx(ctx, tx, ss, d, pc, j)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if len(raw) > maxSupervisionSchedulerInputBytes {
		return errors.New("角色上下文超过 1 MiB，请缩小任务范围")
	}
	if j.Kind == "review" {
		if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set review_fingerprint=? where execution_id=?`, supervisionReviewFingerprint(ss, in), d.ExecutionID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `insert into supervision_scheduler_inputs(execution_id,input_json,input_sha256,created_at) values (?,?,?,?)`, d.ExecutionID, string(raw), supervisionSHA256(raw), time.Now().UTC())
	return err
}

func supervisionReviewFingerprint(ss *supervisionSession, in supervisionRoleInput) string {
	facts := []any{in.Task.TaskID, in.Task.TaskSnapshot, in.Task.RequirementsRevision, in.Plan.Plan, in.Plan.Status, in.ProjectContext.RevisionID, in.ProjectContext.RulesHash, in.Sources, in.BaselineSources, in.WorkspaceChanges, in.ImplementationChanges, in.ImplementationChangesIncomplete, in.Amendments, ss.roleConfig("review"), ss.RouteSnapshot}
	for _, evidence := range in.Evidence {
		if evidence.Kind != "review" && evidence.Kind != "acceptance" {
			facts = append(facts, evidence)
		}
	}
	return supervisionSHA256(mustJSON(facts))
}

func supervisionDuplicateReviewTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, a supervisionWorkflowAction, pc supervisionProjectContext) (bool, error) {
	j := supervisionRoleJob{TaskID: a.TaskID, PlanRevision: a.PlanRevision, Spec: mustJSON(a)}
	in, err := buildSupervisionRoleInputTx(ctx, tx, ss, supervisionDecision{}, pc, j)
	if err != nil {
		return false, err
	}
	fingerprint := supervisionReviewFingerprint(ss, in)
	// Only actual launches count. Cancelled reservations do not consume an attempt.
	var attempts int
	err = tx.QueryRowContext(ctx, `select count(*) from supervision_role_jobs j join supervision_work_items w on w.execution_id=j.execution_id where j.session_id=? and j.task_id=? and j.kind='review' and j.review_fingerprint=? and w.turn_state='consumed'`, ss.ID, a.TaskID, fingerprint).Scan(&attempts)
	return attempts >= 3, err
}

func decodeSupervisionRoleResult(raw string, target any) error {
	if len(raw) > 256<<10 {
		return errors.New("角色输出超限")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := rejectDuplicateSupervisionJSONKeys(decoder, 1); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("角色输出只能有一个 JSON 对象")
	}
	return decodeSupervisionAction(json.RawMessage(raw), target)
}

func (s *Server) completeSupervisionRole(ctx context.Context, permit supervisionExecutionPermit, raw string, runErr error) error {
	release, err := s.acquireSupervisionRoleCompletion(ctx, permit.Decision.ExecutionID)
	if err != nil {
		return err
	}
	defer release()
	j, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil {
		return err
	}
	if (j.Kind != "implementation" && j.Kind != "review") || permit.Role != j.Kind || permit.TaskID != j.TaskID {
		return errSupervisionState
	}
	inputHash := supervisionSHA256(mustJSON([]any{raw, redactAgentText(errorString(runErr)), errors.Is(runErr, context.Canceled)}))
	if j.State == "completed" || j.State == "failed" || j.State == "interrupted" {
		err := s.withSupervisionTx(ctx, jSession(permit), time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
			if _, err := checkSupervisionExecutionIdentityTx(ctx, tx, ss.ID, permit); err != nil {
				return err
			}
			var previous string
			if err := tx.QueryRowContext(ctx, `select completion_input_sha256 from supervision_role_jobs where execution_id=?`, j.ExecutionID).Scan(&previous); err != nil {
				return err
			}
			if previous != "" && previous != inputHash {
				return errSupervisionState
			}
			return nil
		})
		if err != nil {
			return err
		}
		return s.finishSupervisionRoleObservation(ctx, jSession(permit), j.ExecutionID)
	}
	var result json.RawMessage = json.RawMessage(`{}`)
	expectedRevision := ""
	var impl supervisionImplementationResult
	var review supervisionReviewResult
	if runErr == nil {
		if j.Kind == "implementation" {
			runErr = decodeSupervisionRoleResult(raw, &impl)
			if runErr == nil && !supervisionTextValid(impl.Summary, 16000) {
				runErr = errors.New("实施结果缺少说明")
			}
			if runErr == nil {
				result = mustJSON(impl)
				expectedRevision, runErr = s.applySupervisionEdits(ctx, permit, j, impl.Edits)
			}
		} else if j.Kind == "review" {
			runErr = decodeSupervisionRoleResult(raw, &review)
			if runErr == nil {
				result = mustJSON(review)
			}
		}
	}
	err = s.withSupervisionTx(ctx, jSession(permit), time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if _, err := checkSupervisionExecutionTx(ctx, tx, ss, permit); err != nil {
			return err
		}
		var executionState string
		if err := tx.QueryRowContext(ctx, `select state from supervision_executions where execution_id=?`, j.ExecutionID).Scan(&executionState); err != nil {
			return err
		}
		if executionState != "quiescent" {
			return errWorkspaceNotQuiescent
		}
		state, message := "completed", ""
		if runErr != nil {
			state, message = "failed", redactAgentText(runErr.Error())
		}
		if ss.Control != "" || ss.StateVersion != permit.Decision.ExpectedStateVersion {
			state = "interrupted"
			if message == "" {
				message = "控制或要求版本已变化，结果只作为历史保存"
			}
		}
		if j.Kind == "review" && runErr == nil {
			p, err := supervisionTaskPlanTx(ctx, tx, ss.ID, j.TaskID)
			if err != nil {
				return err
			}
			if err := validateSupervisionReview(review, p.Plan); err != nil {
				state, message = "failed", err.Error()
			}
			if review.Verdict != "pass" {
				state = "failed"
			}
			if state == "completed" && p.Status == "review_required" && review.AcceptanceChangeApproved {
				if _, err := tx.ExecContext(ctx, `update supervision_plan_versions set status='ready' where session_id=? and task_id=? and revision=?`, ss.ID, j.TaskID, j.PlanRevision); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `insert into supervision_evidence(id,session_id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at) values (?,?,?,?,'review',?,?,?,?,?,?)`, uuid.NewString(), ss.ID, j.TaskID, j.ExecutionID, j.InputRevision, j.RequirementsRevision, j.PlanRevision, state == "completed", string(result), time.Now().UTC()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set state=?,result=?,error=? where execution_id=?;update supervision_work_items set result_state=?,result_text=?,result_error=?,result_received_at=? where execution_id=?;update supervision_decisions set state='applied',completed_at=? where id=?`, state, string(result), message, j.ExecutionID, state, string(result), message, time.Now().UTC(), j.ExecutionID, time.Now().UTC(), permit.Decision.ID); err != nil {
			return err
		}
		phase := "ready"
		if j.Kind == "implementation" {
			phase = "implemented"
		}
		if j.Kind == "verification" {
			phase = "verified"
		}
		if j.Kind == "review" {
			phase = "reviewed"
		}
		if state != "completed" {
			phase = "needs_repair"
		}
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase=? where session_id=? and task_id=?`, phase, ss.ID, j.TaskID); err != nil {
			return err
		}
		if j.Kind == "implementation" {
			status := "succeeded"
			if state != "completed" {
				status = "failed"
			}
			if _, err := tx.ExecContext(ctx, `update task_runs set status=?,failure_reason=?,finished_at=? where run_id=?;update tasks set status='awaiting_review',updated_at=? where id=?`, status, message, time.Now().UTC(), permit.RunID, time.Now().UTC(), j.TaskID); err != nil {
				return err
			}
		}
		if j.Kind == "review" && state == "failed" {
			if err := reopenSupervisionFinalRepairTx(ctx, tx, ss, j, time.Now().UTC()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set completion_input_sha256=? where execution_id=?`, inputHash, j.ExecutionID); err != nil {
			return err
		}
		if state != "interrupted" {
			if err := queueSupervisionRoleObservationTx(ctx, tx, ss.ID, j, expectedRevision); err != nil {
				return err
			}
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "work_item."+state, map[string]any{"executionId": j.ExecutionID, "taskId": j.TaskID, "role": permit.Role, "error": message, "result": result}, time.Now().UTC())
	})
	if err != nil {
		return err
	}
	return s.finishSupervisionRoleObservation(ctx, jSession(permit), j.ExecutionID)
}

func jSession(p supervisionExecutionPermit) string { return p.Decision.SessionID }

func validateSupervisionReview(r supervisionReviewResult, plan supervisionTaskPlan) error {
	if (r.Verdict != "pass" && r.Verdict != "fail") || !supervisionTextValid(r.Summary, 16000) || len(r.Points) != len(plan.Acceptance) {
		return errors.New("审查格式无效或未覆盖全部验收点")
	}
	seen := map[string]bool{}
	for _, p := range r.Points {
		if seen[p.ID] || !supervisionTextValid(p.Reason, 8000) {
			return errors.New("审查验收点重复或缺少依据")
		}
		seen[p.ID] = true
		if r.Verdict == "pass" && !p.Passed {
			return errors.New("审查仍有未通过验收点")
		}
	}
	for _, p := range plan.Acceptance {
		if !seen[p.ID] {
			return errors.New("审查缺少验收点")
		}
	}
	if r.Verdict == "pass" && len(r.Blockers) > 0 {
		return errors.New("审查存在阻塞问题")
	}
	return nil
}

func supervisionAcceptanceReadyTx(ctx context.Context, tx *sql.Tx, sessionID, taskID, revision string, req, planRevision int64, plan supervisionTaskPlan) error {
	var started bool
	if err := tx.QueryRowContext(ctx, `select implementation_started from supervision_tasks where session_id=? and task_id=?`, sessionID, taskID).Scan(&started); err != nil {
		return err
	}
	if !started {
		return invalidSupervisionWorkflow("任务尚未完成实施或实现核查")
	}
	rows, err := tx.QueryContext(ctx, `select kind,passed,details,rowid from supervision_evidence where session_id=? and task_id=? and revision_id=? and requirements_revision=? and plan_revision=? and rowid>coalesce((select evidence_rowid from supervision_dependency_cutoff where session_id=?),0) order by rowid desc`, sessionID, taskID, revision, req, planRevision, sessionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	reviewOK := false
	reviewSeen := false
	coverage := map[string]bool{}
	latestPoint := map[string]bool{}
	latestSpec := map[string]bool{}
	failedPoints := map[string]bool{}
	var reviewRow, lastValidation int64
	for rows.Next() {
		var kind, details string
		var passed bool
		var evidenceRow int64
		if err := rows.Scan(&kind, &passed, &details, &evidenceRow); err != nil {
			return err
		}
		if kind == "review" && !reviewSeen {
			reviewSeen = true
			reviewOK = passed
			reviewRow = evidenceRow
		}
		if kind == "manual" {
			lastValidation = max(lastValidation, evidenceRow)
			var record struct {
				PointID string `json:"pointId"`
			}
			if err := json.Unmarshal([]byte(details), &record); err != nil {
				return err
			}
			key := "manual:" + record.PointID
			if !latestPoint[key] {
				latestPoint[key] = true
				coverage[key] = passed
			}
		}
		if kind == "command" || kind == "browser" {
			var result supervisionCheckResult
			if err := json.Unmarshal([]byte(details), &result); err != nil {
				return err
			}
			if result.Check.Baseline || result.Interrupted || result.NotExecuted {
				continue
			}
			lastValidation = max(lastValidation, evidenceRow)
			spec := supervisionCheckSemanticHash(result.Check)
			if latestSpec[spec] {
				continue
			}
			latestSpec[spec] = true
			for _, id := range result.Check.AcceptanceIDs {
				key := kind + ":" + id
				ok := passed && result.ExitCode == 0 && !result.Truncated
				coverage[key] = coverage[key] || ok
				failedPoints[key] = failedPoints[key] || !ok
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !reviewOK || reviewRow < lastValidation {
		return invalidSupervisionWorkflow("缺少当前快照和计划版本的独立审查通过证据")
	}
	for _, point := range plan.Acceptance {
		if !point.Critical {
			continue
		}
		switch point.Kind {
		case "command", "browser":
			if !coverage[point.Kind+":"+point.ID] || failedPoints[point.Kind+":"+point.ID] {
				return invalidSupervisionWorkflow("关键验收点 %s 缺少真实验证通过证据", point.ID)
			}
		case "manual":
			if !coverage["manual:"+point.ID] {
				return invalidSupervisionWorkflow("关键人工验收点 %s 尚无人工作证", point.ID)
			}
		}
	}
	return nil
}
