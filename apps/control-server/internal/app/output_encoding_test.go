package app

import (
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// gbkAgentBenchmarkLine 是本次乱码问题的原始现场：benchmark 脚本在中文 Windows 下
// 按系统代码页 CP936 写出的一行结果。服务端按 UTF-8 读取时，这些字节一旦进入
// encoding/json 就会被替换成 U+FFFD，中文不可逆丢失。
const gbkAgentBenchmarkLine = "现在：lower(payload) + like，  命中 401 行  完成   263.6 ms"

func encodeGBK(t *testing.T, text string) []byte {
	t.Helper()
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatalf("encode %q as GBK: %v", text, err)
	}
	return encoded
}

func TestUTF8ChildEnv(t *testing.T) {
	want := []string{"PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"}
	got := utf8ChildEnv()
	if len(got) != len(want) {
		t.Fatalf("utf8ChildEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("utf8ChildEnv()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// 返回的必须是独立切片：调用方（如 projectRunner 叠加 envVars）会直接 append。
	got[0] = "PYTHONIOENCODING=latin-1"
	if again := utf8ChildEnv(); again[0] != want[0] {
		t.Fatalf("utf8ChildEnv 返回的切片被改写后污染了后续调用: %q", again[0])
	}
}

func TestAppendUTF8ChildEnv(t *testing.T) {
	tests := []struct {
		name      string
		inherited []string
		want      []string
	}{
		{
			name:      "空环境只补两个变量",
			inherited: nil,
			want:      []string{"PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"},
		},
		{
			name:      "无关变量原样保留且顺序不变",
			inherited: []string{"PATH=/usr/bin", "HOME=/root"},
			want:      []string{"PATH=/usr/bin", "HOME=/root", "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"},
		},
		{
			name:      "继承来的同名旧值被剔除，避免环境块里出现两个值",
			inherited: []string{"PYTHONIOENCODING=cp936", "PATH=/usr/bin"},
			want:      []string{"PATH=/usr/bin", "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"},
		},
		{
			name:      "变量名大小写不敏感去重",
			inherited: []string{"pythonioencoding=cp936", "pythonutf8=0"},
			want:      []string{"PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendUTF8ChildEnv(tt.inherited)
			if len(got) != len(tt.want) {
				t.Fatalf("appendUTF8ChildEnv(%v) = %v, want %v", tt.inherited, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("appendUTF8ChildEnv(%v)[%d] = %q, want %q", tt.inherited, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// 自带的 UTF-8 变量必须真的落到 agent CLI 进程上，否则治本那一层是空转。
func TestManagedCLIEnvironmentInjectsUTF8Output(t *testing.T) {
	environment := managedCLIEnvironment(nil, []string{"PATH=/usr/bin"})
	if want := []string{"PATH=/usr/bin", "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"}; !slices.Equal(environment, want) {
		t.Fatalf("managedCLIEnvironment() = %v, want %v", environment, want)
	}

	// profile 显式声明同名变量时必须能覆盖默认值——否则用户无法针对个别项目关掉
	// UTF-8 Mode。
	profile := &AgentRuntimeProfile{
		AgentID:  "claude",
		AuthMode: "cli_managed",
		Env:      map[string]string{"PYTHONUTF8": "0"},
	}
	environment = managedCLIEnvironment(profile, []string{"PYTHONUTF8=1"})
	if countValue(environment, "PYTHONUTF8") != 1 {
		t.Fatalf("PYTHONUTF8 应只出现一次, got %v", environment)
	}
	if last := lastValue(environment, "PYTHONUTF8"); last != "0" {
		t.Fatalf("profile 未覆盖默认值: PYTHONUTF8=%q, want 0 (env=%v)", last, environment)
	}
	if lastValue(environment, "PYTHONIOENCODING") != "utf-8" {
		t.Fatalf("未被覆盖的 PYTHONIOENCODING 丢失: %v", environment)
	}
}

// WSL 侧进程拿不到只设到 wsl.exe 上的变量，必须经 WSLENV 透传——wslForwardEnvKeys
// 是前缀白名单，漏了这两个键则 UTF-8 注入在 WSL 路径上会断掉。
func TestWSLBuildEnvForwardsUTF8Output(t *testing.T) {
	cmdEnv, wslenv := wslBuildEnv(utf8ChildEnv())
	if !containsString(cmdEnv, "PYTHONIOENCODING=utf-8") || !containsString(cmdEnv, "PYTHONUTF8=1") {
		t.Fatalf("wslBuildEnv cmdEnv 未含 UTF-8 变量: %v", cmdEnv)
	}
	for _, want := range []string{"PYTHONIOENCODING/u", "PYTHONUTF8/u"} {
		if !containsString(strings.Split(wslenv, ":"), want) {
			t.Fatalf("WSLENV 缺少 %s: wslenv=%q", want, wslenv)
		}
	}
}

// 本次 bug 的字节级回归：真实的 GBK 字节必须被还原成中文，而不是塌成 U+FFFD。
func TestDecodeRunOutputBytesRestoresGBKAgentOutput(t *testing.T) {
	gbk := encodeGBK(t, gbkAgentBenchmarkLine)
	if got := decodeRunOutputBytes(gbk, true); string(got) != gbkAgentBenchmarkLine {
		t.Fatalf("decodeRunOutputBytes(GBK) = %q, want %q", got, gbkAgentBenchmarkLine)
	}
	if decoded := decodeRunOutputLine(gbk, true); strings.ContainsRune(decoded, '\uFFFD') {
		t.Fatalf("还原结果仍含替换符 U+FFFD: %q", decoded)
	}
	// 非 Windows 目标（WSL/SSH 产出的 UTF-8）不做转码，避免误伤。
	if got := decodeRunOutputBytes(gbk, false); string(got) != string(gbk) {
		t.Fatalf("transcode=false 时应原样返回, got %q", got)
	}
}

// 端到端回归：GBK 的 JSONL 行走完「转码 → sanitizeAgentJSONL → json.Marshal」整条
// 链路后，中文必须完整存活、且不出现 U+FFFD。
//
// 这条比单独测解码函数更有价值：encoding/json 才是真正把非法 UTF-8 换成 U+FFFD 的地方，
// 只有把转码插在它之前才救得回来。这里显式传 transcode=true（而非依赖平台闸门），
// 以便非 Windows 的 CI 同样覆盖这条链路——闸门本身由
// TestDecodeAgentOutputBytesHonoursPlatformGate 负责。
func TestAgentJSONLSurvivesGBKTranscode(t *testing.T) {
	line := `{"type":"assistant","message":"` + gbkAgentBenchmarkLine + `"}`
	decoded := decodeRunOutputBytes(encodeGBK(t, line), true)

	sanitized, err := sanitizeAgentJSONL(decoded)
	if err != nil {
		t.Fatalf("sanitizeAgentJSONL: %v", err)
	}
	if strings.ContainsRune(string(sanitized), '\uFFFD') {
		t.Fatalf("全链路后仍出现 U+FFFD: %s", sanitized)
	}

	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(sanitized, &payload); err != nil {
		t.Fatalf("unmarshal sanitized: %v", err)
	}
	if payload.Message != gbkAgentBenchmarkLine {
		t.Fatalf("中文未还原:\n got %q\nwant %q", payload.Message, gbkAgentBenchmarkLine)
	}

	// 反证：跳过转码时同一条链路确实会损坏。若哪天 encoding/json 不再替换非法
	// UTF-8，这条断言会失败并提醒我们重新审视整个兜底方案——避免用例变成摆设。
	broken, err := sanitizeAgentJSONL(encodeGBK(t, line))
	if err != nil {
		t.Fatalf("sanitizeAgentJSONL(未转码): %v", err)
	}
	if !strings.ContainsRune(string(broken), '\uFFFD') {
		t.Fatal("未转码的 GBK JSONL 没有损坏，说明这条回归用例已失去意义")
	}
}

// 兜底转码的能力边界：字节一旦被替换成 U+FFFD 就已经是合法 UTF-8，转码无从下手。
// 把这条边界固定下来，避免以后误以为这层兜底能救回已损坏的历史数据。
func TestDecodeRunOutputBytesCannotRecoverLossyReplacement(t *testing.T) {
	// 与 gbkAgentBenchmarkLine 对应的真实乱码形态（现场粘贴出来的样子）。
	lossy := "\uFFFD\uFFFD\uFFFD\u06A3\uFFFDlower(payload) + like\uFFFD\uFFFD"
	got := decodeRunOutputBytes([]byte(lossy), true)
	if string(got) != lossy {
		t.Fatalf("合法 UTF-8 的已损坏文本不应被改写: %q", got)
	}
}

// decodeAgentOutputBytes 是否转码必须跟随平台判断，防止有人后续把开关写死。
func TestDecodeAgentOutputBytesHonoursPlatformGate(t *testing.T) {
	gbk := encodeGBK(t, gbkAgentBenchmarkLine)
	got := string(decodeAgentOutputBytes(gbk))
	if runtime.GOOS == "windows" {
		if got != gbkAgentBenchmarkLine {
			t.Fatalf("Windows 上应还原为 %q, got %q", gbkAgentBenchmarkLine, got)
		}
		return
	}
	if got != string(gbk) {
		t.Fatalf("非 Windows 上应原样返回, got %q", got)
	}
}

// countValue 统计某个变量名在环境块里出现的次数（大小写不敏感）。
func countValue(environment []string, name string) int {
	count := 0
	for _, item := range environment {
		if key, _, found := strings.Cut(item, "="); found && strings.EqualFold(key, name) {
			count++
		}
	}
	return count
}

// lastValue 返回某变量最后一次出现的值——环境块中后者生效。
func lastValue(environment []string, name string) string {
	value := ""
	for _, item := range environment {
		if key, val, found := strings.Cut(item, "="); found && strings.EqualFold(key, name) {
			value = val
		}
	}
	return value
}
