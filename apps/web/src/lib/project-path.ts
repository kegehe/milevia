/**
 * 判断一段文本像不像**项目内的文件路径**。
 *
 * 用途：会话里被反引号包起来的内容（AI 回答引用文件时最常见的写法）如果确实像路径，
 * 就变成可点的入口，点一下直接在手机上打开那个文件 —— 手机上要看文件，九成是为了
 * 看 AI 刚改了什么，这个入口比从根目录一路点进去重要得多。
 *
 * 判据刻意**保守**：宁可漏（用户还能自己从文件视图找），不可误（把普通句子变成链接，
 * 点下去是一个"文件不存在"的报错，比没有链接更糟）。所以要求：
 *
 *   - 不含空白与反斜杠，不以 `/`、`~` 开头（那不是项目内相对路径）；
 *   - 不含 `..`（路径穿越不在手机端的可点范围里）；
 *   - 不含 `://`（外链不是文件）；
 *   - 最后一段必须带一个**字母开头**的扩展名。这一条排掉了大量伪装者：
 *     `v1.2.3`（扩展名是 `3`）、`react`（没有点）、`foo.123`。
 *
 * 允许带 `:行号` 后缀（`src/main.ts:42` 是 AI 回答里的常客）。行号只作为提示带出去：
 * 手机端的查看器不做跳行，把带行号的字符串当路径去查会直接找不到文件。
 */

/** 反引号里最长能当路径看待的长度。超了基本是代码片段而不是路径。 */
const MAX_PATH_LENGTH = 240;

/** 点开头的整档配置文件名（`.env`、`.gitignore`）。它们是真实文件，不能一刀切掉。 */
const DOTFILE = /^\.[A-Za-z][\w-]{0,31}$/;

// 一段路径成分：不含空白、分隔符与 shell 保留字符。
//
// 刻意**不用 `\w`**：它只认 `[A-Za-z0-9_]`，会把中文目录名整段挡掉
// （`文档/说明.md`、`docs/40-方案.md` 都是真实存在的写法）。用"排除法"定义成分之后，
// 中文、日文、带连字符与点的目录名天然可用，而被排除的那些字符正好是路径里真不该有的。
const SEGMENT = String.raw`[^\s/\\?#:*"<>|]+`;
const EXTENSION = String.raw`\.[A-Za-z][A-Za-z0-9]{0,7}`;
/** 单个文件名（`README.md`、`.env` 之外的普通文件）。 */
const BARE_FILE = new RegExp(String.raw`^${SEGMENT}${EXTENSION}$`);
/** 至少一层目录的路径（`apps/web/src/App.tsx`）。 */
const NESTED_PATH = new RegExp(String.raw`^${SEGMENT}(?:/${SEGMENT})+${EXTENSION}$`);

export interface ProjectFileReference {
  /** 归一化后的项目内相对路径。 */
  path: string;
  /** 原文里的行号（`src/main.ts:42` 的 42）。0 表示没写。 */
  line: number;
}

export function projectFileReference(raw: string): ProjectFileReference | null {
  const text = raw.trim();
  if (!text || text.length > MAX_PATH_LENGTH) return null;
  if (/[\s\\]/.test(text)) return null;
  if (text.includes("..") || text.includes("://")) return null;

  // `./src/main.ts` 是常见的相对写法，前缀剥掉即可，不必当成不合法。
  let path = text.startsWith("./") ? text.slice(2) : text;
  if (!path || path.startsWith("/") || path.startsWith("~")) return null;

  // `:行号` 后缀。只认一位到六位数字，避免把 `a:b` 这种当行号切掉。
  let line = 0;
  const withLine = /^(.*):(\d{1,6})$/.exec(path);
  if (withLine) {
    path = withLine[1];
    line = Number(withLine[2]);
  }

  if (DOTFILE.test(path)) return { path, line };
  if (path.startsWith(".")) return null;
  if (!BARE_FILE.test(path) && !NESTED_PATH.test(path)) return null;
  return { path, line };
}
