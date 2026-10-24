package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 这份名单是手机端能执行的全部优化建议操作。多一个少一个都是"能改项目数据 / 能烧
// AI 配额"的能力在移动，所以把它写死在测试里：改实现必须同时改这个期望，逼出一次
// 有意识的决定。
func TestInsightsRemoteOperationWhitelistIsExplicit(t *testing.T) {
	server := &Server{}
	expected := map[string]string{
		"insights.list":                http.MethodGet,
		"insights.scan":                http.MethodPost,
		"insights.cancel":              http.MethodPost,
		"insights.verify":              http.MethodPost,
		"insights.toTask":              http.MethodPost,
		"insights.delete":              http.MethodPost,
		"insights.findingUpdate":       http.MethodPatch,
		"insights.findingDelete":       http.MethodDelete,
		"insights.findingToTask":       http.MethodPost,
		"insights.findingDismiss":      http.MethodPost,
		"insights.findingDismissClear": http.MethodDelete,
	}
	operations := server.remoteInsightOperations()
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
		if !strings.HasPrefix(operation.Path, "/insights") {
			t.Fatalf("operation %q path = %q, want an /insights path", op, operation.Path)
		}
	}
}

func insightOperation(t *testing.T, op string) remoteOperation {
	t.Helper()
	operation, ok := (&Server{}).remoteInsightOperations()[op]
	if !ok {
		t.Fatalf("operation %q is missing", op)
	}
	return operation
}

// 单条建议的 findingID 会被拼进合成请求的 URL 路径（再经 handler 查库），所以先把
// 形状挡在中继层：不是 UUID 一律 400，否则一个拼好的 findingID 就能把请求指到别的
// 本机路由上。relayTarget 是纯函数，这里可以直接穷举。
func TestRelayInsightFindingPathParamsAreValidated(t *testing.T) {
	operation := insightOperation(t, "insights.findingDismiss")
	valid := "0b8fd035-9f45-4a5e-9f61-2f6b1f6b1f6b"
	for _, test := range []struct {
		params    string
		wantError string
	}{
		{`{}`, "findingID 必填"},
		{`{"findingID": 42}`, "findingID 必须是字符串"},
		{`{"findingID": "../../admin"}`, "findingID 格式不正确"},
		{`{"findingID": "not-a-uuid"}`, "findingID 格式不正确"},
		{`{"findingID": "` + valid + `"}`, ""},
	} {
		_, _, _, err := relayTarget(operation, "file-project", "", json.RawMessage(test.params))
		if test.wantError == "" {
			if err != nil {
				t.Fatalf("findingID %q: unexpected error %v", test.params, err)
			}
			continue
		}
		if err == nil || err.Error() != test.wantError {
			t.Fatalf("findingID %q: error = %v, want %q", test.params, err, test.wantError)
		}
	}
}

// 端到端：op 表 → 合成请求 → handler 查库。insights 全族用 r.PathValue 取参
// （不是 fs / git 那些域用的 chi.URLParam），而真实路由靠 chi 的 path_value 桥
// 才让 PathValue 有值 —— 合成请求那条桥必须由 invokeLocalHandler 自己补上。
// 少了它，这条链上每一环单独看都对（白名单、校验器、桌面端真实请求全绿），
// 手机端却是整域 404。用例：list 认得项目、findingID 经 PathValue 命中那一行。
func TestRelayInsightsEndToEndResolvesPathValues(t *testing.T) {
	server := newTestServer(t)
	projectRoot := t.TempDir()
	now := time.Now().UTC()
	if _, err := server.db.Exec(
		`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('file-project','file-project',?,?,'main',1,?)`,
		projectRoot, server.localRunnerID(), now,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const findingID = "0b8fd035-9f45-4a5e-9f61-2f6b1f6b1f6b"
	if _, err := server.db.Exec(
		`insert into project_insight_scans (id,project_id,status,created_at) values ('scan-1','file-project','completed',?)`, now,
	); err != nil {
		t.Fatalf("insert scan: %v", err)
	}
	if _, err := server.db.Exec(
		`insert into project_insights (id,project_id,scan_id,type,severity,title,summary,status,created_at,updated_at)
		 values (?, 'file-project','scan-1','optimization','normal','标题','摘要','open',?,?)`, findingID, now, now,
	); err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	// list：projectID 只能经 PathValue 桥进来，进不来就是 404 "Not Found"。
	status, payload := relayRPC(t, server, map[string]any{"op": "insights.list", "projectId": "file-project"})
	if status != http.StatusOK || payload["ok"] != true {
		t.Fatalf("insights.list failed: status=%d body=%v", status, payload)
	}
	data, _ := payload["data"].(map[string]any)
	findings, _ := data["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want the seeded one", data["findings"])
	}

	// 单条操作：findingID 经 PathParams 声明位 → URL 路径 → PathValue。
	status, payload = relayRPC(t, server, map[string]any{
		"op": "insights.findingDismiss", "projectId": "file-project", "params": map[string]any{"findingID": findingID},
	})
	if status != http.StatusOK || payload["ok"] != true {
		t.Fatalf("insights.findingDismiss failed: status=%d body=%v", status, payload)
	}
	var gotStatus string
	if err := server.db.QueryRow(`select status from project_insights where id=?`, findingID).Scan(&gotStatus); err != nil {
		t.Fatalf("load finding: %v", err)
	}
	if gotStatus != insightStatusDismissed {
		t.Fatalf("finding status = %q, want %q", gotStatus, insightStatusDismissed)
	}
}
