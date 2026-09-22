// 手机端会话末尾「正在处理」状态条的判据（纯函数，无 React）。
//
// 为什么单独成模块：这条状态要把三份不同来源的读数合起来 —— 徽标（agentId）、
// 阶段词（最新一条运行通知）、已耗时（processingConversations[id].startedAt）。
// 三个渲染点、两套时长格式、四种通知变体，全都写回页面里就是"同一件事在多处各写一遍"，
// 那样每改一次口径都要靠人去 grep。收口在这里，页面只负责调用 + 渲染。

// ⚠️ 阶段词的来源是**通知自己的标题**，不是这里造的一句词。
//
// 第一版写的是 `variant → 词` 的映射表，那是错的，理由值得记下来：
// 桌面时间线（lib/timeline.ts 的 systemItemFromEvent）**已经**把每种事件翻译成了一句中文标题
// ——「正在压缩上下文」「API 重试中」「后台任务启动」「任务转入后台」…而手机端通过
// noticeFromEventFields 拿到的 `RemoteNotice.title` 就是那一句（原样透传，见那里的一句
// "手机端不维护第二套文案枚举"）。在手机端再映射一遍，等于**同一句话有第二个作者**：
// 桌面端把「API 重试中」改成「正在重试」时，手机会继续说旧话，而且这种漂移没人会去 grep。
//
// 所以这里只留"通知没给出可读标题时"的兜底，不复制任何一句桌面文案。
const noticeStageFallbacks: Record<string, string> = {
  // 压缩刚开始：桌面那句标题此时是"正在压缩上下文"，但若服务端只给了 subtype、
  // 标题还没落地，这里至少要说对"在压缩"。这几条不是翻译表，是**缺标题时的兜底词**。
  compact: "正在压缩上下文",
  api_retry: "网络重试中",
};

// 阶段词的最终兜底：拿不到任何通知时说这一句。
// 措辞刻意保持"在处理"而不是"在思考" —— 后者是我们无法证实的拟人化说法。
export const processingStageFallback = "正在处理";

// 允许被缩略的阶段词长度（渲染成"正在编辑 mobile-remote.css"这种时，后面那段的字数）。
// 超过就截断加省略号 —— 状态条必须单行，见下面 processingStageText 的说明。
const stageDetailLimit = 18;

// 收一条通知的文本当作阶段细节。文本可能是标题，也可能是一大段 CLI 输出，
// 所以先取第一行、再压平空白、超长截断。任何一步都不能省：
//   · 取第一行 —— 多行文本会把状态条撑成两行；
//   · 压平空白 —— 换行符在单行容器里会变成空白，看起来像随机空格；
//   · 截断 —— 见 stageDetailLimit。
export function condenseStageDetail(detail: string | undefined | null): string {
  if (!detail) return "";
  const firstLine = detail.split("\n")[0] || "";
  const flat = firstLine.replace(/\s+/g, " ").trim();
  if (!flat) return "";
  if (flat.length <= stageDetailLimit) return flat;
  return `${flat.slice(0, stageDetailLimit)}…`;
}

export type ProcessingStageInput = {
  // 会话里最新的一条运行通知（按 createdAt 排序后的最后一条）。没有就传 null。
  notice: { variant: string; title?: string; detail?: string } | null;
};

// 阶段词。优先级：**通知标题** > 标题缺失时的兜底词 > 「正在处理」。
//
// 为什么标题优先于 detail：标题是"这件事的名字"（已压缩完 / 正在重试），
// detail 是"它的细节"（第 2/5 次重试：服务过载）。状态条只有一行的位置，
// 该显示名字 —— 细节在下面那张状态卡里本来就有。
export function processingStageText(input: ProcessingStageInput): string {
  const notice = input.notice;
  if (notice) {
    const title = condenseStageDetail(notice.title);
    if (title) return title;
    const mapped = noticeStageFallbacks[notice.variant];
    if (mapped) return mapped;
    // 变体不在表里、标题也缺失（服务端加了新事件而前端还没跟上）时**不要瞎猜**：
    // 落回兜底，绝不把英文 subtype（如 "task_updated"）漏到界面上。
  }
  return processingStageFallback;
}

// 已耗时的显示阈值：**10 秒以下不显示**。
// 秒级任务上闪一个「0:03」只增加噪音，而且会让人以为它比实际慢。
export const processingClockVisibleAfterMs = 10_000;

// 把毫秒差格式化成"已运行"的读数。分档规则：
//   < 10s        —— 空串（不显示，见上）
//   < 60s        —— `24 秒`
//   < 60min      —— `1 分 24 秒`
//   >= 1h        —— `1 小时 5 分`
//
// 为什么不上 `12:04:07` 这种读数：状态条要一眼扫过，
// 时钟样式需要读者自己在脑子里做"这有多久"的换算，而秒/分/小时的词自带那个换算。
//
// 负值（客户端与服务端时钟有偏差时会出现）一律当 0 处理 —— 显示「-3 秒」是纯粹的坏。
//
// ⚠️ `!Number.isFinite(ms)` 与 `ms < 0` 这两半句的守卫力**不一样**，别当成同一件事：
//   · `!Number.isFinite` 是**活的** —— NaN 与 ±Infinity 都通不过下面任何一档比较
//     （它们与 `processingClockVisibleAfterMs` 的比较恒为 false），少了它就会掉进
//     `Math.floor(NaN/1000)` ⇒ 渲染出「NaN 秒」。用例在测这个。
//   · `ms < 0` 在当前阈值（10_000）下是**纯防御、当前不可达**：任何负数都必然
//     小于门槛，于是门槛那一行已经先把它们判出去了。只有当
//     `processingClockVisibleAfterMs` 变成非正数（例如有人为了「立即显示耗时」
//     把它设成 0）时，它才会成为唯一去路。
//   所以它**故意留着**（它是那个改动的护栏），并由下面那条结构断言盯着阈值不越过 0 ——
//   若哪天门槛真被改成 0，这条断言会先红，逼着改的人回来把负值一起想清楚。
//   （2026-09-22 变异检验：删掉 `ms < 0` 一条用例都不红 —— 因为它在当前阈值下确实
//     不改变任何返回值。这不是漏测，是「死分支」，按项目铁律要当 bug 追、要么删要么钉。）
export function formatElapsed(ms: number): string {
  if (!Number.isFinite(ms)) return "";
  if (ms < 0 || ms < processingClockVisibleAfterMs) return "";
  const totalSeconds = Math.floor(ms / 1000);
  if (totalSeconds < 60) return `${totalSeconds} 秒`;
  const totalMinutes = Math.floor(totalSeconds / 60);
  if (totalMinutes < 60) {
    const seconds = totalSeconds % 60;
    return `${totalMinutes} 分 ${seconds} 秒`;
  }
  const hours = Math.floor(totalMinutes / 60);
  return `${hours} 小时 ${totalMinutes % 60} 分`;
}

// 徽标上的 Agent 名与配色档位。
//   claude —— Claude Code，用它的品牌橙
//   codex  —— Codex，用近黑墨色
// fallback 落在 claude 上是为了**不改现有观感**：未知 agent 原先就显示"Claude Code"，
// 这里保持同一个默认，只是把颜色一起带上。
export type ProcessingBadgeTone = "claude" | "codex";

export function processingBadge(agentId: string): { label: string; tone: ProcessingBadgeTone } {
  if (agentId === "codex") return { label: "Codex", tone: "codex" };
  return { label: "Claude Code", tone: "claude" };
}

// 从会话的通知列表里取"最新的一条"。
//
// 不直接取数组最后一项：服务端合并通知时（merge: "append"）是往同一条上并内容，
// 顺序不保证与时间一致。按 createdAt 取最大值才是可靠判据；全都没有时间戳时
// 退回数组末项，总比不显示好。
export function latestNotice<T extends { createdAt?: string }>(notices: T[] | undefined): T | null {
  if (!notices || notices.length === 0) return null;
  let best = notices[0];
  let bestAt = Date.parse(best.createdAt || "") || -Infinity;
  // 用下标从 1 起，**不写 `notices.slice(1)`**：这个函数每秒都被重算一次
  //（processingClock 每秒 setState ⇒ 组件重渲染），slice 会每秒白造一个新数组。
  for (let index = 1; index < notices.length; index += 1) {
    const notice = notices[index];
    const at = Date.parse(notice.createdAt || "") || -Infinity;
    if (at >= bestAt) {
      best = notice;
      bestAt = at;
    }
  }
  return best;
}
