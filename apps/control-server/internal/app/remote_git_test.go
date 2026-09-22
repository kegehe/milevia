package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 本文件只测**不构造 Server 的东西**：op 表的形状，以及 relayTarget / 路径参数 /
// 超限文案这几个纯函数。
//
// 为什么刻意这么切：这些正是"错了也不会有人发现"的部分 —— 名单多一条少一条、
// 占位符没有校验器、某个 op 的上限排在通道判据之后、conversationId 被 params 覆盖。
// 它们全都能在不碰数据库、不拉起子进程的前提下穷举，所以能在任何环境里跑
// （构造完整 Server 的用例在本沙箱会被 wsl.exe 拦截，见 docs/41 §13）。

// ─── Git op 名单 ────────────────────────────────────────────────────────────

// 这 28 条是手机端能对仓库做的全部事情。多一条（偷偷开了写能力）或少一条
// （某个 tab 静默空白）都必须在这里同时改，逼出一次有意识的决定。
func TestGitRemoteOperationWhitelistIsExplicit(t *testing.T) {
	server := &Server{}
	expected := []struct {
		op         string
		method     string
		path       string
		pathParams []string
		maxBytes   int
	}{
		{"git.summary", http.MethodGet, "/git/summary", nil, 0},
		{"git.changes", http.MethodGet, "/git/changes", nil, 320 << 10},
		{"git.diff", http.MethodGet, "/git/diff", nil, 320 << 10},
		{"git.log", http.MethodGet, "/git/log", nil, 256 << 10},
		{"git.branches", http.MethodGet, "/git/branches", nil, 128 << 10},
		{"git.operations", http.MethodGet, "/git/operations", nil, 256 << 10},
		{"git.commit", http.MethodGet, "/git/commits/{oid}", []string{"oid"}, 256 << 10},
		{"git.commitDiff", http.MethodGet, "/git/commits/{oid}/diff", []string{"oid"}, 320 << 10},
		{"git.conflicts", http.MethodGet, "/git/conflicts", nil, 128 << 10},
		{"git.conflictContent", http.MethodGet, "/git/conflicts/content", nil, 320 << 10},
		{"git.suggestion", http.MethodGet, "/git/conflicts/suggestions/{suggestionID}", []string{"suggestionID"}, 256 << 10},
		{"git.stage", http.MethodPost, "/git/stage", nil, 0},
		{"git.unstage", http.MethodPost, "/git/unstage", nil, 0},
		{"git.stageAll", http.MethodPost, "/git/stage-all", nil, 0},
		{"git.unstageAll", http.MethodPost, "/git/unstage-all", nil, 0},
		{"git.commitCreate", http.MethodPost, "/git/commits", nil, 0},
		{"git.commitAmend", http.MethodPost, "/git/commits/amend", nil, 0},
		{"git.discard", http.MethodPost, "/git/discard", nil, 0},
		{"git.fetch", http.MethodPost, "/git/fetch", nil, 0},
		{"git.pull", http.MethodPost, "/git/pull", nil, 0},
		{"git.push", http.MethodPost, "/git/push", nil, 0},
		{"git.createBranch", http.MethodPost, "/git/branches", nil, 0},
		{"git.switch", http.MethodPost, "/git/switch", nil, 0},
		{"git.conflictResolve", http.MethodPost, "/git/conflicts/resolve", nil, 0},
		{"git.conflictAbort", http.MethodPost, "/git/conflicts/abort", nil, 0},
		{"git.conflictContinue", http.MethodPost, "/git/conflicts/continue", nil, 0},
		{"git.conflictSuggest", http.MethodPost, "/git/conflicts/suggest", nil, 0},
		{"git.suggestionCancel", http.MethodPost, "/git/conflicts/suggestions/{suggestionID}/cancel", []string{"suggestionID"}, 0},
	}
	operations := server.remoteGitOperations()
	if len(operations) != len(expected) {
		t.Fatalf("Git operation count = %d, want %d", len(operations), len(expected))
	}
	for _, want := range expected {
		operation, ok := operations[want.op]
		if !ok {
			t.Fatalf("Git operation %q is missing", want.op)
		}
		if operation.Method != want.method {
			t.Errorf("%s method = %s, want %s", want.op, operation.Method, want.method)
		}
		if operation.Path != want.path {
			t.Errorf("%s path = %q, want %q", want.op, operation.Path, want.path)
		}
		if !sameStrings(operation.PathParams, want.pathParams) {
			t.Errorf("%s pathParams = %v, want %v", want.op, operation.PathParams, want.pathParams)
		}
		if operation.MaxBytes != want.maxBytes {
			t.Errorf("%s maxBytes = %d, want %d", want.op, operation.MaxBytes, want.maxBytes)
		}
		if operation.Handle == nil {
			t.Errorf("%s has no handler", want.op)
		}
		// 只读那批必须是查询参数（GET 不该带请求体），写入那批必须是请求体。
		if operation.Query != (want.method == http.MethodGet) {
			t.Errorf("%s query = %v, want %v for %s", want.op, operation.Query, want.method == http.MethodGet, want.method)
		}
	}
}

// 合成表的两条性质：不覆盖、不漏条。
func TestRelayCompositeOperationTableHasNoOverlap(t *testing.T) {
	server := &Server{}
	fileOperations := server.remoteFSOperations()
	gitOperations := server.remoteGitOperations()
	for name := range gitOperations {
		if _, clash := fileOperations[name]; clash {
			// 有交集意味着后注册的那份静默覆盖前一份：某个操作会跑到另一个 handler 上，
			// 而两边单独看代码都是对的。
			t.Fatalf("operation %q appears in both the file and the Git table", name)
		}
	}
	merged := server.remoteOperations()
	if len(merged) != len(fileOperations)+len(gitOperations) {
		t.Fatalf("merged table has %d operations, want %d", len(merged), len(fileOperations)+len(gitOperations))
	}
}

// 每个 op 的 MaxBytes 都必须**落在通道帧预算之内**，否则它的判据永远排在通道判据之后 ——
// 本该说"这个差异太大"，用户却会收到一句"超过中继通道容量"。
func TestRemoteOperationBudgetsStayUnderTheFrameLimit(t *testing.T) {
	// 云端读 Agent 帧的硬上限是 512 KiB，超了会掐断整条中继连接。这条预算必须远小于它。
	if relayFrameBudget >= 512<<10 {
		t.Fatalf("relayFrameBudget = %d must stay well under the 512 KiB agent read limit", relayFrameBudget)
	}
	server := &Server{}
	for name, operation := range server.remoteOperations() {
		if operation.MaxBytes <= 0 {
			continue
		}
		if operation.MaxBytes >= relayFrameBudget {
			t.Errorf("%s maxBytes = %d must be under the %d frame budget", name, operation.MaxBytes, relayFrameBudget)
		}
		// 超限文案要靠 Label 说清"什么太大"，没写就只能说"内容太大"。
		if strings.TrimSpace(operation.Label) == "" {
			t.Errorf("%s has a byte cap but no label for the oversize message", name)
		}
	}
}

// Path 里的占位符与 PathParams 必须严格互为对方：少一个会留下 `{oid}` 原样进 URL，
// 多一个则是声明了用不上的参数。
func TestRelayPathPlaceholdersMatchDeclaredParams(t *testing.T) {
	server := &Server{}
	for name, operation := range server.remoteOperations() {
		declared := map[string]bool{}
		for _, param := range operation.PathParams {
			declared[param] = true
			if !strings.Contains(operation.Path, "{"+param+"}") {
				t.Errorf("%s declares path param %q but Path %q has no such placeholder", name, param, operation.Path)
			}
			if _, ok := relayPathParamValidators[param]; !ok {
				t.Errorf("%s declares path param %q without a validator", name, param)
			}
		}
		for _, placeholder := range relayTestPlaceholders(operation.Path) {
			if !declared[placeholder] {
				t.Errorf("%s has placeholder {%s} in Path %q that is not declared in PathParams", name, placeholder, operation.Path)
			}
		}
	}
}

// ─── 路径参数：形状校验 ──────────────────────────────────────────────────────

func TestRelayPathParamShapeValidation(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("0f", 32)
	uuid := "3f2b1c4e-5a6d-4e8f-9b0c-1d2e3f4a5b6c"
	for _, testCase := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"oid", sha1, false},
		// 64 位是 SHA-256 仓库的对象 ID。handler 的 isFullGitObjectID 接受它，
		// 所以中继层也必须接受 —— 比 handler 更严会让个别仓库整片功能用不了，
		// 而那种差异不会让任何测试红。
		{"oid", sha256, false},
		{"oid", strings.Repeat("a", 39), true},
		{"oid", strings.Repeat("a", 41), true},
		{"oid", strings.ToUpper(sha1), true},
		{"oid", sha1 + "/../secret", true},
		{"oid", "", true},
		{"suggestionID", uuid, false},
		{"suggestionID", strings.ToUpper(uuid), false},
		{"suggestionID", "not-a-uuid", true},
		{"suggestionID", "", true},
		// 没登记校验器的名字必须**拒绝**：这是"新增路径参数却忘了写校验器"的直接信号。
		{"branchName", "main", true},
	} {
		err := validateRelayPathParam(testCase.name, testCase.value)
		if testCase.wantErr && err == nil {
			t.Errorf("validateRelayPathParam(%q, %q) = nil, want an error", testCase.name, testCase.value)
		}
		if !testCase.wantErr && err != nil {
			t.Errorf("validateRelayPathParam(%q, %q) = %v, want nil", testCase.name, testCase.value, err)
		}
	}
}

// ─── relayTarget：纯函数，整条链路上最容易出错的一步 ──────────────────────────

func gitOperation(t *testing.T, op string) remoteOperation {
	t.Helper()
	operation, ok := (&Server{}).remoteGitOperations()[op]
	if !ok {
		t.Fatalf("Git operation %q is missing", op)
	}
	return operation
}

func TestRelayTargetBuildsQueryCalls(t *testing.T) {
	target, body, pathParams, err := relayTarget(
		gitOperation(t, "git.diff"), "p1", "",
		json.RawMessage(`{"path":"src/main.go","stage":"worktree"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if !strings.HasPrefix(target, "/api/projects/p1/git/diff?") {
		t.Fatalf("target = %q", target)
	}
	// 查询参数经 url.Values.Encode()，键按字典序 —— 顺序确定，可以直接断言。
	if !strings.Contains(target, "path=src%2Fmain.go") || !strings.Contains(target, "stage=worktree") {
		t.Fatalf("target = %q, want the two query params percent-encoded", target)
	}
	if len(pathParams) != 0 {
		t.Fatalf("pathParams = %v, want none", pathParams)
	}
	_ = body
}

func TestRelayTargetSendsBodyForWriteCalls(t *testing.T) {
	payload := `{"paths":["a.go"],"stateToken":"tok"}`
	_, body, _, err := relayTarget(gitOperation(t, "git.stage"), "p1", "", json.RawMessage(payload))
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if string(body) != payload {
		t.Fatalf("body = %s, want the payload passed through verbatim", body)
	}
	// 没有 params 时也要给一个合法的空对象：某些 handler 会直接 decode。
	_, empty, _, err := relayTarget(gitOperation(t, "git.push"), "p1", "", nil)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if string(empty) != "{}" {
		t.Fatalf("empty body = %s, want {}", empty)
	}
}

// conversationId 决定**落在哪个工作区**（多会话有 worktree 隔离）。
// 它只能来自信封：params 里那一份必须被忽略。
func TestRelayTargetKeepsConversationIDFromEnvelopeOnly(t *testing.T) {
	target, _, _, err := relayTarget(
		gitOperation(t, "git.changes"), "p1", "conv-real",
		json.RawMessage(`{"conversationId":"conv-forged"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if strings.Contains(target, "conv-forged") {
		t.Fatalf("target = %q must not carry the conversationId from params", target)
	}
	if !strings.Contains(target, "conversationId=conv-real") {
		t.Fatalf("target = %q must carry the envelope conversationId", target)
	}

	// 写入类操作同样：它是查询参数，不进请求体。
	target, body, _, err := relayTarget(
		gitOperation(t, "git.stage"), "p1", "conv-real",
		json.RawMessage(`{"paths":["a.go"],"stateToken":"tok","conversationId":"conv-forged"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if !strings.Contains(target, "conversationId=conv-real") {
		t.Fatalf("target = %q must carry the envelope conversationId", target)
	}
	if strings.Contains(string(body), "conv-forged") {
		t.Fatalf("body = %s must not carry the forged conversationId", body)
	}
	// 剔掉那一个键不能顺手把别的键也弄丢 —— 那会让每一次 Git 写入都静默失败。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("body = %s is not a JSON object", body)
	}
	if _, ok := fields["paths"]; !ok {
		t.Fatalf("body = %s lost the paths field", body)
	}
	if _, ok := fields["stateToken"]; !ok {
		t.Fatalf("body = %s lost the stateToken field", body)
	}
	if len(fields) != 2 {
		t.Fatalf("body = %s must keep exactly the business fields", body)
	}
}

func TestRelayTargetRequiresAndSubstitutesPathParams(t *testing.T) {
	oid := strings.Repeat("ab", 20)

	target, _, pathParams, err := relayTarget(
		gitOperation(t, "git.commit"), "p1", "", json.RawMessage(`{"oid":"`+oid+`"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if !strings.Contains(target, "/git/commits/"+oid) {
		t.Fatalf("target = %q, want the oid substituted into the path", target)
	}
	// 路径参数还要交给 invokeLocalHandler 的 extraParams：合成请求没有真的走路由匹配，
	// chi.URLParam 读的就是那份。少了它，handler 拿到的是空串。
	if pathParams["oid"] != oid {
		t.Fatalf("pathParams = %v, want oid for the handler", pathParams)
	}

	// 缺了必填的路径参数要明确报错，而不是把 `{oid}` 原样发出去。
	if _, _, _, err := relayTarget(gitOperation(t, "git.commit"), "p1", "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a missing oid must be rejected")
	}
	// 非字符串同样拒绝。
	if _, _, _, err := relayTarget(gitOperation(t, "git.commit"), "p1", "", json.RawMessage(`{"oid":42}`)); err == nil {
		t.Fatal("a non-string oid must be rejected")
	}
	// 形状不合法在拼装阶段就拦下（不会发到本机 handler）。
	if _, _, _, err := relayTarget(gitOperation(t, "git.commit"), "p1", "", json.RawMessage(`{"oid":"../etc/passwd"}`)); err == nil {
		t.Fatal("a malformed oid must be rejected before building the request")
	}
}

// 即使校验器将来放宽，也不能有人把 `/` 或 `..` 拼进 URL 路径。
func TestRelayPathValuesAreEscaped(t *testing.T) {
	escaped := applyRelayPathValues("/git/commits/{oid}", []string{"oid"}, map[string]string{"oid": "a/b?c#d"})
	if strings.Contains(escaped, "a/b") || strings.Contains(escaped, "?c") {
		t.Fatalf("escaped = %q, want percent-encoding", escaped)
	}
}

// ─── 超限文案 ──────────────────────────────────────────────────────────────

// 这道闸门的文案必须与 Agent 侧通道帧判据那句**不同**：两条闸门若能挡住同一个用例，
// 那条测试就什么都没证明（docs/40 §6）。
func TestRelayOversizeMessageNamesTheContent(t *testing.T) {
	message := relayOversizeError(remoteOperation{Label: "改动差异"}, 320<<10).Error()
	if !strings.Contains(message, "改动差异") {
		t.Fatalf("message = %q, want it to name the content", message)
	}
	if !strings.Contains(message, "320.0 KiB") {
		t.Fatalf("message = %q, want a readable size", message)
	}
	if strings.Contains(message, "中继通道") {
		t.Fatalf("message = %q must differ from the channel frame gate wording", message)
	}
	// 没写 Label 时的兜底也必须是一句人话。
	if fallback := relayOversizeError(remoteOperation{}, 1024).Error(); !strings.Contains(fallback, "内容太大") {
		t.Fatalf("fallback = %q", fallback)
	}
}

func TestRelayHumanBytes(t *testing.T) {
	for _, testCase := range []struct {
		size int
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{320 << 10, "320.0 KiB"},
		{2 << 20, "2.0 MiB"},
	} {
		if got := relayHumanBytes(testCase.size); got != testCase.want {
			t.Errorf("relayHumanBytes(%d) = %q, want %q", testCase.size, got, testCase.want)
		}
	}
}

// ─── 小工具 ────────────────────────────────────────────────────────────────

func relayTestPlaceholders(path string) []string {
	names := []string{}
	for {
		open := strings.Index(path, "{")
		if open < 0 {
			return names
		}
		closeIndex := strings.Index(path[open:], "}")
		if closeIndex < 0 {
			return names
		}
		names = append(names, path[open+1:open+closeIndex])
		path = path[open+closeIndex+1:]
	}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
