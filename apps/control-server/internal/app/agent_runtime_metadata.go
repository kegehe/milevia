package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nodeMinimumEngine = regexp.MustCompile(`^>=\s*([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?$`)

// 安装预检针对默认待装版本，已安装工具的诊断优先读取其自己的包元数据。
// 尤其不能把当前 Claude 的 Node 22 要求套在仍兼容 Node 18 的历史包上。
func installedRuntimeEntry(entry AgentCatalogEntry, recorded agentInstallation) (AgentCatalogEntry, string) {
	entry.MinRuntimeVersion = ""
	unknown := fmt.Sprintf("未能读取 %s 已安装包的 Node.js 要求，未完成运行时兼容性检查", entry.Name)
	if recorded.Prefix == "" {
		return entry, unknown
	}
	install := agentNpmCLIInstall(entry)
	path := filepath.Join(install.packageRoot(recorded.Prefix), install.packageName, "package.json")
	f, err := os.Open(path)
	if err != nil {
		return entry, unknown
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, authOutputLimit+1))
	if err != nil || len(raw) > authOutputLimit {
		return entry, unknown
	}
	var pkg struct {
		Name    string `json:"name"`
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if json.Unmarshal(raw, &pkg) != nil || pkg.Name != entry.NpmPackage {
		return entry, unknown
	}
	if strings.TrimSpace(pkg.Engines.Node) == "" {
		return entry, "" // 包本身没有声明 Node 版本限制。
	}
	match := nodeMinimumEngine.FindStringSubmatch(strings.TrimSpace(pkg.Engines.Node))
	if len(match) == 4 {
		for i := 2; i < 4; i++ {
			if match[i] == "" {
				match[i] = "0"
			}
		}
		entry.MinRuntimeVersion = strings.Join(match[1:], ".")
	} else {
		// 不把未知/复杂 semver 区间臆断成当前目录最低版本；安装由 npm engine-strict 检查。
		return entry, fmt.Sprintf("%s 已安装包的 Node.js 要求包含暂不支持的版本区间，未完成兼容性检查", entry.Name)
	}
	return entry, ""
}
