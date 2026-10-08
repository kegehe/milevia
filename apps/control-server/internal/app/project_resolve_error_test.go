package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 项目不存在 ≠ 操作冲突。
//
// 判据来自一条一直红的测试（`TestGitInitEndpoint`）：它直接调处理器、没给 chi 的路径参数，
// 于是项目 id 是空串、`getProjectByID` 查不到行 —— 返回的却是 **409「当前操作与进行中的
// 操作冲突，请稍后重试」**。那句话在让用户去重试一件永远不会成功的事（项目已经没了）。
//
// 真实场景同样会撞上：在 Git 页停留期间项目被另一个标签页删掉，之后点任何一次写操作，
// 得到的都是"请稍后重试"。同目录的 `getGitRunner` 早就在这个位置分开处理了，其余十几处是漏网的。
func TestMissingProjectIsNotFoundNotConflict(t *testing.T) {
	server := newTestServer(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		// git 入口（原先直接 409 的那条，也是那条红测试踩到的）
		{"git init", http.MethodPost, "/api/projects/no-such-project/git/init"},
		// 走 resolveRequestWorkspaceFromRequest 的那一族
		{"git operations", http.MethodGet, "/api/projects/no-such-project/git/operations"},
		{"terminal sessions", http.MethodGet, "/api/projects/no-such-project/terminal/sessions"},
		// fs 那一族有自己的错误映射（writeFSError / writeSQLitePreviewError），
		// 原先把同一件事报成 400「请求参数无效」
		{"fs read", http.MethodGet, "/api/projects/no-such-project/fs/read?path=a.txt"},
		{"fs write", http.MethodPut, "/api/projects/no-such-project/fs/write"},
		{"fs search", http.MethodGet, "/api/projects/no-such-project/fs/search?query=x"},
		{"sqlite preview", http.MethodGet, "/api/projects/no-such-project/fs/sqlite/tables?path=a.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			server.routes().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d（body %s），想要 404：不存在的项目被当成了别的东西", w.Code, w.Body.String())
			}
			// 文案要说"项目不存在"，不能说"请稍后重试" —— 后者是一件**永远做不成**的事。
			body := w.Body.String()
			if strings.Contains(body, "重试") {
				t.Fatalf("给「项目不存在」配了让用户重试的文案：%s", body)
			}
			if !strings.Contains(body, "项目不存在") {
				t.Fatalf("文案没能说清是哪一类失败（应为「项目不存在」的本地化文案）：%s", body)
			}
		})
	}
}
