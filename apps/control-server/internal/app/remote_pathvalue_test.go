package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

// invokeLocalHandler 用 chi 路由上下文合成请求（没有真的走路由匹配）。真实路由在
// 匹配后会把 URLParams 桥接进 Request.SetPathValue（chi path_value.go），合成请求
// 绕过了那一步 —— 这里钉住"两个取参口径都读得到"：chi.URLParam（fs / git / 会话
// 域在用）与 r.PathValue（insights 全族在用）。少了后者，那个域经中继读到的
// projectID / findingID 全是空串，症状是"电脑上正常、手机端整域 404"。
func TestInvokeLocalHandlerBridgesPathValue(t *testing.T) {
	var viaPathValueProject, viaPathValueFinding string
	var viaChiProject string
	handler := func(w http.ResponseWriter, r *http.Request) {
		viaPathValueProject = r.PathValue("projectID")
		viaPathValueFinding = r.PathValue("findingID")
		viaChiProject = chi.URLParam(r, "projectID")
	}
	invokeLocalHandler(context.Background(), http.MethodGet, "/api/projects/p1/insights", "p1", "", "", handler, nil, "findingID", "f1")
	if viaPathValueProject != "p1" {
		t.Fatalf("r.PathValue(projectID) = %q, want p1", viaPathValueProject)
	}
	if viaPathValueFinding != "f1" {
		t.Fatalf("r.PathValue(findingID) = %q, want f1", viaPathValueFinding)
	}
	if viaChiProject != "p1" {
		t.Fatalf("chi.URLParam(projectID) = %q, want p1", viaChiProject)
	}
}
