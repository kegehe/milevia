// 弹窗参数（URL search）开关的纯计算测试。
//
// 这些用例守着两件曾经真实出事的事：
//   1) 要关的参数本来就不在时，**不许发导航**（多发的那次会凭空多一条历史记录，而且
//      按调用方闭包里的旧 location 算地址 —— 实测就是它把用户推回旧会话 + 复活 ?new=true）；
//   2) 目标地址按**传入的**（最新）location 拼成绝对路径，不受调用方闭包影响。

import assert from "node:assert/strict";
import test from "node:test";

import { dialogParamURL } from "./dialog-params.ts";

test("closing an absent param needs no navigation", () => {
  const location = { pathname: "/projects/p1/conversations/c1", search: "" };
  assert.equal(dialogParamURL(location, "execution", null), null);
  assert.equal(dialogParamURL({ ...location, search: "?usage=true" }, "execution", null), null);
});

test("closing a present param points at the same path without it", () => {
  const location = { pathname: "/projects/p1/conversations/c1", search: "?execution=run-1" };
  assert.equal(dialogParamURL(location, "execution", null), "/projects/p1/conversations/c1");
  // 其它弹窗参数必须原样保留：关一个不该顺手关掉别的。
  assert.equal(
    dialogParamURL({ ...location, search: "?execution=run-1&new=true" }, "execution", null),
    "/projects/p1/conversations/c1?new=true",
  );
});

test("opening a param keeps the path and other params", () => {
  const location = { pathname: "/projects/p1/conversations/c1", search: "" };
  assert.equal(dialogParamURL(location, "usage", "true"), "/projects/p1/conversations/c1?usage=true");
  assert.equal(
    dialogParamURL({ pathname: location.pathname, search: "?history=true" }, "new", "true"),
    "/projects/p1/conversations/c1?history=true&new=true",
  );
});

test("already-open param is not re-pushed, changed value is", () => {
  const location = { pathname: "/projects/p1/conversations/c1", search: "?usage=true" };
  assert.equal(dialogParamURL(location, "usage", "true"), null);
  // 同一个参数换值（execution 指向另一个 run）要真的改。
  assert.equal(
    dialogParamURL({ pathname: location.pathname, search: "?execution=run-1" }, "execution", "run-2"),
    `${location.pathname}?execution=run-2`,
  );
});

test("uses the location it is given, not any earlier one", () => {
  // 这就是修复的核心：调用方把**最新** location 传进来，地址就一定是它的绝对地址。
  const latest = { pathname: "/projects/p1/conversations/c2", search: "?new=true" };
  assert.equal(dialogParamURL(latest, "new", null), "/projects/p1/conversations/c2");
  // 反过来：拿旧 location 来算就会得到旧地址（这正是修复前发生的事，故不再允许调用方自己算）。
  const stale = { pathname: "/projects/p1/conversations/c1", search: "?new=true" };
  assert.equal(dialogParamURL(stale, "new", null), "/projects/p1/conversations/c1");
  assert.notEqual(dialogParamURL(stale, "new", null), dialogParamURL(latest, "new", null));
});
