package app

import (
	"strings"
	"testing"
)

// 夹具取自**真实**的官方文件（2026-09-21 实际抓取），不是按印象编的：
// 这一层的判据全部是关于"官方长什么样"，凭印象写就等于把猜测固化成测试。
//
// 抓取时确认到的三件事实（与 docs/42 §7.4 的初版判断不同，已就地更正）：
//  1. Linux 同时提供 `.tar.gz` 与 `.tar.xz` —— 所以不必引入 xz 依赖，也能选
//     gzip（目标环境里的可得性远高于 xz）。
//  2. **musl 有官方构建**（`linux-x64-musl`），不必像初版那样直接拒绝。
//  3. 逻辑键与文件名不一致：`win-x64-zip` 逻辑键对应的文件是 `win-x64.zip`，
//     所以文件名必须从 SHASUMS 清单里挑，不能自己拼。
const realNodeChecksumsV24 = `3d63405fc65a0d2d2976c1f0bc2fd27bb0bd07212469e705aac3f03ae5ab4c9c  node-v24.21.0-linux-x64-musl.tar.gz
34ab095af8efe9018f21489d3dc19871fb5edd4f130a71469b69c05fd4e32a5e  node-v24.21.0-linux-x64-musl.tar.xz
6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff  node-v24.21.0-linux-x64.tar.gz
fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6  node-v24.21.0-linux-x64.tar.xz
724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5  node-v24.21.0-linux-arm64.tar.gz
6ad1325edbdb5649c379b75a237147a666c95d4f9ae8d340fef2d1575d289ad2  node-v24.21.0-linux-arm64.tar.xz
8779b1bde1d39f8d420e3b57aa657b39891af434d3de44a919044cec06785921  node-v24.21.0-win-arm64.zip
4e5f86609712acf841f3a5e2d9d4854b4c149f6ead024fc38381b6d7b272c636  node-v24.21.0-win-x64.7z
158f7685b44de51f6c0df1d153526cbcd3e1bc739a8dfc607721cef75de9e541  node-v24.21.0-win-x64.zip
ba4e6d110e8c1592a1ecd390f6b05f3da124b13871a5be62b341a07a853c6c32  win-x64/node.exe
`

// realNodeVersionIndexV24 是 dist/index.json 的形状（截取到本用例需要的字段）。
const realNodeVersionIndexV24 = `[
  {"version":"v26.9.0","date":"2026-09-16","lts":false,"files":["linux-x64","linux-arm64","linux-x64-musl","win-x64-zip","win-arm64-zip"]},
  {"version":"v24.21.0","date":"2026-08-20","lts":"Krypton","files":["linux-x64","linux-arm64","linux-x64-musl","win-x64-zip","win-arm64-zip"]},
  {"version":"v22.23.2","date":"2026-07-15","lts":"Jod","files":["linux-x64","linux-arm64","win-x64-zip"]},
  {"version":"v20.20.2","date":"2026-06-10","lts":"Iron","files":["linux-x64","linux-arm64","win-x64-zip"]}
]`

func TestNodePlatformKeyMapsLibc(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		goarch  string
		libc    NodeLibc
		want    NodePlatformKey
		wantErr bool
	}{
		{name: "windows amd64", goos: "windows", goarch: "amd64", want: "win-x64"},
		{name: "windows arm64", goos: "windows", goarch: "arm64", want: "win-arm64"},
		{name: "linux glibc amd64", goos: "linux", goarch: "amd64", libc: NodeLibcGlibc, want: "linux-x64"},
		{name: "linux musl amd64", goos: "linux", goarch: "amd64", libc: NodeLibcMusl, want: "linux-x64-musl"},
		{name: "linux glibc arm64", goos: "linux", goarch: "arm64", libc: NodeLibcGlibc, want: "linux-arm64"},
		// 官方没有 arm64 musl 构建：必须如实拒绝，**不能**静默退回 glibc ——
		// 那会在 Alpine arm64 上装出一个运行时才报 not found 的 Node。
		{name: "linux arm64 musl unsupported", goos: "linux", goarch: "arm64", libc: NodeLibcMusl, wantErr: true},
		{name: "unknown arch", goos: "linux", goarch: "riscv64", wantErr: true},
		{name: "unknown os", goos: "darwin", goarch: "arm64", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := nodePlatformKey(test.goos, test.goarch, test.libc)
			if test.wantErr {
				if err == nil {
					t.Fatalf("期望报错，却得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错：%v", err)
			}
			if got != test.want {
				t.Fatalf("平台键 = %q，期望 %q", got, test.want)
			}
		})
	}
}

func TestParseNodeChecksumsRejectsMalformed(t *testing.T) {
	if _, err := parseNodeChecksums("not a checksum line\n"); err == nil {
		t.Fatal("字段数不对的行应当被拒")
	}
	if _, err := parseNodeChecksums("abcd  node-v1.0.0-linux-x64.tar.gz\n"); err == nil {
		t.Fatal("校验和长度不是 64 的行应当被拒")
	}
	if _, err := parseNodeChecksums("\n# 只有注释\n"); err == nil {
		t.Fatal("空清单应当被拒，而不是返回一个空 map 让上层以为'这个版本没有分发包'")
	}
}

func TestSelectNodeDistributionUsesOfficialFileNames(t *testing.T) {
	checksums, err := parseNodeChecksums(realNodeChecksumsV24)
	if err != nil {
		t.Fatalf("夹具解析失败：%v", err)
	}
	tests := []struct {
		platform NodePlatformKey
		wantName string
		wantSum  string
		wantFmt  nodeArchiveFormat
	}{
		{"linux-x64", "node-v24.21.0-linux-x64.tar.gz", "6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff", nodeArchiveTarGz},
		{"linux-arm64", "node-v24.21.0-linux-arm64.tar.gz", "724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5", nodeArchiveTarGz},
		{"linux-x64-musl", "node-v24.21.0-linux-x64-musl.tar.gz", "3d63405fc65a0d2d2976c1f0bc2fd27bb0bd07212469e705aac3f03ae5ab4c9c", nodeArchiveTarGz},
		{"win-x64", "node-v24.21.0-win-x64.zip", "158f7685b44de51f6c0df1d153526cbcd3e1bc739a8dfc607721cef75de9e541", nodeArchiveZip},
		{"win-arm64", "node-v24.21.0-win-arm64.zip", "8779b1bde1d39f8d420e3b57aa657b39891af434d3de44a919044cec06785921", nodeArchiveZip},
	}
	for _, test := range tests {
		t.Run(string(test.platform), func(t *testing.T) {
			got, err := selectNodeDistribution(checksums, "24.21.0", test.platform, "https://nodejs.org/dist")
			if err != nil {
				t.Fatalf("选择失败：%v", err)
			}
			if got.FileName != test.wantName {
				t.Fatalf("文件名 = %q，期望 %q", got.FileName, test.wantName)
			}
			if got.SHA256 != test.wantSum {
				t.Fatalf("校验和 = %q，期望 %q", got.SHA256, test.wantSum)
			}
			if got.Format != test.wantFmt {
				t.Fatalf("格式 = %q，期望 %q", got.Format, test.wantFmt)
			}
			want := "https://nodejs.org/dist/v24.21.0/" + test.wantName
			if got.URL != want {
				t.Fatalf("下载地址 = %q，期望 %q", got.URL, want)
			}
			// 不允许挑到 .tar.xz 或 .7z：它们是同一平台的另一种压缩，装错了
			// 表现为"解压失败"，而报错完全指不到"选错了包"。
			if strings.HasSuffix(got.FileName, ".tar.xz") || strings.HasSuffix(got.FileName, ".7z") {
				t.Fatalf("挑到了不该选的压缩格式：%s", got.FileName)
			}
		})
	}
}

// TestSelectNodeDistributionDoesNotConfuseMuslWithGlibc 是这一组里最要紧的判别用例。
//
// glibc 与 musl 的两个包只差一个 `-musl` 中缀，选错的表现是"装成功、跑不起来"。
func TestSelectNodeDistributionDoesNotConfuseMuslWithGlibc(t *testing.T) {
	checksums, err := parseNodeChecksums(realNodeChecksumsV24)
	if err != nil {
		t.Fatal(err)
	}
	glibc, err := selectNodeDistribution(checksums, "24.21.0", "linux-x64", "https://nodejs.org/dist")
	if err != nil {
		t.Fatal(err)
	}
	musl, err := selectNodeDistribution(checksums, "24.21.0", "linux-x64-musl", "https://nodejs.org/dist")
	if err != nil {
		t.Fatal(err)
	}
	if glibc.FileName == musl.FileName {
		t.Fatalf("glibc 与 musl 挑到了同一个包：%s", glibc.FileName)
	}
	if strings.Contains(glibc.FileName, "musl") {
		t.Fatalf("glibc 环境挑到了 musl 包：%s", glibc.FileName)
	}
	if !strings.Contains(musl.FileName, "musl") {
		t.Fatalf("musl 环境没挑到 musl 包：%s", musl.FileName)
	}
	if glibc.SHA256 == musl.SHA256 {
		t.Fatal("两个包的校验和相同，说明夹具或匹配有误")
	}
}

// TestSelectNodeDistributionReportsMissingPlatform 覆盖"该版本没有这个平台的分发包"。
//
// 典型场景：较老的版本没有 `linux-x64-musl`。这时必须如实说"这个版本没有"，
// 而不是回落到 glibc 包。
func TestSelectNodeDistributionReportsMissingPlatform(t *testing.T) {
	checksums, err := parseNodeChecksums(realNodeChecksumsV24)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selectNodeDistribution(checksums, "24.21.0", "linux-s390x", "https://nodejs.org/dist"); err == nil {
		t.Fatal("该版本没有的分发包应当报错")
	}
	// 版本号对不上（清单里只有 v24.21.0）也不能挑到别的版本。
	if _, err := selectNodeDistribution(checksums, "20.20.2", "linux-x64", "https://nodejs.org/dist"); err == nil {
		t.Fatal("清单里没有的版本应当报错，而不是拿相近版本顶上")
	}
}

func TestPickNodeVersionPrefersLatestLTS(t *testing.T) {
	entries, err := parseNodeVersionIndex([]byte(realNodeVersionIndexV24))
	if err != nil {
		t.Fatalf("夹具解析失败：%v", err)
	}

	// 空 selector 与 "lts" 都取**最新** LTS：索引按新→旧排，第一条 lts 非 false 的就是它。
	for _, selector := range []string{"", "lts", "LTS"} {
		picked, err := pickNodeVersion(entries, selector)
		if err != nil {
			t.Fatalf("selector=%q 报错：%v", selector, err)
		}
		if picked.nodeVersion() != "24.21.0" {
			t.Fatalf("selector=%q 选到 %s，期望最新 LTS 24.21.0（v26.9.0 的 lts 是 false）", selector, picked.nodeVersion())
		}
		if picked.nodeLTSName() != "Krypton" {
			t.Fatalf("LTS 代号 = %q，期望 Krypton", picked.nodeLTSName())
		}
	}

	// 指定版本（带不带 v 都要认）。
	for _, selector := range []string{"22.23.2", "v22.23.2"} {
		picked, err := pickNodeVersion(entries, selector)
		if err != nil {
			t.Fatalf("selector=%q 报错：%v", selector, err)
		}
		if picked.nodeVersion() != "22.23.2" {
			t.Fatalf("selector=%q 选到 %s", selector, picked.nodeVersion())
		}
	}

	// 不存在的版本如实报错，**不回落**到别的版本。
	if _, err := pickNodeVersion(entries, "18.0.0"); err == nil {
		t.Fatal("索引里没有的版本应当报错，而不是回落")
	}
}

func TestNodeVersionOptionsListsLTSTracks(t *testing.T) {
	entries, err := parseNodeVersionIndex([]byte(realNodeVersionIndexV24))
	if err != nil {
		t.Fatal(err)
	}
	options := nodeVersionOptions(entries, 3)
	if len(options) != 3 {
		t.Fatalf("候选数 = %d，期望 3", len(options))
	}
	// 每个 LTS 线只出现一次（同线有多个补丁版时不该刷屏）。
	seen := map[string]bool{}
	for _, option := range options {
		if option.LTS == "" {
			t.Fatalf("候选里出现了非 LTS 版本：%#v", option)
		}
		if seen[option.LTS] {
			t.Fatalf("LTS 线 %s 出现了多次", option.LTS)
		}
		seen[option.LTS] = true
	}
	if options[0].Version != "24.21.0" || options[1].Version != "22.23.2" {
		t.Fatalf("候选顺序不对：%#v", options)
	}
}

// TestParseNodeVersionIndexRejectsEmpty 保证"读不到"不会被当成"没有可用版本"。
func TestParseNodeVersionIndexRejectsEmpty(t *testing.T) {
	if _, err := parseNodeVersionIndex([]byte(`[]`)); err == nil {
		t.Fatal("空索引应当报错")
	}
	if _, err := parseNodeVersionIndex([]byte(`{`)); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}
