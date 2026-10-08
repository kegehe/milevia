package app

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sshExitSummary 必须是**有界**的：它要和前缀、详情段一起落进 insightRunMessageBytes
// 那 240 字节。原来这条路径直接把 waitErr 交给 %w，而 ssh.ExitError 的文案是
// Waitmsg.String() —— 除了状态码与信号名，它还会拼上 ". Reason was: <远端给的字符串>"，
// 那一段长度由远端 sshd 决定，能把详情段的预算整个挤掉。
//
// 这里能构造的只有 ExitError 的零值（字段私有、没有 setter）与普通错误；后者原本
// 可以是任意长度，正是"远端可控"那一类的替身。
func TestSSHExitSummaryIsBounded(t *testing.T) {
	remoteControlled := "远端给的超长原因" + strings.Repeat("很长", 200)
	summary := sshExitSummary(errors.New(remoteControlled))
	if len(summary) > 40 {
		t.Fatalf("普通错误的摘要没被限长：%d 字节（%q）", len(summary), summary)
	}
	if !strings.HasPrefix(summary, "远端给的超长原因") {
		t.Fatalf("摘要应当保留开头（那是用户拿去搜索的线索），实际 %q", summary)
	}

	exitErr := &ssh.ExitError{}
	if got := sshExitSummary(exitErr); !strings.Contains(got, "exit 0") {
		t.Fatalf("ExitError 应当被压成 exit <status> 的短形态，实际 %q", got)
	}
	// 与运行时的实际形态对齐：这条路径给用户的第一个线索是"退出了"，不是那句英文长句。
	if got := sshExitSummary(exitErr); strings.Contains(got, "Process exited") {
		t.Fatalf("不该原样带上 Waitmsg 的英文长句（它会带出远端可控的 Reason）：%q", got)
	}
}

// 一次性 SSH 运行失败时的整条文案必须**装得进** insightRunErrorMessage 的 240 字节截断
// （它保留开头、从末尾切），且详情段的收尾括号不许被切掉 —— 被切掉就说明预算算错了，
// 而外层截断切掉的恰恰是最有用的报错原文。
//
// ⚠️ 这里**驱动真正组装那条串的函数**（`sshClaudeRunFailureError`），不是自己把算式抄一遍。
// 第一版就是抄的：算式只存在于调用点，于是把调用点改坏（例如沿用本地的 180）它照样绿 ——
// 2026-09-29 的独立复查用变异实测抓到了这一点（改坏后真实文案 257 字节、被截断，而那条
// 测试 PASS）。抽函数 + 驱动它，才是"量真正发出去的那个串"。
//
// 摘要取**非 ExitError** 那一档（40 字节）当最坏形态：它比 `ssh.ExitError` 的上界
// （`exit ` + 十位状态码 + 12 字节信号 = 33）更长，而这条路径确实可能拿到别的错误
// （连接断掉等），`sshExitSummary` 对它们统一是 40 字节上限。
func TestSSHOneShotFailureFitsInsightBudget(t *testing.T) {
	// 用 ASCII 造：中文会在 40 字节处回退到字符边界（实际 39），拿它当"最坏值"会差一字节。
	longSummaryInput := errors.New(strings.Repeat("remote failure detail ", 50))
	if got := len(sshExitSummary(longSummaryInput)); got != 40 {
		t.Fatalf("非 ExitError 的摘要应当是 40 字节上限，实际 %d（预算断言的前提变了）", got)
	}

	// 详情段的输入给到远超额度，逼出收窄逻辑。
	err := sshClaudeRunFailureError(longSummaryInput, nil, strings.Repeat("远端报错原文 ", 100))
	message := err.Error()

	if len(message) > insightRunMessageBytes {
		t.Fatalf("最坏形态 %d 字节，超出 %d 字节预算：%q", len(message), insightRunMessageBytes, message)
	}
	if !strings.HasPrefix(message, sshClaudeExitPrefix) {
		t.Fatalf("开头不是 SSH 前缀：%q", message)
	}
	// 详情段的收尾括号必须还在：它没了就说明额度算多了、外层截断吃掉了末尾。
	if !strings.HasSuffix(message, "）") {
		t.Fatalf("详情段的收尾括号被切掉了（预算没算对）：%q", message)
	}
	if !strings.Contains(message, "（stderr：") {
		t.Fatalf("有 stderr 却不给详情段：用户只剩一个退出码（%q）", message)
	}

	// 前缀必须含"失败"：否则 localizedErrorText 会给它套上"任务执行失败，请查看任务日志
	// 后重试。"（app_test.go 那条哨兵同时钉住这一点，这里就近再钉一次）。
	if !strings.Contains(sshClaudeExitPrefix, "失败") {
		t.Fatalf("SSH 前缀里没有「失败」：%q", sshClaudeExitPrefix)
	}
	// 整条也不许被套上那层误导前缀（这才是用户实际会看到的结果）。
	if text := errorText(err); strings.Contains(text, taskFailureFallbackPrefix) {
		t.Fatalf("整条错误被套上了任务域兜底前缀：%q", text)
	}
}
