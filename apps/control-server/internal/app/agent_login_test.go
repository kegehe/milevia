package app

import "testing"

// 登录输出的授权链接提取（无 Server，纯函数）。
func TestCaptureDeviceURLConditional(t *testing.T) {
	cases := []struct {
		in   string
		want string // 空表示期望提取不到
	}{
		{"Visit https://copilot.tencent.com/activate?code=ABC123 and enter the code", "https://copilot.tencent.com/activate?code=ABC123"},
		{"打开 http://127.0.0.1:8080/auth 完成授权", "http://127.0.0.1:8080/auth"},
		{"没有链接的普通提示", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := captureDeviceURLConditional(c.in); got != c.want {
			t.Fatalf("captureDeviceURLConditional(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}