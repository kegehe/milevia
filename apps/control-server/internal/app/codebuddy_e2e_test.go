package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// codebuddyTestBinary returns a path to an installed codebuddy that we can drive
// in a live harness. Returns "" when none is installed/on PATH. 用真实二进制把
// codebuddy 的 stream-json 会话契约钉死（输入帧 + init/assistant/result 事件），
// runner 据此实现（见 docs/CodeBuddy 方案 §阶段3）。
//
// Windows 下 npm 的 shim 是 .cmd，需经 cmd.exe 启动（与 codexCommandContext 同款）。
func codebuddyTestBinary() string {
	if p := os.Getenv("CODEBUDDY_TEST_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, cand := range []string{"codebuddy", "codebuddy.cmd"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	return ""
}

// runCodebuddyStreamJSON spawns codebuddy in -p stream-json mode with the given
// stdin frames and returns the parsed NDJSON events it emits.
func runCodebuddyStreamJSON(t *testing.T, bin string, frames []string) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if strings.HasSuffix(strings.ToLower(bin), ".cmd") || strings.HasSuffix(strings.ToLower(bin), ".bat") {
		cmd = exec.CommandContext(ctx, "cmd", "/d", "/c", bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	} else {
		cmd = exec.CommandContext(ctx, bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	}
	cmd.Dir = t.TempDir()
	if runtime.GOOS != "windows" {
		configureProcessGroup(cmd)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("open stdin: %v", err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codebuddy: %v", err)
	}
	for _, frame := range frames {
		if _, err := stdin.Write([]byte(frame + "\n")); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	_ = stdin.Close()
	_ = cmd.Wait()

	var events []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // 忽略非 JSON 行
		}
		events = append(events, ev)
	}
	return events
}

// 活得跑一次已安装的 codebuddy，断言 stream-json 会话契约：
//   输入 frame 同 Claude（type=user.message.content），输出依次有 system/init →
//   assistant(text) → result(success)。
// codebuddy 未安装时跳过（不在常规回归里动不动拉起目录探测）。
func TestCodebuddyLiveStreamJSONContract(t *testing.T) {
	bin := codebuddyTestBinary()
	if bin == "" {
		t.Skip("未安装 codebuddy，跳过活 harness（设置 CODEBUDDY_TEST_BIN 指向可执行文件可强制启用）")
	}
	frames := []string{`{"type":"user","message":{"role":"user","content":"say hi in one short sentence"}}`}
	events := runCodebuddyStreamJSON(t, bin, frames)

	var sawInit, sawAssistant, sawResult bool
	var assistantTexts []string
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		switch typ {
		case "system":
			if sub, _ := ev["subtype"].(string); sub == "init" {
				sawInit = true
				if _, ok := ev["session_id"].(string); !ok {
					t.Fatalf("init 事件缺 session_id: %v", ev)
				}
			}
		case "assistant":
			sawAssistant = true
			if msg, ok := ev["message"].(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, part := range content {
						if p, ok := part.(map[string]any); ok && p["type"] == "text" {
							if s, _ := p["text"].(string); s != "" {
								assistantTexts = append(assistantTexts, s)
							}
						}
					}
				}
			}
		case "result":
			if sub, _ := ev["subtype"].(string); sub == "success" {
				sawResult = true
			}
		}
	}
	if !sawInit {
		t.Fatalf("未收到 system/init 事件，共 %d 条: %v", len(events), events)
	}
	if !sawAssistant || len(assistantTexts) == 0 {
		t.Fatalf("未收到 assistant(text) 事件，texts=%v", assistantTexts)
	}
	if !sawResult {
		t.Fatalf("未收到 result(success) 事件")
	}
}