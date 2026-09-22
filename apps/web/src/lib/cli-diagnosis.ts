// CLI 工具故障诊断的前端模型（纯函数 + 类型）。
//
// 为什么单独一个文件：这些是**能出错的判据**（结论怎么念、动作怎么归并、结果怎么说），
// 而它们一旦留在页面里就只能靠源码断言去钉 —— 源码断言挡不住"逻辑写反了"。
// 放这里就能被真的调用、真的断言（与 `lib/task-mutations.ts`、`features/tasks/task-model.ts`
// 同一个理由）。
//
// ⚠️ 所有**内容**（症状、证据、动作的 label/detail）都由服务端下发，这里只做
// "怎么念"与"怎么归并"，绝不重写判定、也不维护 id→文案的映射 —— 那种映射必然与
// 服务端漂移，而漂移的表现是"按钮上写着一件事、点下去做的是另一件"。

/** 一条诊断症状。字段与服务端 diagnoseIssue 一一对应。 */
export type DiagnoseIssue = {
  code: string;
  severity: "blocker" | "warning" | "info";
  summary: string;
  evidence: string[];
  remedies: DiagnoseRemedy[];
};

/** 一个可执行的修复动作。label/detail 由服务端下发。 */
export type DiagnoseRemedy = { id: string; label: string; detail: string };

/**
 * "这个工具在这台机器上的一份安装"。
 *
 * `checked` / `probed` 不是装饰：它们回答"这一条我到底查到了没有"。
 *   - checked=false            → 跨端，这条路径在目标环境上，本机没核对过；
 *   - checked=true, exists=false → 核对过，确实没有；
 *   - checked=true, exists=true, probed=false → 文件在，但本轮没执行过它
 *     （实测预算是有限的）。这一档既不能说"可执行"也不能说"跑不起来"。
 */
export type DiagnosePathFact = {
  source: string;
  label: string;
  path: string;
  checked: boolean;
  exists: boolean;
  probed: boolean;
  works: boolean;
  version: string;
};

export type DiagnosePreflight = {
  installOk: boolean;
  installReason?: string;
  upgradeOk: boolean;
  upgradeReason?: string;
};

export type DiagnoseFailure = {
  action: string;
  result: string;
  detail?: string;
  fromVersion?: string;
  toVersion?: string;
  createdAt: string;
};

export type AgentDiagnosis = {
  agentId: string;
  status: string;
  version: string;
  issues: DiagnoseIssue[];
  paths: DiagnosePathFact[];
  lastFailure?: DiagnoseFailure;
  preflight?: DiagnosePreflight;
  limitations: string[];
  diagnosedAt: string;
};

export type RunnerDiagnosticsView = {
  runnerId: string;
  probeOk: boolean;
  probeError?: string;
  items: AgentDiagnosis[];
  /** 这一轮**没有详查**的工具 id。不能把它们渲染成"没有问题"。 */
  skipped: string[];
  limitations: string[];
};

export type RepairStep = { id: string; label?: string; ok: boolean; detail?: string; skipped?: boolean };
export type RepairResult = { success: boolean; applied: RepairStep[]; diagnosis: AgentDiagnosis };

/** 诊断结论的四种观感。`unknown` 是"没查成"，**不能**与 `ok` 合并。 */
export type DiagnosisTone = "ok" | "warn" | "bad" | "unknown";

// 五种真相各说各的：没有问题 / 发现了问题 / 真的没装 / 该环境不提供 /
// **检测没查成**。最后那一档尤其不能并进"没有问题"。
const DIAGNOSIS_LABELS: Record<string, string> = {
  ok: "没有问题",
  broken: "发现问题",
  "not-installed": "未安装",
  unknown: "检测未完成",
  unsupported: "该环境不提供",
};

/**
 * 结论怎么念。
 *
 * 认不出的状态一律按"检测未完成"说 —— 那是最保守也最不会误导的一档。
 * 反过来说：**不许回落到"没有问题"**，那会把一次没查成伪装成一切正常。
 */
export function diagnosisLabel(status: string): string {
  return DIAGNOSIS_LABELS[status] ?? "检测未完成";
}

export function diagnosisTone(status: string): DiagnosisTone {
  switch (status) {
    case "ok":
      return "ok";
    case "broken":
      return "bad";
    case "not-installed":
    case "unsupported":
      return "warn";
    default:
      return "unknown";
  }
}

/**
 * 该诊断给出的全部动作（按 id 去重，**保持服务端给的顺序**）。
 *
 * 不去排序：执行顺序是服务端的决定（先回滚、再修本地、最后才联网重装），
 * 界面重排一遍就是又造了一份判据。
 */
export function offeredRemedies(diagnosis: AgentDiagnosis): DiagnoseRemedy[] {
  const seen = new Set<string>();
  const out: DiagnoseRemedy[] = [];
  for (const issue of diagnosis.issues) {
    for (const remedy of issue.remedies) {
      if (seen.has(remedy.id)) continue;
      seen.add(remedy.id);
      out.push(remedy);
    }
  }
  return out;
}

/**
 * 这份报告是不是一次**有结论**的检测。
 *
 * `unknown` 那一档说的是"没查成"—— 它既不是"没问题"也不是"有问题"。判断复用
 * `diagnosisTone`（它已经把"认不出的状态"归到 unknown），所以这里与徽标的口径必然一致。
 */
export function isConclusiveDiagnosis(diagnosis: AgentDiagnosis): boolean {
  return diagnosisTone(diagnosis.status) !== "unknown";
}

/**
 * 修复结果的一句话总结。
 *
 * ⚠️ 这里**只出文案、不出成败**：成败是服务端算好的（`RepairResult.success`），
 * 界面直接消费它 —— 同一条规则在两处各实现一遍，改一处另一处不跟随。
 * 文案与那个布尔是必然一致的：`success=false` 只在存在"真的执行过但失败"的动作时成立，
 * 而那种情况下这里的文案就是那句"……没成功"。
 *
 * **三态分开**：`failed`（做了但没成）进失败文案；`skipped`（压根没执行，见
 * `RepairStep.skipped`）只说清"它被跳过了、为什么"，**不算失败** ——
 * 否则用户会收到一句"修复失败"，而实际症状已经修好了。
 */
export function describeRepair(steps: RepairStep[]): string {
  const failed = steps.find((step) => !step.ok && !step.skipped);
  if (failed) {
    return `${failed.label || failed.id} 没成功：${failed.detail || "没有更多信息"}`;
  }
  // ⚠️ 已完成的那一栏要排除 skipped：被跳过的动作也带 detail（那是"为什么没执行"），
  // 混进"做成了什么"里会读成一件自相矛盾的事（"入口已重建；不是平台支持的修复动作"）。
  const done = steps.filter((step) => step.detail && !step.skipped);
  const skipped = steps.filter((step) => step.skipped);
  const parts: string[] = [];
  if (done.length > 0) parts.push(done.map((step) => step.detail).join("；"));
  if (skipped.length > 0) {
    // 要说清**为什么**（确认框里承诺过这句话），不能只报一个数。
    const head = skipped.slice(0, 2).map((step) => `${step.label || step.id}：${step.detail || "没有说明"}`);
    const more = skipped.length > 2 ? `；另有 ${skipped.length - 2} 个` : "";
    parts.push(`另有 ${skipped.length} 个动作没执行（${head.join("；")}${more}）`);
  }
  if (parts.length === 0) return "没有需要执行的动作";
  return parts.join("；");
}

/**
 * 修复前后的对比：哪些症状在修复后**真的没了**。
 *
 * 用 `code` 当身份而不是文案 —— 文案会改，码是稳定的；而"修复完成"这句话本身
 * 证明不了任何事（docs/43 §6.3：不要只说修复完成）。
 * 传不了「修复前」时返回空数组（不是所有症状都算"已解决"）。
 *
 * ⚠️ 「修复后」那份还必须是一次**有结论**的检测：没查成时它可能只带着一条
 * "维护中 / 通道不可用"，拿它算差集会把**所有**旧症状都判成"已解决" ——
 * 那是把"没查"写成"已解决"，与把"读不到"写成"没有"是同一族。
 */
export function resolvedIssues(before: AgentDiagnosis | undefined, after: AgentDiagnosis): DiagnoseIssue[] {
  if (!before) return [];
  if (!isConclusiveDiagnosis(after)) return [];
  const remaining = new Set(after.issues.map((issue) => issue.code));
  return before.issues.filter((issue) => !remaining.has(issue.code));
}

/** "上次修复解决了什么"的一句话。没有解决任何症状时返回空串（界面据此不渲染这一行）。 */
export function resolvedSummary(issues: DiagnoseIssue[], limit = 2): string {
  if (issues.length === 0) return "";
  const head = issues.slice(0, limit).map((issue) => issue.summary).join("；");
  const more = issues.length > limit ? `；另有 ${issues.length - limit} 项` : "";
  return `上次修复已解决 ${issues.length} 项：${head}${more}`;
}

/**
 * 诊断面板里"一条症状都没有"时该说什么。
 *
 * **没查成的报告也是零症状**（跨端那档就是这样：`status=unknown` + 没有症状），
 * 所以这里必须先问"这份报告有结论吗" —— 否则面板会写着"没有发现任何症状"，
 * 而徽标上写的是"检测未完成"。
 */
export function diagnosisEmptyText(diagnosis: AgentDiagnosis): string {
  return isConclusiveDiagnosis(diagnosis)
    ? "这次检测没有发现任何症状。"
    : "这次没有得出结论 —— 原因见下面的「这次没查的部分」。";
}

/**
 * 这一轮**没有详查**某个工具时该怎么说。
 *
 * `skipped` 有两种成因，说法必须不同：
 *  ① 它当前就绪（没什么可疑的，不必详查）；
 *  ② 整条通道不可用（执行环境没准备好）—— 那不是"没有可疑迹象"，是**根本没查**。
 * 说成同一句，就会在通道坏掉时给每个工具都盖一个"没有可疑迹象"的章。
 */
export function skippedDiagnosisText(channelOk: boolean): string {
  return channelOk
    ? "这个工具当前没有可疑迹象，本轮没有详查；需要时可以点「检测这个工具」。"
    : "本轮没有检测这个工具：执行环境当前不可用（原因见上方的「无法检测」）。";
}

/** 路径事实那一列怎么念。**三种"没结论"与两种结论必须分开**。 */
export function pathFactState(fact: DiagnosePathFact): string {
  // 没核对过就**不许**下结论。把"没查"说成"不存在"（或"跑不起来"）是本项目的红线。
  if (!fact.checked) return "未核对";
  if (!fact.exists) return "不存在";
  // 存在但没执行过：既不能说好也不能说坏。
  if (!fact.probed) return "存在，未实测";
  return fact.works ? "可执行" : "存在但跑不起来";
}

/** 时间怎么念。读不出来就返回空串（**不编一个时间**出来）。 */
export function diagnoseMoment(value: string | undefined): string {
  if (!value) return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "";
  const clock = `${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`;
  const now = new Date();
  const sameDay = at.getFullYear() === now.getFullYear() && at.getMonth() === now.getMonth() && at.getDate() === now.getDate();
  return sameDay ? `今天 ${clock}` : `${at.getMonth() + 1}月${at.getDate()}日 ${clock}`;
}

/**
 * 诊断面板顶部那行"读数元信息"：实测版本 + **什么时候测的**。
 *
 * 时间必须写：报告是一次**快照**（诊断是显式动作，页面开着它就一直挂着），
 * 不写时间等于把一份旧读数当成此刻的事实。两样都拿不到时返回空串（不编）。
 */
export function diagnosisMetaLine(diagnosis: AgentDiagnosis): string {
  const parts: string[] = [];
  if (diagnosis.version) parts.push(`实测版本 ${diagnosis.version}`);
  const when = diagnoseMoment(diagnosis.diagnosedAt);
  if (when) parts.push(`诊断于 ${when}`);
  return parts.join(" · ");
}

/**
 * 预检要说的话。
 *
 * 安装与升级在服务端共用同一套判据（`resolveAgentInstallPlan` + `checkRuntimeGate`），
 * 所以两者一致时**只说一句**；不一致时补第二句 —— 那正是这两个字段存在的理由，
 * 而不是把它们摆在响应里没人读。
 */
export function preflightNotes(preflight: DiagnosePreflight): string[] {
  const notes: string[] = [];
  if (!preflight.installOk && preflight.installReason) {
    notes.push(`现在装不了：${preflight.installReason}`);
  }
  if (!preflight.upgradeOk && preflight.upgradeReason && preflight.upgradeReason !== preflight.installReason) {
    notes.push(`升级也不行：${preflight.upgradeReason}`);
  }
  return notes;
}

/**
 * 徽标怎么念 —— 判据要**同时**看结论与症状。
 *
 * `status=ok` 说的是"现在能用"，而 warning 级症状说的是"能跑，但有几处会绊住你"。
 * 只念前半句会得到「没有问题 · 2 项症状」这种自相矛盾的一行 —— 而消灭这种矛盾正是
 * 这一整轮的目的。info 级不算（那是事实说明，不是需要注意的事）。
 */
export function diagnosisBadge(diagnosis: AgentDiagnosis): { label: string; tone: DiagnosisTone } {
  const tone = diagnosisTone(diagnosis.status);
  const noteworthy = diagnosis.issues.filter((issue) => issue.severity !== "info").length;
  if (diagnosis.status === "ok" && noteworthy > 0) {
    return { label: `能用，有 ${noteworthy} 项要留意`, tone: "warn" };
  }
  return { label: diagnosisLabel(diagnosis.status), tone };
}
