package app

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestQoderSessionArgsPermissionModes 钉住三档权限到参数的映射。
//
// 这三条一旦写反，症状是"只读会话真的改了文件"或"工作区写入被静默拒绝、任务空转"——
// 两种都不会报错。
func TestQoderSessionArgsPermissionModes(t *testing.T) {
	cases := []struct{ mode, want string }{
		{"read_only", "default"},
		{"workspace_write", "accept_edits"},
		{"full_control", "bypass_permissions"},
		{"", "accept_edits"},
	}
	for _, tc := range cases {
		args := qoderSessionArgs(AgentSessionRequest{PermissionMode: tc.mode})
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-p --input-format stream-json --output-format stream-json") {
			t.Fatalf("%s: 缺少流式协议参数：%s", tc.mode, joined)
		}
		if !strings.Contains(joined, "--permission-mode "+tc.want) {
			t.Fatalf("%q 应映射到 --permission-mode %s，实际：%s", tc.mode, tc.want, joined)
		}
	}
}

// TestQoderSessionArgsCarryModelResumeAndMCP 钉住"不传就等于静默失效"的四项。
func TestQoderSessionArgsCarryModelResumeAndMCP(t *testing.T) {
	args := qoderSessionArgs(AgentSessionRequest{
		PermissionMode: "workspace_write", Model: "performance",
		Resume: true, SessionID: "sess-9", MCPConfigPath: "/tmp/mcp.json", StrictMCP: true,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--model performance",
		"--resume sess-9",
		"--mcp-config /tmp/mcp.json",
		"--strict-mcp-config",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %q：%s", want, joined)
		}
	}
	// 首轮（不续聊）不带 --resume：带上会让 CLI 去找一个还不存在的会话。
	first := strings.Join(qoderSessionArgs(AgentSessionRequest{PermissionMode: "workspace_write"}), " ")
	if strings.Contains(first, "--resume") {
		t.Fatalf("首轮不应带 --resume：%s", first)
	}
}

// TestQoderAuthState 钉住 `qoder status` 的三态解析。
//
// 两段输入都是**真机原文**（1.1.75 / 1.1.77，Windows）。关键是登录之后输出形状会变：
// 没有 Account 行了，改成 Username / Email / User Type。只认 Account 的解析会把登录后
// 的状态读成"未登录"，用户在终端明明登录成功、页面却一直说未登录。
//
// ⚠️ 第三档（输出形状陌生）必须是 unreadable，**不能**是未登录：那一档的界面会给
// "去登录"，对着一台已经登录的机器说这句话，就是这一轮修的那个毛病。
func TestQoderAuthState(t *testing.T) {
	loggedOut := "Version: 1.1.75\nAccount: Not logged in\n"
	if state := qoderAuthState(loggedOut); state != agentAuthLoggedOut {
		t.Fatalf("Account: Not logged in 应读成未登录，得到 %q", state)
	}
	loggedIn := "Version: 1.1.77\n" +
		"Username: kegehe\n" +
		"Email: 1225959324@qq.com\n" +
		"Avatar: https://qoder.com/users/x/default/avatars\n" +
		"User Type: personal_professional_trial\n" +
		"Login Method: browser\n" +
		"Auth Source: local\n"
	if state := qoderAuthState(loggedIn); state != agentAuthLoggedIn {
		t.Fatalf("登录后的真实输出（Username/Email/...）应读成已登录，得到 %q", state)
	}
	for _, unknown := range []string{"", "Version: 1.1.75", "Version: 1.1.75\nAccount:\n"} {
		if state := qoderAuthState(unknown); state != agentAuthUnreadable {
			t.Fatalf("形状陌生（%q）时只能报 unreadable，得到 %q", unknown, state)
		}
	}
}

// TestQoderTurnErrorIsChineseFailureText 钉住未登录的翻译。
//
// 除了"翻译成中文"，还钉住它**必须含"失败"二字**：localizedErrorText 的直通判据是
// "含中文 且（含失败 或 没有残留英文）"，而这句话里必然带 qoder/login 这类英文词。
func TestQoderTurnErrorIsChineseFailureText(t *testing.T) {
	err := qoderTurnError("Not logged in · Please run /login", "authentication_failed")
	if err == nil || !containsChinese(err.Error()) || !strings.Contains(err.Error(), "失败") {
		t.Fatalf("未登录文案必须可直接展示（中文 + 失败）：%v", err)
	}
	plain := qoderTurnError("model overloaded", "")
	if plain == nil || !strings.Contains(plain.Error(), "model overloaded") {
		t.Fatalf("其它错误应保留原文：%v", plain)
	}
}

// TestQoderEntryIsRunnableWithThreeModes 是目录与 runner 的对齐断言：
// 目录声明的权限面必须都能被 qoderSessionArgs 表达。
func TestQoderEntryIsRunnableWithThreeModes(t *testing.T) {
	entry, ok := agentByID("qoder")
	if !ok {
		t.Fatal("目录里没有 qoder")
	}
	if !entry.RunnableInProject || entry.InstallKind != InstallKindNpmGlobal {
		t.Fatalf("qoder 目录项不可运行或分发方式不对：%+v", entry)
	}
	for _, mode := range entry.PermissionModes {
		if mode == "approval_required" {
			t.Fatal("headless -p 没有审批通道，目录不该声明 approval_required")
		}
		args := strings.Join(qoderSessionArgs(AgentSessionRequest{PermissionMode: mode}), " ")
		if !strings.Contains(args, "--permission-mode") {
			t.Fatalf("%s 没有对应参数：%s", mode, args)
		}
	}
}

// qoderTestBinary 只在**显式**给出 QODER_TEST_BIN 时才启用活体用例。
//
// 与 CodeBuddy 的自动探测不同：Qoder 的 `-p` 一跑就是一次真实模型请求（未登录才免单），
// 在 `go test ./...` 里顺手探测会让每次跑测试都消耗额度。
func qoderTestBinary() string {
	path := os.Getenv("QODER_TEST_BIN")
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// TestQoderLiveVersionAndStatus 是活体校准：能报版本、能读登录态。
func TestQoderLiveVersionAndStatus(t *testing.T) {
	bin := qoderTestBinary()
	if bin == "" {
		t.Skip("未设置 QODER_TEST_BIN，跳过 Qoder 活体校准")
	}
	runner := &qoderCLIRunner{paths: &agentPathResolver{override: map[string]string{"qoder": bin}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !runner.Ready(ctx) {
		t.Fatal("Ready 认为不可用")
	}
	if version := runner.Version(ctx); version == "" {
		t.Fatal("读不到版本")
	}
	// 登录态只断言"能读出结论"：登录与否取决于这台机器，测试不替它下结论。
	_, _ = runner.LoginStatus(ctx)
}

// TestQoderLiveStreamJSONHandshake 是活体校准：真的拉起 `-p --input-format stream-json`，
// 确认 init 帧与终态回调都会到达（未登录时终态是中文"未登录"错误）。
func TestQoderLiveStreamJSONHandshake(t *testing.T) {
	bin := qoderTestBinary()
	if bin == "" {
		t.Skip("未设置 QODER_TEST_BIN，跳过 Qoder 活体校准")
	}
	bin, err := exec.LookPath(bin)
	if err != nil {
		bin = os.Getenv("QODER_TEST_BIN")
	}
	runner := &qoderCLIRunner{paths: &agentPathResolver{override: map[string]string{"qoder": bin}}}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	session, err := runner.StartSession(ctx, AgentSessionRequest{ProjectPath: t.TempDir(), PermissionMode: "read_only"})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// 显式 Stop + 等 Done：只靠 defer Stop 时，Windows 上 node 子进程释放 cwd 比
	// TempDir 清理慢一步，会让用例以"目录被占用"失败——一个与被测行为无关的假阴性。
	defer func() {
		session.Stop()
		select {
		case <-session.Done():
		case <-time.After(10 * time.Second):
		}
	}()
	sink := newRecordingTurnSink()
	if err := session.Send(AgentRunRequest{Prompt: "reply with the single word ok", AgentID: "qoder"}, sink); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-sink.finished:
	case <-time.After(110 * time.Second):
		t.Fatal("未收到回合终态")
	}
	if !sink.initialized || sink.sessionID == "" {
		t.Fatalf("init 帧没有带出 session_id：%q initialized=%v", sink.sessionID, sink.initialized)
	}
}

// TestQoderProfileIsRejectedBeforeSQL 钉住"报一句人话，而不是撞 SQLite 约束"。
//
// 档案表的 agent_id CHECK 目前只允许 claude-code / codex；目录里却有 qoder。
// 不提前拒绝的话，用户点保存拿到的是 CHECK constraint failed（英文、指不到下一步）。
// 等约束放开时，这条用例会先红 —— 那时应当改成"可以保存"，而不是删掉了事。
func TestQoderProfileIsRejectedBeforeSQL(t *testing.T) {
	err := validateManagedProfileInput("qoder", "qoder", "", "", "cli_managed")
	if err == nil || !strings.Contains(err.Error(), "Qoder") {
		t.Fatalf("Qoder 档案应被明确拒绝，实际 err=%v", err)
	}
}

// TestQoderLoginURLTrimsTrailingHint 钉住一条**只在真机上出现**的格式：
// CLI 把授权链接和 "Waiting for browser authorization..." 打在同一行、中间没有空格，
// 通用提取会把 "Waiting" 粘进链接，页面上的链接就点不开了。
func TestQoderLoginURLTrimsTrailingHint(t *testing.T) {
	line := "Please open the following URL in your browser to sign in:  https://qoder.com/device/selectAccounts?challenge=ABC&machine_token=XYZWaiting for browser authorization..."
	got := qoderLoginURL(line)
	want := "https://qoder.com/device/selectAccounts?challenge=ABC&machine_token=XYZ"
	if got != want {
		t.Fatalf("qoderLoginURL = %q, want %q", got, want)
	}
	if strings.Contains(got, "Waiting") {
		t.Fatal("链接里粘上了提示词")
	}
	if qoderLoginURL("Starting browser login...") != "" {
		t.Fatal("没有链接的行不应返回内容")
	}
}

// TestQoderLiveLoginEmitsAuthURL 是登录流程的活体校准：真的拉起 `qoder login`，
// 确认能拿到授权链接、且进程被 runner 持有（而不是随请求结束被杀）。
//
// 用 CI=1 让 CLI 走"无图形界面"分支：同样会打印链接，但**不会真的弹出浏览器**，
// 这样跑测试不会打断操作这台机器的人。用例结束会主动结束登录进程。
func TestQoderLiveLoginEmitsAuthURL(t *testing.T) {
	bin := qoderTestBinary()
	if bin == "" {
		t.Skip("未设置 QODER_TEST_BIN，跳过 Qoder 活体校准")
	}
	t.Setenv("CI", "1")
	runner := &qoderCLIRunner{paths: &agentPathResolver{override: map[string]string{"qoder": bin}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := runner.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if info.AuthURL == "" {
		t.Fatalf("没有拿到授权链接，message=%q", info.Message)
	}
	if !strings.HasPrefix(info.AuthURL, "https://qoder.com/") {
		t.Fatalf("授权链接不对：%q", info.AuthURL)
	}
	if strings.Contains(info.AuthURL, "Waiting") {
		t.Fatalf("授权链接被提示词污染：%q", info.AuthURL)
	}
	runner.loginMu.Lock()
	running := runner.loginCmd
	runner.loginMu.Unlock()
	if running == nil || running.Process == nil {
		t.Fatal("登录进程没有被 runner 持有（请求结束就会丢）")
	}
	_ = running.Process.Kill()
}
