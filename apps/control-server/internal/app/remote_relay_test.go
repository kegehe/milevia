package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── 脚手架 ─────────────────────────────────────────────────────────────────

// newFSRelayTestServer 建一台带一个本地项目的控制服务，项目根就是返回的临时目录。
// 不带 conversationId 时工作区解析落到 project_shared（Path == project.Path），
// 所以这条路径不需要先造会话记录。
func newFSRelayTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	server := newTestServer(t)
	projectRoot := t.TempDir()
	now := time.Now().UTC()
	if _, err := server.db.Exec(
		`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('file-project','file-project',?,?,'main',1,?)`,
		projectRoot, server.localRunnerID(), now,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return server, projectRoot
}

func relayFS(t *testing.T, server *Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal relay body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/remote/rpc", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.relayRPCRequest(response, request)
	decoded := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode relay response %q: %v", response.Body.String(), err)
	}
	return response.Code, decoded
}

func relayFSData(t *testing.T, server *Server, body map[string]any) map[string]any {
	t.Helper()
	status, payload := relayFS(t, server, body)
	if status != http.StatusOK {
		t.Fatalf("relay transport status = %d body %v", status, payload)
	}
	if payload["ok"] != true {
		t.Fatalf("relay reported failure: %v", payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		t.Fatalf("relay returned no data: %v", payload)
	}
	return data
}

// ─── 单一白名单 ─────────────────────────────────────────────────────────────

// 这份名单是手机端能执行的全部文件操作。多一个少一个都是安全边界的移动，
// 所以把它写死在测试里：改实现必须同时改这个期望，逼出一次有意识的决定。
func TestFSRemoteOperationWhitelistIsExplicit(t *testing.T) {
	server := &Server{}
	expected := map[string]string{
		"fs.tree":          http.MethodGet,
		"fs.open":          http.MethodGet,
		"fs.search":        http.MethodGet,
		"fs.write":         http.MethodPut,
		"fs.mkdir":         http.MethodPost,
		"fs.rename":        http.MethodPost,
		"fs.remove":        http.MethodDelete,
		"fs.sqlite.tables": http.MethodGet,
		"fs.sqlite.schema": http.MethodGet,
		"fs.sqlite.rows":   http.MethodGet,
	}
	operations := server.remoteFSOperations()
	if len(operations) != len(expected) {
		t.Fatalf("operation count = %d, want %d", len(operations), len(expected))
	}
	for op, method := range expected {
		operation, ok := operations[op]
		if !ok {
			t.Fatalf("operation %q is missing", op)
		}
		if operation.Method != method {
			t.Fatalf("operation %q method = %s, want %s", op, operation.Method, method)
		}
		if operation.Handle == nil {
			t.Fatalf("operation %q has no handler", op)
		}
		if !strings.HasPrefix(operation.Path, "/fs/") {
			t.Fatalf("operation %q path = %q, want an /fs/ path", op, operation.Path)
		}
	}
}

func TestRelayFSRequestRejectsUnsupportedOperation(t *testing.T) {
	server := newTestServer(t)
	status, payload := relayFS(t, server, map[string]any{"op": "fs.chmod", "projectId": "file-project"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if payload["error"] == nil {
		t.Fatalf("expected an error message, got %v", payload)
	}
}

func TestRelayFSRequestRejectsMissingProject(t *testing.T) {
	server := newTestServer(t)
	status, _ := relayFS(t, server, map[string]any{"op": "fs.tree"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

// projectId 会被拼进合成请求的 URL 路径。它随后仍要经 handler 查库，但先挡住分隔符与
// 编码字符，否则一个 `../` 就能把合成请求指到别的路由上 —— 那些路由没有 projectID
// 校验。这条断言守的是"拼 URL"这个动作本身。
func TestRelayFSRequestRejectsProjectIDWithPathSyntax(t *testing.T) {
	server := newTestServer(t)
	for _, projectID := range []string{"../admin", "a/b", "a\\b", "a?b", "a#b", "a%2fb"} {
		t.Run(projectID, func(t *testing.T) {
			status, _ := relayFS(t, server, map[string]any{"op": "fs.tree", "projectId": projectID})
			if status != http.StatusBadRequest {
				t.Fatalf("projectId %q: status = %d, want 400", projectID, status)
			}
		})
	}
}

// 查询类操作的 params 必须是字符串映射。用 map[string]any 再自行转换会把数字/布尔
// 悄悄改成别的写法，客户端传错类型时应当明确报错。
func TestRelayFSRequestRejectsNonStringQueryParams(t *testing.T) {
	server := newTestServer(t)
	status, payload := relayFS(t, server, map[string]any{
		"op":        "fs.tree",
		"projectId": "file-project",
		"params":    map[string]any{"depth": 3},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body %v, want 400", status, payload)
	}
}

// ─── 端到端：读 → 写 → 再读 ─────────────────────────────────────────────────

func TestRelayFSRequestReadsAndWritesProjectFile(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.WriteFile(filepath.Join(projectRoot, "notes.md"), []byte("# 标题\n正文\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	opened := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "notes.md"},
	})
	if opened["content"] != "# 标题\n正文\n" {
		t.Fatalf("content = %v", opened["content"])
	}
	if opened["editable"] != true {
		t.Fatalf("editable = %v, want true", opened["editable"])
	}
	version, _ := opened["version"].(string)
	if version == "" {
		t.Fatal("version is empty")
	}

	// 保存必须带对版本：这是手机端唯一能挡住"覆盖掉电脑上别人的改动"的手段。
	written := relayFSData(t, server, map[string]any{
		"op":        "fs.write",
		"projectId": "file-project",
		"params":    map[string]string{"path": "notes.md", "content": "# 标题\n改过的正文\n", "expectedVersion": version},
	})
	if written["status"] != "ok" {
		t.Fatalf("write result = %v", written)
	}

	raw, err := os.ReadFile(filepath.Join(projectRoot, "notes.md"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "# 标题\n改过的正文\n" {
		t.Fatalf("file content = %q", string(raw))
	}
}

func TestRelayFSRequestWriteReportsVersionConflict(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.WriteFile(filepath.Join(projectRoot, "notes.md"), []byte("original"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	status, payload := relayFS(t, server, map[string]any{
		"op":        "fs.write",
		"projectId": "file-project",
		"params":    map[string]string{"path": "notes.md", "content": "stale write", "expectedVersion": "sha256-of-something-else"},
	})
	if status != http.StatusOK {
		t.Fatalf("transport status = %d, want 200 (业务失败走 ok:false 而不是 HTTP 错误)", status)
	}
	if payload["ok"] != false {
		t.Fatalf("payload = %v, want ok:false", payload)
	}
	// 状态码必须保留：手机端据此区分版本冲突、lease 占用与参数错误。
	if payload["status"] != float64(http.StatusConflict) {
		t.Fatalf("status = %v, want 409", payload["status"])
	}
	if message, _ := payload["error"].(string); !strings.Contains(message, "已被修改") {
		t.Fatalf("error = %q, want the server's user-facing conflict message", message)
	}
	// 冲突时文件必须原封不动。
	raw, err := os.ReadFile(filepath.Join(projectRoot, "notes.md"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "original" {
		t.Fatalf("file was modified despite the conflict: %q", string(raw))
	}
}

// 路径沙箱不能在 relay 这一层被绕过：所有 fs handler 都经 Filesystem 的路径校验，
// 所以穿越必须被拒。这是"中继命名空间开始提供文件访问"之后最要紧的一条断言。
func TestRelayFSRequestKeepsPathInsideProjectSandbox(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	outside := filepath.Join(filepath.Dir(projectRoot), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	for _, testCase := range []struct {
		name string
		op   string
		body map[string]any
	}{
		{"open may not escape", "fs.open", map[string]any{"params": map[string]string{"path": "../secret.txt"}}},
		{"open may not use an absolute path", "fs.open", map[string]any{"params": map[string]string{"path": outside}}},
		{"write may not escape", "fs.write", map[string]any{"params": map[string]string{"path": "../escaped.txt", "content": "x"}}},
		{"mkdir may not escape", "fs.mkdir", map[string]any{"params": map[string]string{"path": "../escaped-dir"}}},
		{"remove may not escape", "fs.remove", map[string]any{"params": map[string]string{"path": "../secret.txt"}}},
		{"rename may not escape", "fs.rename", map[string]any{"params": map[string]string{"oldPath": "notes.md", "newPath": "../moved.md"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := map[string]any{"op": testCase.op, "projectId": "file-project"}
			for key, value := range testCase.body {
				body[key] = value
			}
			_, payload := relayFS(t, server, body)
			if payload["ok"] != false {
				t.Fatalf("%s was allowed: %v", testCase.op, payload)
			}
		})
	}

	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "secret" {
		t.Fatalf("outside file changed: %q err=%v", string(raw), err)
	}
}

// ─── /fs/open 的闸门 ────────────────────────────────────────────────────────

func TestFSOmittedByMetadataBranches(t *testing.T) {
	for _, testCase := range []struct {
		name string
		info FileInfo
		want string
	}{
		{"small text is readable", FileInfo{IsText: true, Size: 1024, MimeType: "text/plain"}, ""},
		{"text at the view limit is readable", FileInfo{IsText: true, Size: fsRemoteOpenViewLimit, MimeType: "text/plain"}, ""},
		{"text over the view limit is omitted", FileInfo{IsText: true, Size: fsRemoteOpenViewLimit + 1, MimeType: "text/plain"}, fsOmittedTooLarge},
		{"small image is readable", FileInfo{IsText: false, Size: 4096, MimeType: "image/png"}, ""},
		{"image over the binary limit is omitted", FileInfo{IsText: false, Size: fsRemoteOpenBinaryLimit + 1, MimeType: "image/png"}, fsOmittedBinary},
		{"other binary is omitted", FileInfo{IsText: false, Size: 16, MimeType: "application/pdf"}, fsOmittedBinary},
		{"binary without a mime type is omitted", FileInfo{IsText: false, Size: 16, MimeType: ""}, fsOmittedBinary},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := fsOmittedByMetadata(testCase.info); got != testCase.want {
				t.Fatalf("fsOmittedByMetadata = %q, want %q", got, testCase.want)
			}
		})
	}
}

// 图片走 base64，传输长度比文件大小多约三分之一。用文件大小去比查看上限，一张
// 100 KiB 的 PNG 会被放行成 137 KiB 的响应 —— 这条断言把它钉住。
func TestFSOpenImageIsBase64AndMeasuredAfterEncoding(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	pixels := bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, fsRemoteOpenBinaryLimit/4)
	if err := os.WriteFile(filepath.Join(projectRoot, "shot.png"), pixels, 0o644); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	opened := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "shot.png"},
	})
	if opened["encoding"] != "base64" {
		t.Fatalf("encoding = %v, want base64", opened["encoding"])
	}
	content, _ := opened["content"].(string)
	if content == "" {
		t.Fatalf("image content was omitted: %v", opened)
	}
	if bytes, ok := opened["bytes"].(float64); !ok || int(bytes) != len(content) {
		t.Fatalf("bytes = %v, want the transmitted length %d", opened["bytes"], len(content))
	}
	if len(content) <= fsRemoteOpenBinaryLimit {
		t.Fatalf("base64 length %d should exceed the raw limit %d — otherwise this test proves nothing", len(content), fsRemoteOpenBinaryLimit)
	}
	if len(content) > fsRemoteOpenViewLimit {
		t.Fatalf("base64 length %d exceeds the frame budget %d", len(content), fsRemoteOpenViewLimit)
	}
	// 图片不该被标成可编辑。
	if opened["editable"] != false {
		t.Fatalf("editable = %v, want false", opened["editable"])
	}
	if opened["readOnlyReason"] != fsReadOnlyBinaryFile {
		t.Fatalf("readOnlyReason = %v, want %s", opened["readOnlyReason"], fsReadOnlyBinaryFile)
	}
}

// content 的键必须**始终存在**。前端按 `string | null` 钉类型，键时有时无会让它
// 多写一层存在性判断，也容易在某个分支上读到 undefined。（本项目已经踩过一次
// "后端切片留 nil 导致整页空白"。）
func TestFSOpenKeepsContentKeyPresentAsNull(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.WriteFile(filepath.Join(projectRoot, "big.txt"), bytes.Repeat([]byte("a"), fsRemoteOpenViewLimit+16), 0o644); err != nil {
		t.Fatalf("seed big file: %v", err)
	}

	encoded, err := json.Marshal(map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "big.txt"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/remote/rpc", bytes.NewReader(encoded))
	response := httptest.NewRecorder()
	server.relayRPCRequest(response, request)

	// 直接看 relay 原始响应里的 data 片段：走 map[string]any 解码之后
	// 是 null 还是缺失就分不出来了，而这一点正是要断言的。
	if !strings.Contains(response.Body.String(), `"content":null`) {
		t.Fatalf("content key must be present as null, body = %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"omittedReason":"`+fsOmittedTooLarge+`"`) {
		t.Fatalf("omittedReason missing, body = %s", response.Body.String())
	}
}

// 文本在"可编辑上限"与"查看上限"之间必须能看、但不能编辑，且说明原因。
// 允许编辑一个超出可编辑上限的文件，保存时才会在通道上失败 —— 那时用户已经改了半天。
func TestFSOpenLargeTextIsReadableButNotEditable(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	size := fsRemoteOpenEditableLimit + 512
	if err := os.WriteFile(filepath.Join(projectRoot, "large.txt"), bytes.Repeat([]byte("b"), size), 0o644); err != nil {
		t.Fatalf("seed large text: %v", err)
	}

	opened := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "large.txt"},
	})
	if opened["content"] == nil {
		t.Fatalf("large text should still be viewable: %v", opened)
	}
	if opened["editable"] != false {
		t.Fatalf("editable = %v, want false", opened["editable"])
	}
	if opened["readOnlyReason"] != fsReadOnlyFileTooLarge {
		t.Fatalf("readOnlyReason = %v", opened["readOnlyReason"])
	}
	// 内容被省略时 version 必须为空 —— 没见过内容就不该能做带版本的保存。
	if version, _ := opened["version"].(string); version == "" {
		t.Fatal("version must be recorded when content is returned")
	}
}

// 文本的第二道闸门必须量**转义之后**的长度。拿原始长度去比会漏掉一整类文件：
// 引号密集的 JSON、`<` 密集的 HTML —— 它们在 JSON 里每个字符要占 2 到 6 个字节。
//
// 这条守的是"图片按编码后长度判断"那份教训在**文本**上的同一份（base64 会膨胀，
// JSON 转义也会）。漏掉的后果不是"少给几个文件"：那份内容会被发出去，撞上中继的
// 帧上限，于是手机拿到一句"文件超过中继通道容量"—— 而那时服务端已经把它标成
// 可读（甚至可编辑）了。
//
// 夹具刻意落在这条缝里，并**自证**它落在这里：原始大小远低于视图上限（所以元信息
// 那道闸门一定放行），转义后超过传输上限（所以第二道闸门必须拦下）。
func TestFSOpenOmitsTextWhoseEscapedFormExceedsTheTransmitBudget(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	size := 200 << 10
	dense := bytes.Repeat([]byte(`"`), size)
	escaped := escapedJSONLength(string(dense))
	if size >= fsRemoteOpenViewLimit {
		t.Fatalf("fixture is too big to sit in the gap: raw=%d view limit=%d", size, fsRemoteOpenViewLimit)
	}
	if escaped <= fsRemoteOpenTransmitLimit {
		t.Fatalf("fixture does not overflow once escaped: escaped=%d budget=%d — this test would prove nothing", escaped, fsRemoteOpenTransmitLimit)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "quotes.txt"), dense, 0o644); err != nil {
		t.Fatalf("seed quotes: %v", err)
	}
	// 对照：同样大小、纯 ASCII 的内容必须照常给内容 —— 这一条证明那道闸门量的是
	//**转义**而不是大小，否则"拦下来"可能只是"200 KiB 也被拦了"。
	if err := os.WriteFile(filepath.Join(projectRoot, "plain.txt"), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("seed plain: %v", err)
	}

	denseOpened := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "quotes.txt"},
	})
	if content, _ := denseOpened["content"].(string); content != "" {
		// 只报长度：这条夹具有 200 KiB，把内容打进失败输出会把后面的断言全淹掉。
		t.Fatalf("escaped-oversized content must not be sent, got %d bytes", len(content))
	}
	if denseOpened["omittedReason"] != fsOmittedTooLarge {
		t.Fatalf("omittedReason = %v, want %v", denseOpened["omittedReason"], fsOmittedTooLarge)
	}
	if denseOpened["editable"] != false {
		t.Fatalf("editable = %v, want false（发都发不出去的东西不能标成可编辑）", denseOpened["editable"])
	}

	plainOpened := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "plain.txt"},
	})
	if plainOpened["content"] == nil {
		t.Fatalf("a same-sized plain file must still be readable: %v", plainOpened)
	}
}

func TestFSOpenOmitsVersionWhenContentIsOmitted(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.WriteFile(filepath.Join(projectRoot, "huge.txt"), bytes.Repeat([]byte("c"), fsRemoteOpenViewLimit+1), 0o644); err != nil {
		t.Fatalf("seed huge file: %v", err)
	}
	opened := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "huge.txt"},
	})
	if opened["content"] != nil {
		t.Fatalf("content = %v, want null", opened["content"])
	}
	if version, _ := opened["version"].(string); version != "" {
		t.Fatalf("version = %q, want empty when content was not sent", version)
	}
	if opened["editable"] != false {
		t.Fatalf("editable = %v, want false", opened["editable"])
	}
}

func TestFSOpenRejectsDirectory(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.Mkdir(filepath.Join(projectRoot, "src"), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	_, payload := relayFS(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "src"},
	})
	if payload["ok"] != false {
		t.Fatalf("opening a directory must fail: %v", payload)
	}
}

// ─── 目录树 ─────────────────────────────────────────────────────────────────

func TestFSTreeDepthParsing(t *testing.T) {
	for _, testCase := range []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 1, false},
		{"1", 1, false},
		{"3", 3, false},
		{fmt.Sprint(fsTreeMaxDepth), fsTreeMaxDepth, false},
		{fmt.Sprint(fsTreeMaxDepth + 1), 0, true},
		{"0", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
	} {
		t.Run(testCase.raw, func(t *testing.T) {
			got, err := fsTreeDepth(testCase.raw)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("fsTreeDepth(%q) = %d, want an error", testCase.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("fsTreeDepth(%q): %v", testCase.raw, err)
			}
			if got != testCase.want {
				t.Fatalf("fsTreeDepth(%q) = %d, want %d", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestReadTreeRecursesAndHidesDependencyDirectories(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"src/lib", "node_modules/left-pad", "dist"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	filesystem := &LocalFilesystem{projectPath: root}

	budget := &fsTreeBudget{remaining: fsTreeMaxTotalEntry}
	entries, err := readTree(context.Background(), filesystem, "", 3, budget)
	if err != nil {
		t.Fatalf("readTree: %v", err)
	}
	names := map[string]bool{}
	var src *FileEntry
	for index := range entries {
		names[entries[index].Name] = true
		if entries[index].Name == "src" {
			src = &entries[index]
		}
	}
	if names["node_modules"] || names["dist"] {
		t.Fatalf("dependency directories must be hidden, got %v", names)
	}
	if budget.skipped != 2 {
		t.Fatalf("skipped = %d, want 2", budget.skipped)
	}
	// 跳过依赖目录是**预期行为**，不是"这次没取全"。两个数必须分开，
	// 否则手机端就只能对用户说一句含糊的"内容不完整"。
	if budget.truncated {
		t.Fatal("hiding dependency directories must not set truncated")
	}
	if src == nil {
		t.Fatalf("src missing from %v", names)
	}
	var lib *FileEntry
	childNames := map[string]bool{}
	for index := range src.Children {
		childNames[src.Children[index].Name] = true
		if src.Children[index].Name == "lib" {
			lib = &src.Children[index]
		}
	}
	if !childNames["lib"] || !childNames["main.ts"] {
		t.Fatalf("src children = %v, want lib and main.ts", childNames)
	}
	// depth=3 在 src 这一层还剩 2 层：lib 会被读一次（读出它是空的），
	// main.ts 是文件、没有 children。
	if lib == nil || lib.Children == nil || len(lib.Children) != 0 {
		t.Fatalf("lib children = %+v, want an empty (but read) directory", lib)
	}
}

func TestReadTreeBudgetIsSharedAcrossLevels(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for index := 0; index < 5; index++ {
		if err := os.WriteFile(filepath.Join(root, "a", "b", fmt.Sprintf("f%d.txt", index)), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	filesystem := &LocalFilesystem{projectPath: root}

	// 预算只够 a 这一层，b 的内容必然被裁掉。每层各给一份预算的话这里会全取到，
	// 配额就等于没有。
	budget := &fsTreeBudget{remaining: 1}
	_, err := readTree(context.Background(), filesystem, "", 5, budget)
	if err != nil {
		t.Fatalf("readTree: %v", err)
	}
	if !budget.truncated {
		t.Fatalf("budget exhaustion must set truncated (remaining=%d)", budget.remaining)
	}
	if budget.remaining > 0 {
		t.Fatalf("remaining = %d, want the budget to be spent", budget.remaining)
	}
}

// 一个读不到的目录不该让整棵树失败：项目里几十个目录，一个没权限的把全部拖下水
// 会让用户看不到任何文件。标记 unreadable 让它自己承担后果。
func TestReadTreeMarksUnreadableChildButKeepsSiblings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "readable"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "readable", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	filesystem := &brokenDirFilesystem{root: root, broken: "broken"}

	budget := &fsTreeBudget{remaining: fsTreeMaxTotalEntry}
	entries, err := readTree(context.Background(), filesystem, "", 3, budget)
	if err != nil {
		t.Fatalf("readTree must not fail because of one unreadable child: %v", err)
	}
	// 两个同级目录都要在：坏掉的那个标 unreadable，好的那个照常有内容。
	// 一个读不到的目录不允许把整棵树（或它的兄弟）拖下水。
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want both directories", entries)
	}
	byName := map[string]FileEntry{}
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	if !byName["broken"].Unreadable {
		t.Fatalf("broken directory must be marked unreadable: %+v", byName["broken"])
	}
	if byName["readable"].Unreadable {
		t.Fatalf("readable directory must not be marked unreadable: %+v", byName["readable"])
	}
	if len(byName["readable"].Children) != 1 {
		t.Fatalf("readable children = %+v, want one file", byName["readable"].Children)
	}
}

// brokenDirFilesystem 只借 LocalFilesystem 的读取能力，另外让一个子目录读不出来。
type brokenDirFilesystem struct {
	root   string
	broken string
}

func (fs *brokenDirFilesystem) ReadDir(ctx context.Context, path string) ([]FileEntry, error) {
	if path == fs.broken {
		return nil, fmt.Errorf("permission denied")
	}
	if path == "" {
		return []FileEntry{
			{Name: fs.broken, Path: fs.broken, IsDir: true},
			{Name: "readable", Path: "readable", IsDir: true},
		}, nil
	}
	return []FileEntry{{Name: "a.txt", Path: path + "/a.txt"}}, nil
}

func (fs *brokenDirFilesystem) ReadFile(context.Context, string) (*FileContent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) OpenRead(context.Context, string) (io.ReadCloser, FileInfo, error) {
	return nil, FileInfo{}, fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) Stat(context.Context, string) (FileInfo, error) {
	return FileInfo{}, fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) Search(context.Context, string, string) ([]FileEntry, error) {
	return nil, fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) WriteFile(context.Context, string, []byte, string, bool) error {
	return fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) Mkdir(context.Context, string) error {
	return fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) Remove(context.Context, string) error {
	return fmt.Errorf("not implemented")
}
func (fs *brokenDirFilesystem) Rename(context.Context, string, string) error {
	return fmt.Errorf("not implemented")
}

// depth=1 是桌面端一直在用的形状：不走忽略名单、不带 children。静默改掉桌面端
// 看得到什么，等于替它做决定。
func TestTreeWithoutDepthKeepsFlatBehaviour(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.MkdirAll(filepath.Join(projectRoot, "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	data := relayFSData(t, server, map[string]any{
		"op":        "fs.tree",
		"projectId": "file-project",
		"params":    map[string]string{},
	})
	entries, _ := data["entries"].([]any)
	found := false
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["name"] == "node_modules" {
			found = true
			if _, hasChildren := entry["children"]; hasChildren {
				t.Fatalf("flat listing must not carry children: %v", entry)
			}
		}
	}
	if !found {
		t.Fatal("a flat listing must still show node_modules — that is the desktop behaviour")
	}
	if data["truncated"] != false {
		t.Fatalf("truncated = %v, want false", data["truncated"])
	}
}

func TestTreeWithDepthHidesDependenciesAndReportsSkips(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.MkdirAll(filepath.Join(projectRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "src", "main.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	data := relayFSData(t, server, map[string]any{
		"op":        "fs.tree",
		"projectId": "file-project",
		"params":    map[string]string{"depth": "3"},
	})
	if data["skippedDirs"] != float64(1) {
		t.Fatalf("skippedDirs = %v, want 1", data["skippedDirs"])
	}
	entries, _ := data["entries"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["name"] == "node_modules" {
			t.Fatalf("node_modules must be hidden in a deep listing: %v", entry)
		}
	}
}

// 忽略名单是**按小写匹配**的，而目录名的写法按平台五花八门（macOS 上是 `Pods`）。
// 这条用混合大小写的真实目录名去跑，而不是拿名单里的字符串去比名单自己 ——
// 后者在"键写成了大写"时照样通过，而那正是这里出过的错：`Pods` / `DerivedData`
// 两个大写开头的键与 `strings.ToLower(entry.Name)` 永远匹配不上，看起来在名单里，
// 实际一条都没跳过。
func TestTreeWithDepthHidesMixedCaseDependencyDirs(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	for _, dir := range []string{"Pods", "DerivedData", "build"} {
		if err := os.MkdirAll(filepath.Join(projectRoot, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	data := relayFSData(t, server, map[string]any{
		"op":        "fs.tree",
		"projectId": "file-project",
		"params":    map[string]string{"depth": "3"},
	})
	if data["skippedDirs"] != float64(3) {
		t.Fatalf("skippedDirs = %v, want 3 (Pods / DerivedData / build)", data["skippedDirs"])
	}
	entries, _ := data["entries"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if name, _ := entry["name"].(string); name == "Pods" || name == "DerivedData" {
			t.Fatalf("%s must be hidden in a deep listing: %v", name, entry)
		}
	}
}

// `conversationId` 决定**落在哪个工作区**，它只能来自信封 —— 信封里那个值是页面按当前
// 会话定死传下来的。params 是自由填的一袋文件参数，让它同名覆盖等于"一个路径参数顺手
// 改了工作区选择"：客户端多传一个字段，请求就静默落到别的工作区，或者死在
// "会话工作区不存在"上，而那句报错完全指不到真正的原因。
func TestRelayFSRequestIgnoresConversationIDInParams(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	if err := os.WriteFile(filepath.Join(projectRoot, "notes.md"), []byte("# 标题\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// 信封里没有 conversationId（= 项目共享工作区），params 里塞一个不存在的会话。
	// 覆盖生效的话这一步会失败在"会话工作区不存在"，所以断言的就是"读得到文件"。
	data := relayFSData(t, server, map[string]any{
		"op":        "fs.open",
		"projectId": "file-project",
		"params":    map[string]string{"path": "notes.md", "conversationId": "does-not-exist"},
	})
	if data["content"] != "# 标题\n" {
		t.Fatalf("content = %v, want the file read from the project-shared workspace", data["content"])
	}
}

// ─── 辅助函数 ───────────────────────────────────────────────────────────────

func TestLocalHandlerErrorTextPreferences(t *testing.T) {
	if got := localHandlerErrorText([]byte(`{"error":"文件已被修改"}`), http.StatusConflict); got != "文件已被修改" {
		t.Fatalf("structured error = %q", got)
	}
	if got := localHandlerErrorText([]byte("plain failure"), http.StatusBadRequest); got != "plain failure" {
		t.Fatalf("plain text = %q", got)
	}
	// 既不是 JSON 又没有可读文案时，回落成状态码文案而不是空串 ——
	// 返回空串会让手机端只能显示"操作失败"，把服务端已知的原因藏起来。
	if got := localHandlerErrorText(nil, http.StatusBadGateway); got != http.StatusText(http.StatusBadGateway) {
		t.Fatalf("fallback = %q", got)
	}
}

// 手机端按**错误码**分支，而那句文案会被本地化 —— 两件事必须同时成立。
// 这里把"码 + 中文句子"一起钉住：客户端那侧的分类函数依赖它们成对出现
// （见 apps/web 的 classifyGitFailure 与它的用例）。
//
// 为什么值得一条测试：单看客户端用例会给出假绿 —— 它喂的是本地化**之前**的英文原文，
// 而真正上路的是本地化之后的句子。判据必须量真正发出去的那个东西。
func TestRelayErrorCarriesBothLocalizedTextAndMachineCode(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeError(recorder, http.StatusConflict, &projectWorkspaceOccupiedError{owner: "run:abc"})
	body := recorder.Body.Bytes()

	// 断言**真正发给手机的那份答复**（relayErrorResponse 就是 relayRPCRequest 里那一次调用），
	// 而不是分别去测两个取值函数 —— 后者测不出"接线漏了一行"。
	response := relayErrorResponse(http.StatusConflict, body)
	if response.OK {
		t.Fatalf("ok = true, want false")
	}
	if response.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Status)
	}
	if response.Code != "workspace_occupied" {
		t.Fatalf("code = %q, want workspace_occupied", response.Code)
	}
	if !strings.Contains(response.Error, "项目工作区") {
		t.Fatalf("error = %q, want the localized sentence", response.Error)
	}
	// 没有码的失败必须回空串（而不是某句文案）：客户端据此知道"只能退回去匹配文案"。
	if got := localHandlerErrorCode([]byte(`{"error":"Git state changed; refresh the repository"}`)); got != "" {
		t.Fatalf("code = %q, want empty for an un-coded failure", got)
	}
	if got := localHandlerErrorCode(nil); got != "" {
		t.Fatalf("code = %q, want empty for an empty body", got)
	}
}

func TestOrEmptyJSONObjectNeverEmitsInvalidJSON(t *testing.T) {
	if got := string(orEmptyJSONObject(nil)); got != "{}" {
		t.Fatalf("nil body = %q", got)
	}
	if got := string(orEmptyJSONObject([]byte("not json"))); got != "{}" {
		t.Fatalf("invalid body = %q", got)
	}
	if got := string(orEmptyJSONObject([]byte(`{"a":1}`))); got != `{"a":1}` {
		t.Fatalf("valid body = %q", got)
	}
}
