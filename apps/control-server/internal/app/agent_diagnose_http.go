package app

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
)

// 诊断的 HTTP 入口。两个都**只读**（见 agent_diagnose.go 顶部的第 1 条纪律）。
//
// 与其它 agent 路由的差别是刻意的：这两个端点**不要求**该工具在目标环境上可用
// —— 诊断恰恰要回答"为什么不可用"。复用 resolveAgent 的失败分支会让这类诊断
// 变成 404（"不支持的工具"），而那正是最需要诊断的一档。

// resolveDiagnosisTarget 解析 {runnerID} + {agentID}，但不要求后端存在。
func (s *Server) resolveDiagnosisTarget(runnerID, agentID string) (RunnerMeta, AgentCatalogEntry, error) {
	entry, ok := agentByID(agentID)
	if !ok {
		return RunnerMeta{}, AgentCatalogEntry{}, fmt.Errorf("不支持的工具 %s", agentID)
	}
	if runnerID == "wsl-local" {
		// 与 runnerStatus / listRunnerAgents 一致：WSL 可能因冷启动超时未注册，
		// 按需补一次探测。
		s.ensureWSLRunner()
	}
	meta, ok := s.runnerRegistry.getMeta(runnerID)
	if !ok {
		if !isLocalRunnerID(runnerID) {
			return RunnerMeta{}, AgentCatalogEntry{}, errors.New("runner not found")
		}
		// 本机 runner 在极端情况下可能未注册。诊断仍按"本机"语义处理。
		meta = RunnerMeta{ID: runnerID, Name: runnerID, Environment: "local"}
	}
	return meta, entry, nil
}

// diagnoseAgentHandler 是 GET /api/runners/{runnerID}/agents/{agentID}/diagnose。
func (s *Server) diagnoseAgentHandler(w http.ResponseWriter, r *http.Request) {
	meta, entry, err := s.resolveDiagnosisTarget(chi.URLParam(r, "runnerID"), chi.URLParam(r, "agentID"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	report := s.buildAgentDiagnosis(r.Context(), meta, entry)
	// 这次详查是**实测**，它的结论可能推翻缓存里那份批量报告（比如那份说"就绪"、
	// 而这次查出"二进制坏了"）。不作废的话，用户切走再回来会看到旧报告把这次的结论
	// 盖回去 —— 同一台机器上两份报告给出相反结论，而界面无从分辨该信哪个。
	//
	// 连读数一起作废（而不是只清诊断）：上面那次 probeAgent 已经实测过状态，缓存里
	// 那份可能比它旧；只清诊断会让下一轮批量诊断拿旧读数去判"值不值得详查"，于是刚查
	// 出问题的工具反而不被详查、显示成"本轮没有详查"。
	s.invalidateAgentReadings(meta.ID)
	writeJSON(w, http.StatusOK, report)
}

// runnerDiagnosticsView 是 GET /api/runners/{runnerID}/diagnostics 的响应。
type runnerDiagnosticsView struct {
	RunnerID   string `json:"runnerId"`
	ProbeOK    bool   `json:"probeOk"`
	ProbeError string `json:"probeError,omitempty"`
	// Items 只包含**详查过**的工具（含结论为 ok 的）。
	Items []agentDiagnosis `json:"items"`
	// Skipped 是"这一轮没有详查"的工具 id。界面**不能**把它们渲染成"没问题"
	// —— 那是把"没查"写成"没有"，本项目反复禁止的那一类。
	Skipped []string `json:"skipped"`
	// Limitations 是这一轮整体没做的检查（例如跨端尚未接通深度检查）。
	Limitations []string `json:"limitations"`
}

// listRunnerDiagnostics 是 GET /api/runners/{runnerID}/diagnostics。
//
// 批量版本，供管理页一次拿到"哪些工具有问题"。三条约束：
//   - **只对已经有迹象的工具详查**（判据见 needsDeepDiagnosis）：诊断要跑多次子进程，
//     对已经就绪的工具跑一遍纯属浪费；
//   - **通道坏了就整体不查**：与 listRunnerAgents 同源（resolveProbeTarget），
//     否则列表页说"无法检测"、诊断页却列出一排症状，用户不知道该信哪个；
//   - **不进 /api/runners 热路径**（那一条只做轻量探测）。
func (s *Server) listRunnerDiagnostics(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	meta, probeOK, probeError, err := s.resolveProbeTarget(runnerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	view := runnerDiagnosticsView{
		RunnerID: runnerID, ProbeOK: probeOK, ProbeError: probeError,
		Items: []agentDiagnosis{}, Skipped: []string{}, Limitations: []string{},
	}
	if !probeOK {
		// 通道坏了：**一个工具都不详查、也不给任何结论**（probeAgents 的结果一律不可信）。
		// 每个工具如实进 Skipped —— 界面上它是"未检测"，不是"没有问题"。
		for _, entry := range agentCatalog() {
			view.Skipped = append(view.Skipped, entry.ID)
		}
		writeJSON(w, http.StatusOK, view)
		return
	}

	force := forceRefresh(r)
	// 缓存命中就直接回放：这份报告里的 `diagnosedAt` 保持**原来**那个时刻，界面已有的
	// `diagnosisMetaLine` 会自动说明"诊断于 …"—— 不必再为它加字段。
	//
	// ⚠️ 缓存查询放在 `!probeOK` 之后：通道坏掉那一档是"一个工具都没查"，
	// 它本来就不该被缓存住（否则"没查"会被钉住）。
	if !force {
		if cached, ok := s.cachedAgentDiagnostics(meta.ID); ok {
			writeJSON(w, http.StatusOK, cached)
			return
		}
	}

	statuses, _ := s.probeAgentsFor(r.Context(), meta, force)
	if !isLocalRunnerID(meta.ID) {
		// 跨端的详查要在目标环境里跑只读脚本，尚未接通（docs/43 §10 第 6 步）。
		// 仍然逐个给出受限报告 —— 那里至少会如实说"没查成"。
		view.Limitations = append(view.Limitations, "跨端深度检查尚未接通：只读了登记表与安装审计。")
	}

	// 工具之间互相独立，各自都会拉起子进程 —— 并发跑，逐个超时不再相加
	// （与 probeAgents 同一条理由）。
	targets := make([]AgentCatalogEntry, 0, len(statuses))
	for _, status := range statuses {
		entry, known := agentByID(status.ID)
		if !known || !needsDeepDiagnosis(status) {
			view.Skipped = append(view.Skipped, status.ID)
			continue
		}
		targets = append(targets, entry)
	}

	// 运行时状态与具体工具无关，而每个工具的详查都要读它 —— 算**一次**给所有工具共用。
	//
	// ⚠️ 用 sync.Once 惰性求值，而不是在这里直接探：一个工具都不需要详查时（全部就绪），
	// 就不该为它付一次探测（那是 `node --version` + `npm --version` 两个子进程，外加一次
	// Node 版本索引下载）。冷缓存时那几个 goroutine 会各下一份索引 —— 这也是同一个
	// "同一件事不要做 N 遍"。
	shared := diagnoseShared{}
	if isLocalRunnerID(meta.ID) {
		var once sync.Once
		var probe runtimeStatus
		shared.runtime = func() runtimeStatus {
			once.Do(func() { probe = s.probeRuntime(r.Context()) })
			return probe
		}
	}

	reports := make([]agentDiagnosis, len(targets))
	var wait sync.WaitGroup
	for index, entry := range targets {
		wait.Add(1)
		go func(index int, entry AgentCatalogEntry) {
			defer wait.Done()
			// 每个 goroutine 写自己的槽位，互不重叠 —— 这是这里唯一需要的同步。
			reports[index] = s.buildAgentDiagnosisWith(r.Context(), meta, entry, shared)
		}(index, entry)
	}
	wait.Wait()
	view.Items = append(view.Items, reports...)
	writeJSON(w, http.StatusOK, view)
	// 挂到这一轮读数上。读数在深查期间变过的话 storeAgentDiagnostics 会拒收
	// —— 报告是照着那份读数算出来的，读数一变它就不作数了。
	s.storeAgentDiagnostics(meta.ID, view, statuses)
}

// needsDeepDiagnosis 回答"这个工具值不值得详查"。
//
// 判据必须**便宜** —— 它决定要不要跑一次完整诊断（多次子进程 + 扫目录 + 读审计）。
// 因此这里只覆盖"已经有迹象"的那一档，不猜"看起来正常但也许有问题"：
// 一个就绪的工具要详查，用户可以在卡片上显式点一次（那是显式动作，不是轮询）。
//
// ⚠️ 调用它的前提是**通道可用**（probeOk=true）—— 通道坏了时调用方根本不详查，
// 所以这里不再接一个恒真的 probeOK 参数（那种参数是死分支）。
func needsDeepDiagnosis(status AgentStatus) bool {
	if status.Status == agentStatusUnsupported {
		// 这一档的结论已经确定（换环境，不是修工具），详查没有意义。
		return false
	}
	// updating（正在安装/升级）也要详查：详查会给出"此刻结果不可信，等一下再测"
	// 这一档，比什么都不说有用。
	return status.Status != agentStatusReady
}
