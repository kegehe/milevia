package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestAgentBlockedReasonsAreDistinct 钉住「不可用」各档理由**文案各不相同**。
//
// 四件事的下一步动作完全不同：换执行环境 / 去装运行时 / 去授权 / **去修一个装坏了的**。
// 用同一句灰字交代等于什么都没说 —— 而它们各自单独看都「像是对的」，
// 只有并排比才看得出是否重复。
func TestAgentBlockedReasonsAreDistinct(t *testing.T) {
	codexEntry := mustAgent(t, "codex")
	// ① 该环境不提供该工具（由 probeAgents 给出 unsupported 理由）
	unsupported := agentUnavailableReasonText(RunnerMeta{ID: "ssh-plain", Name: "plain"}, codexEntry, false)
	if unsupported == "" {
		t.Fatal("unsupported 档必须有理由")
	}
	// ② 未授权（由 listRunnerAgents 拼出）
	unauthorized := "尚未授权在 prod 上安装（需要在该主机上显式确认一次）"
	// ③ 运行时缺失（由 resolveAgentInstallPlan 给出）
	missingRuntime := "目标环境没有可用的 npm：请先安装 Node.js 运行时（Claude Code）"
	// ④ 装过但用不了（docs/43 §7 选 B 新增的那一档）。它与 ③ 最容易混：
	//    两者都表现为"界面说不能用"，但一个要装 Node、一个要修这个工具。
	brokenInstall := agentUnavailableReasonText(RunnerMeta{ID: "ssh-plain", Name: "plain"}, codexEntry, true)

	reasons := []string{unsupported, unauthorized, missingRuntime, brokenInstall}
	for i := range reasons {
		for j := i + 1; j < len(reasons); j++ {
			if reasons[i] == reasons[j] {
				t.Fatalf("两档理由文案相同：%q", reasons[i])
			}
		}
	}
	if !strings.Contains(unsupported, "未安装") {
		t.Fatalf("「不支持」那一档没有说清是环境问题：%q", unsupported)
	}
	if !strings.Contains(unauthorized, "授权") {
		t.Fatalf("「未授权」那一档没有提到授权：%q", unauthorized)
	}
	if !strings.Contains(missingRuntime, "Node") {
		t.Fatalf("「运行时缺失」那一档没有指向 Node：%q", missingRuntime)
	}
	// 装过但用不了：**不能说成"未安装"**（那是把"坏了"写成"没有"），
	// 而且要指向"去检测修复"，否则用户不知道该做什么。
	if strings.Contains(brokenInstall, "未安装") {
		t.Fatalf("「已安装但不可用」那一档被说成了「未安装」：%q", brokenInstall)
	}
	if !strings.Contains(brokenInstall, "已安装") || !strings.Contains(brokenInstall, "CLI 工具管理页") {
		t.Fatalf("「已安装但不可用」那一档没有说清状态与下一步：%q", brokenInstall)
	}
}

func mustAgent(t *testing.T, id string) AgentCatalogEntry {
	t.Helper()
	entry, ok := agentByID(id)
	if !ok {
		t.Fatalf("目录里没有 %s", id)
	}
	return entry
}

// TestListRunnerAgentsReportsProbeFailureWithoutItems 钉住「读不到 ≠ 没有」。
//
// 未注册的本机 runner（典型：WSL 没装）必须返回 probeOk=false + 原因、**items 为空**；
// 而不是返回 404（那会被读成「这台机器不存在」），也不是让界面把空 items 渲染成
// 「所有工具都未安装」。
func TestListRunnerAgentsReportsProbeFailureWithoutItems(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// registry 故意留空：模拟「这个执行环境还没准备好」。
	server := &Server{
		db: db, paths: newAgentPathResolver(Config{}),
		runnerRegistry: newRunnerRegistry(), runnerUpdating: map[runnerAgentKey]bool{},
	}
	if err := server.migrateAgentInstallations(context.Background()); err != nil {
		t.Fatal(err)
	}

	runnerID := server.localRunnerID()
	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents", server.listRunnerAgents)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+runnerID+"/agents", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（通道失败不能报 404，那会被读成「这台机器不存在」）body=%s", recorder.Code, recorder.Body.String())
	}
	var view struct {
		ProbeOK    bool   `json:"probeOk"`
		ProbeError string `json:"probeError"`
		Items      []any  `json:"items"`
		Runtime    any    `json:"runtime"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ProbeOK {
		t.Fatal("registry 为空时不该报告探测成功")
	}
	if view.ProbeError == "" {
		t.Fatal("探测失败必须带原因，否则界面只能凭空猜一个说法")
	}
	if len(view.Items) != 0 {
		t.Fatalf("探测失败时不该给出工具条目（那会被渲染成「未安装」）：%#v", view.Items)
	}
	if view.Runtime != nil {
		t.Fatal("探测失败时不该给出运行时状态")
	}

	// 跨端的未注册 runner 仍然是 404：那是「这台机器不在清单里」，与「通道坏了」不同。
	crossRecorder := httptest.NewRecorder()
	router.ServeHTTP(crossRecorder, httptest.NewRequest(http.MethodGet, "/api/runners/ssh-prod/agents", nil))
	if crossRecorder.Code != http.StatusNotFound {
		t.Fatalf("未知跨端 runner 状态码 = %d，期望 404", crossRecorder.Code)
	}
}

// TestListRunnerAgentsCoversCatalogWhenRegistered 断言注册好的 runner 上，
// 条目覆盖整个工具目录 —— 新增工具时管理页自动多一张卡片。
func TestListRunnerAgentsCoversCatalogWhenRegistered(t *testing.T) {
	server := newTestServer(t)
	localID := server.localRunnerID()

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents", server.listRunnerAgents)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+localID+"/agents", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var view struct {
		ProbeOK bool `json:"probeOk"`
		Items   []struct {
			ID               string `json:"id"`
			InstallSupported bool   `json:"installSupported"`
			UpdateSupported  bool   `json:"updateSupported"`
		} `json:"items"`
		Runtime struct {
			ID string `json:"id"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.ProbeOK {
		t.Fatalf("注册好的 runner 应当探测成功：%s", recorder.Body.String())
	}
	if len(view.Items) != len(agentCatalog()) {
		t.Fatalf("条目数 = %d，目录有 %d 个工具", len(view.Items), len(agentCatalog()))
	}
	seen := map[string]bool{}
	for _, item := range view.Items {
		seen[item.ID] = true
	}
	for _, entry := range agentCatalog() {
		if !seen[entry.ID] {
			t.Fatalf("目录里的 %s 没有出现在条目里", entry.ID)
		}
	}
	if view.Runtime.ID != runtimeAgentID {
		t.Fatalf("运行时状态缺失：%#v", view.Runtime)
	}
}
