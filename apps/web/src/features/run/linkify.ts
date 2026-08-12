// 把日志文本里的 http/https 链接切成可点击片段。
// 终端输出里 URL 常夹在句子中间，或末尾带着括号/标点（如 `(http://x.com/a),`），
// 这里只做最外层切分：URL 原文作为可打开地址，紧随其后的标点/右括号保留为普通文本。

export type LinkifyPart = {
  text: string;
  /** 命中 URL 时携带完整可打开地址；否则为普通文本 */
  url?: string;
};

const urlPattern = /\bhttps?:\/\/[^\s<>"']+/gi;

/** 剥离 URL 尾部不属于地址的句子标点，以及不配对的右括号。 */
function trimTrailing(url: string): string {
  let result = url.replace(/[.,;:!?]+$/, "");
  if (result.endsWith(")") && !result.includes("(")) result = result.slice(0, -1);
  if (result.endsWith("]") && !result.includes("[")) result = result.slice(0, -1);
  if (result.endsWith("}") && !result.includes("{")) result = result.slice(0, -1);
  return result;
}

export function linkifyText(text: string): LinkifyPart[] {
  const parts: LinkifyPart[] = [];
  let cursor = 0;
  for (const match of text.matchAll(urlPattern)) {
    const index = match.index ?? 0;
    if (index > cursor) parts.push({ text: text.slice(cursor, index) });
    const raw = match[0];
    const url = trimTrailing(raw);
    if (url) {
      parts.push({ text: url, url });
      // 被剥掉的尾部标点仍要显示，只是不再是链接的一部分。
      const trailing = raw.slice(url.length);
      if (trailing) parts.push({ text: trailing });
    } else {
      // 形如 `http://` 的残缺地址，按普通文本保留。
      parts.push({ text: raw });
    }
    cursor = index + raw.length;
  }
  if (cursor < text.length) parts.push({ text: text.slice(cursor) });
  if (parts.length === 0) parts.push({ text });
  return parts;
}
