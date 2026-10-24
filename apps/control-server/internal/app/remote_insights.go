package app

import "net/http"

// 手机端能执行的全部 **优化建议** 操作。名单类型与合成逻辑在 remote_relay.go。
//
// 这 11 条是电脑端优化建议面板的全部 REST 端点（app.go 里 /api/projects/{projectID}/insights*
// 那一组），一条不多一条不少：手机端要整块复用 InsightsPanel，就得让它发出的每个请求
// 都有对应的 op —— 少一条的症状是面板静默空白。
//
// 三条纪律与 fs / git 域相同（见 remote_git.go 开头）：名单写死在测试里、路径参数必须
// 登记形状校验、MaxBytes 只给"返回本来就可能太大"的读操作。findingID 由服务端
// uuid.NewString() 生成，中继层用同一个 UUID 形状校验器挡住路径注入。
//
// 所有操作都复用既有 handler，扫描/复核的互斥登记、配额预留、工作区准入检查、
// 错误文案全部原样继承 —— 手机端不新增任何一条建议语义。
func (s *Server) remoteInsightOperations() map[string]remoteOperation {
	return map[string]remoteOperation{
		// ── 列表与扫描 ──────────────────────────────────────────────────────────
		// 列表把 findings / invalidated / dismissed / 事件一并带回，是这批 op 里
		// 唯一可能顶到 320 KiB 的：截断上限由服务端自己控制（insightFindingsListLimit），
		// 超限只可能是事件积压或字段膨胀，给一句具体的话比撞通道判据强。
		"insights.list": {
			Method: http.MethodGet, Path: "/insights", Query: true,
			MaxBytes: 320 << 10, Label: "建议列表", Handle: s.listInsights,
		},
		// 扫描是**异步**的（立刻返回 + 轮询 insights.list 取进度），不会撞通道超时。
		"insights.scan":   {Method: http.MethodPost, Path: "/insights/scan", Handle: s.triggerInsightScan},
		"insights.cancel": {Method: http.MethodPost, Path: "/insights/cancel", Handle: s.cancelInsightScan},

		// ── 批量动作 ────────────────────────────────────────────────────────────
		// 复核同为异步登记（202 + 轮询）；verify 的 FindingIDs 在请求体里。
		"insights.verify": {Method: http.MethodPost, Path: "/insights/verify", Handle: s.verifyInsightFindings},
		"insights.toTask": {Method: http.MethodPost, Path: "/insights/to-task", Handle: s.addInsightToTasks},
		"insights.delete": {Method: http.MethodPost, Path: "/insights/delete", Handle: s.deleteInsightFindings},

		// ── 单条建议 ────────────────────────────────────────────────────────────
		// findingID 是路径参数：只能从 params 声明位取（PathParams），经 UUID 校验器
		// 与 url.PathEscape 双保险后才进 URL。转任务复用单条端点（体为空对象）。
		"insights.findingUpdate": {
			Method: http.MethodPatch, Path: "/insights/{findingID}",
			PathParams: []string{"findingID"}, Handle: s.updateInsightFinding,
		},
		"insights.findingDelete": {
			Method: http.MethodDelete, Path: "/insights/{findingID}",
			PathParams: []string{"findingID"}, Handle: s.deleteInsightFinding,
		},
		"insights.findingToTask": {
			Method: http.MethodPost, Path: "/insights/{findingID}/to-task",
			PathParams: []string{"findingID"}, Handle: s.addInsightToTask,
		},
		"insights.findingDismiss": {
			Method: http.MethodPost, Path: "/insights/{findingID}/dismiss",
			PathParams: []string{"findingID"}, Handle: s.setInsightDismissed,
		},
		"insights.findingDismissClear": {
			Method: http.MethodDelete, Path: "/insights/{findingID}/dismiss",
			PathParams: []string{"findingID"}, Handle: s.clearInsightDismissed,
		},
	}
}
