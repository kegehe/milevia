import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

/**
 * `openExternal` 的行为 —— 桌面端外链的**唯一**入口。
 *
 * `desktop-invoke-contract.test.mjs` 只证明"宿主注册了 `open_external`"（源码断言），
 * 证明不了"前端点下去真的会去 invoke 它"。中间还隔着三层，任何一层判错，表现都一样是
 * **点了没反应**，而且全都不报错：
 *   1. `getDesktopRuntime()` 认不认当前环境（不认 → 走 Web 分支）；
 *   2. `window.__TAURI_INTERNALS__` 在不在（不在 → 退回 `window.open`，桌面端被宿主
 *      `on_new_window(Deny)` 吃掉）；
 *   3. 协议的白名单放不放行（**分平台**：桌面端 http(s)/mailto，Web 端只 http(s)）。
 *
 * 环境靠**给 `globalThis.window` 打桩**造出来（SDK 里读的是裸 `window`）。因此这里用
 * 动态 `import()`：静态导入会被提升到设桩之前，第一次调用就会读到"没有 window"。
 * 桩只在测试体内设置、结束即删，避免与同文件其它用例互相看见。
 */

type InvokeCall = { command: string; args?: Record<string, unknown> };

const DESKTOP_RUNTIME = {
  apiBase: "http://127.0.0.1:1/",
  wsBase: "ws://127.0.0.1:1/",
  sessionToken: "test-token",
};

/** 造一个 Tauri WebView 形状的全局环境；返回记账用的两个数组。 */
function stubWindow(options: { desktop?: boolean; internals?: boolean; invokeFails?: boolean } = {}) {
  const invoked: InvokeCall[] = [];
  const opened: string[] = [];
  const win: Record<string, unknown> = {
    open: (url: string) => {
      opened.push(url);
      return { focus() {} };
    },
  };
  if (options.desktop !== false) win.__MILEVIA_DESKTOP_RUNTIME__ = DESKTOP_RUNTIME;
  if (options.internals !== false) {
    win.__TAURI_INTERNALS__ = {
      invoke: async (command: string, args?: Record<string, unknown>) => {
        invoked.push({ command, args });
        if (options.invokeFails) throw new Error("command not found");
        return undefined;
      },
    };
  }
  (globalThis as { window?: unknown }).window = win;
  return {
    invoked,
    opened,
    restore: () => {
      delete (globalThis as { window?: unknown }).window;
    },
  };
}

/** 每个用例都从 SDK 取一次函数（模块本身无副作用，取多次是同一个引用）。 */
async function loadOpenExternal() {
  const sdk = await import("@milevia/sdk");
  return sdk.openExternal;
}

test("桌面端：调宿主的 open_external，命令名与参数名都要对得上", async () => {
  const env = stubWindow();
  try {
    await (await loadOpenExternal())("https://example.com/a?b=1&c=2");
    // 负向锚点先行：桩没被扣枪的话，下面的 deepEqual 就是空转通过。
    assert.equal(env.invoked.length, 1, "没有 invoke 宿主，桌面端点下去就是没反应");
    assert.equal(env.invoked[0]?.command, "open_external");
    // 参数名必须与 `fn open_external(url: String)` 的形参一致：Tauri 按名字取参，
    // 对不上是运行期报错，编译期两侧都看不见。
    assert.deepEqual(env.invoked[0]?.args, { url: "https://example.com/a?b=1&c=2" });
    // 桌面端不许走 window.open：它会被宿主的新窗口策略吃掉，且**没有任何报错**。
    assert.deepEqual(env.opened, [], "桌面端退回了 window.open");
  } finally {
    env.restore();
  }
});

test("Web 端：开新标签页，且不去碰不存在的宿主桥", async () => {
  const env = stubWindow({ desktop: false, internals: false });
  try {
    await (await loadOpenExternal())("https://example.com/");
    assert.deepEqual(env.opened, ["https://example.com/"], "Web 端没有开新标签页");
    assert.deepEqual(env.invoked, [], "Web 端不该 invoke 宿主命令");
  } finally {
    env.restore();
  }
});

test("桌面端只放行 http(s)/mailto：其余协议与解析不了的字符串一律不动手", async () => {
  const env = stubWindow();
  try {
    const openExternal = await loadOpenExternal();
    await openExternal("mailto:foo@example.com");
    assert.deepEqual(
      env.invoked.map((call) => call.args),
      [{ url: "mailto:foo@example.com" }],
      "mailto: 没有交给宿主（桌面端就只能是一片死链）",
    );
    env.invoked.length = 0;

    // 这个判据是安全边界：open_external 会把字符串交给系统协议处理器。
    // `//host` 也在这一档 —— `new URL("//host")` 会抛，所以调用方必须**先归一成绝对地址**
    // （`lib/external-link.ts` 就是这么做的），别指望这里兜。
    for (const url of ["javascript:alert(1)", "file:///C:/Windows/win.ini", "ftp://example.com/f", "tel:+8613800000000", "//example.com/pr", "不是链接", ""]) {
      await openExternal(url);
    }
    assert.deepEqual(env.invoked, [], "不该动手的字符串被交给了宿主");
    assert.deepEqual(env.opened, [], "不该动手的字符串被交给了 window.open");
  } finally {
    env.restore();
  }
});

test("Web 端白名单不动：mailto: 与其余协议都不经 window.open", async () => {
  // Web 端这一档落到 window.open，浏览器拿到自己处理不了的协议只会多出一个空标签页；
  // 而邮箱在浏览器里原生就能点 —— 所以这档必须与改动前**逐字一致**。
  const env = stubWindow({ desktop: false, internals: false });
  try {
    const openExternal = await loadOpenExternal();
    for (const url of ["mailto:foo@example.com", "javascript:alert(1)", "file:///C:/Windows/win.ini", "//example.com/pr"]) {
      await openExternal(url);
    }
    assert.deepEqual(env.opened, [], "Web 端把不该接管的协议交给了 window.open");
    assert.deepEqual(env.invoked, []);
  } finally {
    env.restore();
  }
});

test("宿主回绝时退回 window.open（桌面端此刻是空动作，属已知兜底）", async () => {
  const env = stubWindow({ invokeFails: true });
  try {
    await (await loadOpenExternal())("https://example.com/");
    assert.deepEqual(env.invoked.map((call) => call.command), ["open_external"], "没有先试宿主");
    assert.deepEqual(env.opened, ["https://example.com/"], "invoke 失败后没有退回 window.open");
  } finally {
    env.restore();
  }
});

/**
 * 桌面端会渲染到的外链调用点，必须都接在这条通路上。
 *
 * 上面几条钉的是"通路本身对不对"，这条钉的是"**调用点有没有走它**" —— 本批修复前的形状
 * 恰恰是后者：SDK 里的 `openExternal()` 早就写好了，好几个入口却各自渲染裸
 * `<a target="_blank">`，桌面端被两个窗口的 `NewWindowResponse::Deny` 吃掉；通路再对，
 * 没人走也一样是"点了没反应"。
 *
 * 只认**接线形状**（源码文本，先剥注释）：这几处的判据是"有没有接上"，不是"长什么样"，
 * 所以正则只框到 `ExternalLink`/`openExternal` 那一层，允许周围属性继续演进。
 * CLI 页的授权链接不在这里 —— 它由 `cli-tools-page.test.mjs` 单独钉着。
 */
test("外链调用点都接了 ExternalLink / openExternal，没有留在裸锚点上", async () => {
  const cases = [
    { file: "pages/ConversationPage.tsx", wired: /<ExternalLink[^>]*href=\{href\}/, why: "AI 回复的 Markdown 正文" },
    { file: "pages/ConversationPage.tsx", wired: /<ExternalLink[^>]*href=\{externalImage\}/, why: "正文里的外链图片" },
    { file: "pages/OrchestrationPage.tsx", wired: /<ExternalLink[^>]*href=\{href\}/, why: "编排页的消息正文" },
    { file: "features/files/FileViewer.tsx", wired: /<ExternalLink[^>]*href=\{safeHref\(href\)\}/, why: "文件页的 Markdown" },
    { file: "features/run/ProjectRunPanel.tsx", wired: /<ExternalLink[^>]*href=\{part\.url\}/, why: "运行日志里的链接" },
    { file: "pages/McpManagerPage.tsx", wired: /await openExternal\(started\.authorizationUrl\)/, why: "MCP 授权页（连接向导 / 重新授权）" },
  ];
  for (const { file, wired, why } of cases) {
    const source = (await readFile(new URL(file, import.meta.url), "utf8"))
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/(^|[^:])\/\/.*$/gm, "$1");
    assert.match(source, wired, `${file} 的「${why}」没有接在 openExternal 通路上（桌面端点下去没反应）`);
  }
  // 反向锚点：上面那些 `wired` 万一框得太松（比如整段被删掉仍能匹配），这条兜住
  // —— 只要这些文件里还留着"裸锚点 + target=_blank"的旧形状，就说明有入口没改过来。
  for (const file of new Set(cases.map((entry) => entry.file))) {
    const source = (await readFile(new URL(file, import.meta.url), "utf8"))
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/(^|[^:])\/\/.*$/gm, "$1");
    assert.doesNotMatch(source, /<a\s[^>]*target="_blank"/, `${file} 里仍有裸 <a target="_blank">（桌面端点下去没反应）`);
  }
});

test("ExternalLink 挂了 onAuxClick 且用 lib/external-link 的判据", () => {
  // 判据表本身由 lib/external-link.test.ts 逐格钉住，但"组件到底挂没挂中键"它管不着：
  // 中键只发 `auxclick`、不发 `click`，漏了 onAuxClick 就是**桌面端中键死链**（实测过），
  // 而这条只有真点一下（`.tmp/link-click/` 那份夹具）或这条源码断言能发现。
  return readFile(new URL("components/ExternalLink.tsx", import.meta.url), "utf8").then((source) => {
    assert.match(source, /onAuxClick=/, "ExternalLink 没有挂 onAuxClick：桌面端中键会退回死链");
    assert.match(source, /decideExternalLinkClick/, "ExternalLink 没有走 lib/external-link 的判据");
  });
});

test("手机页正文外链分平台：手机保留裸锚点，桌面壳走 ExternalLink", async () => {  // 这是唯一**必须**分平台的地方，两条路都不能少：
  //   · 桌面壳里这一页会渲染（首页「远程控制」→ /mobile），裸 target="_blank" 会被 Deny 吃掉；
  //   · 手机上却必须留裸锚点 —— Capacitor 8 没开 supportMultipleWindows，`window.open` 是
  //     空操作，外链正是靠裸锚点走 `shouldOverrideUrlLoading → launchIntent` 进系统浏览器的。
  // 所以这条同时钉"桌面那条接上了"与"手机那条没被删掉"。
  const source = await readFile(new URL("pages/MobileRemotePage.tsx", import.meta.url), "utf8");
  assert.match(source, /<MobileMarkdownLink href=\{href\}>/, "手机页的正文外链没有接上 MobileMarkdownLink");
  assert.match(source, /isDesktop\(\)\) return <ExternalLink/, "MobileMarkdownLink 丢了桌面那条分支");
  assert.match(source, /<a href=\{href\} target="_blank"/, "MobileMarkdownLink 丢了手机/Web 那条裸锚点（手机上链接会失效）");
});

test("判据放行 ⇒ SDK 一定接受：两处读同一份协议白名单", async () => {
  // 这条钉的是**跨模块不变量**：判据说"这个地址能开"之后，`ExternalLink` 会先 preventDefault
  // 再调 openExternal —— 万一 SDK 那边把协议筛掉了，结果是"点击被吞掉、什么也没发生"，
  // 正是这个问题单最初的样子。
  // ⚠️ 说清楚它的检测力：白名单现在**只有一处**（SDK 的 `externalLinkProtocols`），判据直接
  // 调它，所以"放行⇒接受"是结构性成立的 —— 下面那张表**抓不到**"有人偷偷另写一份"，
  // 真正抓这件事的是前面那条源码断言（`lib/external-link.ts` 必须 import 它）。
  // 表本身留着，是给"有人放宽形状判据 / 改白名单"时当回归用的。
  const judgeSource = await readFile(new URL("lib/external-link.ts", import.meta.url), "utf8");
  assert.match(
    judgeSource,
    /import \{ externalLinkProtocols \} from "@milevia\/sdk"/,
    "外链判据没有读 SDK 的共享白名单（自己另写一份必然与实际放行的那份漂移）",
  );

  const [{ decideExternalLinkClick }, sdk] = await Promise.all([import("./lib/external-link"), import("@milevia/sdk")]);
  const hrefs = [
    "https://a.example/x",
    "http://a.example/",
    "mailto:a@b.c",
    "//a.example/p",
    "ftp://a.example/f",
    "file:///C:/x",
    "javascript:void(0)",
    "tel:+8613800000000",
    "https://",
    "//a.example:99999/x",
    "/projects/1",
    "#anchor",
    "",
  ];
  for (const desktop of [true, false]) {
    const allowed = sdk.externalLinkProtocols(desktop);
    for (const href of hrefs) {
      const decision = decideExternalLinkClick(href, { desktop, button: 0 });
      if (decision.action !== "open-external") continue;
      const protocol = new URL(decision.url).protocol;
      assert.ok(
        allowed.includes(protocol),
        `${desktop ? "桌面" : "Web"}端判据放行了 ${href}（解析出 ${protocol}），SDK 白名单却是 ${allowed.join(" / ")}`,
      );
    }
  }
});
