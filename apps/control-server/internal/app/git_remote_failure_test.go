package app

import (
	"context"
	"fmt"
	"testing"
)

// TestGitOperationFailureClassifiesRemoteOperationFailures 用**实测的 git stderr**钉住
// 远端操作失败的分类。
//
// 这些 stderr 是本机 git 2.52.0 真实跑出来的（不是照着文档编的），每一段都对应
// 用户会真的遇到的一次失败。判据只有两条：
//
//   1. **不能落到零信息量的兜底上**。兜底曾经是"Git 操作失败"——
//      实测七种常见失败里有六种都落到它上面，用户既不知道成没成，也不知道该做什么。
//   2. **每种失败都要说得出下一步**（先 fetch / 去配凭据 / 处理分叉 / 检查分支名）。
func TestGitOperationFailureClassifiesRemoteOperationFailures(t *testing.T) {
	cases := []struct {
		name, stderr, wantCode, wantMessage string
	}{
		{
			// git pull --ff-only 遇到历史分叉（实测 stderr）。
			name:   "pull diverged",
			stderr: "From /tmp/remote\n * branch            main       -> FETCH_HEAD\n   4af3a2b..3e55c49  main       -> origin/main\nhint: Diverging branches can't be fast-forwarded, you need to either:\nhint:\nhint: \tgit merge --no-ff\nhint: Disable this message with \"git config set advice.diverging false\"\nfatal: Not possible to fast-forward, aborting.\n",
			wantCode: "pull_diverged", wantMessage: "本地与远端历史已分叉，无法快进合并。请在电脑上先合并或变基，再重新拉取",
		},
		{
			// 推送被拒：远端有本地没有的提交。
			name:   "push rejected",
			stderr: "To /tmp/remote.git\n ! [rejected]        main -> main (non-fast-forward)\nerror: failed to push some refs to '/tmp/remote.git'\nhint: Updates were rejected because the tip of your current branch is behind\n",
			wantCode: "push_rejected", wantMessage: "推送被拒绝，远端有本地没有的提交。请先获取（fetch）远端更新并合入，再推送",
		},
		{
			// HTTPS 远端仓库不存在或无权访问（私有库没授权时 git 也报这句）。
			name:   "remote repository not found",
			stderr: "remote: Repository not found.\nfatal: repository 'https://github.com/nonexistent-org-xyz/nonexistent-repo-abc.git/' not found\n",
			wantCode: "remote_not_found", wantMessage: "远端仓库不存在或无权访问，请检查仓库地址与访问权限",
		},
		{
			// 远端名写错。
			name:   "remote name typo",
			stderr: "fatal: 'nosuchremote' does not appear to be a git repository\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n",
			wantCode: "remote_not_configured", wantMessage: "远端地址不可用，请检查远端名称与 URL 是否配置正确",
		},
		{
			// 远端没有这个分支。
			name:   "remote ref missing",
			stderr: "fatal: couldn't find remote ref main\n",
			wantCode: "remote_ref_missing", wantMessage: "远端没有这个分支，请检查分支名是否正确",
		},
		{
			// DNS 解析不了。
			name:   "dns failure",
			stderr: "fatal: unable to access 'https://example.invalid/foo.git/': Could not resolve host: example.invalid\n",
			wantCode: "remote_unavailable", wantMessage: "无法连接到 Git 远端，请检查网络或远端地址",
		},
		{
			// 本地没有要推的提交。
			name:   "nothing to push",
			stderr: "error: src refspec main does not match any\nerror: failed to push some refs to '/tmp/remote.git'\n",
			wantCode: "nothing_to_push", wantMessage: "没有可推送的提交，请先创建提交",
		},
		{
			// 当前分支没有上游。
			name:   "no upstream",
			stderr: "fatal: The current branch feature has no upstream branch.\nTo push the current branch and set the remote as upstream, use\n\n    git push --set-upstream origin feature\n",
			wantCode: "no_upstream", wantMessage: "当前分支还没有上游分支，请先设置跟踪分支再推送",
		},
		{
			// 远端要交互输入用户名（凭据没配 / 终端提示被禁用）。
			name:   "credentials required",
			stderr: "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n",
			wantCode: "credentials_required", wantMessage: "远端需要身份凭据。请先在电脑上配置好 Git 凭据（SSH 密钥或凭据管理器），再重试",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, code, message := gitOperationFailure(&gitCommandError{command: "push", stderr: test.stderr})
			if status != gitOperationFailed {
				t.Fatalf("status=%q want=%q", status, gitOperationFailed)
			}
			if code != test.wantCode {
				t.Fatalf("code=%q want=%q (message=%q)", code, test.wantCode, message)
			}
			if message != test.wantMessage {
				t.Fatalf("message=%q want=%q", message, test.wantMessage)
			}
		})
	}
}

// TestGitOperationFailureNeverFallsBackToAnEmptyMessage 守住"兜底也要有信息"这条底线。
//
// 兜底句可以变化，但**不能是零信息量**的那句 —— 实测七种常见失败里有六种会落到兜底，
// 兜底说什么，用户看到的就是什么。
func TestGitOperationFailureNeverFallsBackToAnEmptyMessage(t *testing.T) {
	_, code, message := gitOperationFailure(&gitCommandError{command: "push", stderr: "some unknown git failure"})
	if code != "git_failed" {
		t.Fatalf("code=%q", code)
	}
	if message == "Git 操作失败" {
		t.Fatalf("兜底文案不得是零信息量的 %q", message)
	}
	if message == "" {
		t.Fatal("兜底文案不得为空")
	}
}

// TestGitOperationFailureReportsTimeout 命令超时不带 gitCommandError（runGit 直接把
// ctx 的错误返回上来），必须单独判 —— 否则它也掉进零信息量的兜底。
func TestGitOperationFailureReportsTimeout(t *testing.T) {
	status, code, message := gitOperationFailure(context.DeadlineExceeded)
	if status != gitOperationFailed || code != "git_timeout" {
		t.Fatalf("status=%q code=%q", status, code)
	}
	if message == "" || message == "Git 操作失败" {
		t.Fatalf("超时文案不得是零信息量的 %q", message)
	}
}

// TestGitOperationFailureDoesNotMistakeLocalPathDenialForAuthFailure 守住一个误判：
// "access denied" 也会出现在文件系统的 "unable to access '<路径>': Access denied" 里，
// 那是**本机路径权限**问题，不是远端认证问题 —— 报成"认证失败"会把用户引到错的方向。
// 远端认证的两种真实形态（"Authentication failed" / "Permission denied (publickey)"）
// 各自都有判据，不依赖 "access denied"。
func TestGitOperationFailureDoesNotMistakeLocalPathDenialForAuthFailure(t *testing.T) {
	_, code, _ := gitOperationFailure(&gitCommandError{command: "push", stderr: "fatal: unable to access 'C:/repo/.git': Access denied"})
	if code == "auth_failed" {
		t.Fatal("本机路径权限问题不得报成远端认证失败")
	}
	// 远端认证的两种真实形态仍要判成认证失败。
	for _, stderr := range []string{
		"fatal: Authentication failed for 'https://github.com/x/y.git/'",
		"git@github.com: Permission denied (publickey).",
	} {
		if _, code, _ := gitOperationFailure(&gitCommandError{command: "push", stderr: stderr}); code != "auth_failed" {
			t.Fatalf("stderr=%q code=%q want=auth_failed", stderr, code)
		}
	}
}

// TestGitOperationFailureReportsHungProcess git 挂死且强杀不掉，与"命令超时"不是一回事。
func TestGitOperationFailureReportsHungProcess(t *testing.T) {
	_, code, message := gitOperationFailure(fmt.Errorf("%w: %v", errGitTerminationTimedOut, context.DeadlineExceeded))
	if code != "git_hung" {
		t.Fatalf("code=%q want=git_hung", code)
	}
	if message == "" || message == "Git 操作失败" {
		t.Fatalf("卡死文案不得是零信息量的 %q", message)
	}
}
