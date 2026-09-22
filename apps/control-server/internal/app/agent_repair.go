package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// CLI 工具的修复动作（白名单）。
//
// 与诊断的分工：诊断**只读**、随时可以跑；修复是写操作，因此必须过与安装/升级
// **同一套**闸门（beginAgentMaintenance + remoteInstallAllowed + 审计），少任何一条
// 都是在开一个新的后门。
//
// 三条硬约束：
//
//  1. **`remedies` 是服务端下发 id 的复述，不是命令。** 服务端拿到 id 后在白名单里
//     查表执行，绝不把请求体拼进任何命令（对标 mcpRuntimeCommandPattern 的白名单纪律）。
//  2. **只执行"当前诊断允许的动作"**。判据是诊断报告里各症状给出的 remedies 并集
//     （applicableRemedies）—— 于是界面上的按钮与服务端允许的动作必然一致，
//     不会出现"界面亮着、服务端拒"（docs/42 §19.2 E 的那类分裂）。
//  3. **每个动作都要行为级验收**：不是"文件写好了"，而是"真的执行一次拿到版本"。
//     这是 verifyAgentInstall 早就定下的纪律 —— 只写文件而执行不起来，用户以为修好了。
//
// 明确不做（沿用 docs/42 §11 已拍板的操作面）：不做卸载、不删用户自己的安装
// （系统 npm 全局那份、官方安装器那份）、不用 sudo / 不提权、不允许指定任意版本。
// shadowed-install 那种"机器上有多份"只能报告 —— 删掉哪一份是用户的决定。

// remedyOrder 是执行顺序。**不信任请求里的顺序**：先回滚（本地 rename，秒级、
// 不依赖网络）再清理残骸，最后才考虑联网重装。能一步回到可用，就不该先冒险下载。
var remedyOrder = []string{
	remedyRestoreBackup,
	remedyCleanupInterrupted,
	remedyRebuildShim,
	remedyInstallRuntime,
	remedyReinstall,
}

// agentRemedy 是一个可执行的修复动作。
type agentRemedy struct {
	ID    string
	Label string
	// Detail 会出现在确认框里：说明它做什么、可能影响什么。
	Detail string
	// LocalOnly 的动作只能在本机执行 —— 它们直接操作磁盘（重写入口、改名包目录），
	// 跨端要另写一套在目标环境里跑的东西（docs/43 §10 第 6 步）。
	LocalOnly bool
	Apply     func(ctx context.Context, rc remedyContext) (string, error)
}

// remedyContext 是一次修复需要的全部输入。
//
// 它在执行前由**诊断**填好（recorded / prefix / backup），而不是各动作自己再查一遍：
// 各查一遍会让"执行时的状态"与"诊断时的状态"分叉，而那正是要避免的。
type remedyContext struct {
	Server    *Server
	RunnerID  string
	Entry     AgentCatalogEntry
	Recorded  agentInstallation
	HasRecord bool
	Prefix    string
	// Backup 是本次可回滚过去的那个备份包（没有则为零值）。
	Backup npmPackageBackup
	// Restored 记录本次是否已经执行过 restore-backup —— 重装那一步据此决定要不要跳过。
	Restored bool
}

// agentRemedies 是白名单表本身，也是**唯一的动作定义处**。
//
// 诊断只把这张表里存在的动作放进报告（见 agentDiagnosis.add），所以"界面照着诊断
// 亮出一个点了没反应的动作"这件事在结构上就不可能发生。
//
// 本期**不提供** `reset-record`（按实测重写安装登记）：它改的是"升级走哪条路"的
// 判据，风险比其余几个高一个量级，先只报告（docs/43 §10 第 7 步）。两条认不出安装
// 方式的症状因此改为给 reinstall —— 重装会把登记改写成一种确定的安装方式，
// 那是同一个问题的另一条（更安全）出路。
var agentRemedies = map[string]agentRemedy{
	remedyRestoreBackup: {
		ID:        remedyRestoreBackup,
		Label:     "回滚到上一份完整的安装",
		Detail:    "把当前那份不完整的包挪到一边，把上一份完整的包恢复回原位，并重建命令入口。不联网。",
		LocalOnly: true,
		Apply:     applyRestoreBackup,
	},
	remedyCleanupInterrupted: {
		ID:        remedyCleanupInterrupted,
		Label:     "清理中断安装留下的残骸",
		Detail:    "删除安装目录里那次中断安装留下的临时包目录。只删名字与本次安装完全对得上的那些。",
		LocalOnly: true,
		Apply:     applyCleanupInterrupted,
	},
	remedyRebuildShim: {
		ID:        remedyRebuildShim,
		Label:     "重建命令入口",
		Detail:    "在安装目录里按当前包位置重写命令入口（Windows 是 .cmd/.ps1，其它平台是符号链接）。入口可用时不改动。",
		LocalOnly: true,
		Apply:     applyRebuildShim,
	},
	remedyInstallRuntime: {
		ID:     remedyInstallRuntime,
		Label:  "安装 / 重装 Node.js 运行时",
		Detail: "从官方源下载并解压到平台自己的工具链目录（不需要管理员权限，也不会改动系统里已有的 Node）。下载完成后会校验官方 SHA256。",
		Apply:  applyInstallRuntime,
	},
	remedyReinstall: {
		ID:     remedyReinstall,
		Label:  "重新安装（回到登记的那个版本）",
		Detail: "执行 npm install -g 把该工具重装到它原本所在的位置。若已回滚成功则跳过，不再联网下载。安装期间该工具上的对话会被拒绝。",
		Apply:  applyReinstall,
	},
}

// ── 各动作 ──────────────────────────────────────────────────────────────────

// applyRebuildShim 重建命令入口。
//
// **入口已经可用时不动它**：那既是幂等（连跑两次不该改任何东西），也避免把一份
// 本来好好的入口覆盖成坏的。
func applyRebuildShim(ctx context.Context, rc remedyContext) (string, error) {
	if rc.Prefix == "" {
		return "", errors.New("不知道该在哪个安装目录里重建入口（这台机器上没有记下它的位置）")
	}
	install := agentNpmCLIInstall(rc.Entry)
	shim := diagnoseCommandShim(rc.Prefix, rc.Entry)
	if shim.Exists && !shim.Dangling {
		if probe := probeExecutable(ctx, shim.Path, rc.Entry.VersionArgs); probe.Works {
			return fmt.Sprintf("入口已经可用（%s），没有改动", probe.Version), nil
		}
	}
	if err := ensureNpmCLICommand(rc.Prefix, install); err != nil {
		return "", err
	}
	return verifyCommandPath(ctx, rc.Entry, install.commandPath(rc.Prefix), "入口已重建")
}

// applyCleanupInterrupted 删除中断安装留下的残骸目录。
func applyCleanupInterrupted(ctx context.Context, rc remedyContext) (string, error) {
	if rc.Prefix == "" {
		return "", errors.New("不知道该在哪个安装目录里清理（这台机器上没有记下它的位置）")
	}
	install := agentNpmCLIInstall(rc.Entry)
	packageRoot := install.packageRoot(rc.Prefix)
	removed, refused := 0, 0
	for _, target := range scanNpmPackages(rc.Prefix, rc.Entry).Interrupted {
		// 这一道判据在当前形状下**恒为真**：removableInterruptedDir 的四个条件是
		// scanNpmPackages 那句 `.{包名}-interrupted-` 前缀过滤的**超集**（基名由
		// filepath.Base 给出，必然不含分隔符，也必然不是生效包目录）。它留着是因为
		// 它守的是**唯一不可逆的一步**：一旦以后有人把扫描放宽（比如顺手把 npm 的
		// 备份包 `.{包名}-{版本}` 也收进来），这里就是最后一道闸。
		// 判据本身有独立的表驱动用例（TestRemovableInterruptedDirGuards）。
		if !removableInterruptedDir(packageRoot, filepath.Base(target), install) {
			// 判据不过就**不删**。宁可留下一份残骸，也不能删错目录。
			// 而且要说出来"我没删"，不能把它算进 removed —— 那会把"我没做"
			// 写成"没有这东西"。
			refused++
			continue
		}
		if err := os.RemoveAll(target); err != nil {
			return "", fmt.Errorf("清理 %s 失败：%w", target, err)
		}
		// 行为级验收：**这一个**必须真的没了。
		//
		// 原先这里写成"再扫一遍，看还有没有残骸"，那有两个问题：① 判据对着"所有残骸"
		// 而不是"我刚删的那些"；② 它证明不了删掉的是同一批东西。改成逐个确认。
		if fileExists(target) {
			return "", fmt.Errorf("清理 %s 之后它仍然存在", target)
		}
		removed++
	}
	switch {
	case removed == 0 && refused == 0:
		return "没有找到可以清理的残骸目录", nil
	case removed == 0:
		return fmt.Sprintf("%d 份残骸不符合删除判据，已保留（宁可不删也不能删错）", refused), nil
	case refused > 0:
		return fmt.Sprintf("已清理 %d 份残骸目录，另有 %d 份不符合删除判据已保留", removed, refused), nil
	}
	return fmt.Sprintf("已清理 %d 份残骸目录", removed), nil
}

// applyRestoreBackup 回滚到上一份完整的安装。
func applyRestoreBackup(ctx context.Context, rc remedyContext) (string, error) {
	if rc.Prefix == "" {
		return "", errors.New("不知道该在哪个安装目录里回滚（这台机器上没有记下它的位置）")
	}
	if rc.Backup.Path == "" {
		return "", errors.New("没有找到可回滚的完整备份")
	}
	install := agentNpmCLIInstall(rc.Entry)
	restored, err := rollbackInterruptedNpmInstall(rc.Prefix, rc.Backup.Version, install)
	if err != nil {
		return "", err
	}
	// 注：rc 是值传递，这里**不**改 rc.Restored —— 由调用方在动作成功后登记，
	// 免得多一个"从这里也能改状态"的入口。
	return verifyCommandPath(ctx, rc.Entry, install.commandPath(rc.Prefix), "已回滚到 "+restored)
}

// applyInstallRuntime 安装 / 重装托管 Node 运行时。
func applyInstallRuntime(ctx context.Context, rc remedyContext) (string, error) {
	installation, err := rc.Server.installRuntimeFor(ctx, rc.RunnerID, installRuntimeRequest{Version: "lts"})
	if err != nil {
		return "", err
	}
	// 行为级验收：node 与 npm 都要真的答出版本。
	runtime := rc.Server.runtimeStatusFor(ctx, rc.RunnerID)
	status, ok := runtime.(runtimeStatus)
	if ok {
		if status.Version == "" {
			return "", errors.New("运行时已落地，但 node 执行不起来——请把安装目录报给平台排查")
		}
		if status.NpmVersion == "" {
			return "", errors.New("运行时已落地，但随包的 npm 不可用——请把安装目录报给平台排查")
		}
		return fmt.Sprintf("Node %s 可用（npm %s）", status.Version, status.NpmVersion), nil
	}
	return "Node " + installation.Version + " 已安装", nil
}

// applyReinstall 把工具重装回它原本所在的位置。
func applyReinstall(ctx context.Context, rc remedyContext) (string, error) {
	install := agentNpmCLIInstall(rc.Entry)
	// 已经回滚好了就不必再联网重装 —— 这正是"先回滚再重装"的意思。
	if rc.Restored && rc.Prefix != "" {
		if probe := probeExecutable(ctx, install.commandPath(rc.Prefix), rc.Entry.VersionArgs); probe.Works {
			return fmt.Sprintf("回滚后已经可用（%s），跳过了重装", probe.Version), nil
		}
	}
	version := "latest"
	if rc.HasRecord {
		if _, err := parseSemver(strings.TrimSpace(rc.Recorded.Version)); err == nil {
			version = strings.TrimSpace(rc.Recorded.Version)
		}
	}
	installation, err := rc.Server.installAgentCLIFor(ctx, rc.RunnerID, rc.Entry.ID, version)
	if err != nil {
		// 回到登记版本失败时退一步用 latest：登记的那个版本可能已经从 registry 上撤了，
		// 而"可用的最新版"总比"卡在坏掉的旧版"好。第二次失败才如实报出来。
		if version != "latest" {
			if retry, retryErr := rc.Server.installAgentCLIFor(ctx, rc.RunnerID, rc.Entry.ID, "latest"); retryErr == nil {
				return fmt.Sprintf("%s 装不上（可能已从 registry 撤下），已改为安装最新版 %s", version, retry.Version), nil
			}
		}
		return "", err
	}
	return "已重装 " + installation.Version, nil
}

// ── 安全边界 ────────────────────────────────────────────────────────────────

// removableInterruptedDir 是"可以安全删掉的残骸目录"的**唯一判据**。
//
// 四道约束缺一不可（测试里逐条有反例）：
//  1. 基名非空、不是 "." / ".."、且**不含路径分隔符**（挡住 "../.." 之类的构造）；
//  2. 基名不能是生效包目录本身（否则会把装好的东西删掉）；
//  3. 基名必须以 `.{包名}-interrupted-` 开头 —— 挡住别的包的残骸，
//     也挡住 npm 自己的备份目录（那是 `.包名-版本`，没有 -interrupted-）；
//  4. 它必须真的是 packageRoot 的**直接子目录**。
func removableInterruptedDir(packageRoot, name string, install npmCLIInstall) bool {
	if name == "" || name == "." || name == ".." || name == install.packageName {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if !strings.HasPrefix(name, "."+install.packageName+"-interrupted-") {
		return false
	}
	return sameCleanPath(filepath.Dir(filepath.Join(packageRoot, name)), packageRoot)
}

// applicableRemedies 是"当前状态允许执行的动作"。
//
// 判据**只有一处**：诊断报告里各症状给出的 remedies 的并集。于是界面上的按钮与
// 服务端允许的动作必然一致。
//
// 例外只有一个，而且必须带 `!local` 这个条件：**跨端**诊断目前是受限的
//（分不出"没装"与"坏了"，因此给不出症状），那种状态下允许两个"把它弄回来"的动作，
// 否则跨端用户彻底没有出路。
//
// ⚠️ 为什么 `!local` 不能省：`diagnosisUnknown` 不止"跨端受限"这一档 ——
// 本机**登记表读失败**也是 unknown。而在那种情况下 `recordedForRepair` 给不出
// prefix，`reinstall` 进去会立刻以同一个读失败报错 ⇒ 界面上多出两个点了必失败的
// 按钮，正是这条规则要消灭的东西。
func applicableRemedies(report agentDiagnosis, local bool) map[string]bool {
	out := map[string]bool{}
	for _, issue := range report.Issues {
		for _, remedy := range issue.Remedies {
			out[remedy.ID] = true
		}
	}
	if !local && report.Status == diagnosisUnknown {
		out[remedyReinstall] = true
		out[remedyInstallRuntime] = true
	}
	return out
}

// remedyPlan 是"这次要跑哪些动作、按什么顺序"，以及被丢掉的。
type remedyPlan struct {
	Order   []string
	Dropped []repairStep
}

// planRemedies 把请求里的 id 收敛成一份可执行的计划。
//
// 顺序由服务端定（remedyOrder），**不信任请求里的顺序**；不在白名单里、
// 或者当前状态不允许的，一律进 Dropped（并在响应里如实说出来，不是静默丢弃）。
//
// ⚠️ 判断的**先后顺序有意义**：`LocalOnly && !local` 必须排在 `!allowed` **之前**。
// 反过来的话，跨端用户请求"重建入口"会被告知"诊断里没有给出它"——而真相是
// "这个动作要在目标机器上直接操作文件，跨端还没接通"。两句话对用户的含义完全不同。
func planRemedies(requested []string, report agentDiagnosis, local bool) remedyPlan {
	allowed := applicableRemedies(report, local)
	plan := remedyPlan{Order: []string{}, Dropped: []repairStep{}}
	wanted := map[string]bool{}
	seen := map[string]bool{}
	for _, id := range requested {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		remedy, known := agentRemedies[id]
		switch {
		case !known:
			// ⚠️ **不回显**请求体里的字符串当标识。它只该当"表键"用，而这里的 ID
			// 会随 `applied` 进审计、被落库 —— 那等于让请求体有办法往记录里写东西。
			// 所以标识用常量，客户端给的那个 id 经清洗与截断后只出现在**说明**里
			// （说明只回给发起方自己看）。
			plan.Dropped = append(plan.Dropped, repairStep{ID: remedyUnknownID, OK: false, Skipped: true,
				Detail: "不是平台支持的修复动作（收到的 id：" + clampDiagnoseText(id, 40) + "）"})
		case remedy.LocalOnly && !local:
			plan.Dropped = append(plan.Dropped, repairStep{ID: id, Label: remedy.Label, OK: false, Skipped: true,
				Detail: "该动作要在目标机器上直接操作文件，跨端尚未接通"})
		case !allowed[id]:
			plan.Dropped = append(plan.Dropped, repairStep{ID: id, Label: remedy.Label, OK: false, Skipped: true,
				Detail: "当前状态下这个动作不适用（诊断里没有给出它）"})
		default:
			wanted[id] = true
		}
	}
	for _, id := range remedyOrder {
		if wanted[id] {
			plan.Order = append(plan.Order, id)
		}
	}
	return plan
}

// ── HTTP ───────────────────────────────────────────────────────────────────

// remedyUnknownID 是"客户端报了一个我们不认识的动作"在记录里的标识。
//
// 刻意是个常量而不是请求里那个字符串：`repairStep.ID` 会随响应回给界面、也会进
// 审计落库，而那个字符串是外部输入。
const remedyUnknownID = "unknown-action"

// repairStep 是一条修复动作的执行结果。
type repairStep struct {
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Skipped 表示这个动作**压根没执行**（不在白名单 / 当前不适用 / 跨端不支持）。
	// 它与"执行了但失败"是两件事：界面要把它们分开说，审计也要分开记
	// —— 记成同一个 failed，审计就在撒谎（它记的是"这台机器上发生过什么"）。
	Skipped bool `json:"skipped,omitempty"`
}

// repairResult 是 POST .../repair 的响应。
type repairResult struct {
	Success bool `json:"success"`
	// Applied 按实际执行顺序列出（含被丢掉的，OK=false）。
	Applied []repairStep `json:"applied"`
	// Diagnosis 是修复后**重跑**的报告：界面上要能当场看出"症状真的没了"，
	// 而不是只收到一句"修复完成"。
	Diagnosis agentDiagnosis `json:"diagnosis"`
}

// repairAgentHandler 是 POST /api/runners/{runnerID}/agents/{agentID}/repair。
func (s *Server) repairAgentHandler(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := chi.URLParam(r, "agentID")
	meta, entry, err := s.resolveDiagnosisTarget(runnerID, agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	// 与 installAgentFor 同一道闸门：接口层必须自己拒绝，不能依赖界面不给按钮
	//（docs/42 §9.1 那条教训）。
	if !s.remoteInstallAllowed(r.Context(), runnerID) {
		writeError(w, http.StatusForbidden, fmt.Errorf("尚未授权在 %s 上安装；请先在该主机上确认一次", runnerID))
		return
	}
	var request struct {
		Remedies []string `json:"remedies"`
	}
	if !decodeOptional(w, r, &request) {
		return
	}
	if len(request.Remedies) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("没有指定要执行的修复动作"))
		return
	}
	if len(request.Remedies) > len(remedyOrder) {
		writeError(w, http.StatusBadRequest, errors.New("修复动作数量超出上限"))
		return
	}

	local := isLocalRunnerID(meta.ID)
	// 计划必须建立在**当前**状态上：诊断是权威，请求只是"想做哪些"的意愿。
	// 上下文（登记项 / prefix / 可回滚的备份）与报告一起取，保证两者同源 ——
	// 各自再查一遍会让"执行时的状态"与"诊断时的状态"分叉。
	//
	// ⚠️ 这一步**必须在拿闸门之前**做，顺序不能"顺手"调换：`beginAgentMaintenance`
	// 会置位 `runnerUpdating[runnerID,agentID]`，而 `agentMaintenanceActive` 读的正是
	// 那一位 —— 进了闸门再诊断，诊断只会得到"正在安装或升级，此刻结果不可信"，
	// 于是任何修复都跑不起来。
	//
	// 代价是一扇很小的窗：从这次诊断到真正拿到闸门之间，别的操作可能改变磁盘状态
	// （典型：另一台会话刚把备份包用掉了）。那一档由动作自己兜住 —— 每个 Apply 都在
	// 执行时重新核对它依赖的事实（没有备份就报"没有找到可回滚的完整备份"），
	// 而不是信这份快照。
	before, state := s.diagnosisWithRepairContext(r.Context(), meta, entry)
	plan := planRemedies(request.Remedies, before, local)
	if len(plan.Order) == 0 && len(plan.Dropped) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("没有可执行的动作"))
		return
	}
	if len(plan.Order) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("这些动作在当前状态下都不适用：%s",
			plan.Dropped[0].Detail))
		return
	}

	// 与安装/升级共用同一把闸门（并发 + 活跃会话）。
	release, ok := s.beginAgentMaintenance(w, r, runnerID, agentID, entry.Name)
	if !ok {
		return
	}
	// 释放要**幂等**：下面会在重跑诊断之前先放掉闸门（理由见那里），而出入口仍然
	// 靠 defer 兜住 panic 之类的路径 —— 两道一起，既不会漏放也不会重复放。
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(release) }
	defer releaseGate()

	repairCtx, cancel := context.WithTimeout(s.runtimeCtx, s.config.agentUpdateTimeout())
	defer cancel()

	rc := remedyContext{
		Server: s, RunnerID: runnerID, Entry: entry,
		Recorded: state.recorded, HasRecord: state.hasRecord, Prefix: state.prefix,
	}
	if state.backup != nil {
		rc.Backup = *state.backup
	}

	applied := []repairStep{}
	for index, id := range plan.Order {
		remedy := agentRemedies[id]
		detail, applyErr := remedy.Apply(repairCtx, rc)
		if applyErr != nil {
			applied = append(applied, repairStep{ID: id, Label: remedy.Label, OK: false, Detail: errorText(applyErr)})
			// 后面那些动作**没有执行**，必须如实留在结果里。
			//
			// 不这么做的话，用户只会看到"某一步失败"，无从知道计划还有没有别的部分
			// —— 与 plan.Dropped 同一条纪律：把"我没做"写成"没有这东西"是同一类错。
			// （审计也靠它，因为失败时只写一条记录，见 repairAuditDetail。）
			for _, rest := range plan.Order[index+1:] {
				skipped := agentRemedies[rest]
				applied = append(applied, repairStep{ID: rest, Label: skipped.Label, OK: false, Skipped: true,
					Detail: "前一步没有成功，这个动作没有执行"})
			}
			break
		}
		if id == remedyRestoreBackup {
			rc.Restored = true
		}
		applied = append(applied, repairStep{ID: id, Label: remedy.Label, OK: true, Detail: detail})
	}
	applied = append(applied, plan.Dropped...)

	// 修复动作都做完了 —— **先放掉闸门，再重跑诊断**。
	//
	// ⚠️ 顺序不能反，理由不是洁癖：维护位是**我们自己**刚置上的，而诊断见到维护位会
	// 立刻早退成 `unknown` + `maintenance-active`、**一条探测都不跑**。那样这份"修复后
	// 的报告"里一条症状都没有 ⇒ 界面拿它算差集，会把**所有**旧症状都判成"已解决"
	// （修复失败也照说不误），而"某症状确实消失了"这类断言会**静默变成恒真**
	// —— 2026-09-22 实测踩到，两轮复查都没抓到，因为它让断言变绿而不是变红。
	//
	// 代价是一扇很小的窗：极端情况下另一个操作可能刚好插进来，于是这份报告又变成
	// "维护中"。那一档由两层兜住：报告本身如实说"没查成"，前端也只在报告**有结论**时
	// 才拿它算差集（见 `resolvedIssues`）。
	releaseGate()
	after := s.buildAgentDiagnosis(repairCtx, meta, entry)
	// success 只看**真的执行过**的动作：被跳过（不适用 / 跨端不支持 / 不在白名单 /
	// 前一步失败）的动作不算"修复失败"—— 否则用户会收到一句"修复失败"，而实际症状
	// 可能已经修好了。被跳过的那些仍然在 applied 里（Skipped=true），界面能看见
	// 它们、也知道为什么。
	success := true
	for _, step := range applied {
		if !step.OK && !step.Skipped {
			success = false
			break
		}
	}
	auditResult := "succeeded"
	if !success {
		auditResult = "failed"
	}
	// **一次修复只写一条审计。**
	//
	// 原先失败路径上写了两条：循环里那条带错误原文的，和大循环之后这条带步骤一览的。
	// 同一个事件在审计里出现两次，等于说这台机器上失败过两回 —— 而审计唯一的职责
	// 就是"这台机器上发生过什么"。两条合并成一条：错误原文由 repairAuditDetail 带上。
	_ = s.recordInstallAudit(repairCtx, installAuditEntry{
		RunnerID: runnerID, AgentID: agentID, Action: "repair",
		Result: auditResult, Detail: repairAuditDetail(applied),
	})
	// ⚠️ **失败也回 200**，与 updateAgent 的 500 不同，这是有意的：
	// 修复是"多步、可能部分成功"的动作，前端必须拿到三样东西 ——
	// 哪一步失败了、被跳过了哪些、以及修复后重跑的诊断。
	// 回 500 的话 api() 会直接抛错，那三样全都拿不到，用户只剩一句通用文案。
	// 调用方要看结论请读 `success`（以及 applied 里每一步的 ok/skipped）。
	writeJSON(w, http.StatusOK, repairResult{Success: success, Applied: applied, Diagnosis: after})
}

// repairAuditDetail 把这次跑了哪些动作写进审计（一行，够复盘就行）。
//
// **三态分开记**：真的做了且成功 = `ok`、做了但失败 = `failed`、
// 压根没做 = `skipped`。把最后一档也记成 failed 会让审计说假话
//（"这台机器上发生过什么"是审计唯一的职责）。
// 失败那一步要把**现场**带上：那是排查的唯一线索，而响应里的 detail 不会进审计。
// 经 tailUpdateOutput（脱敏 + 去 ANSI + 截断）再落库 —— 与安装失败同一条纪律。
func repairAuditDetail(steps []repairStep) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		mark := "ok"
		switch {
		case step.Skipped:
			mark = "skipped"
		case !step.OK:
			mark = "failed"
		}
		entry := step.ID + "=" + mark
		if mark == "failed" && strings.TrimSpace(step.Detail) != "" {
			entry += "(" + tailUpdateOutput(step.Detail) + ")"
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, " ")
}

// ── 行为级验收 ──────────────────────────────────────────────────────────────

// verifyCommandPath 执行一次产物，确认它真的能跑。
//
// 只看"文件在不在"不够：一个下载不完整的包也会"存在"，而用户拿到的是"修好了"。
// 这条纪律来自 verifyAgentInstall —— 只是那里的场景是安装，这里是修复。
// lead 是成功时那句话的开头（"入口已重建"之类）。
func verifyCommandPath(ctx context.Context, entry AgentCatalogEntry, path, lead string) (string, error) {
	probe := probeExecutable(ctx, path, entry.VersionArgs)
	if probe.Works {
		return fmt.Sprintf("%s（实测可执行：%s）", lead, probe.Version), nil
	}
	if !probe.Exists {
		return "", fmt.Errorf("%s，但重置后的入口不存在（%s）", lead, path)
	}
	if probe.TimedOut {
		return "", fmt.Errorf("%s，但执行 %s 在 %s 内没有响应", lead, path, diagnoseVersionTimeout)
	}
	return "", fmt.Errorf("%s，但执行 %s 失败%s", lead, path, diagnoseOrDash(probe.Detail))
}

// ── 修复要用的内部上下文 ────────────────────────────────────────────────────
//
// 放在报告之外而不是塞进 agentDiagnosis 的 JSON：登记项、prefix、备份包路径都是
// 服务端内部事实，不该出现在给界面的响应里 —— 一旦进了响应，前端就会有人开始用它拼东西。

type diagnoseRepairContext struct {
	recorded  agentInstallation
	hasRecord bool
	prefix    string
	backup    *npmPackageBackup
}

// recordedForRepair 读出修复要用的上下文（与诊断同一套读取路径）。
func (s *Server) recordedForRepair(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) diagnoseRepairContext {
	out := diagnoseRepairContext{}
	recorded, hasRecord, err := s.recordedInstallation(ctx, meta.ID, entry.ID)
	if err != nil {
		return out
	}
	out.recorded, out.hasRecord = recorded, hasRecord
	out.prefix, _ = s.diagnoseInstallPrefix(ctx, recorded, hasRecord)
	if out.prefix == "" || !hasRecord || recorded.Version == "" {
		return out
	}
	for _, backup := range scanNpmPackages(out.prefix, entry).Backups {
		if backup.Version == recorded.Version {
			candidate := backup
			out.backup = &candidate
			break
		}
	}
	return out
}

// diagnosisWithRepairContext 把报告与它的内部上下文一起产出，供 repair 使用。
//
// ⚠️ 两者是**同一套读取路径**，但不是同一次读：登记项被读了两次（相隔微秒）。
// 别把它当"同源"——真正保证一致的是"两边都用 `recordedInstallation` 这套判据"，
// 而不是"只读了一次"。跨过这道窗口的状态变化由各动作自己兜住（见上方注释）。
func (s *Server) diagnosisWithRepairContext(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) (agentDiagnosis, diagnoseRepairContext) {
	return s.buildAgentDiagnosis(ctx, meta, entry), s.recordedForRepair(ctx, meta, entry)
}
