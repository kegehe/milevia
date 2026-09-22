// main.tsx 的 Service Worker 接线测试。
//
// 为什么值得测：这次修复的全部效果都压在 main.tsx 那几行上 —— 策略算得再对，
// 只要接线写反（把 purge 分支接到注册、或者绕过策略直接注册），白屏就会原样回来，
// 而单测覆盖不到 React 入口文件。这里按仓库既有做法做源码级断言（同
// settings-entry.test.mjs），把「接线正确」固定下来。

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const mainSource = await readFile(new URL("./main.tsx", import.meta.url), "utf8");

test("main.tsx 用策略函数决定 SW 行为，而不是无条件注册", () => {
  assert.match(mainSource, /import \{ purgeServiceWorkerState, serviceWorkerPolicy \} from "\.\/lib\/service-worker"/);
  assert.match(mainSource, /const serviceWorkerDecision = serviceWorkerPolicy\(\{/);
  assert.match(
    mainSource,
    /serviceWorkerDecision === "purge"[\s\S]{0,60}purgeServiceWorkerState\(\)/,
    "purge 分支必须真的去注销 + 清缓存",
  );
});

test("注册只出现在 register 分支里，且 purge 分支优先", () => {
  const registerCalls = mainSource.match(/serviceWorker\.register\(/g) ?? [];
  assert.equal(registerCalls.length, 1, "注册只能有一处，否则可以绕过策略");

  const purgeBranch = mainSource.indexOf('serviceWorkerDecision === "purge"');
  const registerBranch = mainSource.indexOf('serviceWorkerDecision === "register"');
  assert.ok(purgeBranch > -1, "缺少 purge 分支");
  assert.ok(registerBranch > -1, "缺少 register 分支");
  // 顺序即优先级：应用外壳必须先判成 purge，绝不能有机会落到注册分支。
  assert.ok(purgeBranch < registerBranch, "purge 分支必须排在 register 之前");
  assert.ok(
    mainSource.indexOf("serviceWorker.register(") > registerBranch,
    "注册调用必须位于 register 分支之内",
  );
});

test("桌面端 / 原生端 / 托盘三个输入都传给了策略", () => {
  assert.match(mainSource, /isTray,/);
  assert.match(mainSource, /isDesktop: isDesktop\(\)/);
  assert.match(mainSource, /isNativePlatform: Capacitor\.isNativePlatform\(\)/);
  assert.match(mainSource, /protocol: window\.location\.protocol/);
});
