import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { toast } from "sonner";
import { ConfirmDialog } from "../../components/ConfirmDialog";
import { useDocumentVisible } from "../../lib/useDocumentVisible";
import {
  filterFindingsByType,
  insightFindingCounts,
  insightLinkedTaskLabel,
  insightSeverityLabels,
  insightThemeLabels,
  insightThemes,
  insightTypeLabels,
  insightTypeOrder,
  insightVerificationLabels,
  maxInsightEventSeq,
  normalizeInsightSeverity,
  normalizeInsightTheme,
  normalizeInsightType,
  normalizeInsightVerification,
  sortFindings,
  toggleInsightType,
  type InsightEvent,
  type InsightFilter,
  type InsightFinding,
  type InsightScan,
  type InsightTheme,
  type InsightType,
  type InsightVerificationResult,
  type InsightVerificationRun,
} from "./insights-model";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type InsightAgent = "claude-code" | "codex";

// 类型图标：贴合现有 20/24 描边图标风格。
function TypeIcon({ type }: { type: InsightType }) {
  const icons: Record<InsightType, ReactNode> = {
    bug: <path d="M12 4.5a3 3 0 0 1 3 3v1a3 3 0 0 1-3 3 3 3 0 0 1-3-3v-1a3 3 0 0 1 3-3Zm-3 6.5 1.6 1H13l1.6-1M9 12.5 7 15M15 12.5 17 15M12 12.5V17" />,
    style: <><path d="M12 3.5a8.5 8.5 0 1 0 0 17h1.2a2.3 2.3 0 0 0 0-4.6H12a2.3 2.3 0 0 1 0-4.6h5.5a2.3 2.3 0 0 0 2.3-2.3A8.5 8.5 0 0 0 12 3.5Z" /><circle cx="8.5" cy="9.5" r="1" /><circle cx="12" cy="7.5" r="1" /></>,
    optimization: <path d="M12 3.5v2M3.5 12h2M18.5 12h2M12 18.5v2M5.6 5.6 7 7M17 17l1.4 1.4M5.6 18.4 7 17M17 7l1.4-1.4" />,
    feature: <path d="M12 5v14M5 12h14" />,
  };
  return <svg className="insight-type-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">{icons[type]}</svg>;
}

// 把进度事件时间戳格式化为 HH:MM:SS（日志行紧凑展示）。
function formatLogTime(ts: string): string {
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function InsightsPanel({ projectID, request, fail, onOpenFile }: {
  projectID: string;
  request: Request;
  fail: (message: string) => void;
  // 点击卡片的"查看文件"时跳转到文件页并打开该文件（未提供则只展示路径）。
  onOpenFile?: (path: string) => void;
}) {
  const [scan, setScan] = useState<InsightScan | null>(null);
  const [findings, setFindings] = useState<InsightFinding[]>([]);
  const [events, setEvents] = useState<InsightEvent[]>([]);
  const [hasScan, setHasScan] = useState(false);
  const [suppressed, setSuppressed] = useState(0);
  const [openCount, setOpenCount] = useState(0);
  const [truncated, setTruncated] = useState(false);
  const [findingsLimit, setFindingsLimit] = useState(0);
  const [filter, setFilter] = useState<InsightFilter>("all");
  const [theme, setTheme] = useState<InsightTheme>("");
  const [agent, setAgent] = useState<InsightAgent>("claude-code");
  // 默认全部分类选中：后端对"四类全勾选"会归一为空（全查），故 UI 初始即全部勾选，
  // 既符合后端语义也让"全部分类"按钮保持激活态；用户仍可逐个取消来收窄（最后一项
  // 由 toggleInsightType 拦住，见那里的说明）。
  const [focusTypes, setFocusTypes] = useState<InsightType[]>([...insightTypeOrder]);
  const [busy, setBusy] = useState(false);
  const [invalidated, setInvalidated] = useState<InsightFinding[]>([]);
  const [dismissed, setDismissed] = useState<InsightFinding[]>([]);
  const [verification, setVerification] = useState<InsightVerificationRun | null>(null);
  const [showInvalidated, setShowInvalidated] = useState(false);
  const [showDismissed, setShowDismissed] = useState(false);
  const [showRejected, setShowRejected] = useState(false);
  const [verifyAllBusy, setVerifyAllBusy] = useState(false);
  const [addAllBusy, setAddAllBusy] = useState(false);
  // 「全部添加为任务」的确认框：批量转换会一次硬删全部建议，误触成本高，需确认。
  const [confirmAddAll, setConfirmAddAll] = useState(false);
  // 验证中的建议 id 集合：驱动轮询。即使 POST 后的立即刷新失败，轮询仍会继续
  // 直到结果落库（loadInsights 会把它同步为"当前确实 pending 的 id"）。
  const [verifyInFlight, setVerifyInFlight] = useState<Set<string>>(new Set());
  const mountedRef = useRef(true);
  const agentSelectionInitializedRef = useRef(false);
  const requestVersion = useRef(0);
  const scanIdRef = useRef<string | null>(null);
  const logRef = useRef<HTMLDivElement | null>(null);
  // 已拉取到的最大事件 seq：轮询时只取增量，避免每次把整份日志重传。
  const eventSeqRef = useRef(0);

  const loadInsights = useCallback(async () => {
    const version = ++requestVersion.current;
    // 增量拉取进度事件：把「上一次拿到事件的扫描 id + 该扫描的最大 seq」一起交给服务端，
    // 由它判断游标是否属于当前这次扫描。只按 seq 过滤是不行的——客户端手上的游标可能
    // 属于上一次扫描，而新扫描的 seq 从 1 重开，会把它开头的事件永久漏掉。
    const cursorScan = scanIdRef.current;
    const sinceSeq = cursorScan !== null ? eventSeqRef.current : 0;
    const cursorQuery = cursorScan !== null && sinceSeq > 0
      ? `?sinceScan=${encodeURIComponent(cursorScan)}&sinceSeq=${sinceSeq}`
      : "";
    const res = await request<{ defaultAgent?: string; scan: InsightScan | null; findings: InsightFinding[]; events: InsightEvent[]; hasScan: boolean; suppressedCount: number; openCount: number; invalidated?: InsightFinding[]; dismissed?: InsightFinding[]; truncated?: boolean; findingsLimit?: number; verification?: InsightVerificationRun | null }>(
      `/api/projects/${projectID}/insights${cursorQuery}`,
    );
    if (!mountedRef.current || version !== requestVersion.current) return;
    setScan(res.scan);
    if (!agentSelectionInitializedRef.current) {
      agentSelectionInitializedRef.current = true;
      const initialAgent = res.scan?.agent ?? res.defaultAgent;
      if (initialAgent === "claude-code" || initialAgent === "codex") {
        setAgent(initialAgent);
      }
    }
    setFindings(res.findings);
    setInvalidated(res.invalidated ?? []);
    setDismissed(res.dismissed ?? []);
    setTruncated(res.truncated === true);
    setFindingsLimit(res.findingsLimit ?? 0);
    setVerification(res.verification ?? null);
    // 同步 in-flight 集合：只保留当前确实 pending 的建议。结果到达（valid/invalid/
    // failed）后自动清除，轮询随之停止；POST 后立即刷新也能借此拿到 pending。
    const pending = new Set<string>();
    for (const f of res.findings) if (f.verificationResult === "pending") pending.add(f.id);
    for (const f of res.invalidated ?? []) if (f.verificationResult === "pending") pending.add(f.id);
    for (const f of res.dismissed ?? []) if (f.verificationResult === "pending") pending.add(f.id);
    setVerifyInFlight(pending);
    // 进度事件：切换了扫描（scan id 变化）则整体替换，避免新旧日志混排。服务端在
    // sinceScan 与当前扫描不符时会返回全量，因此替换用的 incoming 一定是完整的。
    const scanId = res.scan?.id ?? null;
    const scanChanged = scanId !== scanIdRef.current;
    const incoming = res.events ?? [];
    scanIdRef.current = scanId;
    if (scanChanged) {
      eventSeqRef.current = maxInsightEventSeq(incoming);
      setEvents(incoming);
    } else {
      eventSeqRef.current = Math.max(eventSeqRef.current, maxInsightEventSeq(incoming));
      setEvents((prev) => {
        const known = new Set(prev.map((event) => event.id));
        const fresh = incoming.filter((event) => !known.has(event.id));
        return fresh.length > 0 ? [...prev, ...fresh] : prev;
      });
    }
    setHasScan(res.hasScan);
    setSuppressed(res.suppressedCount);
    setOpenCount(res.openCount ?? res.findings.length);
  }, [projectID, request]);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  useEffect(() => {
    agentSelectionInitializedRef.current = false;
    setAgent("claude-code");
    setFocusTypes([...insightTypeOrder]);
    // 切换项目：清掉上一个项目的事件游标与日志，避免把两个项目的进度混排。
    scanIdRef.current = null;
    eventSeqRef.current = 0;
    setEvents([]);
  }, [projectID]);

  useEffect(() => {
    void loadInsights().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载优化建议"); });
  }, [loadInsights, fail]);

  // 扫描进行中或任一建议验证中时轮询，直到完成/失败。2s 一拉让进度日志与
  // 验证结果保持流动。
  const anyVerifying =
    verification?.status === "running" ||
    verifyInFlight.size > 0 ||
    findings.some((f) => f.verificationResult === "pending") ||
    invalidated.some((f) => f.verificationResult === "pending") ||
    dismissed.some((f) => f.verificationResult === "pending");
  // 页面不可见（窗口最小化/切到后台标签页）时暂停轮询，避免空耗请求与电量；
  // 仅在从「不可见」回到「可见」的那一下补拉一次，补齐后台期间服务器侧已推进的状态。
  const documentVisible = useDocumentVisible();
  const prevDocumentVisible = useRef(documentVisible);
  useEffect(() => {
    const becameVisible = documentVisible && !prevDocumentVisible.current;
    prevDocumentVisible.current = documentVisible;
    if (!documentVisible) return;
    if (scan?.status !== "running" && !anyVerifying) return;
    if (becameVisible) void loadInsights().catch(() => undefined);
    const interval = window.setInterval(() => {
      void loadInsights().catch(() => undefined);
    }, 2_000);
    return () => window.clearInterval(interval);
  }, [documentVisible, scan?.status, anyVerifying, loadInsights]);

  // 新进度事件到达时把日志滚到底部，始终展示最新一条分析信息。
  useEffect(() => {
    if (scan?.status !== "running") return;
    const el = logRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [events.length, scan?.status]);

  const startScan = async () => {
    if (busy) return;
    setBusy(true);
    try {
      await request(`/api/projects/${projectID}/insights/scan`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ agent, theme, types: focusTypes }),
      });
      if (!mountedRef.current) return;
      // 触发后直接拉一次以立即进入 running。不在此处比较 requestVersion：翻页请求
      // 自身有版本保护，而"跳过这次刷新"会让 UI 停在没有 scan 的旧状态上——此时轮询
      // 还没启动（它由 scan.status==='running' 驱动），界面会一直不动。
      await loadInsights();
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "启动分析失败，请重试");
    } finally {
      if (mountedRef.current) setBusy(false);
    }
  };

  // 停止运行中的扫描/复核：取消通过 context 传播给后台 worker，agent 进程被终止，
  // 扫描/复核记录置为 cancelled。kind 指明停哪一种——扫描与复核现在可以同时进行
  // （见后端 insightRunScan/insightRunVerify），各自的「停止」只该停自己那个。
  // 取消是异步的，POST 后立即拉一次让状态尽快收敛。
  const cancelScan = async (kind: "scan" | "verify") => {
    if (busy) return;
    setBusy(true);
    try {
      await request(`/api/projects/${projectID}/insights/cancel`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ kind }),
      });
      if (!mountedRef.current) return;
      await loadInsights().catch(() => undefined);
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "停止分析失败，请重试");
    } finally {
      if (mountedRef.current) setBusy(false);
    }
  };

  const toggleType = (type: InsightType) => {
    setFocusTypes((prev) => toggleInsightType(prev, type));
  };

  const selectAgent = (nextAgent: InsightAgent) => {
    agentSelectionInitializedRef.current = true;
    setAgent(nextAgent);
  };

  // 验证全部有效建议：调 AI 复核每条在当前代码里是否仍然成立（项目可能已被
  // 其它任务迭代修改，或原分析因上下文限制判断不准）。异步执行，结果由轮询带回。
  // 复核与扫描互不阻塞（后端按"项目 × 种类"分别占用）：一次漫长的初扫进行中也能发起，
  // 两者共用凭据时后端会先排队等待额度，面板上仍显示"正在复核"而不是直接报错。
  const verifyAll = async () => {
    if (verifyAllBusy || busy) return;
    setVerifyAllBusy(true);
    const targets = findings.map((f) => f.id);
    // 先把全部有效建议标记为 in-flight，保证轮询启动（即使随后刷新失败）。
    if (targets.length > 0) {
      setVerifyInFlight((prev) => {
        const next = new Set(prev);
        for (const id of targets) next.add(id);
        return next;
      });
    }
    try {
      await request(`/api/projects/${projectID}/insights/verify`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ agent, findingIds: [] }),
      });
    } catch (cause) {
      // POST 失败：清除本次加入的 in-flight，避免卡片停在"验证中"。
      if (targets.length > 0) {
        setVerifyInFlight((prev) => {
          const next = new Set(prev);
          for (const id of targets) next.delete(id);
          return next;
        });
      }
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "验证启动失败，请重试");
    } finally {
      if (mountedRef.current) setVerifyAllBusy(false);
    }
    if (!mountedRef.current) return;
    // 刷新失败也保留 in-flight：轮询会持续，直到 loadInsights 同步为"实际 pending"。
    await loadInsights().catch(() => undefined);
  };

  // 一键把当前全部有效建议添加为任务：后端逐条转换并硬删建议，复核中/已失效自动跳过。
  // 返回 created/skipped/failed 汇总；刷新后已转换的建议从列表消失。
  const addAllToTasks = async () => {
    if (addAllBusy || running || busy) return;
    setAddAllBusy(true);
    try {
      const res = await request<{ created: number; skipped: number; failed: number }>(`/api/projects/${projectID}/insights/to-task`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      });
      if (res.created > 0) {
        const parts = [`已将 ${res.created} 条建议添加为任务`];
        if (res.skipped > 0) parts.push(`跳过 ${res.skipped} 条复核中的建议`);
        if (res.failed > 0) parts.push(`${res.failed} 条失败`);
        toast.success(parts.join("，"));
      } else if (res.failed > 0) {
        const parts = [`添加失败（${res.failed} 条），请重试`];
        if (res.skipped > 0) parts.unshift(`跳过 ${res.skipped} 条复核中的建议`);
        toast.error(parts.join("，"));
      } else if (res.skipped > 0) {
        toast.info(`当前没有可添加的建议，跳过 ${res.skipped} 条复核中的`);
      } else {
        toast.info("当前没有可添加为任务的建议");
      }
      setConfirmAddAll(false); // 后端已返回（含 created=0）：关闭确认框，toast 已说明结果
      await loadInsights().catch(() => undefined);
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "批量添加任务失败，请重试");
    } finally {
      if (mountedRef.current) setAddAllBusy(false);
    }
  };

  // 单个建议验证：加入 in-flight 集合保证轮询启动，POST 成功后立即刷新拿到 pending。
  const verifyFinding = async (id: string) => {
    if (verifyInFlight.has(id)) return;
    setVerifyInFlight((prev) => new Set(prev).add(id));
    try {
      await request(`/api/projects/${projectID}/insights/verify`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ agent, findingIds: [id] }),
      });
    } catch (cause) {
      // POST 失败：立即清除 in-flight，避免卡片停在"验证中"。
      setVerifyInFlight((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "验证启动失败，请重试");
      return;
    }
    if (!mountedRef.current) return;
    await loadInsights().catch(() => undefined);
  };

  // 「不再提示」/「恢复」：用户对某条建议的显式处置。标记后该建议移入折叠区，且后续
  // 扫描不再上报它（与"删除"不同：删除是硬删，下次扫描会重新报告）。
  const setDismissedState = async (id: string, nextDismissed: boolean) => {
    try {
      await request(`/api/projects/${projectID}/insights/${id}/dismiss`, {
        method: nextDismissed ? "POST" : "DELETE",
        headers: { "Content-Type": "application/json" },
        body: nextDismissed ? "{}" : undefined,
      });
      if (!mountedRef.current) return;
      await loadInsights().catch(() => undefined);
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "操作失败，请重试");
    }
  };
  // 空 = 全查（后端语义），故 "全部分类" 是一个单向"全选"动作而非可切换的开关——
  // 不存在有意义的"关掉=不查"状态（清空仍等价于全查）。全选后 user 仍可逐个取消来收窄。
  const allTypesSelected = focusTypes.length === insightTypeOrder.length;
  const selectAllTypes = () => setFocusTypes([...insightTypeOrder]);

  const running = scan?.status === "running";
  const failed = scan?.status === "failed";
  const counts = insightFindingCounts(findings);
  const sorted = sortFindings(findings);
  const visible = filterFindingsByType(sorted, filter);
  // 上次扫描的聚焦方向（用于结果汇总行展示）。
  const lastTheme = normalizeInsightTheme(scan?.theme);
  const lastFocus = (scan?.focusTypes ?? []).map(normalizeInsightType);
  const focusLabel = lastTheme !== "" || lastFocus.length > 0
    ? `本次聚焦：${lastTheme !== "" ? insightThemeLabels[lastTheme] : "全面"}${lastFocus.length > 0 ? ` · ${lastFocus.map((t) => insightTypeLabels[t]).join("、")}` : ""}`
    : null;

  return (
    <div className="insights">
      <header className="insights-head">
        <div className="insights-heading">
          <h2>优化建议</h2>
          <p>AI 主动通读项目，找出可优化点、可新增功能、已有 bug 与样式问题；每条在报告时已独立核实，并可随时对既有建议再验证，确认其在当前代码里是否仍然成立。</p>
        </div>
        {running ? (
          <button
            type="button"
            className="insight-stop"
            disabled={busy}
            onClick={() => void cancelScan("scan")}
            title="停止本次分析，已收集的结果不会保留，可随时重新发起"
          >
            {busy ? "正在停止…" : "停止分析"}
          </button>
        ) : (
          <button type="button" className="primary" disabled={busy} onClick={() => void startScan()}>
            开始分析
          </button>
        )}
      </header>

      {!running && (
        <section className="insights-pickers" aria-label="选择分析方向">
          <div className="insights-picker">
            <span className="insights-picker-label">分析引擎</span>
            <div className="insights-agent-options" role="radiogroup" aria-label="选择分析引擎">
              <label className={agent === "claude-code" ? "active" : ""}>
                <input type="radio" name="insight-agent" value="claude-code" checked={agent === "claude-code"} disabled={anyVerifying} onChange={() => selectAgent("claude-code")} />
                Claude Code
              </label>
              <label className={agent === "codex" ? "active" : ""}>
                <input type="radio" name="insight-agent" value="codex" checked={agent === "codex"} disabled={anyVerifying} onChange={() => selectAgent("codex")} />
                Codex
              </label>
            </div>
          </div>
          <div className="insights-picker">
            <span className="insights-picker-label">聚焦主题</span>
            <div className="insights-theme-chips" role="radiogroup" aria-label="聚焦主题">
              <button
                type="button"
                className={theme === "" ? "active" : ""}
                onClick={() => setTheme("")}
              >
                全面分析
              </button>
              {insightThemes.map((th) => (
                <button
                  key={th}
                  type="button"
                  className={theme === th ? "active" : ""}
                  onClick={() => setTheme(theme === th ? "" : th)}
                >
                  {insightThemeLabels[th]}
                </button>
              ))}
            </div>
          </div>
          <div className="insights-picker">
            <span className="insights-picker-label">查找类型</span>
            <div className="insights-type-options">
              <button
                type="button"
                className={`insights-select-all${allTypesSelected ? " active" : ""}`}
                onClick={selectAllTypes}
                disabled={allTypesSelected}
              >
                全部分类
              </button>
              {insightTypeOrder.map((type) => (
                <label key={type} className="insights-type-option">
                  <input type="checkbox" checked={focusTypes.includes(type)} onChange={() => toggleType(type)} />
                  {insightTypeLabels[type]}
                </label>
              ))}
            </div>
          </div>
        </section>
      )}

      {!hasScan && !running && (
        <section className="insights-empty">
          <p>还没有分析记录。点击「开始分析」，AI 会主动审视项目并列出值得改进的地方（只读、不改动你的代码）。</p>
        </section>
      )}

      {running && (
        <section className="insights-progress" role="status">
          <div className="insights-progress-head">
            <span className="insights-spinner" aria-hidden="true" />
            <div className="insights-progress-copy">
              <b>正在分析项目（含逐项核实），项目较大时可能需要十几分钟或更久…</b>
              <small>已开始：{scan?.startedAt ? new Date(scan.startedAt).toLocaleString() : ""}</small>
            </div>
          </div>
          <div className="insights-progress-log" ref={logRef} aria-label="分析进度">
            {events.length === 0 ? (
              <span className="insights-progress-idle">正在准备分析器…</span>
            ) : (
              events.map((event) => (
                <span key={event.id} className={`insights-log-line insight-log-${event.level}`}>
                  <i aria-hidden="true" />
                  <small>{formatLogTime(event.ts)}</small>
                  <span>{event.message}</span>
                </span>
              ))
            )}
          </div>
        </section>
      )}

      {/* 复核可与扫描同时进行：两者各有进度块，扫描进行中也要显示复核进度，
          否则用户会以为复核没在跑（正是"边扫边核"场景的关键反馈）。 */}
      {verification?.status === "running" && (() => {
        const verifyPct = verification.totalCount > 0
          ? Math.min(100, Math.round((verification.processedCount / verification.totalCount) * 100))
          : 0;
        return (
          <section className="insights-verification-progress" role="status">
            <span className="insights-spinner" aria-hidden="true" />
            <div className="insights-verification-copy">
              <b>正在复核建议 <em>{verification.processedCount}/{verification.totalCount}</em></b>
              <small>{verification.message || "正在读取当前项目代码"}</small>
              <span className="insights-verification-track" aria-hidden="true"><i style={{ width: `${verifyPct}%` }} /></span>
            </div>
            <button
              type="button"
              className="insight-stop"
              disabled={busy}
              onClick={() => void cancelScan("verify")}
              title="停止本次复核，结果不会保留"
            >
              停止
            </button>
          </section>
        );
      })()}

      {failed && scan?.error && (
        <section className="insights-failed" role="alert">
          <b>分析未完成</b>
          <span>{scan.error}</span>
        </section>
      )}

      {scan?.status === "cancelled" && (
        <section className="insights-failed cancelled" role="status">
          <b>分析已取消</b>
          <span>已停止本次分析，未产生新建议；可随时重新发起。</span>
        </section>
      )}

      {/* 扫描进行中同样展示既有建议列表：复核与扫描可以并行，而"验证"入口就在卡片上，
          把列表藏起来会让这次能力在实际界面上到不了手。 */}
      {hasScan && !failed && (
        <>
          <div className="insights-summary">
            <span>共 {openCount} 条有效建议</span>
            {scan && scan.findingsCount > 0 && <span className="insights-new">本次新增 {scan.findingsCount} 条</span>}
            {focusLabel && <span className="insights-focus">{focusLabel}</span>}
            {suppressed > 0 && <span className="insights-suppressed">已忽略 {suppressed} 条此前报告过的建议</span>}
            {scan && scan.rejected && scan.rejected.length > 0 && (
              <button
                type="button"
                className={`insights-rejected-toggle${showRejected ? " active" : ""}`}
                onClick={() => setShowRejected((v) => !v)}
                title="查看本次分析在第 2 轮独立核实中被剔除的候选及其原因"
              >
                核实剔除 {scan.rejected.length} 条
              </button>
            )}
            <span className="insights-summary-actions">
              <button
                type="button"
                className="insight-add-all"
                onClick={() => setConfirmAddAll(true)}
                disabled={running || busy || addAllBusy || openCount === 0}
                title="把当前全部有效建议一键添加为任务（复核中/已失效/已忽略的自动跳过）"
              >
                {addAllBusy ? "正在添加…" : "全部添加为任务"}
              </button>
              <button
                type="button"
                className="insight-verify-all"
                onClick={() => void verifyAll()}
                disabled={busy || verifyAllBusy || anyVerifying || openCount === 0}
                title="调用 AI 复核当前全部有效建议是否仍然成立（项目可能已被其它任务迭代修改；分析进行中也可以发起，两者共用凭据时后端会先排队等额度）"
              >
                {verifyAllBusy || anyVerifying ? "正在验证…" : "验证全部"}
              </button>
              {invalidated.length > 0 && (
                <button
                  type="button"
                  className={`insight-invalidated-toggle${showInvalidated ? " active" : ""}`}
                  onClick={() => setShowInvalidated((v) => !v)}
                  title="查看经验证已失效、从有效列表隐藏的建议"
                >
                  已失效 {invalidated.length} 条
                </button>
              )}
              {dismissed.length > 0 && (
                <button
                  type="button"
                  className={`insight-dismissed-toggle${showDismissed ? " active" : ""}`}
                  onClick={() => setShowDismissed((v) => !v)}
                  title="查看已设为「不再提示」的建议（这些建议不会被后续分析再次上报）"
                >
                  已忽略 {dismissed.length} 条
                </button>
              )}
            </span>
          </div>

          {truncated && (
            <section className="insights-notice" role="status">
              建议数量较多，仅显示前 {findingsLimit || openCount} 条；处理掉一部分后即可看到其余建议。
            </section>
          )}

          {scan && scan.rejected && scan.rejected.length > 0 && showRejected && (
            <section className="insights-rejected" aria-label="被核实剔除的候选">
              <div className="insights-rejected-head">
                <b>第 2 轮核实剔除的候选</b>
                <small>这些候选在独立核实中被判定为不成立（伪需求 / 假 bug / 项目已具备），未写入建议列表。若认为判断有误，可点「开始分析」重新分析或在编辑后自行添加。</small>
              </div>
              <ul>
                {scan.rejected.map((item, index) => (
                  <li key={`${item.title}-${index}`}>
                    <b>{item.title}</b>
                    {item.reason ? <span>{item.reason}</span> : <span className="muted">（未给出原因）</span>}
                  </li>
                ))}
              </ul>
            </section>
          )}

          <div className="insights-filters" role="tablist" aria-label="按类型筛选">
            <button type="button" className={filter === "all" ? "active" : ""} onClick={() => setFilter("all")}>全部 <small>{counts.all}</small></button>
            {insightTypeOrder.map((type) => (
              <button key={type} type="button" className={filter === type ? "active" : ""} onClick={() => setFilter(type)}>
                {insightTypeLabels[type]} <small>{counts[type]}</small>
              </button>
            ))}
          </div>

          <ul className="insights-list">
            {visible.map((finding) => (
              <InsightsFindingCard
                key={finding.id}
                finding={finding}
                projectID={projectID}
                request={request}
                onDeleted={() => void loadInsights()}
                onDismissed={(next) => void setDismissedState(finding.id, next)}
                fail={fail}
                verifying={verifyInFlight.has(finding.id)}
                onVerify={() => void verifyFinding(finding.id)}
                onOpenFile={onOpenFile}
              />
            ))}
            {visible.length === 0 && <li className="insights-none">当前筛选下没有发现。</li>}
          </ul>

          {invalidated.length > 0 && showInvalidated && (
            <section className="insights-invalidated" aria-label="已失效建议">
              <div className="insights-invalidated-head">
                <b>已失效建议</b>
                <small>AI 复核确认这些建议在当前代码里已不成立（问题已修复 / 功能已实现 / 伪建议）。如需恢复可重新验证或删除。</small>
              </div>
              <ul className="insights-list">
                {invalidated.map((finding) => (
                  <InsightsFindingCard
                    key={finding.id}
                    finding={finding}
                    projectID={projectID}
                    request={request}
                    onDeleted={() => void loadInsights()}
                    onDismissed={(next) => void setDismissedState(finding.id, next)}
                    fail={fail}
                    invalidated
                    verifying={verifyInFlight.has(finding.id)}
                    onVerify={() => void verifyFinding(finding.id)}
                    onOpenFile={onOpenFile}
                  />
                ))}
              </ul>
            </section>
          )}

          {dismissed.length > 0 && showDismissed && (
            <section className="insights-dismissed" aria-label="已忽略建议">
              <div className="insights-dismissed-head">
                <b>已设为不再提示的建议</b>
                <small>这些建议不会再出现在后续分析的结果里（分析器不会再报告同一问题）。恢复后会重新进入有效列表。</small>
              </div>
              <ul className="insights-list">
                {dismissed.map((finding) => (
                  <InsightsFindingCard
                    key={finding.id}
                    finding={finding}
                    projectID={projectID}
                    request={request}
                    onDeleted={() => void loadInsights()}
                    onDismissed={(next) => void setDismissedState(finding.id, next)}
                    fail={fail}
                    dismissed
                    verifying={verifyInFlight.has(finding.id)}
                    onVerify={() => void verifyFinding(finding.id)}
                    onOpenFile={onOpenFile}
                  />
                ))}
              </ul>
            </section>
          )}
        </>
      )}

      {confirmAddAll && createPortal(
        <ConfirmDialog
          title="全部添加为任务"
          message={<>确定把当前全部 <b>{openCount}</b> 条有效建议一次性添加为任务？添加后这些建议会从列表消失（可在任务板查看），复核中的会自动跳过。</>}
          confirmLabel="全部添加"
          busy={addAllBusy}
          onConfirm={() => void addAllToTasks()}
          onCancel={() => setConfirmAddAll(false)}
        />,
        document.body,
      )}
    </div>
  );
}

function ActionIcon({ name }: { name: "edit" | "delete" | "task" }) {
  if (name === "edit") return <path d="m14.5 5 2-2a1.4 1.4 0 0 1 2 0l2 2a1.4 1.4 0 0 1 0 2l-9.3 9.3-3.6.6.6-3.6 9.3-9.3Z" />;
  if (name === "delete") return <path d="M5 6.5h14M9 6.5V4.5h6v2M6.5 6.5l.7 13h9.6l.7-13M10 10v6M14 10v6" />;
  return <><path d="M8.5 6.5h10M8.5 12h10M8.5 17.5h10" /><path d="m4.7 6.5.9.9 1.8-2M4.7 12l.9.9 1.8-2M4.7 17.5l.9.9 1.8-2" /></>;
}

// 添加到任务图标（清单样式）：把建议转为任务，经任务下发执行。
function AddTaskIcon() {
  return <svg className="insight-action-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><ActionIcon name="task" /></svg>;
}

// 验证图标（盾牌 + 勾）：卡片「验证」动作按钮。
function VerifyIcon() {
  return (
    <svg className="insight-action-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 3.5 5.5 6v5c0 4.2 2.6 7.6 6.5 9.5 3.9-1.9 6.5-5.3 6.5-9.5V6L12 3.5Z" />
      <path d="m9 12 2 2 4-4" />
    </svg>
  );
}

// 验证中的内联旋转小圆点（按钮/徽章复用）。
function InsightVerifySpinner() {
  return <i className="insight-verify-spin" aria-hidden="true" />;
}

// 把验证时间格式化为 MM-DD HH:MM:SS（紧凑展示）。
function formatVerifyTime(ts: string): string {
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${formatLogTime(ts)}`;
}

// 卡片上的验证状态徽章：仍存在 / 已失效 / 验证失败 / 验证中；未验证不渲染。
function InsightVerifyBadge({ result, note, verifiedAt }: {
  result: InsightVerificationResult;
  note?: string;
  verifiedAt?: string | null;
}) {
  if (result === "pending") {
    return <span className="insight-verify-badge pending" title="AI 正在复核此建议…"><InsightVerifySpinner />{insightVerificationLabels.pending}</span>;
  }
  if (result === "valid") {
    const when = verifiedAt ? ` · ${formatVerifyTime(verifiedAt)}` : "";
    return <span className="insight-verify-badge valid" title={note || "AI 复核确认该建议仍成立"}>✓ {insightVerificationLabels.valid}{when}</span>;
  }
  if (result === "invalid") {
    return <span className="insight-verify-badge invalid">✗ {insightVerificationLabels.invalid}</span>;
  }
  if (result === "failed") {
    return <span className="insight-verify-badge failed" title={note || "验证失败，可重试"}>✗ {insightVerificationLabels.failed}</span>;
  }
  return null;
}

function InsightsFindingCard({ finding, projectID, request, onDeleted, onDismissed, fail, invalidated = false, dismissed = false, verifying = false, onVerify, onOpenFile }: {
  finding: InsightFinding;
  projectID: string;
  request: Request;
  onDeleted: () => void;
  // 切换「不再提示」状态（true=设为不再提示，false=恢复）。
  onDismissed: (nextDismissed: boolean) => void;
  fail: (message: string) => void;
  invalidated?: boolean;
  dismissed?: boolean;
  // 面板级 in-flight 标记：POST 后立即为 true，轮询拿到 pending 后仍为 true，
  // 结果落库（loadInsights 同步 verifyInFlight）后变回 false。
  verifying?: boolean;
  onVerify?: () => void;
  onOpenFile?: (path: string) => void;
}) {
  const type = normalizeInsightType(finding.type);
  const severity = normalizeInsightSeverity(finding.severity);
  const verificationResult = normalizeInsightVerification(finding.verificationResult);
  const isVerifying = verifying || verificationResult === "pending";
  const linkedLabel = insightLinkedTaskLabel(finding);
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [draft, setDraft] = useState({ title: finding.title, summary: finding.summary, severity, type });
  const [saving, setSaving] = useState(false);
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const beginEdit = () => {
    setDraft({ title: finding.title, summary: finding.summary, severity, type });
    setEditing(true);
  };

  const saveEdit = async () => {
    const title = draft.title.trim();
    if (!title) {
      fail("标题不能为空");
      return;
    }
    setSaving(true);
    try {
      await request(`/api/projects/${projectID}/insights/${finding.id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, summary: draft.summary.trim(), severity: draft.severity, type: draft.type }),
      });
      setEditing(false);
      onDeleted(); // 触发父级 loadInsights 刷新
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "保存失败，请重试");
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    setSaving(true);
    try {
      await request(`/api/projects/${projectID}/insights/${finding.id}`, { method: "DELETE" });
      setConfirming(false);
      onDeleted();
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "删除失败，请重试");
    } finally {
      setSaving(false);
    }
  };

  // 把建议转为任务：后端在同一事务里创建任务并硬删该建议，刷新后从列表消失。
  // 【规则 · 勿改】转任务后必须硬删（而非软删/标记已处理/仅隐藏）：下次扫描会重新
  // 报告同一问题（指纹消失），因为任务执行完之前问题可能仍然存在。改动这里会破坏
  // "问题可被再次发现"的语义。后端对应注释见 insights.go convertInsightToTask。
  // 同一问题已有未完成任务时后端会拒绝（409），避免反复堆重复任务。
  const addToTask = async () => {
    if (saving) return;
    setSaving(true);
    try {
      await request(`/api/projects/${projectID}/insights/${finding.id}/to-task`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      });
      toast.success("已添加到任务");
      onDeleted(); // 触发父级 loadInsights 刷新：建议已被删除，不再展示
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "添加到任务失败，请重试");
    } finally {
      if (mountedRef.current) setSaving(false);
    }
  };

  if (editing) {
    return (
      <li className={`insight-card editing`}>
        <span className={`insight-card-type insight-type-${type}`}><TypeIcon type={type} />{insightTypeLabels[type]}</span>
        <label className="insight-edit-field">
          <span>标题</span>
          <input autoFocus value={draft.title} onChange={(e) => setDraft((d) => ({ ...d, title: e.target.value }))} placeholder="一句话标题" />
        </label>
        <label className="insight-edit-field">
          <span>说明</span>
          <textarea value={draft.summary} onChange={(e) => setDraft((d) => ({ ...d, summary: e.target.value }))} rows={3} placeholder="面向用户的两三句说明" />
        </label>
        <label className="insight-edit-field insight-edit-severity">
          <span>类型</span>
          <select value={draft.type} onChange={(e) => setDraft((d) => ({ ...d, type: e.target.value as InsightType }))}>
            {insightTypeOrder.map((t) => <option key={t} value={t}>{insightTypeLabels[t]}</option>)}
          </select>
        </label>
        <label className="insight-edit-field insight-edit-severity">
          <span>严重度</span>
          <select value={draft.severity} onChange={(e) => setDraft((d) => ({ ...d, severity: e.target.value as InsightFinding["severity"] }))}>
            <option value="high">高</option>
            <option value="normal">普通</option>
            <option value="low">低</option>
          </select>
        </label>
        <div className="insight-card-actions">
          <button type="button" className="secondary" disabled={saving} onClick={() => setEditing(false)}>取消</button>
          <button type="button" className="primary" disabled={saving} onClick={() => void saveEdit()}>{saving ? "保存中" : "保存"}</button>
        </div>
      </li>
    );
  }

  return (
    <li className={`insight-card${invalidated ? " invalidated" : ""}${dismissed ? " dismissed" : ""}`}>
      <span className={`insight-card-type insight-type-${type}`}><TypeIcon type={type} />{insightTypeLabels[type]}</span>
      <b>{finding.title}</b>
      <p>{finding.summary}</p>
      {/* AI 验证的判断依据：仍存在/失效/失败的说明。失败 note 已自带失败语义，不再加前缀。 */}
      {(verificationResult === "valid" || verificationResult === "failed") && finding.verificationNote && (
        <p className={`insight-verify-reason${verificationResult === "failed" ? " failed" : ""}`}>
          {verificationResult === "valid" ? "复核：" : ""}{finding.verificationNote}
        </p>
      )}
      {invalidated && finding.verificationNote && (
        <p className="insight-verify-reason invalidated">失效原因：{finding.verificationNote}</p>
      )}
      <div className="insight-card-foot">
        <span className={`insight-severity insight-severity-${severity}`}>{insightSeverityLabels[severity]}</span>
        {finding.fileHint && (
          onOpenFile
            ? <button type="button" className="insight-file link" title={`打开 ${finding.fileHint}`} onClick={() => onOpenFile(finding.fileHint!)}>{finding.fileHint}</button>
            : <span className="insight-file" title={finding.fileHint}>{finding.fileHint}</span>
        )}
        {linkedLabel && <span className={`insight-linked-task${finding.linkedTaskStatus === "done" || finding.linkedTaskStatus === "cancelled" ? " terminal" : ""}`} title={finding.linkedTaskTitle || undefined}>{linkedLabel}</span>}
        <InsightVerifyBadge result={verificationResult} note={finding.verificationNote} verifiedAt={finding.verifiedAt} />
      </div>
      <div className="insight-card-actions">
        {!dismissed && (
          <button
            type="button"
            className="insight-action"
            onClick={onVerify}
            disabled={isVerifying || saving}
            title="调用 AI 复核此建议在当前代码里是否仍然成立（项目可能已被其它任务迭代修改）"
          >
            {isVerifying ? <InsightVerifySpinner /> : <VerifyIcon />}
            {isVerifying ? "验证中…" : verificationResult !== "" || invalidated ? "重新验证" : "验证"}
          </button>
        )}
        {!invalidated && !dismissed && (
          <button type="button" className="insight-action" onClick={() => void addToTask()} disabled={saving || isVerifying} title="添加到任务，通过任务下发执行（复核中暂不可转）"><AddTaskIcon />添加到任务</button>
        )}
        {!invalidated && (
          <button type="button" className="insight-action" onClick={beginEdit} disabled={saving} title="编辑此建议">编辑</button>
        )}
        {dismissed ? (
          <button type="button" className="insight-action" onClick={() => onDismissed(false)} disabled={saving} title="恢复此建议，让它重新进入有效列表">恢复</button>
        ) : (
          <button type="button" className="insight-action" onClick={() => onDismissed(true)} disabled={saving || isVerifying} title="不再提示：此建议移入已忽略列表，后续分析也不再上报同一问题">不再提示</button>
        )}
        <button type="button" className="insight-action danger" onClick={() => setConfirming(true)} disabled={saving} title="删除此建议">删除</button>
      </div>
      {confirming && createPortal(
        <ConfirmDialog
          title="删除建议"
          message={<>确定删除"<b>{finding.title}</b>"？删除后无法恢复，下次扫描该问题可能再次出现。若只是不想再看到它，请改用「不再提示」。</>}
          confirmLabel="删除"
          danger
          busy={saving}
          onConfirm={() => void remove()}
          onCancel={() => setConfirming(false)}
        />,
        document.body,
      )}
    </li>
  );
}
