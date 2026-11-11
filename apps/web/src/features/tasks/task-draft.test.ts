import assert from "node:assert/strict";
import test, { mock } from "node:test";

import { api } from "../../lib/api.ts";
import type { Request } from "./task-model.ts";
import { applyTaskDraft, confirmDraftTasks, draftWaitLabel, isDraftAbort, organizeTaskDraft, type TaskDraft, type TaskDraftItem } from "./task-draft.ts";

const emptyDraft = (overrides: Partial<TaskDraft> = {}): TaskDraft => ({
  understood: "",
  mode: "single",
  assumptions: [],
  questions: [],
  tasks: [],
  ...overrides,
});

test("单条只回填说明，不调用批量创建", async () => {
  const calls: string[] = [];
  const request: Request = async <T>(path: string) => {
    calls.push(path);
    return emptyDraft({
      understood: "补上登录校验",
      tasks: [{ key: "t1", description: "在现有登录接口补上过期校验。", priority: "normal", dependsOn: [] }],
    }) as T;
  };
  const controller = new AbortController();
  const draft = await organizeTaskDraft({ request, projectID: "p1", text: "登录有问题", signal: controller.signal });
  const action = applyTaskDraft(draft);

  assert.equal(action.kind, "single");
  if (action.kind === "single") assert.equal(action.description, "在现有登录接口补上过期校验。");
  assert.deepEqual(calls, ["/api/projects/p1/tasks/draft"]);
});

test("有待补充的问题时不回填任务", () => {
  const action = applyTaskDraft(emptyDraft({
    questions: ["要改哪一个入口？"],
    tasks: [{ key: "t1", description: "不该留下", priority: "normal", dependsOn: [] }],
  }));
  assert.deepEqual(action, { kind: "questions", questions: ["要改哪一个入口？"] });
});

test("多条确认后调用批量接口", async () => {
  const calls: { path: string; body: string }[] = [];
  const request: Request = async <T>(path: string, init?: RequestInit) => {
    calls.push({ path, body: String(init?.body ?? "") });
    return undefined as T;
  };
  const tasks = [
    { key: "t1", description: "先补校验", priority: "high" as const, dependsOn: [] },
    { key: "t2", description: "再补页面", priority: "normal" as const, dependsOn: ["t1"] },
  ];
  assert.equal(applyTaskDraft(emptyDraft({ mode: "split", tasks })).kind, "split");
  await confirmDraftTasks(request, "p1", tasks);

  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/api/projects/p1/tasks/batch");
  assert.deepEqual(JSON.parse(calls[0].body), { tasks });
});

test("取消会把中止信号交给整理请求", async () => {
  const controller = new AbortController();
  let seen: AbortSignal | undefined;
  const request: Request = async <T>(_path: string, init?: RequestInit) => {
    seen = init?.signal ?? undefined;
    controller.abort();
    throw new DOMException("Aborted", "AbortError");
  };

  await assert.rejects(
    () => organizeTaskDraft({ request, projectID: "p1", text: "做一个登录", signal: controller.signal }),
    (cause: unknown) => isDraftAbort(cause),
  );
  assert.equal(seen, controller.signal);
  assert.equal(controller.signal.aborted, true);
});

test("空白输入和已经取消的请求不会发出", async () => {
  const request: Request = async () => {
    throw new Error("不该发出请求");
  };
  await assert.rejects(
    () => organizeTaskDraft({ request, projectID: "p1", text: "  ", signal: new AbortController().signal }),
    /请先输入要整理的内容/,
  );
  const aborted = new AbortController();
  aborted.abort();
  await assert.rejects(
    () => organizeTaskDraft({ request, projectID: "p1", text: "做一个登录", signal: aborted.signal }),
    (cause: unknown) => isDraftAbort(cause),
  );
});

test("空白任务被收成单条，问题会去掉空行", () => {
  const blank = applyTaskDraft(emptyDraft({
    tasks: [{ key: "t1", description: "  ", priority: "normal", dependsOn: [] }],
  }));
  assert.deepEqual(blank, { kind: "single", description: "" });

  const questions = applyTaskDraft(emptyDraft({
    questions: ["  ", "要改哪一个入口？"],
    tasks: [{ key: "t1", description: "不该留下", priority: "normal", dependsOn: [] }],
  }));
  assert.deepEqual(questions, { kind: "questions", questions: ["要改哪一个入口？"] });
});

test("等待文案按当前顺序编号，确认时去掉说明两端空白", async () => {
  const tasks: TaskDraftItem[] = [
    { key: "t1", description: " 先补校验 ", priority: "high", dependsOn: [] },
    { key: "t2", description: "再补页面", priority: "normal", dependsOn: ["t1", "missing"] },
  ];
  assert.equal(draftWaitLabel(tasks[0], tasks), "");
  assert.equal(draftWaitLabel(tasks[1], tasks), "等待第 1 条、missing");

  const calls: string[] = [];
  const request: Request = async <T>(_path: string, init?: RequestInit) => {
    calls.push(String(init?.body ?? ""));
    return undefined as T;
  };
  await confirmDraftTasks(request, "p1", tasks);
  const body = JSON.parse(calls[0]) as { tasks: TaskDraftItem[] };
  assert.equal(body.tasks[0].description, "先补校验");
  assert.deepEqual(body.tasks[1].dependsOn, ["t1", "missing"]);
});

test("整理请求等待到服务端超时之后，并且失败后不重发", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const original = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = ((_url: string, init?: RequestInit) => new Promise((_resolve, reject) => {
    calls += 1;
    init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")));
  })) as typeof fetch;
  try {
    let settled = false;
    let message = "";
    const pending = api("/api/projects/p1/tasks/draft?x=1", {
      method: "POST",
      body: JSON.stringify({ text: "做一个登录" }),
    }).then(() => {
      settled = true;
    }, (cause: unknown) => {
      settled = true;
      message = cause instanceof Error ? cause.message : "";
    });
    await Promise.resolve();
    mock.timers.tick(99_999);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(settled, false);
    assert.equal(calls, 1);
    mock.timers.tick(1);
    await pending;
    assert.equal(settled, true);
    assert.equal(calls, 1);
    assert.match(message, /仍未返回/);
  } finally {
    globalThis.fetch = original;
    mock.timers.reset();
  }
});

test("批量创建仍使用普通请求预算", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const original = globalThis.fetch;
  globalThis.fetch = ((_url: string, init?: RequestInit) => new Promise((_resolve, reject) => {
    init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")));
  })) as typeof fetch;
  try {
    const pending = api("/api/projects/p1/tasks/batch", { method: "POST", body: "{}" });
    await Promise.resolve();
    mock.timers.tick(14_999);
    let settled = false;
    const done = pending.then(() => {
      settled = true;
    }, (cause: unknown) => {
      settled = true;
      throw cause;
    });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(settled, false);
    mock.timers.tick(1);
    await assert.rejects(done, /15 秒/);
  } finally {
    globalThis.fetch = original;
    mock.timers.reset();
  }
});
