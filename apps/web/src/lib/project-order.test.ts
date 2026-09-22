// 卡片顺序的三件事：读出来怎么排、拖到哪里插、两端的顺序互不串味。
// 全是纯函数（存储用内存桩），拖拽的指针部分测不到，顺序本身必须在这里钉死。
import { test } from "node:test";
import assert from "node:assert/strict";

const store = new Map<string, string>();
let writeCount = 0;
(globalThis as unknown as { localStorage: unknown }).localStorage = {
  getItem: (key: string) => store.get(key) ?? null,
  setItem: (key: string, value: string) => { writeCount += 1; store.set(key, value); },
  removeItem: (key: string) => { store.delete(key); },
};

const {
  MOBILE_PROJECT_ORDER_STORAGE_KEY,
  PROJECT_ORDER_STORAGE_KEY,
  dismissMobileDragHint,
  moveProject,
  persistOrder,
  readMobileDragHint,
  reorderIds,
  resetProjectOrder,
  sortProjectIds,
} = await import("./project-order");

const reset = () => { store.clear(); writeCount = 0; };

test("没存过顺序时保持后端给的顺序（新项目不能被凭空挪到前面）", () => {
  reset();
  assert.deepEqual(sortProjectIds(["a", "b", "c"]), ["a", "b", "c"]);
});

test("存过顺序就按存的排；没存过的（新项目）追加到末尾，保持后端相对顺序", () => {
  reset();
  persistOrder(["c", "a"]);
  assert.deepEqual(sortProjectIds(["a", "b", "c", "d"]), ["c", "a", "b", "d"]);
});

test("已删除但仍残留在存储里的 id 会被自然丢弃", () => {
  reset();
  persistOrder(["c", "gone", "a"]);
  assert.deepEqual(sortProjectIds(["a", "c"]), ["c", "a"]);
});

test("顺序没变时不重复写存储，变了才写", () => {
  reset();
  persistOrder(["a", "b"]);
  const baseline = writeCount;
  persistOrder(["a", "b"]);
  assert.equal(writeCount, baseline);
  persistOrder(["b", "a"]);
  assert.equal(writeCount, baseline + 1);
});

test("存储里是坏数据时按「没存过」处理，不抛异常", () => {
  reset();
  store.set(PROJECT_ORDER_STORAGE_KEY, "{ 这不是 JSON");
  assert.deepEqual(sortProjectIds(["a", "b"]), ["a", "b"]);
});

test("resetProjectOrder 之后回到后端顺序", () => {
  reset();
  persistOrder(["c", "b", "a"]);
  resetProjectOrder();
  assert.deepEqual(sortProjectIds(["a", "b", "c"]), ["a", "b", "c"]);
});

test("手机端与桌面端的顺序各存各的（同一个 origin 下互不冲掉）", () => {
  reset();
  persistOrder(["c", "b", "a"]);
  persistOrder(["a", "c", "b"], MOBILE_PROJECT_ORDER_STORAGE_KEY);
  assert.notEqual(PROJECT_ORDER_STORAGE_KEY, MOBILE_PROJECT_ORDER_STORAGE_KEY);
  assert.deepEqual(sortProjectIds(["a", "b", "c"]), ["c", "b", "a"]);
  assert.deepEqual(sortProjectIds(["a", "b", "c"], MOBILE_PROJECT_ORDER_STORAGE_KEY), ["a", "c", "b"]);
  // 重置桌面端那把钥匙，手机端那份必须还在。
  resetProjectOrder();
  assert.deepEqual(sortProjectIds(["a", "b", "c"]), ["a", "b", "c"]);
  assert.deepEqual(sortProjectIds(["a", "b", "c"], MOBILE_PROJECT_ORDER_STORAGE_KEY), ["a", "c", "b"]);
});

test("桌面端的 moveProject 保持原语义（按目标下标插入）", () => {
  reset();
  assert.deepEqual(moveProject(["a", "b", "c", "d"], "d", "b"), ["a", "d", "b", "c"]);
  assert.deepEqual(moveProject(["a", "b", "c"], "a", "a"), ["a", "b", "c"]);
  assert.deepEqual(moveProject(["a", "b", "c"], "a", "zzz"), ["a", "b", "c"]);
});

test("往后挪：插到目标槽位之后", () => {
  assert.deepEqual(reorderIds(["a", "b", "c", "d"], "a", 2, true), ["b", "c", "a", "d"]);
  assert.deepEqual(reorderIds(["a", "b", "c", "d"], "a", 2, false), ["b", "a", "c", "d"]);
});

test("插到自己身上等于没动（往回拖一点点不该抖）", () => {
  assert.deepEqual(reorderIds(["a", "b", "c"], "b", 1, false), ["a", "b", "c"]);
  assert.deepEqual(reorderIds(["a", "b", "c"], "b", 1, true), ["a", "b", "c"]);
});

test("往前挪时的下标要补偿：摘掉自己之后，目标槽位会左移一位", () => {
  // a 从 0 挪到 c（原 2）之后 → [b, c, a, d]；补偿写错会得到 [b, a, c, d]。
  assert.deepEqual(reorderIds(["a", "b", "c", "d"], "a", 2, true), ["b", "c", "a", "d"]);
  // 往后挪到 b（原 1）之后 → [b, a, c, d]。
  assert.deepEqual(reorderIds(["a", "b", "c", "d"], "a", 1, true), ["b", "a", "c", "d"]);
});

test("认不出来的 id / 越界下标一律原样返回，不抛异常", () => {
  const ids = ["a", "b"];
  assert.equal(reorderIds(ids, "zzz", 0, false), ids);
  assert.equal(reorderIds(ids, "a", -1, false), ids);
  assert.equal(reorderIds(ids, "a", 2, false), ids);
  assert.deepEqual(ids, ["a", "b"]);
});

test("挪到最后一位不会掉出去", () => {
  assert.deepEqual(reorderIds(["a", "b", "c"], "a", 2, true), ["b", "c", "a"]);
  assert.deepEqual(reorderIds(["a", "b", "c"], "c", 0, false), ["c", "a", "b"]);
});

test("手机端拖动提示：没拖过要显示，收起之后不再回来", () => {
  reset();
  assert.equal(readMobileDragHint(), true);
  dismissMobileDragHint();
  assert.equal(readMobileDragHint(), false);
  // 收起是一次性的：再读还是收起。
  assert.equal(readMobileDragHint(), false);
});
