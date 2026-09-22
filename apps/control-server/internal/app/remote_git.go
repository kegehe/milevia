package app

import "net/http"

// 手机端能执行的全部 **Git** 操作。名单类型与合成逻辑在 remote_relay.go。
//
// 这 28 条是电脑端 Git 工作台的全部 REST 端点（app.go 里 /api/projects/{projectID}/git/*
// 那一组），一条不多一条不少：手机端要整块复用 GitWorkbench，就得让它发出的每个请求
// 都有对应的 op —— 少一条的症状是那个 tab 静默空白。
//
// 三条纪律：
//
//   - **名单写死在测试里**（TestGitRemoteOperationWhitelistIsExplicit）：多一个少一个
//     都是能改仓库状态的能力在移动，改实现必须同时改测试，逼出一次有意识的决定。
//   - **路径参数必须登记形状校验**（PathParams + relayPathParamValidators）：
//     `oid` / `suggestionID` 会被拼进 URL 路径，不校验就等于开了一个把任意字符串
//     拼进本机路由的口子。
//   - **MaxBytes 是"这个 op 的返回本来就可能太大"**，不是安全限制：超限时给一句
//     具体的话（"改动差异太大…请在电脑上查看"），而不是让它去撞通道帧判据、
//     拿一句用户看不懂的"超过中继通道容量"。
//
// 所有写操作都复用既有 handler，因此**工作区租约、stateToken 乐观锁、审计记录、
// 错误文案全部原样继承** —— 手机端不新增任何一条 Git 语义。
func (s *Server) remoteGitOperations() map[string]remoteOperation {
	return map[string]remoteOperation{
		// ── 只读 ────────────────────────────────────────────────────────────────
		"git.summary":  {Method: http.MethodGet, Path: "/git/summary", Query: true, Handle: s.gitSummary},
		"git.changes":  {Method: http.MethodGet, Path: "/git/changes", Query: true, MaxBytes: 320 << 10, Label: "变更列表", Handle: s.gitChanges},
		"git.diff":     {Method: http.MethodGet, Path: "/git/diff", Query: true, MaxBytes: 320 << 10, Label: "改动差异", Handle: s.gitDiff},
		"git.log":      {Method: http.MethodGet, Path: "/git/log", Query: true, MaxBytes: 256 << 10, Label: "提交历史", Handle: s.gitLog},
		"git.branches": {Method: http.MethodGet, Path: "/git/branches", Query: true, MaxBytes: 128 << 10, Label: "分支列表", Handle: s.gitBranches},
		"git.operations": { // 审计记录：手机端"超时≠失败"那条文案的全部依据（见 docs/41 §3.2）
			Method: http.MethodGet, Path: "/git/operations", Query: true, MaxBytes: 256 << 10, Label: "操作记录", Handle: s.gitOperations,
		},
		"git.commit": { // 读单条提交详情。写入那条叫 git.commitCreate，别混
			Method: http.MethodGet, Path: "/git/commits/{oid}", Query: true,
			PathParams: []string{"oid"}, MaxBytes: 256 << 10, Label: "提交详情", Handle: s.gitCommitDetail,
		},
		"git.commitDiff": {
			Method: http.MethodGet, Path: "/git/commits/{oid}/diff", Query: true,
			PathParams: []string{"oid"}, MaxBytes: 320 << 10, Label: "提交差异", Handle: s.gitCommitDiff,
		},

		// ── 冲突（读）──────────────────────────────────────────────────────────
		"git.conflicts": {
			Method: http.MethodGet, Path: "/git/conflicts", Query: true, MaxBytes: 128 << 10, Label: "冲突列表", Handle: s.gitConflicts,
		},
		"git.conflictContent": {
			Method: http.MethodGet, Path: "/git/conflicts/content", Query: true,
			MaxBytes: 320 << 10, Label: "冲突内容", Handle: s.gitConflictContent,
		},
		"git.suggestion": {
			Method: http.MethodGet, Path: "/git/conflicts/suggestions/{suggestionID}", Query: true,
			PathParams: []string{"suggestionID"}, MaxBytes: 256 << 10, Label: "合并建议", Handle: s.gitConflictSuggestionStatus,
		},

		// ── 变更 / 提交 / 丢弃 ──────────────────────────────────────────────────
		"git.stage":      {Method: http.MethodPost, Path: "/git/stage", Handle: s.gitStage},
		"git.unstage":    {Method: http.MethodPost, Path: "/git/unstage", Handle: s.gitUnstage},
		"git.stageAll":   {Method: http.MethodPost, Path: "/git/stage-all", Handle: s.gitStageAll},
		"git.unstageAll": {Method: http.MethodPost, Path: "/git/unstage-all", Handle: s.gitUnstageAll},
		// 写入那条与只读的 git.commit 同名会让人读错，所以显式叫 Create。
		"git.commitCreate": {Method: http.MethodPost, Path: "/git/commits", Handle: s.gitCommit},
		"git.commitAmend":  {Method: http.MethodPost, Path: "/git/commits/amend", Handle: s.gitAmendCommit},
		// discard 会**丢掉未提交的改动**且不可恢复，确认层由 GitConfirmation 承担（逐字沿用桌面端）。
		"git.discard": {Method: http.MethodPost, Path: "/git/discard", Handle: s.gitDiscard},

		// ── 远端 ────────────────────────────────────────────────────────────────
		"git.fetch": {Method: http.MethodPost, Path: "/git/fetch", Handle: s.gitFetch},
		// pull 在服务端是 `pull --prune --ff-only`（git.go:332）⇒ 不会产生 merge 冲突，
		// 所以它可以安全地出现在手机上。
		"git.pull": {Method: http.MethodPost, Path: "/git/pull", Handle: s.gitPull},
		"git.push": {Method: http.MethodPost, Path: "/git/push", Handle: s.gitPush},

		// ── 分支 ────────────────────────────────────────────────────────────────
		"git.createBranch": {Method: http.MethodPost, Path: "/git/branches", Handle: s.gitCreateBranch},
		"git.switch":       {Method: http.MethodPost, Path: "/git/switch", Handle: s.gitSwitchBranch},

		// ── 冲突（写）──────────────────────────────────────────────────────────
		"git.conflictResolve":  {Method: http.MethodPost, Path: "/git/conflicts/resolve", Handle: s.gitConflictResolve},
		"git.conflictAbort":    {Method: http.MethodPost, Path: "/git/conflicts/abort", Handle: s.gitConflictAbort},
		"git.conflictContinue": {Method: http.MethodPost, Path: "/git/conflicts/continue", Handle: s.gitConflictContinue},
		// 建议是**异步**的（立刻 202 + 轮询 git.suggestion），所以它不会撞上通道超时。
		"git.conflictSuggest": {Method: http.MethodPost, Path: "/git/conflicts/suggest", Handle: s.gitConflictSuggest},
		"git.suggestionCancel": {
			Method: http.MethodPost, Path: "/git/conflicts/suggestions/{suggestionID}/cancel",
			PathParams: []string{"suggestionID"}, Handle: s.gitConflictSuggestCancel,
		},
	}
}
