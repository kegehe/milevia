import assert from "node:assert/strict";
import test from "node:test";

import { createMobileInsightsRequest, type MobileRpcReply } from "./mobile-insights-request";

// 一个记账用的桩：记录每次发出了什么 op / params，并按脚本回话。
// 断言里既看"返回了什么"，也看"打了什么枪" —— REST → op 的映射错了，
// 最容易的症状是"看起来有响应、其实打到了别的 op 上"。
function makeTransport(script: (op: string, params: unknown) => MobileRpcReply | undefined) {
  const calls: Array<{ op: string; params: unknown }> = [];
  const transport = async (op: string, params: unknown): Promise<MobileRpcReply> => {
    calls.push({ op, params });
    const reply = script(op, params);
    if (!reply) throw new Error(`stub has no reply for ${op} ${JSON.stringify(params)}`);
    return reply;
  };
  return { transport, calls };
}

const ok = (data: unknown): MobileRpcReply => ({ ok: true, status: 200, data });

const FINDING = "0b8fd035-9f45-4a5e-9f61-2f6b1f6b1f6b";
const PROJECT = "/api/projects/p1";

// ─── 路径映射 ───────────────────────────────────────────────────────────────

test("集合端点都映射到对应的 op，方法写错要当场报错", async () => {
  for (const testCase of [
    { route: "", method: "GET", op: "insights.list" },
    { route: "/scan", method: "POST", op: "insights.scan" },
    { route: "/cancel", method: "POST", op: "insights.cancel" },
    { route: "/verify", method: "POST", op: "insights.verify" },
    { route: "/to-task", method: "POST", op: "insights.toTask" },
    { route: "/delete", method: "POST", op: "insights.delete" },
  ] as const) {
    const { transport, calls } = makeTransport(() => ok({}));
    const adapter = createMobileInsightsRequest({ transport });
    const body = testCase.method === "POST" ? { body: JSON.stringify({}) } : undefined;
    await adapter.request(`${PROJECT}/insights${testCase.route}`, { method: testCase.method, ...body });
    assert.deepEqual(calls.map((call) => call.op), [testCase.op], `${testCase.route} 的 op 不对`);

    const wrong = makeTransport(() => ok({}));
    const wrongAdapter = createMobileInsightsRequest({ transport: wrong.transport });
    await assert.rejects(
      () => wrongAdapter.request(`${PROJECT}/insights${testCase.route}`, {
        method: testCase.method === "GET" ? "POST" : "GET",
      }),
      (error: unknown) => error instanceof Error && error.message.includes("方法是"),
      `${testCase.route} 的方法写错没有被拦下`,
    );
    assert.equal(wrong.calls.length, 0, "接线错误不应该真的发出去");
  }
});

test("单条建议端点把 findingID 从路径提进 params，body 字段原样透传", async () => {
  const cases = [
    { suffix: "", method: "PATCH", op: "insights.findingUpdate", body: { title: "新标题", severity: "high" } },
    { suffix: "", method: "DELETE", op: "insights.findingDelete" },
    { suffix: "/to-task", method: "POST", op: "insights.findingToTask" },
    { suffix: "/dismiss", method: "POST", op: "insights.findingDismiss" },
    { suffix: "/dismiss", method: "DELETE", op: "insights.findingDismissClear" },
  ] as const;
  for (const testCase of cases) {
    const { transport, calls } = makeTransport(() => ok({}));
    const adapter = createMobileInsightsRequest({ transport });
    const init: RequestInit = { method: testCase.method };
    if ("body" in testCase) init.body = JSON.stringify(testCase.body);
    await adapter.request(`${PROJECT}/insights/${FINDING}${testCase.suffix}`, init);
    assert.deepEqual(calls.map((call) => call.op), [testCase.op]);
    const params = calls[0].params as Record<string, unknown>;
    assert.equal(params.findingID, FINDING, `${testCase.op} 的 findingID 没进 params`);
    if ("body" in testCase) {
      for (const [key, value] of Object.entries(testCase.body)) {
        assert.deepEqual(params[key], value, `${testCase.op} 的 body 字段 ${key} 没透传`);
      }
    }
  }
});

test("列表的游标轮询把 sinceScan / sinceSeq 作为字符串映射传下去", async () => {
  const { transport, calls } = makeTransport(() => ok({ findings: [] }));
  const adapter = createMobileInsightsRequest({ transport });
  await adapter.request(`${PROJECT}/insights?sinceScan=scan-1&sinceSeq=7`);
  assert.deepEqual(calls[0].params, { sinceScan: "scan-1", sinceSeq: "7" });

  await adapter.request(`${PROJECT}/insights`);
  assert.deepEqual(calls[1].params, {}, "没有游标时应发空 params");
});

test("业务失败把服务端的中文原样抛出去", async () => {
  const { transport } = makeTransport((op) =>
    op === "insights.scan"
      ? { ok: false, status: 409, error: "该项目正在扫描建议，请稍候再试" }
      : ok({}),
  );
  const adapter = createMobileInsightsRequest({ transport });
  await assert.rejects(
    () => adapter.request(`${PROJECT}/insights/scan`, { method: "POST", body: JSON.stringify({ agent: "claude-code" }) }),
    (error: unknown) => error instanceof Error && error.message === "该项目正在扫描建议，请稍候再试",
  );
});

test("非 insights 的路径要当场报错，不能静默发出去", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileInsightsRequest({ transport });
  await assert.rejects(() => adapter.request(`${PROJECT}/fs/tree`), /不是优化建议的请求/);
  await assert.rejects(
    () => adapter.request(`${PROJECT}/insights/not-a-real-route`),
    /没有映射这个优化建议端点/,
  );
  assert.equal(calls.length, 0);
});
