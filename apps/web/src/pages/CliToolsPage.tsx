// CLI 工具管理。
//
// 页面的职责只有一条：把"这个执行环境上现在到底是什么情况"如实呈现出来，并给出
// 唯一正确的下一步。所以它有三处刻意不合并的地方：
//
//  1. **三档状态分开渲染** —— 正在读取 / 无法检测（通道坏了）/ 真的没有。
//     合并任何两档，用户就会去做一件没有用的事（去装一个装不上、或本来就有的东西）。
//  2. **"不可安装"分三档、文案各不相同** —— 未授权 / 运行时缺失或过低 / 该架构不支持。
//     这三件事的下一步动作完全不同，用同一句灰字交代等于什么都没说。
//  3. **诊断自身也是三档**（docs/43）—— 检测没查成 / 发现了问题 / 没有问题。
//     把"没查成"并进"没问题"是最坏的一种：用户会以为这台机器一切正常。
//
// 诊断与修复的判据**全部来自服务端**：症状、证据、可用动作（含 label 与说明）都是
// 服务端算好的对象，页面只渲染、不拼命令、也不维护 id→文案的映射 —— 那种映射必然
// 与服务端漂移，而漂移的表现是"按钮上写着一件事、点下去做的是另一件"。

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { useNavigate } from "react-router-dom";
import { api } from "../lib/api";
import { agentDisplayName, loadAgentCatalog, useAgentCatalogState } from "../lib/agent-registry";
import type { AgentCatalogEntry } from "../lib/agent-registry";
import {
  describeRepair,
  diagnoseMoment,
  diagnosisBadge,
  diagnosisEmptyText,
  diagnosisMetaLine,
  offeredRemedies,
  pathFactState,
  preflightNotes,
  resolvedIssues,
  resolvedSummary,
  skippedDiagnosisText,
} from "../lib/cli-diagnosis";
import type { AgentDiagnosis, DiagnoseRemedy, RepairResult, RunnerDiagnosticsView } from "../lib/cli-diagnosis";
import type { RunnerInfo } from "../lib/types";
import "./cli-tools.css";

type RunnerAgentItem = {
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

type RuntimeStatus = {
  id: string;
  installed: boolean;
  version: string;
  npmVersion: string;
  npmPath?: string;
  origin: "system" | "managed" | "none";
  managedPath?: string;
  meetsMinimumFor: string[] | null;
  installSupported: boolean;
  installBlockedReason?: string;
  latestVersion?: string;
  updateAvailable: boolean;
};

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

/** 待确认的动作。安装、升级与修复都要先确认：它们会改动目标环境上的东西。 */
type PendingAction =
  | { kind: "install-runtime" }
  | { kind: "install-agent"; agentID: string }
  | { kind: "update-agent"; agentID: string; to?: string }
  | { kind: "repair"; agentID: string; remedies: DiagnoseRemedy[] };

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
  const [checking, setChecking] = useState<Record<string, boolean>>({});
  const [updates, setUpdates] = useState<Record<string, { updateAvailable: boolean; latestVersion?: string; error?: string }>>({});
  const [pending, setPending] = useState<PendingAction | null>(null);
  // 登录面板状态：loginAgent 非空时打开；loginInfo 承载后端返回的授权链接/指引。
  const [loginAgent, setLoginAgent] = useState<{ agentID: string; name: string } | null>(null);
  const [loginInfo, setLoginInfo] = useState<{ authUrl?: string; userCode?: string; message?: string } | null>(null);
  const [loginBusy, setLoginBusy] = useState(false);
  const [loggedIn, setLoggedIn] = useState(false);
  const [runtimeCatalog, setRuntimeCatalog] = useState<RuntimeCatalog | null>(null);

  // 诊断状态。按 (runner, agent) 索引，理由与 updates 相同：只按 agent 索引会把
  // A 机器上的症状带到 B 机器的同名工具卡片上。
  const [diagnoses, setDiagnoses] = useState<Record<string, AgentDiagnosis>>({});
  const [diagnoseErrors, setDiagnoseErrors] = useState<Record<string, string>>({});
  const [diagnoseBusy, setDiagnoseBusy] = useState<Record<string, boolean>>({});
  const [skippedDiagnoses, setSkippedDiagnoses] = useState<Record<string, boolean>>({});
  const [diagnoseState, setDiagnoseState] = useState<"idle" | "loading" | "ready" | "error">("idle");
  const [diagnoseError, setDiagnoseError] = useState("");
  const [diagnoseLimitations, setDiagnoseLimitations] = useState<string[]>([]);
  /**
   * 批量诊断那一侧读到的**通道状态**。
   *
   * 必须真的消费它：通道坏掉时服务端会把**所有**工具都放进 `skipped`，而 `skipped`
   * 有两种成因（"没什么可疑的"与"根本没查"），说法不同 —— 不消费它，通道坏掉时每个
   * 工具都会盖上一个"当前没有可疑迹象"的章（见 `skippedDiagnosisText`）。
   * 顶层那份 `view.probeOk` 来自另一个端点，可能已经过时，不能替代它。
   */
  const [diagnoseChannel, setDiagnoseChannel] = useState<{ ok: boolean; error?: string } | null>(null);
  /** 展开了哪几份诊断（key 同 updateKey）。 */
  const [openDiagnoses, setOpenDiagnoses] = useState<Record<string, boolean>>({});
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

  const loadView = useCallback(async (runnerID: string) => {
    if (!runnerID) return;
    const generation = ++viewGeneration.current;
    setViewState("loading");
    try {
      const result = await api<RunnerAgentsView>(`/api/runners/${encodeURIComponent(runnerID)}/agents`);
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

  // 检查结果按 (runner, agent) 索引：只按 agent 索引会把 A 上"发现新版本"
  // 带到 B 的同名工具卡片上。
  const updateKey = useCallback((agentID: string) => `${selectedRunner}:${agentID}`, [selectedRunner]);

  // 批量诊断。**不做进列表接口**：它要跑多次子进程、扫目录、读审计，
  // 与 docs/42 §14.G 里"运行时探测不进热路径"是同一条理由。
  const runDiagnostics = useCallback(async (runnerID: string) => {
    if (!runnerID) return;
    const generation = ++diagnoseGeneration.current;
    const prefix = `${runnerID}:`;
    setDiagnoseState("loading");
    setDiagnoseError("");
    try {
      const result = await api<RunnerDiagnosticsView>(`/api/runners/${encodeURIComponent(runnerID)}/diagnostics`);
      if (diagnoseGeneration.current !== generation) return;
      // 服务端会回它自己的 runnerId：与请求的不一致就说明这份结果不属于这次请求，
      // 直接丢掉并如实说 —— 与代际校验同一个目的，多一道。
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
      setDiagnoseLimitations(result.limitations ?? []);
      setDiagnoseState("ready");
    } catch (cause) {
      if (diagnoseGeneration.current !== generation) return;
      // 检测失败**不清空**已有结论：清空会把"这次没查成"变成"没有发现问题"。
      setDiagnoseState("error");
      setDiagnoseError(cause instanceof Error ? cause.message : "无法完成问题检测");
    }
  }, []);

  // 进入页面（或换执行环境）后自动跑一次批量诊断。放在这里而不是列表接口里：
  // 它是"深查"，只该发生在管理页真的被打开时。
  useEffect(() => {
    if (viewState !== "ready") return;
    void runDiagnostics(selectedRunner);
  }, [runDiagnostics, selectedRunner, viewState]);

  /** 单个工具的显式详查（卡片上的「检测这个工具」）。 */
  const diagnoseOne = useCallback(async (agentID: string) => {
    const key = updateKey(agentID);
    setDiagnoseBusy((current) => ({ ...current, [key]: true }));
    try {
      const result = await api<AgentDiagnosis>(
        `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(agentID)}/diagnose`,
      );
      setDiagnoses((current) => ({ ...current, [key]: result }));
      setSkippedDiagnoses((current) => ({ ...current, [key]: false }));
      setDiagnoseErrors((current) => {
        const next = { ...current };
        delete next[key];
        return next;
      });
      setOpenDiagnoses((current) => ({ ...current, [key]: true }));
    } catch (cause) {
      setDiagnoseErrors((current) => ({
        ...current,
        [key]: cause instanceof Error ? cause.message : "检测未完成",
      }));
    } finally {
      setDiagnoseBusy((current) => ({ ...current, [key]: false }));
    }
  }, [selectedRunner, updateKey]);

  const refresh = useCallback(async () => {
    // ⚠️ 这里**不**显式调 runDiagnostics：那个 effect（viewState 回到 ready 时触发）
    // 已经是一个触发点，再调一次就是每次刷新都跑**两遍**完整诊断（每遍都要拉起子进程
    // 探测），而两遍的结论必然一样。触发点只能有一个。
    await Promise.all([loadRunners(), loadView(selectedRunner)]);
  }, [loadRunners, loadView, selectedRunner]);

  const checkUpdate = useCallback(async (agentID: string) => {
    const key = updateKey(agentID);
    setChecking((current) => ({ ...current, [key]: true }));
    try {
      const result = await api<{ updateAvailable: boolean; latestVersion?: string; error?: string }>(
        `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(agentID)}/check-update`,
        { method: "POST" },
      );
      setUpdates((current) => ({ ...current, [key]: result }));
    } catch (cause) {
      setUpdates((current) => ({
        ...current,
        [key]: { updateAvailable: false, error: cause instanceof Error ? cause.message : "检查更新失败" },
      }));
    } finally {
      setChecking((current) => ({ ...current, [key]: false }));
    }
  }, [updateKey]);

  const runPending = useCallback(async () => {
    if (!pending) return;
    const action = pending;
    setPending(null);
    setBusy(true);
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
        await api(`/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(action.agentID)}/install`, {
          method: "POST",
          body: JSON.stringify({ version: "latest" }),
        });
        toast.success(`${agentDisplayName(action.agentID)} 已安装`);
      } else if (action.kind === "update-agent") {
        await api(`/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(action.agentID)}/update`, { method: "POST" });
        toast.success(`${agentDisplayName(action.agentID)} 已升级`);
      } else {
        const result = await api<RepairResult>(
          `/api/runners/${encodeURIComponent(selectedRunner)}/agents/${encodeURIComponent(action.agentID)}/repair`,
          { method: "POST", body: JSON.stringify({ remedies: action.remedies.map((remedy) => remedy.id) }) },
        );
        // 服务端回填了**修复后重跑的**诊断 —— 直接换上去，让用户当场看出症状有没有真的消失。
        const key = updateKey(action.agentID);
        // 差集要在**换掉之前**算：换完就只剩新报告了，"哪些症状没了"这个信息也就丢了。
        // （差集自己会挡掉"修复后那份没查成"的情况，见 resolvedIssues 的注释。）
        const note = resolvedSummary(resolvedIssues(diagnoses[key], result.diagnosis));
        setResolvedNotes((current) => ({ ...current, [key]: note }));
        setDiagnoses((current) => ({ ...current, [key]: result.diagnosis }));
        // 成败**消费服务端算好的那个**（它由"真的执行过的动作"决定），文案由模型出 ——
        // 前端不再自己重算一遍成败。
        const message = note ? `${describeRepair(result.applied)}；${note}` : describeRepair(result.applied);
        if (result.success) toast.success(message);
        else toast.error(message);
      }
      setUpdates({});
      await refresh();
    } catch (cause) {
      // 失败原因原样透出：这类失败几乎总是可操作的（网络、权限、版本），
      // 换成一句"操作失败"会把用户唯一的线索抹掉。
      toast.error(cause instanceof Error ? cause.message : "操作失败");
      await refresh().catch(() => undefined);
    } finally {
      setBusy(false);
    }
  }, [diagnoses, pending, refresh, selectedRunner, updateKey]);

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

  const runtime = view?.runtime ?? null;
  const items = view?.items ?? [];

  // 能不能给这个工具安装：三件事都由**服务端算好** —— 环境支持装、有可用的 npm、
  // 运行时不低于该工具的最低要求（meetsMinimumFor）。
  //
  // 原先这里只用了 runtime.installed，等于把"有没有运行时"重判了一遍：Node 16
  // 满足"装了"但不满足工具要求，npm 缺失时也满足"装了" —— 两种情况界面都会给出
  // 一个点了必失败的按钮（服务端的 checkRuntimeGate 会拒）。
  const canInstallAgent = (item: RunnerAgentItem | undefined, entry: AgentCatalogEntry) =>
    Boolean(item?.installSupported)
    && Boolean(runtime?.npmVersion)
    && (runtime?.meetsMinimumFor?.includes(entry.id) ?? false);
  const runtimeMeetsMinimum = (agentID: string) =>
    runtime?.meetsMinimumFor?.includes(agentID) ?? false;
  const environmentLabel = useMemo(() => {
    const runner = runners.find((item) => item.id === selectedRunner);
    if (!runner) return selectedRunner;
    return `${runner.name}${runner.environment ? `（${runner.environment}）` : ""}`;
  }, [runners, selectedRunner]);

  return <main className="app-shell cli-tools-shell">
    <header className="dashboard-bar">
      <button type="button" className="cli-tools-back secondary" onClick={() => navigate("/")}>返回</button>
      <h1 className="cli-tools-title">CLI 工具管理</h1>
      <div className="cli-tools-bar-actions">
        <button type="button" className="cli-tools-diagnose-all secondary" disabled={diagnoseState === "loading" || viewState !== "ready"} onClick={() => void runDiagnostics(selectedRunner)}>
          {diagnoseState === "loading" ? "检测中…" : "检测问题"}
        </button>
        <button type="button" className="cli-tools-refresh secondary" disabled={viewState === "loading"} onClick={() => void refresh()}>
          {viewState === "loading" ? "刷新中…" : "刷新"}
        </button>
      </div>
    </header>

    <section className="cli-tools-section">
      <p className="cli-tools-intro">
        这里列出平台支持的 AI CLI 工具，以及每个执行环境上它们的安装状态。安装落到平台自己的工具链目录里，
        不需要管理员权限，也不会改动系统里已有的 Node。
      </p>

      {runnersState === "loading" && <div className="cli-tools-loading">正在读取执行环境…</div>}
      {runnersState === "error" && <div className="cli-tools-error" role="alert"><b>读不到执行环境</b><span>{runnersError}</span><button className="secondary" onClick={() => void loadRunners()}>重试</button></div>}

      {runnersState === "ready" && <div className="cli-tools-runners" role="tablist" aria-label="执行环境">
        {runners.map((runner) => <button
          key={runner.id}
          type="button"
          role="tab"
          aria-selected={runner.id === selectedRunner}
          className={`cli-tools-runner${runner.id === selectedRunner ? " selected" : ""}`}
          onClick={() => setSelectedRunner(runner.id)}
        >{runner.name}<small>{runner.environment}</small></button>)}
      </div>}

      {runnersState === "ready" && runners.length === 0 && <div className="cli-tools-loading">
        没有可管理的执行环境。
      </div>}

      {runnersState === "ready" && runners.length > 0 && <p className="cli-tools-current">当前环境：<b>{environmentLabel}</b></p>}

      {viewState === "loading" && <div className="cli-tools-loading">正在检测工具状态…</div>}

      {viewState === "error" && <div className="cli-tools-error" role="alert">
        <b>无法检测</b><span>{viewError}</span>
        <button className="secondary" onClick={() => void loadView(selectedRunner)}>重试</button>
      </div>}

      {/* 通道坏了：这一档**必须**与"未安装"分开说。说成"未安装"会把用户支去装一个
          装不上的东西，而真正要处理的是 WSL / SSH 连接。 */}
      {viewState === "ready" && view && !view.probeOk && <div className="cli-tools-probe" role="alert">
        <b>无法检测</b>
        <span>{view.probeError || "该执行环境当前不可用。"}</span>
        <small>这不是"工具未安装"——先把这个执行环境准备好，再回来刷新。</small>
      </div>}

      {/* 诊断自己也有"没查成"这一档：**不能**并进"没有问题"。 */}
      {viewState === "ready" && view?.probeOk && diagnoseState === "error" && <div className="cli-tools-probe" role="alert">
        <b>检测未完成</b>
        <span>{diagnoseError}</span>
        <small>这不代表"没有问题"——只是这一次没查成。</small>
      </div>}

      {viewState === "ready" && view?.probeOk && diagnoseState === "ready" && diagnoseLimitations.length > 0 && <div className="cli-tools-note cli-tools-limits">
        {diagnoseLimitations.map((line) => <span key={line}>{line}</span>)}
      </div>}

      {viewState === "ready" && view && view.probeOk && <>
        <section className="cli-tools-card cli-tools-runtime">
          <header>
            <h2>Node.js 运行时</h2>
            <span className={`cli-tools-badge ${runtime?.installed ? "ready" : "missing"}`}>
              {runtime?.installed ? `已安装 ${runtime.version}` : "未安装"}
            </span>
          </header>
          <dl className="cli-tools-facts">
            <div><dt>来源</dt><dd>{runtime?.origin === "managed" ? "平台托管" : runtime?.origin === "system" ? "系统已安装" : "未检测到"}</dd></div>
            {runtime?.npmVersion && <div><dt>npm</dt><dd>{runtime.npmVersion}</dd></div>}
            {runtime?.managedPath && <div><dt>安装位置</dt><dd className="cli-tools-path">{runtime.managedPath}</dd></div>}
            {runtimeCatalog && <div><dt>下载源</dt><dd className="cli-tools-path">{runtimeCatalog.source}</dd></div>}
          </dl>
          {/* 前置运行时缺失时，工具那边**不能**给出一个点了必失败的"安装"——
              所以这一块自己带一个明确的下一步。 */}
          {!runtime?.installed && runtime?.installBlockedReason && <p className="cli-tools-note">{runtime.installBlockedReason}</p>}
          {!runtime?.installed && !runtime?.installBlockedReason && view.remoteInstallAllowed && <p className="cli-tools-note">
            CLI 工具需要通过 npm 安装，请先在这里装好 Node.js 运行时。
          </p>}
          {/* 未授权时这一块只给一个动作：授权。安装会在别人的机器上跑 npm 并落一整套
              运行时，先让用户明确同意一次，比给一排点了必失败的按钮诚实。 */}
          {!view.remoteInstallAllowed && <p className="cli-tools-note">
            尚未授权在这台机器上安装。安装会在目标环境里执行 npm 并落一整套 Node.js 运行时，需要先确认一次。
          </p>}
          <div className="cli-tools-actions">
            {!view.remoteInstallAllowed && <button className="primary" disabled={busy} onClick={() => void grantRunnerInstall()}>允许在此主机安装</button>}
            {view.remoteInstallAllowed && runtime?.installSupported && !runtime.installed && <button className="primary" disabled={busy} onClick={() => setPending({ kind: "install-runtime" })}>安装 Node.js 运行时</button>}
            {view.remoteInstallAllowed && runtime?.installSupported && runtime.installed && runtime.updateAvailable && <button className="primary" disabled={busy} onClick={() => setPending({ kind: "install-runtime" })}>升级到 {runtime.latestVersion}</button>}
            {/* node 在、npm 不可用：这一档是**预期会发生**的（分发异常），而它下面每个
                工具都装不了 —— 必须给一个重装入口，否则用户只能看着"请重装"却无处可点。 */}
            {view.remoteInstallAllowed && runtime?.installSupported && runtime.installed && !runtime.npmVersion && <button className="primary" disabled={busy} onClick={() => setPending({ kind: "install-runtime" })}>重装 Node.js 运行时</button>}
            {runtime?.installed && !runtime.updateAvailable && <span className="cli-tools-uptodate">已是最新</span>}
            {runtimeCatalog && runtimeCatalog.versions.length > 0 && !runtime?.installed && <span className="cli-tools-hint">
              默认安装最新 LTS（{runtimeCatalog.versions[0].version}{runtimeCatalog.versions[0].lts ? ` · ${runtimeCatalog.versions[0].lts}` : ""}）
            </span>}
          </div>
        </section>

        <div className="cli-tools-list">
          {/* 目录有三态，缺一不可：读失败与"还没读到"都会被读成"平台没有工具"。 */}
          {catalog.error
            ? <div className="cli-tools-error" role="alert"><b>读不到工具目录</b><span>{catalog.error}</span></div>
            : !catalog.loaded
              ? <div className="cli-tools-loading">正在读取工具目录…</div>
              : catalog.entries.length === 0
                ? <div className="cli-tools-loading">已读到工具目录，但里面没有工具。</div>
                : catalog.entries.map((entry) => {
              const item = items.find((candidate) => candidate.id === entry.id);
              const key = updateKey(entry.id);
              const update = updates[key];
              const hasUpdate = Boolean(update?.updateAvailable && !update.error);
              const diagnosis = diagnoses[key];
              const diagnosisError = diagnoseErrors[key];
              const diagnosisOpen = Boolean(openDiagnoses[key]);
              // 服务端明确说"这一轮没详查"的工具（例如它当前已经就绪）。**必须消费它**：
              // 拿不到 diagnosis 时"没查"与"查了没问题"是两回事，不说清就等于把前者
              // 说成后者。
              const diagnosisSkipped = Boolean(skippedDiagnoses[key]);
              const remedies = diagnosis ? offeredRemedies(diagnosis) : [];
              // 四档各有各的说法，**"没查成"绝不并进"没问题"**。判据在模型里
              //（diagnosisBadge 同时看结论与症状），页面只消费 —— "没有问题 · 2 项症状"
              // 那种自相矛盾的一行因此不可能出现（可以"能用但有事要留意"）。
              const badge = diagnosis && !diagnosisError ? diagnosisBadge(diagnosis) : null;
              const tone = diagnosisError ? "unknown" : badge ? badge.tone : "idle";
              const label = diagnosisError
                ? "检测未完成"
                : badge
                  ? badge.label
                  : diagnoseState === "loading" ? "正在检测…" : "未检测";
              // 面板里的两行"读数元信息"与"预检"，都在模型里算（那是能写反的判据）。
              const metaLine = diagnosis ? diagnosisMetaLine(diagnosis) : "";
              const notes = diagnosis?.preflight ? preflightNotes(diagnosis.preflight) : [];
              const showDiagnosisLine = tone !== "idle" || diagnosisSkipped;
              return <section key={entry.id} className="cli-tools-card cli-tools-agent">
                <header>
                  <h2>{entry.name}</h2>
                  <span className={`cli-tools-badge ${item?.ready ? "ready" : item?.installed ? "pending" : "missing"}`}>
                    {item === undefined
                      // items 里没有这个 id：**不能**说成"未安装" —— 那是把"没有状态"写成"没有"。
                      ? "状态未知"
                      : item.operation === "running"
                        ? "进行中…"
                        : item.installed
                          ? `已安装 ${item.version || ""}`.trim()
                          : item.reason || "未安装"}
                  </span>
                </header>
                <dl className="cli-tools-facts">
                  <div><dt>提供方</dt><dd>{entry.vendor}</dd></div>
                  <div><dt>npm 包</dt><dd className="cli-tools-path">{entry.npmPackage}</dd></div>
                  {item?.binaryPath && <div><dt>安装位置</dt><dd className="cli-tools-path">{item.binaryPath}</dd></div>}
                  <div><dt>最低运行时</dt><dd>Node {entry.minRuntimeVersion}</dd></div>
                </dl>

                {/* "不可安装"各档各给各的说法。判据全部来自服务端或 canInstallAgent ——
                    前端不另判一遍"有没有运行时"：Node 16 或 npm 缺失都满足"装了"，
                    那样界面会给出一个点了必失败的按钮。 */}
                {item === undefined && <p className="cli-tools-note">没有拿到这个工具的状态（服务端未返回该行）。</p>}
                {!item?.installSupported && item?.installBlockedReason && <p className="cli-tools-note">{item.installBlockedReason}</p>}
                {item?.installSupported && !runtime?.installed && <p className="cli-tools-note">
                  {runtime?.installBlockedReason || "需要先安装 Node.js 运行时。"}
                </p>}
                {item?.installSupported && runtime?.installed && !runtime?.npmVersion && <p className="cli-tools-note">
                  运行时装好了，但随包的 npm 不可用 —— 装不了 CLI，请重装 Node.js 运行时。
                </p>}
                {item?.installSupported && Boolean(runtime?.npmVersion) && !runtimeMeetsMinimum(entry.id) && <p className="cli-tools-note">
                  当前 Node {runtime?.version} 低于 {entry.name} 要求的 Node {entry.minRuntimeVersion}，请先升级运行时。
                </p>}

                <div className="cli-tools-actions">
                  {/* 有任务在进行时不给任何操作入口：服务端会返回 409，界面不该亮出按钮。 */}
                  {item?.operation === "running" && <span className="cli-tools-hint">该工具上已有任务在进行，完成前不能再操作。</span>}
                  {item?.installed && item.updateSupported && item.operation !== "running" && <button className="secondary" disabled={Boolean(checking[key]) || busy} onClick={() => void checkUpdate(entry.id)}>
                    {checking[key] ? "检查中…" : "检查更新"}
                  </button>}
                  {item?.installed && hasUpdate && item.autoUpdatable && !item.upgradeNeedsGrant && item.operation !== "running" && <button className="primary" disabled={busy} onClick={() => setPending({ kind: "update-agent", agentID: entry.id, to: update?.latestVersion })}>
                    升级到 {update?.latestVersion}
                  </button>}
                  {/* 有新版本但不能应用内升级：给手动命令，不给一个点了必失败的按钮。 */}
                  {item?.installed && hasUpdate && !item.autoUpdatable && <span className="cli-tools-hint">
                    发现新版本 {update?.latestVersion} · 需在目标环境手动执行 {entry.commandName} update
                  </span>}
                  {/* 能升级但要先授权 —— 与上面那一档**互斥**："平台不支持升级"与"授权还没给"
                      是两回事，两条同时出现会让用户不知道该做哪个。 */}
                  {item?.installed && hasUpdate && item.autoUpdatable && item.upgradeNeedsGrant && <span className="cli-tools-hint">
                    升级需要先在此主机授权（平台装的工具升级会在目标环境里重跑 npm）
                  </span>}
                  {item?.installed && update && !hasUpdate && !update.error && <span className="cli-tools-uptodate">已是最新</span>}
                  {item?.installed && update?.error && <span className="cli-tools-inline-error" role="alert">{update.error}</span>}
                  {/* 未授权时授权入口只在运行时卡片里给**一处** —— 每个工具再各来一个
                      同义按钮，未授权的机器上会出现 N+1 个"允许在此主机安装"。 */}
                  {!item?.installed && canInstallAgent(item, entry) && item?.operation !== "running" && <button className="primary" disabled={busy} onClick={() => setPending({ kind: "install-agent", agentID: entry.id })}>安装</button>}
                  {/* 需要登录的工具：已安装后可发起登录（支持平台内登录流程）。 */}
                  {item?.installed && entry.supportsLogin && item.operation !== "running" && <button className="secondary" disabled={busy} onClick={() => { setLoggedIn(false); setLoginInfo(null); setLoginAgent({ agentID: entry.id, name: entry.name }); }}>登录</button>}
                  {/* 「检测这个工具」是**显式动作**：批量检测只覆盖"已经有迹象"的工具，
                      就绪的工具要详查时由用户点一下。 */}
                  <button type="button" className="secondary cli-tools-diagnose-one" disabled={Boolean(diagnoseBusy[key])} onClick={() => void diagnoseOne(entry.id)}>
                    {diagnoseBusy[key] ? "检测中…" : "检测这个工具"}
                  </button>
                </div>

                {/* 诊断结论一行。**"没查成"不许并进"没问题"** —— 那是把读不到写成没有。
                    ⚠️ 类名只留基础名：变体由 `data-tone` 表达（CSS 也只认它），
                    拼一个 `${...}-${tone}` 上去会多出一个**没人样式化**的死类名。 */}
                {showDiagnosisLine && <div className="cli-tools-diagnosis-line" data-tone={tone} role="status">
                  <b>{label}</b>
                  {diagnosis && diagnosis.issues.length > 0 && <span>{diagnosis.issues.length} 项症状</span>}
                  {diagnosisError && <span>{diagnosisError}</span>}
                  {/* 「这一轮没详查」有两种成因，说法不同（通道坏掉时**不是**"没有可疑迹象"）。 */}
                  {!diagnosis && !diagnosisError && diagnosisSkipped && <span>{skippedDiagnosisText(diagnoseChannel?.ok ?? true)}</span>}
                  {diagnosis && <button type="button" className="secondary" onClick={() => setOpenDiagnoses((current) => ({ ...current, [key]: !diagnosisOpen }))}>
                    {diagnosisOpen ? "收起诊断" : "查看诊断"}
                  </button>}
                </div>}

                {/* 「修复完成」本身证明不了任何事 —— 要说清哪些症状真的没了。
                    ⚠️ 它渲染在**面板之外**：修好之后批量诊断会把该工具判为"不必详查"
                    （skipped，于是 diagnosis 被移除），放在面板里就永远看不见了 ——
                    而那一刻正是最该看见它的时候。 */}
                {resolvedNotes[key] && <p className="cli-tools-resolved" role="status">{resolvedNotes[key]}</p>}

                {diagnosis && diagnosisOpen && <section className="cli-tools-diagnosis">
                  {/* 读数元信息：实测版本 + 什么时候测的（报告是快照，不写时间会变成"此刻的事实"）。 */}
                  {metaLine && <p className="cli-tools-hint">{metaLine}</p>}
                  {/* ① 症状 + 证据 + 可用动作 */}
                  {/* 空态的说法由**结论**决定：没查成时它也是零症状，但那不是"没有发现症状"。 */}
                  {diagnosis.issues.length === 0 && <p className="cli-tools-diagnosis-empty">{diagnosisEmptyText(diagnosis)}</p>}
                  {diagnosis.issues.map((issue) => <div className="cli-tools-issue" key={issue.code} data-severity={issue.severity}>
                    <p className="cli-tools-issue-summary">
                      {/* 变体走 data-*（与上面那行同一个约定），不拼动态类名。 */}
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
                          onClick={() => setPending({ kind: "repair", agentID: entry.id, remedies: [remedy] })}
                        >{remedy.label}</button>)}
                      </div>
                      // 修不了的就**不给按钮**，并说清只能手动处理 —— 给一个点了没反应的
                      // 按钮比不给更坏。
                      : <p className="cli-tools-issue-manual">这个症状平台不能自动修 —— 按上面的证据在目标环境手动处理。</p>}
                  </div>)}
                  {remedies.length > 1 && <div className="cli-tools-diagnosis-actions">
                    <button type="button" className="primary" disabled={busy} onClick={() => setPending({ kind: "repair", agentID: entry.id, remedies })}>
                      修复全部（{remedies.length} 个动作）
                    </button>
                  </div>}

                  {/* ② 这台机器上的所有位置 —— "装了新版却不生效"的唯一解释视图 */}
                  {diagnosis.paths.length > 0 && <div className="cli-tools-diagnosis-block">
                    <h3>这台机器上的所有位置</h3>
                    <table className="cli-tools-paths">
                      <tbody>
                        {diagnosis.paths.map((fact) => <tr key={fact.source + fact.path}>
                          <td>{fact.label}</td>
                          <td className="cli-tools-path">{fact.path}</td>
                          <td>{pathFactState(fact)}</td>
                          <td>{fact.version || "—"}</td>
                        </tr>)}
                      </tbody>
                    </table>
                  </div>}

                  {/* ③ 上次失败的原文（来自审计，一直躺在库里没人读） */}
                  {diagnosis.lastFailure && <div className="cli-tools-diagnosis-block">
                    <h3>上次失败</h3>
                    <p className="cli-tools-hint">{diagnosis.lastFailure.action}
                      {diagnosis.lastFailure.fromVersion ? ` · ${diagnosis.lastFailure.fromVersion}` : ""}
                      {diagnosis.lastFailure.toVersion ? ` → ${diagnosis.lastFailure.toVersion}` : ""}
                      {/* 什么时候失败的 —— 没有它就分不出"上周那次"与"刚才那次"。 */}
                      {diagnoseMoment(diagnosis.lastFailure.createdAt) ? ` · ${diagnoseMoment(diagnosis.lastFailure.createdAt)}` : ""}
                    </p>
                    {diagnosis.lastFailure.detail && <pre className="cli-tools-failure">{diagnosis.lastFailure.detail}</pre>}
                  </div>}

                  {/* ④ 预检：把"点了会失败"提前说出来。安装与升级共用判据 ⇒ 通常只有一行，
                      分歧时才补第二行（见 preflightNotes）。 */}
                  {notes.length > 0 && <div className="cli-tools-diagnosis-block">
                    <h3>预检</h3>
                    {notes.map((line) => <p className="cli-tools-note" key={line}>{line}</p>)}
                  </div>}

                  {diagnosis.limitations.length > 0 && <div className="cli-tools-diagnosis-block">
                    <h3>这次没查的部分</h3>
                    {diagnosis.limitations.map((line) => <p className="cli-tools-hint" key={line}>{line}</p>)}
                  </div>}
                </section>}
              </section>;
            })}
        </div>
      </>}
    </section>

    {pending && <div className="backdrop" role="dialog" aria-modal="true">
      <section className="modal cli-tools-confirm">
        <header><div><label>确认操作</label><h2>{pending.kind === "install-runtime" ? "安装 Node.js 运行时" : pending.kind === "install-agent" ? `安装 ${agentDisplayName(pending.agentID)}` : pending.kind === "update-agent" ? `升级 ${agentDisplayName(pending.agentID)}` : `修复 ${agentDisplayName(pending.agentID)}`}</h2></div></header>
        {pending.kind === "repair"
          ? <div className="cli-tools-confirm-body">
            {/* **不宣称执行顺序**：界面拿到的次序是"症状里出现动作的次序"，而真正执行的
                次序由服务端的 remedyOrder 定（先回滚、再处理本地文件、最后才联网重装）——
                两者可能正好相反。说一句自己做不到的承诺，比不说更坏。 */}
            <p>将执行下面这些动作（实际先后由服务端按依赖关系安排：先回滚、再处理本地文件、最后才联网重装）：</p>
            <ol className="cli-tools-confirm-steps">
              {pending.remedies.map((remedy) => <li key={remedy.id}><b>{remedy.label}</b><span>{remedy.detail}</span></li>)}
            </ol>
            <p className="cli-tools-hint">服务端只会执行当前诊断允许的动作；不适用的会被跳过，并在结果里说明为什么。</p>
          </div>
          : <p className="cli-tools-confirm-body">
            {pending.kind === "install-runtime"
              ? <>将从官方源下载并解压到<b>平台自己的工具链目录</b>（不需要管理员权限，也不会改动系统里已有的 Node）。下载完成后会校验官方 SHA256。</>
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

    {loginAgent && <div className="backdrop" role="dialog" aria-modal="true">
      <section className="modal cli-tools-login">
        <header>
          <div><label>登录</label><h2>{loginAgent.name}</h2></div>
          {loggedIn && <span className="cli-tools-login-ok">已登录</span>}
        </header>
        <div className="cli-tools-login-body">
          {loginInfo === null ? (<>
            <p>该工具需要登录后才能使用。点击「发起登录」在本机启动登录流程；若页面拿到授权链接，请在浏览器打开并完成授权。</p>
            <p className="cli-tools-login-hint">提示：CodeBuddy 的登录由官方交互式授权完成，若未能自动弹出授权链接，请按页面指引在目标环境完成登录。</p>
          </>) : (<>
            {loginInfo.authUrl && <p className="cli-tools-login-url">授权：<a href={loginInfo.authUrl} target="_blank" rel="noopener noreferrer">{loginInfo.authUrl}</a></p>}
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
