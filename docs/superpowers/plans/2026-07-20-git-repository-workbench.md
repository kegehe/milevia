# Git Repository Workbench Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provide a project-scoped Git workbench for status, diffs, staging, commits, fetch, push, and safe branch creation/switching.

**Architecture:** The Go control server gets a typed native-Git adapter and persistent Git-operation service. Git mutations and Claude Run admission use one project workspace lease. React renders a workbench overlay from typed HTTP and project-WebSocket contracts.

**Tech Stack:** Go 1.26, Chi, SQLite, native Git CLI, Gorilla WebSocket, React 19, TypeScript, Vite.

## Global Constraints

- Work only in an isolated worktree containing the current task-board changes; never overwrite or commit unrelated changes.
- Use direct exec.CommandContext argv, GIT_TERMINAL_PROMPT=0, rejecting GIT_ASKPASS, bounded output, and process-group cleanup.
- Diff uses --no-ext-diff --no-textconv; Git network operations reject ext, file, and custom helper protocols.
- stateToken is opaque and server-issued. It binds a canonical state digest and expiry; time never participates in equality.
- Git writes and Claude Run admission use the same ProjectWorkspaceLease.
- No reset, clean, merge, rebase, pull, stash, force push, branch deletion, remote editing, arbitrary Git config, or browser credential entry.

## File Structure

| File | Responsibility |
| --- | --- |
| apps/control-server/internal/app/git.go | Typed Git data, NUL parsers, constrained CLI calls, reads and mutations. |
| apps/control-server/internal/app/git_operations.go | Migration, operation lifecycle/recovery, state tokens, workspace lease, HTTP handlers. |
| apps/control-server/internal/app/app.go | Service construction, routes, project events, Claude admission/terminal lease integration. |
| apps/control-server/internal/app/app_test.go | Temporary-repository integration tests. |
| apps/web/src/features/git/git-model.ts | DTOs and pure presentation helpers. |
| apps/web/src/features/git/GitWorkbench.tsx | Workbench tabs, confirmations, API/event lifecycle. |
| apps/web/src/git.css | Responsive Git workbench styles. |
| apps/web/src/App.tsx | Project-header Git trigger and workbench mount. |

### Task 1: Safe Git Read Model

**Files:** Create apps/control-server/internal/app/git.go; modify apps/control-server/internal/app/app_test.go.

**Interfaces:** Produce GitSnapshot, GitChange, GitBranch, GitCommit, GitDiffStage, and GitRunner. GitRunner exposes Snapshot, Changes, Diff, Log, and Branches using a server-owned repository path.

- [x] **Step 1: Write failing temporary-repository tests**

    func TestGitSnapshotParsesNULPathsAndMissingUpstream(t *testing.T) {
        repo := newTempGitRepository(t)
        writeFile(t, repo, "已修改\n文件.txt", "one\n")
        snapshot, err := newGitRunner().Snapshot(context.Background(), repo)
        if err != nil { t.Fatal(err) }
        if snapshot.Worktree.Untracked != 1 || snapshot.Head.Upstream != "" { t.Fatalf("snapshot=%#v", snapshot) }
    }

    func TestGitDiffDoesNotRunTextconv(t *testing.T) {
        repo := newTempGitRepository(t)
        configureTextconvThatWritesMarker(t, repo)
        writeFile(t, repo, "asset.bin", "changed")
        _, err := newGitRunner().Diff(context.Background(), repo, "asset.bin", gitDiffWorktree)
        if err != nil { t.Fatal(err) }
        if fileExists(filepath.Join(repo, "textconv-ran")) { t.Fatal("textconv executed") }
    }

- [x] **Step 2: Verify RED**

Run: cd apps/control-server && go test ./internal/app -run 'TestGit(Snapshot|Diff)' -count=1

Expected: FAIL because newGitRunner and the Git types do not exist.

- [x] **Step 3: Implement the smallest safe reader**

    func (r *gitCLIRunner) Diff(ctx context.Context, repo, path string, stage GitDiffStage) (string, error) {
        args := []string{"diff", "--no-ext-diff", "--no-textconv"}
        if stage == gitDiffIndex { args = append(args, "--cached") }
        out, err := r.command(ctx, repo, append(args, "--", path)...)
        return string(out), err
    }

Parse git status --porcelain=v2 --branch -z only on NUL boundaries. Use fixed NUL log formatting, cap output, and validate each path relative to the repository root.

- [ ] **Step 4: Verify GREEN and commit**

Run: cd apps/control-server && go test ./internal/app -run 'TestGit(Snapshot|Diff)' -count=1

Then: git add apps/control-server/internal/app/git.go apps/control-server/internal/app/app_test.go && git commit -m "feat: add safe Git repository reader"

### Task 2: Operations, Tokens, and Shared Workspace Lease

**Files:** Create apps/control-server/internal/app/git_operations.go; modify apps/control-server/internal/app/app.go and apps/control-server/internal/app/app_test.go.

**Interfaces:** Produce GitOperation, migrateGit, recoverGitOperations, acquireWorkspace, and projectGitRoutes. Add a project-wide lease used by both startMessage/finishRun and Git mutation handlers.

- [ ] **Step 1: Write failing concurrency and recovery tests**

    func TestGitMutationIsRejectedWhileProjectRunOwnsWorkspace(t *testing.T) {
        server, projectID, conversationID := seedProjectConversation(t)
        startQueuedRun(t, server, conversationID)
        response := requireRequest(t, server.routes(), http.MethodPost,
            "/api/projects/"+projectID+"/git/fetch", "{\"remote\":\"origin\",\"stateToken\":\"token\"}")
        if response.Code != http.StatusConflict { t.Fatalf("status=%d body=%s", response.Code, response.Body.String()) }
    }

    func TestUncertainPushRecoveryNeedsAttention(t *testing.T) {
        server := newTestServer(t)
        operationID := seedRunningPushOperation(t, server)
        if err := server.recoverGitOperations(context.Background()); err != nil { t.Fatal(err) }
        assertGitOperationStatus(t, server, operationID, gitOperationNeedsAttention)
    }

- [ ] **Step 2: Verify RED**

Run: cd apps/control-server && go test ./internal/app -run 'Test(GitMutationIsRejected|UncertainPushRecovery)' -count=1

Expected: FAIL because there are no operation records, project lease, or recovery path.

- [ ] **Step 3: Implement the persistent boundary**

Create git_operations with operation state, request/pre/post snapshots, sanitized excerpts, executor lease and timestamps. Store opaque state tokens server-side with canonical status/index/file fingerprints and short expiry. At process recovery, make unproven pushes needs_attention; never replay a write. Extend Server with workspaceLeases map guarded by the server mutex, acquire it before Claude admission and Git mutation, and release it only when the operation or Run becomes terminal.

- [ ] **Step 4: Verify GREEN and commit**

Run: cd apps/control-server && go test ./internal/app -run 'Test(GitMutationIsRejected|UncertainPushRecovery)' -count=1

Then: git add apps/control-server/internal/app/git_operations.go apps/control-server/internal/app/app.go apps/control-server/internal/app/app_test.go && git commit -m "feat: coordinate Git operations with project runs"

### Task 3: Read APIs and Project Events

**Files:** Modify git_operations.go, app.go, and app_test.go.

**Interfaces:** Add GET routes for summary, changes, diff, log, commits by OID, branches, and operations, plus GET /ws/projects/{projectID}.

- [ ] **Step 1: Write a failing route test**

    func TestProjectGitSummaryReportsCurrentUntrackedFile(t *testing.T) {
        server, projectID, repo := seedGitProject(t)
        writeFile(t, repo, "new.txt", "new")
        response := requireRequest(t, server.routes(), http.MethodGet, "/api/projects/"+projectID+"/git/summary", "")
        if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "\"untracked\":1") { t.Fatalf("%d %s", response.Code, response.Body.String()) }
    }

Also test that absolute diff paths are rejected and operation history can be read after a project-socket reconnect.

- [ ] **Step 2: Verify RED**

Run: cd apps/control-server && go test ./internal/app -run TestProjectGit -count=1

Expected: FAIL with 404 because project Git routes do not exist.

- [ ] **Step 3: Implement routes and replayable events**

Resolve every request through projectID, never a submitted repository path. Validate stage, pagination, ref, OID, and path before invoking GitRunner. Persist every operation transition before broadcasting git.operation.updated. A reconnect reads operations plus summary before subscribing.

- [ ] **Step 4: Verify GREEN and commit**

Run: cd apps/control-server && go test ./internal/app -run TestProjectGit -count=1

Then: git add apps/control-server/internal/app/git_operations.go apps/control-server/internal/app/app.go apps/control-server/internal/app/app_test.go && git commit -m "feat: expose project Git state and events"

### Task 4: Controlled Git Mutations

**Files:** Modify git.go, git_operations.go, and app_test.go.

**Interfaces:** Add POST routes for stage, unstage, commits, fetch, push, branches, and switch. All return HTTP 202 and an operation ID.

- [ ] **Step 1: Write a failing mutation test**

    func TestGitCommitRequiresFreshStateToken(t *testing.T) {
        server, projectID, repo := seedGitProject(t)
        writeFile(t, repo, "feature.txt", "first")
        token := gitSummary(t, server, projectID).StateToken
        stageThroughAPI(t, server, projectID, token, "feature.txt")
        requireStatus(t, server.routes(), http.MethodPost, "/api/projects/"+projectID+"/git/commits",
            "{\"message\":\"add feature\",\"stateToken\":\""+token+"\"}", http.StatusConflict)
        commitThroughAPI(t, server, projectID, gitSummary(t, server, projectID).StateToken, "add feature")
        assertHeadMessage(t, repo, "add feature")
    }

Cover unstage, hook rejection, normal push to a local bare remote, non-fast-forward failure, protected-branch rejection, dirty switch rejection, and force/protocol rejection.

- [ ] **Step 2: Verify RED**

Run: cd apps/control-server && go test ./internal/app -run 'TestGit(Commit|Push|Switch)' -count=1

Expected: FAIL because mutation routes and workers are absent.

- [ ] **Step 3: Implement fixed argv operations**

    func (r *gitCLIRunner) Push(ctx context.Context, repo, remote, branch string, upstream bool) error {
        args := []string{"push"}
        if upstream { args = append(args, "--set-upstream") }
        _, err := r.command(ctx, repo, append(args, remote, "HEAD:"+branch)...)
        return err
    }

Re-read the opaque token inside the workspace lease. Validate selected paths against the just-read change set, validate branches with git check-ref-format --branch, validate remotes against server-listed remotes, reject protected patterns and force/refspec fields, and use protocol.ext.allow=never and protocol.file.allow=never for fetch/push. Write commit text to a mode-0600 temporary file and remove it with defer.

- [ ] **Step 4: Verify GREEN and commit**

Run: cd apps/control-server && go test ./internal/app -run 'TestGit(Commit|Push|Switch)' -count=1

Then: git add apps/control-server/internal/app/git.go apps/control-server/internal/app/git_operations.go apps/control-server/internal/app/app_test.go && git commit -m "feat: add controlled Git mutations"

### Task 5: React Git Workbench

**Files:** Create apps/web/src/features/git/git-model.ts, apps/web/src/features/git/GitWorkbench.tsx, apps/web/src/git.css; modify apps/web/src/App.tsx, apps/web/package.json, and pnpm-lock.yaml.

**Interfaces:** Produce GitWorkbench with projectID, request, fail, and close props. It consumes all Task 3/4 routes and project events.

- [ ] **Step 1: Write a failing pure view-model test**

    it("keeps staged and unstaged versions of one path distinct", () => {
      expect(groupChanges([{ path: "app.go", staged: true }, { path: "app.go", modified: true }]))
        .toEqual({ staged: ["app.go"], unstaged: ["app.go"] });
    });

- [ ] **Step 2: Verify RED**

Run: pnpm --filter @auto/web test -- git-model.test.ts

Expected: FAIL because the model module and test command are absent.

- [ ] **Step 3: Implement the test command and workbench UI**

Add `vitest` as a web dev dependency and `"test": "vitest run"` to apps/web/package.json before implementing the model. Add typed DTOs, then a compact overlay with overview, changes, history, branches, and operations tabs. Keep staged and worktree diffs separate. Every mutation opens a confirmation view, sends the latest stateToken, displays queued operation status, and refreshes only from server state. Add a Git-status trigger to the project header, subscribe only while open, and preserve conversation WebSocket behavior.

- [ ] **Step 4: Verify GREEN and commit**

Run: pnpm --filter @auto/web test -- git-model.test.ts && pnpm --filter @auto/web build

Then: git add apps/web/src/features/git apps/web/src/git.css apps/web/src/App.tsx apps/web/package.json pnpm-lock.yaml && git commit -m "feat: add project Git workbench"

### Task 6: Full Regression

**Files:** Modify document 08 only if implementation changes a documented contract; modify README only for delivery status.

- [ ] **Step 1: Write terminal-operation regression test**

    func TestGitCommitOperationRefreshesSnapshot(t *testing.T) {
        server, projectID, repo := seedGitProject(t)
        writeFile(t, repo, "commit.txt", "content")
        stageAndCommitThroughAPI(t, server, projectID, "commit.txt", "commit through API")
        if got := gitSummary(t, server, projectID).Worktree.Staged; got != 0 { t.Fatalf("staged=%d", got) }
        assertLatestGitOperation(t, server, projectID, gitOperationSucceeded)
    }

- [ ] **Step 2: Run complete verification**

Run: cd apps/control-server && go test -race ./... && go vet ./... && go build ./cmd/control-server

Run: pnpm --filter @auto/web build

Run: git diff --check

Expected: every command exits 0.

- [ ] **Step 3: Commit only after verification**

Then: git add docs/08-Git仓库管理与协作.md README.md && git commit -m "docs: record Git workbench delivery"
