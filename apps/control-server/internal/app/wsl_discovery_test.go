package app

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDecodeUTF16LE(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"with BOM", []byte{0xFF, 0xFE, 'U', 0x00, 'b', 0x00, 'u', 0x00, 'n', 0x00, 't', 0x00, 'u', 0x00}, "Ubuntu"},
		{"without BOM", []byte{'U', 0x00, 'b', 0x00, 'u', 0x00}, "Ubu"},
		{"empty", nil, ""},
		{"only BOM", []byte{0xFF, 0xFE}, ""},
		{"odd length trailing byte dropped", []byte{'A', 0x00, 0x01}, "A"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeUTF16LE(c.in); got != c.want {
				t.Errorf("decodeUTF16LE = %q; want %q", got, c.want)
			}
		})
	}
}

func TestUNCToWslPath(t *testing.T) {
	cases := []struct {
		name   string
		unc    string
		distro string
		want   string
		ok     bool
	}{
		{"wsl$ home", `\\wsl$\Ubuntu\home\user\proj`, "Ubuntu", `/home/user/proj`, true},
		{"wsl.localhost home", `\\wsl.localhost\Ubuntu\home\user`, "Ubuntu", `/home/user`, true},
		{"case insensitive prefix", `\\WSL$\Ubuntu\home`, "Ubuntu", `/home`, true},
		{"distro mismatch", `\\wsl$\Debian\home\user`, "Ubuntu", "", false},
		{"distro empty accepts any", `\\wsl$\Debian\home`, "", `/home`, true},
		{"only root", `\\wsl$\Ubuntu`, "Ubuntu", "/", true},
		{"non wsl unc", `\\server\share\dir`, "Ubuntu", "", false},
		{"plain windows path", `C:\Users\foo`, "Ubuntu", "", false},
		{"linux path", `/home/user`, "Ubuntu", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := uncToWslPath(c.unc, c.distro)
			if ok != c.ok {
				t.Fatalf("uncToWslPath(%q) ok = %v; want %v", c.unc, ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("uncToWslPath(%q) = %q; want %q", c.unc, got, c.want)
			}
		})
	}
}

func TestWslToUncPath(t *testing.T) {
	cases := []struct {
		linux  string
		distro string
		want   string
	}{
		{"/home/user/proj", "Ubuntu", `\\wsl$\Ubuntu\home\user\proj`},
		{"/home", "Ubuntu", `\\wsl$\Ubuntu\home`},
		{"home/user", "Ubuntu", `\\wsl$\Ubuntu\home\user`},
		{"/", "Ubuntu", `\\wsl$\Ubuntu\`},
	}
	for _, c := range cases {
		if got := wslToUncPath(c.linux, c.distro); got != c.want {
			t.Errorf("wslToUncPath(%q, %q) = %q; want %q", c.linux, c.distro, got, c.want)
		}
	}
}

func TestWslUNCRoot(t *testing.T) {
	if got := wslUNCRoot("Ubuntu"); got != `\\wsl$\Ubuntu` {
		t.Errorf("wslUNCRoot = %q; want %q", got, `\\wsl$\Ubuntu`)
	}
}

func TestWslPathWithin(t *testing.T) {
	root := `\\wsl$\Ubuntu\home\tangmaoke`
	cases := []struct {
		name   string
		root   string
		cand   string
		within bool
	}{
		{"equal", root, root, true},
		{"child", root, root + `\proj`, true},
		{"slash child", root, `//wsl$/Ubuntu/home/tangmaoke/proj`, true},
		{"distro case", root, `\\WSL$\Ubuntu\home\tangmaoke\proj`, true},
		{"outside root", root, `\\wsl$\Ubuntu\home\other`, false},
		{"sibling prefix", `\\wsl$\Ubuntu\home\tangmaoke`, `\\wsl$\Ubuntu\home\tangmaokeX`, false},
		{"different distro", root, `\\wsl$\Debian\home\tangmaoke\proj`, false},
		{"localhost form", `\\wsl.localhost\Ubuntu\home`, `\\wsl.localhost\Ubuntu\home\x`, true},
		{"dotdot escape", root, root + `\..\..\..\etc`, false},   // .. 穿越应被拒
		{"dotdot within", root, root + `\a\..\b`, true},          // 内部 .. 归一后仍在 root 内
		{"root is wsl.localhost child", `\\wsl.localhost\Ubuntu`, `\\wsl.localhost\Ubuntu\home`, true},
		{"non-unc", root, `C:\some`, false},
		{"non-unc root", `C:\some`, `C:\some\x`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wslPathWithin(c.root, c.cand); got != c.within {
				t.Errorf("wslPathWithin(%q,%q) = %v; want %v", c.root, c.cand, got, c.within)
			}
		})
	}
}

func TestWslUncNormalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`\\wsl$\Ubuntu\home\x`, `\\wsl$\Ubuntu\home\x`},
		{`//wsl$/Ubuntu/home/x`, `\\wsl$\Ubuntu\home\x`},
		{`\\wsl.localhost\Ubuntu\home`, `\\wsl.localhost\Ubuntu\home`},
		{`///wsl$/Ubuntu`, `\\wsl$\Ubuntu`},
		{`C:\foo`, ""},
		{`/home/x`, ""},
	}
	for _, c := range cases {
		if got := wslUncNormalize(c.in); got != c.want {
			t.Errorf("wslUncNormalize(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestWslJoinUnc(t *testing.T) {
	root := `\\wsl$\Ubuntu\home\tangmaoke`
	cases := []struct {
		child string
		want  string
		ok    bool
	}{
		{"proj", root + `\proj`, true},
		{"a/b", root + `\a\b`, true},
		{"a/../b", root + `\b`, true}, // filepath.Clean 归一
		{"..", "", false},
		{"../x", "", false},
		{`\abs`, "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := wslJoinUnc(root, c.child)
		if ok != c.ok {
			t.Errorf("wslJoinUnc(%q) ok=%v; want %v", c.child, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("wslJoinUnc(%q)=%q; want %q", c.child, got, c.want)
		}
	}
}

func TestWindowsToWSLMntPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain drive", `C:\foo\bar.exe`, `/mnt/c/foo/bar.exe`},
		{"plain drive lowercase preserved", `D:\projects\x.exe`, `/mnt/d/projects/x.exe`},
		{"drive root", `C:\`, `/mnt/c`},
		{"extended-length drive", `\\?\D:\projects\prog\sidecar.exe`, `/mnt/d/projects/prog/sidecar.exe`},
		{"extended-length drive toLowerCase drive only", `\\?\C:\Windows\App.exe`, `/mnt/c/Windows/App.exe`},
		{"extended-length drive with space in dir", `\\?\C:\Program Files\App\bin\hook.exe`, `/mnt/c/Program Files/App/bin/hook.exe`},
		{"extended UNC keeps backslash for share", `\\?\UNC\server\share\x.exe`, `//server/share/x.exe`},
		{"plain unc retains slashes", `\\server\share\x.exe`, `//server/share/x.exe`},
		{"no drive passthrough", `/home/user/x`, `/home/user/x`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := windowsToWSLMntPath(c.in); got != c.want {
				t.Errorf("windowsToWSLMntPath(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

func TestWslEncodeArg(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		encoded bool // true => 应被 base64 包裹；false => 原样返回
	}{
		{"plain simple flag unchanged", "--verbose", false},
		{"plain path unchanged", "/usr/bin/claude", false},
		{"plain flag+value with equals unchanged", "model=claude-4", false},
		{"empty unchanged", "", false},
		{"json settings base64 wrapped", `{"hooks":{"PreToolUse":[{"matcher":"Bash"}]}}`, true},
		{"prompt with spaces base64 wrapped", "请回复 ok", true},
		{"single quote base64 wrapped", `it's`, true},
		{"backtick base64 wrapped", "`ls`", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wslEncodeArg(c.in)
			if !c.encoded {
				if got != c.in {
					t.Errorf("wslEncodeArg(%q) = %q; want %q (unchanged)", c.in, got, c.in)
				}
				return
			}
			// 断言包裹形态 $(echo <b64> | base64 -d)，并解码校验等值。
			if !strings.HasPrefix(got, "$(echo ") || !strings.HasSuffix(got, " | base64 -d)") {
				t.Fatalf("wslEncodeArg(%q) not base64-wrapped: %q", c.in, got)
			}
			b64 := strings.TrimSuffix(strings.TrimPrefix(got, "$(echo "), " | base64 -d)")
			decoded, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatalf("decode %q: %v", b64, err)
			}
			if string(decoded) != c.in {
				t.Errorf("wslEncodeArg roundtrip %q = %q; want %q", c.in, decoded, c.in)
			}
		})
	}
}

// TestWslEncodeArgRoundTrip 验证 base64 编码后的参数经解码可无损还原原始值，
// 模拟 wsl.exe 经 zsh 展开 $(echo <b64> | base64 -d) 后得到原参数字符串。
func TestWslEncodeArgRoundTrip(t *testing.T) {
	for _, in := range []string{
		`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"\"/mnt/d/x.exe\"","timeout":310}]}]}}`,
		"请回复：成功",
		"a b c",
		"path/with/slash",
		`it's & "quoted\ backtick` + "`",
	} {
		encoded := wslEncodeArg(in)
		if encoded == in {
			// 无特殊字符，未编码，直接相等。
			continue
		}
		// 断言形态 $(echo <b64> | base64 -d)。
		if !strings.HasPrefix(encoded, "$(echo ") || !strings.HasSuffix(encoded, " | base64 -d)") {
			t.Fatalf("unexpected encode form: %q", encoded)
		}
		b64 := strings.TrimSuffix(strings.TrimPrefix(encoded, "$(echo "), " | base64 -d)")
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("decode %q: %v", b64, err)
		}
		if string(decoded) != in {
			t.Errorf("roundtrip %q = %q; want %q", in, decoded, in)
		}
	}
}
