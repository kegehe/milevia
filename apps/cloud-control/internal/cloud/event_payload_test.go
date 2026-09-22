package cloud

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 测试里的 JSON 文本一律用**解释型字符串**写，转义序列写成双反斜杠：
// 这个文件里出现 NUL 转义是刻意的，不能让编辑器/工具链把它折叠成一个真字节。
const (
	// nulEscape 是 JSON 里表示 NUL 的转义序列。
	nulEscape = "\\u0000"
	// fffdEscape 是 U+FFFD（替换字符）的转义序列。
	fffdEscape = "\\ufffd"
	// literalBackslashU0000 是**内容**上的反斜杠加 "u0000"：合法文本，不是转义。
	literalBackslashU0000 = "\\\\u0000"
	// ufffd 是替换字符本身。
	ufffd = "�"
)

// 这是本系统最贵的一个坑的守门用例：cloud_events 与 cloud_instances.snapshot 都是
// jsonb，而 jsonb 存不下 NUL 转义（本地真 PostgreSQL 16 实测：
// ERROR: unsupported Unicode escape sequence / DETAIL: 无法转换为文本）。
func TestSanitizeJSONNULReplacesEscapesInsideStrings(t *testing.T) {
	// 真机上出问题的那条 payload 长这样：WSL 输出被按 UTF-16LE 解出来的残渣。
	input := []byte("{\"content\":\"w" + nulEscape + "s" + nulEscape + "l" + nulEscape + ": hello\",\"ok\":true}")
	got := sanitizeJSONNUL(input)

	if bytes.Contains(got, []byte(nulEscape)) {
		t.Fatalf("NUL escape survived sanitizing: %s", got)
	}
	if !json.Valid(got) {
		t.Fatalf("sanitized payload is not valid JSON: %s", got)
	}
	var decoded struct {
		Content string `json:"content"`
		OK      bool   `json:"ok"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal sanitized payload: %v", err)
	}
	if want := "w" + ufffd + "s" + ufffd + "l" + ufffd + ": hello"; decoded.Content != want {
		t.Fatalf("content = %q, want %q", decoded.Content, want)
	}
	if !decoded.OK {
		t.Fatal("sanitizing dropped an unrelated field")
	}
}

// 字面量（反斜杠 + "u0000" 六个字符）是**内容**，不是 NUL 转义。直接做字节替换
// 会篡改用户数据，所以这条必须钉住。
func TestSanitizeJSONNULKeepsEscapedBackslashLiterals(t *testing.T) {
	input := []byte("{\"text\":\"literal " + literalBackslashU0000 + " stays\",\"other\":\"" + nulEscape + " goes\"}")
	got := sanitizeJSONNUL(input)
	var decoded struct {
		Text  string `json:"text"`
		Other string `json:"other"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantText := "literal " + nulEscape + " stays"
	if decoded.Text != wantText {
		t.Fatalf("text = %q, want %q", decoded.Text, wantText)
	}
	if want := ufffd + " goes"; decoded.Other != want {
		t.Fatalf("other = %q, want %q", decoded.Other, want)
	}
}

// 没有 NUL 的 payload 必须原样返回（同一个底层数组）：热路径上大多数事件都走这一支。
func TestSanitizeJSONNULReturnsInputUntouchedWhenClean(t *testing.T) {
	input := []byte(`{"content":"普通内容","n":1}`)
	got := sanitizeJSONNUL(input)
	if &got[0] != &input[0] {
		t.Fatal("clean payload was copied; the fast path should return the same slice")
	}
}

// 其它 Unicode 转义（中文、emoji 代理对）都不该被动。
func TestSanitizeJSONNULKeepsOtherEscapes(t *testing.T) {
	input := []byte(`{"a":"中文","b":"😀"}`)
	got := sanitizeJSONNUL(input)
	if !bytes.Equal(got, input) {
		t.Fatalf("unrelated escapes changed: %s -> %s", input, got)
	}
}

// 真的只有 NUL 转义会变：替换后的字节数与原串相同（都是 6 个字符），
// 所以 256 KiB 那类长度预算不会被洗牌改变。
func TestSanitizeJSONNULKeepsByteLength(t *testing.T) {
	input := []byte("{\"a\":\"" + nulEscape + "\"}")
	got := sanitizeJSONNUL(input)
	if len(got) != len(input) {
		t.Fatalf("length changed: %d -> %d", len(input), len(got))
	}
	if !bytes.Contains(got, []byte(fffdEscape)) {
		t.Fatalf("expected the replacement escape in %s", got)
	}
}

// 数据异常（SQLSTATE 22xxx/23xxx）必须升级成永久冲突：重发一万次也不会成功。
// 不这么做，云端就会对它永远沉默（既不 ack 也不 reject），本地 outbox 那行永远
// 删不掉，队头被钉死 —— 真机 111 万条事件堵在这个点上。
func TestPermanentEventStoreErrorClassifiesDataExceptions(t *testing.T) {
	dataException := &pgconn.PgError{Code: "22P05", Message: "unsupported Unicode escape sequence"}
	classified := permanentEventStoreError(dataException)
	if classified == nil {
		t.Fatal("a data exception was treated as transient; the event would retry forever")
	}
	if !isEventConflict(classified) {
		t.Fatalf("classified error %v is not reported as a conflict", classified)
	}
	if !strings.Contains(classified.Error(), "unsupported Unicode escape sequence") {
		t.Fatalf("classified error lost the database detail: %v", classified)
	}
	if nil == permanentEventStoreError(&pgconn.PgError{Code: "23505", Message: "duplicate key"}) {
		t.Fatal("an integrity violation should be permanent too")
	}
}

// 连接类故障必须仍然是**临时**的：数据库闪断后 Agent 要能重发成功。
func TestPermanentEventStoreErrorLeavesTransientFailuresAlone(t *testing.T) {
	cases := []error{
		&pgconn.PgError{Code: "08006", Message: "connection failure"},
		&pgconn.PgError{Code: "57014", Message: "query canceled"},
		&pgconn.PgError{Code: "53300", Message: "too many connections"},
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range cases {
		if permanentEventStoreError(err) != nil {
			t.Fatalf("%v was classified as permanent; a transient failure must stay retryable", err)
		}
	}
}
