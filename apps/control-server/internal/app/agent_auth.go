package app

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 登录态读数（三态 + 缓存）。
//
// 收口前的缺陷（2026-10-23 实测）：卡片上那颗「登录」只看 supportsLogin 与装没装，
// **完全不看登录态** —— 于是在终端里已经登录的 qoder 卡片上照样亮着「登录」；
// 而唯一能读到登录态的 /login-status 返回的是 bool，codebuddy 判别不了就报 false，
// 界面于是把"读不到"念成"未登录"（用户点进去还得再"检查"一次，检查完仍是未登录）。
//
// 这里按项目一贯的三态重写：
//   - logged_in  ｜CLI 自己报出了身份（qoder 的 Username/Email、opencode 的凭据表 …）；
//   - logged_out ｜CLI 自己说没有凭据（"not logged in" / "0 credentials"）；
//   - unreadable ｜**判断不了**。两种成因分别落在 note 上：工具根本不提供非交互查询，
//     或这一次没读成（命令失败、超时、输出形状变了）。
//
// 「读不到」与「未登录」合并，会让一个已经登录的用户被指去再登录一次 —— 正是这一轮
// 要修的那件事，所以两者在类型上就是两个值。
const (
	agentAuthLoggedIn   = "logged_in"
	agentAuthLoggedOut  = "logged_out"
	agentAuthUnreadable = "unreadable"
)

// agentAuthState 是登录态的取值。空串 = 还没有任何结论（缓存里根本没有这一条）。
type agentAuthState string

// authStateRunner 是可实现"报告该工具是否已登录"的可选接口。
//
// 返回值是**三态**而不是 bool：判别不了必须和"未登录"分开（见上面的常量）。
// error 非空表示**这一次**没读成；state=unreadable 且 error 为空表示该工具根本没有
// 可用的非交互登录态查询（codebuddy 就是这一档）。
type authStateRunner interface {
	LoginStatus(context.Context) (agentAuthState, error)
}

// agentAuthReading 是一份登录态读数。at 是**读完**的时刻：它决定这份读数还能不能直接
// 回放（见 agentAuthTTLFor），也是响应里 probedAt 的来源。
type agentAuthReading struct {
	state agentAuthState
	// note 是判断不了时的原因（"这次为什么没读成"），有结论时为空。
	note string
	// transient 标记"这一次没读成"（而不是"这个工具本来就不能自报"）：前者要按短 TTL
	// 保鲜，否则一次网络抖动会把"读不到"钉住五分钟。
	transient bool
	at        time.Time
}

// readable 报告这份读数是不是一个**结论**。
func (r agentAuthReading) readable() bool {
	return r.state == agentAuthLoggedIn || r.state == agentAuthLoggedOut
}

// loggedIn 只服务 /login-status 的兼容字段 loggedIn（旧客户端读它）。
func (r agentAuthReading) loggedIn() bool { return r.state == agentAuthLoggedIn }

// agentAuthKey 是登录态读数的身份：**哪台机器上的哪个工具**。
// 只按 agentID 索引会把 A 机器的"已登录"带到 B 机器的同名卡片上。
type agentAuthKey struct{ runnerID, agentID string }

const (
	// agentAuthTTL 是登录态读数的保鲜期。它变化得很慢（用户手动登录/登出），
	// 而探一次真的要起一个 node 进程（qoder 的 status 冷启动实测 7.5s），
	// 所以缓存取长一点；手动「检查登录状态」带 force=true 随时绕过它。
	agentAuthTTL = 5 * time.Minute
	// agentAuthFailureTTL 是"这一次没读成"的保鲜期，取得比结论短得多：网络抖一下不该
	// 把"读不到"钉五分钟，也不该每次进页面都去撞一次同样的失败。
	agentAuthFailureTTL = 30 * time.Second
	// agentAuthProbeTimeout 是单次探测的兜底上限。各家 CLI 自己按 20s 收口（qoder 那
	// 7.5s 的冷启动就在预算内），这里只防"某条命令永远不返回"，不要抢在工具自己的
	// 预算之前把它截断 —— 截断的后果是把"已登录"读成"读不到"。
	agentAuthProbeTimeout = 25 * time.Second
)

// agentAuthFlight 是一次正在飞的探测。等待者持有指针，等 done 关闭后直接读 ——
// channel 的 close 建立了 happens-before，不必再抢锁（与 agentProbeRound 同一手法）。
//
// 为什么需要它：页面一进入就会对每个可登录的工具各问一次，而"另一个窗口/手机端也在
// 同一时刻打开"完全正常。没有它，qoder 那条 7.5s 的 status 会被同时拉起两次。
type agentAuthFlight struct {
	done    chan struct{}
	reading agentAuthReading
}

// agentAuthFor 取某个工具的登录态读数：命中缓存就直接回放，否则探测一次并写回。
//
// force=true 绕过缓存（手动「检查登录状态」走这条）。缓存与探测都以
// (runner, agent) 为粒度 —— 换执行环境后读到的是那台机器上的状态。
func (s *Server) agentAuthFor(ctx context.Context, runnerID string, entry AgentCatalogEntry, backend AgentRunner, force bool) agentAuthReading {
	key := agentAuthKey{runnerID: runnerID, agentID: entry.ID}

	s.agentAuthMu.Lock()
	if s.agentAuths == nil {
		s.agentAuths = map[agentAuthKey]agentAuthReading{}
	}
	if s.agentAuthFlights == nil {
		s.agentAuthFlights = map[agentAuthKey]*agentAuthFlight{}
	}
	cached, hasCache := s.agentAuths[key]
	if !force && hasCache && time.Since(cached.at) < agentAuthTTLFor(cached) {
		s.agentAuthMu.Unlock()
		return cached
	}
	if flight, flying := s.agentAuthFlights[key]; flying {
		s.agentAuthMu.Unlock()
		select {
		case <-flight.done:
			return flight.reading
		case <-ctx.Done():
			// 调用方不等了（典型：用户切走页面把请求取消掉）。它手上这份可能根本没有，
			// 如实交回去 —— 探测本身照样会跑完并写回缓存，给后来的人用。
			return cached
		}
	}
	flight := &agentAuthFlight{done: make(chan struct{})}
	s.agentAuthFlights[key] = flight
	s.agentAuthMu.Unlock()

	read := s.probeAgentAuth(ctx, entry, backend)

	s.agentAuthMu.Lock()
	flight.reading = read
	delete(s.agentAuthFlights, key)
	s.agentAuths[key] = read
	s.agentAuthMu.Unlock()
	// 先写 flight.reading 再 close：等待者读到的一定是写完的那份。
	close(flight.done)
	return read
}

// invalidateAgentAuth 丢掉某台机器上**全部**工具的登录态读数。
//
// 与读数缓存同一条纪律（见 agent_readings.go 的三条）：安装 / 升级 / 修复 / 装运行时
// 之后立刻作废 —— 一次修复可能重装或替换了 CLI 的配置，之后还回放「动之前读到的已登录」，
// 就是拿旧状态说新事实。
//
// 只清**已缓存**的值：正在飞的那一轮仍会写回（它最多比这次写操作早一轮，而登录态几乎
// 不受安装影响）；要连它一起挡，得再引入一套世代号，代价大于收益。手动「检查登录状态」
// 本来就带 force，用户随时可以拿回真读数。
func (s *Server) invalidateAgentAuth(runnerID string) {
	if runnerID == "" {
		return
	}
	s.agentAuthMu.Lock()
	for key := range s.agentAuths {
		if key.runnerID == runnerID {
			delete(s.agentAuths, key)
		}
	}
	s.agentAuthMu.Unlock()
}

// agentAuthTTLFor 给出这份读数的保鲜期。
func agentAuthTTLFor(read agentAuthReading) time.Duration {
	if read.transient {
		return agentAuthFailureTTL
	}
	return agentAuthTTL
}

// probeAgentAuth 真的问一次。**不落在请求上下文上**：这份读数是共享状态，产出不该
// 取决于"最先发起请求的那个客户端有多有耐心"（与 probeAgentsFor 同源）。
func (s *Server) probeAgentAuth(parent context.Context, entry AgentCatalogEntry, backend AgentRunner) (read agentAuthReading) {
	// at 是**读完**的时刻，不是开始读的时刻：这份读数描述的是 CLI 在那一刻的样子，
	// 而探测本身可能花掉 7.5s（qoder 冷启动）。
	defer func() { read.at = time.Now() }()
	if !entry.SupportsLogin {
		return agentAuthReading{state: agentAuthUnreadable, note: fmt.Sprintf("%s 不在平台内登录的支持范围内", entry.Name)}
	}
	if backend == nil {
		return agentAuthReading{state: agentAuthUnreadable, note: fmt.Sprintf("这个执行环境没有 %s 的执行面", entry.Name)}
	}
	probe, ok := backend.(authStateRunner)
	if !ok {
		return agentAuthReading{state: agentAuthUnreadable, note: fmt.Sprintf("%s 的平台内登录态查询尚未接通", entry.Name)}
	}
	// 底座取**服务端生命周期**（关闭时一并取消），而不是某一次请求：这份读数是共享状态，
	// 产出不该取决于「最先发起请求的那个客户端有多有耐心」。测试夹具没有 runtimeCtx 时
	// 退回请求上下文，但只去掉它的取消信号。
	base := s.runtimeCtx
	if base == nil {
		base = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithTimeout(base, agentAuthProbeTimeout)
	defer cancel()
	state, err := probe.LoginStatus(ctx)
	if err != nil {
		return agentAuthReading{
			state:     agentAuthUnreadable,
			note:      fmt.Sprintf("读取 %s 的登录状态失败：%v", entry.Name, err),
			transient: true,
		}
	}
	switch state {
	case agentAuthLoggedIn, agentAuthLoggedOut:
		return agentAuthReading{state: state}
	default:
		// 工具自己说不清（codebuddy）。这一档**不能**念成未登录：用户可能明明登录着。
		return agentAuthReading{state: agentAuthUnreadable, note: fmt.Sprintf("%s 无法自动判断登录态，请到目标环境的终端里确认", entry.Name)}
	}
}

// agentAuthStateFromStatusOutput 把"列凭据 / 列状态"类命令的输出读成三态。
//
//   - 命中空标记 ⇒ 未登录（CLI 自己说没有凭据）；
//   - 命令成功、输出非空、且没有空标记 ⇒ 已登录（只能证明"CLI 认为自己有凭据"，
//     不能证明凭据此刻还有效）；
//   - **输出为空 ⇒ 判断不了**。这一档不许冒充任何一种结论：空输出既可能是"没有凭据"，
//     也可能只是一个不认识的版本把输出写到了别的地方。
func agentAuthStateFromStatusOutput(output string, emptyMarkers []string) agentAuthState {
	if strings.TrimSpace(output) == "" {
		return agentAuthUnreadable
	}
	if agentAuthListHasCredentials(output, emptyMarkers) {
		return agentAuthLoggedIn
	}
	return agentAuthLoggedOut
}
