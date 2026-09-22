package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 语义版本比较 —— Claude Code 与 Codex 现在共用这一份。
//
// 原先它只存在于 codex_runner.go（parseCodexSemver / compareCodexSemver），
// 而 Claude 侧走的是 `latest != local` 的字符串不等判断。那是一个真缺陷：
// 装了比 npm latest 更新的预发布版时，字符串不等会把**降级**报成"有更新可用"，
// 用户在管理页点下去就把自己降了级。统一成 semver 之后两种工具口径一致。

// semverVersion 是解析后的语义版本。
type semverVersion struct {
	major int
	minor int
	patch int
	pre   []string
}

// updateAvailableFrom 报告远端版本是否高于本地版本。
//
// 只有"严格更高"才算有更新：相同、更低（例如本地是预发布版而 registry 上是正式版）、
// 以及无法解析，都不算。无法解析时返回错误而不是 false —— 调用方需要把"问不到"
// 与"确实没有新版"分开显示，这是本项目对"空列表三态"的同一条要求。
func updateAvailableFrom(local, latest string) (bool, error) {
	localVersion, err := parseSemver(local)
	if err != nil {
		return false, fmt.Errorf("parse local version %q: %w", local, err)
	}
	latestVersion, err := parseSemver(latest)
	if err != nil {
		return false, fmt.Errorf("parse latest version %q: %w", latest, err)
	}
	return compareSemver(latestVersion, localVersion) > 0, nil
}

// parseSemver 解析 `MAJOR.MINOR.PATCH[-prerelease][+build]`，允许前导 `v`。
//
// 严格而不是宽容：`1.2` 这种两段式会被拒。宽松解析的代价是"把 1.2 当成 1.2.0"
// 之后，任何依赖段数的判据都会静默偏移。
func parseSemver(raw string) (semverVersion, error) {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	value, _, _ = strings.Cut(value, "+")
	core, prerelease, hasPrerelease := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
	}
	parsed := semverVersion{}
	for index, target := range []*int{&parsed.major, &parsed.minor, &parsed.patch} {
		if parts[index] == "" || (len(parts[index]) > 1 && parts[index][0] == '0') {
			return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		value, err := strconv.Atoi(parts[index])
		if err != nil || value < 0 {
			return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		*target = value
	}
	if !hasPrerelease {
		return parsed, nil
	}
	if prerelease == "" {
		return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
	}
	for _, identifier := range strings.Split(prerelease, ".") {
		if identifier == "" {
			return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		for _, character := range identifier {
			if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '-') {
				return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
			}
		}
		if _, err := strconv.Atoi(identifier); err == nil && len(identifier) > 1 && identifier[0] == '0' {
			return semverVersion{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		parsed.pre = append(parsed.pre, identifier)
	}
	return parsed, nil
}

// compareSemver 按 semver 规则比较：返回 -1 / 0 / 1。
//
// 预发布版的优先级低于同号正式版（1.0.0-rc.1 < 1.0.0）。
func compareSemver(left, right semverVersion) int {
	for _, pair := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.pre) == 0 && len(right.pre) > 0 {
		return 1
	}
	if len(left.pre) > 0 && len(right.pre) == 0 {
		return -1
	}
	for index := 0; index < len(left.pre) && index < len(right.pre); index++ {
		leftNumber, leftErr := strconv.Atoi(left.pre[index])
		rightNumber, rightErr := strconv.Atoi(right.pre[index])
		if leftErr == nil && rightErr != nil {
			return -1
		}
		if leftErr != nil && rightErr == nil {
			return 1
		}
		if leftErr == nil && rightErr == nil {
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
			continue
		}
		if left.pre[index] < right.pre[index] {
			return -1
		}
		if left.pre[index] > right.pre[index] {
			return 1
		}
	}
	if len(left.pre) < len(right.pre) {
		return -1
	}
	if len(left.pre) > len(right.pre) {
		return 1
	}
	return 0
}

// runtimeMeetsMinimum 报告实测的运行时版本是否满足目录声明的最低版本。
//
// 满足不了时返回 false 与可读原因，而不是静默放行：放行的结果是用户在
// Node 16 上装完 CLI、点开对话才失败，报错完全指不到"运行时太旧"。
func runtimeMeetsMinimum(actual, minimum string) (bool, error) {
	if strings.TrimSpace(minimum) == "" {
		return true, nil
	}
	actualVersion, err := parseSemver(actual)
	if err != nil {
		return false, fmt.Errorf("parse runtime version %q: %w", actual, err)
	}
	minimumVersion, err := parseSemver(minimum)
	if err != nil {
		return false, fmt.Errorf("parse required runtime version %q: %w", minimum, err)
	}
	return compareSemver(actualVersion, minimumVersion) >= 0, nil
}

// versionTokenPattern 在一段命令输出里定位版本号。
//
// 允许预发布/构建后缀（`2.1.266-beta.1`、`1.0.0+20260101`），因为更新判断要能区分它们。
var versionTokenPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.]+)?`)

// agentVersionFromOutput 从 `--version` 这类输出里取出纯版本号。
//
// ⚠️ 这件事**只能有一处实现**，而且判据必须落在"版本号本身"上，不能是"给每个工具配一个
// 要剥掉的前缀/后缀字符串"。理由不是洁癖，是真实发生过的事故：
//
//	Claude 的输出是 `2.1.266 (Claude Code)` —— 产品名在**后**；
//	Codex 的输出是 `codex-cli 0.155.1`    —— 产品名在**前**。
//
// 目录里原先只有一个 VersionTrim 字段、并且按 TrimSuffix 消费，于是它对 Codex 静默失效：
// 探测（界面）显示 `0.155.1`，而登记表与审计里存的是 `codex-cli 0.155.1` —— 同一个版本
// 在两处不一致，而界面看不出任何异常。2026-09-21 由真实端到端用例（real_e2e_test.go）
// 装了一次真 Codex 才抓出来：它逃过了当时全部单测，因为那些用的都是编造的输出。
//
// 取版本号本身与产品名在前在后无关。整段都找不到版本号时**原样返回**（调用方会把它
// 当"读不出可用版本"处理），而不是编一个值出来。
func agentVersionFromOutput(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if match := versionTokenPattern.FindString(trimmed); match != "" {
		return match
	}
	return trimmed
}
