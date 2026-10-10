// CLI 工具管理页的判据层（纯函数 + 类型）。
//
// 为什么单独一个文件：这里是**能写反的判据** —— 状态词怎么念、主操作是哪一个、
// 什么情况下**不给**按钮、预检说做不了的时候还算不算"能升级"。留在页面里就只能靠
// 源码断言去钉，而源码断言挡不住"逻辑写反了"。放这里就能被真的调用、真的断言
// （与 `lib/cli-diagnosis.ts`、`features/tasks/task-model.ts` 同一个理由）。
//
// ⚠️ 所有**内容**（症状摘要、证据、修复动作的 label/detail）都由服务端下发，这里只做
// "怎么念"与"哪个动作该亮"，绝不重写判定、也不维护 id→文案的映射 —— 那种映射必然与
// 服务端漂移，而漂移的表现是"按钮上写着一件事、点下去做的是另一件"。
//
// 这一层同时修掉一处既有缺陷：**升级按钮原先不看预检**（`CliToolsPage.tsx:603-618`
// 只读了 `autoUpdatable`）。服务端的预检与真实失败是同一句话（`agent_diagnose.go:856`
// 调的 `resolveAgentInstallPlan` + `checkRuntimeGate` 就是点下去真的会走的那两个），
// 所以"预检说升级不行"与"升级按钮亮着"同屏出现，按钮是必失败的那个。

import type { AgentCatalogEntry } from "./agent-registry";
import type { AgentDiagnosis, DiagnosePreflight, DiagnoseRemedy } from "./cli-diagnosis";
import { diagnosisEmptyText, isConclusiveDiagnosis, offeredRemedies, preflightNote, skippedDiagnosisText } from "./cli-diagnosis";

// ── 服务端下发的两行读数（原样搬进来，字段名与 Go 侧一一对应）────────────────

export type RunnerAgentItem = {
  id: string;
  installed: boolean;
  version?: string;
  binaryPath?: string;
  installKindUsed?: string;
  ready: boolean;
  reason?: string;
  installSupported: boolean;
  installBlockedReason?: string;
  /** 升级也要先授权（平台装的工具升级会在目标环境里重跑 npm）。
   *  与"不能装"分开：那一档是"装不了"，这一档是"装完了但升不了"。 */
  upgradeNeedsGrant?: boolean;
  updateSupported: boolean;
  autoUpdatable: boolean;
  operation?: string;
};

export type RuntimeStatus = {
  id: string;
  installed: boolean;
  version: string;
  npmVersion: string;
  npmPath?: string;
  origin: "system" | "managed" | "none";
  managedPath?: string;
  /** 逐工具算好的"当前运行时够不够用"。页面不许自己再判一遍。 */
  meetsMinimumFor: string[] | null;
  installSupported: boolean;
  installBlockedReason?: string;
  latestVersion?: string;
  updateAvailable: boolean;
};

/**
 * 一次"最新版本是多少"的读数。
 *
 * **四态，缺一不可**：还没开始 / 正在读 / 读到了 / 读失败。
 * 少一档的后果是本项目反复踩的那条 —— 把"读不到"渲染成"已是最新"。
 */
export type LatestRead =
  | { state: "loading" }
  | { state: "ready"; latest: string; updateAvailable: boolean }
  | { state: "error"; error: string };

export const latestLoading: LatestRead = { state: "loading" };

// ── 卡片 ───────────────────────────────────────────────────────────────────

/** 卡片右上角的状态图标。观感只有这六档，变体一律走 data-*。 */
export type CardIcon = "loading" | "ok" | "update" | "alert" | "off" | "unknown";

/** 卡片上那个主动作。没有就是"此刻不该给按钮"。它永远回答"我现在需要做什么"。
 *
 *  ⚠️ 「登录」**不是** `CardAction`：它是"能做的事"，不是"要做的事"，占不住主操作位
 *  ——它由 `ToolCard.canLogin` 单独说，页面渲染成主按钮旁边那颗次要按钮。 */
export type CardAction =
  | { kind: "install"; label: string }
  | { kind: "update"; label: string }
  | { kind: "repair"; label: string; remedies: DiagnoseRemedy[] };

export type ToolCard = {
  id: string;
  name: string;
  commandName: string;
  /** 左上角那块牌子上的两个字与底色档 —— 都由**厂商与名字**决定，不看工具 ID。 */
  mark: string;
  tint: string;
  /** 左上角优先渲染的官方产品图标；null = 没有它，界面回落到 `mark` 两字母牌。 */
  logo: AgentLogoKey | null;
  installed: boolean;

  icon: CardIcon;

  /** 「当前版本」那一行。
   *  `loading` = 还在读这一行（页面先出现、读数后填的那一档；样式早就有 `[data-tone="loading"]`
   * 这条规则，只是在这一轮之前**没有任何代码路径会产生它**）。 */
  currentText: string;
  currentTone: "plain" | "none" | "loading";
  /** 「最新版本」那一行。 */
  latestText: string;
  latestTone: "plain" | "new" | "none" | "loading";

  /** 一句话状态（判断），不是素材。 */
  statusTone: "ok" | "warn" | "bad" | "unknown" | "muted";
  statusText: string;

  /** 做不了的时候要说清的那一句（"为什么点不了"）。 */
  note?: string;
  /**
   * **升级受阻的原因**，只有 `updateDecision` 给过 note 时才有值。
   *
   * 与 `note` 分开是必须的：卡片上的 `note` 会先被诊断相关的分支（④ 修不了 / ⑤ 没查成）
   * 占用，而 `update.available` 在那些分支里已经是 true 了 —— 汇总层若拿 `note` 当
   * "升不了的原因"，横幅就会说出「X 有新版本 v2，但现在升不了：这不代表它没问题 ——
   * 只是这一次没查成。」这种把诊断当升级原因的话（2026-09-29 由独立复查实测出来）。
   */
  updateNote?: string;
  /**
   * 那一句的观感。
   *
   * `warn` 只给"有一件事你**做不到**"（预检挡了升级 / 要手动执行 / 要先去授权）；
   * 纯陈述（"装好就能用""正在处理""这不代表它没问题"）一律 `muted`。
   * 全都染成琥珀的后果是：真正要留意的那些被淹没在同样的颜色里。
   */
  noteTone: "warn" | "muted";

  primary?: CardAction;
  /**
   * 该不该在这张卡上给「登录」入口。
   *
   * 判据两条都由**服务端**给：工具目录说它支持平台内登录（`entry.supportsLogin`），
   * 这台机器上它真的装着（没装时点进去只会在服务端 404，点亮一个必失败的按钮比不亮更坏）。
   *
   * ⚠️ 它**不进 `primary`**：登录是"能做的事"不是"要做的事"，与主操作位那颗（安装 /
   * 更新 / 修好它）并列显示，且不参与 `bannerFor` 的任何一条分支。
   */
  canLogin: boolean;
  /**
   * 该不该在这张卡上给「详情」入口。
   *
   * 顶部横幅是打开抽屉的主入口，但它只会指向**一张**卡（`bannerFor` 用 `find` 取第一张
   * 命中的）。于是有两类卡的下一步根本点不开，而卡片自己的文案正是让用户去抽屉里看证据：
   *   - ④ 修不了那一档（`note` 写着"按「详情」里的证据手动处理"），当它不是横幅指的那张时；
   *   - ⑤ 这次没检查成功那一档：`statusTone` 是 `unknown`，不进 `bannerFor` 的任何分支，
   *     连"唯一"的那张横幅都不会为它出现 —— 它的原因（`diagnosisEmptyText`）只在抽屉里。
   *
   * 判据就是"这张卡的下一步在抽屉里"。**不是**"每张卡都给一颗详情"：已是最新的健康卡
   * 不给，那正是删掉旧按钮、避免卡底留下空动作条的原因。
   */
  canOpenDetails: boolean;
  /** 抽屉里要说清的两句：这次没查 / 没详查的原因。 */
  detailNotes: string[];

  // 供上层做汇总用的读数（卡片自己不渲染它们）。
  updateAvailable: boolean;
  canUpdate: boolean;
  /**
   * 这台工具上正有长任务在跑（装 / 升 / 修）。
   *
   * 汇总层必须知道这一条：正在处理的工具**没有升级动作可给**（服务端会 409），
   * 于是它天然落进"有新版本但升不了"那一档 —— 若不排除，顶部横幅会把它说成
   * "升级前要先处理一个问题"，而正确的下一步只是"等它跑完"（卡片自己已经在说
   * 「正在处理…」了）。
   */
  running: boolean;
};

export type ToolCardInput = {
  entry: AgentCatalogEntry;
  /** 服务端在这一行里给这个工具的状态；undefined = 没给。 */
  item?: RunnerAgentItem;
  runtime: RuntimeStatus | null;
  latest: LatestRead;
  diagnosis?: AgentDiagnosis;
  diagnosisError?: string;
  /** 这一轮批量诊断**没有详查**它（两种成因，见 cli-diagnosis 的 skippedDiagnosisText）。 */
  skipped: boolean;
  /** 批量诊断那条通道当时通不通（决定上一条怎么念）。 */
  channelOk: boolean;
  remoteInstallAllowed: boolean;
  /**
   * 这台机器上的读数**还在路上**。
   *
   * ⚠️ 它与"服务端没给这一行"（`item === undefined`）是**两件完全不同的事**：
   * 前者是"我还在读"，后者是"读到了、但没有它"。合并的后果是本项目最忌讳的那一类 ——
   * 把"正在读"写成"读不到"（渲染成「状态未知」+「拿不到它的状态」）。
   *
   * 它同时负责**切换执行环境时不许串味**：换环境那一瞬间 `items` 里还是上一个环境的
   * 读数，这一档把整张卡盖成"正在读"，旧读数一个字都不上屏。
   */
  reading?: boolean;
};

/**
 * 能不能给这个工具安装：三件事都由**服务端算好**。
 *
 * 只读 `runtime.installed` 等于把"有没有运行时"重判了一遍 —— Node 16 满足"装了"但
 * 不满足工具要求，npm 缺失时也满足"装了"，两种情况界面都会给出一个点了必失败的按钮
 * （服务端的 checkRuntimeGate 会拒）。
 */
export function canInstallTool(input: Pick<ToolCardInput, "item" | "runtime" | "entry">): boolean {
  return Boolean(input.item?.installSupported)
    && Boolean(input.runtime?.npmVersion)
    && (input.runtime?.meetsMinimumFor?.includes(input.entry.id) ?? false);
}

/** 该工具的安装被挡在哪一档。四档说法各不相同，**不能**用一句灰字交代。 */
export function installBlockNote(input: Pick<ToolCardInput, "item" | "runtime" | "entry" | "remoteInstallAllowed">): string {
  const { item, runtime, entry } = input;
  if (item && !item.installSupported) {
    // ① 该环境不提供该工具（服务端下发的理由）。
    return item.installBlockedReason || "这个执行环境不提供该工具。";
  }
  if (!runtime?.installed) {
    // ② 前置运行时缺失。
    return runtime?.installBlockedReason || "需要先安装 Node.js 运行时。";
  }
  if (!runtime?.npmVersion) {
    // ③ 运行时装了但随包 npm 不可用（分发异常，预期会发生）。
    return "运行时装好了，但随包的 npm 不可用 —— 装不了 CLI，请重装 Node.js 运行时。";
  }
  if (!(runtime?.meetsMinimumFor?.includes(entry.id) ?? false)) {
    // ④ 运行时版本过低。
    return `当前 Node ${runtime.version} 低于 ${entry.name} 要求的 Node ${entry.minRuntimeVersion}，请先升级运行时。`;
  }
  if (!input.remoteInstallAllowed) {
    return "尚未授权在这台主机上安装。";
  }
  return "";
}

/** 「当前版本」那一行怎么念。**没装**与**版本未知**是两件事。 */
export function currentVersionLine(item: RunnerAgentItem | undefined): { text: string; tone: "plain" | "none" } {
  if (!item) return { text: "状态未知", tone: "none" };
  if (!item.installed) return { text: "未安装", tone: "none" };
  return { text: item.version || "已安装", tone: "plain" };
}

/**
 * 「最新版本」那一行怎么念 —— **四态各说各的**。
 *
 * 读失败时**绝不能**显示"已是最新"：那是把读不到写成没有（本项目反复复发的那一族）。
 * 未安装时即使读到版本也不加"新"的观感（"没装"与"有新版"不是一回事）。
 */
export function latestVersionLine(
  latest: LatestRead,
  installed: boolean,
): { text: string; tone: "plain" | "new" | "none" | "loading" } {
  if (latest.state === "loading") return { text: "读取中…", tone: "loading" };
  if (latest.state === "error") return { text: "读不到", tone: "none" };
  if (!latest.latest) return { text: "—", tone: "none" };
  if (installed && latest.updateAvailable) return { text: latest.latest, tone: "new" };
  return { text: latest.latest, tone: "plain" };
}

/** 「运行依赖」那一行的结论：读数（`text`/`tone`）+ 那颗按钮（`action`），见 `runtimeDepLine`。 */
export type RuntimeDepLine = {
  text: string;
  tone: "update" | "ok" | "unknown";
  /**
   * 现在该点的那颗按钮。undefined = 此刻不给（原因已经写在 `text` 里）。
   *
   * `kind` 与页面的 `PendingAction` 同名同值，且页面是**照着它发请求**的
   * （`setPending({ kind: action.kind })`）—— 不是页面自己写死一个 kind。这样"按钮上写着
   * 一件事、点下去做的是另一件"就成了一条类型上的约束：将来依赖条长出第二种动作时，
   * 忘了在页面接上会编译不过，而不是静默发成 install-runtime（见文件头那条纪律）。
   */
  action?: { kind: "install-runtime"; label: string };
};

/**
 * 「没授权，所以这颗按钮现在不给」那一句缘由 —— **依赖区里只有这一处措辞**。
 *
 * 依赖区里有两处会因为这个原因收回一颗按钮（升到新版本 / 重装运行时）。两处各写一句话，
 * 必然漂移成"同一件事两种说法"，而这一页的存在意义正是"别再让用户从噪音里挑信息"。
 *
 * ⚠️ 管不到卡片那一边：卡片上那句「尚未授权在 … 上安装」是**服务端**算好下发的
 * （`runner_agents.go` 的 `installBlockedReason`），与本句同义不同源 —— 那是有意的
 * （服务端的理由带着具体的 runner 名），别拿同义词去统一它们。
 */
export const grantNeededReason = "需先授权在这台主机上安装";

/**
 * 「运行依赖」那一行怎么念、给不给升级按钮 —— **只有这一处判断**。
 *
 * 与卡片那边 `updateDecision` 同构（同一种判断只能有一份）。这一层同时是两处既有缺陷的
 * 修复，两处的病根是同一个：**文案与动作各判各的**，于是同屏说出"可以升级"却给不出下一步。
 *
 * 一、读不到最新版本的纪律（与 latestVersionLine 同族）：latestVersion 是服务端的
 *     omitempty 字段，只有真取到 Node 版本索引时才有值（runtime_install.go：
 *     `if s.runtimes != nil { if entries, err := fetchNodeVersionIndex…}`），
 *     拿不到时 updateAvailable 也随之恒为 false。只按 updateAvailable 二分，就会给一个
 *     **根本没查成**的环境亮绿灯写"已是最新" —— 那正是这一族反复复发的那件事。
 *
 * 二、升级按钮的**三道闸门**（2026-10-09 修）。按钮此前只由页面自己数条件：
 *     `installSupported && updateAvailable && remoteInstallAllowed`，而那句读数
 *     只看 `updateAvailable`。两道判据一错位，"可升级到 24.21.0"与"没有按钮"必然同屏，
 *     且那一行一个字都不解释 —— 真机实测（WSL 未授权）就是用户报的那个样子：
 *     读数说能升，按钮没了，唯一的原因写在下面另一块讲「安装」的提示里。
 *      ① `installSupported`：这个环境装不了运行时（跨端缺 tar/gzip、平台无官方包）。
 *         服务端给了 `installBlockedReason`，**照原样念出来**，不编新话。
 *      ② `remoteInstallAllowed`：这台主机还没授权平台安装。原因是"先授权"，
 *         入口在那一行下面**单独一处**（`cli-tools-dep-grant`）—— 这里只说破，
 *         不再摆第二个同义按钮（页面在别处批评过"两个入口做同一件事"）。
 *      ③ 都过了才给按钮。
 *
 * ⚠️ 升不了时**不给按钮**是既有规矩（给一个点了必失败的按钮比不给更坏：未授权时
 * 服务端会以 403 拒掉），改的是"不给就得说清为什么"这一半。
 *
 * 前提：**运行时已装**（`runtime.installed`）。没装是另一条分支（那条分支说的是"还没装"），
 * 调用方要先判它，别把这个函数的"已是最新"当成对未装状态的回答。
 */
export function runtimeDepLine(runtime: RuntimeStatus, remoteInstallAllowed: boolean): RuntimeDepLine {
  if (!runtime.updateAvailable) {
    if (!runtime.latestVersion) return { text: "读不到最新版本", tone: "unknown" };
    return { text: "已是最新", tone: "ok" };
  }
  const available = `可升级到 ${runtime.latestVersion}`;
  if (!runtime.installSupported) {
    return { text: `${available}（${runtime.installBlockedReason || "这个环境装不了 Node.js 运行时"}）`, tone: "update" };
  }
  if (!remoteInstallAllowed) {
    return { text: `${available}（${grantNeededReason}）`, tone: "update" };
  }
  return { text: available, tone: "update", action: { kind: "install-runtime", label: `升级到 ${runtime.latestVersion}` } };
}

/**
 * 卡片左上角那块牌子：两个字母 + 一个底色档。
 *
 * ⚠️ 底色档按**厂商**（服务端目录里的 `vendor`）分，**不按工具 ID** —— 本项目已经为
 * "把工具身份写成 `id === "codex" ? … : …`"付过代价：那种写法的默认分支永远落在某一个
 * 工具上，新增工具时界面会把它静默标成错的那个。认不出的厂商落到中性档，**不回落**到
 * 某个已知厂商 —— 那与"把未知工具显示成 Claude Code"是同一个错。
 */
export function cardTint(vendor: string): "anthropic" | "openai" | "other" {
  const key = (vendor || "").toLowerCase();
  if (key.includes("anthropic")) return "anthropic";
  if (key.includes("openai")) return "openai";
  return "other";
}

/** 牌子上那两个字母：取名字里前两个词的词首；只有一个词就取前两个字符。 */
export function cardMark(name: string): string {
  const words = (name || "").split(/\s+/).filter(Boolean);
  if (words.length >= 2) return (words[0][0] + words[1][0]).toUpperCase();
  return (name || "").slice(0, 2).toUpperCase();
}

// ── 官方产品图标 ───────────────────────────────────────────────────────────
// 卡片左上角优先渲染官方图标（真实 SVG 资产在 `src/assets/agent-*.svg`，文件头写有来源）。
//
// ⚠️ 按**工具 ID** 白名单，不按厂商 —— 图标是**产品**的身份，不是厂商的：Anthropic
// 名下若哪天多出第二个工具，按厂商匹配会把 Claude 的星芒盖到它头上，那与"把未知工具
// 显示成 Claude Code"是同一个错（与上面 `cardTint` 按 vendor 分档不冲突：底色档错了
// 只是难看，图标错了是冒充身份）。
// ⚠️ 白名单之外一律 null：界面回落到两字母牌，**绝不**回落到某个已知图标。

export type AgentLogoKey = "claude" | "openai" | "codebuddy";

const AGENT_LOGOS: Readonly<Record<string, AgentLogoKey>> = {
  "claude-code": "claude",
  "codex": "openai",
  "codebuddy": "codebuddy",
};

export function agentLogoKey(agentID: string): AgentLogoKey | null {
  return AGENT_LOGOS[agentID] ?? null;
}

/** 第一条**要留意的**症状（info 不算：那是事实说明，不是需要注意的事）。 */
function firstNoteworthyIssue(diagnosis: AgentDiagnosis | undefined): { summary: string; tone: "warn" | "bad" } | null {
  if (!diagnosis) return null;
  const issue = diagnosis.issues.find((candidate) => candidate.severity !== "info");
  if (!issue) return null;
  return { summary: issue.summary, tone: issue.severity === "blocker" ? "bad" : "warn" };
}

/** 这份诊断有没有"能自动修"的动作。 */
export function repairableRemedies(diagnosis: AgentDiagnosis | undefined): DiagnoseRemedy[] {
  if (!diagnosis) return [];
  return offeredRemedies(diagnosis);
}

/**
 * 「能不能升级、升不了为什么」——**只有这一处判断**。
 *
 * 两个页面分支都要它（"有症状"与"诊断没查成"两条路都还得决定更新按钮亮不亮），
 * 所以它必须是一个函数而不是各写一遍的 if 链。
 *
 * 四道闸门，顺序就是优先级：
 *   ① 服务端的 `autoUpdatable`（跨端 runner 不支持应用内升级）→ 给手动命令；
 *   ② `upgradeNeedsGrant`（还没授权在这台主机上重跑 npm）→ 给授权说明；
 *   ③ **预检**（`preflight.upgradeOk`）—— 这一道是一个既有缺陷的修复：原先按钮的渲染条件
 *      只看了 ①②，于是"预检说做不了"与"升级按钮亮着"同屏，而按钮是必失败的那个；
 *   ④ 没有预检时**不拦**：预检只在本机诊断里下发（跨端那份带 omitempty），
 *      拦的话会把"没查过"当成"不行"，反而挡住本来能升级的工具。
 */
function updateDecision(input: {
  entry: AgentCatalogEntry;
  item: RunnerAgentItem;
  latest: LatestRead;
  preflight?: DiagnosePreflight;
}): { available: boolean; primary?: CardAction; note?: string } {
  const { entry, item, latest, preflight } = input;
  if (!item.installed || latest.state !== "ready" || !latest.updateAvailable) {
    return { available: false };
  }
  if (!item.autoUpdatable) {
    return { available: true, note: `需在目标环境手动执行 ${entry.commandName} update。` };
  }
  if (item.upgradeNeedsGrant) {
    return { available: true, note: "升级需要先在此主机授权（平台装的工具升级会在目标环境里重跑 npm）。" };
  }
  if (preflight && !preflight.upgradeOk) {
    return { available: true, note: preflightNote(preflight) || "现在还不能升级。" };
  }
  // 不携带"升到哪个版本"：确认框只写工具名，升级完成后由 toast 报"旧 → 新"
  // （服务端回的 previousVersion / currentVersion）。这里原来穿了一个 `to`，一路传到
  // pending 却**没有任何消费方** —— 本项目的处置是删掉而不是留着当摆设
  // （对照上面 tally/tallyText 的注释，2026-09-29 复查）。
  return { available: true, primary: { kind: "update", label: "更新" } };
}

/**
 * 把一张工具卡的全部判据算出来。页面只渲染它的结果。
 *
 * 分支的顺序是**有意的**，每一步的注释写了为什么：
 *   拿不到状态 → 有任务在跑 → 没装 → 有要留意的事 → 没查成 → 更新 → 读不到最新版本 → 已是最新。
 */
export function buildToolCard(input: ToolCardInput): ToolCard {
  const { entry, item, runtime, latest, diagnosis, diagnosisError, skipped, channelOk, remoteInstallAllowed } = input;

  const installable = canInstallTool(input);
  const installBlocked = installBlockNote(input);
  const noteworthy = firstNoteworthyIssue(diagnosis);
  const remedies = repairableRemedies(diagnosis);
  const installed = Boolean(item?.installed);
  const running = item?.operation === "running";
  const current = currentVersionLine(item);
  const latestLine = latestVersionLine(latest, installed);
  const update = item ? updateDecision({ entry, item, latest, preflight: diagnosis?.preflight }) : { available: false };

  const card: ToolCard = {
    id: entry.id,
    name: entry.name,
    commandName: entry.commandName,
    mark: cardMark(entry.name),
    tint: cardTint(entry.vendor),
    logo: agentLogoKey(entry.id),
    installed,
    icon: "off",
    currentText: current.text,
    currentTone: current.tone,
    latestText: latestLine.text,
    latestTone: latestLine.tone,
    statusTone: "muted",
    statusText: "",
    noteTone: "muted",
    // `reading` 也要挡：那时 `item` 可能还是上一个执行环境留下的（loadView 不清旧 view），
    // 照它亮出「登录」会是在一台还没读到的机器上按上一台的读数给动作。
    canLogin: Boolean(entry.supportsLogin) && installed && !running && !input.reading,
    // 默认不给：只有"下一步就在抽屉里"的那两档会把它打开（见 canOpenDetails 的注释）。
    canOpenDetails: false,
    detailNotes: [],
    updateAvailable: update.available,
    canUpdate: update.primary?.kind === "update",
    // 在基座上就带上：后面每个过早 return 的分支（④ 修不了、⑤ 没查成…）都会连它一起带走，
    // 而汇总层要的正是"升不了的原因"，不是那张卡此刻在说的别的事。
    updateNote: update.note,
    running,
  };

  // ⓪ 读数还在路上。**必须排在最前面**：这一档与下面那条「服务端没给这一行」是两件事，
  //    而且它还要负责盖掉上一轮/上一个执行环境留下来的 `item`（否则换环境时旧读数会串味）。
  //    不给任何动作：能不能装、能不能升、要不要留意，此刻一条都还没读到。
  if (input.reading) {
    card.icon = "loading";
    card.statusTone = "muted";
    card.statusText = "正在读取…";
    card.currentTone = "loading";
    card.currentText = "读取中…";
    card.latestTone = "loading";
    card.latestText = "读取中…";
    return card;
  }

  // ① 服务端没给这一行：**不是**"未安装"。说成未安装就是把"没有状态"写成"没有"。
  if (item === undefined) {
    card.icon = "unknown";
    card.statusTone = "unknown";
    card.statusText = "拿不到它的状态。";
    card.note = "服务端这次没有回这个工具的状态行（可能是该环境不认识它）。";
    card.detailNotes.push("这次没有拿到这个工具的状态。");
    return card;
  }

  // ② 有长任务在跑：不给任何操作入口（服务端会返回 409，界面不该亮出按钮）。
  if (running) {
    card.icon = "loading";
    card.statusTone = "muted";
    card.statusText = "正在处理…";
    card.note = "该工具上已有任务在进行，完成前不能再操作。";
    return card;
  }

  // ③ 没装：给"装"或"为什么装不了"。
  if (!installed) {
    card.icon = installable ? "off" : "unknown";
    card.statusTone = "muted";
    card.statusText = "还没装。";
    card.note = installable ? "装好就能用。" : installBlocked;
    // "装不了"是 amber（有一件事你做不到）；"装好就能用"是纯陈述。
    card.noteTone = installable ? "muted" : "warn";
    if (installable) card.primary = { kind: "install", label: "安装" };
    return card;
  }

  // ④ 装好了：先看诊断有没有要留意的事 —— 那件事比"能不能升级"要紧。
  if (noteworthy) {
    card.icon = "alert";
    card.statusTone = noteworthy.tone;
    card.statusText = noteworthy.summary;
    if (remedies.length > 0) {
      card.primary = { kind: "repair", label: "修好它", remedies };
    } else {
      // 修不了的就**不给可修动作**（给一个点了没反应的按钮比不给更坏），改成让用户去看证据。
      card.note = "这个问题平台不能自动修 —— 按「详情」里的证据在目标环境手动处理。";
      // 这句话必须点得开：横幅只指向第一张命中的问题卡，这张若不是它（或多张问题卡里的
      // 后几张），没有这颗入口就永远够不着抽屉 —— 而证据只在抽屉里。
      card.canOpenDetails = true;
    }
    return card;
  }

  // ⑤ 报告**没有结论**：它也是零症状，但它绝不是"没有问题"。
  //    这一档必须单独说，否则下面那句「已是最新，不用管它」会把一次没查成
  //    伪装成一切正常 —— 本项目反复复发的那条。
  if (diagnosis && !isConclusiveDiagnosis(diagnosis)) {
    card.icon = "unknown";
    card.statusTone = "unknown";
    card.statusText = "这次没检查成功。";
    card.note = "这不代表它没问题 —— 只是这一次没查成。";
    card.detailNotes.push(diagnosisEmptyText(diagnosis));
    // 这一档 statusTone 是 unknown，不进 bannerFor 的任何分支 → 连横幅都不会为它出现。
    // "为什么没查成"只在抽屉里，所以必须给它一颗自己的入口。
    card.canOpenDetails = true;
    // 更新是另一件事、另一个读数，该给还得给。
    if (update.primary) card.primary = update.primary;
    return card;
  }
  if (diagnosisError) {
    card.detailNotes.push(`这次检测没完成：${diagnosisError}`);
  } else if (!diagnosis && skipped) {
    card.detailNotes.push(skippedDiagnosisText(channelOk));
  }

  // ⑥ 更新这一档：能升就给按钮，升不了就把原因说清楚（判据在 updateDecision 里）。
  if (update.available) {
    card.icon = update.primary ? "update" : "alert";
    card.statusTone = "warn";
    card.statusText = `可以更新到 ${card.latestText}。`;
    if (update.primary) card.primary = update.primary;
    else {
      card.note = update.note;
      card.noteTone = "warn";
    }
    return card;
  }

  // ⑦ 读不到最新版本：**不能说"已是最新"**。
  if (latest.state === "error") {
    card.icon = "unknown";
    card.statusTone = "unknown";
    card.statusText = `读不到最新版本（${latest.error}）。`;
    card.note = "这不代表它是最新的 —— 只是这次没查到。";
    return card;
  }
  if (latest.state === "loading") {
    card.icon = "loading";
    card.statusTone = "muted";
    card.statusText = "正在读最新版本…";
    return card;
  }

  // ⑧ 已是最新：主操作位上一个动作都不给。需要登录的工具另有「登录」那颗次要按钮
  //    （`canLogin`）—— 那是"能做的事"，它占着主操作位只会让"这一列按钮"失去含义。
  card.icon = "ok";
  card.statusTone = "ok";
  card.statusText = "已是最新，不用管它。";
  return card;
}

// ── 页面级汇总 ─────────────────────────────────────────────────────────────

export type Banner = { tone: "warn" | "bad"; text: string; agentID: string; actionLabel?: string };

// ⚠️ 这里原来还有一对 `tally` / `tallyText`（"3 个工具里 2 个已装好，1 个需要处理"那句）。
// 2026-09-23 用户把页脚整块删掉之后，它们就**只剩测试在读**了 —— 本项目的处置是删掉而不是
// 留着当摆设（"算了但没人读"本身就是缺陷）。那句话也不承载新信息：有事时顶部横幅在说，
// 没事时每张卡自己写着「已是最新」。

/**
 * 顶部那条"有事"的横幅 —— **只在有事时存在**。
 *
 * 三条分支是有序的：① 有新版本但升不了；② 有工具用不了；③ 有工具要留意。
 * 一条都不满足就返回 undefined（整条不渲染）。
 *
 * ⚠️ ③ 覆盖**两种**要留意：能修的（给「修好它」）与平台修不了的（如实测超时、包目录
 * 扫不动 —— 服务端不下发 remedy）。后者也**必须**上横幅：这类卡的下一步是去抽屉里看
 * 证据。但横幅只指向**第一张**命中的问题卡，所以这种不可修的卡还会自带一颗「详情」
 * （`ToolCard.canOpenDetails`）兜底 —— 否则多张问题卡里的后几张、以及不产横幅的
 * "没查成"档，都会点不开抽屉。
 *
 * ⚠️ `actionLabel` **只在卡片真有 repair 动作时才给**（2026-09-29 修正）。此前这三条
 * 分支一律写死"修好它"，而"有新版本但升不了"这一档有四种成因（跨端只能手动升 /
 * 要先授权 / 预检挡住 / 该工具正有任务在跑），**四种都没有可修的动作** —— 于是按钮
 * 点下去只是打开详情抽屉，和旁边那颗「看看是什么问题」做的是同一件事（页面自己
 * 在别处批评过"两个入口做同一件事"），文案还断言了一个不存在的"问题"。
 * 现在：没有动作就不给按钮，改成照卡片自己那句 `note`（成因的唯一来源）说清下一步。
 */
export function bannerFor(cards: ToolCard[]): Banner | undefined {
  // 正在跑的工具不参与：它没有升级动作可用（服务端会 409），但"等它跑完"不是
  // "有个问题要处理" —— 卡片自己已经在说「正在处理…」。
  // 最新版本那一趟还没回来的也排除：那时 latestText 是"读取中…"，横幅会说出
  // 「X 有新版本 读取中…」这种半句话（loadView 重读时不清旧 view，于是这里仍带着
  // 上一轮的 updateAvailable —— 旧文案同样会说出这句，属没修完的那半边）。
  const blockedUpdate = cards.find(
    (card) => card.installed && card.updateAvailable && !card.canUpdate && !card.running && card.latestTone !== "loading",
  );
  if (blockedUpdate) {
    const repairable = blockedUpdate.primary?.kind === "repair";
    return {
      tone: "warn",
      text: repairable
        ? `${blockedUpdate.name} 有新版本 ${blockedUpdate.latestText}，但升级前要先处理一个问题。`
        // 原因只能取 updateNote：卡片自己的 note 在别的分支里说的是诊断（见 updateNote 的注释）。
        : `${blockedUpdate.name} 有新版本 ${blockedUpdate.latestText}，但现在升不了：${blockedUpdate.updateNote || "原因见详情"}`,
      agentID: blockedUpdate.id,
      ...(repairable ? { actionLabel: "修好它" } : {}),
    };
  }
  const unusable = cards.find((card) => card.statusTone === "bad");
  if (unusable) {
    return {
      tone: "bad",
      text: `${unusable.name} 现在用不了。`,
      agentID: unusable.id,
      // 修不了就不给按钮（卡片那边同一条规矩：给一个点了没反应的按钮比不给更坏）。
      ...(unusable.primary?.kind === "repair" ? { actionLabel: "修好它" } : {}),
    };
  }
  // ③ 有工具要留意。两种都要上横幅，因为**横幅是问题卡打开抽屉的唯一入口**：
  //    能修的给「修好它」；不能修的（服务端没下发 remedy，如实测超时、包目录扫不动）
  //    卡片上那句"按详情里的证据手动处理"要靠它或卡片自带的「详情」才点得开。
  //    带别的动作（更新 / 安装）的**不算**："可以更新到 v2"不是"需要处理一下"。
  const needsAttention = cards.find(
    (card) => card.statusTone === "warn" && (card.primary?.kind === "repair" || !card.primary),
  );
  if (needsAttention) {
    const repairable = needsAttention.primary?.kind === "repair";
    return {
      tone: "warn",
      text: `${needsAttention.name} 需要处理一下。`,
      agentID: needsAttention.id,
      // 与 ①② 同一条规矩：没有可修动作就不给按钮（给一个点了没反应的按钮比不给更坏），
      // 只留「看看是什么问题」那颗链接去开抽屉。
      ...(repairable ? { actionLabel: "修好它" } : {}),
    };
  }
  return undefined;
}

/**
 * 执行环境那个标签怎么念。
 *
 * 服务端的 `name` 是内部叫法（`Windows Local Runner` / `WSL Local Runner (Ubuntu)`），
 * 而界面这一处只需要回答一件事：**这是哪台机器**。本机那两个环境给固定短名
 * （`Windows` / `WSL`），其余（远端 SSH）仍用 `name` —— 那时它就是主机名，是用户唯一
 * 认得出的东西。
 *
 * ⚠️ 不拼 ``（windows）`` 这种后缀：标签里出现两个同义的环境词纯属噪音，而这一页的
 * 存在意义正是"别再让用户从噪音里挑信息"。
 */
export function runnerLabel(runner: { name?: string; environment?: string } | undefined): string {
  if (!runner) return "";
  const environment = (runner.environment || "").toLowerCase();
  if (environment === "windows") return "Windows";
  if (environment === "wsl") return "WSL";
  return runner.name || runner.environment || "未知环境";
}

// ── 手动安装命令（页面最下面那块，默认折叠）────────────────────────────────

/** 分发方式：与后端 `InstallKindNpmGlobal` 同名同值（agent_catalog.go:20）。 */
const INSTALL_KIND_NPM_GLOBAL = "npm-global";

export type ManualInstallRow = { id: string; name: string; command: string };

/**
 * 手动安装命令清单 —— 平台装不了的时候，用户自己在那台机器上跑的那条命令。
 *
 * 三条判据，都是**有意**的：
 *  1. **只对 npm 全局分发的工具给命令**。将来若有别的分发方式，它**不进这一列**，
 *     也绝不回落成 `npm install -g` —— 那等于教用户做错事（与 `installBlockNote`
 *     里"四档不回落"是同一条纪律）。
 *  2. **命令在这里拼，不在页面里拼**。与 `updateDecision` 里那句
 *     `${entry.commandName} update` 同源：命令只有一处来源，页面只渲染。
 *  3. **列全部目录工具，不按装没装过滤**。这一块是手册，不是状态面板 ——
 *     已装的工具拿同一条命令也能升到最新。它因此不随所选环境变化。
 */
export function buildManualInstallRows(entries: AgentCatalogEntry[]): ManualInstallRow[] {
  return entries
    .filter((entry) => entry.installKind === INSTALL_KIND_NPM_GLOBAL && Boolean(entry.npmPackage))
    .map((entry) => ({
      id: entry.id,
      name: entry.name,
      command: `npm install -g ${entry.npmPackage}@latest`,
    }));
}
