package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 单个 CLI 工具的故障诊断。
//
// 存在意义：管理页原先只有**一个布尔**在描述可用性（`runnerAgentView.Installed`），
// 而那个布尔把三件不同的事压成了一档 —— `agentStatusUnavailable` 同时覆盖
// "resolver 四路全没命中"与"二进制找到了但 --version 返回空"。后果是界面说"未安装"、
// 给出一个点了没用的按钮，而用户真正需要知道的那句话（"一次中断的安装留下了半成品"）
// 从来没被说出来过。
//
// 诊断要做的事只有一件：**把那一档拆开**，并给每一档它自己正确的下一步。
//
// 三条纪律（都不是可选的）：
//
//  1. **只读**。不写文件、不改登记表、不在目标环境留任何痕迹。
//     `docs/42 §18.1` 曾为"探测不该留痕"去掉过一次 mkdir 探可写性 —— 不能加回来。
//     所以这里也**不探测 prefix 可不可写**（那要写文件）：可写性只在**修复失败**时
//     才作为一个事实浮出来，由 agent_repair.go 的错误分类回答。
//  2. **判据只有一份**。报告里的"能不能装 / 能不能升"直接调 `resolveAgentInstallPlan`，
//     不新造第三套 —— `docs/42 §19.2 E` 为判据分裂付过代价。
//  3. **每一项都要带证据**。只给一句"有问题"等于把用户留在原地；证据是路径、版本、
//     以及失败命令的输出尾部（都已清洗与截断）。
//
// 与探测的关系：`probeAgent`（agent_probe.go）回答"现在能不能用"，很轻，进列表热路径；
// 诊断回答"为什么不能、还有哪几份、怎么修"，很重（多次子进程 + 扫目录 + 读审计），
// **只走按需端点**。

// ── 症状码 ──────────────────────────────────────────────────────────────────
//
// 每一档都必须对应**下一步动作各不相同**。把两档并成一句灰字，就是又在重复
// "把两种真相压成一句"这个错误。
const (
	// 安装完整性。
	issueBinaryMissing       = "binary-missing"           // 登记了安装位置，但文件已经不在
	issueBinaryBroken        = "binary-broken"            // 文件在，执行失败（多半是中断安装留下的半成品）
	issueProbeTimeout        = "probe-timeout"            // 能执行但超时 —— 与上一条分开，下一步动作不同
	issueOverrideDead        = "override-path-dead"       // 环境变量覆盖指向了一个已不存在的文件
	issueShimMissing         = "command-shim-missing"     // 命令入口不存在
	issueShimDangling        = "command-shim-dangling"    // 入口在，但它指向的目标不存在
	issuePackageInterrupted  = "package-interrupted"      // 回滚留下的残骸目录
	issuePackageBackup       = "package-backup-available" // 有可回滚的完整旧包
	issueActivePackageBroken = "active-package-broken"    // 生效包目录存在但读不出包元数据
	issuePackageScanFailed   = "package-scan-failed"      // 包目录扫不动（读不到 ≠ 没有）

	// 前置运行时。
	issueRuntimeMissing = "runtime-missing"
	issueRuntimeBroken  = "runtime-broken" // 运行时文件在，但执行不起来
	issueRuntimeTooOld  = "runtime-too-old"
	issueNpmUnavailable = "npm-unavailable"

	// 路径与登记一致性。
	issueRecordStale       = "record-stale"        // 登记的位置已不存在（但工具还能用）
	issueRecordUnreadable  = "record-unreadable"   // 登记表读不动
	issueRecordKindUnknown = "record-kind-unknown" // 认不出的安装方式
	issueNativeUnmanaged   = "native-unmanaged"    // 官方安装器装的，平台不接管

	// 环境档（结论已经确定，不需要"修"）。
	issueEnvUnsupported = "env-unsupported"
	// 正在安装/升级 —— 此刻探测不到结论，**不许**把中间态说成"坏了"。
	issueMaintenanceActive = "maintenance-active"
)

// 严重程度。blocker 是"现在用不了"，warning 是"还能用但会绊倒你"，info 是事实说明。
const (
	severityBlocker = "blocker"
	severityWarning = "warning"
	severityInfo    = "info"
)

// 报告的收敛状态。unsupported 与探测取同一个值，避免两处漂移。
const (
	diagnosisOK            = "ok"
	diagnosisBroken        = "broken"
	diagnosisNotInstalled  = "not-installed"
	diagnosisUnknown       = "unknown" // 该查的没查成 —— 不能渲染成"没问题"
	diagnosisUnsupported   = agentStatusUnsupported
	diagnosisChannelFailed = "channel-failed"
)

// 修复动作 id。**权威定义在 agent_repair.go 的 agentRemedies 表**（那里有 label、
// 说明与实现）；诊断只引用其中的 id，并且只把表里存在的动作放进报告。
const (
	remedyReinstall          = "reinstall"
	remedyRebuildShim        = "rebuild-shim"
	remedyCleanupInterrupted = "cleanup-interrupted"
	remedyRestoreBackup      = "restore-backup"
	remedyInstallRuntime     = "install-runtime"
)

// diagnoseVersionTimeout 是"执行一次 --version"的预算，与 runVersionCommand 同量级。
const diagnoseVersionTimeout = 8 * time.Second

// diagnoseProbeBudget 是**每条诊断最多实测几条候选路径**。
//
// 每一次实测都是一次子进程（单条预算 8 秒），而候选最多 5 条 —— 全测一遍最坏 40 秒，
// 用户会以为页面卡死。两条已经能回答那两个真问题：
// ① 现在生效这份为什么用不了；② 是不是还有另一份能用的。
const diagnoseProbeBudget = 2

// ── 报告模型 ────────────────────────────────────────────────────────────────

// diagnoseRemedy 是报告里给出的一个修复动作（**服务端下发的完整对象**）。
//
// 带 label/detail 而不是只给 id：界面因此不需要维护一份 id→文案的映射 ——
// 那种映射必然与 agentRemedies 漂移，而漂移的表现是"按钮上写着一件事、点下去
// 做的是另一件"，这是最难被发现的一类错。
type diagnoseRemedy struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// diagnoseIssue 是一条诊断发现。
type diagnoseIssue struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
	// Remedies 是该症状可用的修复动作。空表示"平台修不了"，界面据此不给按钮
	// —— 而不是给一个点了必失败的按钮。
	Remedies []diagnoseRemedy `json:"remedies"`
}

// diagnosePathFact 是"这个工具在这台机器上的一份安装"。
//
// 这一块存在的唯一理由：resolver 的顺序是
// `override > 登记表（实测存在）> PATH > 兜底`（agent_paths.go:58），而登记表的路径
// **只要文件还在就会被优先采用** → 一次失败的安装把那个位置毁掉之后，它仍然会压住
// 用户后来新装的那一份。"我明明装了新版，界面还是说不能用"当前没有任何地方能解释，
// 这张表就是那个解释。
// **三个布尔各说一件事，缺一个就会说出自己没查过的话**：
//
//	Checked=false → 这台机器上**没核对过**（跨端：路径在目标环境的文件系统上，
//	                本机 Stat 得到的结论与它无关）→ 界面说"未核对"；
//	Checked=true、Exists=false → 核对过，确实没有；
//	Checked=true、Exists=true、Probed=false → 文件在，但本轮**没执行过**它
//	                → 既不能说"可执行"也不能说"跑不起来"（实测预算是有限的，
//	                见 diagnoseProbeBudget）。
type diagnosePathFact struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Path   string `json:"path"`
	// Checked 表示"这条路径在**本机**核对过"。跨端恒为 false。
	Checked bool `json:"checked"`
	// Exists 与 Works 只在 Checked 为真时有意义。
	Exists bool `json:"exists"`
	// Probed 表示真的执行过它一次。
	Probed  bool   `json:"probed"`
	Works   bool   `json:"works"`
	Version string `json:"version"`
}

// diagnosePreflight 把"点了会失败"提前说出来。
//
// 成本**不是零**（早期注释写错了）：`resolveAgentInstallPlan` 只读文件系统与 PATH，
// 但它为了拿运行时版本会跑一次 `node --version`（以及同处的 `npm --version` 之类）。
// 好处是它**一个字节都不写**，而且与真正点「安装」用的是同一条路径。
type diagnosePreflight struct {
	InstallOK     bool   `json:"installOk"`
	InstallReason string `json:"installReason,omitempty"`
	UpgradeOK     bool   `json:"upgradeOk"`
	UpgradeReason string `json:"upgradeReason,omitempty"`
}

// agentDiagnosis 是单个工具的完整诊断报告。
type agentDiagnosis struct {
	AgentID string `json:"agentId"`
	Status  string `json:"status"`
	// Version 是**实测**版本（真的执行出来过），一份都用不了时为空。
	// 登记版本不往这里塞：那是"我们上次装了什么"，不是"现在能用什么"。
	Version string             `json:"version"`
	Issues  []diagnoseIssue    `json:"issues"`
	Paths   []diagnosePathFact `json:"paths"`
	// LastFailure 取自 agent_install_audit —— 零成本、最有用的现场证据。
	// 用户问"上次到底怎么坏的"，答案一直躺在库里，只是没人读。
	LastFailure *installAuditEntry `json:"lastFailure,omitempty"`
	Preflight   *diagnosePreflight `json:"preflight,omitempty"`
	// Limitations 列出**这一轮没有跑的检查**。非空时界面必须如实说"检测未完成"，
	// 不能把"没查"渲染成"没问题"。
	Limitations []string  `json:"limitations"`
	DiagnosedAt time.Time `json:"diagnosedAt"`
}

// add 记一条症状。remedyIDs 会被换算成**白名单里真实存在的**动作对象 ——
// 白名单里没有的 id 直接不进报告，于是"界面上亮着一个点了没反应的动作"不可能出现。
func (report *agentDiagnosis) add(code, severity, summary string, evidence []string, remedyIDs ...string) {
	if evidence == nil {
		evidence = []string{}
	}
	offered := []diagnoseRemedy{}
	for _, id := range remedyIDs {
		remedy, known := agentRemedies[id]
		if !known {
			continue
		}
		offered = append(offered, diagnoseRemedy{ID: remedy.ID, Label: remedy.Label, Detail: remedy.Detail})
	}
	report.Issues = append(report.Issues, diagnoseIssue{
		Code: code, Severity: severity, Summary: summary,
		Evidence: evidence, Remedies: offered,
	})
}

func (report *agentDiagnosis) limit(reason string) {
	report.Limitations = append(report.Limitations, reason)
}

// finalizeDiagnosis 把一串症状收敛成一个状态。**顺序不许换**：
//
//   - blocker 优先于一切。一个工具"装着、登记着、但跑不起来"时，说"已安装"会让
//     用户以为没事，而那是这一整轮要消灭的错；
//   - "真的没装"只有在**既没有 blocker、也没有任何安装痕迹**时才成立。
func finalizeDiagnosis(report *agentDiagnosis, hasAnyInstallation bool) {
	if report.Status != "" {
		return
	}
	for _, issue := range report.Issues {
		if issue.Severity == severityBlocker {
			report.Status = diagnosisBroken
			return
		}
	}
	if hasAnyInstallation {
		report.Status = diagnosisOK
		return
	}
	report.Status = diagnosisNotInstalled
}

// ── 入口 ────────────────────────────────────────────────────────────────────

// buildAgentDiagnosis 按 Runner 分派：本机走完整诊断，跨端走受限诊断。
func (s *Server) buildAgentDiagnosis(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) agentDiagnosis {
	return s.buildAgentDiagnosisWith(ctx, meta, entry, diagnoseShared{})
}

// buildAgentDiagnosisWith 是同一个入口，外加"这次批量诊断共用的读数"（见 diagnoseShared）。
func (s *Server) buildAgentDiagnosisWith(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry, shared diagnoseShared) agentDiagnosis {
	if isLocalRunnerID(meta.ID) {
		return s.buildLocalAgentDiagnosisWith(ctx, meta, entry, shared)
	}
	return s.buildCrossAgentDiagnosis(ctx, meta, entry)
}

// buildLocalAgentDiagnosis 是 buildLocalAgentDiagnosisWith 的单工具形态。
func (s *Server) buildLocalAgentDiagnosis(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) agentDiagnosis {
	return s.buildLocalAgentDiagnosisWith(ctx, meta, entry, diagnoseShared{})
}

// buildLocalAgentDiagnosisWith 是本机那条完整路径。
func (s *Server) buildLocalAgentDiagnosisWith(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry, shared diagnoseShared) agentDiagnosis {
	report := agentDiagnosis{
		AgentID:     entry.ID,
		Issues:      []diagnoseIssue{},
		Paths:       []diagnosePathFact{},
		Limitations: []string{},
		DiagnosedAt: time.Now().UTC(),
	}

	// 环境档先判：这一档的结论是"换环境"，不是"修工具"，后面所有检查都不必跑。
	if _, unsupportedReason := s.agentBackend(meta, entry); unsupportedReason != "" {
		report.Status = diagnosisUnsupported
		report.add(issueEnvUnsupported, severityInfo, unsupportedReason,
			[]string{"执行环境：" + meta.ID + "（" + diagnoseOrDash(meta.Environment) + "）"})
		return report
	}

	// **安装/升级进行中时不探测。**
	//
	// 这一刻产物可能正被替换（npm 会先改名再解压），探测得到的是"版本为空"，
	// 于是诊断会给出一条**假的"半装"**，而用户可能照着它去点修复 —— 而修复会被
	// beginAgentMaintenance 挡成 409。`probeAgent` 早就有这条判断（agent_probe.go:70
	// 的注释写了完整理由），诊断必须与它**同源**：否则同一时刻两处对同一件事
	// 给出相反结论，而"诊断结论与徽标打架"正是本项目反复禁止的那类错。
	if s.agentMaintenanceActive(meta.ID, entry.ID) {
		report.Status = diagnosisUnknown
		report.add(issueMaintenanceActive, severityInfo,
			fmt.Sprintf("%s 上正在安装或升级，此刻的探测结果不可信", entry.Name),
			[]string{"等这次操作结束后再检测，就会看到真实状态"})
		return report
	}

	// 登记表。**读失败与"没登记"必须分开**：把读失败当成"没登记"，会让一份装着的
	// 工具被当成没装过 —— 那正是"把读不到写成没有"（见 recordedInstallation 注释）。
	recorded, hasRecord, err := s.recordedInstallation(ctx, meta.ID, entry.ID)
	if err != nil {
		report.add(issueRecordUnreadable, severityBlocker, errorText(err),
			[]string{"无法判断这台机器上的安装方式，因此也给不出任何修复动作"})
		report.Status = diagnosisUnknown
		return report
	}

	// ── 1. 路径事实 ─────────────────────────────────────────────────────────
	effective := ""
	if s.paths != nil {
		effective = s.paths.Path(entry.ID)
	}
	prefix, prefixNote := s.diagnoseInstallPrefix(ctx, recorded, hasRecord)
	if prefixNote != "" {
		report.limit(prefixNote)
	}
	candidates := diagnosePathCandidates(entry, effective, recorded, prefix)
	paths, probes := s.probeDiagnosePaths(ctx, entry, candidates)
	report.Paths = paths
	report.Version = diagnoseFirstVersion(paths)

	// "当前生效那份怎么了"要建立在**路径事实**上，而不是"我们恰好实测过它"上。
	//
	// ⚠️ 这里原先写成 `hasEffective := probes[effective]`，而 probes 只在 fact.Exists
	// 为真时才写入 —— 于是"存在吗"与"实测过吗"被压成一个布尔，`!probe.Exists` 那一支
	// （环境变量覆盖指向一个死文件）**永远进不去**。而 resolver 对 override 是
	// **无条件返回**的（agent_paths.go:66）：override 指向死文件时不往下找，命令行
	// 因此用不了；诊断却一条症状都不报，最后判成"没有问题"（登记里的 binary_path
	// 恰好还在时）。这一整轮要消灭的正是这种结论。
	effectiveFact, hasEffective := diagnosePathFactByPath(paths, effective)
	effectiveProbe, probed := probes[effective]
	s.assessResolvedPath(&report, entry, effective, effectiveFact, hasEffective, effectiveProbe, probed, hasRecord, recorded)

	// ── 2. 安装目录内部（前缀里的命令入口与包目录） ──────────────────────────
	if prefix != "" {
		s.assessInstallPrefix(&report, entry, prefix, recorded, hasRecord)
	}

	// ── 3. 前置运行时 ───────────────────────────────────────────────────────
	if hasRecord || hasEffective {
		s.assessRuntime(ctx, shared, &report, entry, recorded, hasRecord)
	}

	// ── 4. 登记一致性 ───────────────────────────────────────────────────────
	s.assessRecord(&report, entry, recorded, hasRecord, paths)

	// ── 5. 预检：把"点了会失败"提前说出来 ───────────────────────────────────
	report.Preflight = s.diagnosePreflightLocal(ctx, meta, entry)

	// ── 6. 上次失败的现场 ───────────────────────────────────────────────────
	report.LastFailure = s.lastAgentInstallFailure(ctx, meta.ID, entry.ID)

	finalizeDiagnosis(&report, hasRecord || diagnoseAnyWorks(paths) || diagnoseAnyExists(paths))
	return report
}

// buildCrossAgentDiagnosis 是跨端（WSL / SSH）那条受限路径。
//
// 本轮只做**能确定的事**，并把没做的事写进 Limitations：
//   - 探测说就绪 ⇒ 它确实能用（那是目标环境给出的事实）；
//   - 探测说不可用 ⇒ **分不出**"没装"与"装坏了"（那需要去目标环境跑一串只读脚本，
//     见 docs/43 §10 第 6 步）⇒ 状态是 unknown，**不能**猜成 broken 或 not-installed；
//   - 登记表与审计里的事实照常给出（它们本来就在本机库里）。
func (s *Server) buildCrossAgentDiagnosis(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) agentDiagnosis {
	report := agentDiagnosis{
		AgentID:     entry.ID,
		Issues:      []diagnoseIssue{},
		Paths:       []diagnosePathFact{},
		Limitations: []string{},
		DiagnosedAt: time.Now().UTC(),
	}
	report.limit("跨端深度检查尚未接通：本轮只读了登记表与安装审计，没有在目标环境里核对文件与包目录。")
	// 预检也要在目标环境里读一次运行时状态（`resolveCrossInstallTarget` 需要它），
	// 尚未接通 —— 如实说出来，否则界面会给人"这台机器上什么都查过了"的印象。
	report.limit("跨端没有预检：要判「点了会不会失败」得先在目标环境读一次运行时状态，尚未接通。")

	recorded, hasRecord, err := s.recordedInstallation(ctx, meta.ID, entry.ID)
	if err != nil {
		report.add(issueRecordUnreadable, severityBlocker, errorText(err), nil)
		report.Status = diagnosisUnknown
		return report
	}
	if hasRecord {
		// ⚠️ **不核对存在性**。这条路径在目标环境的文件系统上，而 fileExists 是本机的
		// Stat —— 对一条 `/usr/local/bin/claude` 报"不存在"，是把**本机的读数写成目标机
		// 的事实**（而且与本报告自己的 Limitations 直接打架：那里刚说过"没有在目标环境
		// 里核对文件"）。所以这里只给出登记内容，Checked 保持 false，界面说"未核对"。
		report.Paths = append(report.Paths, diagnosePathFact{
			Source: "recorded", Label: "登记表记录的安装位置（未在目标环境核对）",
			Path: recorded.BinaryPath,
		})
	}
	report.LastFailure = s.lastAgentInstallFailure(ctx, meta.ID, entry.ID)

	status := s.probeAgent(ctx, meta, entry)
	switch status.Status {
	case agentStatusReady:
		report.Status = diagnosisOK
		report.Version = status.Version
	case agentStatusUnsupported:
		report.Status = diagnosisUnsupported
		report.add(issueEnvUnsupported, severityInfo, status.Reason,
			[]string{"执行环境：" + meta.ID + "（" + diagnoseOrDash(meta.Environment) + "）"})
	case agentStatusUpdating:
		// 正在安装/升级：**不能**把中间态说成"坏了"，也不能说成"没装"。
		// 这一档先前是靠 switch 的 default 顺带落到 unknown 的 —— 顺手把理由说出来，
		// 否则界面上只有一句"检测未完成"，用户不知道在等什么。
		report.Status = diagnosisUnknown
		report.add(issueMaintenanceActive, severityInfo,
			fmt.Sprintf("%s 上正在安装或升级，此刻的探测结果不可信", entry.Name),
			[]string{"等这次操作结束后再检测，就会看到真实状态"})
	default:
		// 分不出"没装"与"坏了"。如实说"没查成"，并说明为什么。
		report.Status = diagnosisUnknown
	}
	return report
}

// ── 路径事实 ────────────────────────────────────────────────────────────────

type pathCandidate struct {
	Source string
	Label  string
	Path   string
}

// diagnosePathCandidates 枚举"这个工具在这台机器上真的可能存在的落点"。
// 顺序即优先级，与 resolver 的解析顺序一致。同一条路径只出现一次（先到者赢标签）。
func diagnosePathCandidates(entry AgentCatalogEntry, effective string, recorded agentInstallation, prefix string) []pathCandidate {
	candidates := []pathCandidate{}
	add := func(source, label, path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		for _, existing := range candidates {
			if sameCleanPath(existing.Path, path) {
				return
			}
		}
		candidates = append(candidates, pathCandidate{Source: source, Label: label, Path: path})
	}
	// 生效路径只要**不是"什么都没命中"**（resolver 那档返回的是裸命令名）就是一条落点。
	//
	// ⚠️ 判据不能写成 `filepath.IsAbs(effective)`：环境变量覆盖可以是一个**相对路径**
	// （`AUTO_CLAUDE_PATH=.\bin\claude.exe`），那种情况下它既不在表里、也就永远不会被
	// 核对 —— 于是"覆盖指向死文件"这一档又被静默吞掉（这正是上一轮修的那个漏报的
	// 另一半）。凡是 resolver 特意返回的路径，就都该出现在这张表里。
	if effective != "" && effective != entry.CommandName {
		add("effective", "当前生效", effective)
	}
	if path, err := exec.LookPath(entry.CommandName); err == nil {
		add("path", "PATH 上找到的", path)
	}
	add("recorded", "登记表记录的安装位置", recorded.BinaryPath)
	if prefix != "" {
		install := agentNpmCLIInstall(entry)
		add("prefix-command", "安装目录里的命令入口", install.commandPath(prefix))
		add("prefix-binary", "安装目录里的可执行文件", install.binaryPath(prefix))
	}
	for _, candidate := range platformFallbackCandidates(entry) {
		add("platform-fallback", "平台兜底候选位置", candidate)
	}
	return candidates
}

// probeDiagnosePaths 给候选路径补上"存不存在 / 能不能跑 / 什么版本"。
//
// 实测**不超过 diagnoseProbeBudget 条**（按优先级取，也就是生效那条 + 紧随其后的
// 另一条）。剩下的只核对"存不存在" —— 不执行、不产生额外子进程，所以它们的
// Probed 为 false：界面对那一档只能说"存在，未实测"。
func (s *Server) probeDiagnosePaths(ctx context.Context, entry AgentCatalogEntry, candidates []pathCandidate) ([]diagnosePathFact, map[string]pathProbe) {
	facts := make([]diagnosePathFact, 0, len(candidates))
	probes := map[string]pathProbe{}
	for _, candidate := range candidates {
		checked, exists := diagnosePathExistence(candidate.Path)
		fact := diagnosePathFact{Source: candidate.Source, Label: candidate.Label, Path: candidate.Path, Checked: checked}
		fact.Exists = exists
		if fact.Exists && len(probes) < diagnoseProbeBudget {
			probe := probeExecutable(ctx, candidate.Path, entry.VersionArgs)
			probes[candidate.Path] = probe
			fact.Probed = true
			fact.Works = probe.Works
			fact.Version = probe.Version
		}
		facts = append(facts, fact)
	}
	return facts, probes
}

// diagnosePathExistence 把"不存在"与"读不到"分开。
//
// 为什么不直接用 `fileExists`：它把 Stat 的**权限 / IO 失败**与"真的不存在"压成同一个
// false（`err == nil && !info.IsDir()`）—— 于是"我读不到"会被念成"不存在"，正是本项目
// 最常复发的那个红线。这一档只有两种真相，必须分开：
//
//	checked=false            → 读不到，**不下结论**（界面念"未核对"）；
//	checked=true、exists=false → 真的没有。
func diagnosePathExistence(path string) (checked, exists bool) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return true, !info.IsDir()
	case os.IsNotExist(err):
		return true, false
	default:
		return false, false
	}
}

// pathProbe 是一次 `--version` 实测的结果。
type pathProbe struct {
	Path     string
	Exists   bool
	Works    bool
	TimedOut bool
	// Canceled 表示这次实测**被中断了**（父上下文取消：请求断了 / 操作结束了）。
	// 它与"太慢"和"坏了"都不同：那是**我们没查完**，不是这个工具的问题。
	Canceled bool
	Version  string
	// Detail 是失败时的现场（已清洗、已截断）—— 用户唯一的线索不能丢。
	Detail string
}

// probeExecutable 执行一次 `<path> --version`，并**把"超时"与"失败"分开**。
//
// 这个区分不是洁癖：超时意味着"启动太慢或卡住了"（再试一次可能就好），
// 失败意味着"产物坏了"（必须重装）。原先两条都被压成"未安装"。
func probeExecutable(ctx context.Context, path string, args []string) pathProbe {
	return probeExecutableWithin(ctx, path, args, diagnoseVersionTimeout)
}

// probeExecutableWithin 与上面同一个实现，只是预算可传 —— 让"超时"那条分支可以在
// 测试里用几百毫秒逼出来，而不必真的等 8 秒。
func probeExecutableWithin(ctx context.Context, path string, args []string, timeout time.Duration) pathProbe {
	probe := pathProbe{Path: path, Exists: fileExists(path)}
	if !probe.Exists {
		return probe
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, path, args...)
	configureProcessGroup(cmd)
	// WaitDelay 是这里**必需**的一句，不是保险：进程被 Kill 之后，它留下的后代
	// （npm 的子 shell、CLI 自己拉起的后台进程）仍然握着 stdout 的写端，于是
	// cmd.Wait() 会一直等到那些进程退出 —— 一个卡住的 CLI 会让一次探测从 8 秒
	// 变成几十秒，而这期间界面什么都不会发生。
	cmd.WaitDelay = 2 * time.Second
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	// probeCtx 自己到点才会是 DeadlineExceeded；父上下文被取消是 Canceled
	// （那是"这次没查完"，不是"这个工具太慢"——两者也不能混说）。
	probe.TimedOut = errors.Is(probeCtx.Err(), context.DeadlineExceeded)
	probe.Canceled = errors.Is(probeCtx.Err(), context.Canceled)
	if err != nil {
		probe.Detail = tailUpdateOutput(out.String())
		if probe.Detail == "" {
			probe.Detail = errorText(err)
		}
		return probe
	}
	probe.Version = agentVersionFromOutput(out.String())
	probe.Works = probe.Version != ""
	if !probe.Works {
		probe.Detail = "命令成功退出，但没有报出版本号"
	}
	return probe
}

// ── 各项检查 ────────────────────────────────────────────────────────────────

// assessResolvedPath 判定"现在生效这份到底怎么了"。
//
// 五种真相各给各的说法，判据不许合并：
//   - resolver 什么都没命中（它返回裸命令名）→ 只有**登记过**才算"原来装着、现在没了"，
//     从没登记过的就是"真的没装"，不该报毛病；
//   - 命中了一个绝对路径但文件不在 → 那是环境变量覆盖指向了死文件。**这一档必须
//     由"路径事实"判定**（有过实测则一定是"存在"，见调用处的注释）；
//   - 文件在但这一轮没实测它 → 什么结论都给不出，如实记一条"没查"；
//   - 执行超时 → 能跑，只是没在预算内回答；
//   - 执行失败 → 产物坏了，必须重装。
func (s *Server) assessResolvedPath(
	report *agentDiagnosis, entry AgentCatalogEntry, effective string,
	fact diagnosePathFact, hasEffective bool,
	probe pathProbe, probed bool,
	hasRecord bool, recorded agentInstallation,
) {
	switch {
	case !hasEffective:
		if hasRecord && recorded.BinaryPath != "" && !fileExists(recorded.BinaryPath) {
			report.add(issueBinaryMissing, severityBlocker,
				fmt.Sprintf("登记表记录的安装位置已经不存在，现在也找不到可用的 %s", entry.CommandName),
				[]string{"登记位置：" + recorded.BinaryPath, "登记版本：" + diagnoseOrDash(recorded.Version)},
				remedyReinstall)
		}
	case !fact.Checked:
		// 连"在不在"都没读到（权限 / IO）。**不许**顺着 `!fact.Exists` 报成"路径不存在"
		// —— 那是把"读不到"写成"没有"。
		report.limit(fmt.Sprintf("当前生效的 %s 读不到（权限或 IO 失败），因此没有它的结论", effective))
	case !fact.Exists:
		// **刻意不给修复动作**：这里要改的是部署方设的那条显式覆盖（环境变量 / 启动
		// 配置），平台既清不掉它、也不能绕过它 —— 重装同样没有用，因为 override 依然
		// 优先于一切。给一个点了修不好的按钮比不给更坏（界面会照实说"平台不能自动修"）。
		report.add(issueOverrideDead, severityBlocker,
			"配置里指定的可执行文件路径不存在。它优先于其它所有查找方式，所以这个工具现在用不了 —— 请修正或删掉那条配置后重新检测",
			[]string{"配置路径：" + effective, "配置来源：部署时的显式覆盖（环境变量 / 启动配置）"})
	case !probed:
		// 文件在，但这一轮没有实测它。**不许**顺着 `!probe.Works` 报成"执行失败" ——
		// 那是把"没查"写成"坏了"。如实说这一项没查。
		report.limit(fmt.Sprintf("当前生效的 %s 这一轮没有实测（实测预算用在了别的候选上，因此没有它的执行结论）", effective))
	case probe.Canceled:
		// 实测被中断（请求断了 / 操作已结束）。**同样不许**报成"执行失败"：
		// 那是我们没查完，不是它坏了 —— 一次断连不该在界面上留下一条假的 blocker。
		report.limit(fmt.Sprintf("当前生效的 %s 这次实测被中断，没有得出执行结论", effective))
	case probe.TimedOut:
		report.add(issueProbeTimeout, severityWarning,
			fmt.Sprintf("%s 在 %s 内没有响应（可能是首次启动慢，也可能进程卡住了）",
				entry.Name, diagnoseVersionTimeout),
			[]string{"执行：" + effective, "预算：" + diagnoseVersionTimeout.String()})
	case !probe.Works:
		report.add(issueBinaryBroken, severityBlocker,
			fmt.Sprintf("%s 的安装文件存在，但执行不起来（多半是一次中断的安装留下的半成品）", entry.Name),
			[]string{"路径：" + effective, "执行结果：" + diagnoseOrDash(probe.Detail)},
			remedyReinstall)
	}
}

// diagnosePathFactByPath 在路径事实表里按同一个规范化规则找某一条。
//
// 用同一个规则（sameCleanPath）而不是字符串相等：那张表的路径是经过 TrimSpace 的，
// 而 resolver 返回的可能是原样的字符串 —— 差一个空格就会让"找到了"变成"没找到"。
func diagnosePathFactByPath(facts []diagnosePathFact, path string) (diagnosePathFact, bool) {
	if strings.TrimSpace(path) == "" {
		return diagnosePathFact{}, false
	}
	for _, fact := range facts {
		if sameCleanPath(fact.Path, path) {
			return fact, true
		}
	}
	return diagnosePathFact{}, false
}

// assessInstallPrefix 查安装目录内部：命令入口 + npm 包目录。
func (s *Server) assessInstallPrefix(report *agentDiagnosis, entry AgentCatalogEntry, prefix string, recorded agentInstallation, hasRecord bool) {
	install := agentNpmCLIInstall(entry)

	// 命令入口。分"不存在"与"存在但悬空"—— 后者执行时的报错
	//（Windows 是"系统找不到指定的路径"）完全指不到真正的原因。
	shim := diagnoseCommandShim(prefix, entry)
	switch {
	case !shim.Exists:
		report.add(issueShimMissing, severityWarning,
			fmt.Sprintf("安装目录里没有 %s 的命令入口（%s）", entry.CommandName, filepath.Base(shim.Path)),
			[]string{"入口路径：" + shim.Path, "包目录：" + install.packageRoot(prefix)},
			remedyRebuildShim, remedyReinstall)
	case shim.Dangling:
		report.add(issueShimDangling, severityWarning,
			fmt.Sprintf("%s 的命令入口指向一个不存在的目标——执行它会报路径找不到", entry.Name),
			[]string{"入口路径：" + shim.Path, "目标：" + diagnoseOrDash(shim.Target)},
			remedyRebuildShim, remedyReinstall)
	}

	// 包目录。扫不动（权限/IO）与"没有"必须分开。
	scan := scanNpmPackages(prefix, entry)
	if scan.ScanError != "" {
		report.add(issuePackageScanFailed, severityWarning, scan.ScanError, nil)
		return
	}
	if scan.ActivePresent && scan.ActiveVersion == "" {
		report.add(issueActivePackageBroken, severityBlocker,
			fmt.Sprintf("%s 的包目录存在，但读不出包信息（安装不完整）", entry.Name),
			[]string{"包目录：" + filepath.Join(install.packageRoot(prefix), install.packageName)},
			remedyReinstall)
	}
	if len(scan.Interrupted) > 0 {
		report.add(issuePackageInterrupted, severityWarning,
			fmt.Sprintf("发现 %d 份中断安装留下的残骸目录（它们不会被自动清理）", len(scan.Interrupted)),
			append([]string{"包目录：" + install.packageRoot(prefix)}, diagnoseTruncate(scan.Interrupted, 4)...),
			remedyCleanupInterrupted)
	}
	// 可回滚的备份：**只有版本与登记版本一致时才算数** —— 与
	// rollbackInterruptedNpmInstall 的判据一致（它也只认 previous 那一个版本）。
	// 不一致的备份恢复过去会让版本悄悄回退，那比"修不好"更坏。
	if hasRecord && recorded.Version != "" {
		for _, backup := range scan.Backups {
			if backup.Version != recorded.Version {
				continue
			}
			report.add(issuePackageBackup, severityInfo,
				fmt.Sprintf("发现一份完整的旧版本 %s，可以直接回滚过去（不必联网重装）", backup.Version),
				[]string{"备份目录：" + backup.Path, "登记版本：" + recorded.Version},
				remedyRestoreBackup, remedyReinstall)
			break
		}
	}
}

// diagnoseShared 是一次诊断里"与具体工具无关"的读数。
//
// 为什么要有它：批量端点会为**每个**工具跑一次完整诊断（goroutine 并发），而运行时状态
// 是工具无关的 —— 每个工具各探一遍，就是 N 份同样的 `node --version` / `npm --version`
// 子进程，外加 N 次并发的 Node 版本索引下载（runtimeManager 只有 TTL 缓存、没有
// single-flight，冷缓存时那几个 goroutine 会各下一份）。
//
// runtime 定义成**函数**而不是值：调用方据此把探测推迟到真的需要时 —— 一个工具都不需要
// 详查时（全部就绪），一次都不会探。nil 表示"调用方没有预探"，此时自己探一次。
type diagnoseShared struct {
	runtime func() runtimeStatus
}

// probedRuntime 取本地运行时读数：有预探的用预探的，没有就自己探。
func (s *Server) probedRuntime(ctx context.Context, shared diagnoseShared) runtimeStatus {
	if shared.runtime != nil {
		return shared.runtime()
	}
	return s.probeRuntime(ctx)
}

// assessRuntime 按**登记里的安装方式**分流查前置运行时。
//
// 托管工具用 `probeRuntime` 的读数（界面上的运行时卡片读的是同一个函数，于是"诊断说
// 运行时没问题、卡片说没有"这种自相矛盾不可能出现）；系统 npm 安装的走
// `assessSystemNpmRuntime`（它必须看系统那一套，理由写在那个函数上）。
func (s *Server) assessRuntime(ctx context.Context, shared diagnoseShared, report *agentDiagnosis, entry AgentCatalogEntry, recorded agentInstallation, hasRecord bool) {
	npmKind := hasRecord && (recorded.InstallKind == installKindNpmManaged || recorded.InstallKind == installKindNpmSystem)
	if !npmKind {
		return
	}
	if recorded.InstallKind != installKindNpmManaged {
		// 装在**系统 npm 全局**里的工具：判据必须来自系统那一套，见 assessSystemNpmRuntime。
		// 顺带也省掉一次它用不上的托管读数（`node --version` + `npm --version`）。
		s.assessSystemNpmRuntime(ctx, report, entry, recorded)
		return
	}
	// —— 以下只处理"平台托管的 npm 工具"：它用的运行时就是我们自己那套工具链 ——
	status := s.probedRuntime(ctx, shared)
	if status.Origin == "managed" && status.Version == "" {
		// 托管运行时的文件在、但执行不起来。probeRuntime 在这种情况下**不会**回落到
		// 系统 node（它回落的条件是 Origin == "none"），于是 status.Installed 为 false
		// —— 与"压根没装运行时"外观相同。下一步动作一样（重装运行时），但说法必须不同。
		report.add(issueRuntimeBroken, severityBlocker,
			"平台托管的 Node.js 运行时装着但执行不起来，装在它下面的工具因此全都用不了",
			[]string{"运行时目录：" + diagnoseOrDash(status.ManagedPath)},
			remedyInstallRuntime)
		return
	}
	if !status.Installed {
		report.add(issueRuntimeMissing, severityBlocker,
			fmt.Sprintf("%s 需要一个 Node.js 运行时，但这台机器上找不到可用的", entry.Name),
			[]string{"要求：Node >= " + entry.MinRuntimeVersion},
			remedyInstallRuntime)
		return
	}
	if status.NpmVersion == "" {
		report.add(issueNpmUnavailable, severityBlocker,
			"找到了 Node.js，但随包的 npm 不可用——装不了也修不了 CLI",
			[]string{"运行时：" + status.Version, "来源：" + status.Origin},
			remedyInstallRuntime)
		return
	}
	if status.Origin != "managed" {
		report.add(issueRuntimeMissing, severityBlocker,
			fmt.Sprintf("%s 原本装在平台托管的工具链里，但那套运行时已经不在了", entry.Name),
			[]string{"登记安装方式：" + recorded.InstallKind, "当前生效的运行时来源：" + status.Origin},
			remedyInstallRuntime)
		return
	}
	// 版本是否够用：托管工具用的就是我们那个 node，与安装闸门（plan.RuntimeVersion =
	// runVersionCommand(managedNodeBinary)）读的是**同一个文件**，所以这里的判据同源。
	if meets, err := runtimeMeetsMinimum(status.Version, entry.MinRuntimeVersion); err == nil && !meets {
		report.add(issueRuntimeTooOld, severityBlocker,
			fmt.Sprintf("当前 Node %s 低于 %s 要求的 %s", status.Version, entry.Name, entry.MinRuntimeVersion),
			[]string{"运行时来源：" + status.Origin},
			remedyInstallRuntime)
	}
}

// assessSystemNpmRuntime 判"装在系统 npm 全局里的工具，它的运行时够不够用"。
//
// ⚠️ 判据必须来自**系统那一套**，不能拿 `probeRuntime` 的读数 —— 后者是"托管优先"的
//（`runtime_install.go`：托管命中就不看系统）。拿它去判一个系统 npm 安装的工具，
// 会在"托管运行时坏掉/太老、系统 node 正常"的机器上给出**假的 blocker**：
// 分明能跑的工具被判成"找不到可用的 Node"或"版本过低"。
//
// 而这里每一步都用既有 helper（`nodeVersionNearNpm` 正是 installAgentCLI 里
// `plan.RuntimeVersion` 的来源），所以不新造判据。
//
// **刻意不给修复动作**：平台能装的只有**托管**运行时，而它换不掉这个工具实际用的
// 那个 node（工具与 npm 都在系统那边）。给一个点了修不好的按钮比不给更坏 ——
// 界面会照实说"平台不能自动修"，证据里指清该去升级哪套 Node。
func (s *Server) assessSystemNpmRuntime(ctx context.Context, report *agentDiagnosis, entry AgentCatalogEntry, recorded agentInstallation) {
	npmPath, lookupErr := exec.LookPath("npm")
	if lookupErr != nil {
		report.add(issueNpmUnavailable, severityBlocker,
			"这台机器上找不到系统 npm，而这个工具（与它的更新）都挂在系统 npm 上——请先装好系统的 Node.js / npm",
			[]string{"登记安装方式：" + recorded.InstallKind, "登记位置：" + diagnoseOrDash(recorded.BinaryPath)})
		return
	}
	version := nodeVersionNearNpm(ctx, npmPath)
	if version == "" {
		report.add(issueRuntimeMissing, severityBlocker,
			fmt.Sprintf("%s 用的是系统 npm（%s），但那套 npm 旁边找不到可用的 node", entry.Name, npmPath),
			[]string{"要求：Node >= " + entry.MinRuntimeVersion})
		return
	}
	if meets, compareErr := runtimeMeetsMinimum(version, entry.MinRuntimeVersion); compareErr == nil && !meets {
		report.add(issueRuntimeTooOld, severityBlocker,
			fmt.Sprintf("系统 Node %s 低于 %s 要求的 %s", version, entry.Name, entry.MinRuntimeVersion),
			[]string{"运行时来源：系统（" + npmPath + "）"})
	}
}

// assessRecord 查登记表与磁盘实测是否一致。
func (s *Server) assessRecord(report *agentDiagnosis, entry AgentCatalogEntry, recorded agentInstallation, hasRecord bool, paths []diagnosePathFact) {
	if !hasRecord {
		return
	}
	switch recorded.InstallKind {
	case "":
		report.add(issueRecordKindUnknown, severityWarning,
			"这台机器上有安装记录，但没记下是用什么方式装的——重装一次会把它写成平台认识的方式",
			[]string{"登记位置：" + recorded.BinaryPath}, remedyReinstall)
	case installKindNpmManaged, installKindNpmSystem:
		// 正常的两种，不报。
	case installKindNative:
		report.add(issueNativeUnmanaged, severityInfo,
			fmt.Sprintf("%s 由官方安装器安装，平台不接管它的升级", entry.Name),
			[]string{"需要更新时请在目标环境执行：" + entry.CommandName + " update"})
	default:
		report.add(issueRecordKindUnknown, severityWarning,
			fmt.Sprintf("认不出的安装方式 %q —— 平台不会去猜它属于哪一档（猜错会把工具装到第二个位置）；重装一次可以把它改写成一种确定的安装方式", recorded.InstallKind),
			[]string{"登记位置：" + recorded.BinaryPath}, remedyReinstall)
	}

	// 登记的位置已经不在，但工具仍然能用（别处有一份）—— 与"完全不可用"不同：
	// 这一档不影响现在，只是下一次升级会重装回那个已经不在的位置。
	// 真正"哪儿都用不了"的那一支由 assessResolvedPath 报成 blocker。
	if recorded.BinaryPath != "" && !fileExists(recorded.BinaryPath) && diagnoseAnyWorks(paths) {
		report.add(issueRecordStale, severityWarning,
			"登记表指的安装位置已不存在，现在用的是别处那一份（升级时会重装回登记的位置）",
			[]string{"登记位置（已不存在）：" + recorded.BinaryPath},
			remedyReinstall)
	}
}

// diagnosePreflightLocal 跑一遍安装计划解析，把结论提前说出来。
//
// 直接调 `resolveAgentInstallPlan` —— 它是**只读**的（fileExists + LookPath + 登记表），
// 而且与真正点「安装」时用的是同一份判据。**不要**在这里另写一套判断：那正是
// docs/42 §19.2 E 收敛掉的东西。
//
// ⚠️ 解析出计划**不等于**装得上：`installAgentCLI` 在解析之后还有一道运行时闸门
// （`checkRuntimeGate`），Node 太旧时是"计划有了但装不了"。只报前半段的话，预检说
// "可以装"、点下去才报"运行时版本过低" —— 而把这句提前正是预检存在的唯一理由。
// 所以这里调**同一个函数**，不另判一遍（判据分裂是本项目付过代价的错）。
func (s *Server) diagnosePreflightLocal(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) *diagnosePreflight {
	preflight := &diagnosePreflight{}
	plan, err := s.resolveAgentInstallPlan(ctx, meta.ID, entry.ID)
	if err == nil {
		err = checkRuntimeGate(plan.RuntimeVersion, entry)
	}
	if err == nil {
		preflight.InstallOK = true
		preflight.UpgradeOK = true
		return preflight
	}
	preflight.InstallReason = errorText(err)
	// 升级走的路与安装相同（performAgentUpdate 对 npm 类就是重装 latest），
	// 所以两者共用同一个判据 —— 不另判一遍。
	preflight.UpgradeReason = preflight.InstallReason
	return preflight
}

// lastAgentInstallFailure 取该工具最近一次失败的原文。
//
// 这是一条**零成本**的证据（数据本来就在 agent_install_audit 里，写入点见 app.go 的
// installAgentFor / updateAgent / installRuntime），而在此之前没人读过它。
// 原样返回之前先清洗并截断：脏控制字符与超长输出都不该进界面。
func (s *Server) lastAgentInstallFailure(ctx context.Context, runnerID, agentID string) *installAuditEntry {
	items, err := s.listInstallAudit(ctx, runnerID, 50)
	if err != nil {
		return nil
	}
	for index := range items {
		item := items[index]
		if item.AgentID != agentID || item.Result != "failed" {
			continue
		}
		item.Detail = tailUpdateOutput(item.Detail)
		return &item
	}
	return nil
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// agentNpmCLIInstall 把目录条目翻译成 npm 落点描述。
// 三处需要它（安装自检、诊断、修复），所以只有一份实现。
func agentNpmCLIInstall(entry AgentCatalogEntry) npmCLIInstall {
	return npmCLIInstall{
		scope:       packageScope(entry.NpmPackage),
		packageName: packageBase(entry.NpmPackage),
		commandName: entry.CommandName,
		binFile:     entry.BinFile,
	}
}

// diagnoseInstallPrefix 找出"这份安装落在哪个 prefix"，找不到就如实说找不到。
//
// 优先级：登记表里记的 prefix（我们自己写的，权威）> 系统 npm 自己报的全局 prefix。
// **不靠推路径**：`/usr/local/bin/claude` 反推 `/usr/local` 看着对，但那是猜，
// 而猜错会让"包目录不完整"这条检查指向一个无关的目录。
func (s *Server) diagnoseInstallPrefix(ctx context.Context, recorded agentInstallation, hasRecord bool) (string, string) {
	if hasRecord && strings.TrimSpace(recorded.Prefix) != "" {
		return recorded.Prefix, ""
	}
	if !hasRecord || recorded.InstallKind != installKindNpmSystem {
		return "", ""
	}
	prefix, err := npmGlobalPrefix(ctx)
	if err != nil {
		return "", fmt.Sprintf("系统 npm 全局安装的产物目录未扫描（问 npm 的全局 prefix 失败：%s）", errorText(err))
	}
	return prefix, ""
}

// npmGlobalPrefix 问 npm 自己的全局 prefix（只读）。
// 与 prepareNpmCLIRecovery 用的是同一条命令，不另造第二套问法。
func npmGlobalPrefix(ctx context.Context) (string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(lookupCtx, "npm", "prefix", "-g")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		return "", errors.New("npm 没有报出全局 prefix")
	}
	return prefix, nil
}

// shimProbe 是一次命令入口检查的结果。
type shimProbe struct {
	Path   string
	Exists bool
	// Dangling 表示入口在，但它指向的目标不存在。执行它会失败，而报错
	//（Windows 是"系统找不到指定的路径"）指不到真正的原因。
	Dangling bool
	Target   string
}

// diagnoseCommandShim 判定安装目录里的命令入口是否可用。
//
// **不只看"文件在不在"**：符号链接可以悬空，而 os.Stat 会跟过去 —— 于是
// fileExists 返回 false，"悬空"与"压根没有入口"就被混成了同一句话。
// 所以先用 Lstat 分"存在/不存在"，再用 Stat 分"能打开/悬空"。
// Windows 的入口是普通 .cmd（不是链接），要读它里面的目标才能判悬空。
func diagnoseCommandShim(prefix string, entry AgentCatalogEntry) shimProbe {
	probe := shimProbe{}
	if prefix == "" {
		return probe
	}
	probe.Path = agentNpmCLIInstall(entry).commandPath(prefix)
	linkInfo, err := os.Lstat(probe.Path)
	if err != nil {
		// Lstat 报"不存在"才是真的没有；其它错误（权限…）按"存在但查不了"处理，
		// 免得把"读不到"说成"没有"。
		probe.Exists = !os.IsNotExist(err)
		return probe
	}
	probe.Exists = true
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		if _, err := os.Stat(probe.Path); err != nil {
			probe.Dangling = true
		}
		return probe
	}
	if runtime.GOOS == "windows" {
		if target := windowsShimTarget(probe.Path); target != "" {
			probe.Target = target
			probe.Dangling = !fileExists(target)
		}
	}
	return probe
}

// windowsShimTarget 从 .cmd 入口里解析出它实际要执行的目标。
//
// 我们和 npm 在 Windows 上写的入口都是这个形状（npm_cli_install.go:184 起的两种：
// `"%~dp0node_modules\…\claude.exe" %*` 与 `node "%~dp0node_modules\…\claude.js" %*`），
// 所以把 `%~dp0` 换成入口所在目录就是真实目标。解析不出来时返回空串 ——
// 那说明这个入口不是那两种形状，**不做判断**（不猜）。
func windowsShimTarget(shimPath string) string {
	raw, err := os.ReadFile(shimPath)
	if err != nil {
		return ""
	}
	text := string(raw)
	index := strings.Index(text, "%~dp0")
	if index < 0 {
		return ""
	}
	rest := text[index+len("%~dp0"):]
	rest = strings.TrimLeft(rest, "\"")
	if end := strings.IndexAny(rest, "\"\r\n"); end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	// `%~dp0` 自带尾部分隔符，所以 rest 是相对入口所在目录的路径。
	return filepath.Join(filepath.Dir(shimPath), rest)
}

// npmPackageScan 是一次包目录扫描的结果。
type npmPackageScan struct {
	// Interrupted 是回滚留下的残骸目录。
	Interrupted []string
	// Backups 是**完整的**旧包（能读出 package.json 的版本）。
	Backups []npmPackageBackup
	// ActiveVersion 是当前生效包目录里的版本；读不出来时为空。
	ActiveVersion string
	// ActivePresent 表示生效包目录存在（哪怕读不出版本）。
	ActivePresent bool
	// ScanError 非空时上面的结论都不可信 —— 读不到 ≠ 没有。
	ScanError string
}

type npmPackageBackup struct {
	Path    string
	Version string
}

// scanNpmPackages 扫 prefix 下的包目录，找"半装残骸"与"可回滚的备份"。
//
// 两种命名都是**现有实现产生**的，不是照印象编的：
//   - npm 替换包时把旧的完整包改名成 `.{包名}-{版本}`（`npm_cli_install.go:122`
//     就是按这个前缀把它找回来的）；
//   - 回滚时把当时那份坏掉的包改名成 `.{包名}-interrupted-{nano}`（同文件 :144）。
//
// 目录不存在是"没有"，读不动是"读不到" —— 两者在这里分开（后者进 ScanError）。
func scanNpmPackages(prefix string, entry AgentCatalogEntry) npmPackageScan {
	scan := npmPackageScan{}
	if prefix == "" {
		return scan
	}
	install := agentNpmCLIInstall(entry)
	packageRoot := install.packageRoot(prefix)
	items, err := os.ReadDir(packageRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			scan.ScanError = fmt.Sprintf("读取包目录 %s 失败：%s", packageRoot, errorText(err))
		}
		return scan
	}
	backupPrefix := "." + install.packageName + "-"
	interruptedPrefix := "." + install.packageName + "-interrupted-"
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		name := item.Name()
		full := filepath.Join(packageRoot, name)
		switch {
		case strings.HasPrefix(name, interruptedPrefix):
			scan.Interrupted = append(scan.Interrupted, full)
		case strings.HasPrefix(name, backupPrefix):
			version, versionErr := npmPackageVersion(full)
			if versionErr == nil && version != "" {
				scan.Backups = append(scan.Backups, npmPackageBackup{Path: full, Version: version})
			}
		case name == install.packageName:
			scan.ActivePresent = true
			if version, versionErr := npmPackageVersion(full); versionErr == nil {
				scan.ActiveVersion = version
			}
		}
	}
	return scan
}

func diagnoseAnyWorks(paths []diagnosePathFact) bool {
	for _, path := range paths {
		if path.Works {
			return true
		}
	}
	return false
}

func diagnoseAnyExists(paths []diagnosePathFact) bool {
	for _, path := range paths {
		if path.Exists {
			return true
		}
	}
	return false
}

func diagnoseFirstVersion(paths []diagnosePathFact) string {
	for _, path := range paths {
		if path.Works && path.Version != "" {
			return path.Version
		}
	}
	return ""
}

func diagnoseOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func diagnoseTruncate(values []string, limit int) []string {
	if len(values) <= limit {
		return append([]string{}, values...)
	}
	out := append([]string{}, values[:limit]...)
	return append(out, fmt.Sprintf("…以及另外 %d 项", len(values)-limit))
}

// clampDiagnoseText 把一段**外部给的**文本（典型：客户端请求体里的字符串）压成
// 可以安全落进记录的形状：先去 ANSI、再脱敏、最后按**字符**截断（按字节会把 UTF-8
// 从中间切断）。
//
// 用途只有一个：凡是我们不认识的、由客户端送来的字符串要出现在响应或审计里时，
// 都先过这里 —— 请求体不该有办法往记录里写东西。
func clampDiagnoseText(value string, limit int) string {
	clean := redactAgentText(stripAnsi(value))
	runes := []rune(clean)
	if len(runes) <= limit {
		return clean
	}
	return string(runes[:limit]) + "…"
}
