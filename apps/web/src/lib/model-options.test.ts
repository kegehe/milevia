// 模型目录为 null 时不许抛错的回归测试。
//
// 现象（已实测）：CodeBuddy 会话里点开底部模型下拉，整个工作区面板被错误边界换成
// 兜底 UI，中间写着 "Cannot read properties of null (reading 'length')"。根因是
// `view?.models.length` —— 可选链只到 view，而服务端把"没有内置目录"发成 JSON null
// （Go 的 nil 切片），null 上读 .length 就抛。Claude / Codex 有目录，所以只有
// CodeBuddy 必现。

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { modelList } from "./model-options.ts";

test("treats a missing or null models field as an empty list", () => {
  // 服务端把"没有目录"发成 null 时不能抛，也不能冒充成"有一个空对象"。
  assert.deepEqual(modelList(null), []);
  assert.deepEqual(modelList(undefined), []);
  assert.deepEqual(modelList({ models: null }), []);
  assert.deepEqual(modelList({ models: [] }), []);

  const options = [{ id: "opus" }];
  assert.equal(modelList({ models: options }), options);
  // 响应对象上根本没有 models 字段（老服务端）同样走空列表。
  assert.deepEqual(modelList({} as { models?: unknown[] }), []);
});

test("never reads view.models.length on the conversation page", () => {
  // 回归闸门：模型下拉与用量弹窗是渲染路径（一次抛错＝整个面板被兜底 UI 替换），
  // 只能经 modelList 取值。谁把 `?.models.length`（可选链只到外层对象）写回来，
  // CodeBuddy 会话点开下拉就会再次白掉面板。
  //
  // 先剥掉注释再匹配：注释里为了说明问题会逐字提到那个写法（本次修复的注释就提到过），
  // 不剥的话闸门会被自己的说明文字绊倒。
  const source = readFileSync(new URL("../pages/ConversationPage.tsx", import.meta.url), "utf8")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/[^\n]*/g, "");

  assert.doesNotMatch(source, /[A-Za-z_$][A-Za-z0-9_$]*\?\.models\.length/);
  assert.doesNotMatch(source, /[A-Za-z_$][A-Za-z0-9_$]*\.models\.length/);
  assert.match(source, /modelList\(view\)/);
  assert.match(source, /modelList\(usage\)/);
});
