package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 钉死 codebuddy 在"需要实际工具调用"（这里用内置 Read 读一个文件）时的 stream-json
// 事件形状：是 CLI 内部自执行工具并给出最终 assistant，还是需要服务器回传 tool_result。
// 这决定会话的 AgentTurnSink 回合握手要不要实现"工具结果批处理回传"。
func TestCodebuddyLiveToolUse(t *testing.T) {
	bin := codebuddyTestBinary()
	if bin == "" {
		t.Skip("未安装 codebuddy，跳过")
	}
	dir := t.TempDir()
	secret := "HELLO-TOOL-42"
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte(secret), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	frames := []string{`{"type":"user","message":{"role":"user","content":"Read the file data.txt in the current directory and tell me its exact content verbatim."}}`}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if strings.HasSuffix(strings.ToLower(bin), ".cmd") || strings.HasSuffix(strings.ToLower(bin), ".bat") {
		cmd = exec.CommandContext(ctx, "cmd", "/d", "/c", bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	} else {
		cmd = exec.CommandContext(ctx, bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	}
	cmd.Dir = dir
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
		if json.Unmarshal([]byte(line), &ev) == nil {
			events = append(events, ev)
		}
	}

	var types []string
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		sub, _ := ev["subtype"].(string)
		var kinds []string
		if msg, ok := ev["message"].(map[string]any); ok {
			if content, ok := msg["content"].([]any); ok {
				for _, part := range content {
					if p, ok := part.(map[string]any); ok {
						if k, _ := p["type"].(string); k != "" {
							kinds = append(kinds, k)
						}
					}
				}
			}
		}
		types = append(types, fmt.Sprintf("%s/%s[%s]", typ, sub, strings.Join(kinds, ",")))
	}
	t.Logf("tool-use 事件序列: %s", strings.Join(types, " -> "))

	lastResult := ""
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ == "result" {
			if r, _ := ev["result"].(string); r != "" {
				lastResult = r
			}
		}
	}
	if !strings.Contains(lastResult, secret) {
		t.Fatalf("工具调用后 result 未包含文件内容 %q，result=%q", secret, truncateSmart(lastResult, 200))
	}
}

func truncateSmart(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}