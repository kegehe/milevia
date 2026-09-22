package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 手机端一次要拿整棵子树，而不是一层一层要：手机到电脑只有中继通道，
// 逐层往返的代价是 0.5–1.5 秒/次（见 docs/40 的通道约束），一个中等项目展开到第三层
// 就是十几次往返。所以递归放在这里，把 N 次往压缩成一次。
//
// 递归刻意放在 handler 层、只用 Filesystem.ReadDir：LocalFilesystem 与 SFTPFilesystem
// 都已经有 ReadDir，用它们拼树就只有一份代码，也不必给两个实现各加一个 depth 参数。
// 这里**没有**"默认深度"这个常量：缺省是 1（保持桌面端的扁平响应），要几层由调用方
// 显式传。手机端的 3 是它自己的选择（见 apps/web 的 mobile-fs-request.ts），
// 服务端只在 fsTreeMaxDepth 上设一个上限 —— 写成服务端常量会让下一个读代码的人
// 以为"服务端默认就给 3 层"，而把它接进 fsTreeDepth 就会静默改掉桌面端的行为。
const (
	fsTreeMaxDepth = 5
	// fsTreeMaxPerDir 与 fsTreeMaxTotalEntry 是配额，不是"建议值"：它们决定一次响应
	// 能否装进中继通道（云端 Agent WebSocket 读上限 512 KiB，见 cloud-control 的
	// SetReadLimit）。超出的部分会置 truncated 并由手机端如实告知用户，绝不静默丢弃。
	fsTreeMaxPerDir     = 500
	fsTreeMaxTotalEntry = 3000
)

// fsTreeIgnoredDirNames 是批量取树时跳过的目录名。
//
// 必须在 Go 侧：这份名单若由前端携带，一台被改过的客户端就能让服务端去遍历
// node_modules —— 那既是几万条条目的性能问题，也是"谁来定义项目结构"的一致性问题。
//
// 只收**明确的依赖/构建产物**，不收编辑器配置（.vscode、.idea）与普通点文件：
// 那些是用户会主动去看的东西，误伤比多传几条严重得多。
//
// ⚠️ 这里的名字是**按小写匹配**的（文件系统大小写不敏感，而目录名的写法五花八门：
// macOS 上是 `Pods`，同族的 Gradle 目录可能是 `Pods`/`pods` 混用）。所以由下面那
// 个构造器统一转小写，**不要**再手写一个 map —— 曾经就是这么写的，而
// `"Pods"` / `"DerivedData"` 两个混合大小写的键与 `strings.ToLower(entry.Name)`
// 永远匹配不上：它们看起来在名单里，实际是两条死条目，谁也没被跳过。
var fsTreeIgnoredDirNames = []string{
	"node_modules",
	".git",
	".hg",
	".svn",
	"dist",
	"build",
	"target",
	".next",
	".nuxt",
	".turbo",
	".svelte-kit",
	"vendor",
	"__pycache__",
	".venv",
	".pytest_cache",
	".mypy_cache",
	".gradle",
	"pods",        // CocoaPods
	"deriveddata", // Xcode
}

// fsTreeIgnoredDirs 由名字表构造，键一定是小写 —— 这条不变式靠构造过程保证，
// 而不是靠"下次记得写小写"。
var fsTreeIgnoredDirs = func() map[string]bool {
	set := make(map[string]bool, len(fsTreeIgnoredDirNames))
	for _, name := range fsTreeIgnoredDirNames {
		set[strings.ToLower(name)] = true
	}
	return set
}()

// fsTreeBudget 是整棵树共享的条目预算。
//
// 跨层共享而不是每层各算一份：每层独立给 500 的话，五层深度下最坏情况是
// 500^5 条，配额等于没有。
type fsTreeBudget struct {
	remaining int
	// skipped 是因忽略名单跳过的目录数。它和 truncated 是两件事：
	// skipped 是预期行为（"隐藏了 3 个依赖目录"），truncated 是**这次没取全**
	// （"条目过多，只显示了前 N 条"）。合成一个布尔量就没法对用户说清是哪一种。
	skipped   int
	truncated bool
}

// fsTreeDepth 解析 depth 查询参数。
//
// 缺省即 0 表示"按老形状只取一层"：桌面端一直在用 /fs/tree 且不传 depth，
// 让它继续拿到扁平的 entries，行为逐字节不变。手机端显式传 depth（默认 3）。
func fsTreeDepth(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 1, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("depth 必须是数字")
	}
	if value < 1 {
		return 0, fmt.Errorf("depth 至少为 1")
	}
	if value > fsTreeMaxDepth {
		return 0, fmt.Errorf("depth 最多为 %d", fsTreeMaxDepth)
	}
	return value, nil
}

// readTree 递归读取一棵子树。
//
// 读不到的**子目录**不会让整棵树失败：权限不足、或者读到一半被删掉的目录都只影响
// 它自己，把它标成 unreadable 让手机端说"这个目录读不到"，比让整个文件视图报错更有用
// （一棵树里有几十个目录，一个坏的把全部拖下水不合理）。根目录读不到才返回错误。
func readTree(ctx context.Context, filesystem Filesystem, path string, depth int, budget *fsTreeBudget) ([]FileEntry, error) {
	entries, err := filesystem.ReadDir(ctx, path)
	if err != nil {
		return nil, err
	}
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir && fsTreeIgnoredDirs[strings.ToLower(entry.Name)] {
			budget.skipped++
			continue
		}
		if len(result) >= fsTreeMaxPerDir || budget.remaining <= 0 {
			budget.truncated = true
			break
		}
		budget.remaining--
		if entry.IsDir && depth > 1 {
			// 子路径由**父路径 + 名字**拼出来，而不是拿 entry.Path 再喂回 ReadDir。
			// 两个原因：
			//   · entry.Path 是 Filesystem 自己算出来的相对路径。本地实现在 projectPath
			//     与 EvalSymlinks 结果不一致时（项目路径里含符号链接/junction 就会出现）
			//     会算出带 `..` 前缀的路径 —— 再喂回去是拿它当"已验证的路径"用，而它其实
			//     只是展示用的字符串；
			//   · 父路径本来就是我们传进去的参数，它已经过一次校验了，在它后面接一个不含
			//     分隔符的名字是这里唯一需要保证的性质。
			children, childErr := readTree(ctx, filesystem, joinTreePath(path, entry.Name), depth-1, budget)
			if childErr != nil {
				entry.Unreadable = true
			} else {
				entry.Children = children
			}
		}
		result = append(result, entry)
	}
	return result, nil
}

// joinTreePath 拼接项目内的相对路径。
//
// 统一用 "/" 而不是 filepath.Join：项目内路径在这套 API 里一直是斜杠分隔的
// （FileEntry.Path 由 relativePath 产出，桌面的前端也按 "/" 解析），
// 而 filepath.Join 在 Windows 上会产出反斜杠，让同一个字段在两端含义不同。
func joinTreePath(parent, name string) string {
	if parent == "" || parent == "/" {
		return name
	}
	return strings.TrimSuffix(parent, "/") + "/" + name
}
