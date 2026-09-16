package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedReleaseTestProject 建一个带 main/dev 的仓库并把项目编排策略指到这两个分支。
// 发布快照只从 dev 分支取 SHA，缺了这条分支整段能力都无法触发。
func seedReleaseTestProject(t *testing.T, server *Server, projectID string) (repo, initialSHA, devSHA string) {
	t.Helper()
	repo = newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	initialSHA = mustGitHead(t, repo)
	runGitForTest(t, repo, "checkout", "-b", "dev")
	writeGitTestFile(t, repo, "dev.txt", "dev\n")
	runGitForTest(t, repo, "add", "dev.txt")
	runGitForTest(t, repo, "commit", "-m", "dev work")
	devSHA = mustGitHead(t, repo)
	runGitForTest(t, repo, "checkout", "main")
	seedGitProjectForTest(t, server, projectID, repo)
	if _, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,agent_id,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,1,'main','dev','claude-code','[]',3,'',?)`, projectID, time.Now().UTC()); err != nil {
		t.Fatalf("insert orchestration config: %v", err)
	}
	return repo, initialSHA, devSHA
}

func releaseSnapshotRequest(t *testing.T, server *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

// seedReleaseTestJob 造一条编排记录，integration_sha 指向任务被集成时落下的提交。
func seedReleaseTestJob(t *testing.T, server *Server, projectID, jobID, taskID, status, integrationSHA string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,1,?,'{}',?,?)`, jobID, projectID, taskID, status, now, now); err != nil {
		t.Fatalf("insert orchestration job %s: %v", jobID, err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,integration_sha,created_at,updated_at) values (?,?,?,?,?,?,?)`, jobID, integrationSHA, "task/"+jobID, t.TempDir(), integrationSHA, now, now); err != nil {
		t.Fatalf("insert git task record %s: %v", jobID, err)
	}
}

func decodeReleaseSnapshot(t *testing.T, response *httptest.ResponseRecorder) ReleaseSnapshot {
	t.Helper()
	var item ReleaseSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode release snapshot: %v body=%s", err, response.Body.String())
	}
	return item
}

func orchestrationJobStatusForTest(t *testing.T, server *Server, jobID string) string {
	t.Helper()
	var status string
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where id=?`, jobID).Scan(&status); err != nil {
		t.Fatalf("read job status %s: %v", jobID, err)
	}
	return status
}

// TestReleaseSnapshotLifecycleDrivesPublishedTasks 覆盖发布快照的完整链路：
// 创建固定快照 → main 尚未包含时拒绝确认 → 合并快照后确认 → 快照内任务标记已发布。
func TestReleaseSnapshotLifecycleDrivesPublishedTasks(t *testing.T) {
	server := newTestServer(t)
	repo, initialSHA, devSHA := seedReleaseTestProject(t, server, "release-project")

	// 新项目没有任何快照时列表返回空数组，而不是 null。
	emptyList := releaseSnapshotRequest(t, server, http.MethodGet, "/api/projects/release-project/orchestration/releases")
	if emptyList.Code != http.StatusOK || strings.TrimSpace(emptyList.Body.String()) != "[]" {
		t.Fatalf("initial release list status=%d body=%s", emptyList.Code, emptyList.Body.String())
	}

	create := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/release-project/orchestration/releases")
	if create.Code != http.StatusCreated {
		t.Fatalf("create release status=%d body=%s", create.Code, create.Body.String())
	}
	snapshot := decodeReleaseSnapshot(t, create)
	wantBranch := "release/" + devSHA[:12]
	if snapshot.Branch != wantBranch || snapshot.DevSHA != devSHA || snapshot.Status != "awaiting_main" {
		t.Fatalf("unexpected release snapshot: %#v", snapshot)
	}
	if head := strings.TrimSpace(gitOutputForTest(t, repo, "rev-parse", wantBranch)); head != devSHA {
		t.Fatalf("release branch %s points at %s want %s", wantBranch, head, devSHA)
	}

	// 同一个 dev SHA 重复发起验收返回既有快照，不产生第二条记录。
	repeat := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/release-project/orchestration/releases")
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat create status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	if existing := decodeReleaseSnapshot(t, repeat); existing.ID != snapshot.ID {
		t.Fatalf("repeat create returned %s want %s", existing.ID, snapshot.ID)
	}

	// 两条待合并记录：一条的集成提交在快照内，一条在旁支上。
	containedTask := createTaskForTest(t, server.routes(), "release-project", "contained task")
	outsideTask := createTaskForTest(t, server.routes(), "release-project", "outside task")
	writeGitTestFile(t, repo, "side.txt", "side\n")
	runGitForTest(t, repo, "checkout", "-b", "side", initialSHA)
	runGitForTest(t, repo, "add", "side.txt")
	runGitForTest(t, repo, "commit", "-m", "side work")
	outsideSHA := mustGitHead(t, repo)
	runGitForTest(t, repo, "checkout", "main")
	if _, err := server.db.Exec(`update tasks set status=? where id in (?,?)`, taskAwaitingReview, containedTask, outsideTask); err != nil {
		t.Fatalf("mark tasks awaiting review: %v", err)
	}
	seedReleaseTestJob(t, server, "release-project", "job-contained", containedTask, "awaiting_main", initialSHA)
	seedReleaseTestJob(t, server, "release-project", "job-outside", outsideTask, "awaiting_main", outsideSHA)

	// main 还没有快照内容时不得确认。
	unmerged := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/release-project/orchestration/releases/"+snapshot.ID+"/confirm")
	if unmerged.Code != http.StatusConflict {
		t.Fatalf("confirm before merge status=%d body=%s", unmerged.Code, unmerged.Body.String())
	}
	// 报错要指名校验过的分支：稳定分支可以在界面上改，写死 main 会让人找错分支。
	if body := unmerged.Body.String(); !strings.Contains(body, "main does not contain the fixed release snapshot") || !strings.Contains(body, devSHA) {
		t.Fatalf("confirm before merge body=%s", body)
	}
	if status := orchestrationJobStatusForTest(t, server, "job-contained"); status != "awaiting_main" {
		t.Fatalf("job status changed before confirmation: %s", status)
	}

	// 用户手动把固定快照合并进 main。
	runGitForTest(t, repo, "merge", "--ff-only", snapshot.Branch)

	confirm := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/release-project/orchestration/releases/"+snapshot.ID+"/confirm")
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirm.Code, confirm.Body.String())
	}
	confirmed := decodeReleaseSnapshot(t, confirm)
	if confirmed.Status != "released_to_main" || confirmed.ConfirmedAt == nil {
		t.Fatalf("unexpected confirmed snapshot: %#v", confirmed)
	}
	if status := orchestrationJobStatusForTest(t, server, "job-contained"); status != "released_to_main" {
		t.Fatalf("contained job status=%s want released_to_main", status)
	}
	if status := orchestrationJobStatusForTest(t, server, "job-outside"); status != "awaiting_main" {
		t.Fatalf("job outside the snapshot was released: %s", status)
	}
	var containedStatus, outsideStatus string
	if err := server.db.QueryRow(`select status from tasks where id=?`, containedTask).Scan(&containedStatus); err != nil {
		t.Fatalf("read contained task: %v", err)
	}
	if err := server.db.QueryRow(`select status from tasks where id=?`, outsideTask).Scan(&outsideStatus); err != nil {
		t.Fatalf("read outside task: %v", err)
	}
	if containedStatus != taskDone || outsideStatus != taskAwaitingReview {
		t.Fatalf("task statuses: contained=%s outside=%s", containedStatus, outsideStatus)
	}

	// 已确认的快照不可重复确认，列表里保留发布记录。
	again := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/release-project/orchestration/releases/"+snapshot.ID+"/confirm")
	if again.Code != http.StatusConflict {
		t.Fatalf("repeat confirm status=%d body=%s", again.Code, again.Body.String())
	}
	list := releaseSnapshotRequest(t, server, http.MethodGet, "/api/projects/release-project/orchestration/releases")
	var snapshots []ReleaseSnapshot
	if err := json.Unmarshal(list.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decode release list: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != snapshot.ID || snapshots[0].Status != "released_to_main" {
		t.Fatalf("unexpected release list: %#v", snapshots)
	}
}

// TestReleaseSnapshotPublishesJobsAdvancedByWorker 不塞种子状态，而是让编排 worker 真实
// 地把任务推进到待合并态，再用发布快照确认发布。
//
// 这条链路正是原缺陷的形态：确认逻辑只认 integrated_to_dev，而 worker 实际写的是
// awaiting_main，两边一旦对不上，"确认已合入"就会静默什么都不做。这里把生产者
// （advanceOrchestrationJob/completeOrchestrationTaskBranch）和消费者（确认接口）
// 绑在同一个用例里，任一侧改了状态名都会失败。
func TestReleaseSnapshotPublishesJobsAdvancedByWorker(t *testing.T) {
	server := newTestServer(t)
	ctx := context.Background()
	projectID := "worker-release"
	repo, _, _ := seedReleaseTestProject(t, server, projectID)

	// 真实 worker 的前半段：任务分支从 dev 切出，在独立 worktree 里提交。
	worktree := filepath.Join(t.TempDir(), "task-worktree")
	runGitForTest(t, repo, "worktree", "add", "-b", "task/worker", worktree, "dev")
	writeGitTestFile(t, worktree, "task.txt", "task work\n")
	runGitForTest(t, worktree, "add", "task.txt")
	runGitForTest(t, worktree, "commit", "-m", "task work")
	commit := mustGitHead(t, worktree)
	// 用户把这条任务并进 dev——发布快照从 dev 取源，快照必须包含任务提交。
	runGitForTest(t, repo, "checkout", "dev")
	runGitForTest(t, repo, "merge", "--ff-only", "task/worker")
	runGitForTest(t, repo, "checkout", "main")

	taskID := createTaskForTest(t, server.routes(), projectID, "worker task")
	// 实现 Run 正常结束会把任务留在待验收态（这里跳过 Agent 执行，直接摆到这一步）。
	// 确认发布时只收口 awaiting_review / action_required，与 mergeTaskBranchToMain 一致。
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
		t.Fatalf("mark task awaiting review: %v", err)
	}
	seedReleaseTestJob(t, server, projectID, "job-worker", taskID, "queued", "")
	if _, err := server.db.Exec(`update git_task_records set base_dev_sha=?,task_branch='task/worker',worktree_path=?,task_commit_sha=? where job_id='job-worker'`, commit, worktree, commit); err != nil {
		t.Fatalf("bind worker record: %v", err)
	}
	// 与 processProjectOrchestration 一致：先取项目租约，再把 token 绑到 job 上。
	token, ok := server.acquireOrchestrationLease(ctx, projectID)
	if !ok {
		t.Fatal("acquire orchestration lease")
	}
	if _, err := server.db.Exec(`update task_orchestration_jobs set lease_token=? where id='job-worker'`, token); err != nil {
		t.Fatalf("bind orchestration lease: %v", err)
	}
	record, err := server.orchestrationTaskRecord(ctx, "job-worker")
	if err != nil {
		t.Fatalf("load worker record: %v", err)
	}
	cfg, err := server.orchestrationConfig(ctx, projectID)
	if err != nil {
		t.Fatalf("load orchestration config: %v", err)
	}
	job := OrchestrationJob{ID: "job-worker", ProjectID: projectID, TaskID: taskID, Status: orchestrationQueued, LeaseToken: token}
	if err := server.resumeCommittedOrchestrationReview(ctx, job, cfg, record); err != nil {
		t.Fatalf("worker finish committed implementation: %v", err)
	}

	// worker 真实落库的状态与集成提交。
	if status := orchestrationJobStatusForTest(t, server, "job-worker"); status != "awaiting_main" {
		t.Fatalf("worker job status=%s want awaiting_main", status)
	}
	var integrationSHA string
	if err := server.db.QueryRow(`select integration_sha from git_task_records where job_id='job-worker'`).Scan(&integrationSHA); err != nil {
		t.Fatalf("read integration sha: %v", err)
	}
	if integrationSHA != commit {
		t.Fatalf("integration sha=%s want %s", integrationSHA, commit)
	}

	// 固定快照 → 合入稳定分支 → 确认发布。
	create := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/"+projectID+"/orchestration/releases")
	if create.Code != http.StatusCreated {
		t.Fatalf("create release status=%d body=%s", create.Code, create.Body.String())
	}
	snapshot := decodeReleaseSnapshot(t, create)
	runGitForTest(t, repo, "merge", "--ff-only", snapshot.Branch)
	confirm := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/"+projectID+"/orchestration/releases/"+snapshot.ID+"/confirm")
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirm.Code, confirm.Body.String())
	}
	if status := orchestrationJobStatusForTest(t, server, "job-worker"); status != "released_to_main" {
		t.Fatalf("worker job status=%s want released_to_main", status)
	}
	assertTaskStatus(t, server, taskID, taskDone)
}

// TestReleaseSnapshotRoutesValidateTargets 覆盖路由参数校验：未知项目 404，缺失 dev
// 分支 409 且提示分支名，未知快照 404。
func TestReleaseSnapshotRoutesValidateTargets(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	seedGitProjectForTest(t, server, "no-dev-project", repo)

	missingProject := releaseSnapshotRequest(t, server, http.MethodGet, "/api/projects/absent/orchestration/releases")
	if missingProject.Code != http.StatusNotFound {
		t.Fatalf("unknown project list status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}

	noDevBranch := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/no-dev-project/orchestration/releases")
	if noDevBranch.Code != http.StatusConflict || !strings.Contains(noDevBranch.Body.String(), "dev branch dev is unavailable") {
		t.Fatalf("missing dev branch status=%d body=%s", noDevBranch.Code, noDevBranch.Body.String())
	}

	unknownSnapshot := releaseSnapshotRequest(t, server, http.MethodPost, "/api/projects/no-dev-project/orchestration/releases/absent/confirm")
	if unknownSnapshot.Code != http.StatusNotFound {
		t.Fatalf("unknown snapshot status=%d body=%s", unknownSnapshot.Code, unknownSnapshot.Body.String())
	}
}
