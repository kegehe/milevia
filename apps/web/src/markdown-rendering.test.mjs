import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { markdownCodeComponents } from "./components/MarkdownCodeBlock.tsx";

const stylesheet = await readFile(new URL("./markdown.css", import.meta.url), "utf8");

// 四个 Markdown 渲染入口都要带上代码块复制按钮；少一处就会出现"某些界面没有按钮"。
const entryPoints = {
  "桌面聊天": await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8"),
  "自动编排": await readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8"),
  "手机端": await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8"),
  "文件预览": await readFile(new URL("./features/files/FileViewer.tsx", import.meta.url), "utf8"),
};

test("markdown keeps ordinary unordered-list markers while preserving task-list layout", () => {
  const rendered = renderToStaticMarkup(React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm] }, "- ordinary item\n- [x] done"));
  assert.match(rendered, /<ul class="contains-task-list">/);
  assert.match(rendered, /<li>ordinary item<\/li>/);
  assert.match(rendered, /<li class="task-list-item">[\s\S]*done<\/li>/);
  assert.match(stylesheet, /\.markdown ul \{[^}]*list-style:\s*disc;/);
  assert.match(stylesheet, /\.markdown ol \{[^}]*list-style:\s*decimal;/);
  assert.match(stylesheet, /\.markdown ul\.contains-task-list,\s*\.markdown li\.task-list-item \{[^}]*list-style:\s*none;/s);
  assert.match(stylesheet, /\.markdown ul\.contains-task-list \{[^}]*padding-left:\s*0;/s);
  assert.match(stylesheet, /\.markdown ul\.contains-task-list\s*>\s*li:not\(\.task-list-item\)\s*\{[^}]*margin-left:\s*22px;[^}]*list-style:\s*disc;/s);
  assert.doesNotMatch(stylesheet, /\.markdown ul li::marker\s*\{[^}]*content:\s*["']?["'];/);
});

test("markdown blocks stay inside mobile message bubbles and scroll horizontally when needed", () => {
  assert.match(stylesheet, /\.markdown pre \{[^}]*width:\s*100%;[^}]*overflow-x:\s*auto;[^}]*max-width:\s*100%;/s);
  assert.match(stylesheet, /\.markdown table \{[^}]*width:\s*max-content;[^}]*min-width:\s*100%;[^}]*max-width:\s*100%;[^}]*overflow-x:\s*auto;/s);
  assert.match(stylesheet, /\.markdown a \{[^}]*overflow-wrap:\s*anywhere;[^}]*word-break:\s*break-word;/s);
  assert.match(stylesheet, /\.markdown td \{[^}]*overflow-wrap:\s*anywhere;[^}]*word-break:\s*break-word;/s);
  // 直接渲染出来的 <img>（手机端聊天、自动编排把 img 留给了浏览器）必须收紧到容器宽度：
  // 贴一张 1600px 的截图会把气泡、甚至整列网格一起撑到屏幕外，而页面又是 overflow-x: hidden，
  // 结果是"消息只剩左边一部分"。max-width 与 height 成对写，避免只压宽度导致图片变形。
  assert.match(stylesheet, /\.markdown img \{\s*max-width:\s*100%;\s*height:\s*auto;\s*\}/);
});

test("fenced code blocks carry a copy button with the raw code text", () => {
  const rendered = renderToStaticMarkup(
    React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm], components: markdownCodeComponents }, "```bash\nnpm run build\n```"),
  );
  // 代码文本必须原样进入 <pre><code>（fenced 块尾部的换行由 remark 保留），按钮不参与代码内容。
  assert.match(rendered, /<div class="markdown-code-block"><pre><code class="language-bash">npm run build\n<\/code><\/pre>/);
  assert.match(rendered, /class="markdown-code-copy" title="复制代码" aria-label="复制代码"/);
  assert.match(rendered, /<span class="markdown-code-copy-label">复制<\/span>/);
});

test("only block code gets a copy button, inline code stays untouched", () => {
  const rendered = renderToStaticMarkup(
    React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm], components: markdownCodeComponents }, "run `npm ci` first"),
  );
  assert.match(rendered, /<code>npm ci<\/code>/);
  assert.doesNotMatch(rendered, /markdown-code-block/);
});

test("indented and quoted code blocks each get exactly one button", () => {
  // 缩进（4 空格）代码块没有 language 类名，走的是同一条 pre 渲染路径。
  const indented = renderToStaticMarkup(
    React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm], components: markdownCodeComponents }, "    indented code"),
  );
  assert.match(indented, /<pre><code>indented code\n<\/code><\/pre>/);
  assert.equal((indented.match(/<div class="markdown-code-block">/g) ?? []).length, 1);
  assert.equal((indented.match(/class="markdown-code-copy"/g) ?? []).length, 1);

  // 引用块里的代码块：容器出现在 blockquote 内部，按钮数量不能翻倍也不能丢。
  const quoted = renderToStaticMarkup(
    React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm], components: markdownCodeComponents }, "> ```ts\n> const a = 1;\n> ```"),
  );
  assert.match(quoted, /<blockquote>[\s\S]*<div class="markdown-code-block">[\s\S]*<\/blockquote>/);
  assert.equal((quoted.match(/<div class="markdown-code-block">/g) ?? []).length, 1);
  assert.equal((quoted.match(/class="markdown-code-copy"/g) ?? []).length, 1);
});

test("a paragraph mixing inline code and a fenced block gets one button per block", () => {
  const rendered = renderToStaticMarkup(
    React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm], components: markdownCodeComponents }, "跑 `npm ci`,然后:\n\n```bash\nnpm run build\n```\n\n再 `npm test`。"),
  );
  assert.match(rendered, /<code>npm ci<\/code>/);
  assert.match(rendered, /<code>npm test<\/code>/);
  assert.equal((rendered.match(/<div class="markdown-code-block">/g) ?? []).length, 1);
  assert.equal((rendered.match(/class="markdown-code-copy"/g) ?? []).length, 1);
});

test("every markdown entry point wires the code block components", () => {
  for (const [name, source] of Object.entries(entryPoints)) {
    assert.match(source, /import \{ markdownCodeComponents \} from "[^"]*MarkdownCodeBlock";/, `${name} 缺少组件导入`);
    // 共享映射必须展开在前面：入口自己的 a/img（以及将来可能的 pre）要能覆盖它，
    // 否则单点定制会被静默吃掉。
    assert.match(source, /components=\{\{ \.\.\.markdownCodeComponents, /, `${name} 未把复制按钮并进 components`);
  }
});

test("code copy button reports the real outcome and resets itself", async () => {
  const source = await readFile(new URL("./components/MarkdownCodeBlock.tsx", import.meta.url), "utf8");
  // 点击必须把真实结果（成功 / 失败）落到状态上，而不是「点了没反应」。
  assert.match(source, /setState\(copied \? "copied" : "failed"\)/);
  // 反馈到期必须自动复位，不能一直停在「已复制」。
  assert.match(source, /setTimeout\(\(\) => setState\("idle"\), COPY_FEEDBACK_MS\)/);
  // 卸载要清掉计时器；且 StrictMode 双跑（挂载→清理→再挂载）后标记必须仍是已挂载，
  // 只在 setup 里置 true 才成立——漏掉就会让开发模式的复制反馈静默失效。
  assert.match(source, /mounted\.current = true;/);
  assert.match(source, /clearTimeout\(timer\.current\)/);
  // 三种状态各自有可见文案，失败不能只是沉默。
  assert.match(source, /"已复制"/);
  assert.match(source, /"复制失败"/);
});

test("code copy button floats above the block without breaking code scrolling", () => {
  assert.match(stylesheet, /\.markdown-code-block \{ position: relative; margin: 14px 0; \}/);
  assert.match(stylesheet, /\.markdown-code-block > pre \{ margin: 0; \}/);
  assert.match(stylesheet, /\.markdown-code-copy \{ position: absolute; top: 6px; right: 6px;/);
  // 固定 11px 会在文件预览把字号调到 20px 时偏小，必须带下限地跟随容器字号。
  assert.match(stylesheet, /\.markdown-code-copy \{[^}]*font-size:\s*max\(11px,\s*\.7em\);/s);
  assert.match(stylesheet, /\.markdown-code-copy-icon \{[^}]*width:\s*max\(12px,\s*1em\);/s);
  // 触摸设备没有 hover，必须常显并让出顶部空间，否则按钮会盖住首行代码。
  assert.match(stylesheet, /@media \(hover: none\) \{[\s\S]*?\.markdown-code-copy \{ opacity: 1; \}[\s\S]*?\.markdown-code-block > pre \{ padding-top: 34px; \}[\s\S]*?\n\}/);
});
