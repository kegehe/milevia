package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type claudeTurnTestSink struct {
	mu       sync.Mutex
	finished []error
}

type claudeOutputTestSink struct {
	events []json.RawMessage
	texts  []string
}

func (sink *claudeOutputTestSink) Event(_ string, payload json.RawMessage) {
	sink.events = append(sink.events, append(json.RawMessage(nil), payload...))
}
func (sink *claudeOutputTestSink) AssistantText(text, _ string) {
	sink.texts = append(sink.texts, text)
}
func (*claudeOutputTestSink) SessionIdentified(string) {}
func (*claudeOutputTestSink) SessionInitialized()      {}

func TestIndependentReviewClaudeRequestUsesExecutableReadOnlyMode(t *testing.T) {
	tools := orchestrationReviewReadOnlyTools("claude-code")
	if len(tools) == 0 {
		t.Fatal("Claude independent review must restrict tools")
	}
	runner := &claudeCLIRunner{config: Config{PermissionMode: "acceptEdits"}}
	args, err := runner.args(AgentRunRequest{
		SessionID:      "review-session",
		Prompt:         "review prompt",
		PermissionMode: "read_only",
		SkipSessionID:  true,
		ReadOnlyTools:  tools,
		PromptViaStdin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--permission-mode default") || !strings.Contains(joined, "--allowedTools") {
		t.Fatalf("independent review args do not use executable read-only mode: %q", args)
	}
	if strings.Contains(joined, "--permission-mode plan") || strings.Contains(joined, "--session-id") || strings.Contains(joined, "review-session") || strings.Contains(joined, "review prompt") {
		t.Fatalf("independent review args still use waiting/session mode: %q", args)
	}
}

func TestClaudeArgsPassesStructuredOutputSchema(t *testing.T) {
	runner := &claudeCLIRunner{}
	schema := json.RawMessage(`{"type":"array","items":{"type":"string"}}`)
	args, err := runner.args(AgentRunRequest{
		Prompt:         "return json",
		PermissionMode: "read_only",
		SkipSessionID:  true,
		OutputSchema:   schema,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArguments(args, "--json-schema", string(schema)) {
		t.Fatalf("structured output schema missing from args: %q", args)
	}
	if _, err := runner.args(AgentRunRequest{Prompt: "return json", PermissionMode: "read_only", SkipSessionID: true, OutputSchema: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid output schema should fail before Claude starts")
	}
}

func TestClaudeOutputRedactsCredentialsBeforeEmitting(t *testing.T) {
	const secret = "sk-claude-test-secret-value-12345"
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	runner.readOutput(strings.NewReader(`{"type":"assistant","api_key":"`+secret+`","message":{"content":[{"type":"text","text":"Authorization: Bearer `+secret+`"}]}}`+"\n"), sink)
	runner.readStderr(strings.NewReader("OPENAI_API_KEY="+secret+"\n"), sink)
	for _, payload := range sink.events {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("credential leaked in event: %s", payload)
		}
	}
	for _, text := range sink.texts {
		if strings.Contains(text, secret) {
			t.Fatalf("credential leaked in assistant text: %s", text)
		}
	}
	if len(sink.texts) != 1 || !strings.Contains(sink.texts[0], "[REDACTED]") {
		t.Fatalf("assistant text was not redacted: %#v", sink.texts)
	}
}

func TestClaudeReadOutputEmitsStructuredResult(t *testing.T) {
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	runner.readOutput(strings.NewReader(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[]}}`+"\n"), sink)
	if len(sink.texts) != 1 || sink.texts[0] != `{"findings":[]}` {
		t.Fatalf("structured result was not forwarded as assistant text: %#v", sink.texts)
	}
}

func TestClaudeStderrCaptureBoundsAndDetail(t *testing.T) {
	capture := &stderrCapture{}
	// 超过行数上限：只保留最近 maxStderrCaptureLines 行。
	for i := 0; i < 20; i++ {
		capture.append(fmt.Sprintf("line-%02d", i))
	}
	got := capture.tail()
	for _, keep := range []string{"line-19", "line-08"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("capture tail %q missing %q", got, keep)
		}
	}
	if strings.Contains(got, "line-00") || strings.Contains(got, "line-06") {
		t.Fatalf("capture tail %q should have dropped oldest lines", got)
	}

	// wsl.exe 的无害主机侧警告（代理/NAT 提示）不应污染；但 wsl.exe 启动命令失败的
	// 真实报错（同样以 "wsl: " 开头）必须保留，包括只提 NAT 的真实网络错误。
	capture.append("wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。")
	capture.append("wsl: NAT 网络连接失败")
	capture.append("wsl: 找不到发行版 BadDistro")
	capture.append("Error: rate limit exceeded")
	if strings.Contains(capture.tail(), "代理") {
		t.Fatalf("wsl host warning leaked into capture: %q", capture.tail())
	}
	if !strings.Contains(capture.tail(), "NAT 网络连接失败") {
		t.Fatalf("real wsl NAT error should not be filtered: %q", capture.tail())
	}
	if !strings.Contains(capture.tail(), "找不到发行版") {
		t.Fatalf("real wsl distro error should not be filtered: %q", capture.tail())
	}
	if !strings.Contains(capture.tail(), "rate limit") {
		t.Fatalf("claude error missing after wsl warning filter: %q", capture.tail())
	}

	// 超长单行：append 先截断到总字节上限，tail 保留末尾且不超限。
	long := &stderrCapture{}
	long.append(strings.Repeat("x", maxStderrCaptureBytes+100))
	if len(long.tail()) > maxStderrCaptureBytes {
		t.Fatalf("capture did not bound bytes: %d", len(long.tail()))
	}
	if long.tail() == "" {
		t.Fatalf("oversized line should not be dropped entirely")
	}
	if !strings.HasSuffix(long.tail(), "xxxxx") {
		t.Fatalf("oversized line should keep the tail end")
	}

	// claudeStderrDetail：脱敏 + 去 ANSI + 有界保留末尾。
	ansi := "\x1b[31mError: rate limit \x1b[0mwith key sk-test-secret-abcdef123"
	detail := claudeStderrDetail(ansi)
	if strings.Contains(detail, "\x1b") {
		t.Fatalf("detail leaked ANSI escapes: %q", detail)
	}
	if strings.Contains(detail, "sk-test-secret-abcdef123") {
		t.Fatalf("detail leaked secret: %q", detail)
	}
	if !strings.Contains(detail, "rate limit") {
		t.Fatalf("detail missing reason: %q", detail)
	}
	if !strings.HasPrefix(detail, "（stderr：") {
		t.Fatalf("detail missing prefix: %q", detail)
	}
	if claudeStderrDetail("   ") != "" {
		t.Fatalf("empty/whitespace stderr should produce empty detail")
	}
	// 构造贴近上限的 detail（maxAgentDetailBytes=180 字节**内容**，括号另计），验证加上固定前缀后
	// 仍落在 insightRunErrorMessage 的 insightRunMessageBytes=240 **字节**截断（保留开头）之内，
	// 不砍关键末尾。（单位别写成"字符"：中文一字三字节，这条预算按字节算。）
	maxDetail := claudeStderrDetail(strings.Repeat("x", 500))
	assertClaudeExitMessageFitsInsightTruncation(t, maxDetail)
	if !strings.HasSuffix(maxDetail, "xxx）") {
		t.Fatalf("max-length detail should keep the tail end: %q", maxDetail)
	}
}

// assertClaudeExitMessageFitsInsightTruncation 钉住"进程退出错误 + 详情段"的字节预算。
//
// insightRunErrorMessage 会把这串东西按 insightRunMessageBytes 截断（且保留开头、切掉
// 末尾），一旦超了，被切掉的恰好是最有用的末尾原因。所以整个形态要一起算：固定前缀
// （claudeExitPrefix）+ Go 的退出描述 + 详情段。
//
// 这里用真实的前缀与真实的上限来算，不抄一份字面量——抄字面量的话，前缀改成中文以后
// 这条断言会**照旧通过**，但它量的已经不是真正发出去的那个串了。
//
// ⚠️ 退出描述取 **Windows 的十位崩溃码**（`exit status 3221225477`，23 字节含尾空格），
// 不是 POSIX 的 "exit status 137 "（16 字节）。原来按后者算，得出"还留了余量"的结论，
// 而真实的长尾（Windows 上进程崩溃）是 22 + 23 + 195 = 恰好 240 —— 余量为 0。
// 按最坏形态钉，这条断言才有意义。
const longestGoExitDescription = "exit status 3221225477 " // Windows STATUS_ACCESS_VIOLATION 一类

func assertClaudeExitMessageFitsInsightTruncation(t *testing.T, detail string) {
	t.Helper()
	if total := len(claudeExitPrefix) + len(longestGoExitDescription) + len(detail); total > insightRunMessageBytes {
		t.Fatalf("进程退出错误最长形态 %d 字节，超出 %d 字节截断上限（详情段 %d 字节，其内层 tail 另有 maxAgentDetailBytes=%d 的帽子）",
			total, insightRunMessageBytes, len(detail), maxAgentDetailBytes)
	}
}

// 详情段的额度是**内容**字节数，括号另计 —— 这条钉的就是这个口径。
//
// 2026-09-29 那轮修复里，我一度把 `claudeRunFailureDetailWithin` 的入参当成"整段（含括号）"，
// 于是本地路径的 CLI 分支从 192 悄悄缩到 180：没有任何断言会红，因为差的只是"少给 12 字节
// 报错原文"。括号是"（CLI：…）"9+3 还是"（stderr：…）"12+3 由运行结果决定，调用方事先不知道，
// 所以反推额度时扣的是**大的那个**（claudeDetailWrapperBytes）。
func TestClaudeFailureDetailContentCapExcludesWrapper(t *testing.T) {
	long := strings.Repeat("远端报错原文", 100) // 远超上限，逼出截断

	// stderr 分支：内容上限 maxAgentDetailBytes，括号 15 字节。
	stderrDetail := claudeRunFailureDetail(nil, long)
	stderrContent := strings.TrimSuffix(strings.TrimPrefix(stderrDetail, "（stderr："), "）")
	if len(stderrContent) > maxAgentDetailBytes {
		t.Fatalf("stderr 详情段内容 %d 字节，超出 maxAgentDetailBytes=%d：括号被算进内容额度了",
			len(stderrContent), maxAgentDetailBytes)
	}
	// 允许一个多字节字符的余量（截断点会回退到 UTF-8 边界），但不能少太多 ——
	// 少一整段就说明额度算小了，那正是这次要防的退化。
	if len(stderrContent) < maxAgentDetailBytes-3 {
		t.Fatalf("stderr 详情段只给了 %d 字节（上限 %d）—— 额度被谁又扣了一道",
			len(stderrContent), maxAgentDetailBytes)
	}

	// CLI 分支：同一份内容额度，括号 12 字节。
	cliDetail := claudeRunFailureDetail(json.RawMessage(`{"type":"result","is_error":true,"result":`+strconv.Quote(long)+`}`), "")
	cliContent := strings.TrimSuffix(strings.TrimPrefix(cliDetail, "（CLI："), "）")
	if !strings.HasPrefix(cliDetail, "（CLI：") {
		t.Fatalf("CLI 自报的可读原因没被用上：%q", cliDetail[:min(len(cliDetail), 40)])
	}
	if len(cliContent) > maxAgentDetailBytes || len(cliContent) < maxAgentDetailBytes-3 {
		t.Fatalf("CLI 详情段内容 %d 字节（上限 %d）—— 与 stderr 分支的口径不一致",
			len(cliContent), maxAgentDetailBytes)
	}

	// 反推额度时必须扣**较大**的括号：同一段内容走哪个分支由运行结果决定。
	if claudeDetailWrapperBytes < len("（CLI：）") || claudeDetailWrapperBytes < len("（stderr：）") {
		t.Fatalf("claudeDetailWrapperBytes=%d 不是较大的那个括号长度，反推额度会算少",
			claudeDetailWrapperBytes)
	}
}

// claudeCLIErrorCodes 的**不变量**：每个译文都必须含"失败"二字。
//
// 理由见 claudeCLIErrorCodeText 的注释：括号里保留着英文原值，所以直通判据要求
// "含中文 且（含失败 或 无残留英文）"，少这两个字就会被套上"任务执行失败，请查看任务
// 日志后重试。"——一句既指不到原因、又建议重试一件重试必然同样失败的事。
//
// 2026-09-29：`error_max_turns` 正是漏网的第三个值（独立复查实测 errorText 的输出）。
// 这条断言的作用是让**下一个**新增的枚举值也逃不掉。
func TestClaudeCLIErrorCodeTextsAllCarryFailureWord(t *testing.T) {
	if len(claudeCLIErrorCodes) < 3 {
		t.Fatalf("枚举表只剩 %d 条，锚点可能已失配（这条断言会退化成空转）", len(claudeCLIErrorCodes))
	}
	for code, text := range claudeCLIErrorCodes {
		if !strings.Contains(text, "失败") {
			t.Errorf("枚举 %s 的译文 %q 里没有「失败」：它会被套上误导前缀", code, text)
		}
		// 端到端再钉一次：真正上屏的那条串不许带兜底前缀。
		got := errorText(errors.New(mapClaudeAPIError(code)))
		if strings.Contains(got, taskFailureFallbackPrefix) {
			t.Errorf("枚举 %s 的错误被套上了兜底前缀：%q", code, got)
		}
		if !containsChinese(got) {
			t.Errorf("枚举 %s 的错误没有中文：%q", code, got)
		}
	}
}

func TestClaudeReadStderrCaptureAccumulatesTail(t *testing.T) {
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	capture := &stderrCapture{}
	runner.readStderrCapture(strings.NewReader("first warning\nreal error: context length exceeded\n"), sink, capture)
	if !strings.Contains(capture.tail(), "real error: context length exceeded") {
		t.Fatalf("capture missing claude error line: %q", capture.tail())
	}
	// 空捕获（nil capture）行为不变：仅事件，不 panic。
	runner.readStderr(strings.NewReader("noise\n"), sink)
	if capture.tail() == "noise" {
		t.Fatalf("nil-capture readStderr should not write into existing capture")
	}
}

// TestClaudeCLIRunErrorIncludesStderrDetail 端到端验证真实失败场景：fake claude 脚本
// 写一行 stderr 后以退出码 1 结束，Run 返回的错误应包含 claude 自己写的 stderr 尾部，
// 而非只有裸的退出描述（claudeExitPrefix + "exit status 1"）。
func TestClaudeCLIRunErrorIncludesStderrDetail(t *testing.T) {
	requirePOSIXShell(t)
	scriptPath := filepath.Join(t.TempDir(), "fake-claude-fail")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nprintf '%s\\n' 'Error: rate limit exceeded' >&2\nexit 1\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	runner := claudeCLIRunner{config: Config{ClaudePath: scriptPath}}
	err := runner.Run(context.Background(), AgentRunRequest{
		SessionID:      "00000000-0000-4000-8000-000000000000",
		ProjectPath:    t.TempDir(),
		Prompt:         "test",
		PermissionMode: "full_control",
	}, &recordingSink{})
	if err == nil {
		t.Fatal("expected error from failing fake Claude CLI")
	}
	if !strings.Contains(err.Error(), claudeExitPrefix) {
		t.Fatalf("error missing exit-wrapper prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Fatalf("error missing stderr detail: %v", err)
	}
}

// TestClaudeRunFailureDetailPicksTheMostUsefulReason 单测失败原因的选择规则：API 级失败
// 只在 stream-json 的 result 事件里报出，必须能从 is_error/api_error_status/result 里
// 还原成人能看懂的一句话；CLI 只给枚举时，stderr 比裸枚举更有信息量，而 success /
// completed 这类无害枚举绝不能当失败原因展示。
func TestClaudeRunFailureDetailPicksTheMostUsefulReason(t *testing.T) {
	apiError := json.RawMessage(
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"terminal_reason":"api_error",` +
			`"result":"API Error: 400 Upstream service returned HTTP 400: {\"model\":\"x\"}"}`)
	detail := claudeRunFailureDetail(apiError, "")
	if !strings.HasPrefix(detail, "（CLI：") {
		t.Fatalf("detail missing prefix: %q", detail)
	}
	if !strings.Contains(detail, "API Error: 400") {
		t.Fatalf("detail missing API error text: %q", detail)
	}

	// 非失败（is_error 缺失或为假）不得产出附加段，否则正常收尾也会被拼进错误。
	if got := claudeRunFailureDetail(json.RawMessage(`{"type":"result","is_error":false,"result":"正常回复"}`), ""); got != "" {
		t.Fatalf("non-error result should produce empty detail, got %q", got)
	}
	if got := claudeRunFailureDetail(json.RawMessage(`{"type":"result","result":"缺少 is_error"}`), ""); got != "" {
		t.Fatalf("result without is_error should produce empty detail, got %q", got)
	}
	if got := claudeRunFailureDetail(nil, ""); got != "" {
		t.Fatalf("no result event should produce empty detail, got %q", got)
	}

	// CLI 没给可读原因（result 为空）时，退回 stderr——它比裸枚举更有信息量。
	enumOnly := json.RawMessage(`{"type":"result","is_error":true,"terminal_reason":"error_max_turns"}`)
	if got := claudeRunFailureDetail(enumOnly, "real crash: context length exceeded"); !strings.Contains(got, "（stderr：") {
		t.Fatalf("stderr should outrank a bare enum, got %q", got)
	}
	// stderr 也没有时才用裸枚举，并补上 HTTP 状态码。
	fallback := claudeRunFailureDetail(json.RawMessage(
		`{"type":"result","is_error":true,"api_error_status":429,"terminal_reason":"api_error"}`), "")
	if !strings.Contains(fallback, "HTTP 429") || !strings.Contains(fallback, "api_error") {
		t.Fatalf("fallback detail missing status/reason: %q", fallback)
	}

	// 无害枚举（实测 API 400 那次的 subtype 正是 "success"；completed 是常态）必须跳过，
	// 不能给用户一句"（CLI：completed）"当作失败原因。
	for _, noise := range []string{
		`{"type":"result","is_error":true,"subtype":"success","terminal_reason":"completed","result":""}`,
		`{"type":"result","is_error":true,"subtype":"completed"}`,
	} {
		if got := claudeRunFailureDetail(json.RawMessage(noise), ""); got != "" {
			t.Fatalf("noise-only enums should yield no detail, got %q for %s", got, noise)
		}
	}
	// 没有可读原因、stderr 也无内容时，非噪声枚举仍要透出（否则用户什么都看不到）。
	if got := claudeRunFailureDetail(json.RawMessage(`{"type":"result","is_error":true,"subtype":"error_during_execution"}`), "  "); !strings.Contains(got, "error_during_execution") {
		t.Fatalf("meaningful enum should still surface, got %q", got)
	}

	// 脱敏 + ANSI 剥离 + 有界：即使塞入超长带密钥的正文也不能撑爆 240 **字节**上限。
	long := claudeRunFailureDetail(json.RawMessage(
		`{"type":"result","is_error":true,"result":"\u001b[31mkey sk-abcdef1234567890 `+strings.Repeat("x", 900)+`"}`), "")
	if strings.Contains(long, "sk-abcdef1234567890") {
		t.Fatalf("detail leaked secret: %q", long)
	}
	if strings.Contains(long, "\x1b") {
		t.Fatalf("detail leaked ANSI escapes: %q", long)
	}
	assertClaudeExitMessageFitsInsightTruncation(t, long)
}

// TestClaudeCLIRunErrorIncludesResultEventDetail 端到端验证真实失败场景：fake claude
// 在 stdout 的 result 事件里报 API 错误、stderr 一个字都没有、退出码 1——Run 返回的
// 错误必须带上 CLI 自报的原因，而不是裸的退出描述（claudeExitPrefix + "exit status 1"）。
func TestClaudeCLIRunErrorIncludesResultEventDetail(t *testing.T) {
	requirePOSIXShell(t)
	scriptPath := filepath.Join(t.TempDir(), "fake-claude-api-error")
	result := `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,` +
		`"terminal_reason":"api_error","result":"API Error: 400 Upstream service returned HTTP 400"}`
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\n" +
		"printf '%s\\n' '" + result + "'\n" +
		"printf '%s\\n' 'unrelated stderr warning' >&2\nexit 1\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	runner := claudeCLIRunner{config: Config{ClaudePath: scriptPath}}
	err := runner.Run(context.Background(), AgentRunRequest{
		SessionID:      "00000000-0000-4000-8000-000000000000",
		ProjectPath:    t.TempDir(),
		Prompt:         "test",
		PermissionMode: "full_control",
	}, &recordingSink{})
	if err == nil {
		t.Fatal("expected error from failing fake Claude CLI")
	}
	if !strings.Contains(err.Error(), claudeExitPrefix) {
		t.Fatalf("error missing exit-wrapper prefix: %v", err)
	}
	// CLI 自报的原因优先于 stderr（API 失败时 stderr 通常只是无关噪声）。
	if !strings.Contains(err.Error(), "API Error: 400") {
		t.Fatalf("error missing CLI-reported API error: %v", err)
	}
	if strings.Contains(err.Error(), "unrelated stderr warning") {
		t.Fatalf("stderr should not shadow the CLI-reported reason: %v", err)
	}
}

// 包装层必须对调用方 sink 的**可选能力**透明：readOutput 会对 sink 做
// assistantMessageIDSetter / assistantDeltaSink 断言，断言失败即静默跳过——包装层若不
// 转发，"增量输出 / 采纳 assistant message id"这类能力就被无声吞掉。今天只有长驻会话用
// 得上它们，但一次性路径不该埋这种雷，所以把不变量钉在测试里。
func TestClaudeResultErrorSinkForwardsOptionalSinkCapabilities(t *testing.T) {
	runner := &claudeCLIRunner{}
	stream := `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1"}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"增量"}}}` + "\n"

	opted := &partialSink{}
	runner.readOutput(strings.NewReader(stream), &claudeResultErrorSink{AgentRunSink: opted})
	if opted.messageID != "msg_1" {
		t.Fatalf("wrapped sink dropped assistant message id: %q", opted.messageID)
	}
	if len(opted.deltas) != 1 || opted.deltas[0] != "增量" {
		t.Fatalf("wrapped sink dropped assistant deltas: %q", opted.deltas)
	}

	// 没实现这两个能力的内层 sink（编排审查、SSH 回合等）不得因此报错或凭空产生事件。
	plain := &plainSink{}
	runner.readOutput(strings.NewReader(stream), &claudeResultErrorSink{AgentRunSink: plain})
	if len(plain.events) != 0 {
		t.Fatalf("stream_event envelopes must not become events: %q", plain.events)
	}
}

// writeFakeClaudeExecutable 写一个假 claude 可执行文件：POSIX 下是带 shebang 的脚本，
// Windows 下是 .cmd（由 Go 经 cmd.exe 执行）。既有夹具用 requirePOSIXShell 在 Windows 上
// 跳过，但"失败时把 CLI 自报原因拼进错误"这件事恰恰只在 Windows 上复现过——跳过就等于
// 在本机没有任何端到端证据，所以这里给两个平台各写一份。
func writeFakeClaudeExecutable(t *testing.T, posixBody, windowsBody string) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "fake-claude.cmd")
		if err := os.WriteFile(path, []byte(windowsBody), 0o700); err != nil {
			t.Fatalf("write fake Claude CLI: %v", err)
		}
		return path
	}
	path := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+posixBody), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	return path
}

// TestClaudeCLIRunSurfacesSelfReportedFailure 端到端（Windows 也真跑）：fake claude 在
// stdout 的 result 事件里自报 API 失败、stderr 一个字节都没有、退出码 1——Run 返回的错误
// 必须带上 CLI 自报的原因，而不是裸的退出描述（claudeExitPrefix + "exit status 1"）。
func TestClaudeCLIRunSurfacesSelfReportedFailure(t *testing.T) {
	const initLine = `{"type":"system","subtype":"init"}`
	const resultLine = `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,` +
		`"terminal_reason":"api_error","result":"API Error: 400 Upstream service returned HTTP 400"}`
	scriptPath := writeFakeClaudeExecutable(t,
		"printf '%s\\n' '"+initLine+"'\nprintf '%s\\n' '"+resultLine+"'\nexit 1\n",
		"@echo off\r\necho "+initLine+"\r\necho "+resultLine+"\r\nexit /b 1\r\n")

	runner := claudeCLIRunner{config: Config{ClaudePath: scriptPath}}
	err := runner.Run(context.Background(), AgentRunRequest{
		SessionID:      "00000000-0000-4000-8000-000000000000",
		ProjectPath:    t.TempDir(),
		Prompt:         "test",
		PermissionMode: "full_control",
	}, &recordingSink{})
	if err == nil {
		t.Fatal("expected error from failing fake Claude CLI")
	}
	if !strings.Contains(err.Error(), claudeExitPrefix) {
		t.Fatalf("error missing exit-wrapper prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "API Error: 400") {
		t.Fatalf("error missing CLI-reported reason: %v", err)
	}
	// stderr 本来就是空的：绝不能凭空多出一段"（stderr：…）"。
	if strings.Contains(err.Error(), "（stderr：") {
		t.Fatalf("empty stderr must not be reported: %v", err)
	}
}

// TestClaudeResultErrorSinkRecordsReadOutputFailure 不依赖子进程夹具（Windows 也能跑）：
// readOutput 解析 stdout 事件流时，result 事件里的失败原因应被包装的 sink 记下（供
// Run 拼进错误信息），且事件仍照常转发给调用方的 sink——包装对调用方透明。
func TestClaudeResultErrorSinkRecordsReadOutputFailure(t *testing.T) {
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	recorded := &claudeResultErrorSink{AgentRunSink: sink}
	stream := `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":400,` +
		`"terminal_reason":"api_error","result":"API Error: 400 upstream rejected the request"}` + "\n"
	runner.readOutput(strings.NewReader(stream), recorded)

	if detail := claudeRunFailureDetail(recorded.failureResult(), ""); !strings.Contains(detail, "API Error: 400 upstream rejected") {
		t.Fatalf("recorded detail missing CLI reason: %q", detail)
	}
	// 事件转发不因包装而丢失：init + result 两条都应到达调用方的 sink。
	if len(sink.events) != 2 {
		t.Fatalf("wrapped sink dropped events: got %d want 2", len(sink.events))
	}
	// 结构化输出文本必须照常透传：Pass A 的 textA 正是靠 AssistantText 累积的，
	// 被包装层吞掉就会退化成"分析代理未返回有效结果"。
	structured := &claudeResultErrorSink{AgentRunSink: &claudeOutputTestSink{}}
	delegate := structured.AgentRunSink.(*claudeOutputTestSink)
	runner.readOutput(strings.NewReader(
		`{"type":"result","is_error":false,"structured_output":{"findings":[{"title":"t"}]}}`+"\n"), structured)
	if len(delegate.texts) != 1 || !strings.Contains(delegate.texts[0], `"findings"`) {
		t.Fatalf("wrapped sink dropped structured output text: %q", delegate.texts)
	}
	// 正常收尾（非失败）不得记下 failure，否则成功运行也会被拼出错误附加段。
	healthy := &claudeResultErrorSink{AgentRunSink: &claudeOutputTestSink{}}
	runner.readOutput(strings.NewReader(`{"type":"result","is_error":false,"result":"一切正常"}`+"\n"), healthy)
	if got := claudeRunFailureDetail(healthy.failureResult(), ""); got != "" {
		t.Fatalf("healthy run should record no failure detail, got %q", got)
	}
	// 出现多条失败 result 时应以最后一条为准（更接近终止状态）。
	multi := &claudeResultErrorSink{AgentRunSink: &claudeOutputTestSink{}}
	runner.readOutput(strings.NewReader(
		`{"type":"result","is_error":true,"result":"第一条失败"}`+"\n"+
			`{"type":"result","is_error":true,"result":"最后一条失败"}`+"\n"), multi)
	if got := claudeRunFailureDetail(multi.failureResult(), ""); !strings.Contains(got, "最后一条失败") {
		t.Fatalf("latest failure result should win, got %q", got)
	}
}

func (*claudeTurnTestSink) Event(string, json.RawMessage) {}
func (*claudeTurnTestSink) AssistantText(string, string)  {}
func (*claudeTurnTestSink) SessionIdentified(string)      {}
func (*claudeTurnTestSink) SessionInitialized()           {}
func (*claudeTurnTestSink) TurnStarted()                  {}
func (sink *claudeTurnTestSink) TurnFinished(err error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.finished = append(sink.finished, err)
}

func (sink *claudeTurnTestSink) errors() []error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]error(nil), sink.finished...)
}

func newClaudeSessionForTimerTest(timeout time.Duration) *claudeCLISession {
	return &claudeCLISession{
		cmd:                    &exec.Cmd{},
		processDone:            make(chan struct{}),
		turnIdleTimeout:        timeout,
		initialResponseTimeout: timeout,
		toolResultTimeout:      timeout,
	}
}

func newClaudeSessionForPhaseTimerTest(initial, afterToolResult, idle time.Duration) *claudeCLISession {
	return &claudeCLISession{
		cmd:                    &exec.Cmd{},
		processDone:            make(chan struct{}),
		turnIdleTimeout:        idle,
		initialResponseTimeout: initial,
		toolResultTimeout:      afterToolResult,
	}
}

func startTimerTestTurn(session *claudeCLISession, turn *claudeSessionTurn, queued ...*claudeSessionTurn) {
	session.mu.Lock()
	session.current = turn
	session.queued = queued
	turn.waitPhase = claudeTurnWaitingInitialResponse
	turn.lastEvent = "user_prompt"
	session.startTurnTimerLocked(turn)
	session.mu.Unlock()
}

func waitForTurnErrors(t *testing.T, sink *claudeTurnTestSink, count int) []error {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errors := sink.errors(); len(errors) == count {
			return errors
		}
		time.Sleep(5 * time.Millisecond)
	}
	return sink.errors()
}

func TestClaudeSessionFailsStalledCurrentAndQueuedTurns(t *testing.T) {
	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	firstSink, secondSink := &claudeTurnTestSink{}, &claudeTurnTestSink{}
	first := &claudeSessionTurn{sink: firstSink}
	second := &claudeSessionTurn{sink: secondSink}
	startTimerTestTurn(session, first, second)

	firstErrors := waitForTurnErrors(t, firstSink, 1)
	secondErrors := waitForTurnErrors(t, secondSink, 1)
	if len(firstErrors) != 1 || !errors.Is(firstErrors[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("first turn errors=%v", firstErrors)
	}
	if len(secondErrors) != 1 || !errors.Is(secondErrors[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("queued turn errors=%v", secondErrors)
	}
	var currentStall *claudeTurnStallError
	if !errors.As(firstErrors[0], &currentStall) {
		t.Fatalf("current error=%T, want *claudeTurnStallError", firstErrors[0])
	}
	var queuedStall *claudeQueuedTurnCancelledError
	if !errors.As(secondErrors[0], &queuedStall) {
		t.Fatalf("queued error=%T, want *claudeQueuedTurnCancelledError", secondErrors[0])
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.stopped || session.current != nil || len(session.queued) != 0 {
		t.Fatalf("session was not fully stopped: stopped=%v current=%v queued=%d", session.stopped, session.current, len(session.queued))
	}
}

func TestClaudeSessionOutputExtendsTurnIdleTimeout(t *testing.T) {
	session := newClaudeSessionForTimerTest(75 * time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	time.Sleep(45 * time.Millisecond)
	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"text","text":"仍在处理"}]`))
	time.Sleep(45 * time.Millisecond)
	if errors := sink.errors(); len(errors) != 0 {
		t.Fatalf("turn timed out despite recent output: %v", errors)
	}
	turnErrors := waitForTurnErrors(t, sink, 1)
	if len(turnErrors) != 1 || !errors.Is(turnErrors[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("turn errors=%v", turnErrors)
	}
}

func TestClaudeSessionToolResultUsesShortContinuationTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(200*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Read"}]`))
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"tool_result"}]`))

	errs := waitForTurnErrors(t, sink, 1)
	if len(errs) != 1 || !errors.Is(errs[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("turn errors=%v", errs)
	}
	var stall *claudeTurnStallError
	if !errors.As(errs[0], &stall) {
		t.Fatalf("stall error=%T, want *claudeTurnStallError", errs[0])
	}
	if stall.phase != claudeTurnWaitingAfterToolResult || stall.toolName != "Read" || stall.lastEvent != "user.tool_result" {
		t.Fatalf("stall details=%+v", stall)
	}
}

func TestClaudeSessionUserReplayDoesNotExtendInitialResponseTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(20*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	time.Sleep(10 * time.Millisecond)
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"text","text":"回放提示词"}]`))
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingInitialResponse {
		t.Fatalf("turn errors=%v", errs)
	}
}

func TestClaudeSessionModelContinuationRestoresLongTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(200*time.Millisecond, 20*time.Millisecond, 75*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Read"}]`))
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"tool_result"}]`))
	time.Sleep(10 * time.Millisecond)
	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"text","text":"继续处理"}]`))
	time.Sleep(45 * time.Millisecond)
	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("model continuation did not restore the long timeout: %v", errs)
	}
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingForActivity {
		t.Fatalf("turn errors=%v", errs)
	}
}

func TestClaudeSessionToolExecutionKeepsLongTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(20*time.Millisecond, 20*time.Millisecond, 75*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Bash"}]`))
	time.Sleep(45 * time.Millisecond)
	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("tool execution timed out before long timeout: %v", errs)
	}
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingForToolResult || stall.toolName != "Bash" {
		t.Fatalf("turn errors=%v", errs)
	}
}

func TestClaudeSessionResultCancelsTurnIdleTimeout(t *testing.T) {
	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.finishCurrent(nil)
	time.Sleep(50 * time.Millisecond)
	turnErrors := sink.errors()
	if len(turnErrors) != 1 || turnErrors[0] != nil {
		t.Fatalf("result completion errors=%v", turnErrors)
	}
}

func TestClaudeSessionResultEventCancelsTimerBeforeCompletion(t *testing.T) {
	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("result", nil)
	time.Sleep(50 * time.Millisecond)
	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("result event allowed a timeout before completion: %v", errs)
	}
	session.finishCurrent(nil)
	if errs := sink.errors(); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("result completion errors=%v", errs)
	}
}

func TestSSHAgentSessionUsesToolResultContinuationTimeout(t *testing.T) {
	session := &sshAgentSession{
		processDone:            make(chan struct{}),
		turnIdleTimeout:        200 * time.Millisecond,
		initialResponseTimeout: 200 * time.Millisecond,
		toolResultTimeout:      20 * time.Millisecond,
	}
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	session.mu.Lock()
	session.current = turn
	turn.waitPhase = claudeTurnWaitingInitialResponse
	turn.lastEvent = "user_prompt"
	session.startTurnTimerLocked(turn)
	session.mu.Unlock()

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Read"}]`))
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"tool_result"}]`))
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingAfterToolResult || stall.toolName != "Read" {
		t.Fatalf("SSH turn errors=%v", errs)
	}
}

func TestConfigFromEnvParsesClaudeTurnIdleTimeout(t *testing.T) {
	t.Setenv("AUTO_CLAUDE_TURN_IDLE_TIMEOUT", "45m")
	t.Setenv("AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT", "6m")
	t.Setenv("AUTO_CLAUDE_TOOL_RESULT_TIMEOUT", "7m")
	t.Setenv("AUTO_AGENT_UPDATE_TIMEOUT", "16m")
	config := ConfigFromEnv()
	if got := config.ClaudeTurnIdleTimeout; got != 45*time.Minute {
		t.Fatalf("ClaudeTurnIdleTimeout=%s, want 45m", got)
	}
	if got := config.ClaudeInitialResponseTimeout; got != 6*time.Minute {
		t.Fatalf("ClaudeInitialResponseTimeout=%s, want 6m", got)
	}
	if got := config.ClaudeToolResultTimeout; got != 7*time.Minute {
		t.Fatalf("ClaudeToolResultTimeout=%s, want 7m", got)
	}
	if got := config.AgentUpdateTimeout; got != 16*time.Minute {
		t.Fatalf("AgentUpdateTimeout=%s, want 16m", got)
	}
}

func TestConfigFromEnvFallsBackForInvalidClaudeTurnIdleTimeout(t *testing.T) {
	for _, value := range []string{"invalid", "0", "-1m"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AUTO_CLAUDE_TURN_IDLE_TIMEOUT", value)
			t.Setenv("AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT", value)
			t.Setenv("AUTO_CLAUDE_TOOL_RESULT_TIMEOUT", value)
			t.Setenv("AUTO_AGENT_UPDATE_TIMEOUT", value)
			config := ConfigFromEnv()
			if got := config.ClaudeTurnIdleTimeout; got != defaultClaudeTurnIdleTimeout {
				t.Fatalf("ClaudeTurnIdleTimeout=%s, want %s", got, defaultClaudeTurnIdleTimeout)
			}
			if got := config.ClaudeInitialResponseTimeout; got != defaultClaudeInitialResponseTimeout {
				t.Fatalf("ClaudeInitialResponseTimeout=%s, want %s", got, defaultClaudeInitialResponseTimeout)
			}
			if got := config.ClaudeToolResultTimeout; got != defaultClaudeToolResultTimeout {
				t.Fatalf("ClaudeToolResultTimeout=%s, want %s", got, defaultClaudeToolResultTimeout)
			}
			if got := config.AgentUpdateTimeout; got != defaultAgentUpdateTimeout {
				t.Fatalf("AgentUpdateTimeout=%s, want %s", got, defaultAgentUpdateTimeout)
			}
		})
	}
}

func writeNpmClaudeInstall(t *testing.T, dir, version string, executable bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "claude.exe"), []byte("test binary"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackInterruptedNpmClaudeInstall(t *testing.T) {
	prefix := t.TempDir()
	packageRoot := claudeNpmCLIInstall.packageRoot(prefix)
	current := filepath.Join(packageRoot, "claude-code")
	backup := filepath.Join(packageRoot, ".claude-code-previous")
	writeNpmClaudeInstall(t, current, "2.1.224", false)
	writeNpmClaudeInstall(t, backup, "2.1.220", true)

	recovered, err := rollbackInterruptedNpmClaudeInstall(prefix, "2.1.220")
	if err != nil {
		t.Fatalf("rollback interrupted install: %v", err)
	}
	if recovered != "2.1.220" {
		t.Fatalf("recovered version=%q, want 2.1.220", recovered)
	}
	if version, err := claudePackageVersion(current); err != nil || version != "2.1.220" {
		t.Fatalf("active package version=%q err=%v, want 2.1.220", version, err)
	}
	if info, err := os.Stat(filepath.Join(current, "bin", "claude.exe")); err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		t.Fatalf("active binary is not usable: info=%v err=%v", info, err)
	}
	assertNpmCLICommandForTest(t, prefix, claudeNpmCLIInstall)
	entries, err := os.ReadDir(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	foundInterrupted := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".claude-code-interrupted-") {
			foundInterrupted = true
		}
	}
	if !foundInterrupted {
		t.Fatal("interrupted package was not retained as a rollback backup")
	}
}

func TestClaudeUpdateRollsBackInterruptedNpmInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses POSIX shell scripts and symlinks")
	}
	prefix := t.TempDir()
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@anthropic-ai")
	active := filepath.Join(packageRoot, "claude-code")
	writeNpmClaudeInstall(t, active, "2.1.220", true)
	claudeScript := `#!/bin/sh
case "$1" in
  --version) echo "2.1.220 (Claude Code)" ;;
  auth) exit 0 ;;
  update)
    mv "$TEST_PACKAGE_ROOT/claude-code" "$TEST_PACKAGE_ROOT/.claude-code-previous"
    mkdir -p "$TEST_PACKAGE_ROOT/claude-code/bin"
    printf '{"version":"2.1.224"}' > "$TEST_PACKAGE_ROOT/claude-code/package.json"
    printf 'interrupted update' > "$TEST_PACKAGE_ROOT/claude-code/bin/claude.exe"
    mv "$TEST_PREFIX/bin/claude" "$TEST_PREFIX/bin/.claude-interrupted"
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(active, "bin", "claude.exe"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe", filepath.Join(prefix, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	npmScript := "#!/bin/sh\nprintf '%s\\n' \"$TEST_PREFIX\"\n"
	if err := os.WriteFile(filepath.Join(prefix, "bin", "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PREFIX", prefix)
	t.Setenv("TEST_PACKAGE_ROOT", packageRoot)
	t.Setenv("PATH", filepath.Join(prefix, "bin")+string(os.PathListSeparator)+"/usr/bin:/bin")

	runner := &claudeCLIRunner{config: Config{ClaudePath: "claude", AgentUpdateTimeout: time.Minute}}
	previous, current, err := runner.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "已自动回滚到 Claude Code 2.1.220") {
		t.Fatalf("update error=%v, want rollback result", err)
	}
	if previous != "2.1.220" || current != "2.1.220" {
		t.Fatalf("update versions previous=%q current=%q", previous, current)
	}
	if got := runner.Version(context.Background()); got != "2.1.220" {
		t.Fatalf("recovered CLI version=%q, want 2.1.220", got)
	}
}

func TestClaudeSessionIdleTimeoutFailsRunsAndReleasesConversation(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,status,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','session','claude-code','running',1,1,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	for _, run := range []struct {
		id     string
		status string
	}{{"first", "running"}, {"second", "queued"}} {
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,agent_id,agent_runtime_id,execution_policy,status,created_at) values (?, 'conversation', 'claude-code', 'wsl-local', 'full_control', ?, ?)`, run.id, run.status, now); err != nil {
			t.Fatalf("insert %s run: %v", run.id, err)
		}
	}

	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	first := &claudeSessionTurn{sink: &agentRunSink{server: server, runID: "first", conversationID: "conversation", agentID: "claude-code", streaming: true}}
	second := &claudeSessionTurn{sink: &agentRunSink{server: server, runID: "second", conversationID: "conversation", agentID: "claude-code", streaming: true}}
	startTimerTestTurn(session, first, second)

	completed := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var firstStatus, secondStatus, conversationStatus string
		err := server.db.QueryRowContext(context.Background(), `select status from runs where id='first'`).Scan(&firstStatus)
		if err == nil {
			err = server.db.QueryRowContext(context.Background(), `select status from runs where id='second'`).Scan(&secondStatus)
		}
		if err == nil {
			err = server.db.QueryRowContext(context.Background(), `select status from conversations where id='conversation'`).Scan(&conversationStatus)
		}
		if err == nil && firstStatus == "failed" && secondStatus == "failed" && conversationStatus == "idle" {
			completed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed {
		var rawPayload []byte
		if err := server.db.QueryRow(`select payload from events where run_id='first' and type='run.failed' order by created_at desc limit 1`).Scan(&rawPayload); err != nil {
			t.Fatalf("read run.failed event: %v", err)
		}
		var details map[string]string
		if err := json.Unmarshal(rawPayload, &details); err != nil {
			t.Fatalf("decode run.failed payload: %v", err)
		}
		if details["stallPhase"] != string(claudeTurnWaitingInitialResponse) || details["lastEvent"] != "user_prompt" {
			t.Fatalf("unexpected stall details: %#v", details)
		}
		var queuedPayload []byte
		if err := server.db.QueryRow(`select payload from events where run_id='second' and type='run.failed' order by created_at desc limit 1`).Scan(&queuedPayload); err != nil {
			t.Fatalf("read queued run.failed event: %v", err)
		}
		if err := json.Unmarshal(queuedPayload, &details); err != nil {
			t.Fatalf("decode queued run.failed payload: %v", err)
		}
		if details["stallPhase"] != "queued" || details["lastEvent"] != "not_started" || details["previousStallPhase"] != string(claudeTurnWaitingInitialResponse) {
			t.Fatalf("unexpected queued stall details: %#v", details)
		}
		return
	}

	var firstStatus, secondStatus, conversationStatus string
	if err := server.db.QueryRow(`select status from runs where id='first'`).Scan(&firstStatus); err != nil {
		t.Fatalf("read first run: %v", err)
	}
	if err := server.db.QueryRow(`select status from runs where id='second'`).Scan(&secondStatus); err != nil {
		t.Fatalf("read second run: %v", err)
	}
	if err := server.db.QueryRow(`select status from conversations where id='conversation'`).Scan(&conversationStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	t.Fatalf("timeout statuses: first=%q second=%q conversation=%q", firstStatus, secondStatus, conversationStatus)
}

func TestParseClaudeMessage(t *testing.T) {
	cases := []struct {
		name      string
		msg       string // JSON for the message field
		wantParts []string
	}{
		{
			"object with content array",
			`{"content":[{"type":"text","text":"hello"},{"type":"tool_use","name":"Read"}]}`,
			[]string{"hello"},
		},
		{
			"object with content plain string",
			`{"content":"直接文本"}`,
			[]string{"直接文本"},
		},
		{
			"object empty content",
			`{"content":[]}`,
			nil,
		},
		{
			"plain string message",
			`"整段文本作为 message 字符串"`,
			[]string{"整段文本作为 message 字符串"},
		},
		{
			"plain empty string message",
			`""`,
			nil,
		},
		{
			"missing message field",
			``,
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw json.RawMessage
			if c.msg != "" {
				raw = json.RawMessage(c.msg)
			} else {
				raw = nil
			}
			parts, _ := parseClaudeMessage(raw)
			if len(parts) != len(c.wantParts) {
				t.Fatalf("parseClaudeMessage(%q) = %#v; want %#v", c.msg, parts, c.wantParts)
			}
			for i := range parts {
				if parts[i] != c.wantParts[i] {
					t.Errorf("part[%d] = %q; want %q", i, parts[i], c.wantParts[i])
				}
			}
		})
	}
}

// TestClaudeReadOutputAcceptsPlainStringMessage 验证 readOutput（一次性 Run 路径）能
// 接受 message 为纯字符串的 assistant 行——即此前 "cannot unmarshal string into ... .message"
// 导致整行丢弃的形态——并照常把文本交给 AssistantText，且不产生 stream.error。
func TestClaudeReadOutputAcceptsPlainStringMessage(t *testing.T) {
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	// 旧的 struct{Content json.RawMessage} 声明会拒绝此行的 "message":"..." 字符串形态。
	runner.readOutput(strings.NewReader(`{"type":"assistant","parent_tool_use_id":"t1","message":"纯字符串消息正文"}`+"\n"), sink)
	if len(sink.texts) != 1 {
		t.Fatalf("expected 1 assistant text, got %#v", sink.texts)
	}
	if sink.texts[0] != "纯字符串消息正文" {
		t.Errorf("assistant text = %q; want 纯字符串消息正文", sink.texts[0])
	}
	for _, e := range sink.events {
		var errEv struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("unexpected stream.error event: %s", string(e))
		}
	}
}

// errClosedReader 在 Read 时返回 os.ErrClosed，模拟进程被停止/强杀后 stdout/stderr
// 管道被关闭、bufio.Scanner 拿到 "file already closed" 的形态。
type errClosedReader struct{}

func (errClosedReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

// TestClaudeReadOutputSilencesPipeClosed 验证：停止时管道关闭（os.ErrClosed）不应
// 产生 stream.error 事件。此前该路径会在对话历史里留下"流错误 / file already closed"。
func TestClaudeReadOutputSilencesPipeClosed(t *testing.T) {
	runner := &claudeCLIRunner{}

	// readOutput（一次性 Run 路径）。
	sink := &claudeOutputTestSink{}
	runner.readOutput(errClosedReader{}, sink)
	for _, e := range sink.events {
		var errEv struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("readOutput emitted stream.error on pipe close: %s", string(e))
		}
	}

	// readStderr（一次性 Run 路径）。
	sink = &claudeOutputTestSink{}
	runner.readStderr(errClosedReader{}, sink)
	for _, e := range sink.events {
		var errEv struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("readStderr emitted stream.error on pipe close: %s", string(e))
		}
	}
}

// TestCodexReadOutputSilencesPipeClosed 同理验证 codex 一次性读循环。
func TestCodexReadOutputSilencesPipeClosed(t *testing.T) {
	sink := &claudeOutputTestSink{}
	readCodexJSONL(errClosedReader{}, sink, "")
	for _, e := range sink.events {
		var errEv struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("readCodexJSONL emitted stream.error on pipe close: %s", string(e))
		}
	}
}
