// 真实页面主题预览 v2：
//  1) 真实 Vite 前端 + API mock（沿用 verify-*.mjs 模式）
//  2) 浅色主题：按「手工设计的色调 ramps」整表换色（饱和度/明度曲线全新设计，不再沿用旧稿）
//     深色主题：明暗翻转分支（已视觉验证）
//  3) 覆盖 CSS 之外的颜色：内联 style + SVG fill/stroke/stop-color 在 DOM 里同步换色
import { fileURLToPath } from "node:url";
import { createServer } from "vite";
import { chromium } from "playwright";
import fs from "node:fs";
import path from "node:path";

const WEB_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const OUT_DIR = path.resolve(WEB_ROOT, "../../outputs/themes-real");
fs.mkdirSync(OUT_DIR, { recursive: true });

const CHROME = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
  || "C:\\Users\\tangmaoke\\AppData\\Local\\ms-playwright\\chromium-1234\\chrome-win64\\chrome.exe";

// ---------------------------------------------------------------------------
// 颜色工具
// ---------------------------------------------------------------------------
function hexToRgba(hex) {
  let h = hex.slice(1);
  if (h.length === 3 || h.length === 4) h = [...h].map((c) => c + c).join("");
  const r = parseInt(h.slice(0, 2), 16), g = parseInt(h.slice(2, 4), 16), b = parseInt(h.slice(4, 6), 16);
  const a = h.length === 8 ? parseInt(h.slice(6, 8), 16) / 255 : 1;
  return { r, g, b, a };
}
function clamp01(v) { return Math.min(1, Math.max(0, v)); }
function rgbToHsl({ r, g, b }) {
  const R = r / 255, G = g / 255, B = b / 255;
  const max = Math.max(R, G, B), min = Math.min(R, G, B);
  const l = (max + min) / 2;
  if (max === min) return { h: 0, s: 0, l };
  const d = max - min;
  const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
  let h;
  if (max === R) h = ((G - B) / d + (G < B ? 6 : 0));
  else if (max === G) h = (B - R) / d + 2;
  else h = (R - G) / d + 4;
  return { h: h * 60, s, l };
}
function hslToRgb(h, s, l) {
  h = (((h % 360) + 360) % 360) / 360;
  if (s === 0) { const v = Math.round(l * 255); return { r: v, g: v, b: v }; }
  const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
  const p = 2 * l - q;
  const f = (t) => {
    if (t < 0) t += 1; if (t > 1) t -= 1;
    if (t < 1 / 6) return p + (q - p) * 6 * t;
    if (t < 1 / 2) return q;
    if (t < 2 / 3) return p + (q - p) * (2 / 3 - t) * 6;
    return p;
  };
  return { r: Math.round(f(h + 1 / 3) * 255), g: Math.round(f(h) * 255), b: Math.round(f(h - 1 / 3) * 255) };
}
function toHex({ r, g, b, a = 1 }) {
  const hex = "#" + [r, g, b].map((v) => Math.round(clamp01(v / 255) * 255).toString(16).padStart(2, "0")).join("");
  return a < 1 ? hex + Math.round(a * 255).toString(16).padStart(2, "0") : hex;
}
function bucket(h, s, l) {
  if (h >= 50 && h < 100) return "primary";
  if (h >= 100 && h < 200) return "core";
  if (h >= 200 && h < 260 && s >= 0.30) return "inkblue";
  if (h >= 18 && h < 50) return "amber";
  if (h >= 345 || h < 18) return "red";
  if (h >= 260 && h < 345) return "violet";
  return "neutral";
}
// 两段线性插值：(0,s0) → (midT,sMid) → (1,s1)
function tri(t, v0, midT, vm, v1) {
  return t <= midT ? v0 + (vm - v0) * (t / midT) : vm + (v1 - vm) * ((t - midT) / (1 - midT));
}

// ---------------------------------------------------------------------------
// 主题定义
// ---------------------------------------------------------------------------
// 浅色：每个色桶一条手工 ramp —— s0/sMid/s1 是饱和度曲线（t=0 最暗端 → t=1 最亮端），l0/lMid/l1 是明度锚点
const LIGHT_RAMPS = {
  neutral: { s0: 0.03, sMid: 0.10, s1: 0.06, l0: 0.13, lMid: 0.52, l1: 0.985, midT: 0.55 },
  core:    { s0: 0.34, sMid: 0.68, s1: 0.34, l0: 0.17, lMid: 0.46, l1: 0.97, midT: 0.52 },
  primary: { s0: 0.46, sMid: 0.80, s1: 0.48, l0: 0.31, lMid: 0.47, l1: 0.95, midT: 0.50 },
  amber:   { s0: 0.58, sMid: 0.86, s1: 0.55, l0: 0.27, lMid: 0.52, l1: 0.94, midT: 0.50 },
  red:     { s0: 0.48, sMid: 0.72, s1: 0.44, l0: 0.29, lMid: 0.50, l1: 0.96, midT: 0.50 },
  inkblue: { s0: 0.22, sMid: 0.40, s1: 0.30, l0: 0.08, lMid: 0.30, l1: 0.60, midT: 0.50 },
};
// 旧色在每个桶里的明度锚（用于求 t）
const OLD_ANCHORS = {
  neutral: [0.09, 1.0], core: [0.10, 0.97], primary: [0.27, 0.95],
  amber: [0.26, 0.94], red: [0.30, 0.96], inkblue: [0.08, 0.62],
};

const THEMES = [
  { slug: "a-sky", name: "晨空蓝", tagline: "清透天青蓝，冷静而不冷淡", light: true,
    hue: { neutral: 218, core: 205, primary: 205, amber: 38, red: 4, inkblue: 222 },
    over: {} },
  { slug: "b-emerald", name: "翡翠墨", tagline: "浓翡翠+近黑墨色，贵气的绿", light: true,
    hue: { neutral: 178, core: 161, primary: 161, amber: 40, red: 356, inkblue: 192 },
    over: { core: { sMid: 0.74, lMid: 0.42 }, primary: { sMid: 0.76, lMid: 0.44 } } },
  { slug: "c-iris", name: "鸢尾紫", tagline: "鸢尾花紫，优雅现代", light: true,
    hue: { neutral: 255, core: 258, primary: 261, amber: 44, red: 348, inkblue: 262 },
    over: { neutral: { sMid: 0.07 } } },
  { slug: "d-linen", name: "亚麻陶", tagline: "亚麻暖底+陶土橘，手作温度", light: true, bgFromCore: true,
    hue: { neutral: 36, core: 152, primary: 22, amber: 38, red: 12, inkblue: 30 },
    over: {
      neutral: { s0: 0.05, sMid: 0.15, s1: 0.10, l1: 0.98 },
      core: { sMid: 0.42 },
      primary: { sMid: 0.74, lMid: 0.48 },
    } },
  { slug: "e-noir-gold", name: "曜黑琥珀", tagline: "纯黑底+鎏金，暗色里的高级感", dark: true,
    neutralHue: 262, coreHue: 152, coreSatMul: 0.8, primaryHue: 40, amberHue: 42, redHue: 2, termHue: 46 },
  { slug: "f-midnight", name: "午夜靛蓝", tagline: "午夜蓝深空+电光靛，控制台气质", dark: true,
    neutralHue: 228, coreHue: 210, coreSatMul: 1.0, primaryHue: 248, amberHue: 45, redHue: 353, termHue: 230 },
];

// ---------------------------------------------------------------------------
// 浅色：ramp 换色
// ---------------------------------------------------------------------------
function transformHexRamp(hex, theme) {
  const { r, g, b, a } = hexToRgba(hex);
  const { h, s, l } = rgbToHsl({ r, g, b });
  if (a < 0.45 && l < 0.4) return hex; // 阴影/半透明深色 → 保持
  if (a < 0.9 && l > 0.92) return hex; // 白色高光 overlay → 保持
  let kind = bucket(h, s, l);
  // 暖纸类主题：淡绿底 tint（core 高明度段）改走暖色中性 ramp，让整页底色变暖
  if (theme.bgFromCore && kind === "core" && l >= 0.9) kind = "neutral";
  const hueMap = kind === "violet" ? theme.hue.neutral : theme.hue[kind];
  const ramp = { ...LIGHT_RAMPS[kind === "violet" ? "neutral" : kind], ...(theme.over[kind] || (kind === "violet" ? theme.over.neutral : undefined)) };
  const [lo, hi] = OLD_ANCHORS[kind] || OLD_ANCHORS.neutral;
  const t = clamp01((l - lo) / (hi - lo));
  const s2 = tri(t, ramp.s0, ramp.midT, ramp.sMid, ramp.s1);
  const l2 = tri(t, ramp.l0, ramp.midT, ramp.lMid, ramp.l1);
  return toHex({ ...hslToRgb(hueMap, s2, clamp01(l2)), a });
}

// ---------------------------------------------------------------------------
// 深色：明暗翻转分支（已视觉验证的路径）
// ---------------------------------------------------------------------------
function transformHexDark(hex, t) {
  const { r, g, b, a } = hexToRgba(hex);
  const { h, s, l } = rgbToHsl({ r, g, b });
  if (a < 0.45 && l < 0.35) return toHex({ r: 0, g: 0, b: 0, a });
  const kind = bucket(h, s, l);
  if (l >= 0.92) {
    const hue2 = kind === "amber" ? t.amberHue : kind === "primary" ? t.primaryHue
      : kind === "red" ? t.redHue : kind === "core" ? t.coreHue : t.neutralHue;
    return toHex({ ...hslToRgb(hue2, Math.min(0.5, s * 0.45), 0.08 + (1 - l) * 0.3), a });
  }
  if (kind === "neutral") {
    let l2 = l, s2 = s * t.neutralSatMul;
    if (l >= 0.94) l2 = 0.062 + (1 - l) * 0.55;
    else if (l >= 0.60) l2 = 0.125 + (l - 0.60) * 0.42;
    else if (l >= 0.30) l2 = 0.56 + (l - 0.30) * 0.38;
    else l2 = 0.87 + l * 0.33;
    s2 = s * t.neutralSatMul * 0.85;
    return toHex({ ...hslToRgb(t.neutralHue, s2, clamp01(l2)), a });
  }
  if (kind === "core") {
    let l2 = l, s2 = Math.min(1, s * t.coreSatMul);
    if (l >= 0.90) { l2 = 0.075 + (1 - l) * 0.5; s2 = s * t.coreSatMul * 0.7; }
    else if (l >= 0.45) { l2 = 0.45 - (l - 0.45) * 0.55; }
    else { l2 = 0.45 + (0.45 - l) * 0.6; s2 = Math.min(1, s * 1.05); }
    return toHex({ ...hslToRgb(t.coreHue, s2, clamp01(l2)), a });
  }
  if (kind === "primary") {
    let l2 = l;
    if (l >= 0.55) l2 = 0.42; else l2 = 0.5 + (0.55 - l) * 0.4; // 深色下主按钮压深，避免浅字上亮底
    return toHex({ ...hslToRgb(t.primaryHue, Math.min(1, s * 0.9), clamp01(l2)), a });
  }
  if (kind === "amber") {
    let l2 = l + 0.04;
    if (l < 0.50) l2 = 0.60 + (0.50 - l) * 0.35;
    return toHex({ ...hslToRgb(t.amberHue, Math.min(1, s * 1.02), clamp01(l2)), a });
  }
  if (kind === "red") {
    let l2 = l + 0.05;
    if (l < 0.50) l2 = 0.62 + (0.50 - l) * 0.35;
    return toHex({ ...hslToRgb(t.redHue, s, clamp01(l2)), a });
  }
  if (kind === "violet") {
    return toHex({ ...hslToRgb(t.coreHue, s * 0.8, l), a });
  }
  return toHex({ ...hslToRgb(t.termHue, s, l), a });
}

function hexToCss(hex) {
  const { r, g, b, a } = hexToRgba(hex);
  return a < 1 ? `rgba(${r},${g},${b},${Number(a.toFixed(3))})` : `rgb(${r},${g},${b})`;
}

function makeTransformer(theme) {
  return theme.dark
    ? (hex) => transformHexDark(hex, theme)
    : (hex) => transformHexRamp(hex, theme);
}

function transformCss(css, transform) {
  let out = css.replace(/#[0-9a-fA-F]{3,8}\b/g, (m) => {
    if (m.length !== 4 && m.length !== 5 && m.length !== 7 && m.length !== 9) return m;
    try { return transform(m.toLowerCase()); } catch { return m; }
  });
  out = out.replace(/rgba?\(\s*([\d.]+)[\s,]+([\d.]+)[\s,]+([\d.]+)[\s/]*(?:[,/]\s*([\d.%]+)\s*)?\)/g, (m, r, g, b, a) => {
    const alpha = a === undefined ? 1 : (a.endsWith("%") ? parseFloat(a) / 100 : parseFloat(a));
    try {
      const next = transform(toHex({ r: +r, g: +g, b: +b, a: alpha }));
      return hexToCss(next);
    } catch { return m; }
  });
  out = out.replace(/\bwhite\b/gi, () => { try { return hexToCss(transform("#ffffff")); } catch { return "white"; } });
  return out;
}

// DOM 内联色（style 属性 + SVG 呈现属性）换色
function domTransformSource(theme) {
  return `(() => {
    ${makeTransformerFactorySource()}
    ;(function walk(){
      const __tf = __makeTransformer(${JSON.stringify(theme)});
      const NAMED = { white: "#ffffff", black: "#000000" };
      const test = /#[0-9a-fA-F]{3,8}\\b|rgba?\\([^)]*\\)|\\bwhite\\b|\\bblack\\b/i;
      const els = document.querySelectorAll("*");
      const fix = (v) => v.replace(/#[0-9a-fA-F]{3,8}\\b|rgba?\\([^)]*\\)|\\bwhite\\b|\\bblack\\b/gi, (m) => {
        if (/^[a-z]+$/i.test(m)) m = NAMED[m.toLowerCase()] || m;
        try { return __tf(m); } catch { return m; }
      });
      for (const el of els) {
        const style = el.getAttribute && el.getAttribute("style");
        if (style && test.test(style)) { el.setAttribute("style", fix(style)); }
        if (el.namespaceURI === "http://www.w3.org/2000/svg") {
          for (const att of ["fill", "stroke", "stop-color", "flood-color"]) {
            const v = el.getAttribute(att);
            if (v && v !== "none" && v !== "currentColor" && test.test(v)) el.setAttribute(att, fix(v));
          }
        }
      }
    })();
  })();
  `;
}

// 供浏览器端使用的转换器工厂（浅色 ramp / 深色翻转 双路径）
function makeTransformerFactorySource() {
  return `function __makeTransformer(theme){
    const hexToRgba=${hexToRgba.toString()};
    const clamp01=${clamp01.toString()};
    const rgbToHsl=${rgbToHsl.toString()};
    const hslToRgb=${hslToRgb.toString()};
    const toHex=${toHex.toString()};
    const bucket=${bucket.toString()};
    const tri=${tri.toString()};
    const LIGHT_RAMPS=${JSON.stringify(LIGHT_RAMPS)};
    const OLD_ANCHORS=${JSON.stringify(OLD_ANCHORS)};
    const transformHexRamp=${transformHexRamp.toString()};
    const transformHexDark=${transformHexDark.toString()};
    return (hex)=> theme.dark ? transformHexDark(hex, theme) : transformHexRamp(hex, theme);
  }`;
}

// ---------------------------------------------------------------------------
// API mock（与 v1 相同的契约）
// ---------------------------------------------------------------------------
const now = Date.now();
const iso = (minAgo) => new Date(now - minAgo * 60_000).toISOString();
const projects = [
  { id: "p1", name: "milevia-desktop", pathDisplay: "milevia-desktop", fullPath: "D:\\projects\\milevia-desktop", runner: "local", environment: "windows", localRunner: true, gitBranch: "main", claudeReady: true, codexReady: true, agentReady: true },
  { id: "p2", name: "cloud-control", pathDisplay: "cloud-control", fullPath: "D:\\projects\\cloud-control", runner: "local", environment: "wsl", localRunner: false, gitBranch: "main", claudeReady: true, codexReady: false, agentReady: true },
  { id: "p3", name: "android-runner", pathDisplay: "android-runner", fullPath: "D:\\projects\\android-runner", runner: "remote-1", environment: "remote", localRunner: false, gitBranch: "dev", claudeReady: false, codexReady: false, agentReady: false },
];
const statuses = [
  { id: "p1", running: 1, conversationCount: 3, activeTitle: "任务队列详情面板修复", insightsRunning: 1, insightsMessage: "正在分析优化建议…" },
  { id: "p2", running: 0, conversationCount: 1, activeTitle: "", insightsRunning: 0, insightsMessage: "" },
  { id: "p3", running: 0, conversationCount: 0, activeTitle: "", insightsRunning: 0, insightsMessage: "" },
];
const conversations = [
  { id: "c1", projectId: "p1", title: "任务队列详情面板修复", preview: "补丁已应用：三种形态的禁用态都重申了底色", status: "idle", agentId: "claude-code", permissionMode: "approval_required", isCurrent: true, isOrchestration: false, lastActivityAt: iso(2), createdAt: iso(180) },
  { id: "c2", projectId: "p1", title: "顺手清理命名", preview: "", status: "idle", agentId: "claude-code", permissionMode: "approval_required", isCurrent: false, isOrchestration: false, lastActivityAt: iso(300), createdAt: iso(400) },
];
const runId = "r1";
const detailMessages = [
  { id: "m1", runId, role: "user", content: "任务队列详情面板里禁用的停止按钮还是实底绿，按 tasks.css 的约定把禁用态降灰，顺便核对 canReviewInline 的口径。", createdAt: iso(9) },
  { id: "m2", runId, role: "assistant", content: "看了下 `InlineTaskDetail`：操作条的按钮只写了 `:disabled { opacity }`，实底绿是全局 `.primary` 的底色透出来的。按约定每个形态的禁用态都要重申底色，我来改。", createdAt: iso(8) },
  { id: "m3", runId, role: "assistant", content: "补丁已应用：三种形态的禁用态现在都重申了底色，队列行与详情面板的验收入口保持同一口径 canReviewInline。", createdAt: iso(2) },
];
const detailEvents = [
  { id: "e1", type: "assistant", runId, createdAt: iso(8), payload: { message: { content: [{ type: "tool_use", id: "tu1", name: "Bash", input: { command: "npm test -- tasks.css", description: "运行任务队列测试" } }] } } },
  { id: "e2", type: "user", runId, createdAt: iso(7), payload: { message: { content: [{ type: "tool_result", tool_use_id: "tu1", content: [{ type: "text", text: "✓ 47 passed (3.2s)\n\n变异检验 escaped 0 / skipped 0" }] }] } } },
  { id: "e3", type: "run.completed", runId, createdAt: iso(7), payload: {} },
  { id: "e4", type: "assistant", runId: "r2", createdAt: iso(5), payload: { message: { content: [{ type: "tool_use", id: "tu2", name: "Bash", input: { command: "git push origin main", description: "推送补丁到远端" } }] } } },
  { id: "e5", type: "approval.requested", runId: "r2", createdAt: iso(5), payload: { approvalId: "ap1", status: "pending", toolName: "Bash", toolInput: { command: "git push origin main", description: "推送补丁到远端" } } },
];
const conversationDetail = { conversation: conversations[0], messages: detailMessages, events: detailEvents, activeRunId: "r2", hasMore: false, hasMoreMessages: false, nextCursor: "" };
const runnerInfo = { id: "local", name: "Windows Local Runner", kind: "windows", status: "ready", agentReady: true, environment: "windows" };

async function mockApi(route) {
  const req = route.request();
  const url = new URL(req.url());
  const p = url.pathname;
  const method = req.method();
  const json = (data) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(data) });
  if (p === "/api/projects" && method === "GET") return json(projects);
  if (p === "/api/projects/statuses") return json(statuses);
  if (p === "/api/projects/availability") return json(projects.map((x) => ({ id: x.id, claudeReady: x.claudeReady, codexReady: x.codexReady, agentReady: x.agentReady })));
  if (/^\/api\/projects\/[^/]+$/.test(p) && method === "GET") return json(projects.find((x) => x.id === p.split("/")[3]) ?? projects[0]);
  if (p === "/api/projects/processes/statuses") return json({ p1: { runStatus: "running", runStartedAt: iso(84) }, p2: { runStatus: "running", runStartedAt: iso(30) }, p3: { runStatus: "stopped" } });
  if (p === "/api/runners") return json([runnerInfo]);
  if (/^\/api\/runners\/[^/]+\/status$/.test(p)) return json(runnerInfo);
  if (/^\/api\/runners\/[^/]+\/agent-profiles$/.test(p)) return json([]);
  if (/^\/api\/projects\/[^/]+\/conversations$/.test(p) && method === "GET") return json({ items: conversations, nextCursor: "" });
  if (/^\/api\/projects\/[^/]+\/git\/summary/.test(p)) return json({
    repositoryState: "ready",
    head: { oid: "8f2c1ab", branch: "main", detached: false, upstream: "origin/main", ahead: 1, behind: 0 },
    worktree: { staged: 1, modified: 2, untracked: 0, deleted: 0, renamed: 0, conflicted: 0 },
    observedAt: iso(1), stateToken: "st-1", changesRevision: "rev-1",
    changes: [
      { path: "apps/web/src/tasks.css", staged: true, modified: true, untracked: false, deleted: false, renamed: false, conflicted: false },
      { path: "apps/web/src/TaskQueue.tsx", staged: false, modified: true, untracked: false, deleted: false, renamed: false, conflicted: false },
    ],
  });
  if (/^\/api\/projects\/[^/]+\/git\/branches/.test(p)) return json([{ name: "main", remote: false, current: true, upstream: "origin/main" }, { name: "dev", remote: false, current: false }]);
  if (/^\/api\/projects\/[^/]+\/tasks$/.test(p)) return json([
    { id: "t1", projectId: "p1", description: "任务队列内联详情面板：禁用态降灰与验收入口对齐", status: "in_progress", priority: "high", position: 0, createdAt: iso(120), updatedAt: iso(10) },
    { id: "t2", projectId: "p1", description: "批量验收执行记录聚合视图", status: "pending", priority: "normal", position: 1, createdAt: iso(200), updatedAt: iso(200) },
    { id: "t3", projectId: "p1", description: "清理过期草稿文件与临时目录", status: "done", priority: "low", position: 2, createdAt: iso(400), updatedAt: iso(50) },
  ]);
  if (/^\/api\/projects\/[^/]+\/input-history/.test(p)) return json([]);
  if (/^\/api\/projects\/[^/]+\/shortcuts/.test(p)) return json([]);
  if (/^\/api\/projects\/[^/]+\/skills/.test(p)) return json([]);
  if (/^\/api\/projects\/[^/]+\/commands/.test(p)) return json({ projectId: "p1", agentId: "claude-code", env: "windows", source: "static", authoritative: false, commands: [], customAllowed: true });
  if (/^\/api\/projects\/[^/]+\/mcp\/status/.test(p)) return json({ projectId: "p1", environment: "windows", agentId: "claude-code", strictMode: false, serverCount: 0, servers: [], updatedAt: iso(1) });
  if (/^\/api\/projects\/[^/]+\/conversations\/activity/.test(p)) return json({ updates: [] });
  if (/^\/api\/conversations\/[^/]+\/models$/.test(p)) return json({ conversationId: "c1", agentId: "claude-code", selected: "claude-sonnet-4-5", effective: "claude-sonnet-4-5", source: "cli_default", models: [{ id: "claude-sonnet-4-5", label: "Sonnet 4.5" }, { id: "claude-opus-4-1", label: "Opus 4.1" }], customAllowed: true });
  if (/^\/api\/conversations\/[^/]+\/workspaces$/.test(p)) return json([{ id: "w1", conversationId: "c1", generation: 1, mode: "project_shared", path: "D:\\projects\\milevia-desktop", state: "active", active: true, createdAt: iso(200) }]);
  if (/^\/api\/conversations\/[^/]+\/checkpoints$/.test(p)) return json({ checkpoints: [] });
  if (/^\/api\/conversations\/[^/]+\/usage/.test(p)) return json({ conversationId: "c1", available: true, context: { runId, conversationId: "c1", available: true, status: "completed", model: "claude-sonnet-4-5", contextWindow: 200000, contextInputTokens: 18400, inputTokens: 21400, outputTokens: 3120, cacheReadTokens: 40200, cacheCreationTokens: 1200, estimatedCostUsd: 0.19, agentTurns: 4, modelSteps: 9, toolCalls: 6, subagentCount: 0, durationMs: 262000, ttftMs: 1400, terminalReason: "", hasResult: true, models: [] }, session: { taskCount: 3, agentTurns: 4, modelSteps: 9, toolCalls: 6, subagentCount: 0, inputTokens: 21400, outputTokens: 3120, cacheReadTokens: 40200, cacheCreationTokens: 1200, estimatedCostUsd: 0.19 }, models: null });
  if (/^\/api\/conversations\/c2$/.test(p)) return json({ conversation: conversations[1], messages: [], events: [], activeRunId: null, hasMore: false, hasMoreMessages: false, nextCursor: "" });
  if (/^\/api\/conversations\/c1$/.test(p) && method === "GET") return json(conversationDetail);
  if (/^\/api\/notifications/.test(p)) return json({ items: [], nextCursor: "" });
  if (/^\/api\/shortcuts/.test(p)) return json([]);
  if (method === "GET") return json({});
  return json({});
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;

function collectCss() {
  const files = [];
  (function walk(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, entry.name);
      if (entry.isDirectory()) { if (entry.name !== "node_modules") walk(p); }
      else if (entry.name.endsWith(".css")) files.push(p);
    }
  })(path.join(WEB_ROOT, "src"));
  return files.sort().map((p) => fs.readFileSync(p, "utf8")).join("\n");
}
const BASE_CSS = collectCss();
const MARK_SVG = fs.readFileSync(path.join(WEB_ROOT, "public", "milevia-mark.svg"), "utf8");

const browser = await chromium.launch({ executablePath: CHROME, headless: true });
for (const theme of THEMES) {
  const transform = makeTransformer(theme);
  const themed = transformCss(BASE_CSS, transform);
  const themedMark = transformCss(MARK_SVG, transform); // 品牌 logo 外链资源：路由层直接回换色版
  fs.writeFileSync(path.join(OUT_DIR, `theme-${theme.slug}.override.css`), themed);
  fs.writeFileSync(path.join(OUT_DIR, `theme-${theme.slug}.mark.svg`), themedMark);
  const domWalkJs = domTransformSource(theme);

  const context = await browser.newContext({ viewport: { width: 1560, height: 1440 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  page.on("pageerror", (e) => console.log("PAGEERROR:", e.message.slice(0, 160)));
  page.on("response", (r) => { if (r.url().includes("/api/projects/statuses")) console.log("RES statuses", r.status(), theme.slug); });
  await page.route("**/ws/**", (route) => route.fulfill({ status: 400 }));
  await page.route("**/milevia-mark.svg", (route) => route.fulfill({ status: 200, contentType: "image/svg+xml", body: themedMark }));
  await page.route("**/api/**", mockApi);

  // ① 概览页
  await page.goto(base + "/", { waitUntil: "networkidle" });
  await page.addStyleTag({ content: themed });
  await page.evaluate(domWalkJs);
  const runningOk = await page.waitForSelector(".task-card.state-running", { timeout: 6000 }).then(() => true).catch(() => false);
  if (!runningOk) {
    console.log("WARN running 状态未出现:", theme.slug);
    const dbg = await page.evaluate(() => ({
      cards: [...document.querySelectorAll(".task-card")].map((el) => el.className),
      bandText: document.querySelector(".task-card .project-band-status")?.textContent ?? null,
    })).catch(() => ({}));
    console.log("DBG", JSON.stringify(dbg));
  }
  await page.waitForTimeout(600);
  await page.setViewportSize({ width: 1560, height: 1000 });
  await page.waitForTimeout(200);
  await page.screenshot({ path: path.join(OUT_DIR, `theme-${theme.slug}-dashboard.png`), fullPage: true });
  console.log("saved", `theme-${theme.slug}-dashboard.png`);

  // ② 对话页
  await page.setViewportSize({ width: 1560, height: 1440 });
  await page.goto(base + "/projects/p1/conversations/c1", { waitUntil: "networkidle" });
  await page.addStyleTag({ content: themed });
  await page.evaluate(domWalkJs);
  await page.waitForTimeout(1200);
  await page.evaluate(() => {
    document.querySelectorAll(".workspace-tab-panel, .workspace-content").forEach((el) => { el.scrollTop = 0; });
    document.querySelectorAll("[class*='timeline'], [class*='scroll']").forEach((el) => { if (el.scrollHeight > el.clientHeight) el.scrollTop = 0; });
  });
  await page.waitForTimeout(300);
  await page.screenshot({ path: path.join(OUT_DIR, `theme-${theme.slug}-chat.png`), fullPage: true });
  console.log("saved", `theme-${theme.slug}-chat.png`);
  await context.close();
}
await browser.close();
await server.close();
console.log("done ->", OUT_DIR);
