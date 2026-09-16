import assert from "node:assert/strict";
import test from "node:test";
import { copyToClipboard } from "./clipboard";

// 这里直接替换全局 navigator / document：clipboard.ts 在调用时才读它们，
// 因此桩只需在调用前装好。node:test 每个文件独立进程，不会污染其它测试。

function stubGlobal(key: "navigator" | "document", value: unknown) {
  Object.defineProperty(globalThis, key, { value, configurable: true, writable: true });
}

function stubDocument(options: { execResult?: boolean; execThrows?: boolean } = {}) {
  const removed: unknown[] = [];
  const appended: unknown[] = [];
  const commands: string[] = [];
  const textarea = {
    value: "",
    style: {} as Record<string, string>,
    setAttribute() {},
    select() {},
    setSelectionRange() {},
    remove() { removed.push(textarea); },
  };
  stubGlobal("document", {
    createElement: () => textarea,
    body: { append: (node: unknown) => void appended.push(node) },
    execCommand: (command: string) => {
      commands.push(command);
      if (options.execThrows) throw new Error(`execCommand ${command} blocked`);
      return options.execResult ?? true;
    },
  });
  return { textarea, appended, removed, commands };
}

test("uses the async clipboard API when it is available", async () => {
  const written: string[] = [];
  stubGlobal("navigator", { clipboard: { writeText: async (value: string) => void written.push(value) } });
  stubDocument();
  assert.equal(await copyToClipboard("npm run build"), true);
  assert.deepEqual(written, ["npm run build"]);
});

test("falls back to execCommand when the async API rejects", async () => {
  stubGlobal("navigator", { clipboard: { writeText: async () => { throw new Error("denied"); } } });
  const fake = stubDocument();
  assert.equal(await copyToClipboard("npm ci"), true);
  assert.equal(fake.textarea.value, "npm ci");
  // 必须真的发起 copy 命令，不能只是"没抛错就当成功"。
  assert.deepEqual(fake.commands, ["copy"]);
  // 临时 textarea 用完必须移除，否则会在页面上堆积不可见的节点。
  assert.deepEqual(fake.appended, [fake.textarea]);
  assert.deepEqual(fake.removed, [fake.textarea]);
});

test("falls back to execCommand when the async API is missing (insecure context)", async () => {
  // 手机端经局域网 http 访问时 navigator.clipboard 不存在，只能走旧方案。
  stubGlobal("navigator", {});
  const fake = stubDocument();
  assert.equal(await copyToClipboard("milevia --help"), true);
  assert.equal(fake.textarea.value, "milevia --help");
  assert.deepEqual(fake.commands, ["copy"]);
});

test("reports failure instead of silently doing nothing", async () => {
  stubGlobal("navigator", {});
  stubDocument({ execThrows: true });
  assert.equal(await copyToClipboard("no clipboard here"), false);
});

test("reports failure when the legacy command is refused", async () => {
  stubGlobal("navigator", {});
  stubDocument({ execResult: false });
  assert.equal(await copyToClipboard("refused"), false);
});

test("never throws when the DOM is unavailable", async () => {
  // 契约：只返回布尔值，永不抛出。ConversationPage 的复制按钮已不再包 try/catch，
  // 这个保证一旦破掉就会变成未处理的 rejection。
  stubGlobal("navigator", {});
  stubGlobal("document", undefined);
  assert.equal(await copyToClipboard("ssr"), false);
});
