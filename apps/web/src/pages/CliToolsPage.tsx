// Cli管理（路由 `/cli-tools`）。
//
// ⚠️ 界面上的名字只有一处来源：**首页入口的 `title` / `<span>` 与这里的 `<h1>` 写同一个字符串**
// （当前是「Cli管理」）。改名字时三处要一起动 —— 有一条结构断言钉着它们相等，
// 就是防"改了入口忘了标题"那种半截改名。
//
// 页面的职责只有一条：**回答"这台机器上的工具现在能不能用、要不要我做点什么"**，
// 并给出唯一正确的下一步。所以它有三处刻意分开的地方：
//
//  1. **三档状态分开渲染** —— 正在读取 / 无法检测（通道坏了）/ 真的没有。
//     合并任何两档，用户就会去做一件没有用的事（去装一个装不上、或本来就有的东西）。
//  2. **"不可安装"分四档、文案各不相同** —— 未授权 / 运行时缺失 / npm 不可用 / 该环境不提供。
//     这四件事的下一步动作完全不同，用同一句灰字交代等于什么都没说。
//     判据全在 `lib/cli-tools-view.ts`（能写反的东西一律放那儿，页面只渲染）。
//  3. **诊断自身也是三档**（docs/43）—— 检测没查成 / 发现了问题 / 没有问题。
//     把"没查成"并进"没问题"是最坏的一种：用户会以为这台机器一切正常。
//
// 首屏**只留判断 + 唯一的下一步**：卡片上是名字、当前版本、最新版本、一句状态、一个动作
// （需要登录的工具在它旁边多一颗「登录」—— 那是"能做的事"，不是"要做的事"，但它同样
// 是卡片上的下一步，不该藏进抽屉）；
// 路径 / 证据 / 日志 / 「这台机器上的所有位置」这些排查素材收进「详情」抽屉 ——
// 它们不是没用，是没必要站在首屏（删除的判据：不看它，会不会做错下一步）。
// 唯一必须留在一线的是「这次没检查成功」，它不能并进"没有问题"。
//
// 诊断与修复的判据**全部来自服务端**：症状、证据、可用动作（含 label 与说明）都是
// 服务端算好的对象，页面只渲染、不拼命令、也不维护 id→文案的映射 —— 那种映射必然
// 与服务端漂移，而漂移的表现是"按钮上写着一件事、点下去做的是另一件"。

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { useNavigate } from "react-router-dom";
import { AgentLogo } from "../components/AgentLogo";
import { api } from "../lib/api";
import { ExternalLink } from "../components/ExternalLink";
import { agentDisplayName, loadAgentCatalog, useAgentCatalogState } from "../lib/agent-registry";
import { copyToClipboard } from "../lib/clipboard";
import {
  describeRepair,
  diagnoseMoment,
  diagnosisEmptyText,
  diagnosisMetaLine,
  pathFactState,
  preflightNote,
  resolvedIssues,
  resolvedSummary,
  skippedDiagnosisText,
} from "../lib/cli-diagnosis";
import { normalizeDiagnosis, normalizeDiagnostics } from "../lib/cli-diagnosis";
import type { AgentDiagnosis, AgentDiagnosisWire, DiagnoseRemedy, RepairResult, RunnerDiagnosticsViewWire } from "../lib/cli-diagnosis";
import { bannerFor, buildManualInstallRows, buildToolCard, grantNeededReason, latestLoading, runnerLabel, runtimeDepLine } from "../lib/cli-tools-view";
import type { LatestRead, RuntimeStatus, RunnerAgentItem, ToolCard } from "../lib/cli-tools-view";
import type { RunnerInfo } from "../lib/types";
import "./cli-tools.css";

type RunnerAgentsView = {
  runnerId: string;
  environment: string;
  probeOk: boolean;
  probeError?: string;
  runtime: RuntimeStatus | null;
  items: RunnerAgentItem[];
  remoteInstallAllowed: boolean;
};

type RuntimeCatalog = { source: string; versions: { version: string; lts?: string; date?: string }[] };

/**
 * `POST …/check-update` 的响应 —— **只声明页面真的会读的那三个字段**。
 *
 * 服务端还会回 `currentVersion` 与 `autoUpdatable`，但这一页不读它们：
 * 当前版本来自列表接口（那一行是权威的）、"能不能应用内升级"由模型按 `item.autoUpdatable`
 * 判。声明了不读的字段就等于承认"这里有第二份口径"，而它迟早会被人拿去做判断。
 * `error` 非空表示"这次没查到"，不是"没有新版本"。
 */
type CheckUpdateResult = {
  updateAvailable: boolean;
  latestVersion?: string;
  error?: string;
};

/** 待确认的动作。安装、升级与修复都要先确认：它们会改动目标环境上的东西。 */
type PendingAction =
  | { kind: "install-runtime" }
  | { kind: "install-agent"; agentID: string }
  | { kind: "update-agent"; agentID: string }
  | { kind: "repair"; agentID: string; remedies: DiagnoseRemedy[] };

/**
 * 给 URL 挂上 `force=true` —— 服务端据此**绕过读数缓存**重新检测（见 agent_readings.go）。
 *
 * 只有**手动动作**才带它：顶栏「重新检查」、通道坏掉后的重试、以及安装/升级/修复之后的
 * 刷新。进入页面与切换执行环境一律不带 —— 那就是"一段时间内只检测一次"的落点。
 */
const withForce = (path: string, force: boolean) => (force ? `${path}?force=true` : path);

export default function CliToolsPage() {
  const navigate = useNavigate();
  // 目录状态要连 loaded / error 一起拿：只取 entries 的话，"还没读到"与"读失败"
  // 都会渲染成"一个工具都没有" —— 那正是本项目反复禁止的"把读不到写成没有"。
  const catalog = useAgentCatalogState();
  const [runners, setRunners] = useState<RunnerInfo[]>([]);
  const [runnersState, setRunnersState] = useState<"loading" | "ready" | "error">("loading");
  const [runnersError, setRunnersError] = useState("");
  const [selectedRunner, setSelectedRunner] = useState("");
  const [view, setView] = useState<RunnerAgentsView | null>(null);
  const [viewState, setViewState] = useState<"loading" | "ready" | "error">("loading");
  const [viewError, setViewError] = useState("");
  const [busy, setBusy] = useState(false);
  /**
   * 正在跑的那次操作（busy 只说"忙"，说不出在忙什么）。
   *
   * 为什么需要它：装/升级/修复是**数十秒到数分钟级**的操作（docs/42 §4.4），而在这之前
   * 点完「确认」的界面是"弹窗关掉、按钮置灰、页面上什么都不说" —— 用户只能盯着一个
   * 没反应的按钮，等到的往往是超时。docs/42 §1.1 把「长任务进度：要有进行中与结果」
   * 列为待补项，这一档就是它。
   *
   * ⚠️ 必须连**跑在哪台机器上**一起记下来，不能只记 action：顶栏可以切执行环境，而
   * 操作还在原来那台上跑。只记 action 的话，切过去之后新环境的同名卡片会亮起
   * "升级中…"，说的是一台根本没在升级的机器。
   */
  const [running, setRunning] = useState<{ action: PendingAction; runnerID: string; runnerText: string } | null>(null);
  const [pending, setPending] = useState<PendingAction | null>(null);
  // 登录面板状态：loginAgent 非空时打开；loginInfo 承载后端返回的授权链接/指引。
  const [loginAgent, setLoginAgent] = useState<{ agentID: string; name: string } | null>(null);
  const [loginInfo, setLoginInfo] = useState<{ authUrl?: string; userCode?: string; message?: string } | null>(null);
  const [loginBusy, setLoginBusy] = useState(false);
  const [loggedIn, setLoggedIn] = useState(false);
  const [runtimeCatalog, setRuntimeCatalog] = useState<RuntimeCatalog | null>(null);
  /** 打开了哪个工具的「详情」抽屉（存 agentID）。 */
  const [openAgent, setOpenAgent] = useState("");

  /**
   * 「最新版本是多少」这一次读数。**四态**（读取中 / 读到了 / 读不到 / 空版本号），
   * 按 (runner, agent) 索引 —— 只按 agent 索引会把 A 机器上"发现新版本"带到
   * B 机器的同名工具卡片上。
   */
  const [latest, setLatest] = useState<Record<string, LatestRead>>({});

  // 诊断状态。按 (runner, agent) 索引，理由同上。
  const [diagnoses, setDiagnoses] = useState<Record<string, AgentDiagnosis>>({});
  const [diagnoseErrors, setDiagnoseErrors] = useState<Record<string, string>>({});
  const [diagnoseBusy, setDiagnoseBusy] = useState<Record<string, boolean>>({});
  const [skippedDiagnoses, setSkippedDiagnoses] = useState<Record<string, boolean>>({});
  const [diagnoseState, setDiagnoseState] = useState<"idle" | "loading" | "ready" | "error">("idle");
  const [diagnoseError, setDiagnoseError] = useState("");
  /**
   * 批量诊断那一侧读到的**通道状态**。
   *
   * 必须真的消费它：通道坏掉时服务端会把**所有**工具都放进 `skipped`，而 `skipped`
   * 有两种成因（"没什么可疑的"与"根本没查"），说法不同 —— 不消费它，通道坏掉时每个
   * 工具都会盖上一个"没有可疑情况"的章。顶层那份 `view.probeOk` 来自另一个端点，
   * 可能已经过时，不能替代它。
   */
  const [diagnoseChannel, setDiagnoseChannel] = useState<{ ok: boolean; error?: string } | null>(null);
  /**
   * "上次修复解决了什么"（key 同 updateKey）。
   *
   * 存在的理由：修复后换上新诊断只能让人**自己比对**——而"修复完成"这句话本身
   * 证明不了任何事（docs/43 §6.3）。这里把差集算出来说给人听。
   */
  const [resolvedNotes, setResolvedNotes] = useState<Record<string, string>>({});

  // 请求代际：切执行环境时旧响应回来不许盖掉新环境的状态 —— 否则会出现
  // "B 的标签下挂着 A 的状态"，而用户以为是 B 的。
  const viewGeneration = useRef(0);
  const diagnoseGeneration = useRef(0);
  const latestGeneration = useRef(0);
  /**
   * 「这一次要不要绕过服务端的读数缓存」。
   *
   * 用 ref 而不是 state：它要穿过 loadView → viewState 回到 ready → 那个触发 effect
   * 这条链，而 effect 的依赖里加一个 state 会让它多触发一轮（诊断跑两遍 —— 下面那条
   * "触发点只能有一个"的注释警告的正是这个）。effect 读到后立刻复位，只作用于这一次。
   */
  const forceNextReadings = useRef(false);

  // 工具目录与运行器清单都在进入页面时拉一次。目录读取失败不阻断页面：
  // 工具名会退化成 id（如实），而不是消失。
  useEffect(() => {
    void loadAgentCatalog().catch(() => undefined);
    void api<RuntimeCatalog>("/api/runtimes/catalog").then(setRuntimeCatalog).catch(() => setRuntimeCatalog(null));
  }, []);

  const loadRunners = useCallback(async () => {
    setRunnersState("loading");
    try {
      const list = await api<RunnerInfo[]>("/api/runners");
      setRunners(list);
      setRunnersState("ready");
      setSelectedRunner((current) => current || list[0]?.id || "");
    } catch (cause) {
      setRunnersState("error");
      setRunnersError(cause instanceof Error ? cause.message : "无法读取执行环境");
    }
  }, []);

  useEffect(() => {
    void loadRunners();
  }, [loadRunners]);

  const loadView = useCallback(async (runnerID: string, force = false) => {
    if (!runnerID) return;
    const generation = ++viewGeneration.current;
    setViewState("loading");
    try {
      const result = await api<RunnerAgentsView>(withForce(`/api/runners/${encodeURIComponent(runnerID)}/agents`, force));
      if (viewGeneration.current !== generation) return;
      setView(result);
      setViewState("ready");
      setViewError("");
    } catch (cause) {
      if (viewGeneration.current !== generation) return;
      setView(null);
      setViewState("error");
      setViewError(cause instanceof Error ? cause.message : "无法读取该执行环境上的工具状态");
    }
  }, []);

  useEffect(() => {
    void loadView(selectedRunner);
  }, [loadView, selectedRunner]);

  const updateKey = useCallback((agentID: string) => `${selectedRunner}:${agentID}`, [selectedRunner]);

  /**
   * 执行环境怎么念（短名，见 `runnerLabel`）。用它的地方有两处：安装/升级确认框里的
   * 「目标环境」，以及**发起操作时**记下"跑在哪台机器上"。
   *
   * ⚠️ 声明位置必须在 `runPending` 之前：那个回调用它捕获环境名，而 `const` 在同一个
   * 函数体里是有暂时性死区的（只靠"回调是稍后才执行的"过不了 TS 的检查，也确实该按
   * 真实的依赖顺序写）。
   */
  const environmentLabel = useMemo(() => {
    const runner = runners.find((item) => item.id === selectedRunner);
    if (!runner) return selectedRunner;
    return runnerLabel(runner);
  }, [runners, selectedRunner]);

  /**
   * 逐个读"最新版本是多少"。
   *
   * **并发**跑（不是串行）：每个工具各问一次官方 registry，而这一趟的瓶颈是**网络**
   * —— 实测 `npm view` 2.4~3.1s/次、直接查 registry 1.9~3.0s/次（node 启动本身只 0.4s），
   * 三条串起来就是它们的和（约 8s，冷启动时还撞上过一条 14.6s 的，合计 22.7s）。
   * 并发之后用户只需要等**最慢的那一条**，而且先回来的卡先出结果 —— 这正是"读数状态
   * 挂在每张卡自己身上、而不是整页一起变灰"的意义所在。
   *
   * 未安装的工具也查：那时"最新版本"是"你可以装到哪一版"，仍然是有用的信息。
   * 换执行环境时靠 `latestGeneration` 整批作废：旧结果回来了也不许写进去。
   */
  const checkLatestVersions = useCallback(async (runnerID: string, agentIDs: string[], force = false) => {
    if (!runnerID || agentIDs.length === 0) return;
    const generation = ++latestGeneration.current;
    const prefix = `${runnerID}:`;
    setLatest((current) => {
      const next = { ...current };
      for (const id of agentIDs) next[`${prefix}${id}`] = latestLoading;
      return next;
    });
    await Promise.all(agentIDs.map(async (agentID) => {
      let read: LatestRead;
      try {
        const result = await api<CheckUpdateResult>(
          withForce(`/api/runners/${encodeURIComponent(runnerID)}/agents/${encodeURIComponent(agentID)}/check-update`, force),
          { method: "POST" },
        );
        // 200 里带 error 也是"没查到" —— 与服务端抛错一样按"读不到"处理，
        // 绝不能回落成"已是最新"。
        read = result.error
          ? { state: "error", error: result.error }
          : {
            state: "ready",
            latest: result.latestVersion ?? "",
            updateAvailable: Boolean(result.updateAvailable),
          };
      } catch (cause) {
        read = { state: "error", error: cause instanceof Error ? cause.message : "读取失败" };
      }
      if (latestGeneration.current !== generation) return;
      setLatest((current) => ({ ...current, [`${prefix}${agentID}`]: read }));
    }));
  }, []);

  // 批量诊断。**不做进列表接口**：它要跑多次子进程、扫目录、读审计，
  // 与 docs/42 §14.G 里"运行时探测不进热路径"是同一条理由。
  const runDiagnostics = useCallback(async (runnerID: string, force = false) => {
    if (!runnerID) return;
    const generation = ++diagnoseGeneration.current;
    const prefix = `${runnerID}:`;
    setDiagnoseState("loading");
    setDiagnoseError("");
    try {
      const wire = await api<RunnerDiagnosticsViewWire>(withForce(`/api/runners/${encodeURIComponent(runnerID)}/diagnostics`, force));
      // ⚠️ 数组字段在边界上收口（见 normalizeDiagnostics 的注释）：服务端一个 null
      // 会让详情抽屉在 .length 上抛异常，而那个异常的后果是整页白屏。
      const result = normalizeDiagnostics(wire);
      if (diagnoseGeneration.current !== generation) return;
      // 服务端会回它自己的 runnerId。**如实说**：这个值就是请求 URL 里的那个参数
      // （agent_diagnose_http.go: `RunnerID: runnerID`），而服务端也不做别名解析（见
      // resolveProbeTarget），所以这一条在实际链路上**永远不会命中** —— 真正挡住"切了环境
      // 却写回上一份结果"的是上面那道代际校验（diagnoseGeneration）。
      // 留着它有两个理由：服务端字段都该有消费点（cli-tools-page.test.mjs 有一条专门钉这个），
      // 以及一旦服务端将来改成回解析后的真实环境（meta.ID / environment），它就会自动生效。
      if (result.runnerId && result.runnerId !== runnerID) {
        setDiagnoseState("error");
        setDiagnoseError("这次检测的结果属于另一个执行环境，已丢弃；请重新检测。");
        return;
      }
      const nextDiagnoses: Record<string, AgentDiagnosis> = {};
      const skipped: Record<string, boolean> = {};
      for (const item of result.items) nextDiagnoses[`${prefix}${item.agentId}`] = item;
      // skipped 与 items 是**互斥且穷尽**的两档：漏掉那一半会被渲染成"没问题"。
      for (const id of result.skipped) {
        if (!nextDiagnoses[`${prefix}${id}`]) skipped[`${prefix}${id}`] = true;
      }
      setDiagnoses(nextDiagnoses);
      setDiagnoseErrors({});
      setSkippedDiagnoses(skipped);
      setDiagnoseChannel({ ok: result.probeOk, error: result.probeError });
      setDiagnoseState("ready");
    } catch (cause) {
      if (diagnoseGeneration.current !== generation) return;
      // 检测失败**不清空**已有结论：清空会把"这次没查成"变成"没有发现问题"。
      setDiagnoseState("error");
      setDiagnoseError(cause instanceof Error ? cause.message : "无法完成问题检测");
    }
  }, []);

  const catalogKey = useMemo(() => catalog.entries.map((entry) => entry.id).join(","), [catalog.entries]);

  // 进入页面（或换执行环境）后自动跑一次批量诊断 + 读一遍各工具的最新版本。
  // 放在这里而不是列表接口里：它们都是"深查"，只该发生在管理页真的被打开时。
  // ⚠️ 触发点**只能有一个**（就是这里的 viewState 回到 ready）：refresh() 里再调一次
  // 就会每次刷新跑两遍完整诊断，而两遍的结论必然一样。
  //
  // ⚠️ catalogKey 必须在依赖里（"最新版本"那一趟要知道有哪些工具，而目录比 /agents 晚到
  // 是常态），但它一变 effect 就整体重跑 —— 于是诊断也会跟着再跑一遍（多次子进程 + 扫目录，
  // 这一页最贵的动作，2026-09-29 复查）。所以用**读数身份**去重：同一份 view 上只跑一次诊断，
  // 目录晚到时只补"最新版本"那一趟。
  const diagnosedViewRef = useRef<typeof view>(null);
  useEffect(() => {
    if (viewState !== "ready" || !view?.probeOk) return;
    // 手动刷新只把"这一次要绕过缓存"的意愿放在 ref 上，由这里取用并**立刻复位**
    // —— 触发点仍然只有这一个，不会因为多了个 force 就多跑一轮。
    const force = forceNextReadings.current;
    forceNextReadings.current = false;
    const alreadyDiagnosed = !force && diagnosedViewRef.current === view;
    diagnosedViewRef.current = view;
    if (!alreadyDiagnosed) void runDiagnostics(selectedRunner, force);
    if (catalogKey) void checkLatestVersions(selectedRunner, catalogKey.split(","), force);
  }, [runDiagnostics, checkLatestVersions, selectedRunner, viewState, view, catalogKey]);

  /** 单个工具的显式详查（详情抽屉里的「重新检测这个工具」）。 */
  const diagnoseOne = useCallback(async (agentID: string) => {
    const key = updateKey(agentID);
    setDiagnoseBusy((current) => ({ ...current, [key]: true }));
    try {
      const wire = await api<AgentDiagnosisWire>(
        `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(agentID)}/diagnose`,
      );
      const result = normalizeDiagnosis(wire);
      setDiagnoses((current) => ({ ...current, [key]: result }));
      setSkippedDiagnoses((current) => ({ ...current, [key]: false }));
      setDiagnoseErrors((current) => {
        const next = { ...current };
        delete next[key];
        return next;
      });
    } catch (cause) {
      setDiagnoseErrors((current) => ({
        ...current,
        [key]: cause instanceof Error ? cause.message : "检测未完成",
      }));
    } finally {
      setDiagnoseBusy((current) => ({ ...current, [key]: false }));
    }
  }, [selectedRunner, updateKey]);

  const refresh = useCallback(async (force = false) => {
    // ⚠️ 这里**不**显式调 runDiagnostics / checkLatestVersions：上面那个 effect
    // （viewState 回到 ready 时触发）已经是一个触发点，再调一次就是每次刷新都跑**两遍**。
    // force 只是交给它：手动刷新与写操作之后的刷新都要拿到**新**读数，不能吃缓存。
    forceNextReadings.current = force;
    await Promise.all([loadRunners(), loadView(selectedRunner, force)]);
  }, [loadRunners, loadView, selectedRunner]);

  const runPending = useCallback(async () => {
    if (!pending) return;
    const action = pending;
    setPending(null);
    setBusy(true);
    setRunning({ action, runnerID: selectedRunner, runnerText: environmentLabel });
    // 「上次修复已解决 N 项」只对**那一次**修复有效：一发起新动作就先清掉，
    // 否则它会一直挂在那儿，说一件早已被后续变更推翻的事。
    const actionAgentID = "agentID" in action ? action.agentID : "";
    if (actionAgentID) {
      const staleKey = updateKey(actionAgentID);
      setResolvedNotes((current) => ({ ...current, [staleKey]: "" }));
    }
    try {
      if (action.kind === "install-runtime") {
        await api(`/api/runners/${encodeURIComponent(selectedRunner)}/runtime/install`, {
          method: "POST",
          body: JSON.stringify({ version: "lts" }),
        });
        toast.success("Node.js 运行时已安装");
      } else if (action.kind === "install-agent") {
        const installed = await api<{ version?: string }>(`/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(action.agentID)}/install`, {
          method: "POST",
          body: JSON.stringify({ version: "latest" }),
        });
        toast.success(`${agentDisplayName(action.agentID)} 已安装${installed?.version ? `：${installed.version}` : ""}`);
      } else if (action.kind === "update-agent") {
        // 说清**从哪个版本到哪个版本**。服务端本来就把这两个数回给了我们
        // （app.go 的 writeJSON）：只报一句"已升级"的话，用户在卡片上看到的版本号
        // 万一还没刷新，就没法分辨"升成功了"与"根本没升"—— 而这正是他来点这个按钮的原因。
        const result = await api<{ previousVersion?: string; currentVersion?: string }>(
          `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(action.agentID)}/update`,
          { method: "POST" },
        );
        const name = agentDisplayName(action.agentID);
        toast.success(result?.previousVersion && result?.currentVersion
          ? `${name} 已升级：${result.previousVersion} → ${result.currentVersion}`
          : `${name} 已升级`);
      } else {
        const raw = await api<RepairResult>(
          `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(action.agentID)}/repair`,
          { method: "POST", body: JSON.stringify({ remedies: action.remedies.map((remedy) => remedy.id) }) },
        );
        // 服务端回填了**修复后重跑的**诊断 —— 直接换上去，让用户当场看出症状有没有真的消失。
        const key = updateKey(action.agentID);
        // 差集要在**换掉之前**算：换完就只剩新报告了，"哪些症状没了"这个信息也就丢了。
        // （差集自己会挡掉"修复后那份没查成"的情况，见 resolvedIssues 的注释。）
        const result = { ...raw, diagnosis: normalizeDiagnosis(raw.diagnosis) };
        const note = resolvedSummary(resolvedIssues(diagnoses[key], result.diagnosis));
        setResolvedNotes((current) => ({ ...current, [key]: note }));
        setDiagnoses((current) => ({ ...current, [key]: result.diagnosis }));
        // 成败**消费服务端算好的那个**（它由"真的执行过的动作"决定），文案由模型出 ——
        // 前端不再自己重算一遍成败。
        const message = note ? `${describeRepair(result.applied)}；${note}` : describeRepair(result.applied);
        if (result.success) toast.success(message);
        else toast.error(message);
      }
      // 写操作刚刚改动了这台机器 —— 这一次刷新必须绕过缓存，否则用户会看着
      // 一份"装之前"的读数，以为操作没生效。
      await refresh(true);
    } catch (cause) {
      // 失败原因原样透出：这类失败几乎总是可操作的（网络、权限、版本），
      // 换成一句"操作失败"会把用户唯一的线索抹掉。
      toast.error(cause instanceof Error ? cause.message : "操作失败");
      await refresh(true).catch(() => undefined);
    } finally {
      setBusy(false);
      setRunning(null);
    }
  }, [diagnoses, environmentLabel, pending, refresh, selectedRunner, updateKey]);

  // 发起登录：把后端返回的授权链接/指引放进登录面板。设备码/浏览器的端到端回调依赖
  // 已安装 codebuddy 的真机输出校准 —— 后端当前会回一个可操作指引（见 agent_login.go）。
  const runLogin = useCallback(async () => {
    if (!loginAgent || loginBusy) return;
    setLoginBusy(true);
    setLoginInfo(null);
    try {
      const result = await api<{ authUrl?: string; userCode?: string; message?: string }>(
        `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(loginAgent.agentID)}/login`,
        { method: "POST" },
      );
      setLoginInfo(result);
    } catch (cause) {
      setLoginInfo({ message: cause instanceof Error ? cause.message : "发起登录失败" });
    } finally {
      setLoginBusy(false);
    }
  }, [loginAgent, loginBusy, selectedRunner]);

  // 核对登录态：向服务端问该工具当前是否已登录。
  const checkLoginStatus = useCallback(async () => {
    if (!loginAgent || loginBusy) return;
    setLoginBusy(true);
    try {
      const result = await api<{ loggedIn: boolean }>(
        `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(loginAgent.agentID)}/login-status`,
      );
      setLoggedIn(result.loggedIn);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "读取登录状态失败");
    } finally {
      setLoginBusy(false);
    }
  }, [loginAgent, loginBusy, selectedRunner]);

  // 逐主机授权：只对这一台机器生效，且可随时撤销（撤销入口在远程控制页/后续版本）。
  const grantRunnerInstall = useCallback(async () => {
    setBusy(true);
    try {
      await api(`/api/runners/${encodeURIComponent(selectedRunner)}/remote-install/grant`, { method: "POST" });
      toast.success("已允许在该主机上安装");
      await loadView(selectedRunner);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "授权失败");
    } finally {
      setBusy(false);
    }
  }, [loadView, selectedRunner]);

  /**
   * 手动安装命令的复制反馈。按工具 ID 记，因为同一个动作在一列里出现 N 次 ——
   * 用一个全局 bool 会让"复制了 Codex"在 Claude Code 那行也显示「已复制」。
   * 复位时间取 `MarkdownCodeBlock` 的同一个值，别处已经是这个手感。
   *
   * ⚠️ 计时器也必须**按 ID 各存各的**（`Record<string, number>`，不是单个 ref）：
   * 共用一个的话，连点两行会把前一行的计时器清掉，那一行就永远停在「已复制」。
   */
  const [manualCopy, setManualCopy] = useState<Record<string, "copied">>({});
  const manualCopyTimers = useRef<Record<string, number>>({});
  const manualCopyMounted = useRef(true);
  useEffect(() => {
    // StrictMode 下 effect 会「挂载 → 清理 → 再挂载」跑两次，标记必须在这里设回 true，
    // 否则开发模式下复制反馈会彻底失效（生产构建不双跑，看不出这个问题）。
    // 与 `components/MarkdownCodeBlock.tsx` 是同一段理由，不另造一套。
    manualCopyMounted.current = true;
    const timers = manualCopyTimers.current;
    return () => {
      manualCopyMounted.current = false;
      for (const id of Object.keys(timers)) window.clearTimeout(timers[id]);
    };
  }, []);

  const copyManualCommand = useCallback(async (agentID: string, command: string) => {
    const ok = await copyToClipboard(command);
    // 写入期间组件可能已经卸载（切走页面），此时不要再排一个没人清理的计时器。
    if (!manualCopyMounted.current) return;
    if (!ok) {
      // 复制失败**不能**静默 —— 用户会以为复制成功，粘出来是上一次的内容。
      toast.error("无法复制命令，请手动选中复制。");
      return;
    }
    setManualCopy((current) => ({ ...current, [agentID]: "copied" }));
    const timers = manualCopyTimers.current;
    if (timers[agentID] !== undefined) window.clearTimeout(timers[agentID]);
    timers[agentID] = window.setTimeout(() => {
      delete timers[agentID];
      setManualCopy((current) => {
        const next = { ...current };
        delete next[agentID];
        return next;
      });
    }, 1600);
  }, []);

  const runtime = view?.runtime ?? null;
  const items = view?.items ?? [];

  // 手动安装命令：命令本身在模型里拼（页面不拼命令），这里只取结果。
  // 不依赖所选环境 —— 这一块是手册，不是状态面板。
  const manualRows = useMemo(() => buildManualInstallRows(catalog.entries), [catalog.entries]);

  // 每张卡的全部判据都在模型里算（那里能被真的调用、真的断言），页面只渲染结果。
  // 页面里因此**不该**出现 meetsMinimumFor / npmVersion / autoUpdatable / preflight
  // 这些判据本身 —— 它们一旦在页面里再出现一次，就有了第二份口径。
  //
  // ⚠️ `reading`：这一页要**先出现、再填读数**。工具的名字与牌面来自工具目录
  // （`/api/agents`，启动时就已经到了，6ms），而"装没装、什么版本、能不能升"要等
  // `/api/runners/{id}/agents`（实测 2.3~6.7s：它要真的去这台机器上探）。
  // 等它的后果就是用户说的"等检查完才出页面"——那不是必要的等待。
  const reading = viewState !== "ready";
  const cards = useMemo<ToolCard[]>(() => catalog.entries.map((entry) => {
    const key = `${selectedRunner}:${entry.id}`;
    return buildToolCard({
      entry,
      item: items.find((candidate) => candidate.id === entry.id),
      runtime,
      latest: latest[key] ?? latestLoading,
      diagnosis: diagnoses[key],
      diagnosisError: diagnoseErrors[key],
      skipped: Boolean(skippedDiagnoses[key]),
      channelOk: diagnoseChannel?.ok ?? true,
      remoteInstallAllowed: Boolean(view?.remoteInstallAllowed),
      reading,
    });
  }), [catalog.entries, selectedRunner, items, runtime, latest, diagnoses, diagnoseErrors, skippedDiagnoses, diagnoseChannel, view?.remoteInstallAllowed, reading]);

  const banner = useMemo(() => bannerFor(cards), [cards]);
  // 「运行依赖」那一行：读数（文案 + 状态档）与**升级按钮亮不亮**由同一个函数一起给
  // （`runtimeDepLine`）—— 两者分家过一次，代价是"可升级到 X"与"没有按钮"同屏且无人解释
  // （2026-10-09 修）。页面因此**不数那三个条件**，也不自己拼按钮文案。
  // 授权那一道进门时必须带上：它是按钮的判据之一，漏传就等于把"未授权"当成"可以升"。
  const depLine = useMemo(
    () => (runtime && runtime.installed ? runtimeDepLine(runtime, Boolean(view?.remoteInstallAllowed)) : null),
    [runtime, view?.remoteInstallAllowed],
  );
  // 单独取出来给 onClick 用：`depLine?.action &&` 那个窄化进不了闭包（TS 会在
  // `depLine.action.kind` 上报 possibly undefined）。动作的种类也由它带来，
  // 页面不自己写死（见 `RuntimeDepLine.action` 的注释）。
  const depAction = depLine?.action;
  const openCard = cards.find((card) => card.id === openAgent);
  const openItem = items.find((candidate) => candidate.id === openAgent);
  const openEntry = catalog.entries.find((entry) => entry.id === openAgent);
  const openKey = openAgent ? updateKey(openAgent) : "";
  const openDiagnosis = openKey ? diagnoses[openKey] : undefined;
  const openDiagnosisError = openKey ? diagnoseErrors[openKey] : "";
  /**
   * 抽屉里那条「预检」说明 —— **算在模型里，页面不判**。
   *
   * 页面此前自己写了一遍 `openDiagnosis.preflight && !openDiagnosis.preflight.installOk`，
   * 而那正是 `preflightNote` 自己那句 `if (!preflight || preflight.installOk) return ""`
   * 的取反：同一件事两处判，将来 `preflightNote` 改了守卫（比如 installOk 为真但仍有话
   * 要说），页面这块会**静默地把话藏掉**。同族的 `preflight.upgradeOk` 早有禁令
   * （见 cli-tools-page.test.mjs 那条「页面里一次都不该出现」），`installOk` 是漏网的
   * 那一个（2026-10-09 补）。现在只有这一处判：有话说就整块出来，没话说整块不出现。
   */
  const openPreflightNote = openDiagnosis ? preflightNote(openDiagnosis.preflight) : "";

  /** 卡片上那个主动作点了之后做什么。 */
  const runPrimary = useCallback((card: ToolCard) => {
    const action = card.primary;
    if (!action) return;
    if (action.kind === "install") setPending({ kind: "install-agent", agentID: card.id });
    else if (action.kind === "update") setPending({ kind: "update-agent", agentID: card.id });
    else setPending({ kind: "repair", agentID: card.id, remedies: action.remedies });
  }, []);

  return <main className="app-shell cli-tools-shell">
    <header className="dashboard-bar cli-tools-bar">
      {/* 返回收成圆形图标钮（aria-label 承担可读名）：纯导航，视觉上让位给标题。 */}
      <button type="button" className="cli-tools-back" aria-label="返回首页" onClick={() => navigate("/")}>←</button>
      {/* 终端徽标：这页管的是命令行工具，徽标把页面身份立起来（纯装饰，可读名在 h1）。 */}
      <span className="cli-tools-badge" aria-hidden="true">&gt;_</span>
      <h1 className="cli-tools-title">Cli管理</h1>
      {/* 只有一个执行环境时不摆选择器：那一整块（标签组 + "当前环境：…"那行）在单机用户那里
          是纯噪音。多环境时它是必需的（docs/42 §8.2：多环境必须可选）。 */}
      {runnersState === "ready" && runners.length > 1 && <div className="cli-tools-env" role="tablist" aria-label="执行环境">
        {runners.map((runner) => <button
          key={runner.id}
          type="button"
          role="tab"
          aria-selected={runner.id === selectedRunner}
          className="cli-tools-env-item"
          onClick={() => {
            // 切执行环境是"去看另一台机器"，不是"重新检查这台" —— 别把上一次手动刷新
            // 残留的意愿带过去（万一那次刷新失败、下面那个 effect 没触发，ref 会留着 true）。
            forceNextReadings.current = false;
            setSelectedRunner(runner.id);
          }}
        >{runnerLabel(runner)}</button>)}
      </div>}
      <div className="cli-tools-bar-actions">
        <button type="button" className="cli-tools-refresh secondary" disabled={viewState === "loading"} onClick={() => void refresh(true)}>
          <span className="cli-tools-refresh-glyph" aria-hidden="true">↻</span>
          {viewState === "loading" ? "读取中…" : "重新检查"}
        </button>
      </div>
    </header>

    {/* 进行中横幅。装/升级是数十秒到数分钟级的操作（docs/42 §4.4），而在这之前
        点完「确认」的界面是"弹窗关掉、按钮置灰、什么都不说" —— 用户唯一能做的就是
        盯着一个没反应的按钮，或者以为点漏了**再点一次**。这一条要说清三件事：
        在做什么、要多久、别再点。 */}
    {running && <div className="cli-tools-banner" data-tone="busy" role="status">
      <span className="cli-tools-banner-mark" aria-hidden="true">…</span>
      <span>{pendingRunningText(running.action, running.runnerText)}</span>
    </div>}

    <section className="cli-tools-body">
      {/* ⚠️ 这里原先是「正在读取执行环境…」一整块 —— 它把页面按到 `/api/runners` 回来为止
          （实测 1~2s）。现在这一档由**卡片自己**承担（名字先出、读数显示"读取中"），
          页面不再为了执行环境列表空等。 */}
      {runnersState === "error" && <div className="cli-tools-block cli-tools-error" role="alert">
        <b>读不到执行环境</b><span>{runnersError}</span>
        <button className="secondary" onClick={() => void loadRunners()}>重试</button>
      </div>}
      {/* 空态要有下一步动作（卡片式），不能是一行灰字。 */}
      {runnersState === "ready" && runners.length === 0 && <div className="cli-tools-block cli-tools-empty">
        <b>没有可管理的执行环境</b>
        <span>工具要装在某台机器的环境里。先回首页加载一个项目，或配对一个远程主机。</span>
        <button className="primary" onClick={() => navigate("/")}>回首页</button>
      </div>}

      {runnersState !== "error" && (runnersState !== "ready" || runners.length > 0) && <>
        {viewState === "error" && <div className="cli-tools-block cli-tools-error" role="alert">
          <b>读不到这台机器上的工具状态</b><span>{viewError}</span>
          <button className="secondary" onClick={() => void loadView(selectedRunner, true)}>重试</button>
        </div>}

        {/* 通道坏了：这一档**必须**与"未安装"分开说。说成"未安装"会把用户支去装一个
            装不上的东西，而真正要处理的是 WSL / SSH 连接。 */}
        {viewState === "ready" && view && !view.probeOk && <div className="cli-tools-block cli-tools-warn" role="alert">
          <b>无法检测</b>
          <span>{view.probeError || "该执行环境当前不可用。"}</span>
          <small>这不是"工具未安装"——先把这个执行环境准备好，再回来刷新。</small>
        </div>}

        {/* 只有"正在读"与"读到了且通道是通的"才画卡片：
            读不到 / 通道坏了这两种**不该**画出一排永远"读取中"的卡（那是在装还在读）。 */}
        {viewState !== "error" && !(viewState === "ready" && view && !view.probeOk) && <>
          {/* 顶部横幅：**只在有事时存在**。它要说的是"有一件事需要你处理"，不是"三件小事"。
             读数还没到时它是空的（没有结论可说）。 */}
          {banner && <div className="cli-tools-banner" data-tone={banner.tone} role="status">
            <span className="cli-tools-banner-mark" aria-hidden="true">!</span>
            <span>{banner.text}</span>
            {/* 只有卡片**真有** repair 动作时才给按钮：否则点下去与旁边那颗
                「看看是什么问题」是同一个效果（两个入口做同一件事）。 */}
            {banner.actionLabel && <button className="primary" disabled={busy} onClick={() => {
              const card = cards.find((candidate) => candidate.id === banner.agentID);
              // `actionLabel` 只在卡片真有 repair 动作时才由模型给出（见 bannerFor），
              // 所以这里只有那一档可走 —— 原来那句"否则打开抽屉"的兜底是**到不了的**
              // 死分支（2026-10-08：卡片上的「详情」已删，兜底跟着一起清掉）。
              if (card?.primary?.kind !== "repair") return;
              setPending({ kind: "repair", agentID: card.id, remedies: card.primary.remedies });
            }}>{banner.actionLabel}</button>}
            <button type="button" className="cli-tools-link" onClick={() => setOpenAgent(banner.agentID)}>看看是什么问题</button>
          </div>}

          {/* 诊断自己也有"没查成"这一档：**不能**并进"没有问题"。它是全页唯一必须留在一线的
              "我们不知道" —— 别的排障素材都可以收进抽屉，这一句不行。 */}
          {viewState === "ready" && view?.probeOk && (diagnoseState === "error" || diagnoseChannel?.ok === false) &&
            <div className="cli-tools-block cli-tools-warn" role="alert">
              <b>这次没检查成功</b>
              <span>{diagnoseState === "error" ? diagnoseError : (diagnoseChannel?.error || "检测通道当前不可用。")}</span>
              <small>这不代表"没有问题"——只是这一次没查成。</small>
              <button className="secondary" disabled={diagnoseState === "loading"} onClick={() => void runDiagnostics(selectedRunner, true)}>重新检测</button>
            </div>}

          {/* 区块标题行只有名字与计数：**重新检查**在顶栏那一处，
              这里不再摆第二颗同义按钮 —— 两个入口做同一件事，用户就得去猜它们差在哪。 */}
          <div className="cli-tools-sechead">
            <h2>工具</h2>
            {catalog.loaded && !catalog.error && catalog.entries.length > 0 &&
              <span className="cli-tools-count">{catalog.entries.length} 个</span>}
          </div>

          {/* 目录有三态，缺一不可：读失败与"还没读到"都会被读成"平台没有工具"。
              ⚠️ 「还没读到目录」才用骨架：那时连**名字**都没有，只能给占位。
              目录一到就画真卡片（`reading` 那档负责"读数还在路上"）——
              这就是"先出页面、再填读数"的落点。 */}
          {catalog.error
            ? <div className="cli-tools-block cli-tools-error" role="alert">
              <b>读不到工具目录</b><span>{catalog.error}</span>
              <button className="secondary" onClick={() => void loadAgentCatalog().catch(() => undefined)}>重试</button>
            </div>
            : !catalog.loaded
              ? <div className="cli-tools-skeleton" role="status" aria-label="正在读取这台机器上的工具">
                {[0, 1, 2].map((index) => <div className="cli-tools-skeleton-card" key={index}>
                  <span className="cli-tools-skeleton-mark" />
                  <span className="cli-tools-skeleton-line" data-w="long" />
                  <span className="cli-tools-skeleton-line" data-w="short" />
                  <span className="cli-tools-skeleton-pill" />
                </div>)}
              </div>
              : catalog.entries.length === 0
                ? <div className="cli-tools-block cli-tools-pending" role="status">已读到工具目录，但里面没有工具。</div>
                : <div className="cli-tools-grid">
                  {cards.map((card) => {
                    const key = updateKey(card.id);
                    return <article className="cli-tools-card" key={card.id} data-tone={card.statusTone}>
                      <header className="cli-tools-card-head">
                        {/* 官方图标优先（判据在模型的 `agentLogoKey`）；没有官方图标
                            的工具回落两字母牌 —— 绝不拿别的产品的图标冒充。 */}
                        <span className="cli-tools-mark" data-brand={card.tint} aria-hidden="true">
                          {card.logo ? <AgentLogo logo={card.logo}/> : card.mark}
                        </span>
                        <b>{card.name}</b>
                        <span className="cli-tools-icon" data-icon={card.icon} aria-label={iconLabel(card.icon)}>
                          {iconGlyph(card.icon)}
                        </span>
                      </header>
                      <dl className="cli-tools-readings">
                        <div><dt>当前版本</dt><dd data-tone={card.currentTone}>{card.currentText}</dd></div>
                        <div><dt>最新版本</dt><dd data-tone={card.latestTone}>{card.latestText}</dd></div>
                      </dl>
                      <p className="cli-tools-status" data-tone={card.statusTone}>{card.statusText}</p>
                      {card.note && <p className="cli-tools-note" data-tone={card.noteTone}>{card.note}</p>}
                      {/* 「修复完成」本身证明不了任何事 —— 要说清哪些症状真的没了。
                          ⚠️ 它渲染在**详情抽屉之外**：修好之后批量诊断会把该工具判为"不必详查"
                          （skipped，于是 diagnosis 被移除），放在抽屉里就永远看不见了 ——
                          而那一刻正是最该看见它的时候。 */}
                      {resolvedNotes[key] && <p className="cli-tools-resolved" role="status">{resolvedNotes[key]}</p>}
                      {/* ⚠️ 一个动作都没有时**整条不渲染**。这条动作区带 border-top 与浅底
                          （`cli-tools-card-actions`），空着渲染会在卡底留下一条 23px 的空条 ——
                          「已是最新、不需要登录」那一档（最健康、最常见的一档）张张如此。
                          所以这三颗（主操作 / 登录 / 详情）都可能是 undefined，守卫要把它们
                          全算进来。 */}
                      {(card.primary || card.canLogin || card.canOpenDetails) && <div className="cli-tools-card-actions">
                        {card.primary && <button className="primary" disabled={busy} onClick={() => runPrimary(card)}>
                          {running && running.runnerID === selectedRunner && "agentID" in running.action && running.action.agentID === card.id
                            ? pendingActionText(running.action).button
                            : card.primary.label}
                        </button>}
                        {/* 需要登录的工具（`canLogin` 由模型按目录的 supportsLogin + 真的装没装判）
                            把入口摆在卡片上：点开就是登录面板，不用先进详情抽屉。
                            它是**次要**按钮 —— 登录是"能做的事"，不是"这一屏要做的事"。 */}
                        {card.canLogin && <button type="button" className="secondary" disabled={busy} onClick={() => {
                          setLoggedIn(false);
                          setLoginInfo(null);
                          setLoginAgent({ agentID: card.id, name: card.name });
                        }}>登录</button>}
                        {/* 「详情」只在模型判定"这张卡的下一步就在抽屉里"时出现（canOpenDetails）。
                            横幅只指向**第一张**问题卡，别的卡（第 2 张要处理的、"没检查成功"那一档）
                            否则没有任何入口，而卡片文案却让用户"按详情里的证据处理"。
                            它不占主操作位、不参与 bannerFor；已是最新的健康卡不给它。 */}
                        {card.canOpenDetails && <button type="button" className="secondary" disabled={busy} onClick={() => setOpenAgent(card.id)}>详情</button>}
                      </div>}
                    </article>;
                  })}
                </div>}
        </>}
      </>}
    </section>

    {viewState === "ready" && view?.probeOk && <>
      {/* 运行依赖：它不是目录里的工具，但每个工具都靠它。一行说清"够不够用"。 */}
      <section className="cli-tools-body cli-tools-dep">
        <div className="cli-tools-sechead"><h2>运行依赖</h2></div>
        {/* 仪表行（2026-09-25，docs/42 §28）：徽标立身份、大数字读数做主角、右侧状态点。
            没装的那一档沿用同一套语言但徽标转灰 —— 「没装」是实态，不冒充读数。 */}
        <div className="cli-tools-dep-bar">
          {runtime?.installed
            ? <><span className="cli-tools-dep-mark" aria-hidden="true">⬢</span>
              <span className="cli-tools-dep-who">
                <b>Node.js 运行时</b>
                <small><code>{runtime.version}</code>{runtime.npmVersion && <> · npm {runtime.npmVersion}</>} · {runtime.origin === "managed" ? "平台托管，不动系统里已有的 Node" : "系统已安装"}</small>
              </span>
              {/* 满足度是这一栏存在的理由 —— 大数字读数。只显示服务端给的数，不自己判。 */}
              <span className="cli-tools-dep-meter">
                <b>{runtime.meetsMinimumFor?.length ?? 0} / {cards.length}</b>
                <span>工具满足</span>
              </span>
              <span className="cli-tools-dep-right">
                {/* 右边是**读数**（状态点 + 一句话），有新版**且真的点得动**时才跟一颗动作按钮。
                    两件事都由模型一起给（`runtimeDepLine`）：读不到最新版本时**不能**写"已是最新"，
                    升不了时**也不能**只写"可升级到 X" 而不说为什么 —— 这正是 2026-10-09 修的
                    那个症状（未授权的 WSL 上：读数说能升、按钮没了、那一行一个字都不解释）。 */}
                <span className="cli-tools-dep-state" data-tone={depLine?.tone}>
                  {depLine?.text}
                </span>
                {depAction && <button className="secondary" disabled={busy} onClick={() => setPending({ kind: depAction.kind })}>
                  {depAction.label}
                </button>}
              </span></>
            : <><span className="cli-tools-dep-mark" data-tone="missing" aria-hidden="true">⬢</span>
              <span className="cli-tools-dep-who">
                <b>Node.js 运行时还没装</b>
                <small>{runtime?.installBlockedReason || "CLI 工具要通过 npm 安装，需要先装好它。"}</small>
              </span>
              <span className="cli-tools-dep-right">
                {/* 未授权时这里**不给动作**：安装会以 403 失败，亮一个点了必失败的按钮
                    比不亮更坏。授权入口在这一栏下面单独一处（见 `cli-tools-dep-grant`）。 */}
                {view?.remoteInstallAllowed && runtime?.installSupported &&
                  <button className="primary" disabled={busy} onClick={() => setPending({ kind: "install-runtime" })}>
                    安装 Node.js 运行时
                  </button>}
                {/* LTS 提示是**陈述**不是动作，不进授权门 —— 恰恰是要做「允许安装」
                    决策的机器上，"会装哪个版本"这个决策输入最该可见（2026-09-25 复查修正）。 */}
                {runtimeCatalog && runtimeCatalog.versions.length > 0 && <span className="cli-tools-hintline">
                  默认装最新 LTS（{runtimeCatalog.versions[0].version}{runtimeCatalog.versions[0].lts ? ` · ${runtimeCatalog.versions[0].lts}` : ""}）
                </span>}
              </span></>}
        </div>
        {/* 未授权：给**一处**授权入口。
            ⚠️ 它必须与"运行时装没装"**无关**。原先它嵌在上面那个三元式的 else 分支里，
            于是只在"运行时装没装 = 没装"时才渲染 —— 而远端机器上 Node 通常早就装好了，
            后果是每张工具卡都写着「尚未授权…」、页面上却没有任何地方能授权，
            所有工具因此永远没有「安装」按钮（2026-09-24 修的正是这个）。
            入口仍然**只有这一处**：每个工具再各来一个同义按钮，未授权的机器上会出现 N+1 个。 */}
        {!view?.remoteInstallAllowed && <p className="cli-tools-dep-grant">
          <span>这台主机还没授权平台安装 —— 安装会在它上面执行 npm 包代码（需要时还会落一整套 Node.js 运行时），需要你先显式确认一次。</span>
          <button className="primary" disabled={busy} onClick={() => void grantRunnerInstall()}>允许在此主机安装</button>
        </p>}
        {/* 运行时装好了但随包的 npm 不可用：这一档是**预期会发生**的（分发异常），
            而它下面每个工具都装不了 —— 必须给一个重装入口，否则用户只能看着"请重装"却无处可点。
            ⚠️ 授权门只挡**按钮**，不挡这一整段（2026-10-09 修）：未授权时整段消失等于把
            "随包的 npm 不可用"这条读数也一起藏掉 —— 而它是这台机器的事实，与授没授权无关
            （与"运行依赖"那一行同一个病：读数被动作的判据连坐）。收回按钮时照上面那条
            缘由说清为什么，不写第二份措辞。 */}
        {runtime?.installed && runtime.installSupported && !runtime.npmVersion &&
          <p className="cli-tools-note">运行时装好了，但随包的 npm 不可用 —— 装不了 CLI，请重装 Node.js 运行时。
            {view?.remoteInstallAllowed
              ? <button className="secondary" disabled={busy} onClick={() => setPending({ kind: "install-runtime" })}>重装</button>
              : <span>{grantNeededReason}（见上）。</span>}
          </p>}
      </section>

      {/* 这里原来还有两块"元信息"：一句汇总（`tallyText`）+ 一行「本次检查范围：Windows」。
          整块已删（2026-09-23，用户指出它们是噪音）。删得掉的前提是**没有事实因此失联**：
            - 「上次检查」= 每张卡详情里的「诊断于 今天 09:00」（`diagnosisMetaLine`）；
            - 「这次没查的部分」= 每个工具自己那条 `limitations`（跨端时逐个工具都发了）；
            - 环境名 = 多环境时顶部的选择器；
            - 那句汇总本身没有新信息：有事时由顶部横幅说，没事时每张卡自己写着「已是最新」。
          ⚠️ 唯一不能跟着一起删的是上面那块「**这次没检查成功**」—— 它是全页的"我们不知道"。 */}
    </>}

    {/* ── 手动安装命令：平台装不了时的出路 ─────────────────────────────────
        默认折叠（原生 `<details>` 不带 `open`）：它是**手册**，不是这一屏的下一步，
        站着不动才是对的；用户要它的时候自己展开。

        ⚠️ 它**有意放在 `probeOk` 那道门之外**：通道探测不到（WSL 没起来 / SSH 连不上）
        恰恰是最需要自己上手装的时候。命令由 `buildManualInstallRows` 从目录里拼
        （页面不拼命令），清单内容与所选环境无关。 */}
    {viewState === "ready" && manualRows.length > 0 &&
      <section className="cli-tools-body cli-tools-manual">
        <details>
          <summary>
            <h2>手动安装命令</h2>
          </summary>
          <ul className="cli-tools-manual-list">
            {manualRows.map((row) => <li key={row.id}>
              <span className="cli-tools-manual-name">{row.name}</span>
              <code>{row.command}</code>
              <button
                type="button"
                className="secondary cli-tools-manual-copy"
                data-state={manualCopy[row.id] ?? "idle"}
                onClick={() => void copyManualCommand(row.id, row.command)}
              >{manualCopy[row.id] === "copied" ? "已复制" : "复制"}</button>
            </li>)}
          </ul>
        </details>
      </section>}

    {/* ── 详情抽屉：所有"给排障用"的素材都在这儿 ─────────────────────────── */}
    {openAgent && openCard && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="cli-tools-detail-title">
      <section className="modal cli-tools-detail">
        <header>
          <div><label>详情</label><h2 id="cli-tools-detail-title">{openCard.name}</h2></div>
          <span className="cli-tools-detail-state" data-tone={openCard.statusTone}>{openCard.statusText}</span>
        </header>
        <div className="cli-tools-detail-body">
          {openCard.detailNotes.map((line) => <p className="cli-tools-hintline" key={line}>{line}</p>)}

          {/* 诊断面板：症状 + 证据 + 动作，内容全部来自服务端。 */}
          {(openDiagnosis || openDiagnosisError) && <section className="cli-tools-diagnosis">
            {openDiagnosisError && <p className="cli-tools-note">这次检测没完成：{openDiagnosisError}</p>}
            {openDiagnosis && <>
              {/* 读数元信息：实测版本 + 什么时候测的（报告是快照，不写时间会变成"此刻的事实"）。 */}
              {diagnosisMetaLine(openDiagnosis) && <p className="cli-tools-hintline">{diagnosisMetaLine(openDiagnosis)}</p>}
              {/* 空态的说法由**结论**决定：没查成时它也是零症状，但那不是"没有发现症状"。 */}
              {openDiagnosis.issues.length === 0 && <p className="cli-tools-diagnosis-empty">{diagnosisEmptyText(openDiagnosis)}</p>}
              {openDiagnosis.issues.map((issue) => <div className="cli-tools-issue" key={issue.code} data-severity={issue.severity}>
                <p className="cli-tools-issue-summary">
                  {/* 变体走 data-*（项目约定：别拼动态类名）。 */}
                  <span className="cli-tools-severity" data-severity={issue.severity}>
                    {issue.severity === "blocker" ? "用不了" : issue.severity === "warning" ? "会绊住" : "说明"}
                  </span>
                  {issue.summary}
                </p>
                {issue.evidence.length > 0 && <ul className="cli-tools-issue-evidence">
                  {issue.evidence.map((line) => <li key={line}><code>{line}</code></li>)}
                </ul>}
                {issue.remedies.length > 0
                  ? <div className="cli-tools-issue-actions">
                    {issue.remedies.map((remedy) => <button
                      key={remedy.id}
                      type="button"
                      className="secondary"
                      title={remedy.detail}
                      disabled={busy}
                      onClick={() => setPending({ kind: "repair", agentID: openCard.id, remedies: [remedy] })}
                    >{remedy.label}</button>)}
                  </div>
                  // 修不了的就**不给按钮**，并说清只能手动处理 —— 给一个点了没反应的
                  // 按钮比不给更坏。
                  : <p className="cli-tools-issue-manual">这个症状平台不能自动修 —— 按上面的证据在目标环境手动处理。</p>}
              </div>)}
              {openPreflightNote && <div className="cli-tools-diagnosis-block">
                <h3>预检</h3>
                <p className="cli-tools-note">{openPreflightNote}</p>
              </div>}
              {/* ② 这台机器上的所有位置 —— "装了新版却不生效"的唯一解释视图 */}
              {openDiagnosis.paths.length > 0 && <div className="cli-tools-diagnosis-block">
                <h3>这台机器上的所有位置</h3>
                <table className="cli-tools-paths">
                  <tbody>
                    {openDiagnosis.paths.map((fact) => <tr key={fact.source + fact.path}>
                      <td>{fact.label}</td>
                      <td className="cli-tools-path">{fact.path}</td>
                      <td>{pathFactState(fact)}</td>
                      <td>{fact.version || "—"}</td>
                    </tr>)}
                  </tbody>
                </table>
              </div>}
              {/* ③ 上次失败的原文（来自审计，一直躺在库里没人读） */}
              {openDiagnosis.lastFailure && <div className="cli-tools-diagnosis-block">
                <h3>上次失败</h3>
                <p className="cli-tools-hintline">{openDiagnosis.lastFailure.action}
                  {openDiagnosis.lastFailure.fromVersion ? ` · ${openDiagnosis.lastFailure.fromVersion}` : ""}
                  {openDiagnosis.lastFailure.toVersion ? ` → ${openDiagnosis.lastFailure.toVersion}` : ""}
                  {/* 什么时候失败的 —— 没有它就分不出"上周那次"与"刚才那次"。 */}
                  {diagnoseMoment(openDiagnosis.lastFailure.createdAt) ? ` · ${diagnoseMoment(openDiagnosis.lastFailure.createdAt)}` : ""}
                </p>
                {openDiagnosis.lastFailure.detail && <pre className="cli-tools-failure">{openDiagnosis.lastFailure.detail}</pre>}
              </div>}
              {/* ④ 这次没查的部分 */}
              {openDiagnosis.limitations.length > 0 && <div className="cli-tools-diagnosis-block">
                <h3>这次没查的部分</h3>
                {openDiagnosis.limitations.map((line) => <p className="cli-tools-hintline" key={line}>{line}</p>)}
              </div>}
            </>}
            {/* 这一轮**没有详查**它的原因 —— skipped 有两种成因，说法必须不同。 */}
            {!openDiagnosis && !openDiagnosisError && skippedDiagnoses[openKey] &&
              <p className="cli-tools-hintline">{skippedDiagnosisText(diagnoseChannel?.ok ?? true)}</p>}
          </section>}

          <div className="cli-tools-diagnosis-block">
            <h3>安装信息</h3>
            <dl className="cli-tools-facts">
              {openItem?.binaryPath && <div><dt>安装位置</dt><dd className="cli-tools-path">{openItem.binaryPath}</dd></div>}
              <div><dt>命令名</dt><dd className="cli-tools-path">{openCard.commandName}</dd></div>
              {openEntry && <div><dt>最低运行时</dt><dd>Node {openEntry.minRuntimeVersion}</dd></div>}
            </dl>
          </div>
        </div>
        <footer>
          <button className="secondary" onClick={() => setOpenAgent("")}>关闭</button>
          <button
            className="secondary"
            disabled={Boolean(diagnoseBusy[openKey])}
            onClick={() => void diagnoseOne(openAgent)}
          >{diagnoseBusy[openKey] ? "检测中…" : "重新检测这个工具"}</button>
        </footer>
      </section>
    </div>}

    {pending && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="cli-tools-confirm-title">
      <section className="modal cli-tools-confirm">
        <header><div><label>确认操作</label><h2 id="cli-tools-confirm-title">{pendingActionText(pending).title}</h2></div></header>
        {pending.kind === "repair"
          ? <div className="cli-tools-confirm-body">
            {/* **不宣称执行顺序**：界面拿到的次序是"症状里出现动作的次序"，而真正执行的
                次序由服务端的 remedyOrder 定（先回滚、再处理本地文件、最后才联网重装）——
                两者可能正好相反。说一句自己做不到的承诺，比不说更坏。 */}
            <p>将执行下面这些动作（实际先后由服务端按依赖关系安排：先回滚、再处理本地文件、最后才联网重装）：</p>
            <ol className="cli-tools-confirm-steps">
              {pending.remedies.map((remedy) => <li key={remedy.id}><b>{remedy.label}</b><span>{remedy.detail}</span></li>)}
            </ol>
            <p className="cli-tools-hintline">服务端只会执行当前诊断允许的动作；不适用的会被跳过，并在结果里说明为什么。</p>
          </div>
          /* 下载源与托管位置是这次动作的**决策素材**：写具体值，不写"官方源/工具链
             目录"这种没法核对的词。托管位置只在 origin=managed（升级场景）时有值；
             首次安装时还没有这个目录，就不渲染，不假装知道。 */
          : <p className="cli-tools-confirm-body">
            {pending.kind === "install-runtime"
              ? <>将从{runtimeCatalog?.source ? <><code>{runtimeCatalog.source}</code>（官方源）</> : "官方源"}下载并解压到<b>平台自己的工具链目录</b>{runtime?.managedPath ? <>（<code>{runtime.managedPath}</code>）</> : ""}（不需要管理员权限，也不会改动系统里已有的 Node）。下载完成后会校验官方 SHA256。</>
              : <>将执行 <code>npm install -g</code> 把 {pending.kind === "install-agent" ? "该工具" : "该工具的最新版"}装到{pending.kind === "install-agent" ? "当前运行时使用的 npm 全局位置" : "原安装位置"}。
                安装期间该工具上的对话会被拒绝。</>}
          </p>}
        <p className="cli-tools-confirm-env">目标环境：<b>{environmentLabel}</b></p>
        <footer>
          <button className="secondary" onClick={() => setPending(null)}>取消</button>
          <button className="primary" onClick={() => void runPending()}>确认</button>
        </footer>
      </section>
    </div>}

    {loginAgent && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="cli-tools-login-title">
      <section className="modal cli-tools-login">
        <header>
          <div><label>登录</label><h2 id="cli-tools-login-title">{loginAgent.name}</h2></div>
          {loggedIn && <span className="cli-tools-login-ok">已登录</span>}
        </header>
        <div className="cli-tools-login-body">
          {loginInfo === null ? (<>
            <p>该工具需要登录后才能使用。点击「发起登录」在本机启动登录流程；若页面拿到授权链接，请在浏览器打开并完成授权。</p>
            {/* 这条只对 CodeBuddy 说：它的登录是官方交互式 TUI，拿不到无头设备码 URL
                （后端 agent_login.go 那两条等价指引要等点过「发起登录」才经 message 出现）。
                以前对所有工具无条件显示，其它工具那边它是废话。 */}
            {loginAgent.agentID === "codebuddy" && <p className="cli-tools-hintline">提示：CodeBuddy 的登录由官方交互式授权完成，若未能自动弹出授权链接，请按页面指引在目标环境完成登录。</p>}
          </>) : (<>
            {loginInfo.authUrl && <p className="cli-tools-login-url">授权：<ExternalLink href={loginInfo.authUrl} rel="noreferrer">{loginInfo.authUrl}</ExternalLink></p>}
            {loginInfo.userCode && <p>设备码：<b className="cli-tools-login-code">{loginInfo.userCode}</b></p>}
            {loginInfo.message && <p>{loginInfo.message}</p>}
          </>)}
        </div>
        <footer>
          <button className="secondary" disabled={loginBusy} onClick={() => setLoginAgent(null)}>关闭</button>
          <button className="secondary" disabled={loginBusy} onClick={() => void checkLoginStatus()}>{loginBusy ? "读取中…" : (loggedIn ? "刷新登录状态" : "检查登录状态")}</button>
          <button className="primary" disabled={loginBusy} onClick={() => void runLogin()}>{loginBusy ? "处理中…" : "发起登录"}</button>
        </footer>
      </section>
    </div>}
  </main>;
}

/**
 * 这次操作在做什么。
 *
 * 三处必须用**同一份**说法：确认弹窗的标题、进行中横幅、卡片按钮的文案。各写一遍
 * 必然有一天只剩两处改了 —— 那时用户会看到"确认安装"点下去变成"正在升级"。
 */
function pendingActionText(action: PendingAction): { title: string; running: string; button: string } {
  switch (action.kind) {
    case "install-runtime":
      return { title: "安装 Node.js 运行时", running: "正在安装 Node.js 运行时…", button: "安装中…" };
    case "install-agent":
      return { title: `安装 ${agentDisplayName(action.agentID)}`, running: `正在安装 ${agentDisplayName(action.agentID)}…`, button: "安装中…" };
    case "update-agent":
      return { title: `升级 ${agentDisplayName(action.agentID)}`, running: `正在升级 ${agentDisplayName(action.agentID)}…`, button: "升级中…" };
    default:
      return { title: `修复 ${agentDisplayName(action.agentID)}`, running: `正在修复 ${agentDisplayName(action.agentID)}…`, button: "修复中…" };
  }
}

/**
 * 进行中横幅那句话。
 *
 * 带上**执行环境**（`runnerText` 由发起时捕获，不是现读的选择框）：顶栏可以切环境，
 * 而这条横幅要说的是"哪台机器上正在跑什么"，切过去之后不能变成新环境的名字。
 *
 * 「不要重复点击」不是客套：这几种操作在服务端是**串行闸门**（`beginAgentMaintenance`），
 * 第二次点击要么被拒（看起来像失败），要么在第一次的几分钟里一直转圈。
 */
function pendingRunningText(action: PendingAction, runnerText: string): string {
  return `${runnerText}：${pendingActionText(action).running}这是分钟级操作，期间请不要重复点击。`;
}

/** 状态图标里那个字形。形状与观感一起构成"一眼看出该管哪张"。 */
function iconGlyph(icon: ToolCard["icon"]): string {
  if (icon === "ok") return "✓";
  if (icon === "update") return "↑";
  if (icon === "alert") return "!";
  return "";
}

/** 图标的可读名（图标本身在无障碍树里是装饰，含义必须另有一份文字）。 */
function iconLabel(icon: ToolCard["icon"]): string {
  if (icon === "ok") return "状态：可以使用";
  if (icon === "update") return "状态：可以更新";
  if (icon === "alert") return "状态：需要留意";
  if (icon === "off") return "状态：未安装";
  if (icon === "loading") return "状态：正在读取";
  return "状态：读不到";
}
