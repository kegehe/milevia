package app

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 项目列表刷新与连通性探测解耦。
//
// 背景：GET /api/projects 过去会对每个远端主机（SSH runner）与每个跨端目标
// （Windows⇄WSL）同步做一次 codex 就绪探测。而项目总览页每 30 秒、以及每收到一次
// WS 事件都会全量拉取该列表——只要有一台远端主机响应慢或离线，整个列表页就要等
// 数秒才有反应（5s 超时 × 多台主机，串行累加）。
//
// 本文件把这条路径拆成两截：
//
//	GET /api/projects               纯数据库读取 + 本机 PATH 级就绪判定，毫秒返回；
//	GET /api/projects/availability  单独承担远端/跨端连通性探测，供前端异步合并。
//
// 探测本身也做了收敛：按 runner/跨端目标去重（多个项目共享一条 SSH 连接只探一次）、
// 并发有上限、单次与整批都有超时，并对结果做短 TTL 缓存以吸收 WS 事件抖动带来的重复请求。

const (
	// projectAvailabilityProbeTTL 是连通性探测结果的保鲜期。项目总览页在 WS 事件驱动下
	// 可能短时间内多次刷新列表，TTL 内直接复用上次探测结果，避免对远端主机形成探测风暴。
	projectAvailabilityProbeTTL = 5 * time.Second

	// projectAvailabilityProbeTimeout 是单次探测的上限：超过即判定为不可用。
	projectAvailabilityProbeTimeout = 5 * time.Second

	// projectAvailabilityParallelism 限制同时进行的探测数量，避免一次刷新对多台远端主机
	// 同时建立 SSH 通道。
	projectAvailabilityParallelism = 8

	// projectAvailabilityBudget 是整批探测的总预算：极端情况下（大量远端主机离线）
	// 本接口也必须按时返回；未在预算内完成的探测按"不可用"处理，不会拖住前端。
	projectAvailabilityBudget = 12 * time.Second

	// codexProbeKeyLocal 是本机 codex 就绪探测（exec.LookPath，无任何远端 I/O）的去重键。
	// 它足够廉价，可以在项目列表接口里同步求值；其余探测一律交给 availability 接口。
	codexProbeKeyLocal = "local-codex"
)

// codexProbe 描述一次 codex 就绪探测。
type codexProbe struct {
	// key 是去重键：一次刷新中相同 key 只探测一次（SSH 按 runner、跨端按目标环境）。
	key string
	// local 为 true 表示该探测只做本机 PATH 查找（无远端 I/O），可同步求值。
	local bool
	run   func(context.Context) bool
}

// codexProbeCacheEntry 是某个探测键上次的连通性结果及其时间戳。
type codexProbeCacheEntry struct {
	ready bool
	at    time.Time
}

// projectAvailabilityItem 是 GET /api/projects/availability 的单条结果。
type projectAvailabilityItem struct {
	ID          string `json:"id"`
	ClaudeReady bool   `json:"claudeReady"`
	CodexReady  bool   `json:"codexReady"`
	AgentReady  bool   `json:"agentReady"`
}

// projectRowsForListing 读取项目列表的公共行集（含落库的 claude_ready）。
// 它只做纯粹的数据库读取，不触发任何远端/跨端连通性探测，并在返回前释放 SQLite
// 连接——调用方随后可能进行耗时探测，不能把连接占在手里。
func (s *Server) projectRowsForListing(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `select id,name,path,coalesce(nullif(runner_id,''),runner),git_branch,claude_ready,created_at from projects order by created_at desc`)
	if err != nil {
		return nil, err
	}
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.Runner, &p.GitBranch, &p.ClaudeReady, &p.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		p.RunnerID = p.Runner
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return projects, nil
}

// projectCodexProbe 返回项目 codex 就绪探测的描述。返回零值（key 为空）表示该项目
// 无法确定 codex 就绪，直接判为不可用。
//
// 判定口径与 decorateProjectAvailability 完全一致：按项目的目标环境选探测对象，
// 保证"探测的是项目所在环境那一端的 CLI"。
func (s *Server) projectCodexProbe(project *Project) codexProbe {
	switch {
	case isLocalRunnerID(project.Runner):
		target := s.resolveAgentTargetEnv(project.Runner, project.Path)
		if target == agentTargetEnvWSL || (target == agentTargetEnvWindows && runtime.GOOS == "windows") {
			// 服务端本机环境：仅 exec.LookPath，无远端 I/O。
			return codexProbe{key: codexProbeKeyLocal, local: true, run: func(ctx context.Context) bool {
				return s.codexRunner.Ready(ctx)
			}}
		}
		// 跨端（如 WSL 服务端下的 /mnt/ 项目）：要去 Windows 侧探测。
		return codexProbe{key: "target:" + string(target), run: func(ctx context.Context) bool {
			return s.codexReadyForTarget(ctx, target)
		}}
	case project.Runner == "wsl-local":
		// Windows 服务端 + wsl-local：Codex 在 WSL 侧，跨端探测其真实就绪，
		// 不误用本机 Windows codex。
		return codexProbe{key: "runner:" + project.Runner, run: func(ctx context.Context) bool {
			return s.codexReadyForTarget(ctx, agentTargetEnvWSL)
		}}
	case strings.HasPrefix(project.Runner, "ssh-"):
		runnerID := project.Runner
		return codexProbe{key: "runner:" + runnerID, run: func(ctx context.Context) bool {
			return s.sshCodexReadyContext(ctx, runnerID)
		}}
	default:
		return codexProbe{}
	}
}

// sshCodexReadyContext 报告给定 SSH runner 背后的远端主机上 Codex 是否可用。
// 超时由调用方通过 ctx 控制（探测集合统一预算见 probeProjectCodexReadiness）。
func (s *Server) sshCodexReadyContext(ctx context.Context, runnerID string) bool {
	r, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		return false
	}
	codexR, ok := r.(CodexCapableRunner)
	if !ok {
		return false
	}
	return codexR.CodexReady(ctx)
}

// freshCodexProbeCache 返回仍在保鲜期内的探测结果（key → ready），并顺带淘汰过期条目
// ——否则已删除主机的陈旧条目会一直留在 map 里（只在超过 256 条时才被裁剪）。
func (s *Server) freshCodexProbeCache() map[string]bool {
	cutoff := time.Now().Add(-projectAvailabilityProbeTTL)
	s.availabilityMu.Lock()
	defer s.availabilityMu.Unlock()
	fresh := map[string]bool{}
	for key, entry := range s.codexProbeCache {
		if entry.at.Before(cutoff) {
			delete(s.codexProbeCache, key)
			continue
		}
		fresh[key] = entry.ready
	}
	return fresh
}

// storeCodexProbeCache 写入探测结果。探测键按 runner/跨端目标去重，数量与远端主机数
// 同阶；仅作为异常增长时的兜底，超过上限再清理一轮过期项。
func (s *Server) storeCodexProbeCache(entries map[string]codexProbeCacheEntry) {
	s.availabilityMu.Lock()
	defer s.availabilityMu.Unlock()
	if s.codexProbeCache == nil {
		s.codexProbeCache = map[string]codexProbeCacheEntry{}
	}
	for key, entry := range entries {
		s.codexProbeCache[key] = entry
	}
	if len(s.codexProbeCache) > 256 {
		cutoff := time.Now().Add(-projectAvailabilityProbeTTL)
		for key, entry := range s.codexProbeCache {
			if entry.at.Before(cutoff) {
				delete(s.codexProbeCache, key)
			}
		}
	}
}

// probeProjectCodexReadiness 计算每个项目的 codex 就绪度，返回 projectID → ready。
// 先按探测键去重，再并发探测；TTL 内命中的键不再触碰远端。
func (s *Server) probeProjectCodexReadiness(ctx context.Context, projects []Project) map[string]bool {
	probes := make(map[string]codexProbe, len(projects))
	projectKeys := make(map[string]string, len(projects))
	for index := range projects {
		p := &projects[index]
		probe := s.projectCodexProbe(p)
		if probe.key == "" {
			continue
		}
		projectKeys[p.ID] = probe.key
		if _, ok := probes[probe.key]; !ok {
			probes[probe.key] = probe
		}
	}

	result := make(map[string]bool, len(probes))
	pending := make([]codexProbe, 0, len(probes))
	for key, ready := range s.freshCodexProbeCache() {
		if _, needed := probes[key]; needed {
			result[key] = ready
		}
	}
	for key, probe := range probes {
		if _, cached := result[key]; cached {
			continue
		}
		pending = append(pending, probe)
	}

	if len(pending) > 0 {
		ready := make([]bool, len(pending))
		budgetCtx, cancel := context.WithTimeout(ctx, projectAvailabilityBudget)
		defer cancel()
		slots := make(chan struct{}, projectAvailabilityParallelism)
		var wg sync.WaitGroup
		for index, probe := range pending {
			wg.Add(1)
			go func(index int, probe codexProbe) {
				defer wg.Done()
				select {
				case slots <- struct{}{}:
				case <-budgetCtx.Done():
					// 预算耗尽：按不可用处理，保证接口按时返回。
					return
				}
				defer func() { <-slots }()
				probeCtx, cancelProbe := context.WithTimeout(budgetCtx, projectAvailabilityProbeTimeout)
				defer cancelProbe()
				ready[index] = probe.run(probeCtx)
			}(index, probe)
		}
		wg.Wait()

		entries := make(map[string]codexProbeCacheEntry, len(pending))
		at := time.Now()
		for index, probe := range pending {
			result[probe.key] = ready[index]
			entries[probe.key] = codexProbeCacheEntry{ready: ready[index], at: at}
		}
		s.storeCodexProbeCache(entries)
	}

	byProject := make(map[string]bool, len(projectKeys))
	for id, key := range projectKeys {
		byProject[id] = result[key]
	}
	return byProject
}

// listProjectAvailability 单独承担"连通性探测"，与 GET /api/projects 的列表刷新解耦。
// 前端先拿到列表（毫秒级）再异步合并这里的结果，因此某台远端主机慢或离线只会延迟
// 就绪度徽标的更新，不会再拖住整个项目列表页。
func (s *Server) listProjectAvailability(w http.ResponseWriter, r *http.Request) {
	projects, err := s.projectRowsForListing(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	codexReady := s.probeProjectCodexReadiness(r.Context(), projects)
	items := make([]projectAvailabilityItem, 0, len(projects))
	for index := range projects {
		p := &projects[index]
		ready := codexReady[p.ID]
		items = append(items, projectAvailabilityItem{
			ID:          p.ID,
			ClaudeReady: p.ClaudeReady,
			CodexReady:  ready,
			AgentReady:  p.ClaudeReady || ready,
		})
	}
	writeJSON(w, http.StatusOK, items)
}
