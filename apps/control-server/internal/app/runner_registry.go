package app

import (
	"sort"
	"sync"
)

// RootEntry describes a directory root accessible through a Runner.
type RootEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
}

// RunnerMeta holds the metadata for a registered Runner.
type RunnerMeta struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Environment string      `json:"environment"` // "wsl", "remote-linux"
	Host        string      `json:"host,omitempty"`
	Root        string      `json:"root"`
	Roots       []RootEntry `json:"roots"`
}

// runnerMetaEntry 是 registry 里的一项：注册时用的 ID，与它对外公布的元信息。
//
// ⚠️ 两者**不保证相等** —— 调用方可以拿 A 的 ID 注册 B 的元信息（app_test.go 里就
// 有一处：`register(server.localRunnerID(), …, server.wslLocalMeta())`）。所以查找
// 一律以这里的 id 为准，绝不能用 meta.ID 反查。
type runnerMetaEntry struct {
	id   string
	meta RunnerMeta
}

// runnerRegistry manages all available Runners. It is safe for concurrent use.
type runnerRegistry struct {
	mu      sync.RWMutex
	runners map[string]AgentRunner // runnerID → runner instance
	// metas 按**首次注册的先后**排列，这个先后就是界面上的顺序（见 list）。
	// 刻意用切片而不是 map：map 的迭代顺序是随机的，直接遍历它会让 Cli管理 页
	// 顶部那排 Windows / WSL / SSH 每次刷新都重排。用切片也让"顺序"与"内容"
	// 只有一份状态，不存在两者不同步、某个执行环境从界面上静默消失的可能。
	metas []runnerMetaEntry
}

func newRunnerRegistry() *runnerRegistry {
	return &runnerRegistry{runners: make(map[string]AgentRunner)}
}

// register adds a Runner to the registry. It overwrites any existing entry
// with the same ID.
func (reg *runnerRegistry) register(id string, runner AgentRunner, meta RunnerMeta) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.runners[id] = runner
	// 已注册过的 ID（如 SSH 重连）**原地替换**：位置跟着"这台机器什么时候来的"，
	// 重连不是新机器，不该把它甩到列表末尾。
	for index := range reg.metas {
		if reg.metas[index].id == id {
			reg.metas[index].meta = meta
			return
		}
	}
	reg.metas = append(reg.metas, runnerMetaEntry{id: id, meta: meta})
}

// unregister removes a Runner from the registry. It is a no-op when the ID
// is not registered.
func (reg *runnerRegistry) unregister(id string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	delete(reg.runners, id)
	for index := range reg.metas {
		if reg.metas[index].id == id {
			reg.metas = append(reg.metas[:index], reg.metas[index+1:]...)
			return
		}
	}
}

// runnerEnvironmentRank 决定执行环境在界面上的先后：本机 Windows → 本机 WSL → 远端。
//
// 判据用 `environment` 而不是 ID 或注册顺序：Windows 服务端的 wsl-local 启动期可能
// 探测超时（WSL 冷启动），由 ensureWSLRunner 在 SSH 恢复**之后**才补注册 —— 只按
// 注册顺序排会把 WSL 挤到各台 SSH 后面。
func runnerEnvironmentRank(environment string) int {
	switch environment {
	case "windows":
		return 0
	case "wsl":
		return 1
	default:
		return 2
	}
}

// list returns metadata for every registered Runner, in a stable order:
// 本机 Windows、本机 WSL、然后各远端（SSH），同档内按注册先后。
// The returned slice is a copy — callers may mutate it freely.
func (reg *runnerRegistry) list() []RunnerMeta {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]RunnerMeta, 0, len(reg.metas))
	for _, entry := range reg.metas {
		out = append(out, entry.meta)
	}
	// ⚠️ SliceStable：同档内保持上面的注册顺序，排序只做"分档"，不打乱档内先后。
	sort.SliceStable(out, func(i, j int) bool {
		return runnerEnvironmentRank(out[i].Environment) < runnerEnvironmentRank(out[j].Environment)
	})
	return out
}

// all returns a snapshot of registered runners for lifecycle management.
func (reg *runnerRegistry) all() []AgentRunner {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]AgentRunner, 0, len(reg.runners))
	for _, runner := range reg.runners {
		out = append(out, runner)
	}
	return out
}

// get returns the Runner instance for the given ID. The boolean is false when
// no Runner with that ID is registered.
func (reg *runnerRegistry) get(id string) (AgentRunner, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r, ok := reg.runners[id]
	return r, ok
}

// getMeta returns the metadata for the given runner ID.
func (reg *runnerRegistry) getMeta(id string) (RunnerMeta, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, entry := range reg.metas {
		if entry.id == id {
			return entry.meta, true
		}
	}
	return RunnerMeta{}, false
}
