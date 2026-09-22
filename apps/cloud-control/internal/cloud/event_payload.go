package cloud

import "bytes"

// PostgreSQL 的 jsonb **不接受** JSON 里表示 NUL 的转义序列（反斜杠 + "u0000"）：
//
//	ERROR:  unsupported Unicode escape sequence
//	DETAIL: 该转义无法转换为文本
//
// 同一条文本存进 text 或 json 都没问题，只有 jsonb 不行 —— 本地用真 PostgreSQL
// 16 实测确认。而 CLI 的输出里带 NUL 是常态：真机库里有 95 条事件带着 84 个这种
// 转义，是 WSL 那边 UTF-16LE 解码的残留。
//
// 这些事件此前永远存不进 cloud_events，而且云端把它当"临时数据库故障"处理：
// 既不回 ack 也不回 reject（见 agentConnect 的错误分类），于是本地 outbox 里对应的
// 行永远删不掉、每次读取都被它们占住批次名额，一条 WebSocket 连接在 4.5 小时里被
// 灌了 792 MB 的重复确认。
//
// 这里在入库前把它们洗掉：NUL 换成 U+FFFD（替换字符）。丢的是一个本来就无法表示的
// 控制字符，换来的是这条事件能被存下来、能被确认、不再堵住后面的队列。
var nulJSONEscape = []byte("\\u0000")

// replacementJSONEscape 是 U+FFFD 的 JSON 转义形式。
var replacementJSONEscape = []byte("\\ufffd")

// sanitizeJSONNUL 把 JSON 文本里处于**字符串内部**的 NUL 转义替换掉。
//
// 必须按转义状态扫描，不能直接做字节替换：字面量 `\\u0000`（反斜杠 + "u0000"
// 六个字符）是合法内容，替换它等于篡改用户数据。所以这里在字符串态下逐段前进：
// 遇到反斜杠就把整段转义一起消费，只有恰好是 NUL 转义的那一段才改写。
func sanitizeJSONNUL(data []byte) []byte {
	if !bytes.Contains(data, nulJSONEscape) {
		return data
	}
	out := make([]byte, 0, len(data))
	inString := false
	for index := 0; index < len(data); {
		current := data[index]
		if !inString {
			out = append(out, current)
			if current == '"' {
				inString = true
			}
			index++
			continue
		}
		if current == '\\' && index+1 < len(data) {
			// 反斜杠 u XXXX：连四个十六进制位一起消费，长度不足就按普通转义处理。
			if data[index+1] == 'u' && index+6 <= len(data) && isHexDigits(data[index+2:index+6]) {
				if bytes.Equal(data[index:index+6], nulJSONEscape) {
					out = append(out, replacementJSONEscape...)
				} else {
					out = append(out, data[index:index+6]...)
				}
				index += 6
				continue
			}
			out = append(out, data[index], data[index+1])
			index += 2
			continue
		}
		out = append(out, current)
		if current == '"' {
			inString = false
		}
		index++
	}
	return out
}

func isHexDigits(data []byte) bool {
	for _, digit := range data {
		switch {
		case digit >= '0' && digit <= '9':
		case digit >= 'a' && digit <= 'f':
		case digit >= 'A' && digit <= 'F':
		default:
			return false
		}
	}
	return true
}
