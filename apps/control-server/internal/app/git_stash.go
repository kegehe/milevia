package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errGitStashApplyFailed      = errors.New("Git stash restore needs inspection")
	errGitStashCreateIncomplete = errors.New("Git stash creation needs inspection")
)

type GitStash struct {
	OID       string    `json:"oid"`
	Ref       string    `json:"ref"`
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"createdAt"`
}

func listGitStashes(ctx context.Context, runner GitRunner, repo string) ([]GitStash, error) {
	output, err := runner.runGit(ctx, repo, "stash", "list", "--format=%H%x00%gd%x00%gs%x00%cI%x00")
	if err != nil {
		return nil, err
	}
	stashes := []GitStash{}
	fields := strings.Split(string(output), "\x00")
	for i := 0; i+3 < len(fields); i += 4 {
		oid := strings.TrimSpace(fields[i])
		createdAt, err := time.Parse(time.RFC3339, fields[i+3])
		if !isFullGitObjectID(oid) || err != nil {
			return nil, errors.New("invalid Git stash list output")
		}
		stashes = append(stashes, GitStash{OID: oid, Ref: fields[i+1], Subject: fields[i+2], CreatedAt: createdAt})
	}
	return stashes, nil
}

func (s *Server) gitStashes(w http.ResponseWriter, r *http.Request) {
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return
	}
	stashes, err := listGitStashes(r.Context(), runner, repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stashes)
}

func (s *Server) gitStashCreate(w http.ResponseWriter, r *http.Request) {
	s.gitStashMutation(w, r, false)
}

func (s *Server) gitStashApply(w http.ResponseWriter, r *http.Request) {
	s.gitStashMutation(w, r, true)
}

func (s *Server) gitStashMutation(w http.ResponseWriter, r *http.Request, apply bool) {
	var input struct {
		StateToken   string `json:"stateToken"`
		Message      string `json:"message"`
		OID          string `json:"oid"`
		RestoreIndex *bool  `json:"restoreIndex"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Message = strings.TrimSpace(input.Message)
	if utf8.RuneCountInString(input.Message) > 200 || strings.ContainsRune(input.Message, '\x00') {
		writeError(w, http.StatusBadRequest, errors.New("收起说明不能超过 200 个字符或包含空字符"))
		return
	}
	// 使用不可变的对象 ID，防止别的工作区新建 Stash 后恢复了错误的 stash@{n}。
	if apply && !isFullGitObjectID(input.OID) {
		writeError(w, http.StatusBadRequest, errors.New("无效的 Stash 对象 ID"))
		return
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	if !hasGitHead(state.snapshot) {
		writeError(w, http.StatusConflict, errors.New("创建首次提交后才能使用 Stash"))
		return
	}
	overview, err := runner.ConflictOverview(r.Context(), repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	if state.snapshot.Worktree.Conflicted > 0 || overview.Context.OperationType != GitConflictOperationNone {
		writeError(w, http.StatusConflict, errors.New("请先解决冲突并完成或中止当前 Git 操作，再使用 Stash"))
		return
	}
	typ, summary := "stash_create", "收起改动（含未跟踪文件）"
	args := []string{"stash", "push", "--include-untracked"}
	if apply {
		if len(state.changes) > 0 {
			writeError(w, http.StatusConflict, errors.New("恢复 Stash 前请先提交、收起或丢弃当前工作区改动"))
			return
		}
		stashes, err := listGitStashes(r.Context(), runner, repo)
		if err != nil {
			s.writeGitReadError(w, err)
			return
		}
		found := false
		for _, stash := range stashes {
			if stash.OID == input.OID {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusConflict, errors.New("该 Stash 已不存在，请刷新列表"))
			return
		}
		typ, summary = "stash_apply", "恢复 Stash "+input.OID[:8]+"（保留备份）"
		args = []string{"stash", "apply"}
		if input.RestoreIndex == nil || *input.RestoreIndex {
			args = append(args, "--index")
		} else {
			summary += "，不还原暂存状态"
		}
		args = append(args, input.OID)
	} else {
		if len(state.changes) == 0 {
			writeError(w, http.StatusConflict, errGitNoChanges)
			return
		}
		if input.Message != "" {
			args = append(args, "--message", input.Message)
			summary += "：" + input.Message
		}
	}
	result, err := s.executeGitOperationForWorkspace(r.Context(), runner, projectID, state.workspaceID, state.workspacePath, repo, typ, summary, state.snapshot, func(runner GitRunner) error {
		_, err := runner.runGit(r.Context(), repo, args...)
		if err != nil {
			// push/apply 失败时也可能已保存备份或处理部分文件，结果必须人工核对。
			if apply {
				return fmt.Errorf("%w: %v", errGitStashApplyFailed, err)
			}
			return fmt.Errorf("%w: %v", errGitStashCreateIncomplete, err)
		}
		if !apply {
			// Git 不会收起子模块内部的改动，甚至可能零退出却没有创建任何 Stash。
			// 只有确认没有剩余改动，才能告诉用户可以继续切分支或移除工作区。
			remaining, err := runner.Changes(r.Context(), repo)
			if err != nil || len(remaining) != 0 {
				return errGitStashCreateIncomplete
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
