package app

import (
	"regexp"
	"strings"
	"testing"
)

// TestRemoteCLICommandsPrefixManagedPath 钉住"远端跑 CLI 一律前置托管目录"这条纪律。
//
// 平台的托管 npm prefix 不在 SSH 非交互 shell 的 PATH 上（见 crossManagedPathValue
// 的注释）。漏了前缀，远端就会把"平台在它上面装好的那份"说成没装 —— 而且不止一处：
// 探测、SSH 预检、升级、MCP 注入各踩一次，每一处都是一句"远端没有这个工具"。
//
// 判据用**源码**而不是运行时：这些命令要在真 SSH 上才发得出去，而"有没有前缀"
// 是拼装问题，源码一眼可见（与项目里既有的接线断言同一手法）。
//
// ⚠️ 判据只覆盖"命令是字符串字面量"的那类调用点：命令由变量拼出来的（脚本、
// 变量）无法用源码判定，那几处由各自的脚本自己负责（例如
// crossRuntimeProbeScript 用绝对路径区分托管与系统 npm，故**故意**不加前缀）。
func TestRemoteCLICommandsPrefixManagedPath(t *testing.T) {
	cliNames := []string{"claude", "codex", "opencode", "qoder", "codebuddy", "cursor-agent"}
	// execCommand(ctx, "命令字面量") —— 第二个参数是字面量才可能是漏前缀的写法。
	literal := regexp.MustCompile(`execCommand\([^,]+,\s*"([^"]*)"`)
	for _, file := range []string{"ssh_runner.go", "ssh_connection.go", "mcp_client.go"} {
		code := stripGoComments(t, readGoSource(t, file))
		for _, match := range literal.FindAllStringSubmatch(code, -1) {
			command := match[1]
			for _, name := range cliNames {
				if strings.Contains(command, name) {
					t.Fatalf("%s 里有一条直接执行 CLI 的远端命令（漏了托管 PATH 前置）：%q", file, command)
				}
			}
		}
	}
}
