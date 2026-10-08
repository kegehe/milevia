import assert from "node:assert/strict";
import test from "node:test";
import {
  CLOUD_DEFAULT_WAIT_MS,
  CLOUD_MAX_WAIT_MS,
  cloudClientWaitMs,
} from "./cloud-budget.ts";

// 这条不变量的失效方式很安静：手机端在旁边小声报一句"云端请求超时"，而电脑上那次
// push 还在跑、最后是成功的。所以它必须是一条能跑的判据，不是一句注释。
test("手机端永远等得比云端久", () => {
  for (const proposed of [undefined, 0, 1_000, 20_000, 45_000, 60_000, 120_000, 999_999]) {
    const client = cloudClientWaitMs(proposed);
    const cloud = Math.min(proposed && proposed > 0 ? proposed : CLOUD_DEFAULT_WAIT_MS, CLOUD_MAX_WAIT_MS);
    assert.ok(
      client > cloud,
      `提议 ${proposed} 时手机端等 ${client}ms，云端等 ${cloud}ms —— 手机先报"超时"，云端还在等一个早就丢掉的响应`,
    );
  }
});

test("不提议超时的那条路（文件适配器）也压得住云端的默认值", () => {
  // 文件适配器从不提议超时（mobile-fs-request.ts 的信封没有 timeoutMs 字段），
  // 于是它走的就是云端默认值这一档。原先手机端恰好也是 15 秒 —— 比云端的 20 秒还短。
  assert.ok(
    cloudClientWaitMs() > CLOUD_DEFAULT_WAIT_MS,
    "手机端默认等待必须大于云端的 rpcDefaultTimeout（20s）",
  );
});

test("调用方提议得越久，手机端就等得越久", () => {
  // 网络写（push/fetch，mobile-git-request.ts 提议 60 秒）必须真的能等到 60 秒。
  assert.ok(cloudClientWaitMs(60_000) > 60_000, "提议 60 秒却等不到 60 秒，等于没提议");
  assert.ok(cloudClientWaitMs(45_000) > 45_000);
  // 超过云端上界的提议会被云端夹到 120 秒，手机端不必跟着无限放长。
  assert.equal(cloudClientWaitMs(999_999), CLOUD_MAX_WAIT_MS + 5_000);
});
