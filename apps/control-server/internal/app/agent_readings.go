package app

import (
	"context"
	"net/http"
	"time"
)

// CLI 工具读数的缓存（"一段时间内只检测一次"）。
//
// 背景：管理页一次加载会打三个端点，而它们**各自都要探一遍**这台机器 ——
//   - GET /api/runners              → listRunners 里对每个 runner 跑 probeAgents
//   - GET /api/runners/{id}/agents  → listRunnerAgents 再跑一遍 probeAgents + probeRuntime
//   - GET /api/runners/{id}/diagnostics → listRunnerDiagnostics 第三遍 probeAgents + 深度诊断
//
// 外加逐工具的 `check-update` 各问一次官方 registry（纯网络，实测 1.9~3.1s/次）。
// 反复进出页面就是反复全量重探。这里按机器存一份读数，让同一次加载只探一遍、
// 一段时间内再进来直接复用。
//
// 三条纪律（缺一条就会把"没查"说成"没问题"，或把旧读数当成此刻的事实）：
//
//  1. **维护态不入缓存** —— 安装/升级进行中是动态状态，缓存住会让"正在更新"在操作
//     结束之后还挂着（与 probeAgent 里那条早退判断同一条理由，agent_probe.go:66-72）；
//  2. **写操作完成后立刻失效** —— 否则刚装完的工具界面上还写着"未安装"
//     （见 invalidateAgentReadings 的调用点）；
//  3. **读数带时刻** —— 缓存命中时界面必须能说出"这份读数是多久以前拿的"。
const (
	// agentReadingsTTL 是读数里**没有**"不可用"时的保鲜期。写操作会主动失效、
	// 手动刷新可绕过，所以这个值可以取长一点。
	agentReadingsTTL = 5 * time.Minute
	// agentReadingsStaleTTL 是含"不可用"读数时的保鲜期，取得比成功短：用户可能正在
	// 另一个终端里装它，不该让一次"没读到"钉住太久（与 wsl_agent_runner.go:79-94
	// 同一个取舍）。
	agentReadingsStaleTTL = 30 * time.Second
	// agentDiagnosticsTTL 是批量诊断报告的保鲜期。它与读数同寿、一起失效，
	// 避免出现"卡片说不可用、抽屉里却没有对应症状"。
	agentDiagnosticsTTL = 5 * time.Minute
	// agentUpdateCheckTTL 是"最新版本是多少"的保鲜期。它是一次真实网络查询，
	// 而 registry 上的最新版几分钟内不会变。
	agentUpdateCheckTTL = 10 * time.Minute
	// agentUpdateCheckFailureTTL 是查不到时的保鲜期，取得短：网络抖一下不该让
	// "读不到"钉十分钟，也不该每次进页面都去撞一次 registry。
	agentUpdateCheckFailureTTL = 60 * time.Second
	// agentProbeTimeout 是一次"这台机器上全部工具"的探测上限。probeAgents 内部并发跑，
	// 所以它是"最慢那个工具"的时间，不是各工具之和。
	//
	// 取 45s 而不是贴着单个探测自己的上限：WSL 那条路的刷新探测本身就允许 30s
	// （wslProbeRefreshTimeout），整体上限若与它相等，一次正常的慢探测会被整体截断成
	// "全都不可用"。它只是个兜底，真正管住单个工具的是它自己的超时。
	agentProbeTimeout = 45 * time.Second
)

// agentProbeRound 是一轮正在飞的探测。等待者持有指针，等 done 关闭后直接读 ——
// channel 的 close 建立了 happens-before，不必再抢锁，也不会读到下一轮的结果。
type agentProbeRound struct {
	done     chan struct{}
	statuses []AgentStatus
	at       time.Time
	// accepted 报告这一轮的结果**有没有被写回缓存**。等待者靠它决定能不能回放：
	// 探测期间发生过写操作（epoch 变了）或整体超时时，结果不入缓存，此时回放等于把
	// **写操作之前**的读数标上"探测结束时刻"交给界面 —— 正是 epoch 机制要挡的那件事。
	accepted bool
}

// agentReading 是某台机器上"工具状态"的一份读数快照（含由它派生的批量诊断报告）。
type agentReading struct {
	at       time.Time
	statuses []AgentStatus
	// diagnostics 是这一轮批量诊断的结果；nil = 还没有，或已随读数变化作废。
	diagnostics   *runnerDiagnosticsView
	diagnosticsAt time.Time
	// epoch 是发起这次探测时的失效世代。写操作会把世代 +1，于是"跨越了一次写操作"的
	// 探测结果不会被写回缓存 —— 否则刚装完的工具会被一份装之前的读数盖回去，
	// 而且要盖住整整一个 TTL。
	epoch uint64
	// round 非 nil 表示这台机器上有一次探测在飞（singleflight）。
	round *agentProbeRound
	// roundSeq 是"发起过多少轮探测"的序号，每次新建 round 自增。
	//
	// 它挡的是：手动刷新（force=true 直接新建一轮）与**已经在飞的那一轮**并发时，
	// 旧那一轮结束时只要 epoch 没变就会照样写回缓存，把新（force）那份顶掉 ——
	// 与 probeAgentsFor 注释里"手动刷新之后，后续的自动读取应当命中这份新读数"
	// 这条不变量相反。只有最新一轮有权写回。
	roundSeq uint64
}

// fresh 报告这份读数是否还在保鲜期内。失败档用更短的 TTL（见常量注释）。
func (r agentReading) fresh(now time.Time) bool {
	if r.at.IsZero() || len(r.statuses) == 0 {
		return false
	}
	ttl := agentReadingsTTL
	if anyAgentStatus(r.statuses, agentStatusUnavailable) {
		ttl = agentReadingsStaleTTL
	}
	return now.Sub(r.at) < ttl
}

// agentUpdateCheckKey 是"最新版本"读数的身份：**哪台机器上的哪个工具**。
// 只按 agentID 索引会把 A 机器上"发现新版本"带到 B 机器的同名卡片上。
type agentUpdateCheckKey struct{ runnerID, agentID string }

// agentUpdateCheck 是一次"最新版本是多少"的读数。
type agentUpdateCheck struct {
	at        time.Time
	available bool
	latest    string
	// autoUpdatable / current 与 `latest` 同源：它们是同一次响应里的三个字段，
	// 分开取会让"回放"与"首查"给出不同形状。
	autoUpdatable bool
	current       string
	// errText 非空表示**这次没查到**（网络失败等），不是"没有新版本"。
	// 缓存它必须原样回放：回落成"已是最新"是本项目的红线（见 CliToolsPage.tsx:219-222）。
	errText string
}

// forceRefresh 读出"这一次要不要绕过读数缓存"。
//
// 只有**手动动作**才带它（顶栏「重新检查」、写操作之后的 refresh），进入页面与切换
// 执行环境一律不带 —— 这就是"只有手动点重新检查才重新触发检测"的落点。
// 参数名沿用本项目既有约定（app.go:5948 的 stopRun 用的是同一个）。
func forceRefresh(r *http.Request) bool {
	return r.URL.Query().Get("force") == "true"
}

// probeAgentsFor 是"某台机器上全部工具状态"的完整形态：force=true 绕过缓存重探，
// 并把新读数写回缓存（手动刷新之后，后续的自动读取应当命中这份新读数）。
// 第二个返回值是这份读数的取得时刻（零值 = 没读到）。
//
// ⚠️ 调用方的 ctx 取消时返回 `(nil, 零值)`：那种情况下调用方已经不在了（响应写不出去），
// 而探测本身仍在服务端生命周期上跑完、结果照样进缓存给后来的人用。调用方对返回值要按
// "可能为空"处理（`len(nil)==0` 天然安全）。
func (s *Server) probeAgentsFor(ctx context.Context, meta RunnerMeta, force bool) ([]AgentStatus, time.Time) {
	// 循环：等待在飞的那一轮时，醒来后必须**重新判断**能不能回放（见下面 accepted 那段）。
	// 每一轮 continue 都对应"某一轮已经结束"，所以不会在同一轮上空转。
	for {
		s.agentReadingsMu.Lock()
		if s.agentReadings == nil {
			s.agentReadings = map[string]agentReading{}
		}
		entry := s.agentReadings[meta.ID]
		// ⚠️ 维护期间**读**也不走缓存：维护位是动态状态，回放一份"就绪"会让别的客户端
		// （手机端、另一个窗口）在工具正被替换时看到"可用"——而那一刻执行它可能拿到半成品。
		//
		// 代价是这段时间里整台机器都按实时探测走：维护中的那个工具很廉价（probeAgent 见到
		// 维护位就返回 updating，一个子进程都不跑），其余工具会真的重探。维护是有界的
		// （安装/升级最长几分钟）且这段时间的请求本来就少 —— 用这一点代价换"维护态在所有
		// 客户端都立刻可见"，值得。
		if !force && !s.anyMaintenanceActive(meta.ID) {
			if entry.fresh(time.Now()) {
				statuses, at := entry.statuses, entry.at
				s.agentReadingsMu.Unlock()
				return statuses, at
			}
			if entry.round != nil {
				// 已经有一次探测在飞（典型：进页面时 /api/runners 与 /agents 同时打过来）：
				// 等它，别叠第二份 —— 与 WSL 探测缓存同一条 singleflight 理由
				// （wsl_agent_runner.go:198-199）。
				round := entry.round
				s.agentReadingsMu.Unlock()
				select {
				case <-round.done:
					if round.accepted {
						return round.statuses, round.at
					}
					// 那一轮结束了但**没被采纳**（探测期间发生过写操作，或整体超时）：
					// 它的 statuses 是写操作**之前**的读数、at 却是探测结束时刻，直接回放
					// 就是把旧读数标成"刚刚读到的"——正是 epoch 机制要挡的那件事
					// （2026-09-29 定位）。重新走一遍决策：大概率自己再探一轮。
					continue
				case <-ctx.Done():
					// 调用方不等了（典型：用户切走了页面）。探测本身跑在服务端生命周期上、
					// 结果照样会写进缓存给后来的人用 —— 这里只是不替一个已经走掉的请求干等。
					return nil, time.Time{}
				}
			}
		}
		epoch := entry.epoch
		entry.roundSeq++
		seq := entry.roundSeq
		round := &agentProbeRound{done: make(chan struct{})}
		entry.round = round
		s.agentReadings[meta.ID] = entry
		s.agentReadingsMu.Unlock()

		// 收尾用 defer：无论探测是正常返回还是 panic，都要清掉在飞标记并放行等待者。
		// 否则这个 runner 会永久卡在"在飞"状态，之后每次自动读取都只能等到自己的 deadline
		// （前端 60 秒）才返回空读数 —— 界面把"没查成"渲染成"没有工具"。
		// 同一条纪律在 wsl_agent_runner.go 里已经写明并被使用。
		defer func() {
			s.agentReadingsMu.Lock()
			if current := s.agentReadings[meta.ID]; current.round == round {
				current.round = nil
				s.agentReadings[meta.ID] = current
			}
			s.agentReadingsMu.Unlock()
			close(round.done)
		}()

		// 探测跑在**服务端生命周期**上下文上，而不是某个请求的上下文：这份读数是共享状态，
		// 产出不该取决于"最先发起的那个请求有多有耐心"（与 wsl_agent_runner.go:137-143 同源）。
		probeCtx, cancel := s.agentProbeContext()
		defer cancel()
		statuses := s.probeAgentsUncached(probeCtx, meta)
		// 整体超时/被取消 ⇒ 这份读数不完整（每个工具都会落成"不可用"，而那不是事实）。
		// 不写进缓存：宁可下次重探，也不要把一次"没查成"钉住一个 TTL
		// —— 与 probeRuntime 那条"取消时不入缓存"同源。
		timedOut := probeCtx.Err() != nil
		round.statuses, round.at = statuses, time.Now()

		s.agentReadingsMu.Lock()
		current := s.agentReadings[meta.ID]
		next := current
		// 三个条件缺一不可：世代没变（没被写操作作废）、还是最新那一轮（force 与在飞轮并发时
		// 旧的不能顶掉新的）、以及没有整体超时。
		if current.epoch == epoch && current.roundSeq == seq && !timedOut {
			// accepted 要在持锁时置位：等待者对它的读取由 close(done) 建立 happens-before。
			round.accepted = true
			if anyAgentStatus(statuses, agentStatusUpdating) {
				// **维护态不入缓存**：那是一个动态状态，缓存住会让"正在更新"在结束后还挂着。
				// 但这一轮的结果仍然是"刚读到的"，只是不能当缓存用 —— accepted 照置，
				// 等待者拿它没有撒谎（它就是此刻的事实）。
				next.at, next.statuses = time.Time{}, nil
				next.diagnostics, next.diagnosticsAt = nil, time.Time{}
			} else {
				// 读数变了 ⇒ 由它派生的诊断报告不再成立，一起作废。
				if current.diagnostics != nil && !agentStatusesEqual(current.statuses, statuses) {
					next.diagnostics, next.diagnosticsAt = nil, time.Time{}
				}
				next.at, next.statuses = round.at, statuses
			}
		}
		s.agentReadings[meta.ID] = next
		s.agentReadingsMu.Unlock()

		return statuses, round.at
	}
}

// agentProbeContext 给出探测应当使用的生命周期上下文。runtimeCtx 为零值时退回
// Background（测试夹具），与 wslAgentRunner.backgroundContext 同一个理由。
func (s *Server) agentProbeContext() (context.Context, context.CancelFunc) {
	base := s.runtimeCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, agentProbeTimeout)
}

// cachedAgentDiagnostics 取出仍可用的批量诊断报告。
func (s *Server) cachedAgentDiagnostics(runnerID string) (runnerDiagnosticsView, bool) {
	s.agentReadingsMu.Lock()
	defer s.agentReadingsMu.Unlock()
	entry := s.agentReadings[runnerID]
	if entry.diagnostics == nil || entry.diagnosticsAt.IsZero() {
		return runnerDiagnosticsView{}, false
	}
	if time.Since(entry.diagnosticsAt) >= agentDiagnosticsTTL {
		return runnerDiagnosticsView{}, false
	}
	return *entry.diagnostics, true
}

// storeAgentDiagnostics 把这一轮结果挂到读数上。
//
// **读数变了就不存** —— 报告是照着那份读数算出来的，读数一变它就不作数了。
// 这一条同时挡住了"写操作与深查并发"时的错位：写操作把读数清空了，这里必然不相等。
func (s *Server) storeAgentDiagnostics(runnerID string, view runnerDiagnosticsView, statuses []AgentStatus) {
	s.agentReadingsMu.Lock()
	defer s.agentReadingsMu.Unlock()
	current, ok := s.agentReadings[runnerID]
	if !ok || current.at.IsZero() {
		return
	}
	if !agentStatusesEqual(current.statuses, statuses) {
		return
	}
	if anyAgentStatus(statuses, agentStatusUpdating) {
		return
	}
	current.diagnostics, current.diagnosticsAt = &view, time.Now()
	s.agentReadings[runnerID] = current
}

// cachedAgentUpdateCheck 取出仍可用的"最新版本"读数。
func (s *Server) cachedAgentUpdateCheck(key agentUpdateCheckKey) (agentUpdateCheck, bool) {
	s.agentUpdateChecksMu.Lock()
	defer s.agentUpdateChecksMu.Unlock()
	value, ok := s.agentUpdateChecks[key]
	if !ok || value.at.IsZero() {
		return agentUpdateCheck{}, false
	}
	ttl := agentUpdateCheckTTL
	if value.errText != "" {
		ttl = agentUpdateCheckFailureTTL
	}
	if time.Since(value.at) >= ttl {
		return agentUpdateCheck{}, false
	}
	return value, true
}

// storeAgentUpdateCheck 记下一次"最新版本"读数（成功与失败都记，失败用更短 TTL）。
func (s *Server) storeAgentUpdateCheck(key agentUpdateCheckKey, value agentUpdateCheck) {
	s.agentUpdateChecksMu.Lock()
	defer s.agentUpdateChecksMu.Unlock()
	if s.agentUpdateChecks == nil {
		s.agentUpdateChecks = map[agentUpdateCheckKey]agentUpdateCheck{}
	}
	s.agentUpdateChecks[key] = value
}

// invalidateAgentReadings 丢弃某台机器上"工具读数"的全部缓存。
//
// **每一个改动目标环境的动作完成后都必须走到它**（安装 / 升级 / 修复 / 装运行时），
// 否则用户刚装完工具、界面还写着"未安装"。
func (s *Server) invalidateAgentReadings(runnerID string) {
	if runnerID == "" {
		return
	}
	s.agentReadingsMu.Lock()
	if s.agentReadings != nil {
		current := s.agentReadings[runnerID]
		// 只清读数，**保留 epoch、在飞的那一轮与轮次序号**：
		//   - epoch+1 ⇒ 跨越这次写操作的探测结果不会被写回缓存；
		//   - round 留着 ⇒ 等待者不会被永久挂住（探测结束时会被唤醒）；
		//   - roundSeq 留着 ⇒ 序号必须单调，清零会让"发起于失效之前的旧那一轮"误判成最新
		//     （两个字段合起来才够：见 roundSeq 的注释）。
		s.agentReadings[runnerID] = agentReading{epoch: current.epoch + 1, round: current.round, roundSeq: current.roundSeq}
	}
	s.agentReadingsMu.Unlock()

	s.agentUpdateChecksMu.Lock()
	for key := range s.agentUpdateChecks {
		if key.runnerID == runnerID {
			delete(s.agentUpdateChecks, key)
		}
	}
	s.agentUpdateChecksMu.Unlock()
}

// agentStatusesEqual 比较两份读数是否**逐项相同**。AgentStatus 全是 string 字段，
// 可以直接比 —— 这正是"读数变了就作废派生报告"那条判据要的语义。
func agentStatusesEqual(a, b []AgentStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// anyAgentStatus 报告读数里是否有某个状态（用于维护态与失败档的判断）。
func anyAgentStatus(statuses []AgentStatus, want string) bool {
	for _, status := range statuses {
		if status.Status == want {
			return true
		}
	}
	return false
}
