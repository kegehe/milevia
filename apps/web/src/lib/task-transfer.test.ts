import assert from "node:assert/strict";
import test from "node:test";

import { TASK_TRANSFER_MAX_TASKS, parseTaskTransferFile, previewTaskImport, taskTransferFileName, taskTransferImportBody } from "./task-transfer";

const fileText = (tasks: unknown[], extra: Record<string, unknown> = {}) =>
  JSON.stringify({ format: "milevia.tasks", version: 1, tasks, ...extra });

test("parses a valid export file and strips the UTF-8 BOM", () => {
  const text = "\uFEFF" + fileText([
    { key: "t1", description: "说明。", dependsOn: null },
    { key: "t2", description: "说明二。", dependsOn: ["t1", "t1"] },
  ]);
  const result = parseTaskTransferFile(text);
  assert.equal(result.ok, true);
  if (!result.ok) return;
  assert.equal(result.file.tasks.length, 2);
  assert.deepEqual(result.file.tasks[0].dependsOn, []);
  assert.deepEqual(result.file.tasks[1].dependsOn, ["t1"]);
  assert.equal(result.file.format, "milevia.tasks");
});

test("rejects malformed files", () => {
  const cases = [
    "",
    "not json",
    JSON.stringify({ format: "other", version: 1, tasks: [] }),
    fileText([], { version: 2 }),
    JSON.stringify({ format: "milevia.tasks", version: 1 }),
    fileText([]),
    fileText([{ key: "t1", description: "说明。" }, { key: "t1", description: "说明二。" }]),
    fileText([{ key: "t 1", description: "说明。" }]),
    fileText([{ key: "t1", description: "说明。", dependsOn: ["t9"] }]),
    fileText([{ key: "t1", description: "说明。", dependsOn: ["t1"] }]),
    fileText([
      { key: "t1", description: "说明一。", dependsOn: ["t2"] },
      { key: "t2", description: "说明二。", dependsOn: ["t1"] },
    ]),
    fileText(Array.from({ length: TASK_TRANSFER_MAX_TASKS + 1 }, (_, index) => ({ key: "t" + (index + 1), description: "说明。" }))),
  ];
  for (const text of cases) {
    const result = parseTaskTransferFile(text);
    assert.equal(result.ok, false, "expected failure for: " + text.slice(0, 40));
    if (!result.ok) assert.ok(result.reason.length > 0);
  }
});

test("accepts a cycle that only exists through a satisfied (done) edge", () => {
  const text = fileText([
    { key: "t1", description: "已完成的前置。", status: "done", dependsOn: ["t2"] },
    { key: "t2", description: "依赖已完成前置的任务。", status: "todo", dependsOn: ["t1"] },
  ]);
  const result = parseTaskTransferFile(text);
  assert.equal(result.ok, true);
});

test("previews counts and duplicates by trimmed description", () => {
  const parsed = parseTaskTransferFile(fileText([
    { key: "t1", description: " 同一件事。" },
    { key: "t2", description: "新任务。", dependsOn: ["t1"] },
    { key: "t3", description: "同一件事。" },
    { key: "t4", description: "   " },
  ]));
  assert.equal(parsed.ok, true);
  if (!parsed.ok) return;
  const preview = previewTaskImport(parsed.file, [{ description: "同一件事。" }]);
  assert.deepEqual(preview, { tasks: 4, dependencies: 1, duplicates: 2 });
});


test("does not count dependencies of duplicated tasks", () => {
  const parsed = parseTaskTransferFile(fileText([
    { key: "t1", description: "已存在。", dependsOn: ["t2"] },
    { key: "t2", description: "新任务。" },
  ]));
  assert.equal(parsed.ok, true);
  if (!parsed.ok) return;
  assert.deepEqual(previewTaskImport(parsed.file, [{ description: "已存在。" }]), { tasks: 2, dependencies: 0, duplicates: 1 });
});
test("builds a Windows-safe file name", () => {
  const day = new Date(2026, 9, 27);
  assert.equal(taskTransferFileName("my/project:1", day), "任务-my-project-1-20261027.json");
  assert.equal(taskTransferFileName("", day), "任务-20261027.json");
  assert.equal(taskTransferFileName("   ", day), "任务-20261027.json");
  assert.equal(taskTransferFileName("a".repeat(80), day), "任务-" + "a".repeat(40) + "-20261027.json");
});

test("accepts a file whose content contains a replacement character", () => {
  const result = parseTaskTransferFile(fileText([{ key: "t1", description: "说明里有一个替换字符 \uFFFD。" }]));
  assert.equal(result.ok, true);
});

test("rejects content that decodes to NUL bytes", () => {
  const result = parseTaskTransferFile('{"format":"milevia.tasks","version":1,"tasks":[]}\u0000');
  assert.equal(result.ok, false);
});

test("normalizes status and timestamps for the wire format", () => {
  const result = parseTaskTransferFile(fileText([
    { key: "t1", description: "已完成。", status: " Done ", createdAt: "2026-10-20" },
    { key: "t2", description: "依赖它。", dependsOn: ["t1"], createdAt: "not-a-date" },
  ]));
  assert.equal(result.ok, true);
  if (!result.ok) return;
  assert.equal(result.file.tasks[0].status, "done");
  // Date.parse 接受纯日期，Go 的 time.Time 不接受——必须转成 RFC3339 再发给服务端。
  assert.equal(result.file.tasks[0].createdAt, "2026-10-20T00:00:00.000Z");
  assert.equal(result.file.tasks[1].createdAt, undefined);
});

test("import body only carries the fields the server decodes", () => {
  const parsed = parseTaskTransferFile(fileText([{ key: "t1", description: "说明。" }], { exportedAt: "2026-10-20", droppedDependencies: 1.5 }));
  assert.equal(parsed.ok, true);
  if (!parsed.ok) return;
  const body = taskTransferImportBody(parsed.file);
  assert.deepEqual(Object.keys(body).sort(), ["format", "tasks", "version"]);
});

test("truncates project names without splitting surrogate pairs", () => {
  const day = new Date(2026, 9, 27);
  const name = taskTransferFileName("😀".repeat(50), day);
  assert.equal(name, "任务-" + "😀".repeat(40) + "-20261027.json");
  assert.equal((name as string & { isWellFormed?: () => boolean }).isWellFormed?.(), true);
});
