package app

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// 本文件实现 Windows 桌面端对 WSL 的探测与路径转换。服务端运行于 Windows 时，
// wsl-local runner 的发行版发现、home 探测以及 UNC 路径（\\wsl$\... 或
// \\wsl.localhost\...）与 WSL 内 Linux 路径之间的互转都在此完成。
//
// 所有探测函数在无 WSL（wsl.exe 不存在或执行失败）时返回明确错误，调用方据此
// 跳过 wsl-local runner 注册，绝不影响 windows-local。本文件不加 build tag：
// Linux 上 exec.LookPath("wsl.exe") 自然失败，函数内 OS 无关，CI 可编译。

// wslExePath 返回 wsl.exe 的绝对路径；找不到时返回错误。
func wslExePath() (string, error) {
	return exec.LookPath("wsl.exe")
}

// detectDefaultWSLDistro 返回 WSL 默认发行版名（如 "Ubuntu"）。
// wsl.exe --list --quiet 的输出为 UTF-16LE（通常带 BOM），需解码后取第一个非空行。
// ctx 无截止时间时兜底 5s 超时，避免 WSL 互操作挂起阻塞启动。
func detectDefaultWSLDistro(ctx context.Context) (string, error) {
	wslPath, err := wslExePath()
	if err != nil {
		return "", errors.New("未找到 wsl.exe（可能未安装 WSL）")
	}
	probeCtx := ctx
	if _, hasDeadline := probeCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(probeCtx, 5*time.Second)
		defer cancel()
	}
	out, err := exec.CommandContext(probeCtx, wslPath, "--list", "--quiet").Output()
	if err != nil {
		return "", err
	}
	for _, name := range strings.Split(decodeUTF16LE(out), "\n") {
		name = strings.TrimSpace(name)
		if name != "" {
			return name, nil
		}
	}
	return "", errors.New("wsl.exe 未返回任何发行版")
}

// detectWSLHome 返回指定发行版内当前用户的 Linux home 路径（如 "/home/user"）。
// wsl.exe -e sh -c '...' 的 stdout 为 UTF-8。
func detectWSLHome(ctx context.Context, distro string) (string, error) {
	wslPath, err := wslExePath()
	if err != nil {
		return "", err
	}
	probeCtx := ctx
	if _, hasDeadline := probeCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(probeCtx, 5*time.Second)
		defer cancel()
	}
	out, err := exec.CommandContext(probeCtx, wslPath, "-d", distro, "-e", "sh", "-c", "echo $HOME").Output()
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", errors.New("WSL home 探测为空")
	}
	return home, nil
}

// decodeUTF16LE 把 UTF-16LE 字节流（可能带 BOM）解码为 UTF-8 字符串。
// wsl.exe 的 --list / --status 输出即此编码。
func decodeUTF16LE(b []byte) string {
	// 去除 UTF-16 LE BOM（FF FE）。
	b = bytes.TrimPrefix(b, []byte{0xFF, 0xFE})
	if len(b)%2 != 0 {
		// 长度奇数：丢弃末尾半字，避免解码越界。
		b = b[:len(b)-1]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

// uncToWslPath 把 WSL 的 UNC 路径（\\wsl$\Ubuntu\home\user 或 \\wsl.localhost\Ubuntu\...）
// 转换为 WSL 内的 Linux 路径（/home/user）。非 WSL UNC 前缀返回 ("", false)。
// distro 用于校验 UNC 中的发行段与探测到的默认发行版一致。
func uncToWslPath(uncPath, distro string) (string, bool) {
	var rest string
	matched := false
	for _, prefix := range []string{`\\wsl$\`, `\\wsl.localhost\`} {
		if strings.HasPrefix(uncPath, prefix) {
			rest = uncPath[len(prefix):]
			matched = true
			break
		}
		// 大小写不敏感匹配前缀（\\WSL$\... 也接受）。
		if len(uncPath) >= len(prefix) && strings.EqualFold(uncPath[:len(prefix)], prefix) {
			rest = uncPath[len(prefix):]
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	// rest 形如 "Ubuntu\home\user"；剥去发行段。
	sep := strings.IndexAny(rest, `/\`)
	if sep < 0 {
		// 仅 \\wsl$\Ubuntu，根视角，返回 "/"。
		return "/", true
	}
	distroSeg := rest[:sep]
	if distro != "" && !strings.EqualFold(distroSeg, distro) {
		return "", false
	}
	linuxRest := strings.ReplaceAll(rest[sep:], `\`, `/`)
	linuxRest = strings.TrimPrefix(linuxRest, `/`)
	if linuxRest == "" {
		return "/", true
	}
	return `/` + linuxRest, true
}

// wslToUncPath 把 WSL 内 Linux 路径（/home/user）转换为 UNC 路径（\\wsl$\Ubuntu\home\user）。
func wslToUncPath(linuxPath, distro string) string {
	linuxPath = strings.TrimPrefix(strings.ReplaceAll(linuxPath, `/`, `\`), `\`)
	return `\\wsl$\` + distro + `\` + linuxPath
}

// wslUNCRoot 返回某发行版的 UNC 根（\\wsl$\Ubuntu），用于 roots 探测的基线。
func wslUNCRoot(distro string) string {
	return `\\wsl$\` + distro
}

// windowsToWSLMntPath 把 Windows 路径（C:\foo\bar.exe）转换为 WSL 内的 /mnt 互操作路径
// （/mnt/c/foo/bar.exe）。供 wslAgentRunner 把 Windows 侧的 approval hook 可执行文件
// 暴露给 WSL 内 claude 的 sh 调用。非 Windows 盘符路径原样返回。
//
// Windows 桌面端传入的 approval hook 可执行路径可能是 \\?\ 扩展长度前缀形式
// （\\?\D:\...\milevia-approval.exe，见 main.rs 的 --approval-hook 参数）。这类前缀让
// 盘符判断 winPath[1]==':' 失效，直接经 ReplaceAll 会产出 /?/D:/... 的畸形路径，且
// //?/ 会被 WSL 内 zsh 当成 glob 模式（"no matches found"），导致 claude 审批 hook 无法
// 启动、整个会话失败。故先剥离 \\?\（含 \\?\UNC\）前缀，再做盘符转换。
func windowsToWSLMntPath(winPath string) string {
	p := winPath
	// 剥离 Windows 扩展长度路径前缀：
	//   \\?\D:\...   -> D:\...        （盘符形式）
	//   \\?\UNC\host\share\... -> \\host\share\... （UNC 形式，保持 \\ 让后续原样返回）
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		p = `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`):
		p = p[len(`\\?\`):]
	}
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(string(p[0]))
		rest := strings.ReplaceAll(p[2:], `\`, `/`)
		rest = strings.TrimPrefix(rest, `/`)
		if rest == "" {
			return `/mnt/` + drive
		}
		return `/mnt/` + drive + `/` + rest
	}
	return strings.ReplaceAll(p, `\`, `/`)
}

// 本段为 WSL UNC 路径的"字符串前缀校验"基础设施。
//
// 背景：WSL 的项目路径以 UNC（\\wsl$\Ubuntu\... 或 \\wsl.localhost\Ubuntu\...）形式
// 存储并复用 LocalFilesystem / allowedPathForRunner 做范围校验。但这些校验函数依赖
// filepath.EvalSymlinks，而实测 EvalSymlinks 对 UNC（9P 文件系统）不可靠——Windows API
// 报 "The system cannot find" 而失败，导致 WSL 项目无法浏览/创建。因此对 WSL UNC 路径
// 改用具前缀关系的纯字符串匹配，不触发文件系统解析。

// isWSLUncPath 报告路径是否为 WSL UNC（\\wsl$... / \\wsl.localhost... 或正斜杠变体）。
func isWSLUncPath(p string) bool {
	return wslUncNormalize(p) != ""
}

// wslUncNormalize 把 WSL UNC 路径归一为规范反斜杠形式（\\wsl$\Ubuntu\home\... 或
// \\wsl.localhost\Ubuntu\...）。非 WSL UNC 路径返回空串。斜杠方向不敏感，不解析文件系统。
func wslUncNormalize(p string) string {
	// 先统一为反斜杠；//wsl$、/wsl$ 等正斜杠变体都可被识别。
	bs := strings.ReplaceAll(p, `/`, `\`)
	lower := strings.ToLower(bs)
	// 剥离可能的双反斜杠中的冗余（\\wsl$\ 与 \\\\wsl$\ 都收敛到两反斜杠）。
	for !strings.HasPrefix(lower, `\\wsl$\`) && !strings.HasPrefix(lower, `\\wsl.localhost\`) && strings.HasPrefix(lower, `\\`) {
		bs = bs[1:]
		lower = lower[1:]
	}
	prefix := ""
	if strings.HasPrefix(lower, `\\wsl.localhost\`) {
		prefix = `\\wsl.localhost\`
	} else if strings.HasPrefix(lower, `\\wsl$\`) {
		prefix = `\\wsl$\`
	} else {
		return ""
	}
	// 保留前缀 + 后续路径（发行段 + 内容），去尾部分隔符。
	rest := strings.TrimRight(bs[len(prefix):], `\`)
	if rest == "" {
		return strings.TrimRight(prefix, `\`) // 仅 \\wsl$\Ubuntu（发行根）
	}
	return prefix + rest
}

// wslPathWithin 判断 candidate 是否位于 root 之下（含相等），用字符串关系而非 EvalSymlinks。
// root 与 candidate 均须为 WSL UNC；先归一再 filepath.Clean（抑制 .. / . 段）后比较前缀，
// 防止 ".." 目录穿越逃逸出 root。任一非 WSL UNC 返回 false。
func wslPathWithin(root, candidate string) bool {
	nr := wslUncNormalize(root)
	nc := wslUncNormalize(candidate)
	if nr == "" || nc == "" {
		return false
	}
	// 用 Clean 归一 .. / . 段（含 root 自身），杜绝 .. 穿越。
	nrC := filepath.Clean(nr)
	ncC := filepath.Clean(nc)
	// 大小写不敏感（发行段与路径段）。
	nrL, ncL := strings.ToLower(nrC), strings.ToLower(ncC)
	if ncL == nrL {
		return true
	}
	return strings.HasPrefix(ncL, nrL+`\`)
}

// wslJoin joinUnc 把相对子路径安全地追加到 UNC root，返回规范 UNC 形式。
// 拒绝绝对/上层越界片段（".."/绝对路径），防路径逃逸。
func wslJoinUnc(root, child string) (string, bool) {
	nr := wslUncNormalize(root)
	if nr == "" || child == "" {
		return "", false
	}
	clean := filepath.Clean(strings.ReplaceAll(child, `/`, `\`))
	if clean == "" || clean == "." || clean == `\` || filepath.IsAbs(clean) || strings.HasPrefix(clean, `\`) {
		return "", false
	}
	// 防止 ".." 片段逃逸。
	if clean == ".." || strings.HasPrefix(clean, `..\`) {
		return "", false
	}
	return wslUncNormalize(nr + `\` + clean), true
}
