package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 直接模式（计划级 executionMode=branch）的回归测试：Agent 在项目工作目录里、用户指定的
// 已有分支上改文件，不建分支、不建工作树、不提交、不合并，收口为 applied_to_branch。
// 这些断言全部建立在真实 git 状态上——直接模式的全部风险都在真实仓库里。

// gitIn 在指定目录跑 git，把输出交给调用方判断。刻意不调用 t.Fatalf：它也会在 runner 桩
// 的 goroutine 里被调用，而 t.Fatalf 只能从测试 goroutine 调用。
func gitIn(dir string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func mustGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitIn(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return output
}

// initDirectModeProject 造一个真实仓库（main 分支 + 一个基线提交）并注册为项目。
func initDirectModeProject(t *testing.T, server *Server, projectID string) string {
	t.Helper()
	repo := t.TempDir()
	mustGitIn(t, repo, "init", "-b", "main")
	mustGitIn(t, repo, "config", "user.email", "test@example.com")
	mustGitIn(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatalf("seed readme: %v", err)
	}
	mustGitIn(t, repo, "add", "README.md")
	mustGitIn(t, repo, "commit", "-m", "baseline")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,1,?)`, projectID, projectID, repo, server.localRunnerID(), "main", now); err != nil {
		t.Fatalf("insert direct mode project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values (?,?,'00000000-0000-4000-8000-000000000000','idle',0,1,?)`, "conversation-"+projectID, projectID, now); err != nil {
		t.Fatalf("insert direct mode conversation: %v", err)
	}
	return repo
}

func createBatchWithBodyForTest(t *testing.T, server *Server, projectID, body string) OrchestrationBatch {
	t.Helper()
	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches", bytes.NewBufferString(body)))
	if create.Code != http.StatusAccepted {
		t.Fatalf("create batch: %d body=%s", create.Code, create.Body.String())
	}
	var batch OrchestrationBatch
	if err := json.Unmarshal(create.Body.Bytes(), &batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	return batch
}

func addTasksToBatchForTest(t *testing.T, server *Server, projectID, batchID string, taskIDs ...string) {
	t.Helper()
	if len(taskIDs) == 0 {
		return
	}
	quoted := make([]string, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		quoted = append(quoted, fmt.Sprintf("%q", taskID))
	}
	add := httptest.NewRecorder()
	server.routes().ServeHTTP(add, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches/"+batchID+"/tasks", bytes.NewBufferString(`{"taskIds":[`+strings.Join(quoted, ",")+`]}`)))
	if add.Code != http.StatusNoContent {
		t.Fatalf("add tasks to batch: %d body=%s", add.Code, add.Body.String())
	}
}

func startBatchForTest(t *testing.T, server *Server, projectID, batchID string) {
	t.Helper()
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches/"+batchID+"/start", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("start batch: %d body=%s", response.Code, response.Body.String())
	}
}

// createDirectBatchForTest 建一个直接模式计划、加入子任务并放行（闸门）。
func createDirectBatchForTest(t *testing.T, server *Server, projectID, name, target string, taskIDs ...string) OrchestrationBatch {
	t.Helper()
	batch := createBatchWithBodyForTest(t, server, projectID, fmt.Sprintf(`{"name":%q,"conversationStrategy":"new","executionMode":"branch","targetBranch":%q}`, name, target))
	addTasksToBatchForTest(t, server, projectID, batch.ID, taskIDs...)
	startBatchForTest(t, server, projectID, batch.ID)
	return batch
}

// waitForOrchestrationStatus 等到作业进入 wants 之一。needs_human / stopped 不在 wants 里时
// 立刻失败并带上 last_error 与 frozen_reason——直接模式出问题时这两条信息就是根因。
func waitForOrchestrationStatus(t *testing.T, server *Server, taskID string, wants ...string) string {
	t.Helper()
	want := make(map[string]bool, len(wants))
	for _, item := range wants {
		want[item] = true
	}
	deadline := time.Now().Add(20 * time.Second)
	var status, lastError string
	for time.Now().Before(deadline) {
		if err := server.db.QueryRow(`select status,last_error from task_orchestration_jobs where task_id=?`, taskID).Scan(&status, &lastError); err != nil {
			t.Fatalf("load orchestration status: %v", err)
		}
		if want[status] {
			return status
		}
		if status == orchestrationNeedsHuman || status == orchestrationStopped {
			var frozen string
			_ = server.db.QueryRow(`select coalesce(config.frozen_reason,'') from task_orchestration_jobs job left join project_orchestration_configs config on config.project_id=job.project_id where job.task_id=?`, taskID).Scan(&frozen)
			t.Fatalf("job reached %q, want %v: last_error=%q frozen_reason=%q", status, wants, lastError, frozen)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job status %q, want %v: last_error=%q", status, wants, lastError)
	return status
}

func TestDirectModeRunsInProjectWorktreeWithoutBranchOrCommit(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-project"
	repo := initDirectModeProject(t, server, projectID)
	before := mustGitIn(t, repo, "rev-parse", "HEAD")

	var mu sync.Mutex
	workspaces := []string{}
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		mu.Lock()
		workspaces = append(workspaces, request.ProjectPath)
		mu.Unlock()
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})

	taskID := createTaskForTest(t, server.routes(), projectID, "Direct mode task")
	createDirectBatchForTest(t, server, projectID, "direct", "main", taskID)
	waitForOrchestrationStatus(t, server, taskID, orchestrationApplied)

	mu.Lock()
	seen := append([]string{}, workspaces...)
	mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("runner was never called")
	}
	for _, workspace := range seen {
		if workspace != repo {
			t.Fatalf("agent workspace = %q, want the project worktree %q", workspace, repo)
		}
	}
	// 不提交：目标分支的 HEAD 不能动，改动必须留在工作区里等用户自己 commit。
	after := mustGitIn(t, repo, "rev-parse", "HEAD")
	if after != before {
		t.Fatalf("direct mode must not create a commit: head %s -> %s", before, after)
	}
	status := mustGitIn(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "implemented.txt") {
		t.Fatalf("expected an uncommitted change in the project worktree, got %q", status)
	}
	// 不建分支、不建工作树。
	if branches := mustGitIn(t, repo, "branch", "--format=%(refname:short)"); strings.TrimSpace(branches) != "main" {
		t.Fatalf("branches = %q, want only main", branches)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(repo), ".auto-worktrees")); !os.IsNotExist(err) {
		t.Fatalf("direct mode must not create an orchestration worktree (stat err=%v)", err)
	}
	// 记录里刻意不写任务分支与工作区：清理路径会对非空值执行 git branch -D 与
	// git worktree remove --force，写进去等于让「清理资源」去删用户的分支或仓库根。
	var mode, branch, worktree, integration string
	if err := server.db.QueryRow(`select execution_mode,coalesce(task_branch,''),coalesce(worktree_path,''),coalesce(integration_sha,'') from git_task_records where job_id=(select id from task_orchestration_jobs where task_id=?)`, taskID).Scan(&mode, &branch, &worktree, &integration); err != nil {
		t.Fatalf("load git task record: %v", err)
	}
	if mode != orchestrationModeBranch {
		t.Fatalf("execution_mode = %q, want %q", mode, orchestrationModeBranch)
	}
	if branch != "" || worktree != "" || integration != "" {
		t.Fatalf("direct mode record must stay empty: branch=%q worktree=%q integration=%q", branch, worktree, integration)
	}
	// 任务停在待验收，等用户确认与提交。
	assertTaskStatus(t, server, taskID, taskAwaitingReview)
	// 提示词是 agent 唯一的软约束（编排会话拿的是 full_control），必须落在快照里可审计。
	var prompt string
	if err := server.db.QueryRow(`select prompt_snapshot from task_runs where task_id=? order by sequence desc limit 1`, taskID).Scan(&prompt); err != nil {
		t.Fatalf("load prompt snapshot: %v", err)
	}
	if !strings.Contains(prompt, "不要创建任何提交") || !strings.Contains(prompt, "目标分支是 main") {
		t.Fatalf("direct mode prompt is missing its constraints: %q", prompt)
	}
}

func TestDirectModeRetryStaysInTheProjectWorktree(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-retry"
	repo := initDirectModeProject(t, server, projectID)

	var mu sync.Mutex
	attempts := 0
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current == 1 {
			return errors.New("first attempt failed")
		}
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})

	taskID := createTaskForTest(t, server.routes(), projectID, "Direct retry task")
	createDirectBatchForTest(t, server, projectID, "direct-retry", "main", taskID)
	waitForOrchestrationStatus(t, server, taskID, orchestrationApplied)

	mu.Lock()
	total := attempts
	mu.Unlock()
	if total < 2 {
		t.Fatalf("expected the second attempt to run, attempts=%d", total)
	}
	// 第二轮派发是从 git_task_records 读回 base/branch/worktree 的，而直接模式那一行的
	// worktree_path 是空的。照抄它会让 os.Stat("") 命中 ErrNotExist，从而走进
	// `git worktree add -b "" "" <base>` —— 每一次重试都会炸在 "create task worktree"。
	var lastError string
	if err := server.db.QueryRow(`select last_error from task_orchestration_jobs where task_id=?`, taskID).Scan(&lastError); err != nil {
		t.Fatalf("load last error: %v", err)
	}
	if strings.Contains(lastError, "create task worktree") {
		t.Fatalf("retry fell back to worktree creation: %q", lastError)
	}
	if _, err := os.Stat(filepath.Join(repo, "implemented.txt")); err != nil {
		t.Fatalf("the retry must have kept working in the project worktree: %v", err)
	}
}

func TestDirectModeRefusesWhenTheWorktreeIsOnAnotherBranch(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-mismatch"
	repo := initDirectModeProject(t, server, projectID)
	mustGitIn(t, repo, "checkout", "-b", "other")

	var mu sync.Mutex
	calls := 0
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})

	taskID := createTaskForTest(t, server.routes(), projectID, "Mismatched branch task")
	createDirectBatchForTest(t, server, projectID, "mismatch", "main", taskID)
	waitForOrchestrationStatus(t, server, taskID, orchestrationNeedsHuman)

	mu.Lock()
	total := calls
	mu.Unlock()
	if total != 0 {
		t.Fatalf("the agent must not run when the worktree is on another branch, calls=%d", total)
	}
	var lastError string
	if err := server.db.QueryRow(`select last_error from task_orchestration_jobs where task_id=?`, taskID).Scan(&lastError); err != nil {
		t.Fatalf("load last error: %v", err)
	}
	// last_error 存的是本地化后的文案：分支名（main）会被 containsUntranslatedEnglish 判成
	// 英文，所以直接模式的错误必须在翻译表之前单独拦住，这条断言同时守住"没有退化成
	// 通用前缀"这件事。
	if !strings.Contains(lastError, "直接模式要求项目工作区正检出计划选定的分支") {
		t.Fatalf("last_error = %q, want the localized branch mismatch reason", lastError)
	}
	// 改动一个都没落下，而且 Milevia 没有替用户切分支。
	if _, err := os.Stat(filepath.Join(repo, "implemented.txt")); !os.IsNotExist(err) {
		t.Fatalf("refused dispatch must not touch the worktree (stat err=%v)", err)
	}
	if current := mustGitIn(t, repo, "branch", "--show-current"); strings.TrimSpace(current) != "other" {
		t.Fatalf("Milevia must never switch branches for the user, now on %q", current)
	}
}

func TestDirectModeTakesTheTargetBranchFromThePlanNotProjectPolicy(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-branch"
	repo := initDirectModeProject(t, server, projectID)
	mustGitIn(t, repo, "checkout", "-b", "release/x")
	mustGitIn(t, repo, "commit", "--allow-empty", "-m", "release baseline")
	releaseSHA := mustGitIn(t, repo, "rev-parse", "HEAD")
	enableOrchestrationForTest(t, server, projectID)

	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})

	taskID := createTaskForTest(t, server.routes(), projectID, "Release branch task")
	createDirectBatchForTest(t, server, projectID, "release", "release/x", taskID)
	waitForOrchestrationStatus(t, server, taskID, orchestrationApplied)

	// 计划选定的分支才是目标；项目策略里的稳定分支必须原样保留（计划级参数不占项目级字段）。
	var mainBranch string
	if err := server.db.QueryRow(`select main_branch from project_orchestration_configs where project_id=?`, projectID).Scan(&mainBranch); err != nil {
		t.Fatalf("load project policy: %v", err)
	}
	if mainBranch != "main" {
		t.Fatalf("project main_branch = %q, want main: a plan level choice must not rewrite the project policy", mainBranch)
	}
	var base, target string
	if err := server.db.QueryRow(`select record.base_dev_sha,job.policy_snapshot from git_task_records record join task_orchestration_jobs job on job.id=record.job_id where job.task_id=?`, taskID).Scan(&base, &target); err != nil {
		t.Fatalf("load base sha: %v", err)
	}
	if base != releaseSHA {
		t.Fatalf("base_dev_sha = %q, want the selected branch tip %q", base, releaseSHA)
	}
	cfg := orchestrationPlanConfig(projectID, target)
	if cfg.ExecutionMode != orchestrationModeBranch || cfg.TargetBranch != "release/x" {
		t.Fatalf("job snapshot = %+v, want branch mode on release/x", cfg)
	}
}

func TestAppliedToBranchJobIsSkippedByTheScheduler(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	finished := createTaskForTest(t, server.routes(), projectID, "Already applied")
	waiting := createTaskForTest(t, server.routes(), projectID, "Still queued")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-applied',?,?,1,?,7,'{}',?,?)`, projectID, finished, orchestrationApplied, now, now); err != nil {
		t.Fatalf("insert applied job: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-queued',?,?,2,?,7,'{}',?,?)`, projectID, waiting, orchestrationQueued, now, now); err != nil {
		t.Fatalf("insert queued job: %v", err)
	}
	job, err := server.nextOrchestrationJob(context.Background(), projectID)
	if err != nil {
		t.Fatalf("next orchestration job: %v", err)
	}
	// 判别性就在这里：少了排除列表项时取到的是队首那条已收口的作业，它既不能被准备
	// （"job is not ready to prepare"）也不会被状态守卫改掉，于是永远占着队首把后面的
	// 作业挡死——不是冻结，但同样让队列停摆。
	if job == nil || job.TaskID != waiting {
		t.Fatalf("scheduler picked %+v, want the queued job behind the applied one", job)
	}
}

// 只挂着已收口作业的项目：调度器必须"取不到活"就直接返回，不能把它当"取到了却没法准备"
// 的失败去处理。注意这里验的是"不冻结"；真正判别性的性质是下面那条"跳过它去取后面的作业"——
// 少了排除列表项时 failOrchestrationJob 的 UPDATE 因状态守卫影响 0 行而早退，队列不会冻结，
// 但已收口的作业会永远占着队首、把后面的作业挡死，那才是这条排除列表要防的故障。
func TestAppliedToBranchJobAloneDoesNotFreezeTheQueue(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskID := createTaskForTest(t, server.routes(), projectID, "Applied only")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-applied',?,?,1,?,7,'{}',?,?)`, projectID, taskID, orchestrationApplied, now, now); err != nil {
		t.Fatalf("insert applied job: %v", err)
	}
	server.processProjectOrchestration(context.Background(), projectID)
	var frozen, status string
	if err := server.db.QueryRow(`select frozen_reason from project_orchestration_configs where project_id=?`, projectID).Scan(&frozen); err != nil {
		t.Fatalf("load frozen reason: %v", err)
	}
	if frozen != "" {
		t.Fatalf("a finished direct mode job must not freeze the queue: %q", frozen)
	}
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where task_id=?`, taskID).Scan(&status); err != nil {
		t.Fatalf("load job status: %v", err)
	}
	if status != orchestrationApplied {
		t.Fatalf("job status = %q, want it untouched", status)
	}
}

// 依赖判定有两份 SQL（orchestrationDependenciesIntegrated 与 validateTaskDispatchTx 的事务内
// 副本）。只改一处的话，编排自己会放行、派发事务却拒绝，报出来的是一句看不出根因的
// "task has unfinished predecessor tasks"。这条用例走真实派发路径，把两处一起钉住。
func TestDirectModeDependencyOnAnAppliedTaskIsSatisfied(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-deps"
	initDirectModeProject(t, server, projectID)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})
	first := createTaskForTest(t, server.routes(), projectID, "First direct task")
	second := createTaskForTest(t, server.routes(), projectID, "Second direct task")
	if _, err := server.db.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, second, first, time.Now().UTC()); err != nil {
		t.Fatalf("insert dependency: %v", err)
	}
	createDirectBatchForTest(t, server, projectID, "deps", "main", first, second)
	waitForOrchestrationStatus(t, server, first, orchestrationApplied)
	waitForOrchestrationStatus(t, server, second, orchestrationApplied)
}

func TestDeleteOrchestrationBatchAllowsAppliedJobs(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "Applied A")
	taskB := createTaskForTest(t, server.routes(), projectID, "Applied B")
	// 计划本身用默认的隔离工作树模式：这条用例验的是删除守卫的**状态**白名单，
	// 与执行方式无关；建直接模式计划会要求项目是个真仓库，白白引入无关依赖。
	batch := createBatchWithBodyForTest(t, server, projectID, `{"name":"applied plan","conversationStrategy":"new"}`)
	addTasksToBatchForTest(t, server, projectID, batch.ID, taskA, taskB)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update task_orchestration_jobs set status=?,updated_at=? where batch_id=?`, orchestrationApplied, now, batch.ID); err != nil {
		t.Fatalf("mark jobs applied: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID+"/orchestration/batches/"+batch.ID, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete plan of applied jobs: %d body=%s", response.Code, response.Body.String())
	}
	// 只摘标签：作业行保留（已编排的任务受审计保留策略保护，job 行不能被顺手删掉）。
	var remaining, grouped int
	if err := server.db.QueryRow(`select count(*) from task_orchestration_jobs where task_id in (?,?)`, taskA, taskB).Scan(&remaining); err != nil {
		t.Fatalf("count remaining jobs: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining jobs = %d, want the rows kept after detaching", remaining)
	}
	if err := server.db.QueryRow(`select count(*) from task_orchestration_jobs where batch_id=?`, batch.ID).Scan(&grouped); err != nil {
		t.Fatalf("count grouped jobs: %v", err)
	}
	if grouped != 0 {
		t.Fatalf("grouped jobs = %d, want them detached", grouped)
	}
}

func TestAppliedToBranchTaskStaysReviewableAndRedispatched(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskID := createTaskForTest(t, server.routes(), projectID, "Applied reviewable task")
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
		t.Fatalf("prepare task for review: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-applied',?,?,1,?,7,'{}',?,?)`, projectID, taskID, orchestrationApplied, now, now); err != nil {
		t.Fatalf("insert applied job: %v", err)
	}
	// 验收：直接模式已收口，作业不该拦住用户确认完成（reviewTask 的"编排中"白名单刻意不含它）。
	review := httptest.NewRecorder()
	server.routes().ServeHTTP(review, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"accept"}`)))
	if review.Code != http.StatusOK {
		t.Fatalf("accept applied task: %d body=%s", review.Code, review.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskDone)

	// 手动重下发：编排已结束，orchestrationOwnsTask 不该再持有这个任务。
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
		t.Fatalf("reopen task: %v", err)
	}
	dispatch := httptest.NewRecorder()
	server.routes().ServeHTTP(dispatch, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dispatch", bytes.NewBufferString(`{}`)))
	if dispatch.Code == http.StatusConflict && strings.Contains(dispatch.Body.String(), "自动编排尚未结束此任务") {
		t.Fatalf("an applied task must not block manual re-dispatch: %d body=%s", dispatch.Code, dispatch.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskRunning)
}

func TestDirectModeReportsAnAgentCommit(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-self-commit"
	repo := initDirectModeProject(t, server, projectID)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		if err := os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600); err != nil {
			return err
		}
		if _, err := gitIn(request.ProjectPath, "add", "-A"); err != nil {
			return err
		}
		_, err := gitIn(request.ProjectPath, "commit", "-m", "agent committed anyway")
		return err
	})
	taskID := createTaskForTest(t, server.routes(), projectID, "Self committing task")
	createDirectBatchForTest(t, server, projectID, "self-commit", "main", taskID)
	// 提交不改变"改动要不要用户自己处理"这件事，所以照常收口；但必须留痕，
	// 否则"不自动提交"就成了一句没人核对的承诺。
	waitForOrchestrationStatus(t, server, taskID, orchestrationApplied)
	var lastError string
	if err := server.db.QueryRow(`select last_error from task_orchestration_jobs where task_id=?`, taskID).Scan(&lastError); err != nil {
		t.Fatalf("load last error: %v", err)
	}
	if !strings.Contains(lastError, "直接模式约定不创建提交") {
		t.Fatalf("last_error = %q, want a note about the agent created commit", lastError)
	}
	if head := mustGitIn(t, repo, "rev-parse", "HEAD"); head == mustGitIn(t, repo, "rev-parse", "main~1") {
		t.Fatalf("expected the agent commit to be the branch tip, head=%s", head)
	}
}

func TestDirectModeFailsWhenTheAgentSwitchesBranch(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-agent-branch"
	initDirectModeProject(t, server, projectID)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		if err := os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600); err != nil {
			return err
		}
		_, err := gitIn(request.ProjectPath, "checkout", "-b", "agent-branch")
		return err
	})
	taskID := createTaskForTest(t, server.routes(), projectID, "Branch switching task")
	createDirectBatchForTest(t, server, projectID, "agent-branch", "main", taskID)
	// 改动落在了别的分支上：必须让人看见，而不是乐观地记成「已写入 main」。
	waitForOrchestrationStatus(t, server, taskID, orchestrationNeedsHuman)
	var lastError string
	if err := server.db.QueryRow(`select last_error from task_orchestration_jobs where task_id=?`, taskID).Scan(&lastError); err != nil {
		t.Fatalf("load last error: %v", err)
	}
	if !strings.Contains(lastError, "直接模式的任务结束时工作区不在计划选定的分支上") {
		t.Fatalf("last_error = %q, want the localized branch switch reason", lastError)
	}
}

func TestCreateOrchestrationBatchValidatesDirectMode(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-validate"
	initDirectModeProject(t, server, projectID)

	missing := httptest.NewRecorder()
	server.routes().ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches", bytes.NewBufferString(`{"name":"missing branch","executionMode":"branch","targetBranch":"nope"}`)))
	if missing.Code != http.StatusConflict {
		t.Fatalf("create with a missing branch: %d body=%s", missing.Code, missing.Body.String())
	}
	// 分支名会被 containsUntranslatedEnglish 判成英文，所以本地化必须在翻译表之前单独拦截。
	if !strings.Contains(missing.Body.String(), "直接模式要求目标分支已存在") {
		t.Fatalf("expected the localized direct mode reason, body=%s", missing.Body.String())
	}

	invalid := httptest.NewRecorder()
	server.routes().ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches", bytes.NewBufferString(`{"name":"bad mode","executionMode":"sideways"}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("create with an invalid mode: %d body=%s", invalid.Code, invalid.Body.String())
	}
	if !strings.Contains(invalid.Body.String(), "执行方式只能是") {
		t.Fatalf("expected the localized execution mode reason, body=%s", invalid.Body.String())
	}
}

// nonLocalRunnerIDForTest 返回一个**必然不是**服务端本机 runner 的 id：Windows 服务端上
// wsl-local 是跨端（文件走 UNC、AI 经 wsl.exe），其余平台上跨端的才是 windows-local。
func nonLocalRunnerIDForTest() string {
	if runtime.GOOS == "windows" {
		return "wsl-local"
	}
	return "windows-local"
}

// 非本机 runner 的项目：直接模式必须在**建计划**时就拒掉。留到派发是 needs_human 加整个
// 项目队列冻结（prepareAndDispatchOrchestrationJob 的前置检查），而那时用户只看到一句
// 指向不明的失败——他并不知道是自己选错了项目。前端那道闸门只能拦界面上的操作：API 直连
// 或任何不走这个弹窗的客户端都绕得过去，所以这道校验必须落在服务端。
func TestCreateOrchestrationBatchRejectsDirectModeOffTheServerRunner(t *testing.T) {
	server := newTestServer(t)
	projectID := "direct-off-runner"
	initDirectModeProject(t, server, projectID)
	if _, err := server.db.Exec(`update projects set runner=? where id=?`, nonLocalRunnerIDForTest(), projectID); err != nil {
		t.Fatalf("switch project runner: %v", err)
	}
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches", bytes.NewBufferString(`{"name":"off runner","executionMode":"branch","targetBranch":"main"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("create direct batch off the server runner: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "只支持运行在服务端本机运行器上的项目") {
		t.Fatalf("expected the localized runner reason, body=%s", recorder.Body.String())
	}
	// 拒掉就要真的什么都没建：留下一条计划行等于队列里躺着一个必然失败的作业。
	var batches int
	if err := server.db.QueryRow(`select count(*) from orchestration_batches where project_id=?`, projectID).Scan(&batches); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	if batches != 0 {
		t.Fatalf("rejected direct batch left %d rows behind", batches)
	}
	// 顺序也是判据的一部分：本机 runner 检查必须排在分支存在性检查**之前**。否则非本机项目
	// 会先被拿去在本机跑一次 git show-ref（远端路径在本机根本不存在），用户拿到一句指错
	// 方向的"目标分支不存在"409，而真正的原因（项目不在本机 runner 上）一个字都没提。
	masked := httptest.NewRecorder()
	server.routes().ServeHTTP(masked, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/batches", bytes.NewBufferString(`{"name":"off runner 2","executionMode":"branch","targetBranch":"no-such-branch"}`)))
	if masked.Code != http.StatusBadRequest || !strings.Contains(masked.Body.String(), "只支持运行在服务端本机运行器上的项目") {
		t.Fatalf("a missing branch masked the runner check: %d body=%s", masked.Code, masked.Body.String())
	}
}

// 前端那道闸门读的是项目载荷里的 localRunner，而它由 decorateProjectPresentation 用
// isLocalRunnerID 填。这里把**线上字段名**也钉住：json tag 写错时前端的判据会静默变成
// undefined（=不是本机），直接模式对所有项目都不再出现，而没有任何测试会红。
func TestProjectPayloadCarriesLocalRunner(t *testing.T) {
	server := newTestServer(t)
	local := Project{Runner: server.localRunnerID(), Path: t.TempDir()}
	server.decorateProjectPresentation(&local)
	other := Project{Runner: nonLocalRunnerIDForTest(), Path: t.TempDir()}
	server.decorateProjectPresentation(&other)
	payload, err := json.Marshal([]Project{local, other})
	if err != nil {
		t.Fatalf("marshal projects: %v", err)
	}
	if !strings.Contains(string(payload), `"localRunner":true`) || !strings.Contains(string(payload), `"localRunner":false`) {
		t.Fatalf("project payload is missing the localRunner flag the orchestration page reads: %s", payload)
	}
}

// 建计划时拦不住的那两条路（隔离工作树模式、建完计划再改项目 runner）仍会在派发时撞上
// 同一个前置检查，并被 failOrchestrationJob 记成 needs_human 加整个项目队列冻结。用户
// 只能从 last_error 看原因，所以那句原文必须有中文映射，且要指明是"服务端本机运行器"——
// 落到通用兜底（"请查看任务日志后重试"）时用户没有任何日志可看。
func TestOrchestrationOffRunnerFailureIsLocalized(t *testing.T) {
	text := errorText(errors.New("automatic orchestration currently requires a local runner"))
	if !strings.Contains(text, "服务端本机运行器") || strings.Contains(text, taskFailureFallback) {
		t.Fatalf("off-runner dispatch failure text = %q", text)
	}
}

func TestOrchestrationExecutionModeMigrationDefaultsToWorktree(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	// 模拟升级前的库：把新列去掉，再插入一条"老"计划行。
	for _, statement := range []string{`alter table orchestration_batches drop column execution_mode`, `alter table orchestration_batches drop column target_branch`} {
		if _, err := server.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into orchestration_batches (id,project_id,name,conversation_strategy,started_at,created_at,updated_at) values ('legacy-batch',?,'legacy','new',?,?,?)`, projectID, now, now, now); err != nil {
		t.Fatalf("insert legacy batch: %v", err)
	}
	if err := server.migrateOrchestration(context.Background()); err != nil {
		t.Fatalf("migrate orchestration: %v", err)
	}
	assertLegacyBatchMode := func() {
		t.Helper()
		var mode, target string
		if err := server.db.QueryRow(`select execution_mode,coalesce(target_branch,'') from orchestration_batches where id='legacy-batch'`).Scan(&mode, &target); err != nil {
			t.Fatalf("load legacy batch: %v", err)
		}
		if mode != orchestrationModeWorktree || target != "" {
			t.Fatalf("legacy batch = (%q,%q), want the worktree default", mode, target)
		}
	}
	assertLegacyBatchMode()
	// 幂等：再跑一次不会改动既有数据。
	if err := server.migrateOrchestration(context.Background()); err != nil {
		t.Fatalf("migrate orchestration again: %v", err)
	}
	assertLegacyBatchMode()
}

// 第二道锁：清理路径拿到的工作区等于仓库根时直接拒绝。第一道锁是直接模式刻意不往记录里
// 写这两个值——但一道锁不够，git worktree remove --force <仓库根> 是不可逆的。
// 断言必须钉在"拒绝的是我们自己的守卫"上：只断言"报错了"会被 git 自己的报错蒙混过去
// （删主工作树本来就会被 git 拒绝），那样这条测试就失去了判别力。
func TestRemoveOrchestrationGitResourcesRefusesTheProjectRoot(t *testing.T) {
	server := newTestServer(t)
	repo := t.TempDir()
	mustGitIn(t, repo, "init", "-b", "main")
	err := server.removeOrchestrationGitResources(context.Background(), repo, "project", "job", repo, "")
	if err == nil {
		t.Fatal("removing the project worktree as a task worktree must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to remove the project worktree") {
		t.Fatalf("err = %v, want our own refusal before any git command runs", err)
	}
	if _, statErr := os.Stat(repo); statErr != nil {
		t.Fatalf("the project directory must survive the refusal: %v", statErr)
	}
}
