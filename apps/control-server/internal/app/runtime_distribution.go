package app

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// Node.js 托管运行时的分发包选择。
//
// 这一层刻意做成纯函数（只吃字符串、只吐结构体），因为它的判据全部来自
// 官方那两个文件的**真实形状**，而那两个形状会变：
//
//   - `dist/index.json`：`version` 带前导 v；`lts` 是 `false` 或代号字符串
//     （"Krypton" / "Jod" / "Iron" …），不是布尔；`files` 是逻辑键数组
//     （`linux-x64`、`win-x64-zip`、`linux-x64-musl` …）。
//   - `dist/vX/SHASUMS256.txt`：每行 `<sha256>  <文件名>`（两个空格）。
//
// ⚠️ **逻辑键与文件名并不一致**：`linux-x64` 对应 `node-vX-linux-x64.tar.gz`，
// 而 `win-x64-zip` 对应 `node-vX-win-x64.zip`。所以这里**不自己拼文件名**，
// 而是拿 SHASUMS 当权威清单按后缀挑 —— 拼名字迟早会拼错，而且拼错了要到
// 下载 404 时才发现。

// NodeLibc 是目标环境的 C 运行库（决定选 glibc 还是 musl 构建）。
type NodeLibc string

const (
	NodeLibcGlibc NodeLibc = "glibc"
	NodeLibcMusl  NodeLibc = "musl"
)

// NodePlatformKey 是官方分发包的平台逻辑键（如 "linux-x64"、"win-arm64"）。
type NodePlatformKey string

// nodePlatformKey 把 (GOOS, GOARCH, libc) 映射成官方的平台键。
//
// 为什么 libc 必须进判据：Alpine / musl 环境上装 glibc 构建会在**运行时**报
// `not found`，而用户拿到的是一句"安装成功"。官方为 x64 musl 单独提供了
// `linux-x64-musl`（较老版本没有，那种情况由上层如实拒绝）。
func nodePlatformKey(goos, goarch string, libc NodeLibc) (NodePlatformKey, error) {
	switch goos {
	case "windows":
		switch goarch {
		case "amd64":
			return "win-x64", nil
		case "arm64":
			return "win-arm64", nil
		}
		return "", fmt.Errorf("Windows %s 暂无官方分发包", goarch)
	case "linux":
		switch goarch {
		case "amd64":
			if libc == NodeLibcMusl {
				return "linux-x64-musl", nil
			}
			return "linux-x64", nil
		case "arm64":
			if libc == NodeLibcMusl {
				// 官方目前没有 arm64 musl 构建；不要静默退回 glibc。
				return "", fmt.Errorf("Linux arm64 + musl 暂无官方分发包")
			}
			return "linux-arm64", nil
		}
		return "", fmt.Errorf("Linux %s 暂无官方分发包", goarch)
	}
	return "", fmt.Errorf("不支持在该平台安装 Node.js 运行时：%s", goos)
}

// localNodeLibc 给出本机 C 运行库。仅用于本机安装；跨端环境由目标环境自己报。
func localNodeLibc() NodeLibc {
	if runtime.GOOS != "linux" {
		return NodeLibcGlibc
	}
	// Alpine 与其他 musl 发行版都有这个文件；它存在即说明是 musl。
	if fileExists("/etc/alpine-release") {
		return NodeLibcMusl
	}
	return NodeLibcGlibc
}

// nodeArchiveFormat 从文件名推导压缩格式。解压是纯 Go 实现，因此目标环境
// **不需要**额外装解压工具 —— 这也消掉了"远端缺 tar/xz"这一类失败。
type nodeArchiveFormat string

const (
	nodeArchiveTarGz nodeArchiveFormat = "tar.gz"
	nodeArchiveZip   nodeArchiveFormat = "zip"
)

// runtimeDistribution 是一个可下载的具体分发包。
type runtimeDistribution struct {
	// Version 不带前导 v（"24.21.0"）。
	Version string `json:"version"`
	// Platform 是官方平台键。
	Platform NodePlatformKey `json:"platform"`
	// FileName 来自 SHASUMS256.txt，是权威文件名。
	FileName string            `json:"fileName"`
	Format   nodeArchiveFormat `json:"format"`
	SHA256   string            `json:"sha256"`
	URL      string            `json:"url"`
}

// parseNodeChecksums 解析 SHASUMS256.txt。
//
// 格式是 `<sha256>  <filename>`（两个空格，但按任意空白切更稳）。
// 行内还可能出现 `win-x64/node.exe` 这种带目录的条目，一并保留 —— 文件名匹配
// 用的是后缀，带斜杠的条目不会误命中。
func parseNodeChecksums(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("无法解析校验和行：%q", line)
		}
		sum, name := fields[0], fields[1]
		if len(sum) != 64 {
			return nil, fmt.Errorf("校验和长度不是 64：%q", line)
		}
		out[name] = sum
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("校验和清单是空的")
	}
	return out, nil
}

// archiveSuffix 给出该平台在官方清单里的文件名后缀。
//
// 这里体现的就是"逻辑键 ≠ 文件名"：Linux 用 `<key>.tar.gz`，Windows 用
// `<key>.zip`，而 Windows 的逻辑键本身带着 `-zip` 后缀。
func archiveSuffix(platform NodePlatformKey) (suffix string, format nodeArchiveFormat, err error) {
	switch {
	case strings.HasPrefix(string(platform), "win-"):
		return string(platform) + ".zip", nodeArchiveZip, nil
	case strings.HasPrefix(string(platform), "linux-"):
		// 用 .tar.gz 而不是 .tar.xz：gzip 在目标环境里的可得性远高于 xz
		// （busybox tar 支持 -z 但常常没有 -J）。虽然本实现是纯 Go 解压，
		// 但这条对将来"在远端直接解压"的路径同样重要。
		return string(platform) + ".tar.gz", nodeArchiveTarGz, nil
	}
	return "", "", fmt.Errorf("未知平台键 %q", platform)
}

// selectNodeDistribution 从校验和清单里挑出目标平台的分发包。
//
// 用**后缀匹配**而不是拼名字：`-linux-x64.tar.gz` 不是
// `-linux-x64-musl.tar.gz` 的后缀，所以两种 libc 不会互相误命中。
func selectNodeDistribution(checksums map[string]string, version string, platform NodePlatformKey, baseURL string) (runtimeDistribution, error) {
	suffix, format, err := archiveSuffix(platform)
	if err != nil {
		return runtimeDistribution{}, err
	}
	want := "-" + suffix
	candidates := make([]string, 0, 2)
	for name := range checksums {
		if strings.HasPrefix(name, "node-v"+version+"-") && strings.HasSuffix(name, want) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return runtimeDistribution{}, fmt.Errorf("该版本没有 %s 的官方分发包（版本过老或平台键有误）", platform)
	}
	// 稳定排序：正常情况下只有一个候选，排序只是为了结果不随 map 遍历顺序变化。
	sort.Strings(candidates)
	name := candidates[0]
	return runtimeDistribution{
		Version:  version,
		Platform: platform,
		FileName: name,
		Format:   format,
		SHA256:   strings.ToLower(checksums[name]),
		URL:      strings.TrimRight(baseURL, "/") + "/v" + version + "/" + name,
	}, nil
}

// nodeVersionEntry 是 dist/index.json 里的一条。
//
// LTS 字段用 any 接：它是 `false` **或**代号字符串（官方把两种形态混在一个字段里），
// 用 bool 接会在遇到代号时整条解析失败，用 string 接会丢掉 `false`。
type nodeVersionEntry struct {
	Version string   `json:"version"`
	LTS     any      `json:"lts"`
	Date    string   `json:"date"`
	Files   []string `json:"files"`
}

// nodeLTSName 返回该版本的 LTS 代号；不是 LTS 时返回空串。
func (e nodeVersionEntry) nodeLTSName() string {
	name, ok := e.LTS.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// nodeVersion 去掉前导 v。
func (e nodeVersionEntry) nodeVersion() string {
	return strings.TrimPrefix(strings.TrimSpace(e.Version), "v")
}

// parseNodeVersionIndex 解析 dist/index.json。
func parseNodeVersionIndex(raw []byte) ([]nodeVersionEntry, error) {
	var entries []nodeVersionEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("解析 Node 版本索引失败：%w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("Node 版本索引是空的")
	}
	return entries, nil
}

// pickNodeVersion 按 selector 选一个版本号（不带 v）。
//
// selector："lts" 取最新 LTS；具体版本号（"24.21.0" / "v24.21.0"）原样使用；
// 空串等同 "lts"。选不到时报错而不是悄悄回落到别的版本 —— 用户点的是 LTS，
// 给一个非 LTS 是另一种"说成了别的东西"。
func pickNodeVersion(entries []nodeVersionEntry, selector string) (nodeVersionEntry, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" || strings.EqualFold(selector, "lts") {
		for _, entry := range entries {
			if entry.nodeLTSName() != "" {
				return entry, nil
			}
		}
		return nodeVersionEntry{}, fmt.Errorf("版本索引里没有标记为 LTS 的版本")
	}
	want := strings.TrimPrefix(selector, "v")
	for _, entry := range entries {
		if entry.nodeVersion() == want {
			return entry, nil
		}
	}
	return nodeVersionEntry{}, fmt.Errorf("版本索引里没有 %s", selector)
}

// nodeVersionOption 是给界面用的候选版本。
type nodeVersionOption struct {
	Version string `json:"version"`
	LTS     string `json:"lts,omitempty"`
	Date    string `json:"date,omitempty"`
}

// nodeVersionOptions 从索引里挑出可选版本（最近若干个 LTS + 最近一个当前版）。
//
// 界面上"选哪个 Node"不该是一长串 200 个版本：只有 LTS 与最新当前版对用户有意义。
func nodeVersionOptions(entries []nodeVersionEntry, limit int) []nodeVersionOption {
	out := []nodeVersionOption{}
	seenLTS := map[string]bool{}
	for _, entry := range entries {
		name := entry.nodeLTSName()
		if name == "" || seenLTS[name] {
			continue
		}
		seenLTS[name] = true
		out = append(out, nodeVersionOption{Version: entry.nodeVersion(), LTS: name, Date: entry.Date})
		if len(out) >= limit {
			break
		}
	}
	return out
}
