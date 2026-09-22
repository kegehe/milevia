package cloud

import (
	"strings"
	"testing"
)

// 手机名是**纯展示**字段（永远不会被用来鉴权），但它会直接印在电脑端界面上
// （"当前绑定的手机：Xiaomi 14"），所以必须在入口处收口：控制字符会把那一行排版冲掉，
// 无长度上限则等于让手机往库里塞任意大小的字符串。
func TestSanitizeDeviceName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"空串就是未知手机，交给界面兜底", "", ""},
		{"只去掉首尾空白", "  Xiaomi 14  ", "Xiaomi 14"},
		{"控制字符一律剔除（换行会把那行排版冲掉）", "Xiaomi\n14\r\t", "Xiaomi14"},
		{"超长截断到 64 个字符", strings.Repeat("机", 80), strings.Repeat("机", 64)},
		{"按字符而不是字节截断（中文名不会被切碎）", strings.Repeat("测", 65), strings.Repeat("测", 64)},
	}
	for _, item := range cases {
		got := sanitizeDeviceName(item.raw)
		if got != item.want {
			t.Fatalf("%s: sanitizeDeviceName(%q) = %q, want %q", item.name, item.raw, got, item.want)
		}
	}
	if len([]rune(sanitizeDeviceName(strings.Repeat("机", 80)))) != 64 {
		t.Fatal("截断后必须是 64 个字符，而不是 64 个字节")
	}
}
