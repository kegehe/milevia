import assert from "node:assert/strict";
import test from "node:test";

import { buildTimeline, eventDiagnostic } from "./timeline.ts";
import type { Event, TimelineItem } from "./types.ts";

const at = "2026-07-30T11:29:00.000Z";

function event(id: string, type: string, payload: unknown): Event {
  return { id, type, payload, runId: "run-1", createdAt: at };
}

test("localizes 'No completion record' background-task notices in Chinese", () => {
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", summary: 'No completion record was found for background agent "审查News/订阅/日报页面" from the previous session. It may have been stopped, or it may have been running when the previous Claude Code process exited — either way its transcript is saved on disk, so its progress is not lost.' }),
  ]);
  // 从 TimelineItem 直接提取 system 成员，别手抄形状：手写谓词要么漏字段、要么把
  // variant 写成宽的 string，会因不可赋值给 TimelineItem 而整段收窄失效（TS2677）。
  const system = timeline.find((item): item is Extract<TimelineItem, { kind: "system" }> => item.kind === "system");
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台代理未找到完成记录");
  assert.equal(system.system.detail, "「审查News/订阅/日报页面」可能仍在运行，或其进程已在会话退出后终止。");
});

test("keeps template detail even when taskStatus already decides the title", () => {
  // 当 payload.status 标记失败、且 summary 是英文 Background command 模板时，
  // 标题由状态决定，但 detail 必须仍保留退出码与命令名（不可丢失）。
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", status: "failed", summary: 'Background command "npm run build" failed (exit code 1)' }),
  ]);
  const system = timeline.find((item) => item.kind === "system") as { kind: "system"; system: { title: string; detail?: string } } | undefined;
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台任务失败");
  assert.equal(system.system.detail, "失败（退出码 1）npm run build");
});

test("recordMatch overrides a contradictory failed taskStatus (extreme combo)", () => {
  // 极端防御：CLI 同时发 status=failed 又发 No-completion-record 摘要时，
  // 后者是更具体的中性特征，应主导标题与状态，避免「失败标题 + 中性正文」的自相矛盾。
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", status: "failed", summary: 'No completion record was found for background agent "X" from the previous session.' }),
  ]);
  const system = timeline.find((item) => item.kind === "system") as { kind: "system"; system: { title: string; detail?: string; metadata?: { state?: string } } } | undefined;
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台代理未找到完成记录");
  assert.equal(system.system.detail, "「X」可能仍在运行，或其进程已在会话退出后终止。");
  assert.equal(system.system.metadata?.state, "info");
});

test("keeps unrecognized background-task summaries verbatim (graceful fallback)", () => {
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", summary: "Some brand new CLI wording that changes constantly" }),
  ]);
  const system = timeline.find((item) => item.kind === "system") as { kind: "system"; system: { title: string; detail?: string } } | undefined;
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台任务通知");
  assert.equal(system.system.detail, "Some brand new CLI wording that changes constantly");
});

test("appends original English errors after the Chinese fallback", () => {
  const timeline = buildTimeline([], [
    event("detail", "error", { message: "Authentication expired. Run codex login." }),
    event("terminal", "run.failed", { error: "Codex exited: exit status 1" }),
  ]);

  const errors = timeline.filter((item) => item.kind === "error");
  assert.deepEqual(errors.map((item) => [item.title, item.detail]), [["执行错误", "任务执行失败，请查看任务日志后重试。：Authentication expired. Run codex login."]]);
});

test("appends original English turn failure details after the Chinese fallback", () => {
  const detailed = buildTimeline([], [event("turn", "turn.failed", { error: { message: "Request rejected by the service" } })]);
  assert.deepEqual(detailed.filter((item) => item.kind === "error").map((item) => item.detail), ["任务执行失败，请查看任务日志后重试。：Request rejected by the service"]);

  const fallback = buildTimeline([], [event("terminal", "run.failed", { error: "Codex exited: exit status 1" })]);
  assert.deepEqual(fallback.filter((item) => item.kind === "error").map((item) => item.detail), ["任务执行失败，请查看任务日志后重试。：Codex exited: exit status 1"]);
});

test("keeps a Chinese-led runner failure verbatim instead of re-wrapping it", () => {
  // 服务端已经把这类错误本地化过了（中文包装 + Go 的英文退出描述）。判据是“含失败就直通”，
  // 与 app.go 的 localizedErrorText 逐条一致。少了这条，同一句错误在桌面端会多套一层
  // “任务执行失败，请查看任务日志后重试。”，与手机端给出的形态不一致。
  const timeline = buildTimeline([], [event("terminal", "run.failed", { error: "Codex 运行失败：exit status 1" })]);
  const details = timeline.filter((item) => item.kind === "error").map((item) => item.detail);
  assert.deepEqual(details, ["Codex 运行失败：exit status 1"]);
});

test("defers a bare Codex exit status in both wordings", () => {
  // 这个判据原先没有任何用例守着。它必须同时认中文与英文两种措辞：英文那份是 events 表里
  // 的历史事件（回放时原文不变），中文那份是新服务端产出的形态。
  for (const wording of ["Codex exited: exit status 1", "Codex 运行失败：exit status 1"]) {
    const diagnostic = eventDiagnostic(event("terminal", "run.failed", { error: wording }));
    assert.ok(diagnostic, `没有解析出诊断：${wording}`);
    assert.equal(diagnostic.deferFallback, true, `没有被判定为可延后：${wording}`);
  }
  // 带上可读原因就不再是“只有退出码”，不该被压着。
  const detailed = eventDiagnostic(event("terminal", "run.failed", { error: "Codex 运行失败：exit status 1（CLI：rate limit）" }));
  assert.equal(detailed?.deferFallback, false);
});

test("coalesces multiple CLI stderr events from one run into one diagnostic", () => {
  const timeline = buildTimeline([], [
    event("stderr-1", "stderr", { message: "> @milevia/web@0.0.1 build" }),
    event("stderr-2", "stderr", { message: "src/features/tasks/TaskQueue.tsx(534,10): error TS2554" }),
    event("stderr-3", "stderr", { message: "src/features/tasks/TaskQueue.tsx(568,234): error TS2554" }),
  ]);

  const errors = timeline.filter((item) => item.kind === "error");
  assert.equal(errors.length, 1);
  assert.match(errors[0].detail, /@milevia\/web@0\.0\.1 build/);
  assert.match(errors[0].detail, /TS2554/);
  assert.match(errors[0].detail, /\n/);
});

test("renders decoded WSL launcher warning as readable detail, no fallback prefix", () => {
  // wsl.exe 的 UTF-16LE 主机侧警告经服务端解码后是含技术术语（wsl/localhost/NAT）的
  // 纯中文；不应被当作"未翻译英文"而包上"工具输出异常：..." 前缀。
  //
  // 这三个词是前端**多出来**的（app.go 的白名单里没有它们），只挂在"本地化不经手"的
  // 原始输出链上（rawOutputTerms）。别把它们挪进默认表：那样 run.failed / error 的判定
  // 就与服务端不同表了 —— 见下面那条同表用例。
  const timeline = buildTimeline([], [
    event("stderr-wsl", "stderr", { message: "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。" }),
  ]);
  const errors = timeline.filter((item) => item.kind === "error");
  assert.equal(errors.length, 1);
  assert.equal(errors[0].title, "CLI 输出");
  assert.equal(errors[0].detail, "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。");
});

// 原始输出的宽表不许外溢到**服务端已经判过**的诊断上。
//
// 两边是两条不同的链：
//  - rawOutputTerms（宽表）：工具结果、Codex 输出、聚合 stderr。服务端在这些链上只做搬运
//    （tool_result / item.* 原样转发，stderr 逐行发事件且刻意不进快照），本地化根本没经手，
//    所以口径由前端自己定，取宽一点 —— 一条含 wsl/NAT 的可读中文不该被套前缀。
//  - allowedTechnicalTerms（窄表 = app.go 那份）：run.failed / error / turn.failed /
//    stream.error 的 detail 在服务端已经被 localizedErrorText 判过一道，前端再判时必须
//    同表，否则同一条错误在两端形态不同。
test("原始输出的宽表不外溢到服务端已判过的诊断上", () => {
  const wslNotice = "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。";

  // 宽表这一侧：工具结果（原始输出）原样保留。
  const timeline = buildTimeline([], [
    event("a1", "assistant", { message: { content: [{ type: "tool_use", id: "tu1", name: "Bash", input: { command: "ls" } }] } }),
    event("u1", "user", { message: { content: [{ type: "tool_result", tool_use_id: "tu1", is_error: true, content: wslNotice }] } }),
  ]);
  const toolItem = timeline.find((item): item is Extract<TimelineItem, { kind: "tool" }> => item.kind === "tool");
  assert.ok(toolItem, "expected a tool item");
  assert.equal(
    toolItem.action.output?.content,
    wslNotice,
    "工具结果这条原始输出链被换成了窄表 —— 可读的中文又被套上了兜底前缀",
  );

  // 窄表那一侧：同一条文案若走**服务端已判过**的诊断链，前端必须用窄表 —— 也就是把 wsl
  // 也当成残留英文（服务端实测就是这么判的）。
  //
  // ⚠️ 这一句里不能出现"失败"二字：那个分支在查白名单**之前**就 return 了，写进去会把
  // 这张表的作用整个遮掉，断言再怎么写都恒真（这条用例的第一版就是这个毛病）。
  const diagnostic = eventDiagnostic(event("e1", "run.failed", {
    error: "说明：wsl 已完成，请稍后重试。",
  }));
  assert.equal(
    diagnostic?.detail,
    "任务执行失败，请查看任务日志后重试。：说明：wsl 已完成，请稍后重试。",
    "诊断链上 wsl 被放过了 —— 前端在这里用了宽表，而服务端用的是窄表",
  );
});

// 残留英文的判据必须与 app.go 的 containsUntranslatedEnglish **同表同算法**。
//
// 2026-10-08 之前这里用的是一份**不同的表**：多了 NAT/localhost/wsl（服务端表里没有，
// 实测服务端会把它们判成残留英文），少了 NUL。于是 `含 NUL 说明` 这类纯中文文案在服务端
// 原样直通、在前端却被套上"任务执行失败，请查看任务日志后重试。"，`wsl/NAT/localhost`
// 则反过来 —— 同一个错误在任务卡、通知与桌面/手机时间线上形态不一致。
//
// 注意判据的**长度门槛两端一致**：1~2 个字母的拉丁片段（no / ls / px）两边都放过，
// 不要试图"顺手收紧"成 {1,} —— 那会和服务端分叉，而且是更难查的一种分叉。
test("英文残留判据与服务端同表：NUL 放过，NAT/localhost/wsl 也算残留", () => {
  const passes = (text: string) => eventDiagnostic(event("e", "run.failed", { error: text }))?.detail;

  // NUL 在 app.go 的白名单里 → 两端都必须原样直通。
  assert.equal(
    passes("环境变量名称不能为空，且不能包含 = 或 NUL 字符"),
    "环境变量名称不能为空，且不能包含 = 或 NUL 字符",
    "NUL 没被放过 —— 词表比 app.go 少了一个词（原先就是少了它）",
  );

  // NAT / localhost / wsl 不在 app.go 的白名单里 → 两端都必须套兜底前缀。
  // 这三个词只允许出现在 rawOutputTerms（原始输出那几条链），不许回到本地化这条链上。
  for (const term of ["NAT", "localhost", "wsl"]) {
    assert.equal(
      passes(`说明：${term} 已完成`),
      `任务执行失败，请查看任务日志后重试。：说明：${term} 已完成`,
      `${term} 被当成技术术语放过了 —— 它只属于 rawOutputTerms，不属于本地化这条链`,
    );
  }

  // 门槛长度也别动：3 个字母的残留要判为残留，1~2 个的两端一样放过。
  assert.equal(passes("说明：var 已完成"), "任务执行失败，请查看任务日志后重试。：说明：var 已完成");
  assert.equal(passes("操作完成：见 no 目录"), "操作完成：见 no 目录");
});

// 断词规则也必须与服务端一致：Go 只按 ASCII 字母切词，数字与下划线不是词字符。
//
// 所以 `NUL2` 会被切成白名单里的 `NUL` + 残留的 `2`，**直通**。旧写法用的是 `\b`，而 JS
// 的 \b 把数字/下划线也算 \w —— `NUL2` 既 replace 不掉、又整体成一个 ≥3 字母段，被判成
// 残留英文。实测这四个形态在服务端全是直通，前端也必须一样。
test("断词规则与服务端一致：数字/下划线紧邻白名单词仍算直通", () => {
  const passes = (text: string) => eventDiagnostic(event("e", "run.failed", { error: text }))?.detail;
  for (const text of ["说明：NUL2 已完成", "说明：NUL_x 已完成", "说明：JSON2 已完成", "说明：2NUL 已完成"]) {
    assert.equal(
      passes(text),
      text,
      `${text} 被判成了残留英文 —— 断词又用回 \\b 了（前端的 \\b 把数字/下划线也算词字符，服务端不算）`,
    );
  }
  // 反面：紧邻数字只是**不断词**，不等于放过真正的英文残留。
  assert.equal(passes("说明：var2 已完成"), "任务执行失败，请查看任务日志后重试。：说明：var2 已完成");
});

// 白名单是一份**拷贝**：app.go 那边加/删一个词，这里必须同步，否则两端形态又分叉。
// 与其等它静默漂移，不如直接从 app.go 把词表读出来比。读不到文件（例如只拷了 web 目录
// 的环境）就跳过，别把一个文件系统依赖变成红灯。
//
// ⚠️ 必须双向比（`deepEqual` 集合相等），不能只做"服务端每个词前端都放过"：
// **多出来的词才是最初那起 bug 的成因**（NAT/localhost/wsl 就是这么跑偏的），
// 单向检查对它完全无感。这条用例的第一版就是单向的。
test("allowedTechnicalTerms 与 app.go 的词表集合相等（双向，多的词也算漂移）", async () => {
  const { readFile } = await import("node:fs/promises");
  const source = await readFile(
    new URL("../../../control-server/internal/app/app.go", import.meta.url),
    "utf8",
  ).catch(() => "");
  if (!source) return;

  const body = source.slice(source.indexOf("func containsUntranslatedEnglish"));
  assert.ok(body.length > 0, "app.go 里找不到 containsUntranslatedEnglish");
  const block = body.slice(0, body.indexOf("}"));
  const serverTerms = [...block.matchAll(/"([A-Za-z]+)":\s*true/g)].map((match) => match[1]);
  // 锚点自证：抠不到就说明解析退化了，别让它悄悄对一个空集通过（同 mobile-remote-agent.test.mjs 的写法）。
  assert.ok(serverTerms.length >= 10, `从 app.go 解析出的词表太短：${JSON.stringify(serverTerms)}`);

  // 前端那张表没有导出（它是实现细节），同样从源码里抠；两个 set 字面量各自一行。
  const frontend = await readFile(new URL("./timeline.ts", import.meta.url), "utf8");
  const narrow = frontend.match(/const allowedTechnicalTerms = new Set\(\[([^\]]*)\]\)/)?.[1] ?? "";
  assert.ok(narrow.includes("NUL"), "抠不到 allowedTechnicalTerms 的字面量（改成多行/换写法了？）");
  const webTerms = [...narrow.matchAll(/"([A-Za-z]+)"/g)].map((match) => match[1]);

  const sort = (terms: string[]) => [...terms].sort();
  assert.deepEqual(
    sort(webTerms),
    sort(serverTerms),
    "前端 allowedTechnicalTerms 与 app.go 的词表不是同一个集合（少一个词或多一个词都算漂移；"
      + "多出来的词正是 NAT/localhost/wsl 那起 bug 的成因）",
  );

  // 行为抽查：证明上面抠出来的字面量**真的**被判定逻辑用着，而不是一个已失效的常量。
  for (const term of serverTerms) {
    const diagnostic = eventDiagnostic(event("t", "run.failed", { error: `说明：${term} 已完成` }));
    assert.equal(
      diagnostic?.detail,
      `说明：${term} 已完成`,
      `${term} 在 app.go 的白名单里，前端却没放过它 —— 表抠对了但没接进判定`,
    );
  }
  assert.equal(
    eventDiagnostic(event("t2", "run.failed", { error: "说明：wsl 已完成" }))?.detail,
    "任务执行失败，请查看任务日志后重试。：说明：wsl 已完成",
    "本地化这条链上 wsl 被放过了 —— 宽表（rawOutputTerms）外溢了",
  );
});

// 宽表只能等于"窄表 + 那三个原始输出专用词"。写死比较，避免以后有人往宽表里再塞词
// 而没人发现（宽表一旦长出新词，本地化链与原始输出链的分界就没人守了）。
test("rawOutputTerms 只比窄表多 NAT / localhost / wsl 三个词", async () => {
  const { readFile } = await import("node:fs/promises");
  const frontend = await readFile(new URL("./timeline.ts", import.meta.url), "utf8").catch(() => "");
  if (!frontend) return;
  const raw = frontend.match(/const rawOutputTerms = new Set\(\[\.\.\.allowedTechnicalTerms,([^\]]*)\]\)/)?.[1] ?? "";
  assert.ok(raw.trim(), "抠不到 rawOutputTerms 的定义（改成别的写法了？锚点要跟着更新）");
  const extra = [...raw.matchAll(/"([A-Za-z]+)"/g)].map((match) => match[1]);
  assert.deepEqual([...extra].sort(), ["NAT", "localhost", "wsl"]);
});

test("unwraps Codex shell wrapper and labels command execution as terminal command", () => {
  const timeline = buildTimeline([], [
    event("cmd-start", "item.started", { item: { id: "item_1", type: "command_execution", command: "/usr/bin/zsh -lc 'cat hello.txt'", aggregated_output: "", exit_code: null, status: "in_progress" } }),
    event("cmd-done", "item.completed", { item: { id: "item_1", type: "command_execution", command: "/usr/bin/zsh -lc 'cat hello.txt'", aggregated_output: "hello\n", exit_code: 0, status: "completed" } }),
  ]);
  const tools = timeline.filter((item) => item.kind === "tool");
  assert.equal(tools.length, 1);
  const action = (tools[0] as any).action;
  assert.equal(action.name, "终端命令");
  assert.equal(action.input.command, "cat hello.txt");
  assert.equal(action.output?.content, "hello\n");
});

test("formats Codex file_change with readable Chinese labels", () => {
  const timeline = buildTimeline([], [
    event("file-done", "item.completed", { item: { id: "item_2", type: "file_change", changes: [{ path: "/tmp/proj/hello.txt", kind: "add" }, { path: "/tmp/proj/foo.py", kind: "modify" }], status: "completed" } }),
  ]);
  const tools = timeline.filter((item) => item.kind === "tool");
  assert.equal(tools.length, 1);
  const action = (tools[0] as any).action;
  assert.equal(action.name, "文件修改");
  assert.ok(action.input.description.includes("新增"), `description should contain 新增: ${action.input.description}`);
  assert.ok(action.input.description.includes("修改"), `description should contain 修改: ${action.input.description}`);
  assert.ok(action.output?.content.includes("新增"), `output should contain 新增: ${action.output?.content}`);
});

// 失败诊断的判据顺序必须与服务端一致：**先判内部错误，再判中文直通**。
//
// 两个判据都在 localizedErrorDetail 里，顺序决定一条"含中文 + 带 Go 栈帧痕迹"的错误
// 会不会原样上屏。服务端 localizedErrorText 明确先判 isInternalError
// （"Checked first so a wrapped '… 失败' message can never carry a stack trace to the UI"）；
// 前端原来把中文分支排在前面，于是同一条错误在桌面端会把 `.go:` / `panic:` 带给用户，
// 而服务端那边是兜底句 —— 两端形态不一致（2026-09-29 对齐）。
test("含中文的 Go 内部错误也要被换成兜底句（判据顺序与服务端一致）", () => {
  const diagnostic = eventDiagnostic(event("e1", "run.failed", {
    error: "任务执行失败：panic: runtime error (cli/runner.go:42)",
  }));
  assert.ok(diagnostic, "这条事件应当解析出诊断");
  assert.equal(
    diagnostic.detail,
    "任务执行失败，请查看任务日志后重试。",
    "含中文的栈帧被原样上屏了 —— isInternalError 又被排到中文分支后面",
  );
  // 反面：普通的中文失败原因**不许**被换成兜底句（那正是"失败"分支存在的理由）。
  const readable = eventDiagnostic(event("e2", "run.failed", { error: "Codex 运行失败：exit status 1" }));
  assert.equal(readable?.detail, "Codex 运行失败：exit status 1");
});
