package app

import (
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 本文件集中处理「子进程输出编码」这一个问题。
//
// 中文 Windows 下，python/cmd 等进程在 stdout/stderr 被重定向到管道时，会按系统 ANSI
// 代码页（CP936/GBK）编码中文——因为此时它们不再有控制台，取的是 locale 编码而非 UTF-8。
// 服务端统一按 UTF-8 读取，于是一旦这些字节进入 encoding/json，Go 会把非法 UTF-8 字节
// 静默替换成 U+FFFD，中文就此不可逆地丢失（连原始字节都还原不回来）。
//
// 两道防线：
//
//  1. 治本——utf8ChildEnv/appendUTF8ChildEnv：给拉起的 agent CLI 注入 UTF-8 输出环境
//     变量。agent 派生出的 bash → python 继承同一份环境，脚本从一开始就按 UTF-8 写管道，
//     服务端根本不会收到 GBK 字节。
//  2. 兜底——decodeAgentOutputLine：对已经到达服务端的非法 UTF-8 行按 GBK 转码，避免
//     它直接落进 json.Unmarshal / json.Marshal 变成 U+FFFD。主要覆盖 stderr、项目运行
//     输出、命令目录探针这些「原始字节直通」的路径。

// utf8ChildEnv 返回需注入到 agent 子进程的 UTF-8 输出环境变量。
//
//   - PYTHONIOENCODING=utf-8：把 Python 的 sys.stdout/sys.stderr 固定为 UTF-8。这是本
//     类乱码的直接根因所在——stdout 被重定向时 Python 默认使用 locale 编码。
//   - PYTHONUTF8=1：开启 Python UTF-8 Mode，覆盖 PYTHONIOENCODING 管不到的路径
//     （例如绕过 sys.stdout 直接写 sys.stdout.buffer）。
//
// 注意 PYTHONUTF8 同时会把 open() 的默认编码改成 UTF-8，即不止影响标准流；若某个项目
// 依赖 locale 编码读写文件，可在 profile 的 Env 里显式覆盖这个变量。
//
// 每次返回新切片，调用方可以自由 append，不会共享底层数组造成串改。
func utf8ChildEnv() []string {
	return []string{"PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"}
}

// appendUTF8ChildEnv 在 inherited 之后追加 UTF-8 输出变量，并剔除 inherited 中同名的
// 旧值。
//
// 同名变量在环境块里出现两次时哪个生效是实现定义的（见 envEntriesMissingFrom），所以
// 必须显式剔除旧值，不能依赖「后者覆盖前者」。
func appendUTF8ChildEnv(inherited []string) []string {
	extra := utf8ChildEnv()
	overridden := make(map[string]struct{}, len(extra))
	for _, item := range extra {
		if name, _, found := strings.Cut(item, "="); found {
			overridden[strings.ToUpper(name)] = struct{}{}
		}
	}
	result := make([]string, 0, len(inherited)+len(extra))
	for _, item := range inherited {
		if name, _, found := strings.Cut(item, "="); found {
			if _, drop := overridden[strings.ToUpper(name)]; drop {
				continue
			}
		}
		result = append(result, item)
	}
	return append(result, extra...)
}

// envEntriesMissingFrom 返回 entries 中变量名尚未出现在 existing 里的那些项，供追加
// 默认值时避免产生重复变量名。
//
// 同名变量在环境块里出现两次时哪个生效是实现定义的（glibc 的 getenv 取第一个，
// Windows 取最后一个），所以任何「追加默认值」的地方都不应该留下重复项。
func envEntriesMissingFrom(entries, existing []string) []string {
	defined := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		if name, _, found := strings.Cut(item, "="); found {
			defined[strings.ToUpper(name)] = struct{}{}
		}
	}
	missing := make([]string, 0, len(entries))
	for _, item := range entries {
		name, _, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		if _, exists := defined[strings.ToUpper(name)]; exists {
			continue
		}
		missing = append(missing, item)
	}
	return missing
}

// gbkDecoder 复用的 GBK→UTF-8 解码器，避免每行重建。
// x/text 的 Decoder 允许并发调用，stdout/stderr 两个读取线程共享同一实例是安全的
// （见 TestDecodeRunOutputLineConcurrent）。
var gbkDecoder = simplifiedchinese.GBK.NewDecoder()

// transcodeLocalAgentOutput 报告本机读到的 agent 输出是否需要 GBK 兜底。
//
// 只有 Windows 服务端拉起的本机进程才可能写出 GBK 字节；WSL/SSH 目标产出 UTF-8。后者
// 不必特殊处理：decodeRunOutputLine 内部会识别「整行已是合法 UTF-8」并原样返回，因此
// 即使判断放宽也不会误伤，这里只是把判断写得符合实际。
const transcodeLocalAgentOutput = runtime.GOOS == "windows"

// decodeRunOutputBytes 把一行输出规范化为 UTF-8 字节。transcode 为 false 时原样返回；
// 为 true 时，若整行已是合法 UTF-8 则原样返回（避免把已按 UTF-8 输出的程序二次
// 破坏），否则尝试按 GBK 解码。转码失败时退回原始字节，交给上层按原样处理。
func decodeRunOutputBytes(line []byte, transcode bool) []byte {
	if !transcode || utf8.Valid(line) || len(line) == 0 {
		return line
	}
	decoded, err := gbkDecoder.Bytes(line)
	if err != nil {
		return line
	}
	return decoded
}

// decodeRunOutputLine 是 decodeRunOutputBytes 的字符串形态，供直接落文本的调用方使用。
func decodeRunOutputLine(line []byte, transcode bool) string {
	return string(decodeRunOutputBytes(line, transcode))
}

// decodeAgentOutputBytes 是 claude/codex 的 stdout/stderr 读取路径在把原始字节交给
// encoding/json 之前必须经过的一道转换。返回字节切片是因为 JSONL 解析需要原始字节，
// 走字符串会多一次 []byte↔string 拷贝。
func decodeAgentOutputBytes(line []byte) []byte {
	return decodeRunOutputBytes(line, transcodeLocalAgentOutput)
}

// decodeAgentOutputLine 是 decodeAgentOutputBytes 的字符串形态。
func decodeAgentOutputLine(line []byte) string {
	return string(decodeAgentOutputBytes(line))
}
