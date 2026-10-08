package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunnerRegistryListOrderIsStable 回归：执行环境标签的顺序曾经每次刷新都变。
//
// 根因是 list() 直接遍历 metas 这个 map —— Go 的 map 迭代顺序是随机的，同一份注册
// 内容两次调用就能给出两种顺序。用户看到的就是 Cli管理 页顶部的
// Windows / WSL / SSH 标签一会儿一个样。
//
// ⚠️ 断言"多跑几次都一样"，而不是只比一次：只比一次的话，旧实现有 1/n! 的概率
// 蒙对（n 个 runner），这个用例就形同虚设。
func TestRunnerRegistryListOrderIsStable(t *testing.T) {
	reg := newRunnerRegistry()
	// 故意乱序注册：注册先后不是期望顺序，期望顺序由 environment 分档决定。
	reg.register("ssh-b", nil, RunnerMeta{ID: "ssh-b", Name: "build-host", Environment: "remote-linux"})
	reg.register("wsl-local", nil, RunnerMeta{ID: "wsl-local", Name: "WSL Local Runner", Environment: "wsl"})
	reg.register("ssh-a", nil, RunnerMeta{ID: "ssh-a", Name: "prod", Environment: "remote-linux"})
	reg.register("windows-local", nil, RunnerMeta{ID: "windows-local", Name: "Windows Local Runner", Environment: "windows"})

	want := []string{"windows-local", "wsl-local", "ssh-b", "ssh-a"}
	for attempt := 0; attempt < 50; attempt++ {
		got := runnerIDs(reg.list())
		if !equalIDs(got, want) {
			t.Fatalf("第 %d 次读取的执行环境顺序是 %v，期望 %v（本机 Windows → 本机 WSL → 各 SSH，同档内按添加先后）", attempt+1, got, want)
		}
	}
}

// TestRunnerRegistryListOrderSurvivesReregisterAndUnregister 钉住两条位置规则：
// 重连（同 ID 再注册）保持原位；删除只摘掉自己，不影响别人的先后。
func TestRunnerRegistryListOrderSurvivesReregisterAndUnregister(t *testing.T) {
	reg := newRunnerRegistry()
	reg.register("windows-local", nil, RunnerMeta{ID: "windows-local", Environment: "windows"})
	reg.register("ssh-a", nil, RunnerMeta{ID: "ssh-a", Environment: "remote-linux"})
	reg.register("ssh-b", nil, RunnerMeta{ID: "ssh-b", Environment: "remote-linux"})

	// SSH 重连走的就是这条路径：同一个 ID 再注册一次。
	reg.register("ssh-a", nil, RunnerMeta{ID: "ssh-a", Environment: "remote-linux"})
	if got, want := runnerIDs(reg.list()), []string{"windows-local", "ssh-a", "ssh-b"}; !equalIDs(got, want) {
		t.Fatalf("重连后顺序变成 %v，期望 %v（重连不是新机器，不该被甩到末尾）", got, want)
	}

	reg.unregister("ssh-a")
	if got, want := runnerIDs(reg.list()), []string{"windows-local", "ssh-b"}; !equalIDs(got, want) {
		t.Fatalf("删除 ssh-a 后顺序是 %v，期望 %v", got, want)
	}
	// 删掉再重新添加 = 新机器，排到同档末尾。
	reg.register("ssh-a", nil, RunnerMeta{ID: "ssh-a", Environment: "remote-linux"})
	if got, want := runnerIDs(reg.list()), []string{"windows-local", "ssh-b", "ssh-a"}; !equalIDs(got, want) {
		t.Fatalf("重新添加 ssh-a 后顺序是 %v，期望 %v", got, want)
	}
}

// TestRunnerRegistryListPutsLazyWSLBeforeSSH 钉住分档的判据是 environment 而非注册顺序：
// Windows 服务端启动期 WSL 冷启动探测超时，wsl-local 会在 SSH 恢复之后才补注册
// （ensureWSLRunner），此时它仍必须排在各台 SSH 之前。
func TestRunnerRegistryListPutsLazyWSLBeforeSSH(t *testing.T) {
	reg := newRunnerRegistry()
	reg.register("windows-local", nil, RunnerMeta{ID: "windows-local", Environment: "windows"})
	reg.register("ssh-a", nil, RunnerMeta{ID: "ssh-a", Environment: "remote-linux"})
	reg.register("wsl-local", nil, RunnerMeta{ID: "wsl-local", Environment: "wsl"})

	if got, want := runnerIDs(reg.list()), []string{"windows-local", "wsl-local", "ssh-a"}; !equalIDs(got, want) {
		t.Fatalf("补注册的 wsl-local 顺序是 %v，期望 %v", got, want)
	}
}

// TestRunnerRegistryListCoversEveryRegisteredRunner 保证排序没有把谁漏掉：
// 排序是按 order 重建的，一旦两边不同步（漏注册或漏删）就会在界面上少一个执行环境。
func TestRunnerRegistryListCoversEveryRegisteredRunner(t *testing.T) {
	reg := newRunnerRegistry()
	reg.register("windows-local", nil, RunnerMeta{ID: "windows-local", Environment: "windows"})
	reg.register("ssh-a", nil, RunnerMeta{ID: "ssh-a", Environment: "remote-linux"})
	reg.unregister("nobody")
	if got := reg.list(); len(got) != 2 {
		t.Fatalf("list() 返回 %d 项，期望 2 项", len(got))
	}
	// 返回值必须是副本：调用方改它不该影响 registry 内部状态。
	got := reg.list()
	got[0].Name = "被调用方改过的名字"
	if reg.list()[0].Name == "被调用方改过的名字" {
		t.Fatal("list() 返回的是内部切片，调用方一改就污染 registry")
	}
}

// TestListRunnersEndpointKeepsEnvironmentOrder 走真实路由 GET /api/runners 反复取值，
// 断言接口给出的顺序固定。这是用户实际看到的那一层 —— registry 排对了但处理器又自己
// 遍历一遍 map 的话，界面上照样会飘，所以两条都要钉。
//
// 顺序在 Windows 与 Linux 服务端上一致：本机两档（Windows 在前、WSL 在后）分档靠
// environment，跟本机是哪一档无关。
func TestListRunnersEndpointKeepsEnvironmentOrder(t *testing.T) {
	server := newTestServer(t)
	stub := runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	// New() 已经注册了本平台的 local runner（Windows → windows-local，Linux → wsl-local）。
	server.runnerRegistry.register("windows-local", stub, RunnerMeta{ID: "windows-local", Name: "Windows Local Runner", Environment: "windows"})
	server.runnerRegistry.register("wsl-local", stub, RunnerMeta{ID: "wsl-local", Name: "WSL Local Runner", Environment: "wsl"})
	server.runnerRegistry.register("ssh-a", stub, RunnerMeta{ID: "ssh-a", Name: "prod", Environment: "remote-linux"})
	server.runnerRegistry.register("ssh-b", stub, RunnerMeta{ID: "ssh-b", Name: "build-host", Environment: "remote-linux"})

	want := []string{"windows-local", "wsl-local", "ssh-a", "ssh-b"}
	for attempt := 0; attempt < 20; attempt++ {
		recorder := httptest.NewRecorder()
		server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /api/runners 状态码 %d body=%s", recorder.Code, recorder.Body.String())
		}
		var payload []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("解析 /api/runners 响应失败：%v", err)
		}
		ids := make([]string, 0, len(payload))
		for _, item := range payload {
			ids = append(ids, item.ID)
		}
		if !equalIDs(ids, want) {
			t.Fatalf("第 %d 次 GET /api/runners 的顺序是 %v，期望 %v", attempt+1, ids, want)
		}
	}
}

// TestRunnerRegistryKeysByRegistrationID 钉住查找用的是**注册时的那个 ID**，不是 meta.ID。
//
// 两者可以不一致 —— app_test.go 里就有一处 `register(server.localRunnerID(), …, server.wslLocalMeta())`，
// 在 Windows 上等于用 "windows-local" 这个 key 注册了一份 ID 写着 "wsl-local" 的元信息。
// 一旦有人图省事改成用 meta.ID 反查，这类条目就会查不到、并从 /api/runners 里消失。
func TestRunnerRegistryKeysByRegistrationID(t *testing.T) {
	reg := newRunnerRegistry()
	reg.register("windows-local", nil, RunnerMeta{ID: "wsl-local", Name: "WSL Local Runner", Environment: "wsl"})

	meta, ok := reg.getMeta("windows-local")
	if !ok || meta.ID != "wsl-local" {
		t.Fatalf("按注册 ID 查不到：ok=%v meta=%+v", ok, meta)
	}
	if _, ok := reg.getMeta("wsl-local"); ok {
		t.Fatal("用 meta.ID 也能查到 —— 查找的键跑偏了")
	}
	// list() 给出去的是元信息本身（所以这里是 meta.ID）。
	if got := runnerIDs(reg.list()); !equalIDs(got, []string{"wsl-local"}) {
		t.Fatalf("list() 给出 %v，期望 [wsl-local]", got)
	}
	// 同一个 key 再注册一次是原地替换，不新增一项。
	reg.register("windows-local", nil, RunnerMeta{ID: "wsl-local", Name: "换过名字", Environment: "wsl"})
	if got := reg.list(); len(got) != 1 || got[0].Name != "换过名字" {
		t.Fatalf("原地替换后 list() = %+v，期望仍是一项且名字已更新", got)
	}
}

func runnerIDs(metas []RunnerMeta) []string {
	ids := make([]string, 0, len(metas))
	for _, meta := range metas {
		ids = append(ids, meta.ID)
	}
	return ids
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
