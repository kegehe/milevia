package app

import (
	"bytes"
)

// wslStderrSplit 是 WSL 侧 stderr 管道的 bufio.SplitFunc：兼容 wsl.exe 与 WSL 内 CLI
// 在同一管道内的混合编码输出。
//
// wsl.exe（Windows 主机侧启动器）把自身的警告以 UTF-16LE 写入 stderr——例如 NAT 模式
// 下检测到 localhost 代理时，每次启动都会打印
// "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。"
// （ASCII 字符为 "x\x00" 两字节，中文为 UTF-16 码元）。而 WSL 内 CLI 自身的输出仍是
// UTF-8。若按默认 ScanLines 以 0x0a 切分，UTF-16LE 的换行 "0a 00" 会被切成两半、
// CJK 字符低字节恰为 0x0a 时还会被误判换行，且整行按 UTF-8 解读会变成
// "wsl: �hKm0R localhost ..." 这类乱码（即本机曾出现的现象）。
//
// 因此：检测到 NUL 字节（UTF-16LE 特征）时按 "0a 00" 切行并解码为 UTF-8；否则退化为
// 普通 ScanLines 语义原样返回（UTF-8 / GBK 行不受影响）。返回的 token 已规范化为 UTF-8。
func wslStderrSplit(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if bytes.IndexByte(data, 0) >= 0 {
		for i := 0; i+1 < len(data); i++ {
			if data[i] == '\n' && data[i+1] == 0 {
				end := i
				// 剥离 UTF-16LE 的 \r\n 中的 \r（0d 00）。
				if end >= 2 && data[end-1] == 0 && data[end-2] == '\r' {
					end -= 2
				}
				return i + 2, []byte(decodeUTF16LE(data[:end])), nil
			}
		}
		if !atEOF {
			return 0, nil, nil
		}
		// 流末未换行的 UTF-16LE：剥离尾部孤立的 \r（0d 00）后整体解码，与
		// \n 终止行的 \r\n 剥离保持一致。
		n := len(data)
		if n >= 2 && data[n-2] == '\r' && data[n-1] == 0 {
			data = data[:n-2]
		}
		return n, []byte(decodeUTF16LE(data)), nil
	}
	// 非 UTF-16 内容：按普通换行切分（与 bufio.ScanLines 一致，含 \r\n 及文件末
	// 无换行行的尾部 \r 剥离）。
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		tok := data[:i]
		if len(tok) > 0 && tok[len(tok)-1] == '\r' {
			tok = tok[:len(tok)-1]
		}
		return i + 1, tok, nil
	}
	if atEOF && len(data) > 0 {
		tok := data
		if tok[len(tok)-1] == '\r' {
			tok = tok[:len(tok)-1]
		}
		return len(data), tok, nil
	}
	return 0, nil, nil
}
