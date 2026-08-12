package app

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// TestWslInnerToken 验证 wslInnerToken 产出双引号包裹的 base64 命令替换 token：
// 外层双引号保证在 sh 脚本体内命令替换结果不被字段切分；base64 内容不含 shell 特殊字符。
func TestWslInnerToken(t *testing.T) {
	in := `hello world "quoted" $home`
	tok := wslInnerToken(in)
	if !strings.HasPrefix(tok, `"$(echo `) || !strings.HasSuffix(tok, ` | base64 -d)"`) {
		t.Fatalf("wslInnerToken(%q) = %q; want double-quoted base64 substitution", in, tok)
	}
	// base64 载荷本身不含 shell 特殊字符（这就是它能无损穿越两层 argv 的原因）。
	noPrefix := strings.TrimPrefix(tok, `"$(echo `)
	payload := strings.TrimSuffix(noPrefix, ` | base64 -d)"`)
	if strings.ContainsAny(payload, " \t\n'\"\\{}[]()*?$;&|<>#~`") {
		t.Fatalf("wslInnerToken base64 payload contains shell-special chars: %q", payload)
	}
	// 还原应等于原始参数。
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if string(decoded) != in {
		t.Errorf("roundtrip mismatch: %q -> %q", in, decoded)
	}
}

// TestWslNativeCommandScript 验证 wslNativeCommand 构造的自包含脚本包含：
//   - wslPathPrefix（PATH 前置原生 npm bin，防 Windows shim 劫持）
//   - cd <workdir>（工作目录在脚本内显式设置）
//   - exec <cli> <args>（每个参数都经 wslInnerToken 编码，可被 sh 还原为单 argv）
func TestWslNativeCommandScript(t *testing.T) {
	r := &wslAgentRunner{distro: "Ubuntu", codex: &codexCLIRunner{config: Config{CodexPath: "codex"}}}
	args := []string{"exec", "-c", `model="gpt-5-codex"`, "--json", "hello world from milevia"}
	cmd := r.wslNativeCommand(context.Background(), r.codex.config.CodexPath, args, nil, "/home/tangmaoke/proj")

	// wsl.exe -d <distro> -- sh -c <wslEncodeArg(script)>（-- 而非 -e，见 wslNativeCommand）。
	raw := cmd.String()
	// argv 形态：wsl.exe -d <distro> -- sh -c <base64脚本token>（-- 而非 -e，见 wslNativeCommand）。
	for _, want := range []string{"-d ", "Ubuntu", "-- ", "sh -c", "base64 -d"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("wslNativeCommand argv missing %q in: %q", want, raw)
		}
	}
	// 直接从 cmd.Args 拿最后一个参数，解码一层 base64 得到脚本本体。
	tail := cmd.Args[len(cmd.Args)-1]
	body := wslDecodeArgForTest(t, tail)

	for _, want := range []string{
		"export PATH=",
		".npm-global/bin",
		`cd "$(echo L2hvbWUvdGFuZ21hb2tlL3Byb2o= | base64 -d)"`, // /home/tangmaoke/proj
		"exec codex",
		`"$(echo`, // 每个参数均为双引号包裹的 base64 命令替换
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script missing %q:\n%s", want, body)
		}
	}
	// workdir 兜底：空 workdir 时不产生 cd 段。
	cmdNoCD := r.wslNativeCommand(context.Background(), "codex", []string{"--version"}, nil, "")
	bodyNoCD := wslDecodeArgForTest(t, cmdNoCD.Args[len(cmdNoCD.Args)-1])
	if strings.Contains(bodyNoCD, "; cd ") || strings.Contains(bodyNoCD, `cd "$`) {
		t.Errorf("empty workdir should not emit cd segment:\n%s", bodyNoCD)
	}
}

// wslDecodeArgForTest 把 wslEncodeArg 的 $(echo <b64> | base64 -d) 形式还原为明文，
// 用于断言脚本内容。纯测试辅助，不做任何进程拉起。
func wslDecodeArgForTest(t *testing.T, arg string) string {
	t.Helper()
	const open = "$(echo "
	if !strings.HasPrefix(arg, open) || !strings.HasSuffix(arg, " | base64 -d)") {
		return arg // 无非特殊字符时不编码，直接返回原文
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(arg, open), " | base64 -d)")
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode script arg: %v", err)
	}
	return string(decoded)
}
