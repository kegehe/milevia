// 横幅的渲染断言。
//
// 走的是本仓既有的「读源码钉行为」路子（同 `ProjectRunPanel.test.mjs`）：组件挂在
// Tauri 的 `invoke` 上，node:test 里跑不起来；而这里要守的恰恰是"界面上到底写没写
// 那句话"，纯函数测不到。
//
// 回归的是一条**死字段**：Rust 侧一直在往 `download.error` 写后台下载失败的原因
// （还专门做了中文化），前端却没有任何读者 —— 静默下载失败时横幅只说一句"后台下载
// 未完成"，用户拿不到任何原因。这里钉住"它必须被渲染出来"。
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./UpdateBanner.tsx", import.meta.url), "utf8");

test("后台下载失败的原因要渲染在横幅上，不能只说「未完成」", () => {
  assert.match(source, /const downloadError = downloadFailureReason\(status\);/);
  // 原因接在"后台下载未完成"后面，而不是被丢掉
  assert.match(
    source,
    /后台下载未完成\$\{downloadError \? `：\$\{downloadError\}` : ""\}，点击后重新下载并安装/,
  );
});

test("横幅那一行是 nowrap + 省略号，长原因被截断时得能从 title 里读到全文", () => {
  assert.match(source, /title=\{failed && downloadError \? statusLine : undefined\}/);
  // 没有失败原因时不留一个空 title（悬停弹出"当前 v0.1.7"这种无意义提示）
  assert.doesNotMatch(source, /title=\{downloadError\}/);
});
