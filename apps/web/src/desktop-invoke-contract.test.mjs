import assert from "node:assert/strict";
import test from "node:test";
import { readdir, readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

/**
 * 桌面宿主（Tauri）的 `invoke` 契约：**前端每 invoke 一个命令，`main.rs` 的
 * `invoke_handler` 那张表里就必须有它**。
 *
 * 这个坑本仓已经踩过两次，两次都是同一个形状：前端在调、宿主没注册，点下去只拿到
 * Tauri 的 "command not found"，用户看到的是一个"点了没反应/报错"的按钮：
 *   · `open_app_data_directory`（2026-09-17 复查发现，注释留在 main.rs 里）；
 *   · `open_external`（2026-09-29 复查发现）—— 它是 `@milevia/sdk` 的 `openExternal()`
 *     唯一依赖，缺了它**桌面端所有外链都打不开**，而两个窗口都 Deny 新窗口，
 *     连退回 `window.open` 也救不回来。
 *
 * 两侧没有共享类型（前端是字符串，宿主是宏），**编译期抓不到** —— 所以只能靠这条源码断言。
 * 它读三处：
 *   1. `main.rs` 里 `generate_handler![…]` 的花括号内容（宿主注册表，权威）；
 *   2. `apps/web/src` 下所有 `invoke("…")` 字面量；
 *   3. `packages/sdk/src` 下同上（`open_external` 就写在 SDK 里，web 只是重导出）；
 *   4. `main.rs` 注入给前端的托盘动作表（`__MILEVIA_TRAY_ACTIONS__`）里那些 `invoke('…')`
 *      —— 它们是 Rust 侧写死的字符串，扫前端源码扫不到，但同样是"前端会调"的命令。
 */
const repoRoot = new URL("../../../", import.meta.url); // apps/web/src → 仓库根
const mainRsURL = new URL("apps/desktop/src-tauri/src/main.rs", repoRoot);

const mainRs = await readFile(mainRsURL, "utf8");

/** 宿主注册表：`generate_handler![]` 里的命令名（**只认这一处**，它是权威）。 */
function registeredCommands(source) {
  const table = source.match(/invoke_handler\(tauri::generate_handler!\[([\s\S]*?)\]\)/)?.[1] ?? "";
  return new Set(
    table
      .split(",")
      .map((entry) => entry.trim())
      .filter((entry) => /^[a-z_][a-z0-9_]*$/.test(entry)),
  );
}

/** 递归收集要扫的前端源文件。 */
async function sourceFiles(rootURL, suffixes) {
  const out = [];
  const walk = async (url) => {
    for (const entry of await readdir(url, { withFileTypes: true })) {
      const child = new URL(`${entry.name}${entry.isDirectory() ? "/" : ""}`, url);
      if (entry.isDirectory()) {
        await walk(child);
        continue;
      }
      if (suffixes.some((suffix) => entry.name.endsWith(suffix))) out.push(child);
    }
  };
  await walk(rootURL);
  return out;
}

/**
 * 剥掉注释再扫。**必须先剥**：注释里会写出调用形态（包括本文件自己那段说明），
 * 不剥就会把"示例/被注释掉的调用"当成真调用 —— 本仓为此吃过两次亏
 * （见 `agent-registry.test.mjs` 里那条"负向断言必须先剥注释"）。
 */
function stripComments(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:])\/\/.*$/gm, "$1");
}

/** 从一段源码里抠出全部 invoke 调用的命令名（支持双引号/单引号/反引号）。 */
function invokedCommands(source) {
  return [...stripComments(source).matchAll(/invoke\(\s*["'`]([a-z_][a-z0-9_]*)["'`]/g)].map(
    (match) => match[1],
  );
}

test("前端 invoke 的每个命令都出现在宿主的 invoke_handler 里", async () => {
  const registered = registeredCommands(mainRs);
  assert.ok(registered.size >= 10, `解析出的注册表太小（${registered.size} 条），锚点可能已经失配`);

  const files = [
    ...(await sourceFiles(new URL("apps/web/src/", repoRoot), [".ts", ".tsx", ".mjs"])),
    ...(await sourceFiles(new URL("packages/sdk/src/", repoRoot), [".ts", ".tsx"])),
  ];
  assert.ok(files.length > 50, `扫到的前端源文件太少（${files.length} 个），路径可能写错了`);

  const invoked = new Map(); // 命令名 → 出现的文件（报错时要指出是哪儿在调）
  for (const file of files) {
    const relPath = fileURLToPath(file).slice(fileURLToPath(repoRoot).length).replaceAll("\\", "/");
    for (const name of invokedCommands(await readFile(file, "utf8"))) {
      if (!invoked.has(name)) invoked.set(name, relPath);
    }
  }

  // Rust 侧注入的托盘动作表：它是给前端用的 invoke 包装，命令名写在**字符串**里，
  // 扫前端源码扫不到。扫整个 main.rs 的单引号形态即可 —— 全文件只有那一处这样写。
  // （别拿 `__MILEVIA_TRAY_ACTIONS__` + `writable: false` 去框它：文件里前面还有一处
  //  `writable: false`（注入运行时配置那行），非贪婪匹配会只吃到前半段、扫出 0 条 ——
  //  我第一版就是这么写的，靠下面那条"至少 5 条"的断言才发现。）
  const injectedNames = [...mainRs.matchAll(/invoke\('([a-z_]+)'\)/g)].map((match) => match[1]);
  assert.ok(
    injectedNames.length >= 5,
    `托盘动作表只扫到 ${injectedNames.length} 条 invoke，锚点/形态可能已经变了（这条不许退化成空转）`,
  );
  for (const name of injectedNames) invoked.set(name, "apps/desktop/src-tauri/src/main.rs（托盘动作表）");

  // 反向对照：扫不到任何调用，说明锚点失效，这条断言就成了空转。
  assert.ok(invoked.size > 0, "没扫到任何 invoke 调用，锚点已失配");

  const missing = [...invoked.entries()].filter(([name]) => !registered.has(name));
  assert.deepEqual(
    missing,
    [],
    `这些命令前端在调、宿主没注册（点下去只会得到 Tauri 的 "command not found"）：${missing
      .map(([name, where]) => `${name} ← ${where}`)
      .join("；")}`,
  );
});

test("这批修复依赖的两个命令确实注册上了", () => {
  // 上一条是"通用断言"，它依赖"确实有人在调"。这两个是本仓**真的漏过**的，
  // 单独钉住：将来有人删掉调用点、或把注册删了，这条会直接点名。
  const registered = registeredCommands(mainRs);
  for (const name of ["open_external", "open_app_data_directory"]) {
    assert.ok(registered.has(name), `${name} 不在 invoke_handler 表里`);
  }
  // open_external 的白名单：它是唯一会被交给系统协议处理器的入口。三档放行的都是
  // "交出去有确定去处"的协议（http/https → 浏览器，mailto: → 邮件客户端），
  // 下面那条反向断言比正向更重要 —— 别哪天顺手把 file:/javascript: 也放进来。
  const body = mainRs.match(/fn open_external\(url: String\)[\s\S]*?\n\}/)?.[0] ?? "";
  assert.match(body, /"http"/, "open_external 丢掉了 http 白名单");
  assert.match(body, /"https"/, "open_external 丢掉了 https 白名单");
  assert.match(body, /"mailto"/, "open_external 丢掉了 mailto 白名单");
  for (const scheme of ["file", "javascript", "tel", "ftp"]) {
    assert.doesNotMatch(body, new RegExp(`"${scheme}`), `open_external 放行了 ${scheme}:（详见 main.rs 的注释）`);
  }
});
