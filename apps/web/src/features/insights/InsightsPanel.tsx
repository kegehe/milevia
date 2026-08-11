import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { ConfirmDialog } from "../../components/ConfirmDialog";
import {
  buildInsightPrompt,
  filterFindingsByType,
  insightFindingCounts,
  insightSeverityLabels,
  insightThemeLabels,
  insightThemes,
  insightTypeLabels,
  insightTypeOrder,
  normalizeInsightSeverity,
  normalizeInsightTheme,
  normalizeInsightType,
  sortFindings,
  type InsightFilter,
  type InsightFinding,
  type InsightScan,
  type InsightTheme,
  type InsightType,
} from "./insights-model";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;

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

export function InsightsPanel({ projectID, request, fail }: {
  projectID: string;
  request: Request;
  fail: (message: string) => void;
}) {
  const [scan, setScan] = useState<InsightScan | null>(null);
  const [findings, setFindings] = useState<InsightFinding[]>([]);
  const [hasScan, setHasScan] = useState(false);
  const [suppressed, setSuppressed] = useState(0);
  const [openCount, setOpenCount] = useState(0);
  const [filter, setFilter] = useState<InsightFilter>("all");
  const [theme, setTheme] = useState<InsightTheme>("");
  const [focusTypes, setFocusTypes] = useState<InsightType[]>([]);
  const [busy, setBusy] = useState(false);
  const mountedRef = useRef(true);
  const requestVersion = useRef(0);

  const loadInsights = useCallback(async () => {
    const version = ++requestVersion.current;
    const res = await request<{ scan: InsightScan | null; findings: InsightFinding[]; hasScan: boolean; suppressedCount: number; openCount: number }>(
      `/api/projects/${projectID}/insights`,
    );
    if (!mountedRef.current || version !== requestVersion.current) return;
    setScan(res.scan);
    setFindings(res.findings);
    setHasScan(res.hasScan);
    setSuppressed(res.suppressedCount);
    setOpenCount(res.openCount ?? res.findings.length);
  }, [projectID, request]);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  useEffect(() => {
    void loadInsights().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载优化建议"); });
  }, [loadInsights, fail]);

  // 扫描进行中时轮询，直到完成/失败。
  useEffect(() => {
    if (scan?.status !== "running") return;
    const interval = window.setInterval(() => {
      void loadInsights().catch(() => undefined);
    }, 5_000);
    return () => window.clearInterval(interval);
  }, [scan?.status, loadInsights]);

  const startScan = async () => {
    if (busy) return;
    setBusy(true);
    const version = ++requestVersion.current;
    try {
      await request(`/api/projects/${projectID}/insights/scan`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ theme, types: focusTypes }),
      });
      if (!mountedRef.current || version !== requestVersion.current) return;
      // 触发后直接拉一次以立即进入 running。
      await loadInsights();
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "启动分析失败，请重试");
    } finally {
      if (mountedRef.current) setBusy(false);
    }
  };

  const toggleType = (type: InsightType) => {
    setFocusTypes((prev) => (prev.includes(type) ? prev.filter((t) => t !== type) : [...prev, type]));
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
          <p>AI 主动通读项目，找出可优化点、可新增功能、已有 bug 与样式问题；并对每条做了独立核实，只展示确认存在的内容。</p>
        </div>
        <button type="button" className="primary" disabled={running || busy} onClick={() => void startScan()}>
          {running ? "正在分析…" : "开始分析"}
        </button>
      </header>

      {!running && (
        <section className="insights-pickers" aria-label="选择分析方向">
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
        </section>
      )}

      {!hasScan && !running && (
        <section className="insights-empty">
          <p>还没有分析记录。点击「开始分析」，AI 会主动审视项目并列出值得改进的地方（只读、不改动你的代码）。</p>
        </section>
      )}

      {running && (
        <section className="insights-progress" role="status">
          <span className="insights-spinner" aria-hidden="true" />
          <div>
            <b>正在分析项目（含逐项核实），通常需要几分钟…</b>
            <small>已开始：{scan?.startedAt ? new Date(scan.startedAt).toLocaleString() : ""}</small>
          </div>
        </section>
      )}

      {failed && scan?.error && (
        <section className="insights-failed" role="alert">
          <b>分析未完成</b>
          <span>{scan.error}</span>
        </section>
      )}

      {hasScan && !running && !failed && (
        <>
          <div className="insights-summary">
            <span>共 {openCount} 条有效建议</span>
            {scan && scan.findingsCount > 0 && <span className="insights-new">本次新增 {scan.findingsCount} 条</span>}
            {focusLabel && <span className="insights-focus">{focusLabel}</span>}
            {suppressed > 0 && <span className="insights-suppressed">已忽略 {suppressed} 条此前报告过的建议</span>}
          </div>

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
              <InsightsFindingCard key={finding.id} finding={finding} projectID={projectID} request={request} onDeleted={() => void loadInsights()} fail={fail} />
            ))}
            {visible.length === 0 && <li className="insights-none">当前筛选下没有发现。</li>}
          </ul>
        </>
      )}
    </div>
  );
}

// 把一条建议发送到对话页输入框的跨页通道（复用 addFile 模式：sessionStorage + URL param）。
const SEND_TO_CHAT_KEY = "milevia_insight_prompt";

function ActionIcon({ name }: { name: "edit" | "delete" | "send" }) {
  if (name === "edit") return <path d="m14.5 5 2-2a1.4 1.4 0 0 1 2 0l2 2a1.4 1.4 0 0 1 0 2l-9.3 9.3-3.6.6.6-3.6 9.3-9.3Z" />;
  if (name === "delete") return <path d="M5 6.5h14M9 6.5V4.5h6v2M6.5 6.5l.7 13h9.6l.7-13M10 10v6M14 10v6" />;
  return <><path d="m4.5 4.5 15 7.2-6.6 2.1-2.1 6.7-6.3-16Z" /><path d="m12.9 13.8 3-3" /></>;
}

function SendIcon() {
  return <svg className="insight-action-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><ActionIcon name="send" /></svg>;
}

function InsightsFindingCard({ finding, projectID, request, onDeleted, fail }: {
  finding: InsightFinding;
  projectID: string;
  request: Request;
  onDeleted: () => void;
  fail: (message: string) => void;
}) {
  const type = normalizeInsightType(finding.type);
  const severity = normalizeInsightSeverity(finding.severity);
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [draft, setDraft] = useState({ title: finding.title, summary: finding.summary, severity });
  const [saving, setSaving] = useState(false);

  const beginEdit = () => {
    setDraft({ title: finding.title, summary: finding.summary, severity });
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
        body: JSON.stringify({ title, summary: draft.summary.trim(), severity: draft.severity }),
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

  const sendToChat = () => {
    try {
      sessionStorage.setItem(SEND_TO_CHAT_KEY, buildInsightPrompt(finding));
    } catch {
      fail("无法写入剪贴板缓存，请重试");
      return;
    }
    // 复用 addFile 通道：跳转到对话页，附带 addInsight 参数，由 ConversationPage 消费追加到输入框。
    window.location.href = `/projects/${projectID}/conversations?addInsight=true`;
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
    <li className="insight-card">
      <span className={`insight-card-type insight-type-${type}`}><TypeIcon type={type} />{insightTypeLabels[type]}</span>
      <b>{finding.title}</b>
      <p>{finding.summary}</p>
      <div className="insight-card-foot">
        <span className={`insight-severity insight-severity-${severity}`}>{insightSeverityLabels[severity]}</span>
        {finding.fileHint && <span className="insight-file" title={finding.fileHint}>{finding.fileHint}</span>}
      </div>
      <div className="insight-card-actions">
        <button type="button" className="insight-action" onClick={sendToChat} title="发送到对话页输入框，快速发起修复/功能添加"><SendIcon />发送到对话</button>
        <button type="button" className="insight-action" onClick={beginEdit} title="编辑此建议">编辑</button>
        <button type="button" className="insight-action danger" onClick={() => setConfirming(true)} title="删除此建议">删除</button>
      </div>
      {confirming && createPortal(
        <ConfirmDialog
          title="删除建议"
          message={<>确定删除"<b>{finding.title}</b>"？删除后无法恢复，下次扫描该问题可能再次出现。</>}
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
