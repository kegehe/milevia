package app

import (
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

// runnerRegistry manages all available Runners. It is safe for concurrent use.
type runnerRegistry struct {
	mu      sync.RWMutex
	runners map[string]AgentRunner // runnerID → runner instance
	metas   map[string]RunnerMeta  // runnerID → metadata
}

func newRunnerRegistry() *runnerRegistry {
	return &runnerRegistry{
		runners: make(map[string]AgentRunner),
		metas:   make(map[string]RunnerMeta),
	}
}

// register adds a Runner to the registry. It overwrites any existing entry
// with the same ID.
func (reg *runnerRegistry) register(id string, runner AgentRunner, meta RunnerMeta) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.runners[id] = runner
	reg.metas[id] = meta
}

// unregister removes a Runner from the registry. It is a no-op when the ID
// is not registered.
func (reg *runnerRegistry) unregister(id string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	delete(reg.runners, id)
	delete(reg.metas, id)
}

// list returns metadata for every registered Runner. The returned slice is a
// copy — callers may mutate it freely.
func (reg *runnerRegistry) list() []RunnerMeta {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]RunnerMeta, 0, len(reg.metas))
	for _, m := range reg.metas {
		out = append(out, m)
	}
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
	m, ok := reg.metas[id]
	return m, ok
}
