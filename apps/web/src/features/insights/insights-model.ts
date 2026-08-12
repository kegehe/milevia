// 项目主动优化建议（Insights）前端模型 —— 见 docs/25-项目主动优化建议实现方案.md
// 纯函数与类型集中在 model 里便于单测，组件只负责渲染与交互。

export type InsightType = "bug" | "style" | "optimization" | "feature";
export type InsightSeverity = "low" | "normal" | "high";
export type InsightScanStatus = "running" | "completed" | "failed" | "cancelled";

// 主题方向（空串 = 全面分析）。
export type InsightTheme = "security" | "performance" | "ux" | "architecture" | "stability" | "";

export type InsightScan = {
  id: string;
  projectId: string;
  status: InsightScanStatus;
  error?: string;
  agent?: string;
  theme?: string;
  focusTypes?: string[];
  findingsCount: number;
  suppressedCount: number;
  createdAt: string;
  startedAt?: string | null;
  completedAt?: string | null;
};

export type InsightFinding = {
  id: string;
  projectId: string;
  scanId: string;
  type: InsightType;
  severity: InsightSeverity;
  title: string;
  summary: string;
  fileHint?: string;
  status: string;
  createdAt: string;
  // 建议再验证（re-verify）落库状态：''=未验证 | pending=验证中 | valid=确认仍存在 |
  // invalid=已失效（从有效列表隐藏） | failed=验证失败（可重试）。
  verificationResult?: InsightVerificationResult;
  verificationNote?: string;
  verifiedAt?: string | null;
};

// 建议再验证的结果状态，与后端 insightVerify* 常量一一对应。
export type InsightVerificationResult = "" | "pending" | "valid" | "invalid" | "failed";

export type InsightVerificationRun = {
  id: string;
  status: "running" | "completed" | "failed" | "cancelled";
  error?: string;
  message?: string;
  totalCount: number;
  processedCount: number;
  createdAt: string;
  startedAt?: string | null;
  completedAt?: string | null;
};

export type InsightsResponse = {
  scan: InsightScan | null;
  findings: InsightFinding[];
  events: InsightEvent[];
  hasScan: boolean;
  suppressedCount: number;
  openCount: number;
  verification?: InsightVerificationRun | null;
  // 经验证已失效、从有效列表隐藏的建议（折叠展示，含 AI 判断依据）。
  invalidated?: InsightFinding[];
};

// 一条分析过程中的进度消息（"分析信息"滚动展示）。level 与后端一一对应。
export type InsightEventLevel = "info" | "success" | "warn" | "error";
export type InsightEvent = {
  id: string;
  seq: number;
  ts: string;
  level: InsightEventLevel;
  message: string;
};

export const insightTypeLabels: Record<InsightType, string> = {
  bug: "有 bug",
  style: "样式问题",
  optimization: "可优化项",
  feature: "可新增功能",
};

export const insightSeverityLabels: Record<InsightSeverity, string> = {
  low: "低",
  normal: "普通",
  high: "高",
};

export const insightTypeOrder: InsightType[] = ["bug", "style", "optimization", "feature"];

// 扫描可选择聚焦的主题方向（暂不含"全面分析"空串值，以便 UI 单独当作"全部"渲染）。
export const insightThemes: InsightTheme[] = ["security", "performance", "ux", "architecture", "stability"];

export const insightThemeLabels: Record<InsightTheme, string> = {
  security: "安全",
  performance: "性能",
  ux: "UX",
  architecture: "架构",
  stability: "稳定性",
  "": "全面分析",
};

/** 把后端任意字符串归一为合法主题；非法/空 → ""（全面分析）。 */
export function normalizeInsightTheme(value?: string): InsightTheme {
  if (value === "security" || value === "performance" || value === "ux" || value === "architecture" || value === "stability" || value === "") {
    return value;
  }
  return "";
}

const severityRank: Record<InsightSeverity, number> = { high: 0, normal: 1, low: 2 };

export function isInsightType(value: string): value is InsightType {
  return (insightTypeOrder as string[]).includes(value);
}

function isInsightSeverity(value: string): value is InsightSeverity {
  return value === "high" || value === "normal" || value === "low";
}

/** 把后端任意字符串归一为合法类型/严重度。 */
export function normalizeInsightType(value?: string): InsightType {
  return isInsightType(value ?? "") ? (value as InsightType) : "optimization";
}
export function normalizeInsightSeverity(value?: string): InsightSeverity {
  return isInsightSeverity(value ?? "") ? (value as InsightSeverity) : "normal";
}

// 建议再验证状态标签（卡片徽章 / 已失效折叠区）。
export const insightVerificationLabels: Record<InsightVerificationResult, string> = {
  "": "未验证",
  pending: "验证中…",
  valid: "已复核 · 仍存在",
  invalid: "已失效",
  failed: "验证失败",
};

/** 把后端任意字符串归一为合法验证状态；非法/缺省 → ""（未验证）。 */
export function normalizeInsightVerification(value?: string): InsightVerificationResult {
  if (value === "pending" || value === "valid" || value === "invalid" || value === "failed") return value;
  return "";
}

export type InsightFilter = InsightType | "all";

// 排序：动态类型在前，同类型内 high → normal → low → created_at 升序（稳定）。
export function sortFindings(findings: InsightFinding[]): InsightFinding[] {
  return [...findings].sort((a, b) => {
    const ta = insightTypeOrder.indexOf(normalizeInsightType(a.type));
    const tb = insightTypeOrder.indexOf(normalizeInsightType(b.type));
    if (ta !== tb) return ta - tb;
    const sa = severityRank[normalizeInsightSeverity(a.severity)];
    const sb = severityRank[normalizeInsightSeverity(b.severity)];
    if (sa !== sb) return sa - sb;
    return a.createdAt.localeCompare(b.createdAt);
  });
}

export function filterFindingsByType(findings: InsightFinding[], filter: InsightFilter): InsightFinding[] {
  if (filter === "all") return findings;
  return findings.filter((f) => normalizeInsightType(f.type) === filter);
}

export function insightFindingCounts(findings: InsightFinding[]): Record<InsightFilter, number> {
  const counts: Record<InsightFilter, number> = { all: findings.length, bug: 0, style: 0, optimization: 0, feature: 0 };
  for (const f of findings) counts[normalizeInsightType(f.type)] += 1;
  return counts;
}
