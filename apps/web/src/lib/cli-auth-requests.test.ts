import assert from "node:assert/strict";
import test from "node:test";
import { CLIAuthRequests } from "./cli-auth-requests";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

test("登录后手动刷新先返回时，旧的自动查询不能覆盖它", async () => {
  const requests = new CLIAuthRequests();
  const auto = deferred<string>();
  const manual = deferred<string>();
  let displayed = "";
  const read = async (response: Promise<string>) => {
    const current = requests.beginReading("local", "cursor");
    const state = await response;
    if (current()) displayed = state;
  };
  const automaticRead = read(auto.promise);
  const manualRead = read(manual.promise);
  manual.resolve("logged_in");
  await manualRead;
  auto.resolve("logged_out");
  await automaticRead;
  assert.equal(displayed, "logged_in");
});

test("不同环境与工具的并发查询互不作废", () => {
  const requests = new CLIAuthRequests();
  const local = requests.beginReading("local", "cursor");
  const ssh = requests.beginReading("ssh-prod", "cursor");
  const other = requests.beginReading("local", "codex");
  assert.ok(local() && ssh() && other());
  requests.beginReading("local", "cursor");
  assert.equal(local(), false);
  assert.ok(ssh() && other());
});

test("关闭重开同一工具后，旧响应及 finally 不会改变新面板", async () => {
  const requests = new CLIAuthRequests();
  const old = deferred<string>();
  let message = "";
  let busy = true;
  const current = requests.beginPanel();
  const pending = old.promise.then((result) => {
    if (current()) message = result;
  }).finally(() => { if (current()) busy = false; });
  requests.closePanel();
  const reopened = requests.beginPanel();
  old.resolve("old authorization URL");
  await pending;
  assert.equal(message, "");
  assert.equal(busy, true);
  assert.ok(reopened());
  requests.closePanel();
  assert.equal(reopened(), false, "切换环境或卸载也使面板作废");
});

test("手动检查会作废面板初次查询，授权操作也有自己的顺序", () => {
  const requests = new CLIAuthRequests();
  const initial = requests.beginPanel();
  const manual = requests.beginPanel();
  assert.equal(initial(), false);
  const login = requests.beginPanel();
  assert.equal(manual(), false);
  assert.ok(login());
});
